package application

import (
	"context"
	"errors"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/domain"
	"github.com/ayna/ayna-backend/internal/platform/id"
)

// Clock is injected so tests do not depend on wall time.
type Clock func() time.Time

var SystemClock Clock = time.Now

// Identity resolves the authenticated caller and reports their consent.
//
// A narrow port rather than an import of the iam module: the boundary lint
// forbids one module reaching into another's internals, and this is the entire
// surface skinanalysis needs from identity. The API wires the two together;
// neither module knows the other exists.
//
// Both facts come back in one call because they are always wanted together and
// answered by the same read. Splitting them would mean two round trips on the
// hottest path in the feature, for no gain in clarity.
type Identity interface {
	Resolve(ctx context.Context, subject string) (userID string, hasConsent bool, err error)
}

// Disclaimer is served with every report (NFR-6).
//
// Server-supplied so the wording can be corrected without shipping a release —
// which matters for the one sentence in the product that carries medical
// weight.
const Disclaimer = "Ayna gives you an impression of your skin, not a diagnosis. " +
	"For anything that worries you, see a dermatologist."

// RulesetVersion is the SeverityRuleSet these severities were graded under.
//
// PD-2 leaves the values open pending medical review, so this is stamped on
// every report: when the thresholds change, old reports stay explicable rather
// than being retroactively reinterpreted.
const RulesetVersion = "ailab-pro-degree-2026-08-24"

// DefaultWeeklyAllowance is how many completed scans a free user gets per
// rolling week (PD-3).
const DefaultWeeklyAllowance = 1

// SubmitScan takes a photo and returns a finished scan.
//
// THE ANALYSIS RUNS INLINE, inside the request. That is a deliberate deviation
// from ADR-002's separate-deployable topology, recorded here because it is the
// kind of shortcut that becomes invisible:
//
//   - Measured p95 for the vendor is 2.9s, well inside NFR-2's 8s budget, so a
//     queue would add a worker cold start and make the user wait longer.
//   - What a queue actually buys is retries, vendor-outage resilience and spend
//     control under a spike. At present traffic none of those bind.
//   - The middle option -- return 202 and finish in a goroutine -- is NOT
//     available on Cloud Run: CPU is only allocated during a request, so work
//     outliving the response gets throttled or killed.
//
// The seam is preserved: everything below calls domain.Analyzer, so moving to
// the worker is an adapter change and touches no client. Revisit when scans
// become frequent enough that concurrent paid calls need a ceiling.
type SubmitScan struct {
	scans    domain.ScanRepository
	analyze  domain.Analyzer
	identity Identity
	now      Clock

	// allowance is injected rather than read from a constant so development
	// can raise it. One scan per rolling week is right for users and makes the
	// feature untestable: a single test scan locks you out for seven days.
	allowance int
}

func NewSubmitScan(
	scans domain.ScanRepository,
	analyze domain.Analyzer,
	identity Identity,
	now Clock,
	allowance int,
) *SubmitScan {
	if allowance <= 0 {
		allowance = DefaultWeeklyAllowance
	}
	return &SubmitScan{
		scans: scans, analyze: analyze, identity: identity,
		now: now, allowance: allowance,
	}
}

