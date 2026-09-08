package http

import (
	"bytes"
	"context"
	"encoding/json"

	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ayna/ayna-backend/internal/modules/iam/application"
	iampg "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/postgres"
	"github.com/ayna/ayna-backend/internal/platform/auth"
	"github.com/ayna/ayna-backend/internal/platform/id"
)

// End-to-end through every layer: HTTP -> middleware -> handler -> use case ->
// repository -> real Postgres, with the whole thing inside a transaction that
// is rolled back.
//
// The layers most likely to be wrong are the seams between them — an error
// mapped to the wrong status, a field named differently on the wire than in
// the domain, a subject that never reaches the query. Testing each layer alone
// with fakes would exercise none of those.

func env(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration tests")
	}
	return url
}

// harness wires the real stack against a rolled-back transaction.
func harness(t *testing.T, fn func(srv http.Handler, sub string)) {
	t.Helper()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, env(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	users := iampg.NewUserRepository(tx)
	consents := iampg.NewConsentRepository(tx)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	clock := func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) }

	h := NewHandler(
		application.NewRegisterUser(users, clock),
		application.NewGetCurrentUser(users),
		application.NewGrantConsent(users, consents, clock),
		application.NewConsentStatus(users, consents),
		application.NewRequestDeletion(users, clock),
		application.NewSetPhotoRetention(users),
		// nil allowance source: these tests cover identity and consent, and the
		// handler is written to fall back to a fresh allowance when none is
		// wired -- a profile must not fail to load because a count is missing.
		nil,
		log,
	)

	authn, err := auth.NewDevAuthenticator("dev")
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}

	mux := http.NewServeMux()
	h.Mount(mux, authn)

	fn(mux, "auth0|"+id.New(id.PrefixUser))
}

func do(t *testing.T, srv http.Handler, method, path, sub string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if sub != "" {
		req.Header.Set(auth.DevSubjectHeader, sub)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func register(t *testing.T, srv http.Handler, sub string, birthYear int) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, srv, "POST", "/v1/users", sub, map[string]any{
		"birthYear": birthYear, "timezone": "Asia/Karachi", "displayName": "Sana",
	})
}

// ---------------------------------------------------------------------------

func TestRegisterThenFetchTheCurrentUser(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		rec := register(t, srv, sub, 1999)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/users = %d, want 201: %s", rec.Code, rec.Body)
		}

		var created struct {
			ID          string `json:"id"`
			BirthYear   int    `json:"birthYear"`
			Timezone    string `json:"timezone"`
			HasConsent  bool   `json:"hasConsent"`
			Entitlement struct {
				Tier       string `json:"tier"`
				HasHeatmap bool   `json:"hasHeatmap"`
				HasSkinAge bool   `json:"hasSkinAge"`
			} `json:"entitlement"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if !id.Valid(id.PrefixUser, created.ID) {
			t.Errorf("id %q does not match the contract's shape", created.ID)
		}
		if created.Timezone != "Asia/Karachi" {
			t.Errorf("timezone = %q", created.Timezone)
		}
		if created.HasConsent {
			t.Error("a brand-new user must not already have consent")
		}
		// Capability flags, not tier inference. A free user has no heatmap and
		// no skin age (PD-3), and the client is told so directly.
		if created.Entitlement.Tier != "free" ||
			created.Entitlement.HasHeatmap || created.Entitlement.HasSkinAge {
			t.Errorf("unexpected entitlement: %+v", created.Entitlement)
		}

		got := do(t, srv, "GET", "/v1/users/me", sub, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("GET /v1/users/me = %d: %s", got.Code, got.Body)
		}
	})
}

func TestAnUnderageSignUpIsRefusedWithItsOwnProblemType(t *testing.T) {
	// The client shows a specific, non-punitive screen for this. A generic 400
	// would leave it with nothing to distinguish "you are too young" from "your
	// JSON was malformed".
	harness(t, func(srv http.Handler, sub string) {
		rec := register(t, srv, sub, 2015)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("Content-Type = %q, want application/problem+json", ct)
		}

		var p struct {
			Type   string `json:"type"`
			Status int    `json:"status"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if p.Type != "https://api.ayna.app/problems/under-minimum-age" {
			t.Errorf("problem type = %q", p.Type)
		}
		if p.Status != 403 {
			t.Errorf("problem status = %d", p.Status)
		}
		if p.Detail == "" {
			t.Error("a problem with no detail gives the client nothing to show")
		}
	})
}

