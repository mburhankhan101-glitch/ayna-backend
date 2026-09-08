package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	iam "github.com/ayna/ayna-backend/internal/modules/iam/domain"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakePolicies struct {
	cohorts []iam.RetentionCohort
	err     error
}

func (f fakePolicies) RetentionCohorts(context.Context) ([]iam.RetentionCohort, error) {
	return f.cohorts, f.err
}

// call records one ExpireHeatmaps invocation so the test can assert on the
// cutoff, which is the only thing in this job that can be subtly wrong.
type call struct {
	userIDs []string
	before  time.Time
}

type fakeImages struct {
	calls []call
	// failOn makes one specific cohort fail, keyed by its first user id.
	failOn string
	n      int64
}

func (f *fakeImages) ExpireHeatmaps(_ context.Context, ids []string, before time.Time) (int64, error) {
	f.calls = append(f.calls, call{userIDs: ids, before: before})
	if f.failOn != "" && len(ids) > 0 && ids[0] == f.failOn {
		return 0, errors.New("boom")
	}
	return f.n, nil
}

func sweeperAt(t time.Time, p policySource, i imageExpirer) *retentionSweeper {
	s := newRetentionSweeper(p, i, quietLog())
	s.now = func() time.Time { return t }
	return s
}

// The cutoff is the whole job. Everything else is plumbing.
func TestSweepCutoffIsPolicyDaysBeforeNow(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	images := &fakeImages{n: 3}

	s := sweeperAt(now, fakePolicies{cohorts: []iam.RetentionCohort{
		{Days: 7, UserIDs: []string{"usr_a"}},
		{Days: 365, UserIDs: []string{"usr_b", "usr_c"}},
	}}, images)

	result, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if len(images.calls) != 2 {
		t.Fatalf("want one delete per cohort, got %d", len(images.calls))
	}

	wantWeek := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if !images.calls[0].before.Equal(wantWeek) {
		t.Errorf("7-day cutoff = %v, want %v", images.calls[0].before, wantWeek)
	}

	wantYear := time.Date(2025, 9, 2, 12, 0, 0, 0, time.UTC)
	if !images.calls[1].before.Equal(wantYear) {
		t.Errorf("365-day cutoff = %v, want %v", images.calls[1].before, wantYear)
	}

	if result.Cleared != 6 {
		t.Errorf("cleared = %d, want 6 (3 per cohort)", result.Cleared)
	}
	if result.ByPolicy["7d"] != 3 || result.ByPolicy["365d"] != 3 {
		t.Errorf("per-policy counts wrong: %v", result.ByPolicy)
	}
}

// A user who chose "keep indefinitely" has no cohort at all, so the sweep must
// never be handed their id. This is the test that would catch the worst
// possible regression in this feature: deleting photos someone asked to keep.
func TestSweepNeverTouchesUsersWithoutAPolicy(t *testing.T) {
	images := &fakeImages{}
	s := sweeperAt(time.Now(), fakePolicies{cohorts: nil}, images)

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(images.calls) != 0 {
		t.Fatalf("swept %d cohorts when none had a policy", len(images.calls))
	}
}

// A non-positive window would put the cutoff in the future and clear every
// image the cohort owns. The CHECK constraint makes it unreachable; this makes
// it unreachable twice.
func TestSweepSkipsNonPositiveWindows(t *testing.T) {
	images := &fakeImages{}
	s := sweeperAt(time.Now(), fakePolicies{cohorts: []iam.RetentionCohort{
		{Days: 0, UserIDs: []string{"usr_a"}},
		{Days: -30, UserIDs: []string{"usr_b"}},
	}}, images)

	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(images.calls) != 0 {
		t.Fatalf("a non-positive window reached the delete: %+v", images.calls)
	}
}

// One failing cohort must not stop the others. A transient error on the 7-day
// policy silently disabling the 365-day one would mean a single bad statement
// stops enforcing the promise product-wide.
func TestSweepContinuesAfterACohortFails(t *testing.T) {
	images := &fakeImages{n: 2, failOn: "usr_a"}
	s := sweeperAt(time.Now(), fakePolicies{cohorts: []iam.RetentionCohort{
		{Days: 7, UserIDs: []string{"usr_a"}},
		{Days: 30, UserIDs: []string{"usr_b"}},
	}}, images)

	result, err := s.Sweep(context.Background())
	if err == nil {
		t.Fatal("a failed cohort must surface as an error so the scheduler retries")
	}
	if len(images.calls) != 2 {
		t.Fatalf("second cohort was skipped after the first failed")
	}
	// The successful cohort's count survives, so a partial sweep is
	// distinguishable from one that did nothing.
	if result.ByPolicy["30d"] != 2 {
		t.Errorf("counts from the surviving cohort were lost: %v", result.ByPolicy)
	}
	if _, reported := result.ByPolicy["7d"]; reported {
		t.Error("the failed cohort must not report a count")
	}
}

func TestSweepReportsUnreadablePolicies(t *testing.T) {
	images := &fakeImages{}
	s := sweeperAt(time.Now(), fakePolicies{err: errors.New("db down")}, images)

	if _, err := s.Sweep(context.Background()); err == nil {
		t.Fatal("want an error when the policies cannot be read")
	}
	if len(images.calls) != 0 {
		t.Fatal("nothing may be deleted when the policy list failed to load")
	}
}
