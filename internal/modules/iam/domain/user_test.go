package domain

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)

func mustUser(t *testing.T, birthYear int) *User {
	t.Helper()
	u, err := NewUser("usr_test", "auth0|abc", birthYear, "Asia/Karachi", "Sana", now)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	return u
}

// ---------------------------------------------------------------------------
// The age gate (PD-1)
// ---------------------------------------------------------------------------

func TestTheAgeGateRefusesCreationRatherThanFlaggingAUser(t *testing.T) {
	// Refusing at construction is the whole design: an underage account is
	// never created, so there is never an underage record to purge, leak, or
	// forget to filter out of a query somewhere.
	_, err := NewUser("usr_x", "auth0|abc", 2015, "UTC", "", now)
	if !errors.Is(err, ErrUnderMinimumAge) {
		t.Fatalf("got %v, want ErrUnderMinimumAge", err)
	}
}

func TestTheAgeGateBoundary(t *testing.T) {
	for _, c := range []struct {
		name      string
		birthYear int
		wantErr   bool
	}{
		{"comfortably over", 1990, false},
		{"turns 18 this year", now.Year() - 18, false},
		{"turns 17 this year", now.Year() - 17, true},
		{"born this year", now.Year(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewUser("usr_x", "auth0|abc", c.birthYear, "UTC", "", now)
			gotErr := errors.Is(err, ErrUnderMinimumAge)
			if gotErr != c.wantErr {
				t.Errorf("birthYear %d: err=%v, wantUnderAge=%v", c.birthYear, err, c.wantErr)
			}
		})
	}
}

func TestAgeIsDerivedFromTheYearAloneAndThatIsDeliberate(t *testing.T) {
	// Only the year is stored (PD-1), so everyone is treated as born on
	// 1 January. Someone who turns 18 in December passes in January.
	//
	// This test exists to make that a recorded decision rather than an
	// accident: the alternative is collecting a full date of birth, and the
	// privacy cost of that outweighs up to a year of imprecision on a policy
	// line that is not a legal age of consent.
	january := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	if _, err := NewUser("usr_x", "auth0|abc", 2008, "UTC", "", january); err != nil {
		t.Errorf("expected the year-only approximation to admit this user, got %v", err)
	}
}

func TestUserRejectsMissingAuth0Subject(t *testing.T) {
	// The sub is the join key to the identity provider. A user without one is
	// unreachable: no request could ever resolve to it.
	for _, sub := range []string{"", "   "} {
		if _, err := NewUser("usr_x", sub, 1990, "UTC", "", now); !errors.Is(err, ErrMissingAuth0Sub) {
			t.Errorf("sub %q: got %v, want ErrMissingAuth0Sub", sub, err)
		}
	}
}

func TestUserRejectsAnUnusableTimezone(t *testing.T) {
	// Caught at construction because PD-5 computes streak weeks in this zone.
	// An invalid value would surface much later as a wrong week boundary,
	// which looks like a streak bug rather than a bad registration.
	if _, err := NewUser("usr_x", "auth0|abc", 1990, "Mars/Olympus_Mons", "", now); !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("got %v, want ErrInvalidTimezone", err)
	}
}

func TestBirthYearInTheFutureIsRejected(t *testing.T) {
	if _, err := NewUser("usr_x", "auth0|abc", now.Year()+1, "UTC", "", now); !errors.Is(err, ErrInvalidBirthYear) {
		t.Errorf("got %v, want ErrInvalidBirthYear", err)
	}
}

// ---------------------------------------------------------------------------
// Rehydration
// ---------------------------------------------------------------------------

func TestRehydrateDoesNotReapplyTheAgeGate(t *testing.T) {
	// If MinimumAge were ever raised, re-validating on load would lock out
	// existing users mid-session — a policy change disguised as a bug. The
	// rules that applied at creation are the rules that govern the account.
	u, err := Rehydrate("usr_old", "auth0|old", 2015, "UTC", "", nil, now, nil)
	if err != nil {
		t.Fatalf("Rehydrate refused a stored user: %v", err)
	}
	if u.BirthYear() != 2015 {
		t.Errorf("birth year = %d, want 2015", u.BirthYear())
	}
}

// ---------------------------------------------------------------------------
// Deletion (FR-11)
// ---------------------------------------------------------------------------

func TestDeletionIsRequestedOnceAndNotReStamped(t *testing.T) {
	u := mustUser(t, 1990)

	if err := u.RequestDeletion(now); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if !u.IsPendingDeletion() {
		t.Fatal("user should be pending deletion")
	}

	// A second request must not silently extend the SLA the user was already
	// given.
	later := now.Add(48 * time.Hour)
	if err := u.RequestDeletion(later); !errors.Is(err, ErrAlreadyPendingDeletion) {
		t.Errorf("second request: got %v, want ErrAlreadyPendingDeletion", err)
	}
	if got := *u.DeletionRequestedAt(); !got.Equal(now) {
		t.Errorf("timestamp moved to %v; it must stay at the original %v", got, now)
	}
}

