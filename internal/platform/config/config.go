// Package config loads runtime configuration from the environment.
//
// Twelve-factor, deliberately: Cloud Run injects configuration as environment
// variables and there is no config file on the container. Everything is read
// once at startup and validated there, so a misconfigured deploy fails
// immediately and visibly rather than at the first request that needs the
// missing value.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Env is "dev", "staging" or "prod". Never inferred -- an unset
	// environment defaulting to prod is how staging data ends up in the real
	// database.
	Env string

	// Port is supplied by Cloud Run and must be honoured exactly; a container
	// listening anywhere else is killed as unhealthy with no useful error.
	Port int

	DatabaseURL string

	// ShutdownGrace must stay under Cloud Run's SIGTERM-to-SIGKILL window
	// (10s). Exceeding it means in-flight requests are cut off mid-response.
	ShutdownGrace time.Duration

	// HealthTimeout bounds the readiness dependency check. Kept short: a
	// readiness probe that hangs is worse than one that fails, because the
	// platform cannot tell the difference between slow and dead.
	HealthTimeout time.Duration

	// Version is stamped at build time (-ldflags) so a running container can
	// say which commit it is. Without it, "is the deploy live yet?" is
	// guesswork.
	Version string

	// Auth0Domain is the tenant host, no scheme: "ayna.eu.auth0.com".
	Auth0Domain string

	// Auth0Audience is the API identifier configured in Auth0. Never defaulted
	// to empty and never optional outside dev — an unset audience means the
	// service would accept tokens minted for some other API entirely.
	Auth0Audience string

	// AILabAPIKey authenticates against the vision provider selected in ADR-003.
	//
	// Optional, and deliberately so: without it the API still starts and every
	// other route works, while a scan fails cleanly as a vendor error that costs
	// the user no allowance. Refusing to boot would make the whole service
	// unavailable because one paid feature is unconfigured.
	AILabAPIKey string

	// ScanWeeklyAllowance overrides the free-tier scan limit.
	//
	// Exists for development: the real limit is one per rolling week, which is
	// correct for users and makes the feature untestable -- one scan and you are
	// locked out for seven days. Unset or zero keeps the production default.
	ScanWeeklyAllowance int
}

// UsesDevAuth reports whether this configuration runs without real token
// verification. True only in dev, and only when Auth0 is not configured — so
// setting the Auth0 variables locally switches a dev machine onto the real
// verifier without any other change.
func (c Config) UsesDevAuth() bool {
	return c.Env == "dev" && (c.Auth0Domain == "" || c.Auth0Audience == "")
}

func (c Config) IsProd() bool { return c.Env == "prod" }

// Load reads and validates configuration, returning every problem at once
// rather than one per restart.
func Load(version string) (Config, error) {
	var problems []string

	cfg := Config{
		Env:                 getenv("APP_ENV", "dev"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		ShutdownGrace:       getduration("SHUTDOWN_GRACE", 8*time.Second),
		HealthTimeout:       getduration("HEALTH_TIMEOUT", 2*time.Second),
		Version:             version,
		Auth0Domain:         strings.TrimSpace(os.Getenv("AUTH0_DOMAIN")),
		Auth0Audience:       strings.TrimSpace(os.Getenv("AUTH0_AUDIENCE")),
		AILabAPIKey:         strings.TrimSpace(os.Getenv("AILAB_API_KEY")),
		ScanWeeklyAllowance: atoiOrZero(os.Getenv("SCAN_WEEKLY_ALLOWANCE")),
	}

	port, err := strconv.Atoi(getenv("PORT", "8080"))
	if err != nil || port <= 0 || port > 65535 {
		problems = append(problems, "PORT must be a valid port number")
	}
	cfg.Port = port

	switch cfg.Env {
	case "dev", "staging", "prod":
	default:
		problems = append(problems, `APP_ENV must be one of "dev", "staging", "prod"`)
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}

	// Neon terminates unencrypted connections, and a URL without sslmode is
	// the most common first-deploy failure. Catching it here turns a confusing
	// runtime error into a startup message that names the fix.
	if cfg.DatabaseURL != "" && !strings.Contains(cfg.DatabaseURL, "sslmode=") {
		problems = append(problems,
			"DATABASE_URL should carry ?sslmode=require (Neon refuses unencrypted connections)")
	}

	if cfg.ShutdownGrace >= 10*time.Second {
		problems = append(problems,
			"SHUTDOWN_GRACE must be under 10s or Cloud Run will SIGKILL mid-request")
	}

	// Outside dev, Auth0 is not optional. Without this the service would fall
	// back to the dev authenticator — which trusts a header — and the failure
	// would be silent: everything works, and anyone can be anyone.
	//
	// Checked here rather than at the wiring site so a misconfigured deploy
	// dies at startup with a message naming the missing variable, instead of
	// serving traffic with no authentication.
	if cfg.Env != "dev" {
		if cfg.Auth0Domain == "" {
			problems = append(problems, "AUTH0_DOMAIN is required outside dev")
		}
		if cfg.Auth0Audience == "" {
			problems = append(problems, "AUTH0_AUDIENCE is required outside dev")
		}
	}

	// A domain carrying a scheme is a configuration mistake that would
	// otherwise produce "https://https://..." and an opaque fetch failure.
	if strings.Contains(cfg.Auth0Domain, "://") {
		problems = append(problems,
			`AUTH0_DOMAIN must be a bare host such as "ayna.eu.auth0.com", with no scheme`)
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// Redacted returns the config with the database credentials removed, for
// logging at startup. Connection strings carry passwords, and a log line is
// the easiest place in the world to leak one.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"env":            c.Env,
		"port":           c.Port,
		"version":        c.Version,
		"database":       redactURL(c.DatabaseURL),
		"shutdown_grace": c.ShutdownGrace.String(),
	}
}

func redactURL(raw string) string {
	if raw == "" {
		return "(unset)"
	}
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return "(set)"
	}
	return raw[:scheme+3] + "***:***" + raw[at:]
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getduration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

var ErrNotConfigured = errors.New("not configured")

// atoiOrZero parses an optional numeric setting, treating anything unparseable
// as unset. A malformed override should fall back to the safe default rather
// than refuse to boot -- this governs a spending limit, and the default is the
// conservative one.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
