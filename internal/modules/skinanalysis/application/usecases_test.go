package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/application"
	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/domain"
)

// fakeIdentity maps one subject to one internal user id, which is the whole
// point: the two strings look nothing alike, so a use case that confuses them
// cannot accidentally pass.
type fakeIdentity struct {
	sub       string
	userID    string
	consented bool
}

func (f fakeIdentity) Resolve(_ context.Context, subject string) (string, bool, error) {
	if subject != f.sub {
		return "", false, domain.ErrScanNotFound
	}
	return f.userID, f.consented, nil
}

type fakeScans struct {
	byID map[string]*domain.Scan

	// recent is returned newest-first, matching what the real repository's
	// ORDER BY submitted_at DESC produces. The trend logic depends on that
	// ordering to pick a week's latest scan, so a fake that returned them in
	// any other order would let a real bug pass.
	recent []*domain.Scan
}

func (f *fakeScans) FindByID(_ context.Context, id string) (*domain.Scan, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrScanNotFound
	}
	return s, nil
}

func (f *fakeScans) Save(context.Context, *domain.Scan, string) error { return nil }
func (f *fakeScans) Update(context.Context, *domain.Scan) error       { return nil }
func (f *fakeScans) FindByIdempotencyKey(context.Context, string, string) (*domain.Scan, error) {
	return nil, domain.ErrScanNotFound
}
func (f *fakeScans) CountSpentSince(context.Context, string, time.Time) (int, error) {
	return 0, nil
}

func (f *fakeScans) FindHeatmap(_ context.Context, id string) ([]byte, string, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, "", domain.ErrScanNotFound
	}
	return s.Heatmap, s.UserID, nil
}

func (f *fakeScans) ListRecent(_ context.Context, _ string, limit int) ([]*domain.Scan, error) {
	if len(f.recent) > limit {
		return f.recent[:limit], nil
	}
	return f.recent, nil
}

// The bug this test exists for, in full, because it reached a real user:
//
// GetScan took a parameter named userID. The HTTP handler has only the
// authenticated SUBJECT, so it passed that. The ownership check then compared
// "usr_01ABC" against "google-oauth2|123", which can never match, and every
// read returned 404 -- after the vendor had been paid and the scan stored.
//
// The user saw "Scan not found. Nothing was charged." while their credits were
// gone. A test that only checked the happy path with matching strings would
// have passed throughout.
func TestGetScanResolvesTheSubjectRatherThanComparingItToAUserID(t *testing.T) {
	const (
		subject = "google-oauth2|108812345678901234567"
		userID  = "usr_01M10FEC7VMKGBPK8XNAG4EX9C"
		scanID  = "scn_01M10FEC7VMKGBPK8XNAG4EX9D"
	)

	scans := &fakeScans{byID: map[string]*domain.Scan{
		scanID: {ID: scanID, UserID: userID, Status: domain.StatusCompleted},
	}}

	uc := application.NewGetScan(scans, fakeIdentity{
		sub: subject, userID: userID, consented: true,
	})

	got, err := uc.Execute(context.Background(), subject, scanID)
	if err != nil {
		t.Fatalf("owner could not read their own scan: %v", err)
	}
	if got.ID != scanID {
		t.Errorf("got scan %q, want %q", got.ID, scanID)
	}
}

func TestGetScanRefusesSomeoneElsesScan(t *testing.T) {
	const scanID = "scn_01OTHER"

	scans := &fakeScans{byID: map[string]*domain.Scan{
		scanID: {ID: scanID, UserID: "usr_01SOMEONE_ELSE"},
	}}

	uc := application.NewGetScan(scans, fakeIdentity{
		sub: "google-oauth2|999", userID: "usr_01ME", consented: true,
	})

	_, err := uc.Execute(context.Background(), "google-oauth2|999", scanID)

	// Not found rather than forbidden: telling a caller that a scan exists but
	// is not theirs confirms the id is real, and a scan id points at a face.
	if !errors.Is(err, domain.ErrScanNotFound) {
		t.Errorf("got %v, want ErrScanNotFound", err)
	}
}

