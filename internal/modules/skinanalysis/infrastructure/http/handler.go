// Package http is the skin-analysis transport layer.
//
// Translation only: HTTP in, use case call, HTTP out. The allowance rule, the
// consent gate and the decision not to charge for a rejected photo all live in
// the domain and application layers — this package's job is to make each of
// them look like the status code the OpenAPI contract promises.
package http

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/application"
	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/domain"
	"github.com/ayna/ayna-backend/internal/platform/auth"
	"github.com/ayna/ayna-backend/internal/platform/httpx"
)

type Handler struct {
	submit    *application.SubmitScan
	get       *application.GetScan
	list      *application.ListScans
	trend     *application.GetTrend
	allowance *application.MyAllowance
	heatmap   *application.GetHeatmap
	log       *slog.Logger
}

func NewHandler(
	submit *application.SubmitScan,
	get *application.GetScan,
	list *application.ListScans,
	trend *application.GetTrend,
	allowance *application.MyAllowance,
	heatmap *application.GetHeatmap,
	log *slog.Logger,
) *Handler {
	return &Handler{
		submit: submit, get: get, list: list, trend: trend,
		allowance: allowance, heatmap: heatmap, log: log,
	}
}

func (h *Handler) Mount(mux *http.ServeMux, authn auth.Authenticator) {
	protected := auth.Middleware(authn)

	mux.Handle("POST /v1/scans", protected(http.HandlerFunc(h.submitScan)))
	mux.Handle("GET /v1/scans", protected(http.HandlerFunc(h.listScans)))
	mux.Handle("GET /v1/profile/me/trend", protected(http.HandlerFunc(h.getTrend)))
	mux.Handle("GET /v1/scans/{scanId}", protected(http.HandlerFunc(h.getScan)))
	mux.Handle("GET /v1/scans/{scanId}/report", protected(http.HandlerFunc(h.getReport)))
	mux.Handle("GET /v1/scans/{scanId}/heatmap", protected(http.HandlerFunc(h.getHeatmap)))
}

// ---------------------------------------------------------------------------
// Wire types. Separate from the domain so a field rename in one is not
// silently a breaking API change in the other.
// ---------------------------------------------------------------------------

// EstimatedSeconds is what the client paces its progress against.
//
// Measured rather than guessed: p95 across the spike was 2.9s. Served from here
// so it can be corrected from production timings without shipping an app
// release -- the client must not hardcode a number only the server can know.
const EstimatedSeconds = 3

type statusResponse struct {
	ScanID          string  `json:"scanId"`
	Status          string  `json:"status"`
	Stage           *string `json:"stage,omitempty"`
	AllowanceSpent  bool    `json:"allowanceSpent"`
	RejectionReason *string `json:"rejectionReason,omitempty"`
	EstimatedSecs   *int    `json:"estimatedSeconds,omitempty"`
}

func statusOf(s *domain.Scan) statusResponse {
	out := statusResponse{
		ScanID:         s.ID,
		Status:         string(s.Status),
		AllowanceSpent: s.AllowanceSpent,
	}
	if s.Stage != "" {
		v := string(s.Stage)
		out.Stage = &v
	}
	if s.RejectionReason != nil {
		v := string(*s.RejectionReason)
		out.RejectionReason = &v
	}
	if s.Status == domain.StatusProcessing {
		// Only while there is still something to wait for. Sending an estimate
		// alongside a finished scan invites a client to keep counting down
		// after the answer has already arrived.
		e := EstimatedSeconds
		out.EstimatedSecs = &e
	}
	return out
}

// ---------------------------------------------------------------------------

