// Package postgres implements the IAM ports against Postgres.
//
// Everything here depends inward: it imports the domain, the domain does not
// import it. That is the dependency inversion ADR-001 rests on, and
// internal/arch fails the build if it is ever reversed.
//
// The other rule this package holds to: **no driver error escapes**. A caller
// gets domain.ErrUserNotFound, never pgx.ErrNoRows; domain.ErrAuth0SubTaken,
// never a raw 23505. Leaking driver errors upward would make the application
// layer depend on Postgres by accident, through its error handling, without
// ever importing it.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ayna/ayna-backend/internal/modules/iam/domain"
	"github.com/ayna/ayna-backend/internal/platform/database"
)

// constraintAuth0Sub is the UNIQUE constraint from 00001_users.sql. Named
// explicitly so a violation of some *other* unique constraint added later is
// not misreported as "this identity already has a profile".
const constraintAuth0Sub = "users_auth0_sub_key"

type UserRepository struct {
	db database.DBTX
}

// NewUserRepository takes a DBTX rather than a pool so the caller controls the
// transaction boundary. See database.DBTX for why that matters.
func NewUserRepository(db database.DBTX) *UserRepository {
	return &UserRepository{db: db}
}

var _ domain.UserRepository = (*UserRepository)(nil)

const userColumns = `id, auth0_sub, birth_year, timezone, display_name,
	                 deletion_requested_at, created_at, photo_retention_days`

func (r *UserRepository) Save(ctx context.Context, u *domain.User) error {
	const q = `
		INSERT INTO users (id, auth0_sub, birth_year, timezone, display_name, created_at, updated_at,
		                   photo_retention_days)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $6, $7)`

	_, err := r.db.Exec(ctx, q,
		u.ID(),
		u.Auth0Sub(),
		u.BirthYear(),
		u.Timezone().String(),
		u.DisplayName(),
		u.CreatedAt(),
		u.PhotoRetentionDays(),
	)
	if err != nil {
		// Uniqueness is enforced by the database, not by a check-then-insert:
		// two concurrent sign-ups would both pass a prior existence check and
		// both proceed. Attempt the insert, translate the failure.
		if database.IsUniqueViolation(err, constraintAuth0Sub) {
			return domain.ErrAuth0SubTaken
		}
		return fmt.Errorf("save user: %w", err)
	}
	return nil
}

// FindByAuth0Sub resolves the caller of an authenticated request. This is the
// hottest read in the system, which is why 00001_users.sql gives auth0_sub its
// own index rather than relying on the UNIQUE constraint's implicit one.
func (r *UserRepository) FindByAuth0Sub(ctx context.Context, sub string) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE auth0_sub = $1`
	return r.scanOne(ctx, q, sub)
}

func (r *UserRepository) FindByID(ctx context.Context, id string) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return r.scanOne(ctx, q, id)
}

// Update persists the mutable fields.
//
// Note what is absent: birth_year and auth0_sub are never updated. Birth year
// passed the age gate at creation and re-writing it would let a user edit
// their way around PD-1; auth0_sub is the identity itself, and a changing
// identity key is not a key.
func (r *UserRepository) Update(ctx context.Context, u *domain.User) error {
	const q = `
		UPDATE users
		   SET timezone = $2,
		       display_name = NULLIF($3, ''),
		       deletion_requested_at = $4,
		       photo_retention_days = $5,
		       updated_at = now()
		 WHERE id = $1`

	tag, err := r.db.Exec(ctx, q,
		u.ID(),
		u.Timezone().String(),
		u.DisplayName(),
		u.DeletionRequestedAt(),
		u.PhotoRetentionDays(),
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	// A silent no-op update is worse than an error: the caller believes it
	// saved something. Zero rows means the row is gone (a completed purge, or
	// a bad id), and the caller needs to know that.
	if tag.RowsAffected() == 0 {
		return domain.ErrUserNotFound
	}
	return nil
}

func (r *UserRepository) scanOne(ctx context.Context, q string, arg any) (*domain.User, error) {
	var (
		id, auth0Sub, tz string
		birthYear        int
		displayName      *string
		deletionAt       *time.Time
		createdAt        time.Time
		retentionDays    *int
	)

	err := r.db.QueryRow(ctx, q, arg).Scan(
		&id, &auth0Sub, &birthYear, &tz, &displayName, &deletionAt, &createdAt,
		&retentionDays,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrUserNotFound
		}
		return nil, fmt.Errorf("query user: %w", err)
	}

	var name string
	if displayName != nil {
		name = *displayName
	}

	// Rehydrate, never NewUser: the age gate must not re-run on load. If
	// MinimumAge were raised, re-validating here would lock out existing users
	// mid-session, which is a policy change disguised as a database read.
	return domain.Rehydrate(id, auth0Sub, birthYear, tz, name, deletionAt, createdAt, retentionDays)
}

// RetentionCohorts groups users by their photo retention policy.
//
// One query returning at most three rows, because PhotoRetentionOptions is a
// closed set of three values. Users with no policy are excluded by the WHERE
// clause rather than filtered afterwards, so the partial index on
// photo_retention_days is the whole access path.
//
// Accounts pending deletion are excluded too. FR-11's purge is already going
// to remove everything they own, and having two jobs racing to delete the same
// rows buys nothing but a confusing log.
func (r *UserRepository) RetentionCohorts(ctx context.Context) ([]domain.RetentionCohort, error) {
	const q = `
		SELECT photo_retention_days, array_agg(id)
		  FROM users
		 WHERE photo_retention_days IS NOT NULL
		   AND deletion_requested_at IS NULL
		 GROUP BY photo_retention_days`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("retention cohorts: %w", err)
	}
	defer rows.Close()

	var out []domain.RetentionCohort
	for rows.Next() {
		var c domain.RetentionCohort
		if err := rows.Scan(&c.Days, &c.UserIDs); err != nil {
			return nil, fmt.Errorf("scan cohort: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