func TestAnUnderageSignUpCreatesNothing(t *testing.T) {
	// The gate refuses creation rather than flagging a created row, so there
	// must be no record at all afterwards — nothing to purge, nothing to leak,
	// nothing to forget to filter out of a query later.
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 2015); rec.Code != http.StatusForbidden {
			t.Fatalf("setup: got %d", rec.Code)
		}
		if rec := do(t, srv, "GET", "/v1/users/me", sub, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET after refused sign-up = %d, want 404", rec.Code)
		}
	})
}

func TestRegisteringTwiceIsAConflict(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("first: %d", rec.Code)
		}
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusConflict {
			t.Errorf("second = %d, want 409: %s", rec.Code, rec.Body)
		}
	})
}

func TestUnauthenticatedRequestsAreRejectedBeforeReachingTheDatabase(t *testing.T) {
	harness(t, func(srv http.Handler, _ string) {
		for _, c := range []struct{ method, path string }{
			{"POST", "/v1/users"},
			{"GET", "/v1/users/me"},
			{"POST", "/v1/consent"},
			{"DELETE", "/v1/users/me"},
		} {
			rec := do(t, srv, c.method, c.path, "", map[string]any{})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", c.method, c.path, rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Errorf("%s %s: no WWW-Authenticate header", c.method, c.path)
			}
		}
	})
}

func TestConsentFlowsThroughAndShowsUpOnTheUser(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}

		granted := true
		rec := do(t, srv, "POST", "/v1/consent", sub, map[string]any{
			"policyVersion": CurrentPolicyVersion, "granted": granted,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/consent = %d: %s", rec.Code, rec.Body)
		}

		me := do(t, srv, "GET", "/v1/users/me", sub, nil)
		var body struct {
			HasConsent bool `json:"hasConsent"`
		}
		if err := json.Unmarshal(me.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.HasConsent {
			t.Error("hasConsent should be true after granting")
		}
	})
}

func TestRevokingConsentIsRecordedAndTakesEffect(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}
		for _, granted := range []bool{true, false} {
			rec := do(t, srv, "POST", "/v1/consent", sub, map[string]any{
				"policyVersion": CurrentPolicyVersion, "granted": granted,
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("consent granted=%v: %d — a revocation is a valid record", granted, rec.Code)
			}
		}

		me := do(t, srv, "GET", "/v1/users/me", sub, nil)
		var body struct {
			HasConsent bool `json:"hasConsent"`
		}
		_ = json.Unmarshal(me.Body.Bytes(), &body)
		if body.HasConsent {
			t.Error("hasConsent should be false after revoking")
		}
	})
}

func TestAMissingGrantedFieldIsRejectedRatherThanTreatedAsFalse(t *testing.T) {
	// Defaulting an absent `granted` to false would silently record a
	// revocation the user never made — and it would look identical in the
	// audit trail to one they did.
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}
		rec := do(t, srv, "POST", "/v1/consent", sub, map[string]any{
			"policyVersion": CurrentPolicyVersion,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 for an absent granted field", rec.Code)
		}
	})
}

func TestConsentToAnOldPolicyDoesNotAuthoriseTheCurrentOne(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}
		rec := do(t, srv, "POST", "/v1/consent", sub, map[string]any{
			"policyVersion": "2020-01-01", "granted": true,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("consent: %d", rec.Code)
		}

		me := do(t, srv, "GET", "/v1/users/me", sub, nil)
		var body struct {
			HasConsent bool `json:"hasConsent"`
		}
		_ = json.Unmarshal(me.Body.Bytes(), &body)
		if body.HasConsent {
			t.Error("consent to a superseded policy version must not count as current consent")
		}
	})
}

