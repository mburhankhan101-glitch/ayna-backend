package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"github.com/auth0/go-jwt-middleware/v2/validator"
)

// Auth0Authenticator verifies an Auth0-issued access token and returns its
// subject.
//
// # The four checks, and why each one is load-bearing
//
//  1. **Signature, against Auth0's published keys.** Fetched from the tenant's
//     JWKS endpoint and cached. Without this, anyone can mint any token.
//
//  2. **Algorithm pinned to RS256.** This is the check people omit, and it is
//     the one that matters most. A validator that accepts whatever `alg` the
//     token declares can be handed an HS256 token signed with the RSA *public*
//     key as the HMAC secret — the key is public, so the attacker has it, and
//     a naive verifier will happily confirm the signature. Pinning to a single
//     asymmetric algorithm closes that entirely. `alg: none` dies here too.
//
//  3. **Issuer.** The token must come from OUR tenant. Otherwise a token from
//     any Auth0 tenant on the internet — including one the attacker created
//     five minutes ago — verifies against a JWKS we fetched from them.
//
//  4. **Audience.** The token must have been issued FOR this API. Auth0 tokens
//     are audience-scoped, and skipping this lets a token minted for some
//     other API in the same tenant authenticate here.
//
// Expiry and not-before are checked by the validator with a small leeway for
// clock skew between Auth0 and this process.
type Auth0Authenticator struct {
	v *validator.Validator
}

// jwksCacheTTL bounds how long a rotated-out key stays trusted, and how often
// this process calls Auth0.
//
// Auth0 rotates signing keys rarely. Five minutes keeps a revoked key's window
// short without turning every cold start into a fetch storm — and Cloud Run
// cold-starts often enough that this matters.
const jwksCacheTTL = 5 * time.Minute

// clockSkewLeeway tolerates a small disagreement between Auth0's clock and
// this container's. Deliberately small: leeway is a window in which an expired
// token still works, so it buys reliability at the direct cost of revocation
// latency.
const clockSkewLeeway = 30 * time.Second

// NewAuth0Authenticator builds a verifier for one tenant and one API.
//
// domain is the tenant host with no scheme ("ayna.eu.auth0.com").
// audience is the API identifier configured in Auth0.
func NewAuth0Authenticator(domain, audience string) (*Auth0Authenticator, error) {
	if domain == "" {
		return nil, errors.New("auth0: domain is required")
	}
	if audience == "" {
		// Refused rather than defaulted. An empty audience in some libraries
		// means "accept any", which would silently accept tokens issued for a
		// different API entirely.
		return nil, errors.New("auth0: audience is required")
	}

	// Always https, and always built rather than accepted as a whole URL from
	// configuration: an issuer of "http://..." would have this fetching signing
	// keys over plaintext, where anyone on the path can replace them.
	issuer, err := url.Parse("https://" + strings.TrimSuffix(domain, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("auth0: bad domain %q: %w", domain, err)
	}

	provider := jwks.NewCachingProvider(issuer, jwksCacheTTL)

	v, err := validator.New(
		provider.KeyFunc,
		validator.RS256, // see check 2 above — never taken from the token
		issuer.String(),
		[]string{audience},
		validator.WithAllowedClockSkew(clockSkewLeeway),
	)
	if err != nil {
		return nil, fmt.Errorf("auth0: build validator: %w", err)
	}

	return &Auth0Authenticator{v: v}, nil
}

// newAuth0AuthenticatorWithKeyFunc is the seam the tests use: it takes a key
// source directly, so the whole verification path can be exercised against a
// locally generated key pair instead of a live tenant.
//
// Unexported, so no production code path can reach it and supply its own keys.
func newAuth0AuthenticatorWithKeyFunc(
	keyFunc func(context.Context) (interface{}, error),
	issuer, audience string,
) (*Auth0Authenticator, error) {
	v, err := validator.New(
		keyFunc, validator.RS256, issuer, []string{audience},
		validator.WithAllowedClockSkew(clockSkewLeeway),
	)
	if err != nil {
		return nil, err
	}
	return &Auth0Authenticator{v: v}, nil
}

func (a *Auth0Authenticator) Subject(r *http.Request) (string, error) {
	token, err := bearerToken(r)
	if err != nil {
		return "", err
	}

	claims, err := a.v.ValidateToken(r.Context(), token)
	if err != nil {
		// The specific reason is deliberately not returned to the caller.
		// "signature invalid" versus "audience mismatch" tells an attacker
		// which part of a forgery to fix next; the client only needs to know
		// to sign in again.
		return "", fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}

	validated, ok := claims.(*validator.ValidatedClaims)
	if !ok {
		return "", fmt.Errorf("%w: unexpected claims type %T", ErrInvalidCredentials, claims)
	}

	sub := validated.RegisteredClaims.Subject
	if sub == "" {
		// A token that verifies but carries no subject cannot identify anyone.
		// Treating it as valid would mean querying for user "" and returning a
		// confusing 404 instead of rejecting an unusable token.
		return "", fmt.Errorf("%w: token has no subject", ErrInvalidCredentials)
	}
	return sub, nil
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", ErrNoCredentials
	}

	// SplitN, not TrimPrefix: a header of "Bearerfoo" must not be read as the
	// token "foo".
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", fmt.Errorf("%w: expected a Bearer token", ErrInvalidCredentials)
	}

	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", ErrNoCredentials
	}
	return token, nil
}