// ---------------------------------------------------------------------------
// Timezone (PD-5)
// ---------------------------------------------------------------------------

func TestChangingTimezoneDoesNotRewriteHistory(t *testing.T) {
	// The method changes where future weeks begin and nothing else. It has no
	// access to past streaks by design — a user who watched a streak reach
	// three weeks must not see it become two because they flew somewhere.
	u := mustUser(t, 1990)
	before := u.CreatedAt()

	if err := u.ChangeTimezone("Europe/London"); err != nil {
		t.Fatalf("ChangeTimezone: %v", err)
	}
	if u.Timezone().String() != "Europe/London" {
		t.Errorf("timezone = %q, want Europe/London", u.Timezone())
	}
	if !u.CreatedAt().Equal(before) {
		t.Error("changing timezone must not touch createdAt")
	}
}

func TestChangingToAnInvalidTimezoneLeavesTheOldOneIntact(t *testing.T) {
	u := mustUser(t, 1990)
	if err := u.ChangeTimezone("Nowhere/Nothing"); err == nil {
		t.Fatal("expected an error")
	}
	if u.Timezone().String() != "Asia/Karachi" {
		t.Errorf("timezone became %q after a failed change; it must be unchanged", u.Timezone())
	}
}

func TestAgeAtIsTheNumberTheReportComparesSkinAgeAgainst(t *testing.T) {
	u := mustUser(t, 2000)
	if got := u.AgeAt(now); got != 26 {
		t.Errorf("AgeAt = %d, want 26", got)
	}
}

func TestNewUserGetsAFiniteRetentionByDefault(t *testing.T) {
	// The product promises photos are deleted on a schedule. A default of
	// "keep forever" would make that true only for users who went looking for
	// the setting, which is the majority of them never.
	u, err := NewUser("usr_1", "auth0|1", 1999, "Asia/Karachi", "", time.Now())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	got := u.PhotoRetentionDays()
	if got == nil {
		t.Fatal("a new user defaulted to keeping photos indefinitely")
	}
	if *got != DefaultPhotoRetentionDays {
		t.Errorf("default retention = %d, want %d", *got, DefaultPhotoRetentionDays)
	}
}

func TestSetPhotoRetentionAcceptsOnlyTheOfferedOptions(t *testing.T) {
	u, err := NewUser("usr_1", "auth0|1", 1999, "UTC", "", time.Now())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}

	for _, days := range PhotoRetentionOptions {
		d := days
		if err := u.SetPhotoRetention(&d); err != nil {
			t.Errorf("rejected an offered option %d: %v", days, err)
		}
	}

	// nil is the fourth choice, not an invalid one.
	if err := u.SetPhotoRetention(nil); err != nil {
		t.Errorf("rejected 'keep indefinitely': %v", err)
	}
	if u.PhotoRetentionDays() != nil {
		t.Error("nil retention did not clear the policy")
	}

	// The value goes straight into a SQL interval, so anything outside the
	// closed set is a way to write a policy that silently never expires.
	for _, bad := range []int{0, -1, 1, 3650} {
		b := bad
		if err := u.SetPhotoRetention(&b); !errors.Is(err, ErrInvalidRetention) {
			t.Errorf("accepted %d days, want ErrInvalidRetention, got %v", bad, err)
		}
	}
}

func TestSetPhotoRetentionCopiesTheValue(t *testing.T) {
	// The handler decodes into a struct it may reuse. Aliasing the caller's
	// pointer would let a later request mutate a persisted aggregate.
	u, err := NewUser("usr_1", "auth0|1", 1999, "UTC", "", time.Now())
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}

	days := 7
	if err := u.SetPhotoRetention(&days); err != nil {
		t.Fatalf("SetPhotoRetention: %v", err)
	}
	days = 365

	if got := *u.PhotoRetentionDays(); got != 7 {
		t.Errorf("retention followed the caller's pointer: got %d, want 7", got)
	}
}

func TestRehydrateAcceptsARetentionNoLongerOffered(t *testing.T) {
	// Same rule as the age gate: a value that was legal when written stays
	// legal. Narrowing the option list must not make existing rows unloadable.
	legacy := 90
	u, err := Rehydrate("usr_1", "auth0|1", 1999, "UTC", "", nil, time.Now(), &legacy)
	if err != nil {
		t.Fatalf("Rehydrate refused a stored retention value: %v", err)
	}
	if got := *u.PhotoRetentionDays(); got != 90 {
		t.Errorf("retention = %d, want the stored 90", got)
	}
}
