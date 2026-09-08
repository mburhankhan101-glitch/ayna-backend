// Package application holds the IAM use cases.
//
// A use case orchestrates domain objects and calls ports. It never touches
// concrete infrastructure: everything it needs arrives as an interface defined
// by the domain, which is what lets these be tested with fakes and lets the
// storage change without them noticing.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ayna/ayna-backend/internal/modules/iam/domain"
	"github.com/ayna/ayna-backend/internal/platform/id"
)

// Clock is injected rather than calling time.Now() directly, so the age gate's
// boundary behaviour is testable. "Turns 18 today" must not be decided by
// whatever the wall clock says during a test run.
type Clock func() time.Time

// SystemClock is the production clock.
func SystemClock() time.Time { return time.Now().UTC() }

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

type RegisterUserInput struct {
	Auth0Sub    string
	BirthYear   int
	Timezone    string
	DisplayName string
}

type RegisterUser struct {
	users domain.UserRepository
	now   Clock
}

func NewRegisterUser(users domain.UserRepository, now Clock) *RegisterUser {
	return &RegisterUser{users: users, now: now}
}

// Execute creates the local profile after Auth0 has authenticated someone for
// the first time.
//
// The age gate is applied by domain.NewUser, not here. That placement is the
// point: a second entry path into user creation would otherwise be a second
// chance to forget the check, and PD-1 is not a rule that survives being
// remembered.
func (uc *RegisterUser) Execute(ctx context.Context, in RegisterUserInput) (*domain.User, error) {
	u, err := domain.NewUser(
		id.New(id.PrefixUser),
		in.Auth0Sub,
		in.BirthYear,
		in.Timezone,
		in.DisplayName,
		uc.now(),
	)
	if err != nil {
		return nil, err // domain errors pass through; transport maps them to status codes
	}

	if err := uc.users.Save(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// Reading the current user
// ---------------------------------------------------------------------------

type GetCurrentUser struct {
	users domain.UserRepository
}

func NewGetCurrentUser(users domain.UserRepository) *GetCurrentUser {
	return &GetCurrentUser{users: users}
}

func (uc *GetCurrentUser) Execute(ctx context.Context, auth0Sub string) (*domain.User, error) {
	return uc.users.FindByAuth0Sub(ctx, auth0Sub)
}

// ---------------------------------------------------------------------------
// Consent
// ---------------------------------------------------------------------------

type GrantConsentInput struct {
	Auth0Sub      string
	PolicyVersion string
	Granted       bool
}

type GrantConsent struct {
	users    domain.UserRepository
	consents domain.ConsentRepository
	now      Clock
}

func NewGrantConsent(u domain.UserRepository, c domain.ConsentRepository, now Clock) *GrantConsent {
	return &GrantConsent{users: u, consents: c, now: now}
}

// Execute records a consent decision (FR-1).
//
// `Granted: false` is a revocation and is just as valid an outcome — it
// appends a record rather than deleting the grant, because the question asked
// later is "was this user consented when that scan ran?", and only a history
// can answer it.
func (uc *GrantConsent) Execute(ctx context.Context, in GrantConsentInput) (domain.ConsentRecord, error) {
	u, err := uc.users.FindByAuth0Sub(ctx, in.Auth0Sub)
	if err != nil {
		return domain.ConsentRecord{}, err
	}

	rec, err := domain.NewConsentRecord(
		id.New(id.PrefixConsent), u.ID(), in.PolicyVersion, in.Granted, uc.now(),
	)
	if err != nil {
		return domain.ConsentRecord{}, err
	}

	if err := uc.consents.Append(ctx, rec); err != nil {
		return domain.ConsentRecord{}, err
	}
	return rec, nil
}

// ConsentStatus answers whether the user may submit a scan under the policy
// version currently in force.
type ConsentStatus struct {
	users    domain.UserRepository
	consents domain.ConsentRepository
}

func NewConsentStatus(u domain.UserRepository, c domain.ConsentRepository) *ConsentStatus {
	return &ConsentStatus{users: u, consents: c}
}

func (uc *ConsentStatus) Execute(ctx context.Context, auth0Sub, policyVersion string) (domain.ConsentState, error) {
	u, err := uc.users.FindByAuth0Sub(ctx, auth0Sub)
	if err != nil {
		return domain.ConsentState{}, err
	}
	return uc.consents.LatestFor(ctx, u.ID(), policyVersion)
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

// DeletionSLA is how long the purge is promised to take (FR-11). Returned to
// the client so it can state a real timeframe rather than "soon".
const DeletionSLA = 30 * 24 * time.Hour

type RequestDeletion struct {
	users domain.UserRepository
	now   Clock
}

func NewRequestDeletion(users domain.UserRepository, now Clock) *RequestDeletion {
	return &RequestDeletion{users: users, now: now}
}

// Execute marks the account for purge.
//
// A repeat request is not an error to the caller: someone tapping "delete my
// account" twice has expressed the same wish twice, and the second tap should
// not produce a failure screen. The domain refuses to re-stamp the timestamp,
// so the SLA the user was already given cannot be silently extended, and that
// refusal is absorbed here rather than surfaced.
func (uc *RequestDeletion) Execute(ctx context.Context, auth0Sub string) (requestedAt, completesBy time.Time, err error) {
	u, err := uc.users.FindByAuth0Sub(ctx, auth0Sub)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	if err := u.RequestDeletion(uc.now()); err != nil {
		if errors.Is(err, domain.ErrAlreadyPendingDeletion) {
			at := *u.DeletionRequestedAt()
			return at, at.Add(DeletionSLA), nil
		}
		return time.Time{}, time.Time{}, err
	}

	if err := uc.users.Update(ctx, u); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("persist deletion request: %w", err)
	}

	at := *u.DeletionRequestedAt()
	return at, at.Add(DeletionSLA), nil
}

// ---------------------------------------------------------------------------
// Photo retention (NFR-4)
// ---------------------------------------------------------------------------

// SetPhotoRetention changes how long a user's photos are kept.
//
// It only records the policy. Nothing is deleted here: the sweep in the worker
// is what removes images, on its own schedule. That separation is deliberate --
// a settings tap should not block on a bulk delete, and a delete that runs only
// when someone happens to open settings is not a retention policy.
type SetPhotoRetention struct {
	users domain.UserRepository
}

func NewSetPhotoRetention(users domain.UserRepository) *SetPhotoRetention {
	return &SetPhotoRetention{users: users}
}

// Execute validates and saves. `days` is nil for "keep indefinitely".
func (uc *SetPhotoRetention) Execute(ctx context.Context, auth0Sub string, days *int) (*domain.User, error) {
	u, err := uc.users.FindByAuth0Sub(ctx, auth0Sub)
	if err != nil {
		return nil, err
	}
	if err := u.SetPhotoRetention(days); err != nil {
		return nil, err
	}
	if err := uc.users.Update(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}
