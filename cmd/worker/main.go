// Command worker is the composition root for the AI analysis worker (ADR-002).
//
// It is a separate deployable from day one because its workload is shaped
// differently: slow, expensive per call, and bursty (NFR-3, NFR-7).
//
// # Why a worker has an HTTP server at all
//
// It runs on Cloud Run, which scales to zero. River is a *pull* queue, and a
// process scaled to zero is not polling for anything. Rather than pin an
// instance always-on and pay for idle time, the worker exposes a wake-up
// endpoint:
//
//  1. The API commits the Scan and the job in ONE transaction. The job is
//     durable the instant that commit returns.
//  2. The API fires a fire-and-forget ping at POST /wake. Cloud Run cold-starts
//     this service, it drains the queue, and it scales back to zero.
//  3. Cloud Scheduler hits the same endpoint every few minutes as a safety net.
//
// The property that makes this safe: **the ping is an optimisation, never a
// correctness requirement.** If it is lost, delayed, or arrives twice, the job
// is already committed in Postgres and the scheduled tick collects it. A lost
// ping costs latency; it can never cost a scan. That is the opposite of
// "commit, then publish to an external queue", where a lost publish loses the
// job outright.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	iampg "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/postgres"
	scanpg "github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/postgres"
	"github.com/ayna/ayna-backend/internal/platform/config"
	"github.com/ayna/ayna-backend/internal/platform/database"
	"github.com/ayna/ayna-backend/internal/platform/health"
	"github.com/ayna/ayna-backend/internal/platform/httpserver"
	"github.com/ayna/ayna-backend/internal/platform/logger"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		os.Stderr.WriteString("fatal: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(version)
	if err != nil {
		return err
	}

	log := logger.New(cfg.Env, cfg.Version)
	log.Info("starting worker", slog.Any("config", cfg.Redacted()))

	ctx := context.Background()

	pool, err := database.Open(ctx, database.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("database connected")

	h := health.New(cfg.Version, cfg.HealthTimeout,
		health.CheckerFunc{Label: "postgres", Fn: database.HealthCheck(pool)},
	)

	// The retention sweep, assembled from both modules' repositories. See
	// retention.go for why the coordination happens here and not inside either
	// module.
	sweeper := newRetentionSweeper(
		iampg.NewUserRepository(pool),
		scanpg.NewScanRepository(pool),
		log,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.Live)
	mux.HandleFunc("GET /readyz", h.Ready)
	mux.HandleFunc("POST /wake", wake(log))
	mux.HandleFunc("POST /internal/retention/sweep", sweepHandler(sweeper, log))

	srv := httpserver.New(cfg.Port, logger.Middleware(log)(mux), log, cfg.ShutdownGrace)
	return srv.Run(ctx)
}

// wake drains the job queue.
//
// It answers 202 immediately and does the work in the background, because the
// caller is either a fire-and-forget ping from the API or a scheduler tick --
// neither waits for a result, and holding the connection open for the length
// of an AI analysis would tie up the caller for seconds.
//
// The handler must stay idempotent and safe to call concurrently: the ping and
// the scheduler tick will overlap, and River's own locking is what makes each
// job run once regardless.
func wake(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := logger.FromContext(r.Context(), log)
		l.Info("wake received")

		// River client drains here once the skinanalysis module exists.
		// Until then this endpoint exists so the deploy shape and the wake-up
		// path are proven before there is any work to do.

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}
}

// sweepHandler runs the photo retention sweep (NFR-4).
//
// # Authentication
//
// There is none in this handler, deliberately. The worker is deployed with
// --no-allow-unauthenticated, so Cloud Run rejects any caller without a valid
// OIDC token from a service account holding roles/run.invoker before the
// request ever reaches Go. Adding a second, hand-rolled shared-secret check on
// top would be a weaker mechanism sitting in front of a stronger one, and a
// secret to rotate for no gain.
//
// # Why this one waits, when /wake does not
//
// /wake answers 202 and works in the background because its caller is a
// fire-and-forget ping. This caller is Cloud Scheduler, which records the
// response, retries on failure, and is the only place anyone will look to find
// out whether the sweep ran. A 202 would make every run look successful,
// including the ones that deleted nothing because the query was broken.
//
// The work is three UPDATE statements, so there is no risk of holding the
// connection long enough to matter.
func sweepHandler(s *retentionSweeper, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := logger.FromContext(r.Context(), log)

		result, err := s.Sweep(r.Context())
		if err != nil {
			// 500 so Cloud Scheduler retries and the failure is visible. The
			// counts still go in the body: a partial sweep is not a sweep that
			// did nothing, and the difference matters when reading the log.
			l.Error("retention sweep failed", slog.String("error", err.Error()))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "partial",
				"result": result,
			})
			return
		}

		l.Info("retention sweep complete", slog.Int64("cleared", result.Cleared))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
