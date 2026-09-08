// Package http is the IAM transport layer.
//
// Its only job is translation: HTTP in, use case call, HTTP out. No business
// rules live here — the age gate is in the domain, and this package's
// responsibility is to make its refusal look like the 403 the OpenAPI contract
// promises.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/iam/application"
	"github.com/ayna/ayna-backend/internal/modules/iam/domain"
	"github.com/ayna/ayna-backend/internal/platform/auth"
	"github.com/ayna/ayna-backend/internal/platform/httpx"
)

// CurrentPolicyVersion is the consent wording in force. When it changes, prior
// consent stops counting and users are asked again — which is the entire
// reason consent records are version-scoped.
const CurrentPolicyVersion = "2026-08-01"

type Handler struct {
	register  *application.RegisterUser
	current   *application.GetCurrentUser
	consent   *application.GrantConsent
	status    *application.ConsentStatus
	deletion  *application.RequestDeletion
	retention *application.SetPhotoRetention
	allowance AllowanceSource
	log       *slog.Logger
}

func NewHandler(
	register *application.RegisterUser,
	current *application.GetCurrentUser,
	consent *application.GrantConsent,
	status *application.ConsentStatus,
	deletion *application.RequestDeletion,
	retention *application.SetPhotoRetention,
	allowance AllowanceSource,
	log *slog.Logger,
) *Handler {
	return &Handler{register, current, consent, status, deletion, retention, allowance, log}
}

// Mount registers the module's routes.
//
// Every route here is behind the authentication middleware, including
// POST /users: Auth0 has already identified the caller by then, and this
// endpoint decides whether that identity may have an account at all.
func (h *Handler) Mount(mux *http.ServeMux, authn auth.Authenticator) {
	protected := auth.Middleware(authn)

	mux.Handle("POST /v1/users", protected(http.HandlerFunc(h.createUser)))
	mux.Handle("GET /v1/users/me", protected(http.HandlerFunc(h.getCurrentUser)))
	mux.Handle("DELETE /v1/users/me", protected(http.HandlerFunc(h.deleteCurrentUser)))
	mux.Handle("PATCH /v1/users/me", protected(http.HandlerFunc(h.updateRetention)))
	mux.Handle("POST /v1/consent", protected(http.HandlerFunc(h.grantConsent)))
}

// ---------------------------------------------------------------------------
// Wire types. Separate from the domain on purpose: the domain must be free to
// change shape without altering a published contract, and these are what the
// OpenAPI file describes.
// ---------------------------------------------------------------------------

type createUserRequest struct {
	BirthYear   int    `json:"birthYear"`
	Timezone    string `json:"timezone"`
	DisplayName string `json:"displayName"`
}

type entitlementResponse struct {
	Tier           string `json:"tier"`
	ScansRemaining int    `json:"scansRemaining"`
	PeriodResetsAt string `json:"periodResetsAt"`
	HasHeatmap     bool   `json:"hasHeatmap"`
	HasSkinAge     bool   `json:"hasSkinAge"`
}

type userResponse struct {
	ID          string              `json:"id"`
	DisplayName *string             `json:"displayName"`
	BirthYear   int                 `json:"birthYear"`
	Timezone    string              `json:"timezone"`
	HasConsent  bool                `json:"hasConsent"`
	Entitlement entitlementResponse `json:"entitlement"`
	StreakWeeks int                 `json:"streakWeeks"`

	// How long photos are kept, in days, or null for indefinitely (NFR-4).
	//
	// Travels with the profile rather than sitting behind its own GET: the
	// settings screen needs it to render the currently selected option, and a
	// second round trip to draw one radio button is a round trip too many.
	PhotoRetentionDays *int `json:"photoRetentionDays"`
}

// retentionRequest is PATCH /v1/users/me.
//
// A POINTER to a pointer would be the only way to distinguish "set this to
// null" from "field omitted" in one struct, which is unreadable. Instead the
// field is required and null is a legal value: the client always sends the
// whole policy, because there is only one field and partial updates of a
// single-field resource are a distinction without a difference.
type retentionRequest struct {
	PhotoRetentionDays *int `json:"photoRetentionDays"`
}

