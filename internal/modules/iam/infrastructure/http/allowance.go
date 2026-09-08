package http

import (
	"context"
	"time"
)

// Allowance is what the profile reports about remaining scans.
type Allowance struct {
	Remaining int
	Limit     int
	ResetsAt  time.Time
}

// AllowanceSource tells IAM how many scans a user has left.
//
// A narrow port rather than an import of the skinanalysis module: the boundary
// lint forbids one module reaching into another's internals, and this is the
// entire surface IAM needs. The API wires the two together in its composition
// root; neither module knows the other exists.
//
// Nil is allowed and means "assume a fresh allowance". That is not laziness —
// it keeps the profile endpoint working if the scan module is ever unavailable,
// and a profile that fails to load because a *count* could not be computed
// would lock the user out of the whole app over a number on one card.
type AllowanceSource interface {
	For(ctx context.Context, userID string) (Allowance, error)
}
