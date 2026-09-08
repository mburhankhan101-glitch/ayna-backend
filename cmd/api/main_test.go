package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	iamhttp "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/http"
	sahttp "github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/http"
	"github.com/ayna/ayna-backend/internal/platform/auth"
	"github.com/ayna/ayna-backend/internal/platform/health"
	"github.com/ayna/ayna-backend/internal/platform/logger"
)

func serve(t *testing.T, checkers ...health.Checker) http.Handler {
	t.Helper()
	log := logger.New("dev", "test-sha")
	h := health.New("test-sha", 500*time.Millisecond, checkers...)

	// The IAM handler is constructed with nil use cases: these tests exercise
	// probes and routing only, and Mount reads method values without calling
	// them. A test that touched an IAM route would panic, which is the correct
	// outcome for a test claiming to cover something it has not wired.
	iam := iamhttp.NewHandler(nil, nil, nil, nil, nil, nil, nil, log)
	scans := sahttp.NewHandler(nil, nil, nil, nil, nil, nil, log)
	authn, err := auth.NewDevAuthenticator("dev")
	if err != nil {
		t.Fatal(err)
	}

	return logger.Middleware(log)(routes(h, iam, scans, authn))
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The paths Cloud Run's probes hit. A typo here is invisible in review and
// fatal on deploy: the platform marks the revision unhealthy and rolls back,
// with nothing in the logs explaining why.
func TestProbeEndpointsExistAtTheExpectedPaths(t *testing.T) {
	h := serve(t)

	for _, path := range []string{"/livez", "/healthz", "/readyz"} {
		if code := get(t, h, path).Code; code == http.StatusNotFound {
			t.Errorf("%s returned 404 — Cloud Run's probe would fail", path)
		}
	}
}

// /livez exists so liveness is checkable from outside the platform.
//
// Cloud Run's edge answers the exact path "/healthz" with Google's own HTML
// 404 and never delivers the request to the container. Neighbouring paths
// (/healthz2, /Healthz, /healthz/, /livez) all arrive normally, so this is one
// reserved string rather than a prefix or a pattern.
//
// Nothing was ever down because of this: Cloud Run's probe reaches the
// container below the front end, and this service uses a TCP probe on the port
// rather than an HTTP one. What it broke was checking, since every curl of
// /healthz against the public URL looks like a dead service.
//
// The honest limit of this test: it cannot reproduce the interception, which
// happens outside the process. What it can do is fail if someone deletes
// /livez as an apparent duplicate of /healthz, which is how this regresses.
func TestLivezExistsBecauseHealthzIsUnreachableOnCloudRun(t *testing.T) {
	if code := get(t, serve(t), "/livez").Code; code != http.StatusOK {
		t.Fatalf("/livez = %d, want 200 — the only liveness path reachable from outside", code)
	}
}

func TestHealthzReportsTheBuildVersion(t *testing.T) {
	// "Is the new revision actually live?" is unanswerable without this, and
	// it is the first question asked after every deploy.
	rec := get(t, serve(t), "/healthz")

	var body struct{ Version string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz did not return JSON: %v", err)
	}
	if body.Version != "test-sha" {
		t.Errorf("version = %q, want the injected build stamp", body.Version)
	}
}

func TestReadyzFailsWhenPostgresIsUnreachable(t *testing.T) {
	down := health.CheckerFunc{Label: "postgres", Fn: func(context.Context) error {
		return errors.New("dial tcp: connection refused")
	}}

	if code := get(t, serve(t, down), "/readyz").Code; code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503 when the database is down", code)
	}
}

func TestHealthzStaysUpWhenPostgresIsDown(t *testing.T) {
	// Liveness must not depend on the database. If it did, a database blip
	// would restart every instance simultaneously and turn a recoverable
	// outage into a total one.
	down := health.CheckerFunc{Label: "postgres", Fn: func(context.Context) error {
		return errors.New("dial tcp: connection refused")
	}}

	if code := get(t, serve(t, down), "/healthz").Code; code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 — liveness must ignore dependencies", code)
	}
}

func TestEveryResponseCarriesACorrelationID(t *testing.T) {
	// NFR-9. Once the worker is a separate service, this header is the only
	// thing tying "the user submitted a scan" to "the worker failed" across
	// two sets of logs.
	if got := get(t, serve(t), "/healthz").Header().Get("X-Correlation-Id"); got == "" {
		t.Error("no X-Correlation-Id on the response")
	}
}

func TestAnInboundCorrelationIDIsPreserved(t *testing.T) {
	h := serve(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Correlation-Id", "req_abc123")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Correlation-Id"); got != "req_abc123" {
		t.Errorf("correlation id = %q, want the inbound value preserved", got)
	}
}

func TestAHostileCorrelationIDIsRejected(t *testing.T) {
	// The value is echoed into a response header and into log lines, so an
	// unbounded or newline-bearing value from the network is a header- and
	// log-injection vector. It must be replaced, not sanitised in place.
	for _, hostile := range []string{
		"abc\r\nX-Injected: evil",
		"../../etc/passwd",
		string(make([]byte, 500)),
	} {
		h := serve(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Correlation-Id", hostile)
		h.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Correlation-Id")
		if got == hostile {
			t.Errorf("hostile correlation id echoed back: %q", got)
		}
		if got == "" {
			t.Error("a rejected id should be replaced with a generated one, not dropped")
		}
	}
}

func TestUnknownPathsReturn404(t *testing.T) {
	if code := get(t, serve(t), "/nope").Code; code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", code)
	}
}
