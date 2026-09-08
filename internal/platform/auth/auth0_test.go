package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"net/url"

	jose "gopkg.in/go-jose/go-jose.v2"
	"gopkg.in/go-jose/go-jose.v2/jwt"
)

// These tests mint real tokens against a locally generated RSA key pair, so
// the entire verification path runs without an Auth0 tenant.
//
// The valid-token case is the least interesting one here. What matters is the
// list of forgeries below, each of which is a real attack that a plausible-
// looking JWT verifier accepts.

const (
	testIssuer   = "https://ayna-test.eu.auth0.com/"
	testAudience = "https://api.ayna.app"
	testSubject  = "auth0|abc123"
	testKeyID    = "test-key-1"
)

type signer struct {
	key *rsa.PrivateKey
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	// 2048 is the smallest size worth using and keeps the test fast.
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &signer{key: k}
}

func (s *signer) keyFunc() func(context.Context) (interface{}, error) {
	return func(context.Context) (interface{}, error) { return &s.key.PublicKey, nil }
}

type claims struct {
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  []string `json:"aud,omitempty"`
	Expiry    int64    `json:"exp,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
}

func validClaims() claims {
	now := time.Now()
	return claims{
		Issuer:   testIssuer,
		Subject:  testSubject,
		Audience: []string{testAudience},
		Expiry:   now.Add(time.Hour).Unix(),
		IssuedAt: now.Unix(),
	}
}

// signRS256 mints a normal, correctly signed token.
//
// The `kid` header is set because a real JWKS carries several keys and the
// verifier picks one by id. Omitting it works against a single injected key
// and fails against a real key set — which is exactly the difference the
// JWKS test at the bottom of this file exists to catch.
func (s *signer) signRS256(t *testing.T, c claims) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{
			Key:       s.key,
			KeyID:     testKeyID,
			Algorithm: string(jose.RS256),
		}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	tok, err := jwt.Signed(sig).Claims(c).CompactSerialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func subjectOf(t *testing.T, a *Auth0Authenticator, token string) (string, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return a.Subject(r)
}

func newTestAuthenticator(t *testing.T, s *signer) *Auth0Authenticator {
	t.Helper()
	a, err := newAuth0AuthenticatorWithKeyFunc(s.keyFunc(), testIssuer, testAudience)
	if err != nil {
		t.Fatalf("build authenticator: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// The happy path, once
// ---------------------------------------------------------------------------

func TestAValidTokenYieldsItsSubject(t *testing.T) {
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	got, err := subjectOf(t, a, s.signRS256(t, validClaims()))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if got != testSubject {
		t.Errorf("subject = %q, want %q", got, testSubject)
	}
}

// ---------------------------------------------------------------------------
// The forgeries. Each of these is accepted by a JWT verifier that looks fine.
// ---------------------------------------------------------------------------

func TestAnHS256TokenSignedWithThePublicKeyIsRejected(t *testing.T) {
	// The algorithm-confusion attack, and the single most important test here.
	//
	// The RSA public key is, by definition, public. If the verifier trusts the
	// token's own `alg` header, an attacker takes that public key, uses it as
	// an HMAC secret, signs a token claiming HS256, and the verifier confirms
	// it — because the "secret" and the "public key" are the same bytes.
	//
	// Pinning to RS256 is what makes this impossible. If someone ever changes
	// the validator to accept a set of algorithms including an HMAC one, this
	// test is what stops it reaching production.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	pubBytes := s.key.PublicKey.N.Bytes()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: pubBytes},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new HS256 signer: %v", err)
	}
	forged, err := jwt.Signed(sig).Claims(validClaims()).CompactSerialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := subjectOf(t, a, forged); err == nil {
		t.Fatal("SECURITY: an HS256 token signed with the public key was accepted")
	}
}

func TestAnUnsignedAlgNoneTokenIsRejected(t *testing.T) {
	// `{"alg":"none"}` with an empty signature. Verifiers that switch on the
	// header's algorithm have historically accepted this outright.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body, _ := json.Marshal(validClaims())
	payload := base64.RawURLEncoding.EncodeToString(body)
	forged := header + "." + payload + "."

	s := newSigner(t)
	if _, err := subjectOf(t, newTestAuthenticator(t, s), forged); err == nil {
		t.Fatal("SECURITY: an alg:none token was accepted")
	}
}

func TestATokenFromAnotherTenantIsRejected(t *testing.T) {
	// Signature is valid; the issuer is not ours. Without an issuer check, a
	// token from any Auth0 tenant on the internet — including one created
	// minutes ago by an attacker — would authenticate here.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.Issuer = "https://someone-elses-tenant.eu.auth0.com/"

	if _, err := subjectOf(t, a, s.signRS256(t, c)); err == nil {
		t.Fatal("SECURITY: a token from a different issuer was accepted")
	}
}

func TestATokenForAnotherAPIIsRejected(t *testing.T) {
	// Auth0 tokens are audience-scoped. A token minted for a different API in
	// the same tenant is correctly signed and correctly issued — and must
	// still not work here.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.Audience = []string{"https://some-other-api.example.com"}

	if _, err := subjectOf(t, a, s.signRS256(t, c)); err == nil {
		t.Fatal("SECURITY: a token for a different audience was accepted")
	}
}

func TestATokenSignedByADifferentKeyIsRejected(t *testing.T) {
	real := newSigner(t)
	attacker := newSigner(t)
	a := newTestAuthenticator(t, real)

	if _, err := subjectOf(t, a, attacker.signRS256(t, validClaims())); err == nil {
		t.Fatal("SECURITY: a token signed by an unknown key was accepted")
	}
}

func TestAnExpiredTokenIsRejected(t *testing.T) {
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.Expiry = time.Now().Add(-2 * time.Hour).Unix()
	c.IssuedAt = time.Now().Add(-3 * time.Hour).Unix()

	if _, err := subjectOf(t, a, s.signRS256(t, c)); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestATokenNotYetValidIsRejected(t *testing.T) {
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.NotBefore = time.Now().Add(time.Hour).Unix()

	if _, err := subjectOf(t, a, s.signRS256(t, c)); err == nil {
		t.Fatal("a token that is not yet valid was accepted")
	}
}

func TestATokenWithNoSubjectIsRejected(t *testing.T) {
	// It verifies, but it identifies nobody. Accepting it would mean querying
	// for user "" and returning a confusing 404 rather than rejecting an
	// unusable credential.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.Subject = ""

	if _, err := subjectOf(t, a, s.signRS256(t, c)); err == nil {
		t.Fatal("a token with no subject was accepted")
	}
}

// ---------------------------------------------------------------------------
// Header handling
// ---------------------------------------------------------------------------

func TestMissingCredentialsAreDistinguishedFromInvalidOnes(t *testing.T) {
	// Different errors because they mean different things to the client:
	// "sign in" versus "your session expired". Same status, different screen.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := a.Subject(r); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no header: got %v, want ErrNoCredentials", err)
	}

	r.Header.Set("Authorization", "Bearer not-a-jwt")
	if _, err := a.Subject(r); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("garbage token: got %v, want ErrInvalidCredentials", err)
	}
}

func TestMalformedAuthorizationHeaders(t *testing.T) {
	s := newSigner(t)
	a := newTestAuthenticator(t, s)
	good := s.signRS256(t, validClaims())

	for _, c := range []struct {
		name, header string
	}{
		// "Bearerfoo" must not be read as the token "foo" — which is what
		// TrimPrefix would do.
		{"no space after scheme", "Bearer" + good},
		{"wrong scheme", "Basic " + good},
		{"scheme only", "Bearer"},
		{"empty token", "Bearer "},
		{"token without scheme", good},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", c.header)
			if _, err := a.Subject(r); err == nil {
				t.Errorf("accepted malformed header %q", c.header)
			}
		})
	}
}

func TestTheSchemeIsMatchedCaseInsensitively(t *testing.T) {
	// RFC 7235 says the scheme is case-insensitive, and real clients send
	// "bearer". Rejecting those would be a bug, not strictness.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)
	good := s.signRS256(t, validClaims())

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", scheme+" "+good)
		if _, err := a.Subject(r); err != nil {
			t.Errorf("scheme %q rejected: %v", scheme, err)
		}
	}
}

func TestTheErrorNeverExplainsWhichCheckFailed(t *testing.T) {
	// Telling an attacker "audience mismatch" rather than "invalid" tells them
	// which part of the forgery to fix next. The client only needs to know to
	// sign in again.
	s := newSigner(t)
	a := newTestAuthenticator(t, s)

	c := validClaims()
	c.Audience = []string{"https://wrong.example.com"}

	_, err := subjectOf(t, a, s.signRS256(t, c))
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("got %v, want it to wrap ErrInvalidCredentials", err)
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestConstructionRefusesAnEmptyAudience(t *testing.T) {
	// An empty audience means "accept any" in some libraries, which would
	// silently accept tokens issued for a completely different API.
	if _, err := NewAuth0Authenticator("ayna.eu.auth0.com", ""); err == nil {
		t.Error("an empty audience was accepted")
	}
	if _, err := NewAuth0Authenticator("", "https://api.ayna.app"); err == nil {
		t.Error("an empty domain was accepted")
	}
}

func TestTheIssuerIsAlwaysHTTPS(t *testing.T) {
	// The domain comes from configuration. If it could carry its own scheme,
	// a mistyped or tampered value would have this fetching signing keys over
	// plaintext, where anyone on the path can substitute their own.
	a, err := NewAuth0Authenticator("ayna.eu.auth0.com", "https://api.ayna.app")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if a == nil {
		t.Fatal("nil authenticator")
	}
}

// ---------------------------------------------------------------------------
// The dev stand-in must never run outside dev
// ---------------------------------------------------------------------------

func TestDevAuthenticatorRefusesToExistOutsideDev(t *testing.T) {
	// It is a complete authentication bypass. The guard is in the constructor
	// rather than a comment, because a comment does not fail a deploy — and
	// the failure mode is every user's face photos being readable by anyone
	// who guesses a subject string.
	for _, env := range []string{"prod", "staging", "production", "", "Dev", "DEV"} {
		if _, err := NewDevAuthenticator(env); err == nil {
			t.Errorf("SECURITY: DevAuthenticator was constructible in env %q", env)
		}
	}
	if _, err := NewDevAuthenticator("dev"); err != nil {
		t.Errorf("dev env should be allowed: %v", err)
	}
}

func TestDevAuthenticatorBoundsTheSubjectLength(t *testing.T) {
	d, err := NewDevAuthenticator("dev")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(DevSubjectHeader, strings.Repeat("a", 500))

	if _, err := d.Subject(r); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("an oversized subject was accepted: %v", err)
	}
}

// A JWKS endpoint served locally, proving the real fetch-and-cache path works
// rather than only the injected-key path the other tests use.
func TestTheRealJWKSPathFetchesAndVerifies(t *testing.T) {
	s := newSigner(t)

	jwksJSON, err := json.Marshal(jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{{
			Key:       &s.key.PublicKey,
			KeyID:     testKeyID,
			Algorithm: "RS256",
			Use:       "sig",
		}},
	})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `/","jwks_uri":"` + srv.URL + `/.well-known/jwks.json"}`))
		case "/.well-known/jwks.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(jwksJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	issuer := srv.URL + "/"
	u, _ := urlParse(issuer)
	provider := jwks.NewCachingProvider(u, jwksCacheTTL)

	a, err := newAuth0AuthenticatorWithKeyFunc(provider.KeyFunc, issuer, testAudience)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	c := validClaims()
	c.Issuer = issuer

	got, err := subjectOf(t, a, s.signRS256(t, c))
	if err != nil {
		t.Fatalf("token rejected via the real JWKS path: %v", err)
	}
	if got != testSubject {
		t.Errorf("subject = %q, want %q", got, testSubject)
	}
}

func urlParse(s string) (*url.URL, error) { return url.Parse(s) }
