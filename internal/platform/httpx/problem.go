// Package httpx holds HTTP helpers shared by every module's transport layer.
//
// It deliberately knows nothing about any domain: modules map their own errors
// onto these shapes, rather than this package growing a switch over every
// error in the system.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ayna/ayna-backend/internal/platform/logger"
)

// Problem is RFC 9457 Problem Details, and it is the ONLY error shape this API
// returns.
//
// One shape everywhere is the point: a client that has to branch on three
// different error bodies will get one of them wrong, and the one it gets wrong
// will be the rare path nobody tested.
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	CorrelationID string `json:"correlationId,omitempty"`

	// ResetsAt carries the moment a scan allowance renews, on 402 only. The
	// contract specifies it so the paywall can say when the next free scan
	// arrives instead of only offering to sell something.
	ResetsAt string `json:"resetsAt,omitempty"`
}

const problemBase = "https://api.ayna.app/problems/"

// WriteProblem sends a Problem response.
//
// `detail` is shown to a user, so it must say what went wrong and what to do
// about it — never an apology, never a driver message. Internal causes are
// logged, not returned: an error body is the last place to leak a schema.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, slug, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(Problem{
		Type:          problemBase + slug,
		Title:         title,
		Status:        status,
		Detail:        detail,
		CorrelationID: logger.Correlation(r.Context()),
	})
}

// WriteInternal logs the real cause and returns a deliberately vague body.
//
// The correlation id is the bridge: the user quotes it, and it leads straight
// to the log line carrying the actual error. That is how you get useful
// support without putting internals in a response.
func WriteInternal(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	logger.FromContext(r.Context(), log).Error("unhandled error", "error", err)

	WriteProblem(w, r, http.StatusInternalServerError,
		"internal", "Something went wrong",
		"Something went wrong on our side. Try again in a moment.")
}

// WriteJSON sends a success response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// DecodeJSON reads a JSON body with two guards that matter.
//
// The size limit stops an unbounded body exhausting memory. DisallowUnknownFields
// makes a client typo an error instead of a silent default — a request sending
// `birthyear` when the contract says `birthYear` should be told so, not quietly
// treated as year zero and refused by the age gate for a reason nobody can see.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// WriteBadRequest is the standard response to a malformed or invalid body.
func WriteBadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusBadRequest,
		"invalid-request", "That request could not be read", detail)
}
