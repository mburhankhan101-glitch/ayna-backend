// Package domain holds the IAM entities and the rules that govern them.
//
// It imports nothing outside the standard library — enforced by
// internal/arch, which fails the build otherwise. That constraint is what
// makes these rules testable without a database and portable if IAM is ever
// split into its own service (NFR-8).
//
// # What is ours and what is Auth0's
//
// Auth0 answers "who is this person": credentials, OTP delivery, tokens,
// session lifetime. None of that is here.
//
// What is here is everything about *what they are allowed to do*: whether
// they are old enough to have an account at all (PD-1), whether they have
// consented to photo processing (FR-1), and which timezone their week
// boundaries are computed in (PD-5). That distinction is the whole reason
// this module is not a thin wrapper around an SDK.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MinimumAge is PD-1, and it is a product decision rather than a technical
// one. The product combines face photos, health-adjacent claims and
// appearance scoring; restricting to adults removes parental-consent
// branches, child-directed store classification, and the need to defend
// scoring a fifteen-year-old's face.
//
// Widening this later is easy. Narrowing it after minors hold accounts is
// not — which is why the check refuses creation rather than flagging a
// created user.
const MinimumAge = 18

var (
	ErrUnderMinimumAge        = errors.New("below the minimum age")
	ErrMissingAuth0Sub        = errors.New("auth0 subject is required")
	ErrInvalidTimezone        = errors.New("timezone must be a valid IANA zone")
	ErrInvalidBirthYear       = errors.New("birth year is out of range")
	ErrAlreadyPendingDeletion = errors.New("deletion already requested")
	ErrInvalidRetention       = errors.New("photo retention must be 7, 30 or 365 days, or unset")
)

// PhotoRetentionOptions are the only accepted values, in days (NFR-4).
//
// A closed set rather than any positive integer. The sweep turns this value
// straight into a SQL interval, and "any integer" there is a way for 100000 to
// mean "never delete" while looking like a retention policy. It also keeps the
// settings screen and the API in agreement about what can be chosen, without
// either having to be the source of truth.
//
// nil is the fourth option and means keep indefinitely. It is deliberately not
// in this list: it is the absence of a policy, not a longer one.
var PhotoRetentionOptions = []int{7, 30, 365}

// DefaultPhotoRetentionDays is what a new account gets before anyone touches
// settings.
//
// A finite default, not "keep forever". The product's promise is that photos
// go away on a schedule; a default of "never" would make that true only for
// users who went looking for the control.
const DefaultPhotoRetentionDays = 30

// User is the local profile.
//
// Fields are unexported with accessors so the invariants below cannot be
// bypassed by constructing the struct directly. A `User{}` built by hand
// would be one that never passed the age gate.
type User struct {
	id                  string
	auth0Sub            string
	birthYear           int
	timezone            *time.Location
	displayName         string
	deletionRequestedAt *time.Time
	createdAt           time.Time

	// nil means keep photos indefinitely. See PhotoRetentionOptions.
	photoRetentionDays *int
}

// NewUser applies the age gate and constructs a valid user, or fails.
//
// `now` is a parameter rather than a call to time.Now() so the boundary
// behaviour is testable — someone turning 18 today is exactly the case that
// must not be decided by whatever the clock happens to say during a test run.
func NewUser(id, auth0Sub string, birthYear int, tz string, displayName string, now time.Time) (*User, error) {
	if strings.TrimSpace(auth0Sub) == "" {
		return nil, ErrMissingAuth0Sub
	}
	if birthYear < 1900 || birthYear > now.Year() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidBirthYear, birthYear)
	}

	// Age from the year alone, which is the only thing stored (PD-1). This
	// necessarily treats everyone as having their birthday on 1 January, so it
	// admits people who turn 18 later this calendar year.
	//
	// That is the deliberate direction to be wrong in: the alternative is
	// collecting a full date of birth, and the privacy cost of that outweighs
	// up to a year of imprecision on a gate that is a policy line rather than
	// a legal age of consent.
	if now.Year()-birthYear < MinimumAge {
		return nil, ErrUnderMinimumAge
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTimezone, tz)
	}

	defaultRetention := DefaultPhotoRetentionDays

	return &User{
		id:          id,
		auth0Sub:    auth0Sub,
		birthYear:   birthYear,
		timezone:    loc,
		displayName: strings.TrimSpace(displayName),
		createdAt:   now,

		// Set here as well as in the column DEFAULT. The database default
		// covers rows written by anything that bypasses this constructor;
		// this covers a User that is created and read back in memory without
		// ever round-tripping, which is most of the tests.
		photoRetentionDays: &defaultRetention,
	}, nil
}

