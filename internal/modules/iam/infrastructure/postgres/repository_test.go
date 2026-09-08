package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ayna/ayna-backend/internal/modules/iam/domain"
	"github.com/ayna/ayna-backend/internal/platform/id"
)

// These are integration tests: they run real SQL against a real Postgres.
//
// Mocking a database tests the mock. The things most likely to be wrong here
// are the SQL itself, the column mapping, and the translation of driver errors
// into domain errors — and a fake gets all three right by construction, which
// is precisely why it proves nothing.
//
// Every test runs inside a transaction that is ALWAYS rolled back, so the
// suite writes nothing durable. That is what makes it safe to point at a
// development database without a cleanup step to forget.
//
// Skipped when DATABASE_URL is unset, so `go test ./...` stays green on a
// machine with no database (CI included, until it has one).

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration tests")
	}

	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// withTx gives the test a transaction that is rolled back on completion,
// pass or fail.
func withTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()

	tx, err := pool(t).Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Rollback, never commit. If this ever becomes a commit the suite starts
	// leaving rows in a real database.
	defer func() { _ = tx.Rollback(ctx) }()

	fn(ctx, tx)
}

func newTestUser(t *testing.T) *domain.User {
	t.Helper()
	u, err := domain.NewUser(
		id.New(id.PrefixUser),
		"auth0|"+id.New(id.PrefixUser),
		1999, "Asia/Karachi", "Sana",
		time.Now().UTC().Truncate(time.Microsecond),
	)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	return u
}

func TestSaveAndFindRoundTripsEveryField(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)
		want := newTestUser(t)

		if err := repo.Save(ctx, want); err != nil {
			t.Fatalf("Save: %v", err)
		}

		got, err := repo.FindByAuth0Sub(ctx, want.Auth0Sub())
		if err != nil {
			t.Fatalf("FindByAuth0Sub: %v", err)
		}

		if got.ID() != want.ID() {
			t.Errorf("id = %q, want %q", got.ID(), want.ID())
		}
		if got.BirthYear() != want.BirthYear() {
			t.Errorf("birthYear = %d, want %d", got.BirthYear(), want.BirthYear())
		}
		if got.DisplayName() != want.DisplayName() {
			t.Errorf("displayName = %q, want %q", got.DisplayName(), want.DisplayName())
		}
		// The timezone must survive the round trip as an IANA name: PD-5
		// computes week boundaries in it, and a zone that degrades to UTC on
		// read would silently move every streak boundary.
		if got.Timezone().String() != "Asia/Karachi" {
			t.Errorf("timezone = %q, want Asia/Karachi", got.Timezone())
		}
		if got.IsPendingDeletion() {
			t.Error("a fresh user must not be pending deletion")
		}
	})
}

func TestASecondProfileForTheSameIdentityIsRefusedAsADomainError(t *testing.T) {
	// The caller must see domain.ErrAuth0SubTaken, not a raw 23505. If the
	// driver error leaked, the application layer would end up depending on
	// Postgres through its error handling without ever importing it.
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)
		first := newTestUser(t)

		if err := repo.Save(ctx, first); err != nil {
			t.Fatalf("first Save: %v", err)
		}

		duplicate, err := domain.NewUser(
			id.New(id.PrefixUser), first.Auth0Sub(), 1995, "UTC", "", time.Now().UTC(),
		)
		if err != nil {
			t.Fatalf("NewUser: %v", err)
		}

		err = repo.Save(ctx, duplicate)
		if !errors.Is(err, domain.ErrAuth0SubTaken) {
			t.Fatalf("got %v, want domain.ErrAuth0SubTaken", err)
		}
	})
}

func TestMissingUserIsADomainErrorNotPgxErrNoRows(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)

		_, err := repo.FindByAuth0Sub(ctx, "auth0|nobody")
		if !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("FindByAuth0Sub: got %v, want domain.ErrUserNotFound", err)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			t.Error("pgx.ErrNoRows leaked out of the repository")
		}

		if _, err := repo.FindByID(ctx, "usr_nonexistent"); !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("FindByID: got %v, want domain.ErrUserNotFound", err)
		}
	})
}

func TestUpdatePersistsTimezoneAndDeletionRequest(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)
		u := newTestUser(t)
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("Save: %v", err)
		}

		if err := u.ChangeTimezone("Europe/London"); err != nil {
			t.Fatalf("ChangeTimezone: %v", err)
		}
		if err := u.RequestDeletion(time.Now().UTC()); err != nil {
			t.Fatalf("RequestDeletion: %v", err)
		}
		if err := repo.Update(ctx, u); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := repo.FindByID(ctx, u.ID())
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.Timezone().String() != "Europe/London" {
			t.Errorf("timezone = %q, want Europe/London", got.Timezone())
		}
		if !got.IsPendingDeletion() {
			t.Error("deletion request did not persist")
		}
	})
}

