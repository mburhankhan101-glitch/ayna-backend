// Package ailab adapts AILab Tools Skin Analyze Pro to the domain's Analyzer
// port.
//
// Everything vendor-specific lives here, and the list is longer than a docs
// page suggests. Each item below cost a real run of the spike to learn:
//
//   - The scale runs BACKWARDS. acne_score 45 means severe acne, not mild.
//     Reading it as a problem magnitude inverts every severity in the product.
//   - Redness is red_spot_score, NOT sensitivity_score. On one real face those
//     read 90 (None) and 30 (Severe) — the app would have reported severe
//     redness on clear skin.
//   - return_maps takes named map identifiers, not a boolean. Sending "1"
//     fails every call with UNSUPPORTED_PARAMETER_VALUES, so no maps are
//     requested and FR-4's heatmap is not yet buildable.
//   - total_score runs roughly 70-100, not 0-100, and needs stretching or every
//     user clusters in the top third and the trend looks flat.
//
// See ADR-003. R-1 is still open: Pro scored 98-100 for acne on four faces, two
// of which visibly had acne, and 93 on a fifth with clearly moderate acne. The
// same photo cropped tighter scored 88 -- framing moves the number, which is
// why this adapter now trims before sending.
package ailab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/domain"
	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/photo"
)

const endpoint = "https://www.ailabapi.com/api/portrait/analysis/skin-analysis-pro"

type Analyzer struct {
	key string
	hc  *http.Client
}

func New(key string) *Analyzer {
	return &Analyzer{
		key: key,
		// Comfortably above the measured p95 of 2.9s and well inside the
		// caller's own deadline. A vendor tail latency of 30s must surface as
		// a failed scan, not as a request that outlives the user's patience.
		hc: &http.Client{Timeout: 25 * time.Second},
	}
}

func (a *Analyzer) Name() string { return "ailab-pro" }

// Severity bands, from AILab's published Degree & Score Reference. Adopted as
// the v1 SeverityRuleSet under PD-2, whose values remain open pending review —
// which is why RulesetVersion is stamped on every report.
const (
	bandNone     = 90 // >= this is None
	bandMild     = 70
	bandModerate = 50
)

func severityFor(vendorScore float64) domain.Severity {
	switch {
	case vendorScore >= bandNone:
		return domain.SeverityNone
	case vendorScore >= bandMild:
		return domain.SeverityMild
	case vendorScore >= bandModerate:
		return domain.SeverityModerate
	default:
		return domain.SeveritySevere
	}
}

// fieldFor maps each FR-4 concern to the vendor field that actually carries it.
var fieldFor = map[domain.IssueType]string{
	domain.IssueAcne: "acne_score",

	// red_spot_score, NOT sensitivity_score. Sensitivity is a vendor composite
	// about reactivity; FR-4 promises visible redness, and the two disagreed by
	// 60 points on a real face during the spike.
	domain.IssueRedness: "red_spot_score",

	// Hydration, and NOT inverted a second time. A high water_score already
	// means no dryness; the shared inversion below handles it correctly.
	domain.IssueDryness: "water_score",

	// WATCH: if melanin_score tracks skin colour rather than uneven pigment,
	// this is where tone bias would enter the product. ADR-003 R-2.
	domain.IssueDarkSpots: "melanin_score",

	domain.IssueTexture: "rough_score",
	domain.IssuePores:   "pores_score",
}

