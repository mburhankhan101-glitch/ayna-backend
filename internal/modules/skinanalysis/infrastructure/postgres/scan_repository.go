package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/domain"
	"github.com/ayna/ayna-backend/internal/platform/database"
)

// ScanRepository stores scans and their idempotency claims.
//
// Takes a DBTX rather than a pool so the same code runs inside a transaction or
// outside one. That is load-bearing here: Save must write the scan and claim
// the key atomically, and a repository that opened its own connection could not
// express that.
type ScanRepository struct{ db database.DBTX }

func NewScanRepository(db database.DBTX) *ScanRepository {
	return &ScanRepository{db: db}
}

func (r *ScanRepository) Save(ctx context.Context, s *domain.Scan, key string) error {
	// One statement, two inserts, via a CTE.
	//
	// Both rows or neither: a scan without its key could be re-run on retry and
	// buy a second vendor call, and a key without its scan would block the
	// user's next legitimate attempt forever. A single statement is atomic on
	// its own, which means this works whether the caller handed us a pool or a
	// transaction -- InTx needs a concrete pool and would close that door.
	_, err := r.db.Exec(ctx, `
		WITH inserted AS (
			INSERT INTO scans (id, user_id, status, stage, allowance_spent, submitted_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, user_id
		)
		INSERT INTO scan_idempotency (user_id, idempotency_key, scan_id)
		SELECT user_id, $7, id FROM inserted`,
		s.ID, s.UserID, string(s.Status), string(s.Stage),
		s.AllowanceSpent, s.SubmittedAt, key,
	)
	if err != nil {
		// A duplicate key means a concurrent retry won the race. The caller
		// resolves it by reading back the existing scan, which is what
		// idempotency should do -- and the reason this is a unique index
		// rather than a check-then-insert in the handler, which would be a
		// race under exactly the retry it exists to handle.
		if database.IsUniqueViolation(err, "scan_idempotency_pkey") {
			return domain.ErrDuplicateSubmission
		}
		return fmt.Errorf("insert scan: %w", err)
	}
	return nil
}

func (r *ScanRepository) Update(ctx context.Context, s *domain.Scan) error {
	var report []byte
	if s.Report != nil {
		b, err := json.Marshal(s.Report)
		if err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		report = b
	}

	var reason *string
	if s.RejectionReason != nil {
		v := string(*s.RejectionReason)
		reason = &v
	}

	_, err := r.db.Exec(ctx, `
		UPDATE scans
		   SET status = $2, stage = $3, allowance_spent = $4,
		       rejection_reason = $5, report = $6, ruleset_version = $7,
		       provider = $8, completed_at = $9, heatmap = $10
		 WHERE id = $1`,
		s.ID, string(s.Status), string(s.Stage), s.AllowanceSpent,
		reason, report, s.RulesetVersion, s.Provider, s.CompletedAt, s.Heatmap,
	)
	if err != nil {
		return fmt.Errorf("update scan: %w", err)
	}
	return nil
}

const selectScan = `
	SELECT id, user_id, status, stage, allowance_spent, rejection_reason,
	       report, ruleset_version, provider, submitted_at, completed_at
	  FROM scans`

func (r *ScanRepository) FindByID(ctx context.Context, id string) (*domain.Scan, error) {
	return r.one(ctx, selectScan+` WHERE id = $1`, id)
}

func (r *ScanRepository) FindByIdempotencyKey(ctx context.Context, userID, key string) (*domain.Scan, error) {
	return r.one(ctx, selectScan+`
		 WHERE id = (SELECT scan_id FROM scan_idempotency
		              WHERE user_id = $1 AND idempotency_key = $2)`,
		userID, key,
	)
}

