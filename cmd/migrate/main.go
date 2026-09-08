// Command migrate applies database migrations.
//
// # Why this is a separate binary, not something the API does at startup
//
// Cloud Run runs many instances and scales to zero, so "migrate on boot" means
// every cold start races every other cold start to alter the schema. Goose
// takes a lock, so it would not corrupt anything — but it would make every
// cold start wait on a database lock, on the request path, for no benefit.
//
// Worse, it couples deploys to migrations: a migration that fails would crash-
// loop the whole service instead of failing one clearly-labelled step. Schema
// changes are deliberate acts and get their own command.
//
//	migrate up       apply everything pending
//	migrate down     roll back one
//	migrate status   what is applied, what is not
//	migrate version  the current version
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ayna/ayna-backend/internal/platform/config"
	"github.com/ayna/ayna-backend/internal/platform/logger"
	"github.com/ayna/ayna-backend/migrations"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		os.Stderr.WriteString("fatal: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	cfg, err := config.Load(version)
	if err != nil {
		return err
	}
	log := logger.New(cfg.Env, cfg.Version)

	// database/sql rather than pgxpool: goose needs the standard interface,
	// and this connection is short-lived and single-purpose.
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger()) // goose's own output is noisy; we log the outcome
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	before, _ := goose.GetDBVersionContext(ctx, db)

	switch cmd {
	case "up":
		if err := goose.UpContext(ctx, db, "."); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
	case "down":
		if err := goose.DownContext(ctx, db, "."); err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
	case "status":
		// Status is the one command whose value IS goose's own output, so the
		// NopLogger set above is swapped back out for a real one.
		goose.SetLogger(log2goose{})
		return goose.StatusContext(ctx, db, ".")
	case "version":
		v, err := goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return err
		}
		fmt.Printf("schema version: %d\n", v)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want up, down, status or version)", cmd)
	}

	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}

	if before == after {
		log.Info("schema already up to date", "version", after)
	} else {
		log.Info("schema migrated", "from", before, "to", after)
	}
	return nil
}

// log2goose adapts goose's logger interface onto plain stdout. goose does not
// export a constructor for its default logger, so implementing the four
// methods directly is simpler than reaching for one.
type log2goose struct{}

func (log2goose) Fatalf(format string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, format, v...)
	os.Exit(1)
}
func (log2goose) Printf(format string, v ...interface{}) { fmt.Printf(format, v...) }
