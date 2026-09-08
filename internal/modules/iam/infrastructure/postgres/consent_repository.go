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

// ConsentRepository stores the append-only consent history (FR-1).
//
// There is no Update and no Delete here, matching the port. That absence is
// the contract rather than an omission: revoking consent appends a record with
// granted = false. An audit trail with an edit path is not an audit trail, and
// the question that eventually gets asked — "was this user consented at the
// moment that scan ran?" — is only answerable from a history.
type ConsentRepository struct {
	db database.DBTX
}

func NewConsentRepository(db database.DBTX) *ConsentRepository {
	return &ConsentRepository{db: db}
}

var _ domain.ConsentRepository = (*ConsentRepository)(nil)

func (r *ConsentRepository) Append(ctx context.Context, rec domain.ConsentRecord) error {
	const q = `
		INSERT INTO consent_records (id, user_id, policy_version, granted, recorded_at)
		VALUES ($1, $2, $3, $4, $5)`

	_, err := r.db.Exec(ctx, q,
		rec.ID, rec.UserID, rec.PolicyVersion, rec.Granted, rec.RecordedAt,
	)
	if err != nil {
		return fmt.Errorf("append consent: %w", err)
	}
	return nil
}

// LatestFor returns the most recent record for one policy version.
//
// Version-scoped on purpose: consent to an earlier wording must not silently
// authorise a later one. A user who agreed to the 2025 policy has said nothing
// about the 2026 one, and this query reflects that by finding nothing.
//
// The tie-break on id is not decoration. Two records can share a recorded_at:
// a double-tap, a retry, or any two writes inside the same clock tick — and
// Postgres timestamps are microsecond-resolution, which is easy to land on
// twice. With ORDER BY recorded_at alone, a grant and a revocation written in
// the same instant resolve in whatever order the index happens to return,
// so the same user could be reported as consented or not on alternate reads.
//
// Ids are ULIDs, which sort by generation time, so the later record wins the
// tie deterministically. Found by a test that granted and revoked through a
// frozen clock — the exact case a fixed clock makes reproducible and a real
// one hides.
func (r *ConsentRepository) LatestFor(ctx context.Context, userID, policyVersion string) (domain.ConsentState, error) {
	const q = `
		SELECT id, user_id, policy_version, granted, recorded_at
		  FROM consent_records
		 WHERE user_id = $1 AND policy_version = $2
		 ORDER BY recorded_at DESC, id DESC
		 LIMIT 1`

	var (
		id, uid, version string
		granted          bool
		recordedAt       time.Time
	)

	err := r.db.QueryRow(ctx, q, userID, policyVersion).
		Scan(&id, &uid, &version, &granted, &recordedAt)
	if err != nil {
		// No record is not an error: it means "never asked under this
		// version", which the domain represents as a zero ConsentState and
		// distinguishes from an explicit refusal.
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ConsentState{}, nil
		}
		return domain.ConsentState{}, fmt.Errorf("query consent: %w", err)
	}

	return domain.ConsentState{
		Latest: &domain.ConsentRecord{
			ID:            id,
			UserID:        uid,
			PolicyVersion: version,
			Granted:       granted,
			RecordedAt:    recordedAt,
		},
	}, nil
}
