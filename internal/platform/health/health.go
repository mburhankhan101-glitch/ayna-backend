// Package health implements liveness and readiness endpoints.
//
// The two are genuinely different questions, and conflating them is the most
// common way to make a deploy worse rather than better:
//
//	/healthz  LIVENESS  -- is this process functioning? No dependencies checked.
//	                       A failure here means "restart me".
//	/readyz   READINESS -- can this instance serve traffic right now? Checks
//	                       dependencies. A failure means "route around me".
//
// If liveness pinged the database, a brief database blip would make every
// instance look dead, the platform would restart all of them, and a recoverable
// outage would become a full one. Liveness therefore checks nothing external,
// on purpose.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Checker is one dependency that readiness cares about. Implementations must
// respect ctx cancellation -- a check that ignores its deadline defeats the
// timeout that protects the probe.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckerFunc adapts a function into a Checker.
type CheckerFunc struct {
	Label string
	Fn    func(ctx context.Context) error
}

func (c CheckerFunc) Name() string                    { return c.Label }
func (c CheckerFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

type Handler struct {
	version  string
	timeout  time.Duration
	checkers []Checker
	started  time.Time
}

func New(version string, timeout time.Duration, checkers ...Checker) *Handler {
	return &Handler{
		version:  version,
		timeout:  timeout,
		checkers: checkers,
		started:  time.Now(),
	}
}

type response struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Uptime  string            `json:"uptime"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// Live answers liveness. It deliberately performs no dependency checks: if
// this handler runs at all, the process is alive.
func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, response{
		Status:  "ok",
		Version: h.version,
		Uptime:  time.Since(h.started).Round(time.Second).String(),
	})
}

// Ready answers readiness by checking every dependency concurrently, under one
// shared deadline.
//
// Concurrently and not in sequence: with three checks and a 2s timeout each,
// sequential execution can take 6s, and a probe that outlives its own interval
// is a second failure mode on top of the one it was meant to detect.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	results := make(map[string]string, len(h.checkers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, c := range h.checkers {
		wg.Add(1)
		go func(c Checker) {
			defer wg.Done()
			msg := "ok"
			if err := c.Check(ctx); err != nil {
				msg = "failed: " + err.Error()
			}
			mu.Lock()
			results[c.Name()] = msg
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	status, code := "ok", http.StatusOK
	for _, v := range results {
		if v != "ok" {
			status, code = "degraded", http.StatusServiceUnavailable
			break
		}
	}

	writeJSON(w, code, response{
		Status:  status,
		Version: h.version,
		Uptime:  time.Since(h.started).Round(time.Second).String(),
		Checks:  results,
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// Probe responses must never be cached; a cached "ok" outlives the truth.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