// Reading a scan must not require current consent.
//
// Someone who consented, scanned, and then revoked has already paid for that
// report. Gating the read on live consent would delete their access to
// something they own as a side effect of exercising a privacy right.
func TestGetScanDoesNotRequireCurrentConsent(t *testing.T) {
	const (
		subject = "google-oauth2|111"
		userID  = "usr_01REVOKED"
		scanID  = "scn_01PAIDFOR"
	)

	scans := &fakeScans{byID: map[string]*domain.Scan{
		scanID: {ID: scanID, UserID: userID, Status: domain.StatusCompleted},
	}}

	uc := application.NewGetScan(scans, fakeIdentity{
		sub: subject, userID: userID, consented: false,
	})

	if _, err := uc.Execute(context.Background(), subject, scanID); err != nil {
		t.Errorf("revoking consent must not hide an already-paid-for scan: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Trend
// ---------------------------------------------------------------------------

func completed(id string, at time.Time, score int) *domain.Scan {
	return &domain.Scan{
		ID: id, UserID: "usr_01ME", Status: domain.StatusCompleted,
		SubmittedAt: at, AllowanceSpent: true,
		Report: &domain.Report{ScanID: id, OverallScore: score},
	}
}

func trendFor(scans []*domain.Scan, weeks int) ([]application.TrendPoint, int, error) {
	uc := application.NewGetTrend(
		&fakeScans{recent: scans},
		fakeIdentity{sub: "sub", userID: "usr_01ME", consented: true},
		func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) },
	)
	return uc.Execute(context.Background(), "sub", weeks)
}

// One point per week, using that week's LATEST scan.
//
// Not an average. The user is asking "where am I now", and averaging a Monday
// retake with a Saturday one answers a question nobody posed — it would also
// let a bad early-week photo drag down a week the user finished well.
func TestTrendKeepsTheLatestScanInEachWeek(t *testing.T) {
	// Same week, three days apart. Newest first, as the repository returns.
	points, _, err := trendFor([]*domain.Scan{
		completed("scn_thu", time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC), 71),
		completed("scn_mon", time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC), 40),
	}, 6)
	if err != nil {
		t.Fatal(err)
	}

	if len(points) != 1 {
		t.Fatalf("got %d points, want 1 — both scans fall in the same week", len(points))
	}
	if points[0].OverallScore != 71 {
		t.Errorf("score = %d, want 71 (Thursday, the later scan)", points[0].OverallScore)
	}
}

// The chart reads left to right in time, but the repository hands scans back
// newest-first. Getting this backwards draws every trend in reverse, which
// looks plausible and is exactly wrong.
func TestTrendIsOldestFirst(t *testing.T) {
	points, _, err := trendFor([]*domain.Scan{
		completed("scn_w3", time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC), 60),
		completed("scn_w2", time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC), 50),
		completed("scn_w1", time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC), 40),
	}, 6)
	if err != nil {
		t.Fatal(err)
	}

	want := []int{40, 50, 60}
	if len(points) != len(want) {
		t.Fatalf("got %d points, want %d", len(points), len(want))
	}
	for i, w := range want {
		if points[i].OverallScore != w {
			t.Errorf("point %d = %d, want %d", i, points[i].OverallScore, w)
		}
	}
}

// Only completed scans are points. A rejected photo produced no reading, and
// plotting it as a zero would draw a cliff into the chart for a scan that never
// measured anything.
func TestTrendIgnoresScansWithNoReport(t *testing.T) {
	points, total, err := trendFor([]*domain.Scan{
		{ID: "scn_rej", Status: domain.StatusRejected,
			SubmittedAt: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)},
		completed("scn_ok", time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC), 55),
	}, 6)
	if err != nil {
		t.Fatal(err)
	}

	if len(points) != 1 || points[0].OverallScore != 55 {
		t.Errorf("got %v, want a single point of 55", points)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 — a rejection is not a data point", total)
	}
}

// Free users see a window, not an error, and the total says what is behind it.
func TestTrendTruncatesWithoutHiding(t *testing.T) {
	var scans []*domain.Scan
	for i := 0; i < 10; i++ {
		scans = append(scans, completed(
			"scn", time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC).AddDate(0, 0, -7*i), 50+i,
		))
	}

	points, total, err := trendFor(scans, 6)
	if err != nil {
		t.Fatal(err)
	}

	if len(points) != 6 {
		t.Errorf("got %d points, want the 6-week window", len(points))
	}
	if total != 10 {
		t.Errorf("total = %d, want 10 — the client must be able to say what is missing", total)
	}
	// The window keeps the most RECENT weeks. Truncating from the other end
	// would show a new user six weeks of nothing and hide the scan they just
	// took.
	if points[len(points)-1].OverallScore != 50 {
		t.Errorf("last point = %d, want 50 (the newest week)",
			points[len(points)-1].OverallScore)
	}
}

// ---------------------------------------------------------------------------
// Allowance
// ---------------------------------------------------------------------------

var allowanceNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func allowanceFor(scans []*domain.Scan, limit int) (application.Allowance, error) {
	uc := application.NewGetAllowance(
		&fakeScans{recent: scans},
		func() time.Time { return allowanceNow },
		limit,
	)
	return uc.Execute(context.Background(), "usr_01ME")
}

func spent(at time.Time) *domain.Scan {
	return &domain.Scan{
		ID: "scn", UserID: "usr_01ME", Status: domain.StatusCompleted,
		SubmittedAt: at, AllowanceSpent: true,
	}
}

