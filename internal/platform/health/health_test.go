package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func ok(name string) Checker {
	return CheckerFunc{Label: name, Fn: func(context.Context) error { return nil }}
}

func broken(name string) Checker {
	return CheckerFunc{Label: name, Fn: func(context.Context) error {
		return errors.New("connection refused")
	}}
}

func slow(name string, d time.Duration) Checker {
	return CheckerFunc{Label: name, Fn: func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

func call(h http.HandlerFunc) (int, response) {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var body response
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestLivenessIgnoresDependencies(t *testing.T) {
	// The whole point of the split. If liveness checked the database, a brief
	// database blip would make every instance look dead, the platform would
	// restart all of them at once, and a recoverable outage becomes a total
	// one. Liveness must answer "is this process running", nothing more.
	h := New("v1", time.Second, broken("postgres"))

	code, body := call(h.Live)
	if code != http.StatusOK {
		t.Errorf("liveness = %d, want 200 even with a dead dependency", code)
	}
	if len(body.Checks) != 0 {
		t.Errorf("liveness must not run checks, got %v", body.Checks)
	}
}

func TestReadinessFailsWhenADependencyIsDown(t *testing.T) {
	h := New("v1", time.Second, ok("cache"), broken("postgres"))

	code, body := call(h.Ready)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503", code)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if body.Checks["cache"] != "ok" {
		t.Errorf("a healthy check should still report ok: %v", body.Checks)
	}
}

func TestReadinessPassesWhenEverythingIsUp(t *testing.T) {
	h := New("v1", time.Second, ok("postgres"), ok("cache"))

	code, body := call(h.Ready)
	if code != http.StatusOK || body.Status != "ok" {
		t.Errorf("got %d/%q, want 200/ok", code, body.Status)
	}
}

func TestChecksRunConcurrentlyUnderOneDeadline(t *testing.T) {
	// Three 200ms checks must finish in roughly 200ms, not 600ms. A probe
	// that outlives its own interval is a second failure mode stacked on the
	// one it was meant to detect.
	h := New("v1", 2*time.Second,
		slow("a", 200*time.Millisecond),
		slow("b", 200*time.Millisecond),
		slow("c", 200*time.Millisecond),
	)

	start := time.Now()
	code, _ := call(h.Ready)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Errorf("got %d, want 200", code)
	}
	if elapsed > 450*time.Millisecond {
		t.Errorf("took %v — checks appear to be running sequentially", elapsed)
	}
}

func TestASlowDependencyTimesOutRatherThanHanging(t *testing.T) {
	h := New("v1", 100*time.Millisecond, slow("postgres", 5*time.Second))

	start := time.Now()
	code, _ := call(h.Ready)

	if code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v — the timeout is not bounding the check", elapsed)
	}
}

func TestProbeResponsesAreNotCacheable(t *testing.T) {
	// A cached "ok" outlives the truth, and a proxy that caches a probe turns
	// a health check into a health guess.
	rec := httptest.NewRecorder()
	New("v1", time.Second).Ready(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