func (r *ScanRepository) CountSpentSince(ctx context.Context, userID string, since time.Time) (int, error) {
	var n int
	// allowance_spent, not a row count. Rejected and failed scans are kept for
	// the user's own history but neither cost anything, so neither may count
	// against the week.
	err := r.db.QueryRow(ctx, `
		SELECT count(*) FROM scans
		 WHERE user_id = $1 AND allowance_spent AND submitted_at >= $2`,
		userID, since,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count scans: %w", err)
	}
	return n, nil
}

func (r *ScanRepository) one(ctx context.Context, sql string, args ...any) (*domain.Scan, error) {
	s, err := scanFrom(r.db.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		// A domain error, never the driver's. The application layer must be
		// able to branch on absence without knowing Postgres is underneath.
		return nil, domain.ErrScanNotFound
	}
	return s, err
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows, which is what lets a
// single-row read and a list share one decoder. Two copies of this mapping
// would drift the first time a column is added, and the drift would be silent:
// a list showing stale-shaped scans still renders.
type rowScanner interface{ Scan(dest ...any) error }

func scanFrom(row rowScanner) (*domain.Scan, error) {
	var (
		s       domain.Scan
		status  string
		stage   *string
		reason  *string
		report  []byte
		ruleset *string
		prov    *string
	)

	err := row.Scan(
		&s.ID, &s.UserID, &status, &stage, &s.AllowanceSpent, &reason,
		&report, &ruleset, &prov, &s.SubmittedAt, &s.CompletedAt,
	)
	if err != nil {
		return nil, err
	}

	s.Status = domain.Status(status)
	if stage != nil {
		s.Stage = domain.Stage(*stage)
	}
	if reason != nil {
		v := domain.RejectionReason(*reason)
		s.RejectionReason = &v
	}
	s.RulesetVersion = ruleset
	s.Provider = prov

	if len(report) > 0 {
		var rep domain.Report
		if err := json.Unmarshal(report, &rep); err != nil {
			return nil, fmt.Errorf("decode report: %w", err)
		}
		s.Report = &rep
	}

	return &s, nil
}

func (r *ScanRepository) ListRecent(ctx context.Context, userID string, limit int) ([]*domain.Scan, error) {
	rows, err := r.db.Query(ctx, selectScan+`
		 WHERE user_id = $1
		 ORDER BY submitted_at DESC
		 LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list scans: %w", err)
	}
	defer rows.Close()

	var out []*domain.Scan
	for rows.Next() {
		s, err := scanFrom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FindHeatmap reads just the overlay bytes.
//
// Its own query, and deliberately not part of selectScan. Every list and status
// read would otherwise drag ~30KB of image per row through the connection for
// a column almost none of them use -- and a history of twenty scans would move
// half a megabyte to render six numbers.
func (r *ScanRepository) FindHeatmap(ctx context.Context, id string) ([]byte, string, error) {
	var (
		heatmap []byte
		userID  string
	)
	err := r.db.QueryRow(ctx,
		`SELECT heatmap, user_id FROM scans WHERE id = $1`, id,
	).Scan(&heatmap, &userID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", domain.ErrScanNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read heatmap: %w", err)
	}
	return heatmap, userID, nil
}

// ExpireHeatmaps clears images past their retention window (NFR-4).
//
// `heatmap IS NOT NULL` is in the predicate as well as implied by the SET,
// because without it every sweep would rewrite every old row it had already
// cleared -- generating dead tuples and a growing table for no work done. With
// it, a second sweep over the same range updates nothing and reports zero,
// which is also what makes the count in the logs mean something.
//
// The image is nulled and the row is kept. See the port for why.
func (r *ScanRepository) ExpireHeatmaps(ctx context.Context, userIDs []string, before time.Time) (int64, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}

	const q = `
		UPDATE scans
		   SET heatmap = NULL
		 WHERE user_id = ANY($1)
		   AND submitted_at < $2
		   AND heatmap IS NOT NULL`

	tag, err := r.db.Exec(ctx, q, userIDs, before)
	if err != nil {
		return 0, fmt.Errorf("expire heatmaps: %w", err)
	}
	return tag.RowsAffected(), nil
}
