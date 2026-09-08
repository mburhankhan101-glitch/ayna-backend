package domain

import (
	"errors"
	"time"
)

var (
	ErrScanNotFound      = errors.New("scan not found")
	ErrNoConsent         = errors.New("photo processing has not been consented to")
	ErrNoAllowance       = errors.New("no scans remaining in this period")
	ErrPhotoTooLarge     = errors.New("photo is too large")
	ErrPhotoEmpty        = errors.New("photo is empty")
	ErrIdempotencyKeyReq = errors.New("Idempotency-Key is required")

	// ErrDuplicateSubmission means this exact request was already accepted.
	// Not an error to surface: the caller reads back the original scan and
	// returns it, which is what makes a retry free rather than a second charge.
	ErrDuplicateSubmission = errors.New("scan already submitted with this key")
)

// MaxPhotoBytes caps an upload.
//
// A modern phone camera at full resolution produces 8MB+; this app sends
// ResolutionPreset.high, which lands around 300KB–1.5MB. 8MB leaves generous
// headroom while still refusing a client that uploads something that is not a
// selfie, before it reaches a paid endpoint.
const MaxPhotoBytes = 8 << 20

type Status string

const (
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusRejected   Status = "rejected"
	StatusFailed     Status = "failed"
)

// Stage is the named step the analysis is on.
//
// Exists so the client can say what is happening rather than spin. Today the
// whole analysis runs inside POST /scans, so every transition lands before the
// response and the client observes only the last one. The vocabulary is defined
// now so moving the work to the worker later changes no contract and no client.
type Stage string

const (
	StageReceived       Stage = "received"
	StageQualityChecked Stage = "quality_checked"
	StageScoring        Stage = "scoring"
	StageWritingReport  Stage = "writing_report"
)

// RejectionReason is why a photo could not be analysed.
//
// Every value is something the user can fix, which is the test for whether it
// belongs here. A vendor outage is not a rejection — that is StatusFailed, and
// it must not spend the user's allowance either.
type RejectionReason string

const (
	RejectBlurry       RejectionReason = "blurry"
	RejectTooDark      RejectionReason = "too_dark"
	RejectNoFace       RejectionReason = "no_face_detected"
	RejectFaceTooSmall RejectionReason = "face_too_small"
	RejectOccluded     RejectionReason = "occluded"
)

type Severity string

const (
	SeverityUnknown  Severity = "unknown"
	SeverityNone     Severity = "none"
	SeverityMild     Severity = "mild"
	SeverityModerate Severity = "moderate"
	SeveritySevere   Severity = "severe"
)

type IssueType string

const (
	IssueAcne      IssueType = "acne"
	IssueRedness   IssueType = "redness"
	IssueDryness   IssueType = "dryness"
	IssueDarkSpots IssueType = "dark_spots"
	IssueTexture   IssueType = "texture"
	IssuePores     IssueType = "pores"
)

// AllIssueTypes is the fixed set FR-4 promises. A report carries all six or it
// is not a report — a concern the analysis could not assess appears with
// SeverityUnknown, never omitted.
var AllIssueTypes = []IssueType{
	IssueAcne, IssueRedness, IssueDryness,
	IssueDarkSpots, IssueTexture, IssuePores,
}

type Issue struct {
	Type     IssueType `json:"type"`
	Severity Severity  `json:"severity"`

	// Score is problem magnitude, 0-100, HIGHER IS WORSE.
	//
	// This is the inverse of AILab's own scale, where a high score means
	// healthy skin. The inversion happens in the vendor adapter so nothing
	// above it has to remember which way a given vendor counts — that
	// confusion already produced one real bug during the spike.
	Score *int `json:"score,omitempty"`

	Confidence *float64 `json:"confidence,omitempty"`

	// SeverityLevels is how many levels the source can actually distinguish.
	//
	// On the wire because of ADR-003: one AILab endpoint returns 0-100 scores
	// (four levels) and the other returns presence flags (two). A client
	// drawing a four-step bar for a two-level signal is claiming precision that
	// does not exist, so it has to be told rather than assume.
	SeverityLevels int `json:"severityLevels"`
}

type Report struct {
	ScanID       string `json:"scanId"`
	OverallScore int    `json:"overallScore"`

	// SkinAge is nil on tiers or vendors without a reading. Rendering the
	// absence is required: substituting chronological age produces "right in
	// step", which is a fabricated result rather than a missing one.
	SkinAge          *int `json:"skinAge,omitempty"`
	ChronologicalAge *int `json:"chronologicalAge,omitempty"`

	Issues         []Issue   `json:"issues"`
	HeatmapURL     *string   `json:"heatmapUrl,omitempty"`
	PhotoURL       *string   `json:"photoUrl,omitempty"`
	RulesetVersion string    `json:"rulesetVersion"`
	Disclaimer     string    `json:"disclaimer"`
	GeneratedAt    time.Time `json:"generatedAt"`
}

// Scan is the aggregate.
type Scan struct {
	ID              string
	UserID          string
	Status          Status
	Stage           Stage
	AllowanceSpent  bool
	RejectionReason *RejectionReason
	Report          *Report

	// Heatmap is the composited redness overlay, or nil. Held on the aggregate
	// rather than inside Report because it is bytes served from its own
	// endpoint, not a field of the JSON document.
	Heatmap []byte

	RulesetVersion *string
	Provider       *string
	SubmittedAt    time.Time
	CompletedAt    *time.Time
}

// NewScan starts one, in the only state a new scan can legally be in.
func NewScan(id, userID string, now time.Time) *Scan {
	return &Scan{
		ID:          id,
		UserID:      userID,
		Status:      StatusProcessing,
		Stage:       StageReceived,
		SubmittedAt: now,
	}
}

// Complete records a successful analysis.
//
// This is the only path that sets AllowanceSpent, and it is set here rather
// than at submission on purpose: charging on submit would bill the user for
// photos the vendor refused.
func (s *Scan) Complete(
	r *Report, heatmap []byte, provider, rulesetVersion string, now time.Time,
) {
	s.Heatmap = heatmap
	s.Status = StatusCompleted
	s.Stage = StageWritingReport
	s.Report = r
	s.Provider = &provider
	s.RulesetVersion = &rulesetVersion
	s.AllowanceSpent = true
	s.CompletedAt = &now
}

// Reject records a photo the user can retake.
//
// Explicitly does not spend the allowance. The vendor does not bill a request
// it refused, so charging the user for one would be taking money for nothing —
// and it is the single most damaging thing this feature could do quietly.
func (s *Scan) Reject(reason RejectionReason, now time.Time) {
	s.Status = StatusRejected
	s.RejectionReason = &reason
	s.AllowanceSpent = false
	s.CompletedAt = &now
}

// Fail records something that was not the user's fault: a vendor outage, a
// timeout, a malformed response. Also free, for the same reason.
func (s *Scan) Fail(now time.Time) {
	s.Status = StatusFailed
	s.AllowanceSpent = false
	s.CompletedAt = &now
}

func (s *Scan) IsTerminal() bool { return s.Status != StatusProcessing }