func TestAllowanceIsFullWithNoScans(t *testing.T) {
	a, err := allowanceFor(nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Remaining != 1 {
		t.Errorf("remaining = %d, want 1", a.Remaining)
	}
	// No reset time when nothing is exhausted. A countdown to capacity the
	// user already has is worse than no countdown.
	if !a.ResetsAt.IsZero() {
		t.Errorf("resetsAt = %v, want zero", a.ResetsAt)
	}
}

// The bug this replaces: rejected and failed scans are stored, and counting
// rows instead of spends would lock someone out for a week of blurry photos
// they were never charged for.
func TestAllowanceIgnoresScansThatCostNothing(t *testing.T) {
	a, err := allowanceFor([]*domain.Scan{
		{ID: "a", Status: domain.StatusRejected, AllowanceSpent: false,
			SubmittedAt: allowanceNow.Add(-time.Hour)},
		{ID: "b", Status: domain.StatusFailed, AllowanceSpent: false,
			SubmittedAt: allowanceNow.Add(-2 * time.Hour)},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Remaining != 1 {
		t.Errorf("remaining = %d, want 1 — neither scan was charged", a.Remaining)
	}
}

// The reset is when the oldest spend AGES OUT of the rolling window, not a flat
// 24 hours. The old Retry-After said 86400 unconditionally, which on a seven-day
// window is wrong by up to six days — and wrong in the direction that sends the
// user back tomorrow to find the button still dead.
func TestAllowanceResetsWhenTheOldestSpendAgesOut(t *testing.T) {
	scannedAt := allowanceNow.Add(-2 * 24 * time.Hour) // two days ago

	a, err := allowanceFor([]*domain.Scan{spent(scannedAt)}, 1)
	if err != nil {
		t.Fatal(err)
	}

	if a.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", a.Remaining)
	}

	want := scannedAt.Add(7 * 24 * time.Hour)
	if !a.ResetsAt.Equal(want) {
		t.Errorf("resetsAt = %v, want %v (five days away, not one)", a.ResetsAt, want)
	}
}

func TestAllowanceIgnoresScansOutsideTheWindow(t *testing.T) {
	a, err := allowanceFor([]*domain.Scan{
		spent(allowanceNow.Add(-8 * 24 * time.Hour)), // last week
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Remaining != 1 {
		t.Errorf("remaining = %d, want 1 — that scan has aged out", a.Remaining)
	}
}

func TestAllowanceNeverGoesNegative(t *testing.T) {
	// Possible if the limit is lowered while someone is mid-period.
	a, err := allowanceFor([]*domain.Scan{
		spent(allowanceNow.Add(-time.Hour)),
		spent(allowanceNow.Add(-2 * time.Hour)),
		spent(allowanceNow.Add(-3 * time.Hour)),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Remaining != 0 {
		t.Errorf("remaining = %d, want 0 — never a negative count on a card", a.Remaining)
	}
}

// ---------------------------------------------------------------------------
// Heatmap
// ---------------------------------------------------------------------------

func heatmapFor(scan *domain.Scan, subject, userID string) ([]byte, error) {
	uc := application.NewGetHeatmap(
		&fakeScans{byID: map[string]*domain.Scan{scan.ID: scan}},
		fakeIdentity{sub: "sub", userID: userID, consented: true},
	)
	return uc.Execute(context.Background(), subject, scan.ID)
}

func TestHeatmapIsServedToItsOwner(t *testing.T) {
	got, err := heatmapFor(&domain.Scan{
		ID: "scn_a", UserID: "usr_01ME", Heatmap: []byte{0xFF, 0xD8, 0xFF},
	}, "sub", "usr_01ME")
	if err != nil {
		t.Fatalf("owner could not read their own overlay: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d bytes, want the stored overlay", len(got))
	}
}

// A scan with no overlay is a tier difference, not a missing resource. The
// client renders a report without one; treating it as "not found" would send it
// down a retry path that can never succeed.
func TestMissingOverlayIsItsOwnError(t *testing.T) {
	_, err := heatmapFor(&domain.Scan{
		ID: "scn_b", UserID: "usr_01ME", Heatmap: nil,
	}, "sub", "usr_01ME")

	if !errors.Is(err, application.ErrNoHeatmap) {
		t.Errorf("got %v, want ErrNoHeatmap", err)
	}
}

// Not found rather than forbidden: confirming a scan id exists tells an
// attacker they guessed one, and a scan id points at a photograph of a face.
func TestSomeoneElsesOverlayIsNotFound(t *testing.T) {
	_, err := heatmapFor(&domain.Scan{
		ID: "scn_c", UserID: "usr_01SOMEONE_ELSE", Heatmap: []byte{0xFF},
	}, "sub", "usr_01ME")

	if !errors.Is(err, domain.ErrScanNotFound) {
		t.Errorf("got %v, want ErrScanNotFound", err)
	}
}

// ExpireHeatmaps is the retention sweep's half of the port. The sweep itself is
// tested in cmd/worker, where both modules are assembled; here it only has to
// satisfy the interface.
func (f *fakeScans) ExpireHeatmaps(context.Context, []string, time.Time) (int64, error) {
	return 0, nil
}