func TestDeletionIsAcceptedAndRepeatableWithoutFailing(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}

		first := do(t, srv, "DELETE", "/v1/users/me", sub, nil)
		if first.Code != http.StatusAccepted {
			t.Fatalf("first delete = %d, want 202: %s", first.Code, first.Body)
		}

		var body struct {
			RequestedAt string `json:"requestedAt"`
			CompletesBy string `json:"completesBy"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.RequestedAt == "" || body.CompletesBy == "" {
			t.Error("the response must state the purge SLA, not just accept")
		}

		// Tapping "delete my account" twice is the same wish expressed twice,
		// not an error. The timestamp must not move, so the SLA the user was
		// given cannot be silently extended.
		second := do(t, srv, "DELETE", "/v1/users/me", sub, nil)
		if second.Code != http.StatusAccepted {
			t.Fatalf("second delete = %d, want 202", second.Code)
		}
		var again struct {
			RequestedAt string `json:"requestedAt"`
		}
		_ = json.Unmarshal(second.Body.Bytes(), &again)

		// Compare instants, not strings. The first value is built in memory
		// and the second is read back from Postgres, so before the handler
		// normalised to UTC these were the same moment rendered as "12:00:00Z"
		// and "17:00:00+05:00". A string comparison called that a bug when the
		// bug was the formatting.
		firstAt, err := time.Parse(time.RFC3339, body.RequestedAt)
		if err != nil {
			t.Fatalf("parse first: %v", err)
		}
		secondAt, err := time.Parse(time.RFC3339, again.RequestedAt)
		if err != nil {
			t.Fatalf("parse second: %v", err)
		}
		if !firstAt.Equal(secondAt) {
			t.Errorf("requestedAt moved from %v to %v", firstAt, secondAt)
		}

		// And the format is now consistent, which is what makes a client's
		// naive string comparison safe rather than accidentally correct.
		if body.RequestedAt != again.RequestedAt {
			t.Errorf("same instant rendered two ways: %q then %q", body.RequestedAt, again.RequestedAt)
		}
	})
}

func TestUnknownFieldsAreRejectedRatherThanSilentlyIgnored(t *testing.T) {
	// A field the contract does not define is a client bug, and silently
	// dropping it means the client keeps sending something it believes is
	// having an effect.
	harness(t, func(srv http.Handler, sub string) {
		rec := do(t, srv, "POST", "/v1/users", sub, map[string]any{
			"birthYear": 1999, "timezone": "UTC", "nickname": "sana",
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400 for an unknown field: %s", rec.Code, rec.Body)
		}
	})
}

func TestFieldNameCasingIsToleratedByGosDecoder(t *testing.T) {
	// Recorded because it is surprising and was initially assumed otherwise:
	// encoding/json matches field names CASE-INSENSITIVELY, so `birthyear`
	// binds to `birthYear` and DisallowUnknownFields does not object.
	//
	// That is benign — the client gets the result it intended rather than a
	// silent zero — but it means casing is not something the API can enforce,
	// and any future validation must not assume it does.
	harness(t, func(srv http.Handler, sub string) {
		rec := do(t, srv, "POST", "/v1/users", sub, map[string]any{
			"birthyear": 1999, "timezone": "UTC",
		})
		if rec.Code != http.StatusCreated {
			t.Errorf("got %d, want 201: Go binds birthyear to birthYear", rec.Code)
		}
	})
}

func TestEveryErrorCarriesACorrelationIdOrExplainsItself(t *testing.T) {
	harness(t, func(srv http.Handler, sub string) {
		rec := register(t, srv, sub, 2015) // 403
		var p map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"type", "title", "status", "detail"} {
			if _, ok := p[k]; !ok {
				t.Errorf("problem body is missing %q: %v", k, p)
			}
		}
	})
}

// Guard against the transaction helper ever being switched to commit.
func TestHarnessRollsBack(t *testing.T) {
	url := env(t)
	ctx := context.Background()

	sub := "auth0|rollback-probe-" + id.New(id.PrefixUser)
	harness(t, func(srv http.Handler, _ string) {
		if rec := register(t, srv, sub, 1999); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d", rec.Code)
		}
	})

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE auth0_sub = $1`, sub).Scan(&n); err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d row(s) survived the test harness — it is committing, not rolling back", n)
	}
}
