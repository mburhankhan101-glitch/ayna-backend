// Package httpserver runs an HTTP server that shuts down cleanly.
//
// Graceful shutdown is not optional on Cloud Run. The platform sends SIGTERM
// and SIGKILLs 10 seconds later; an instance that ignores SIGTERM drops every
// request it was mid-way through on every scale-down and every deploy. Since
// Cloud Run scales to zero, scale-downs are routine rather than rare, so this
// is a normal-path concern here, not an edge case.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

type Server struct {
	srv   *http.Server
	log   *slog.Logger
	grace time.Duration
}

func New(port int, h http.Handler, log *slog.Logger, grace time.Duration) *Server {
	return &Server{
		srv: &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: h,

			// Timeouts are set explicitly because Go's defaults are "none",
			// and a server with no read timeout can be held open indefinitely
			// by a slow client.
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		log:   log,
		grace: grace,
	}
}

// Run serves until SIGTERM or SIGINT, then drains in-flight requests.
//
// Returns nil on a clean shutdown so the container exits 0 -- a non-zero exit
// on a routine scale-down would show up as a crash in every dashboard.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", s.srv.Addr)
		if err := s.srv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining", "grace", s.grace)
	}

	// context.Background(), not the cancelled ctx: the shutdown deadline must
	// be its own, or draining is cancelled the moment it starts.
	drainCtx, cancel := context.WithTimeout(context.Background(), s.grace)
	defer cancel()

	if err := s.srv.Shutdown(drainCtx); err != nil {
		// Requests still running past the grace window get cut. Log it loudly
		// rather than failing: the alternative is being SIGKILLed anyway.
		s.log.Error("graceful shutdown incomplete", "error", err)
		return nil
	}

	s.log.Info("shutdown complete")
	return nil
}
