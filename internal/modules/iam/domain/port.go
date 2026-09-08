package domain

import (
	"context"
	"errors"
)

// ErrUserNotFound is returned by repositories rather than a driver-specific
// "no rows" error. The application layer must be able to branch on absence
// without knowing that Postgres is underneath.
var ErrUserNotFound = errors.New("user not found")

// ErrAuth0SubTaken means a profile already exists for this identity — the
// caller signed up twice, usually by retrying a request that succeeded.
var ErrAuth0SubTaken = errors.New("a profile already exists for this identity")

// UserRepository is a PORT: an interface defined by the domain describing what
// it needs, not what a database happens to offer.
//
// That direction is the actual dependency inversion. Postgres implements this;
// this never mentions Postgres. Swapping the store, or standing IAM up as its
// own service, changes the implementation and nothing above it.
type UserRepository interface {
	// Save persists a new user. It must return ErrAuth0SubTaken rather than a
	// raw constraint violation, so uniqueness is enforced by the database (the
	// only place it can be enforced correctly under concurrency) while the
	// caller still sees a domain error.
	Save(ctx context.Context, u *User) error

	// FindByAuth0Sub resolves the caller of an authenticated request. This is
	// the hottest read in the system.
	FindByAuth0Sub(ctx context.Context, sub string) (*User, error)

	FindByID(ctx context.Context, id string) (*User, error)

	Update(ctx context.Context, u *User) error

	// RetentionCohorts groups users by their photo retention policy (NFR-4).
	//
	// Grouped rather than returned per user because there are only three legal
	// values, so this is at most three rows however many accounts exist. The
	// sweep then issues one delete per cohort instead of one per user.
	//
	// Users who chose "keep indefinitely" are absent, not present with a nil
	// policy. A cohort the sweep must never touch has no business being handed
	// to it at all.
	//
	// The honest limit: each cohort carries its user IDs, so this loads every
	// id with a finite policy into memory. At this product's scale that is
	// nothing. If it ever stops being nothing, the fix is to page this rather
	// than to join across the two tables, which would be the shortcut that
	// welds IAM and scans together.
	RetentionCohorts(ctx context.Context) ([]RetentionCohort, error)
}

// RetentionCohort is the set of users sharing one photo retention policy.
type RetentionCohort struct {
	Days    int
	UserIDs []string
}

// ConsentRepository stores the append-only consent history.
//
// There is no Update and no Delete, and their absence is the contract: a
// revocation is an Append with Granted false. An interface that offered
// deletion would invite exactly the edit that destroys the audit trail.
type ConsentRepository interface {
	Append(ctx context.Context, r ConsentRecord) error

	// LatestFor returns the most recent record for one policy version, or a
	// zero ConsentState when the user has never responded to it.
	LatestFor(ctx context.Context, userID, policyVersion string) (ConsentState, error)
}