func (a *Analyzer) Analyze(ctx context.Context, jpeg []byte) (*domain.Analysis, error) {
	// Normalise the framing before it costs anything. The trimmed bytes are
	// what the vendor sees, so they are also what the overlay must composite
	// onto -- the map comes back at the dimensions it was given.
	//
	// A phone selfie is mostly room: one measured 826x1280 frame carried the
	// face across 4% of its pixels and scored "Clear" for acne, while a tighter
	// crop of the same photo scored "Mild". Trimming here rather than in the app
	// means the ratio can be tuned from a deploy instead of a store release,
	// which matters while the right ratio is still an open question, and means
	// every client version sends comparable framing -- without which FR-8 trends
	// move with how far the user held their arm.
	//
	// Width is never touched, so no face can lose its edges. See the photo
	// package for why nothing smarter happens without a real face detector.
	jpeg = photo.TrimTall(jpeg)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("image", "scan.jpg")
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(jpeg); err != nil {
		return nil, err
	}
	// return_maps takes NAMED IDENTIFIERS, comma-separated, not a boolean.
	// Sending "1" failed every call with UNSUPPORTED_PARAMETER_VALUES.
	//
	// Only red_area is requested. The vendor offers five that map to FR-4
	// concerns and the other four were rejected on inspection: brown_area and
	// water_area render the WHOLE FACE (brown_area on a Fitzpatrick IV subject
	// is the R-2 tone risk made visible), while rough_area and
	// texture_enhanced_pores are sparse stroke annotations in a different
	// visual language and are usually blank on clear skin.
	//
	// There is no acne map. The vendor does not offer one, which is a third
	// independent signal that acne is what it handles worst (R-1).
	_ = w.WriteField("return_maps", "red_area")
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("ailabapi-api-key", a.key)

	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ailab: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var parsed struct {
		ErrorCode    int    `json:"error_code"`
		ErrorCodeStr string `json:"error_code_str"`
		ErrorMsg     string `json:"error_msg"`
		Result       struct {
			ScoreInfo map[string]json.RawMessage `json:"score_info"`
			SkinAge   *struct {
				Value int `json:"value"`
			} `json:"skin_age"`
			FaceMaps map[string]string `json:"face_maps"`

			// The eye rectangles live here, not on result. Found by reading a
			// saved response rather than assuming -- the docs give field names
			// without their parents, which has cost a wrong path three times.
			DarkCircleMark struct {
				LeftEye  photo.Rect `json:"left_eye_rect"`
				RightEye photo.Rect `json:"right_eye_rect"`
			} `json:"dark_circle_mark"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("ailab: unreadable response: %w", err)
	}

	if parsed.ErrorCode != 0 {
		// Vendor errors split two ways, and the split decides whether the user
		// is charged. Anything about the photo is the user's to fix and costs
		// them nothing; anything else is our problem and also costs them
		// nothing, but is not something a retake will solve.
		if r, ok := rejectionFor(parsed.ErrorCodeStr); ok {
			return nil, &domain.RejectedError{Reason: r}
		}
		return nil, fmt.Errorf("ailab: %s: %s", parsed.ErrorCodeStr, parsed.ErrorMsg)
	}

	scores := parsed.Result.ScoreInfo
	if len(scores) == 0 {
		// A 200 with no scores means the vendor looked and found nothing to
		// look at. Treated as a rejection rather than a failure so the user
		// gets a retake prompt instead of an apology.
		return nil, &domain.RejectedError{Reason: domain.RejectNoFace}
	}

	issues := make([]domain.Issue, 0, len(domain.AllIssueTypes))
	for _, it := range domain.AllIssueTypes {
		issues = append(issues, issueFrom(it, scores))
	}

	out := &domain.Analysis{Issues: issues}

	// The redness overlay, masked and composited here so nothing downstream
	// has to know the vendor draws on white or that lips read as redness.
	out.Heatmap = rednessOverlay(
		jpeg,
		parsed.Result.FaceMaps["red_area"],
		parsed.Result.DarkCircleMark.LeftEye,
		parsed.Result.DarkCircleMark.RightEye,
	)

	if total, ok := numFrom(scores["total_score"]); ok {
		out.OverallScore = stretch(total)
	}
	if parsed.Result.SkinAge != nil {
		v := parsed.Result.SkinAge.Value
		out.SkinAge = &v
	}

	return out, nil
}

func issueFrom(it domain.IssueType, scores map[string]json.RawMessage) domain.Issue {
	raw, present := scores[fieldFor[it]]
	if !present {
		// Unknown, never "none". Telling someone they have no redness when
		// nothing ever looked is the worst failure this product can produce,
		// and it is the one a zero-value default produces by accident.
		return domain.Issue{
			Type:           it,
			Severity:       domain.SeverityUnknown,
			SeverityLevels: 4,
		}
	}

	vendor, ok := numFrom(raw)
	if !ok {
		return domain.Issue{
			Type:           it,
			Severity:       domain.SeverityUnknown,
			SeverityLevels: 4,
		}
	}

	// The inversion, in exactly one place. The vendor counts health; the
	// product counts problems.
	magnitude := int(100 - vendor)
	if magnitude < 0 {
		magnitude = 0
	}
	if magnitude > 100 {
		magnitude = 100
	}

	return domain.Issue{
		Type:           it,
		Severity:       severityFor(vendor),
		Score:          &magnitude,
		SeverityLevels: 4,
	}
}

// stretch rescales the vendor's 70-100 band onto a usable 0-100.
//
// total_score is documented as running 70-100. Showing it raw squeezes every
// user into the top third of the scale, which makes FR-8's trend line look
// flatter than the underlying change actually is.
func stretch(total float64) int {
	const lo, hi = 70.0, 100.0
	v := (total - lo) / (hi - lo) * 100
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return int(v)
	}
}

func numFrom(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	// Some fields arrive as an object carrying a value.
	var obj struct {
		Value *float64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Value != nil {
		return *obj.Value, true
	}
	return 0, false
}

// rejectionFor maps the vendor's error codes that mean "the photo is the
// problem" onto something the user can act on.
func rejectionFor(code string) (domain.RejectionReason, bool) {
	switch code {
	case "FACE_NOT_DETECTED", "NO_FACE_DETECTED", "ERROR_FACE_NOT_FOUND":
		return domain.RejectNoFace, true
	case "FACE_TOO_SMALL":
		return domain.RejectFaceTooSmall, true
	case "IMAGE_TOO_DARK":
		return domain.RejectTooDark, true
	case "IMAGE_BLURRY":
		return domain.RejectBlurry, true
	case "INVALID_IMAGE", "IMAGE_ERROR", "UNSUPPORTED_IMAGE_FORMAT":
		return domain.RejectNoFace, true
	}
	return "", false
}

// rednessOverlay turns the vendor's red_area map into something showable.
//
// Three steps, each covering a way the raw map misleads:
//
//  1. Decode it out of base64.
//  2. Blank the mouth. Lips are the reddest thing on a face and are meant to
//     be; left in, they are the user's "worst redness".
//  3. Multiply it onto the photo, so white reads as nothing and the tint
//     darkens only where there is a finding.
//
// Returns nil at the first sign of trouble. A report with no overlay is a tier
// the product already ships; a wrong overlay is a false claim about a face.
func rednessOverlay(baseJPEG []byte, mapB64 string, left, right photo.Rect) []byte {
	if mapB64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(mapB64)
	if err != nil {
		return nil
	}
	return photo.Composite(baseJPEG, photo.MaskMouth(raw, left, right))
}
