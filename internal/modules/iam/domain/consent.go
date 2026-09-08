package domain

import (
	"errors"
	"strings"
	"time"
)

var ErrMissingPolicyVersion = errors.New("policy version is required")

// ConsentRecord is one immutable statement about photo processing (FR-1).
//
// Immutable is the point. Revoking consent does not modify or delete this
// record — it appends a new one with Granted false. An audit trail that can be
// edited is not an audit trail, and the question that will eventually be asked
// is "was this user consented at the moment that scan ran?", which only an
// append-only history can answer.
//
// There are no mutating methods on this type on purpose.
type ConsentRecord struct {
	ID            string
	UserID        string
	PolicyVersion string
	Granted       bool
	RecordedAt    time.Time
}

// NewConsentRecord builds a record. `granted` false is a revocation, and is
// just as valid a record as a grant.
func NewConsentRecord(id, userID, policyVersion string, granted bool, now time.Time) (ConsentRecord, error) {
	if strings.TrimSpace(policyVersion) == "" {
		return ConsentRecord{}, ErrMissingPolicyVersion
	}
	return ConsentRecord{
		ID:            id,
		UserID:        userID,
		PolicyVersion: policyVersion,
		Granted:       granted,
		RecordedAt:    now,
	}, nil
}

// ConsentState answers the only question the scan flow actually asks: may this
// user submit a photo right now, under the policy version currently in force?
type ConsentState struct {
	// Latest is the most recent record for the policy version being checked,
	// or nil when the user has never responded to that version.
	Latest *ConsentRecord
}

// AllowsScanning is the gate on every scan submission.
//
// Note the three-way distinction, which a bare boolean would lose:
//
//	no record at all  -> never asked under this version; ask now
//	record, granted   -> proceed
//	record, revoked   -> do not re-prompt on this screen; they said no
//
// Collapsing "never asked" and "said no" into one false would produce an app
// that nags someone who has already declined.
func (s ConsentState) AllowsScanning() bool {
	return s.Latest != nil && s.Latest.Granted
}

// HasResponded distinguishes "never asked" from "asked and declined".
func (s ConsentState) HasResponded() bool { return s.Latest != nil }

// WasRevoked reports an explicit decline, as opposed to silence.
func (s ConsentState) WasRevoked() bool {
	return s.Latest != nil && !s.Latest.Granted
}