// Rehydrate rebuilds a User from storage WITHOUT re-running the age gate.
//
// A repository must not re-validate: the rules that applied when the account
// was created are the rules that governed it. If MinimumAge were ever raised,
// re-validating on load would silently lock out existing users mid-session,
// which is a policy change disguised as a bug.
func Rehydrate(
	id, auth0Sub string,
	birthYear int,
	tz string,
	displayName string,
	deletionRequestedAt *time.Time,
	createdAt time.Time,
	photoRetentionDays *int,
) (*User, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTimezone, tz)
	}
	return &User{
		id:                  id,
		auth0Sub:            auth0Sub,
		birthYear:           birthYear,
		timezone:            loc,
		displayName:         displayName,
		deletionRequestedAt: deletionRequestedAt,
		createdAt:           createdAt,

		// NOT validated against PhotoRetentionOptions, for the same reason the
		// age gate is not re-run: a value that was legal when it was written
		// stays legal. Narrowing the option list later must not make existing
		// rows unloadable.
		photoRetentionDays: photoRetentionDays,
	}, nil
}

func (u *User) ID() string                      { return u.id }
func (u *User) Auth0Sub() string                { return u.auth0Sub }
func (u *User) BirthYear() int                  { return u.birthYear }
func (u *User) Timezone() *time.Location        { return u.timezone }
func (u *User) DisplayName() string             { return u.displayName }
func (u *User) CreatedAt() time.Time            { return u.createdAt }
func (u *User) DeletionRequestedAt() *time.Time { return u.deletionRequestedAt }

// PhotoRetentionDays is how long this user's photos are kept, or nil for
// indefinitely (NFR-4).
func (u *User) PhotoRetentionDays() *int { return u.photoRetentionDays }

// SetPhotoRetention changes the policy. nil means keep indefinitely.
//
// Shortening it does not delete anything here. The sweep is what removes
// images, and it runs on its own schedule -- so a user who picks 7 days keeps
// last week's overlay until the next sweep passes over it. That lag is worth
// naming because the settings copy must not promise an instant purge it does
// not perform; "Delete my photos after" is a policy, and the screen says so.
func (u *User) SetPhotoRetention(days *int) error {
	if days == nil {
		u.photoRetentionDays = nil
		return nil
	}
	for _, allowed := range PhotoRetentionOptions {
		if *days == allowed {
			v := *days
			// Copied rather than aliased. Storing the caller's pointer would
			// let a handler that reuses its decode buffer mutate a persisted
			// aggregate from the outside.
			u.photoRetentionDays = &v
			return nil
		}
	}
	return fmt.Errorf("%w: %d", ErrInvalidRetention, *days)
}

// AgeAt is the chronological age the report screen compares skin age against
// (FR-5). Same year-only approximation as the gate, and the same reason.
func (u *User) AgeAt(now time.Time) int { return now.Year() - u.birthYear }

// IsPendingDeletion reports whether a purge is in flight. Such a user must not
// be able to submit new scans — accepting work for an account being destroyed
// wastes a paid vendor call and creates data the purge has already passed.
func (u *User) IsPendingDeletion() bool { return u.deletionRequestedAt != nil }

// RequestDeletion marks the account for purge (FR-11).
//
// Idempotent by refusal rather than silently re-stamping: a second request
// must not extend the SLA the user was already promised.
func (u *User) RequestDeletion(now time.Time) error {
	if u.IsPendingDeletion() {
		return ErrAlreadyPendingDeletion
	}
	u.deletionRequestedAt = &now
	return nil
}

// ChangeTimezone updates where the user's weeks begin.
//
// Deliberately does NOT recompute past streaks. A timezone change would
// otherwise rewrite history the user has already seen — a streak they watched
// reach three weeks silently becoming two is worse than a boundary being
// slightly wrong for one week (PD-5).
func (u *User) ChangeTimezone(tz string) error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidTimezone, tz)
	}
	u.timezone = loc
	return nil
}
