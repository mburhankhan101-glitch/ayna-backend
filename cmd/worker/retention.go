package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	iam "github.com/ayna/ayna-backend/internal/modules/iam/domain"
)

// The photo retention sweep (NFR-4).
//
// # Why this lives in the composition root
//
// It needs two things that belong to different modules: what each user's
// retention policy is (IAM) and which stored images are past it (skinanalysis).
// Neither module may import the other, and a SQL join across `users` and
// `scans` would slip past internal/arch -- which checks Go imports, not
// tables -- while welding the two together exactly as the boundary forbids.
//
// So the sweep is assembled here, from two narrow interfaces this file
// declares. IAM knows what a policy is and nothing about images; skinanalysis
// knows how to clear an image and nothing about policies.
//
// # What it deletes
//
// The image, never the row. Someone who asked for their photos to be deleted
// after 30 days did not ask for their history to be erased, and FR-8's entire
// value is the trend line getting longer. Scores are not photographs.

// policySource is IAM's half: who has a finite policy, grouped by its value.
type policySource interface {
	RetentionCohorts(ctx context.Context) ([]iam.RetentionCohort, error)
}

// imageExpirer is skinanalysis' half: clear stored images for these users older
// than this instant, and say how many.
type imageExpirer interface {
	ExpireHeatmaps(ctx context.Context, userIDs []string, before time.Time) (int64, error)
}

// SweepResult is what the sweep did, for the log and for the response.
//
// Counted per cohort rather than as one total, because "cleared 40 images" is
// not an answer to "is the 7-day policy actually running?" -- and that is the
// question this job exists to be able to answer.
type SweepResult struct {
	Cleared  int64            `json:"cleared"`
	ByPolicy map[string]int64 `json:"byPolicy"`
	RanAt    time.Time        `json:"ranAt"`
}

type retentionSweeper struct {
	policies policySource
	images   imageExpirer
	log      *slog.Logger

	// now is injectable so the cutoff arithmetic is testable. A sweep is
	// entirely a statement about time, and a test that cannot control the clock
	// can only assert that it did not crash.
	now func() time.Time
}

func newRetentionSweeper(p policySource, i imageExpirer, log *slog.Logger) *retentionSweeper {
	return &retentionSweeper{policies: p, images: i, log: log, now: time.Now}
}

// Sweep clears every image that has outlived its owner's policy.
//
// One delete per cohort, not per user: there are three legal policy values, so
// this is three statements however many accounts exist.
//
// A cohort that fails does not abort the others. The policies are independent,
// and letting a transient error on the 7-day cohort skip the 365-day one would
// mean a single bad statement silently stops enforcing a privacy promise
// across the whole product.
func (s *retentionSweeper) Sweep(ctx context.Context) (SweepResult, error) {
	start := s.now()
	result := SweepResult{ByPolicy: map[string]int64{}, RanAt: start}

	cohorts, err := s.policies.RetentionCohorts(ctx)
	if err != nil {
		return result, fmt.Errorf("read retention cohorts: %w", err)
	}

	var failed error
	for _, c := range cohorts {
		if c.Days <= 0 || len(c.UserIDs) == 0 {
			// Defensive, and worth being explicit about: a zero or negative
			// window would compute a cutoff in the future and clear every image
			// the cohort owns. The column has a CHECK constraint, so this is
			// unreachable -- which is exactly why it is cheap to keep.
			continue
		}

		cutoff := start.AddDate(0, 0, -c.Days)
		n, err := s.images.ExpireHeatmaps(ctx, c.UserIDs, cutoff)
		if err != nil {
			s.log.Error("retention cohort failed",
				slog.Int("days", c.Days),
				slog.Int("users", len(c.UserIDs)),
				slog.String("error", err.Error()),
			)
			failed = err
			continue
		}

		key := fmt.Sprintf("%dd", c.Days)
		result.ByPolicy[key] = n
		result.Cleared += n

		s.log.Info("retention cohort swept",
			slog.Int("days", c.Days),
			slog.Int("users", len(c.UserIDs)),
			slog.Time("cutoff", cutoff),
			slog.Int64("cleared", n),
		)
	}

	// Partial success is still reported as an error so the scheduler's retry
	// and the alerting see it, but the counts for the cohorts that DID run are
	// returned alongside. Throwing those away would make a partial failure
	// indistinguishable from a total one.
	return result, failed
}