func (h *Handler) submitScan(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "idempotency-key-required",
			"Idempotency-Key is required",
			"Every scan costs a paid analysis and one of your weekly scans. "+
				"The key is what makes a retry free rather than a second charge.")
		return
	}

	// multipart, not base64 JSON. A base64 body inflates a 1MB photo to 1.4MB
	// on a connection this product cannot assume is good, and buffers the whole
	// thing as a string on both ends.
	if err := r.ParseMultipartForm(domain.MaxPhotoBytes); err != nil {
		httpx.WriteBadRequest(w, r, "Send the photo as multipart/form-data in a 'photo' field.")
		return
	}

	file, _, err := r.FormFile("photo")
	if err != nil {
		httpx.WriteBadRequest(w, r, "Missing 'photo' field.")
		return
	}
	defer file.Close()

	// LimitReader as well as the ParseMultipartForm cap: the form limit governs
	// what is buffered in memory, not what a single part may contain.
	jpeg, err := io.ReadAll(io.LimitReader(file, domain.MaxPhotoBytes+1))
	if err != nil {
		httpx.WriteInternal(w, r, h.log, err)
		return
	}

	scan, err := h.submit.Execute(r.Context(), sub, key, jpeg)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// 202 even though the work is already done.
	//
	// The contract promises submit-then-poll, and the client is written to it.
	// Returning 200 with the report inline would be honest about today's
	// synchronous implementation and would break every client the day the
	// analysis moves to the worker. The shape of the API should describe the
	// contract, not the current deployment.
	w.Header().Set("Location", "/v1/scans/"+scan.ID)
	httpx.WriteJSON(w, http.StatusAccepted, statusOf(scan))
}

func (h *Handler) getScan(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	scan, err := h.get.Execute(r.Context(), sub, r.PathValue("scanId"))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusOf(scan))
}

func (h *Handler) getReport(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	scan, err := h.get.Execute(r.Context(), sub, r.PathValue("scanId"))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	if scan.Report == nil {
		// 409, not 404. The scan exists; it just has no report yet, and telling
		// the client "not found" would send it down a retry path that never
		// succeeds instead of one that waits.
		httpx.WriteProblem(w, r, http.StatusConflict, "report-not-ready",
			"No report yet",
			"This scan has not finished, or it was rejected. Check its status first.")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, scan.Report)
}

// writeDomainError maps domain errors onto the contract's statuses.
//
// One place, so a new error cannot quietly become a 500 in one handler and a
// 400 in another.
func (h *Handler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrScanNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "scan-not-found",
			"Scan not found", "No scan with that id belongs to you.")

	case errors.Is(err, domain.ErrNoConsent):
		httpx.WriteProblem(w, r, http.StatusForbidden, "consent-required",
			"Consent required",
			"Photo processing needs your explicit consent before a scan can run.")

	case errors.Is(err, domain.ErrNoAllowance):
		// Handled separately from the rest so it can carry resetsAt. This is a
		// rate limit, and the one thing the user wants is WHEN -- "try again
		// later" from an app that knows the exact minute is just withholding.
		h.writeAllowanceExhausted(w, r)

	case errors.Is(err, domain.ErrPhotoTooLarge):
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "photo-too-large",
			"That photo is too large", "Photos must be under 8MB.")

	case errors.Is(err, domain.ErrPhotoEmpty),
		errors.Is(err, domain.ErrIdempotencyKeyReq):
		httpx.WriteBadRequest(w, r, err.Error())

	default:
		httpx.WriteInternal(w, r, h.log, err)
	}
}

type summaryResponse struct {
	ScanID          string  `json:"scanId"`
	Status          string  `json:"status"`
	SubmittedAt     string  `json:"submittedAt"`
	AllowanceSpent  bool    `json:"allowanceSpent"`
	OverallScore    *int    `json:"overallScore,omitempty"`
	SkinAge         *int    `json:"skinAge,omitempty"`
	RejectionReason *string `json:"rejectionReason,omitempty"`
}

func summaryOf(s *domain.Scan) summaryResponse {
	out := summaryResponse{
		ScanID:         s.ID,
		Status:         string(s.Status),
		SubmittedAt:    s.SubmittedAt.UTC().Format(time.RFC3339),
		AllowanceSpent: s.AllowanceSpent,
	}
	// Score and age only when there is a report. A list row must be able to
	// say "no result" rather than showing a zero that looks like a reading.
	if s.Report != nil {
		v := s.Report.OverallScore
		out.OverallScore = &v
		out.SkinAge = s.Report.SkinAge
	}
	if s.RejectionReason != nil {
		v := string(*s.RejectionReason)
		out.RejectionReason = &v
	}
	return out
}

