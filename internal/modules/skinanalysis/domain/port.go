package domain

import (
	"context"
	"time"
)

// Analyzer is the vendor boundary, and the reason ADR-002 confined lock-in to
// one package.
//
// It takes JPEG bytes and returns six severities. Everything specific to AILab
// — that its scale runs backwards, that Pro and Basic disagree about acne, that
// `sensitivity_score` is not redness — lives behind this and nowhere else.
//
// It is also the seam the architecture turns on. Today the implementation runs
// inside the HTTP request; moving it to the worker means calling the same
// interface from a different process. Nothing above this line knows which.
type Analyzer interface {
	// Analyze returns a result, or a RejectedError when the photo itself is
	// the problem.
	//
	// The distinction matters commercially, not just semantically: a rejection
	// costs the user nothing because the vendor billed nothing, while a
	// completed analysis spends both. An implementation that collapses the two
	// into one error type makes that impossible to honour.
	Analyze(ctx context.Context, jpeg []byte) (*Analysis, error)

	// Name identifies the vendor and endpoint, recorded on every scan so a
	// later provider change does not silently mix two scales in one trend.
	Name() string
}

// Analysis is what a vendor produced, before it becomes a user-facing Report.
type Analysis struct {
	// OverallScore is already rescaled to 0-100 where higher is better.
	OverallScore int

	// SkinAge is nil when the endpoint has no such field. Not zero — a vendor
	// that never produced a skin age and one that returned 0 are different
	// facts, and only one of them is worth showing.
	SkinAge *int

	Issues []Issue

	// Heatmap is the redness overlay, already composited onto the photo.
	//
	// Nil whenever anything about it was uncertain. A report without an overlay
	// is a tier the product already ships; an overlay built from a map that did
	// not match is a false claim about someone's face.
	Heatmap []byte
}

// RejectedError means the photo cannot be analysed and the user should retake.
//
// A distinct type rather than a sentinel because it carries the reason, and the
// reason is the entire value to the user: "too dark" and "no face" lead to
// different actions.
type RejectedError struct{ Reason RejectionReason }

func (e *RejectedError) Error() string { return "photo rejected: " + string(e.Reason) }

// ScanRepository is the port for persistence.
type ScanRepository interface {
	// Save inserts a new scan and claims its idempotency key in the same
	// transaction.
	//
	// Both or neither: a scan stored without its key would be re-runnable on
	// retry, buying a second vendor call, and a key claimed without a scan
	// would block the user's next legitimate attempt forever.
	Save(ctx context.Context, s *Scan, idempotencyKey string) error

	// Update persists the result of an analysis.
	Update(ctx context.Context, s *Scan) error

	FindByID(ctx context.Context, id string) (*Scan, error)

	// FindByIdempotencyKey returns the scan a previous identical request
	// created, or ErrScanNotFound. This is what makes a retry free.
	FindByIdempotencyKey(ctx context.Context, userID, key string) (*Scan, error)

	// FindHeatmap returns the overlay bytes and the owning user, or
	// ErrScanNotFound. Separate from FindByID because the image is ~30KB and
	// no other read wants it.
	FindHeatmap(ctx context.Context, scanID string) (heatmap []byte, userID string, err error)

	// ListRecent returns a user's scans, newest first.
	//
	// Includes rejected and failed ones. They cost the user nothing, but they
	// are part of what happened, and a history that quietly omits them would
	// disagree with the allowance the user can see on their own home screen.
	ListRecent(ctx context.Context, userID string, limit int) ([]*Scan, error)

	// CountSpentSince counts a user's ALLOWANCE-SPENDING scans in the period.
	//
	// Not the same as counting rows. Rejected and failed scans are stored — the
	// history is worth keeping — but neither spent anything, so neither may
	// count against the user's week.
	CountSpentSince(ctx context.Context, userID string, since time.Time) (int, error)

	// ExpireHeatmaps clears stored images for the given users where the scan is
	// older than `before`, and reports how many it cleared (NFR-4).
	//
	// It clears the image and KEEPS THE ROW. The scores are not photos: a user
	// who asked for their pictures to be deleted after 30 days did not ask for
	// their trend to be erased, and FR-8's whole value is the line getting
	// longer. Deleting the row would also silently change history the user has
	// already seen.
	//
	// Takes a user set rather than a policy so this module never has to know
	// what a retention policy is. IAM owns that; this owns images.
	ExpireHeatmaps(ctx context.Context, userIDs []string, before time.Time) (int64, error)
}