type consentRequest struct {
	PolicyVersion string `json:"policyVersion"`
	Granted       *bool  `json:"granted"`
}

type consentResponse struct {
	PolicyVersion string `json:"policyVersion"`
	Granted       bool   `json:"granted"`
	RecordedAt    string `json:"recordedAt"`
}

type deletionResponse struct {
	RequestedAt string `json:"requestedAt"`
	CompletesBy string `json:"completesBy"`
}

// ---------------------------------------------------------------------------

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteBadRequest(w, r, "The request body could not be read as JSON.")
		return
	}
	if req.Timezone == "" {
		httpx.WriteBadRequest(w, r, "A timezone is required, for example Asia/Karachi.")
		return
	}

	u, err := h.register.Execute(r.Context(), application.RegisterUserInput{
		Auth0Sub:    auth.MustSubject(r.Context()),
		BirthYear:   req.BirthYear,
		Timezone:    req.Timezone,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, h.toUserResponse(r.Context(), u, false))
}

func (h *Handler) getCurrentUser(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	u, err := h.current.Execute(r.Context(), sub)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// Consent is part of the client's launch state: it decides whether the
	// camera can be opened at all, so it travels with the user rather than
	// requiring a second round trip.
	state, err := h.status.Execute(r.Context(), sub, CurrentPolicyVersion)
	if err != nil {
		httpx.WriteInternal(w, r, h.log, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, h.toUserResponse(r.Context(), u, state.AllowsScanning()))
}

func (h *Handler) grantConsent(w http.ResponseWriter, r *http.Request) {
	var req consentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteBadRequest(w, r, "The request body could not be read as JSON.")
		return
	}
	// A pointer, so an absent field is distinguishable from an explicit false.
	// Defaulting a missing `granted` to false would silently record a
	// revocation the user never made.
	if req.Granted == nil {
		httpx.WriteBadRequest(w, r, "granted must be stated explicitly as true or false.")
		return
	}

	rec, err := h.consent.Execute(r.Context(), application.GrantConsentInput{
		Auth0Sub:      auth.MustSubject(r.Context()),
		PolicyVersion: req.PolicyVersion,
		Granted:       *req.Granted,
	})
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, consentResponse{
		PolicyVersion: rec.PolicyVersion,
		Granted:       rec.Granted,
		RecordedAt:    rec.RecordedAt.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) deleteCurrentUser(w http.ResponseWriter, r *http.Request) {
	at, by, err := h.deletion.Execute(r.Context(), auth.MustSubject(r.Context()))
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	// 202, not 204: the purge spans Postgres, object storage and the vendor,
	// so it is genuinely asynchronous and the response says when it completes.
	httpx.WriteJSON(w, http.StatusAccepted, deletionResponse{
		// .UTC() before formatting: a value built in memory renders as "Z",
		// while the same instant read back from Postgres renders with the
		// session offset. Both are valid RFC 3339 and both parse, but an API
		// that formats one field two ways invites a client to compare them as
		// strings and be wrong.
		RequestedAt: at.UTC().Format(time.RFC3339),
		CompletesBy: by.UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------

func (h *Handler) toUserResponse(
	ctx context.Context, u *domain.User, hasConsent bool,
) userResponse {
	// A fresh allowance is the fallback when the source is absent or errors.
	// Erring toward "you may scan" is the right direction: the submit path
	// enforces the real limit anyway, so the worst case is one 429 -- whereas
	// erring toward "you may not" would block a user who is entitled.
	allowance := Allowance{Remaining: 1, Limit: 1}
	if h.allowance != nil {
		if a, err := h.allowance.For(ctx, u.ID()); err == nil {
			allowance = a
		}
	}

	// Empty rather than a fabricated date when scans are available. A reset
	// time for something not exhausted is a countdown to nothing.
	var resetsAt string
	if !allowance.ResetsAt.IsZero() {
		resetsAt = allowance.ResetsAt.UTC().Format(time.RFC3339)
	}

	var name *string
	if n := u.DisplayName(); n != "" {
		name = &n
	}

	return userResponse{
		ID:          u.ID(),
		DisplayName: name,
		BirthYear:   u.BirthYear(),
		Timezone:    u.Timezone().String(),
		HasConsent:  hasConsent,
		// The TIER is still hardcoded to free until the Billing module exists.
		// The COUNTS are not: they were, and the profile claimed one scan was
		// always available with a reset date of "now plus seven days"
		// recomputed on every request. The app believed the button was live,
		// offered it, and the user met the limit as a 429 after taking the
		// photo -- the one moment the answer is most annoying, because the work
		// is already done.
		//
		// Capability flags stay explicit rather than inferred from the tier
		// name, as the contract requires: a client that maps tiers to features
		// itself goes stale the moment pricing changes.
		Entitlement: entitlementResponse{
			Tier:           "free",
			ScansRemaining: allowance.Remaining,
			PeriodResetsAt: resetsAt,
			HasHeatmap:     false,
			HasSkinAge:     false,
		},
		StreakWeeks:        0,
		PhotoRetentionDays: u.PhotoRetentionDays(),
	}
}

// writeDomainError maps domain errors onto the status codes the OpenAPI
// contract promises. Anything unrecognised becomes a 500 with the cause
// logged, never guessed at — a wrong status code is worse than a 500, because
// the client acts on it.
func (h *Handler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrUnderMinimumAge):
		// A distinct status and slug because the client shows a specific,
		// non-punitive screen for this rather than a generic failure.
		httpx.WriteProblem(w, r, http.StatusForbidden,
			"under-minimum-age", "Below the minimum age",
			"Ayna is available to people aged 18 and over.")

	case errors.Is(err, domain.ErrAuth0SubTaken):
		httpx.WriteProblem(w, r, http.StatusConflict,
			"profile-exists", "You already have a profile",
			"This account is already set up. Try signing in instead.")

	case errors.Is(err, domain.ErrUserNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound,
			"no-profile", "No profile yet",
			"Finish setting up your account before continuing.")

	case errors.Is(err, domain.ErrInvalidTimezone):
		httpx.WriteBadRequest(w, r, "That timezone is not one we recognise. Use an IANA name such as Asia/Karachi.")

	case errors.Is(err, domain.ErrInvalidBirthYear):
		httpx.WriteBadRequest(w, r, "That birth year does not look right.")

	case errors.Is(err, domain.ErrMissingPolicyVersion):
		httpx.WriteBadRequest(w, r, "policyVersion is required.")

	default:
		httpx.WriteInternal(w, r, h.log, err)
	}
}

// updateRetention sets how long photos are kept (NFR-4).
//
// PATCH rather than PUT: the body carries one field of the profile, and a PUT
// would imply the client is replacing the whole resource -- including birth
// year, which is exactly the field PD-1 says must never be editable.
//
// Returns the full user rather than 204, so the client's cached profile stays
// correct without a follow-up GET. The settings screen renders from that
// profile, and a stale copy would show the option the user just changed away
// from.
func (h *Handler) updateRetention(w http.ResponseWriter, r *http.Request) {
	sub := auth.MustSubject(r.Context())

	var req retentionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteBadRequest(w, r, "The request body could not be read as JSON.")
		return
	}

	u, err := h.retention.Execute(r.Context(), sub, req.PhotoRetentionDays)
	if err != nil {
		h.writeDomainError(w, r, err)
		return
	}

	state, err := h.status.Execute(r.Context(), sub, CurrentPolicyVersion)
	if err != nil {
		httpx.WriteInternal(w, r, h.log, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, h.toUserResponse(r.Context(), u, state.AllowsScanning()))
}