func (uc *SubmitScan) Execute(
	ctx context.Context, subject, idempotencyKey string, jpeg []byte,
) (*domain.Scan, error) {
	if idempotencyKey == "" {
		return nil, domain.ErrIdempotencyKeyReq
	}
	if len(jpeg) == 0 {
		return nil, domain.ErrPhotoEmpty
	}
	if len(jpeg) > domain.MaxPhotoBytes {
		return nil, domain.ErrPhotoTooLarge
	}

	// Identity first: an idempotency key is scoped per user, so there is no
	// lookup to do until we know who is asking.
	userID, consented, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return nil, err
	}

	// A retry of a request that already succeeded returns the original scan,
	// and is checked before consent and allowance on purpose. Someone who
	// consented, scanned, and then revoked must still be able to read back the
	// scan they already paid for -- refusing it would be punishing them for a
	// dropped connection.
	if existing, err := uc.scans.FindByIdempotencyKey(ctx, userID, idempotencyKey); err == nil {
		return existing, nil
	} else if !errors.Is(err, domain.ErrScanNotFound) {
		return nil, err
	}

	// Consent before analysis, always. FR-1 is not a formality: this is the
	// point where a face photo leaves the country and reaches a third party.
	if !consented {
		return nil, domain.ErrNoConsent
	}

	// Allowance is checked against scans that actually spent it, so a week of
	// blurry photos does not lock someone out.
	spent, err := uc.scans.CountSpentSince(ctx, userID, uc.periodStart())
	if err != nil {
		return nil, err
	}
	if spent >= uc.allowance {
		return nil, domain.ErrNoAllowance
	}

	now := uc.now()
	scan := domain.NewScan(id.New(id.PrefixScan), userID, now)

	if err := uc.scans.Save(ctx, scan, idempotencyKey); err != nil {
		if errors.Is(err, domain.ErrDuplicateSubmission) {
			// A concurrent retry claimed the key first. Read back what it
			// created rather than failing -- the user pressed once.
			return uc.scans.FindByIdempotencyKey(ctx, userID, idempotencyKey)
		}
		return nil, err
	}

	result, err := uc.analyze.Analyze(ctx, jpeg)

	var rejected *domain.RejectedError
	switch {
	case errors.As(err, &rejected):
		scan.Reject(rejected.Reason, uc.now())
	case err != nil:
		// A vendor outage is not the user's fault and does not spend their
		// allowance. The error is swallowed into the scan's state rather than
		// returned, so the client polls to a definite answer instead of seeing
		// a 500 for something it can do nothing about.
		scan.Fail(uc.now())
	default:
		scan.Complete(
			uc.report(scan.ID, result, uc.now()),
			result.Heatmap,
			uc.analyze.Name(), RulesetVersion, uc.now(),
		)
	}

	if err := uc.scans.Update(ctx, scan); err != nil {
		return nil, err
	}
	return scan, nil
}

func (uc *SubmitScan) report(scanID string, a *domain.Analysis, now time.Time) *domain.Report {
	return &domain.Report{
		ScanID:         scanID,
		OverallScore:   a.OverallScore,
		SkinAge:        a.SkinAge,
		Issues:         a.Issues,
		RulesetVersion: RulesetVersion,
		Disclaimer:     Disclaimer,
		GeneratedAt:    now,
	}
}

// periodStart is the beginning of the current allowance week.
//
// A rolling seven days rather than a calendar week: a calendar reset lets
// someone scan on Sunday night and again on Monday morning, which is two paid
// calls in twelve hours from a "weekly" allowance.
func (uc *SubmitScan) periodStart() time.Time {
	return uc.now().Add(-allowancePeriod)
}

// GetScan reads one back, scoped to its owner.
//
// It takes the SUBJECT, not a user id, and resolves it — because the caller is
// an HTTP handler and the only thing a handler has is the authenticated
// subject. The first version of this took a `userID` parameter and the handler
// dutifully passed the subject into it, so `s.UserID != userID` compared
// `usr_01...` against `google-oauth2|...` and every single read 404'd. The
// scan existed, the vendor had been paid, and the API said it did not exist.
//
// The parameter name was the whole bug. Taking `subject` makes the type of the
// argument obvious at the call site and makes the mistake unrepresentable.
type GetScan struct {
	scans    domain.ScanRepository
	identity Identity
}

func NewGetScan(scans domain.ScanRepository, identity Identity) *GetScan {
	return &GetScan{scans: scans, identity: identity}
}

func (uc *GetScan) Execute(ctx context.Context, subject, scanID string) (*domain.Scan, error) {
	userID, _, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return nil, err
	}

	s, err := uc.scans.FindByID(ctx, scanID)
	if err != nil {
		return nil, err
	}
	if s.UserID != userID {
		// Not found, not forbidden. Distinguishing the two tells an attacker
		// which scan ids exist, and a scan id is a face photo.
		return nil, domain.ErrScanNotFound
	}
	return s, nil
}

// MaxListLimit caps a history request.
//
// This backs a phone list, not an export. An unbounded limit would also make a
// small account cheap to enumerate, and every row is a record of someone's face
// being analysed.
const MaxListLimit = 50

// DefaultListLimit is what a client gets when it does not ask.
const DefaultListLimit = 20

