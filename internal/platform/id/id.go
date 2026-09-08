// Package id generates prefixed, sortable identifiers.
//
// The format is a ULID with a type prefix: `usr_01J8XK4M2P9QRSTVWXYZABCDEF`.
// Three properties earn it over a UUID:
//
//   - **Sortable.** The leading 48 bits are a millisecond timestamp, so IDs
//     sort chronologically as plain strings. `ORDER BY id` is `ORDER BY
//     created_at` for free, and a B-tree index on it appends rather than
//     fragmenting the way random UUIDv4 primary keys do.
//   - **Prefixed.** `usr_` versus `scn_` makes a mis-wired join obvious in a
//     log line instead of being a silent lookup miss.
//   - **Crockford base32.** No I, L, O or U, so no digit/letter confusion when
//     an id is read aloud from a support ticket, and no accidental words.
//
// Implemented here rather than pulled in as a dependency because the whole
// thing is forty lines of standard library, and because it must stay callable
// from anywhere without dragging a module into a package that should not have
// one.
package id

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// Crockford base32. Exactly 32 symbols, excluding I, L, O and U.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Prefixes. Every entity that gets an id gets one here rather than inline at
// the call site, so the set is enumerable and collisions are visible.
const (
	PrefixUser    = "usr"
	PrefixScan    = "scn"
	PrefixReport  = "rep"
	PrefixConsent = "con"
	PrefixEvent   = "evt"
)

// New returns a prefixed ULID, e.g. New(PrefixUser) -> "usr_01J8XK...".
//
// It panics if the system entropy source fails. That is deliberate: a process
// that cannot generate unpredictable identifiers must not continue serving —
// degrading to a weaker source would be worse than stopping, because nothing
// downstream would ever notice.
func New(prefix string) string {
	return prefix + "_" + newULID(time.Now())
}

func newULID(t time.Time) string {
	var sb strings.Builder
	sb.Grow(26)

	// 48-bit millisecond timestamp across the first 10 characters. Ten
	// characters hold 50 bits, so the top two are always zero until the year
	// 10889 — which is a comfortable margin.
	ms := uint64(t.UnixMilli())
	for i := 0; i < 10; i++ {
		shift := uint(45 - 5*i)
		sb.WriteByte(alphabet[(ms>>shift)&0x1F])
	}

	// 80 bits of entropy across the remaining 16 characters. 10 bytes is
	// exactly 16 base32 symbols, so there is no padding and no partial symbol.
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Sprintf("id: system entropy unavailable: %v", err))
	}

	var buf uint32
	var bits uint
	for _, b := range entropy {
		buf = buf<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(alphabet[(buf>>bits)&0x1F])
		}
	}

	return sb.String()
}

// Valid reports whether s is a well-formed id with the given prefix.
//
// Used at trust boundaries — a path parameter, an event payload — so a
// malformed id is rejected before it reaches a query rather than after.
func Valid(prefix, s string) bool {
	want := prefix + "_"
	if !strings.HasPrefix(s, want) {
		return false
	}
	body := s[len(want):]
	if len(body) != 26 {
		return false
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(alphabet, body[i]) < 0 {
			return false
		}
	}
	return true
}

// TimeOf recovers the generation time from an id.
//
// Handy in an incident: a single id in a log line tells you when the thing was
// created without a database round trip.
func TimeOf(s string) (time.Time, bool) {
	i := strings.IndexByte(s, '_')
	if i < 0 || len(s)-i-1 != 26 {
		return time.Time{}, false
	}
	body := s[i+1:]

	var ms uint64
	for j := 0; j < 10; j++ {
		v := strings.IndexByte(alphabet, body[j])
		if v < 0 {
			return time.Time{}, false
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}