func (h *Handler) listScans(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	// An unparseable limit falls back to the default rather than 400ing. The
	// parameter is a convenience on a read; refusing the whole request because
	// a query string was malformed serves nobody.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	scans, err := h.list.Execute(r.Context(), sub, limit)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	out := make([]summaryResponse, 0, len(scans))
	for _, s := range scans {
		out = append(out, summaryOf(s))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"scans": out})
}

type trendPointResponse struct {
	WeekStart    string `json:"weekStart"`
	OverallScore int    `json:"overallScore"`
	SkinAge      *int   `json:"skinAge,omitempty"`
}

func (h *Handler) getTrend(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	weeks := application.FreeTrendWeeks
	switch r.URL.Query().Get("period") {
	case "months6":
		weeks = 26
	case "year":
		weeks = 52
	}

	points, total, err := h.trend.Execute(r.Context(), sub, weeks)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	out := make([]trendPointResponse, 0, len(points))
	for _, p := range points {
		out = append(out, trendPointResponse{
			WeekStart:    p.WeekStart.UTC().Format("2006-01-02"),
			OverallScore: p.OverallScore,
			SkinAge:      p.SkinAge,
		})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"period": r.URL.Query().Get("period"),
		"points": out,
		// Truncated rather than refused, and the total is sent alongside so the
		// client can say "6 more behind Plus". A paywall works because you can
		// see what you are missing, not because the door is unmarked.
		"truncated":      total > len(out),
		"totalAvailable": total,
	})
}

// writeAllowanceExhausted answers a 429 with the real reset time.
//
// Retry-After used to be a hardcoded 86400. On a rolling seven-day window that
// is wrong by up to six days, and it is wrong in the direction that wastes the
// user's time: they come back tomorrow, find the button still dead, and learn
// the app does not know its own rules.
func (h *Handler) writeAllowanceExhausted(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	detail := "Your weekly scan has been used. Rejected photos never count."

	if a, err := h.allowance.Execute(r.Context(), sub); err == nil &&
		!a.ResetsAt.IsZero() {
		secs := int(time.Until(a.ResetsAt).Seconds())
		if secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		// The date goes in the body as well as the header. Retry-After is for
		// machines; the client needs a timestamp it can render as "Tuesday".
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"type":     "about:blank/allowance-exhausted",
			"title":    "No scans left this week",
			"detail":   detail,
			"status":   http.StatusTooManyRequests,
			"resetsAt": a.ResetsAt.UTC().Format(time.RFC3339),
		})
		return
	}

	httpx.WriteProblem(w, r, http.StatusTooManyRequests, "allowance-exhausted",
		"No scans left this week", detail)
}

// getHeatmap serves the redness overlay as an image.
//
// Bytes, not JSON. The contract's `heatmapUrl` is a URL because that is what an
// <img> wants, and base64ing a 30KB JPEG into a report body would inflate it by
// a third and force every client to decode it before it could be displayed.
func (h *Handler) getHeatmap(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	img, err := h.heatmap.Execute(r.Context(), sub, r.PathValue("scanId"))
	if err != nil {
		if errors.Is(err, application.ErrNoHeatmap) {
			// 404 is right here: this scan genuinely has no overlay, and no
			// amount of retrying will produce one.
			httpx.WriteProblem(w, r, http.StatusNotFound, "no-heatmap",
				"No overlay for this scan",
				"This scan did not produce a redness overlay.")
			return
		}
		h.writeDomainError(w, r, err)
		return
	}

	// Private, because this is a photograph of someone's face. A shared cache
	// holding it would be a face photo sitting on someone else's infrastructure.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	_, _ = w.Write(img)
}
