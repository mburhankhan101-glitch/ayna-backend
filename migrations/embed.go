// Package migrations embeds the SQL migration files into the binary.
//
// Embedded rather than read from disk so `cmd/migrate` is a single self-
// contained binary: the container that runs a migration carries the exact SQL
// that was reviewed and committed, with no chance of it running against a
// different working directory or a stale volume.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