func TestUpdatingAMissingRowFailsInsteadOfSilentlyDoingNothing(t *testing.T) {
	// An UPDATE that matches zero rows is not success. The caller believes it
	// saved something; the row is gone (a completed purge, or a bad id).
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)
		ghost := newTestUser(t) // never saved

		if err := repo.Update(ctx, ghost); !errors.Is(err, domain.ErrUserNotFound) {
			t.Errorf("got %v, want domain.ErrUserNotFound", err)
		}
	})
}

func TestUpdateCannotRewriteBirthYearOrIdentity(t *testing.T) {
	// Birth year passed the age gate at creation; letting an update rewrite it
	// would be an edit path around PD-1. auth0_sub is the identity itself.
	// Neither appears in the UPDATE statement, and this proves it.
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		repo := NewUserRepository(tx)
		u := newTestUser(t)
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("Save: %v", err)
		}

		tampered, err := domain.Rehydrate(
			u.ID(), "auth0|someone-else", 2015, "UTC", "", nil, u.CreatedAt(), nil,
		)
		if err != nil {
			t.Fatalf("Rehydrate: %v", err)
		}
		if err := repo.Update(ctx, tampered); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := repo.FindByID(ctx, u.ID())
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.BirthYear() != 1999 {
			t.Errorf("birth year became %d; Update must not touch it", got.BirthYear())
		}
		if got.Auth0Sub() != u.Auth0Sub() {
			t.Errorf("auth0_sub became %q; Update must not touch it", got.Auth0Sub())
		}
	})
}

// ---------------------------------------------------------------------------
// Consent
// ---------------------------------------------------------------------------

func TestConsentHistoryIsAppendOnlyAndTheLatestRecordWins(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		users := NewUserRepository(tx)
		consents := NewConsentRepository(tx)

		u := newTestUser(t)
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save user: %v", err)
		}

		const policy = "2026-08-01"
		base := time.Now().UTC().Truncate(time.Microsecond)

		grant, _ := domain.NewConsentRecord(id.New(id.PrefixConsent), u.ID(), policy, true, base)
		if err := consents.Append(ctx, grant); err != nil {
			t.Fatalf("append grant: %v", err)
		}

		state, err := consents.LatestFor(ctx, u.ID(), policy)
		if err != nil {
			t.Fatalf("LatestFor: %v", err)
		}
		if !state.AllowsScanning() {
			t.Fatal("after granting, scanning should be allowed")
		}

		// Revoke by APPENDING, never by updating or deleting the grant.
		revoke, _ := domain.NewConsentRecord(
			id.New(id.PrefixConsent), u.ID(), policy, false, base.Add(time.Minute),
		)
		if err := consents.Append(ctx, revoke); err != nil {
			t.Fatalf("append revoke: %v", err)
		}

		state, err = consents.LatestFor(ctx, u.ID(), policy)
		if err != nil {
			t.Fatalf("LatestFor: %v", err)
		}
		if state.AllowsScanning() {
			t.Error("after revoking, scanning must not be allowed")
		}
		if !state.WasRevoked() {
			t.Error("state should report an explicit revocation, not silence")
		}

		// The original grant is still there. This is the whole reason the
		// table is append-only: "was this user consented when that scan ran?"
		// has to remain answerable.
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM consent_records WHERE user_id = $1`, u.ID(),
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Errorf("history has %d records, want 2 - revoking must not erase the grant", n)
		}
	})
}

func TestConsentToOneVersionSaysNothingAboutAnother(t *testing.T) {
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		users := NewUserRepository(tx)
		consents := NewConsentRepository(tx)

		u := newTestUser(t)
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save user: %v", err)
		}

		old, _ := domain.NewConsentRecord(
			id.New(id.PrefixConsent), u.ID(), "2025-01-01", true, time.Now().UTC(),
		)
		if err := consents.Append(ctx, old); err != nil {
			t.Fatalf("Append: %v", err)
		}

		// Asking about the current policy must find nothing, so the user is
		// re-prompted rather than silently held to wording they never saw.
		state, err := consents.LatestFor(ctx, u.ID(), "2026-08-01")
		if err != nil {
			t.Fatalf("LatestFor: %v", err)
		}
		if state.HasResponded() {
			t.Error("consent to an older policy version must not answer for a newer one")
		}
		if state.AllowsScanning() {
			t.Error("stale consent must not authorise scanning")
		}
	})
}

func TestNeverAskedIsNotAnError(t *testing.T) {
	// Absence is a valid state, not a failure. Returning an error here would
	// force every caller to distinguish "no record" from "database down".
	withTx(t, func(ctx context.Context, tx pgx.Tx) {
		state, err := NewConsentRepository(tx).LatestFor(ctx, "usr_nobody", "2026-08-01")
		if err != nil {
			t.Fatalf("LatestFor returned an error for a user with no records: %v", err)
		}
		if state.HasResponded() {
			t.Error("expected an empty state")
		}
	})
}