// ListScans returns a user's past scans, newest first.
type ListScans struct {
	scans    domain.ScanRepository
	identity Identity
}

func NewListScans(scans domain.ScanRepository, identity Identity) *ListScans {
	return &ListScans{scans: scans, identity: identity}
}

func (uc *ListScans) Execute(ctx context.Context, subject string, limit int) ([]*domain.Scan, error) {
	// Clamped rather than rejected. A client asking for 500 wants "as many as
	// I can have", and a 400 on a read that could have succeeded is friction
	// for no protection the cap does not already provide.
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	userID, _, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return nil, err
	}
	return uc.scans.ListRecent(ctx, userID, limit)
}

// TrendPoint is one week's reading (FR-8).
type TrendPoint struct {
	WeekStart    time.Time
	OverallScore int
	SkinAge      *int
}

// GetTrend builds the score history.
//
// Weekly rather than per-scan, because the allowance is weekly: a per-scan
// x-axis would space points by how often someone happened to scan, so a gap
// would read as a flat stretch rather than as missing data.
type GetTrend struct {
	scans    domain.ScanRepository
	identity Identity
	now      Clock
}

func NewGetTrend(scans domain.ScanRepository, identity Identity, now Clock) *GetTrend {
	return &GetTrend{scans: scans, identity: identity, now: now}
}

// FreeTrendWeeks is how far back a free user can see.
//
// Truncated rather than refused: the paywall works because you can see what
// you are missing. The response says how many points exist in total so the
// client can say "6 more behind Plus" instead of pretending there is nothing.
const FreeTrendWeeks = 6

func (uc *GetTrend) Execute(ctx context.Context, subject string, weeks int) ([]TrendPoint, int, error) {
	if weeks <= 0 {
		weeks = FreeTrendWeeks
	}

	userID, _, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return nil, 0, err
	}

	// Enough rows to cover the window with room for multiple scans in a week.
	all, err := uc.scans.ListRecent(ctx, userID, MaxListLimit)
	if err != nil {
		return nil, 0, err
	}

	// One point per week, using that week's LATEST completed scan.
	//
	// Latest rather than an average: the user is asking "where am I now",
	// and averaging a Monday retake with a Saturday one answers a question
	// nobody posed. ListRecent is newest-first, so the first scan seen for a
	// week is already the one to keep.
	byWeek := map[time.Time]TrendPoint{}
	var order []time.Time

	for _, s := range all {
		if s.Status != domain.StatusCompleted || s.Report == nil {
			continue
		}
		wk := weekStart(s.SubmittedAt)
		if _, seen := byWeek[wk]; seen {
			continue
		}
		byWeek[wk] = TrendPoint{
			WeekStart:    wk,
			OverallScore: s.Report.OverallScore,
			SkinAge:      s.Report.SkinAge,
		}
		order = append(order, wk)
	}

	total := len(order)

	// Oldest first for the chart; `order` was built newest-first.
	points := make([]TrendPoint, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		points = append(points, byWeek[order[i]])
	}
	if len(points) > weeks {
		points = points[len(points)-weeks:]
	}

	return points, total, nil
}

// weekStart normalises a timestamp to the Monday of its week.
//
// PD-5 counts streaks in the user's own timezone; this is the same reasoning
// applied to the chart, so a scan and the week it lands in agree. Today it uses
// the timestamp's own location, which is UTC from the database — a known
// simplification to correct when the user's timezone is threaded through.
func weekStart(t time.Time) time.Time {
	d := int(t.Weekday())
	if d == 0 {
		d = 7 // Sunday closes the week rather than opening it
	}
	y, m, day := t.AddDate(0, 0, -(d - 1)).Date()
	return time.Date(y, m, day, 0, 0, 0, 0, t.Location())
}

// Allowance is what a user has left this period, and when it comes back.
type Allowance struct {
	Remaining int
	Limit     int

	// ResetsAt is when the next scan becomes available.
	//
	// Zero when scans are available now — a reset time for something that is
	// not exhausted is a countdown to nothing, and a client showing it would
	// tell a user to wait for capacity they already have.
	ResetsAt time.Time
}

