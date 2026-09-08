// Package logger provides structured logging and request correlation.
//
// NFR-9 requires structured logs and request tracing from the first deployed
// version, on the grounds that it is cheap now and expensive to retrofit. This
// is that, at its smallest useful size: JSON to stdout (which Cloud Run parses
// into structured entries automatically), and a correlation ID threaded
// through every log line of a request.
//
// The correlation ID is the same `correlationId` the event envelope in
// 05-Project-Structure-and-Contracts carries, so a log line, an HTTP request
// and a domain event can be tied together once the worker is a separate
// process and "which request caused this?" stops being obvious.
package logger

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type ctxKey int

const correlationKey ctxKey = iota

// New returns a logger appropriate to the environment: JSON in deployed
// environments so Cloud Run can index it, human-readable text locally.
func New(env, version string) *slog.Logger {
	level := slog.LevelDebug
	if env != "dev" {
		level = slog.LevelInfo
	}

	var h slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if env == "dev" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(h).With("service_version", version, "env", env)
}

// WithCorrelation stores a correlation ID on the context.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// Correlation reads the correlation ID, returning "" when absent.
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

// FromContext returns a logger already tagged with the request's correlation
// ID, so handlers cannot forget to include it.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := Correlation(ctx); id != "" {
		return base.With("correlation_id", id)
	}
	return base
}

// Middleware assigns a correlation ID and logs one line per completed request.
//
// It honours an inbound X-Correlation-Id so a trace survives across service
// boundaries -- the API will pass its own ID to the worker when it fires the
// wake-up ping, and both sides' logs then join up.
func Middleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitiseCorrelationID(r.Header.Get("X-Correlation-Id"))
			if id == "" {
				id = newID()
			}

			ctx := WithCorrelation(r.Context(), id)
			w.Header().Set("X-Correlation-Id", id)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r.WithContext(ctx))

			// Health probes fire constantly and would drown everything else.
			// Log them only when they fail, which is the only time they are
			// interesting.
			if isProbe(r.URL.Path) && rec.status < 400 {
				return
			}

			base.LogAttrs(ctx, levelFor(rec.status), "request",
				slog.String("correlation_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

// sanitiseCorrelationID refuses anything unreasonable from a caller. The value
// is echoed into response headers and log fields, so an unbounded or
// newline-bearing string from the network is a log-injection vector.
func sanitiseCorrelationID(v string) string {
	if len(v) == 0 || len(v) > 64 {
		return ""
	}
	for _, r := range v {
		isAllowed := r == '-' || r == '_' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z')
		if !isAllowed {
			return ""
		}
	}
	return v
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// newID generates a short, sortable, URL-safe correlation ID.
//
// Deliberately not a UUID dependency: this needs to be unique enough to join
// log lines within a retention window, not globally unique forever.
func newID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	now := time.Now().UnixNano()

	var sb strings.Builder
	sb.Grow(16)
	for i := 0; i < 12; i++ {
		sb.WriteByte(alphabet[now%int64(len(alphabet))])
		now /= int64(len(alphabet))
		if now == 0 {
			now = time.Now().UnixNano()
		}
	}
	return "req_" + sb.String()
}
