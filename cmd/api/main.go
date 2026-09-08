// Command api is the composition root for the modular monolith (ADR-001).
//
// Everything is wired here and nowhere else: this is the only place that knows
// both what the application needs and which concrete adapter satisfies it.
// Modules receive their dependencies as interfaces, which is what keeps the
// dependency arrow pointing inward and lets the arch test in arch_test.go
// enforce that mechanically.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	iamapp "github.com/ayna/ayna-backend/internal/modules/iam/application"
	iamhttp "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/http"
	iampg "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/postgres"
	saapp "github.com/ayna/ayna-backend/internal/modules/skinanalysis/application"
	"github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/ailab"
	sahttp "github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/http"
	sapg "github.com/ayna/ayna-backend/internal/modules/skinanalysis/infrastructure/postgres"
	"github.com/ayna/ayna-backend/internal/platform/auth"
	"github.com/ayna/ayna-backend/internal/platform/config"
	"github.com/ayna/ayna-backend/internal/platform/database"
	"github.com/ayna/ayna-backend/internal/platform/health"
	"github.com/ayna/ayna-backend/internal/platform/httpserver"
	"github.com/ayna/ayna-backend/internal/platform/logger"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=$(git rev-parse --short HEAD)"
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Not slog: if configuration failed, the logger may not exist yet, and
		// a startup error that fails to print is the worst kind.
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
	log.Info("starting api", slog.Any("config", cfg.Redacted()))

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

	// Authentication.
	//
	// The dev stand-in is reachable only when Env is dev AND Auth0 is
	// unconfigured — and config.Load already refuses to produce such a config
	// outside dev. Two independent guards, because this is the one place where
	// a mistake means every user's face photos are readable by anyone who
	// guesses a subject string.
	var authn auth.Authenticator
	if cfg.UsesDevAuth() {
		log.Warn("USING THE DEV AUTHENTICATOR: requests are trusted on an " +
			"X-Dev-Subject header with no verification. Set AUTH0_DOMAIN and " +
			"AUTH0_AUDIENCE to use real tokens.")
		if authn, err = auth.NewDevAuthenticator(cfg.Env); err != nil {
			return err
		}
	} else {
		if authn, err = auth.NewAuth0Authenticator(cfg.Auth0Domain, cfg.Auth0Audience); err != nil {
			return err
		}
		log.Info("auth0 verification enabled", "domain", cfg.Auth0Domain, "audience", cfg.Auth0Audience)
	}

	// IAM module. Composed here and only here: this is the one place that
	// knows both what the application needs and which adapter satisfies it.
	users := iampg.NewUserRepository(pool)
	consents := iampg.NewConsentRepository(pool)

	// Built before the IAM handler because the profile reports remaining scans,
	// which only the scan module can count.
	scans := sapg.NewScanRepository(pool)
	getAllowance := saapp.NewGetAllowance(scans, saapp.SystemClock, cfg.ScanWeeklyAllowance)

	currentUser := iamapp.NewGetCurrentUser(users)
	consentStatus := iamapp.NewConsentStatus(users, consents)

	iamHandler := iamhttp.NewHandler(
		iamapp.NewRegisterUser(users, iamapp.SystemClock),
		currentUser,
		iamapp.NewGrantConsent(users, consents, iamapp.SystemClock),
		consentStatus,
		iamapp.NewRequestDeletion(users, iamapp.SystemClock),
		iamapp.NewSetPhotoRetention(users),
		allowanceBridge{get: getAllowance},
		log,
	)

	// Skin analysis. The analyzer is the only place AILab is named, per ADR-002
	// and ADR-003 -- swapping the vendor is a change to this one line and the
	// package behind it.
	analyzer := ailab.New(cfg.AILabAPIKey)

	// One bridge shared by every skinanalysis use case. Building it once keeps
	// the two modules joined in exactly one place.
	identity := identityBridge{current: currentUser, consent: consentStatus}

	scanHandler := sahttp.NewHandler(
		saapp.NewSubmitScan(
			scans, analyzer, identity,
			saapp.SystemClock,
			cfg.ScanWeeklyAllowance,
		),
		// Consent is deliberately not required to READ a scan: someone who
		// consented, scanned, then revoked must still see what they paid for.
		saapp.NewGetScan(scans, identity),
		saapp.NewListScans(scans, identity),
		saapp.NewGetTrend(scans, identity, saapp.SystemClock),
		saapp.NewMyAllowance(getAllowance, identity),
		saapp.NewGetHeatmap(scans, identity),
		log,
	)

	srv := httpserver.New(cfg.Port, logger.Middleware(log)(routes(h, iamHandler, scanHandler, authn)), log, cfg.ShutdownGrace)
	return srv.Run(ctx)
}

// routes is separate from run so the wiring can be exercised in a test without
// a live database. Route registration is exactly the kind of thing that looks
// obviously correct and is quietly wrong — a typo in a path only shows up when
// the platform's health probe fails and the deploy rolls back.
func routes(
	h *health.Handler,
	iam *iamhttp.Handler,
	scans *sahttp.Handler,
	authn auth.Authenticator,
) http.Handler {
	mux := http.NewServeMux()

	// Probes are deliberately outside the auth middleware: Cloud Run's health
	// checks present no credentials, and a 401 there is an unhealthy revision.
	mux.HandleFunc("GET /healthz", h.Live)
	mux.HandleFunc("GET /readyz", h.Ready)

	iam.Mount(mux, authn)
	scans.Mount(mux, authn)

	// Further modules mount here as they are built:
	//   skinanalysis.NewHandler(...).Mount(mux, authn)

	return mux
}