// GetAllowance reports the user's remaining scans.
//
// Exists because the profile endpoint used to hardcode `scansRemaining: 1` and
// a reset date of "now plus seven days", recomputed on every request. The app
// therefore believed a scan was always available, offered the button, and the
// user discovered the limit as a 429 after taking a photo — the worst possible
// moment, since by then they have already done the work.
type GetAllowance struct {
	scans domain.ScanRepository
	now   Clock
	limit int
}

func NewGetAllowance(scans domain.ScanRepository, now Clock, limit int) *GetAllowance {
	if limit <= 0 {
		limit = DefaultWeeklyAllowance
	}
	return &GetAllowance{scans: scans, now: now, limit: limit}
}

func (uc *GetAllowance) Execute(ctx context.Context, userID string) (Allowance, error) {
	window := uc.now().Add(-allowancePeriod)

	recent, err := uc.scans.ListRecent(ctx, userID, MaxListLimit)
	if err != nil {
		return Allowance{}, err
	}

	// Only scans that actually spent the allowance. Rejections and failures are
	// stored and shown in history, but neither cost the user anything.
	var spent []time.Time
	for _, s := range recent {
		if s.AllowanceSpent && s.SubmittedAt.After(window) {
			spent = append(spent, s.SubmittedAt)
		}
	}

	out := Allowance{Limit: uc.limit, Remaining: uc.limit - len(spent)}
	if out.Remaining < 0 {
		out.Remaining = 0
	}
	if out.Remaining > 0 {
		return out, nil
	}

	// Exhausted. The next scan unlocks when the OLDEST spend still inside the
	// window ages out of it — not a flat "24 hours", which is what the old
	// Retry-After header claimed and which could be six days wrong on a rolling
	// seven-day period.
	//
	// ListRecent is newest-first, so the last entry is the oldest.
	out.ResetsAt = spent[len(spent)-1].Add(allowancePeriod)
	return out, nil
}

// allowancePeriod is the rolling window. Rolling rather than a calendar week:
// a calendar reset lets someone scan on Sunday night and again on Monday
// morning, which is two paid calls in twelve hours from a "weekly" allowance.
const allowancePeriod = 7 * 24 * time.Hour

// MyAllowance is [GetAllowance] for a caller identified by SUBJECT.
//
// A separate type rather than an overload, because the two arguments are both
// strings and confusing them is exactly the bug that made every scan read
// return 404: a handler passed the Auth0 subject into a parameter named
// userID, and the comparison could never match. Two named types make the
// mistake impossible to write.
type MyAllowance struct {
	get      *GetAllowance
	identity Identity
}

func NewMyAllowance(get *GetAllowance, identity Identity) *MyAllowance {
	return &MyAllowance{get: get, identity: identity}
}

func (uc *MyAllowance) Execute(ctx context.Context, subject string) (Allowance, error) {
	userID, _, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return Allowance{}, err
	}
	return uc.get.Execute(ctx, userID)
}

// ErrNoHeatmap means the scan produced no overlay.
//
// Distinct from "scan not found" so the client can render a report without one
// rather than treating a tier difference as a missing resource.
var ErrNoHeatmap = errors.New("no heatmap for this scan")

// GetHeatmap serves the redness overlay, scoped to its owner.
type GetHeatmap struct {
	scans    domain.ScanRepository
	identity Identity
}

func NewGetHeatmap(scans domain.ScanRepository, identity Identity) *GetHeatmap {
	return &GetHeatmap{scans: scans, identity: identity}
}

func (uc *GetHeatmap) Execute(ctx context.Context, subject, scanID string) ([]byte, error) {
	// Subject, resolved -- not a userID parameter. Taking the wrong one here is
	// the bug that 404'd every scan read after the vendor had been paid.
	userID, _, err := uc.identity.Resolve(ctx, subject)
	if err != nil {
		return nil, err
	}

	heatmap, owner, err := uc.scans.FindHeatmap(ctx, scanID)
	if err != nil {
		return nil, err
	}
	if owner != userID {
		// Not found, not forbidden. Confirming a scan id exists tells an
		// attacker they have guessed one, and a scan id points at a face.
		return nil, domain.ErrScanNotFound
	}
	if len(heatmap) == 0 {
		return nil, ErrNoHeatmap
	}
	return heatmap, nil
}
