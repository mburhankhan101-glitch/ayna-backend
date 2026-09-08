package config

import (
	"strings"
	"testing"
	"time"
)

const validURL = "postgres://u:p@host/db?sslmode=require"

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadAcceptsAValidEnvironment(t *testing.T) {
	// Auth0 is part of what "valid" means in prod now — a prod config without
	// it is exactly the misconfiguration TestAuth0IsMandatoryOutsideDev exists
	// to catch.
	setEnv(t, map[string]string{
		"APP_ENV": "prod", "PORT": "9090", "DATABASE_URL": validURL,
		"AUTH0_DOMAIN": "ayna.eu.auth0.com", "AUTH0_AUDIENCE": "https://api.ayna.app",
	})

	cfg, err := Load("abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 || !cfg.IsProd() || cfg.Version != "abc123" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	// One problem per restart is a miserable way to configure a deploy: you
	// fix the port, redeploy, discover the URL is wrong, redeploy again.
	setEnv(t, map[string]string{
		"APP_ENV": "production", "PORT": "0", "DATABASE_URL": "",
	})

	_, err := Load("v")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"APP_ENV", "PORT", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s:\n%v", want, err)
		}
	}
}

func TestMissingSSLModeIsCaughtAtStartup(t *testing.T) {
	// The most common first-deploy failure against Neon. Catching it here
	// turns an opaque runtime error into a message that names the fix.
	setEnv(t, map[string]string{
		"APP_ENV": "dev", "DATABASE_URL": "postgres://u:p@host/db",
	})

	_, err := Load("v")
	if err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Errorf("expected an sslmode complaint, got: %v", err)
	}
}

func TestShutdownGraceMustFitCloudRunsKillWindow(t *testing.T) {
	// Cloud Run SIGKILLs 10s after SIGTERM. A grace period at or beyond that
	// guarantees requests are cut off rather than drained.
	setEnv(t, map[string]string{
		"APP_ENV": "dev", "DATABASE_URL": validURL, "SHUTDOWN_GRACE": "15s",
	})

	_, err := Load("v")
	if err == nil || !strings.Contains(err.Error(), "SIGKILL") {
		t.Errorf("expected a shutdown-grace complaint, got: %v", err)
	}
}

func TestDefaultsAreSane(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": validURL})

	cfg, err := Load("v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080 (Cloud Run's default)", cfg.Port)
	}
	if cfg.Env != "dev" {
		t.Errorf("Env = %q, want dev -- an unset env must never default to prod", cfg.Env)
	}
	if cfg.ShutdownGrace >= 10*time.Second {
		t.Errorf("default grace %v is not under the SIGKILL window", cfg.ShutdownGrace)
	}
}

func TestRedactedNeverLeaksThePassword(t *testing.T) {
	cfg := Config{DatabaseURL: "postgres://alice:hunter2@ep-x.neon.tech/ayna?sslmode=require"}

	got, _ := cfg.Redacted()["database"].(string)
	if strings.Contains(got, "hunter2") || strings.Contains(got, "alice") {
		t.Fatalf("credentials leaked into logs: %q", got)
	}
	if !strings.Contains(got, "neon.tech") {
		t.Errorf("redaction removed too much to be useful: %q", got)
	}
}

func TestRedactedHandlesMalformedURLsWithoutLeaking(t *testing.T) {
	// A URL that fails to parse must fail closed. Returning the raw string on
	// the "I couldn't parse it" path is how secrets reach log aggregators.
	for _, raw := range []string{"", "not-a-url", "postgres://nopassword/db", "@@@"} {
		got, _ := Config{DatabaseURL: raw}.Redacted()["database"].(string)
		if raw != "" && got == raw && strings.Contains(raw, "://") {
			t.Errorf("Redacted() returned the raw URL for %q", raw)
		}
	}
}

// ---------------------------------------------------------------------------
// Auth0
// ---------------------------------------------------------------------------

func TestAuth0IsMandatoryOutsideDev(t *testing.T) {
	// Without this check a production deploy that forgot the Auth0 variables
	// would fall back to the dev authenticator, which trusts a header. The
	// failure would be silent: everything works, and anyone can be anyone.
	for _, env := range []string{"prod", "staging"} {
		setEnv(t, map[string]string{
			"APP_ENV": env, "DATABASE_URL": validURL,
			"AUTH0_DOMAIN": "", "AUTH0_AUDIENCE": "",
		})

		_, err := Load("v")
		if err == nil {
			t.Fatalf("env %q: config loaded with no Auth0 configuration", env)
		}
		for _, want := range []string{"AUTH0_DOMAIN", "AUTH0_AUDIENCE"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("env %q: error should name %s:\n%v", env, want, err)
			}
		}
	}
}

func TestDevAuthIsOnlyPossibleInDevAndOnlyWhenAuth0IsAbsent(t *testing.T) {
	setEnv(t, map[string]string{
		"APP_ENV": "dev", "DATABASE_URL": validURL,
		"AUTH0_DOMAIN": "", "AUTH0_AUDIENCE": "",
	})
	cfg, err := Load("v")
	if err != nil {
		t.Fatalf("dev without auth0 should load: %v", err)
	}
	if !cfg.UsesDevAuth() {
		t.Error("dev with no Auth0 config should use the dev authenticator")
	}

	// Configuring Auth0 locally switches a dev machine onto real verification
	// with no other change — which is how you test the real path before deploy.
	setEnv(t, map[string]string{
		"APP_ENV": "dev", "DATABASE_URL": validURL,
		"AUTH0_DOMAIN": "ayna.eu.auth0.com", "AUTH0_AUDIENCE": "https://api.ayna.app",
	})
	cfg, err = Load("v")
	if err != nil {
		t.Fatalf("dev with auth0 should load: %v", err)
	}
	if cfg.UsesDevAuth() {
		t.Error("dev WITH Auth0 configured must use the real verifier")
	}
}

func TestAuth0DomainMustNotCarryAScheme(t *testing.T) {
	// A domain with a scheme produces "https://https://..." and an opaque
	// fetch failure at the first request rather than at startup.
	setEnv(t, map[string]string{
		"APP_ENV": "dev", "DATABASE_URL": validURL,
		"AUTH0_DOMAIN": "https://ayna.eu.auth0.com", "AUTH0_AUDIENCE": "https://api.ayna.app",
	})
	if _, err := Load("v"); err == nil || !strings.Contains(err.Error(), "no scheme") {
		t.Errorf("got %v, want a complaint about the scheme", err)
	}
}
