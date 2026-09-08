// Package database owns the Postgres connection pool.
//
// Pool sizing is the thing worth thinking about here, and it is shaped by two
// facts that pull in opposite directions:
//
//   - Cloud Run scales horizontally. Ten instances at 25 connections each is
//     250 connections, which will exhaust a small Postgres before the
//     application notices anything is wrong.
//   - Neon pools connections at its proxy, so modest per-instance limits cost
//     much less than they would against a bare Postgres.
//
// So the per-instance ceiling is deliberately low. The failure it prevents --
// a traffic spike exhausting the database and taking down every instance at
// once, including the ones serving unrelated traffic -- is exactly the
// scenario NFR-3 says the system must absorb.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

func DefaultConfig(url string) Config {
	return Config{
		URL:      url,
		MaxConns: 10,
		// Zero idle connections: a scaled-to-zero service holding open
		// connections helps nobody, and Neon charges for compute time kept
		// awake by them.
		MinConns:        0,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
}

// Open creates the pool and verifies it can actually reach the database.
//
// pgxpool.New is lazy -- it returns a pool without connecting -- so without
// the explicit Ping a container with a wrong password starts "successfully"
// and fails on the first real request instead of at deploy time.
func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// HealthCheck returns a readiness check for the pool.
//
// It runs `SELECT 1` rather than Ping: Ping can be satisfied by a connection
// that is open but unusable, whereas a query proves the database will actually
// answer. The distinction matters during a Neon cold start, where the
// connection establishes before the compute is ready to serve.
func HealthCheck(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		if one != 1 {
			return fmt.Errorf("unexpected result from SELECT 1: %d", one)
		}
		return nil
	}
}
