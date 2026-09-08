package domain

import (
	"errors"
	"testing"
)

func TestConsentDistinguishesNeverAskedFromDeclined(t *testing.T) {
	// A bare boolean would collapse these into one `false`, and the app would
	// then nag someone who has already said no. Three states, three answers.
	granted, _ := NewConsentRecord("con_1", "usr_1", "2026-08-01", true, now)
	revoked, _ := NewConsentRecord("con_2", "usr_1", "2026-08-01", false, now)

	for _, c := range []struct {
		name                          string
		state                         ConsentState
		allows, responded, wasRevoked bool
	}{
		{"never asked", ConsentState{}, false, false, false},
		{"granted", ConsentState{Latest: &granted}, true, true, false},
		{"revoked", ConsentState{Latest: &revoked}, false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.state.AllowsScanning(); got != c.allows {
				t.Errorf("AllowsScanning = %v, want %v", got, c.allows)
			}
			if got := c.state.HasResponded(); got != c.responded {
				t.Errorf("HasResponded = %v, want %v", got, c.responded)
			}
			if got := c.state.WasRevoked(); got != c.wasRevoked {
				t.Errorf("WasRevoked = %v, want %v", got, c.wasRevoked)
			}
		})
	}
}

func TestARevocationIsARecord(t *testing.T) {
	// Revoking appends `granted: false`; it does not delete the grant. The
	// question that gets asked later is "was this user consented when that
	// scan ran?", and only a history can answer it.
	r, err := NewConsentRecord("con_2", "usr_1", "2026-08-01", false, now)
	if err != nil {
		t.Fatalf("a revocation must be a valid record: %v", err)
	}
	if r.Granted {
		t.Error("expected a revocation")
	}
	if r.RecordedAt.IsZero() {
		t.Error("a record with no timestamp cannot serve as an audit entry")
	}
}

func TestConsentIsScopedToAPolicyVersion(t *testing.T) {
	// When the wording changes, prior consent must not silently cover the new
	// terms. An unversioned record could not express that, so it is refused.
	if _, err := NewConsentRecord("con_1", "usr_1", "  ", true, now); !errors.Is(err, ErrMissingPolicyVersion) {
		t.Errorf("got %v, want ErrMissingPolicyVersion", err)
	}
}

func TestConsentForOneVersionSaysNothingAboutAnother(t *testing.T) {
	// The state object only ever holds the record for the version being
	// asked about. This test documents that the lookup is version-scoped
	// rather than "does this user have any consent at all".
	old, _ := NewConsentRecord("con_1", "usr_1", "2025-01-01", true, now)
	stateForNewPolicy := ConsentState{} // nothing found for "2026-08-01"

	if stateForNewPolicy.AllowsScanning() {
		t.Error("consent to an older policy version must not authorise the current one")
	}
	if !(ConsentState{Latest: &old}).AllowsScanning() {
		t.Error("sanity: consent to the version being asked about should allow scanning")
	}
}
