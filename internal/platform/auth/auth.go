// Package auth resolves the caller's identity from a request.
//
// The identity provider is Auth0 (ADR-004), but nothing here mentions it: the
// Authenticator interface is a port, and the Auth0 JWT verifier will be one
// implementation of it. That is what lets the handlers be written and tested
// before an Auth0 tenant exists.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ayna/ayna-backend/internal/platform/httpx"
)

type ctxKey int

const subjectKey ctxKey = iota

var (
	// ErrNoCredentials means no token was presented at all.
	ErrNoCredentials = errors.New("no credentials presented")

	// ErrInvalidCredentials means one was presented and did not verify.
	// Kept distinct from ErrNoCredentials so the client can tell "log in" from
	// "your session expired" — different screens, different copy.
	ErrInvalidCredentials = errors.New("credentials are not valid")
)

// Authenticator turns a request into a stable subject identifier.
//
// The subject is Auth0's `sub` claim, which is what the users table joins on.
// Deliberately NOT the email: people change theirs, and a join key that
// changes is not a key.
type Authenticator interface {
	Subject(r *http.Request) (string, error)
}

// SubjectFrom reads the authenticated subject placed on the context by
// Middleware. The second return is false on an unauthenticated request.
func SubjectFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(subjectKey).(string)
	return s, ok && s != ""
}

// MustSubject is for handlers mounted behind Middleware, where an
// unauthenticated request is impossible by construction. It panics rather than
// returning an empty string, because a silent empty subject would query the
// database for user "" and return a confusing 404 instead of an obvious bug.
func MustSubject(ctx context.Context) string {
	s, ok := SubjectFrom(ctx)
	if !ok {
		panic("auth: no subject on context — handler mounted outside Middleware")
	}
	return s
}

// Middleware authenticates the request or rejects it.
//
// Rejection is 401 with the RFC 9457 body the rest of the API uses, not a bare
// http.Error, so a client never has to parse two error formats.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub, err := a.Subject(r)
			if err != nil {
				status, slug, title, detail := 0, "", "", ""
				switch {
				case errors.Is(err, ErrNoCredentials):
					status, slug = http.StatusUnauthorized, "not-authenticated"
					title, detail = "Sign in to continue", "This request needs a signed-in user."
				default:
					status, slug = http.StatusUnauthorized, "invalid-credentials"
					title, detail = "Your session has expired", "Sign in again to continue."
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="ayna"`)
				httpx.WriteProblem(w, r, status, slug, title, detail)
				return
			}

			ctx := context.WithValue(r.Context(), subjectKey, sub)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ---------------------------------------------------------------------------

// DevAuthenticator trusts an `X-Dev-Subject` header.
//
// This exists so the handlers, use cases and repositories can be built and
// tested end to end before an Auth0 tenant is configured. It is, obviously, a
// complete authentication bypass: anyone who can reach the service can be
// anyone.
//
// It therefore cannot be constructed outside a dev environment. That check is
// in the constructor rather than in a comment, because a comment does not fail
// a deploy — and the failure mode here is not a bug, it is every user's face
// photos being readable by anyone who guesses a subject string.
type DevAuthenticator struct{}

// NewDevAuthenticator refuses to exist anywhere but dev.
func NewDevAuthenticator(env string) (*DevAuthenticator, error) {
	if env != "dev" {
		return nil, fmt.Errorf(
			"auth: DevAuthenticator is an authentication bypass and cannot run in %q; "+
				"configure the Auth0 verifier for this environment", env)
	}
	return &DevAuthenticator{}, nil
}

const DevSubjectHeader = "X-Dev-Subject"

func (d *DevAuthenticator) Subject(r *http.Request) (string, error) {
	sub := strings.TrimSpace(r.Header.Get(DevSubjectHeader))
	if sub == "" {
		return "", ErrNoCredentials
	}
	// Bound the length so a hostile value cannot bloat a log line or a
	// database column. The real verifier will impose its own limits; this one
	// still should not be the weak point while it exists.
	if len(sub) > 128 {
		return "", ErrInvalidCredentials
	}
	return sub, nil
}
