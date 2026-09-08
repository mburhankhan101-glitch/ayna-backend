package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the query surface shared by a connection pool and a transaction.
//
// Repositories accept this rather than *pgxpool.Pool, and that choice is
// load-bearing rather than stylistic. ADR-004 requires the Scan and its
// analysis job to be written in ONE transaction — if a repository could only
// be handed a pool, it would open its own connection and that guarantee would
// be impossible to express. Taking DBTX means the caller decides the
// transaction boundary, which is where that decision belongs.
//
// It also makes tests honest: an integration test can run inside a transaction
// it rolls back, so it exercises real SQL against a real database without
// leaving anything behind.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Both satisfy DBTX. Asserted at compile time so a pgx upgrade that changes
// either signature fails here rather than at the first call site.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
//
// The rollback is deferred rather than written on each error path: a function
// with four returns has four chances to forget one, and a leaked transaction
// holds a connection and its locks until the pool reaps it.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			// Roll back before re-panicking, or the connection is returned to
			// the pool mid-transaction and poisons whoever gets it next.
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation, optionally narrowed to one constraint by name.
//
// Uniqueness has to be enforced by the database — a check-then-insert in
// application code is a race that two concurrent sign-ups will eventually win.
// So the insert is attempted and this translates the resulting driver error
// into something the domain can talk about.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" { // unique_violation
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}
