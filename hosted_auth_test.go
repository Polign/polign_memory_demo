package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type jwksFixture struct {
	key      *rsa.PrivateKey
	kid      string
	fetches  int
	verifier *tokenVerifier
	now      time.Time
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &jwksFixture{key: key, kid: "k1", now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		f.fetches++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	v, err := newTokenVerifier(srv.URL, "client-123", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return f.now }
	f.verifier = v
	return f
}

func (f *jwksFixture) token(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	base := map[string]any{"iss": f.verifier.issuer, "aud": "client-123", "sub": "user-1", "email": "Alice@Example.com", "token_use": "id", "exp": f.now.Add(time.Hour).Unix(), "iat": f.now.Unix()}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(base)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyAcceptsAValidIDToken(t *testing.T) {
	f := newJWKSFixture(t)
	who, err := f.verifier.Verify(context.Background(), f.token(t, "k1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if who.Subject != "user-1" || who.Email != "alice@example.com" {
		t.Fatalf("identity = %+v", who)
	}
	// Audience may also arrive as a list.
	if _, err := f.verifier.Verify(context.Background(), f.token(t, "k1", map[string]any{"aud": []string{"other", "client-123"}})); err != nil {
		t.Fatal(err)
	}
	if f.fetches != 1 {
		t.Fatalf("jwks fetched %d times, want 1 (cached)", f.fetches)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	f := newJWKSFixture(t)
	cases := map[string]string{
		"wrong audience":   f.token(t, "k1", map[string]any{"aud": "someone-else"}),
		"wrong issuer":     f.token(t, "k1", map[string]any{"iss": "https://evil.example"}),
		"access token":     f.token(t, "k1", map[string]any{"token_use": "access"}),
		"expired":          f.token(t, "k1", map[string]any{"exp": f.now.Add(-time.Minute).Unix()}),
		"future":           f.token(t, "k1", map[string]any{"iat": f.now.Add(time.Hour).Unix()}),
		"no subject":       f.token(t, "k1", map[string]any{"sub": nil}),
		"unknown key":      f.token(t, "k9", nil),
		"tampered payload": tamper(f.token(t, "k1", nil)),
		"garbage":          "not.a.token",
	}
	for name, token := range cases {
		_, err := f.verifier.Verify(context.Background(), token)
		if !errors.Is(err, errUnauthorized) {
			t.Errorf("%s: err = %v, want unauthorized", name, err)
		}
	}
	// A token signed with a different key but a known kid must fail on signature.
	other := newJWKSFixture(t)
	other.verifier = f.verifier
	if _, err := f.verifier.Verify(context.Background(), other.token(t, "k1", nil)); !errors.Is(err, errUnauthorized) {
		t.Fatalf("foreign signature: err = %v", err)
	}
}

func tamper(token string) string {
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), "user-1", "user-2", 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	return strings.Join(parts, ".")
}

func TestVerifyRefetchesKeysOnRotation(t *testing.T) {
	f := newJWKSFixture(t)
	if _, err := f.verifier.Verify(context.Background(), f.token(t, "k1", nil)); err != nil {
		t.Fatal(err)
	}
	f.kid = "k2"
	// Within a minute of the last fetch an unknown kid does not refetch.
	if _, err := f.verifier.Verify(context.Background(), f.token(t, "k2", nil)); !errors.Is(err, errUnauthorized) || f.fetches != 1 {
		t.Fatalf("early rotation: err = %v fetches = %d", err, f.fetches)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.verifier.Verify(context.Background(), f.token(t, "k2", nil)); err != nil || f.fetches != 2 {
		t.Fatalf("rotation: err = %v fetches = %d", err, f.fetches)
	}
}

func TestNewTokenVerifierRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct{ issuer, audience string }{{"http://plain", "aud"}, {"https://ok", ""}, {"https://ok?x=1", "aud"}} {
		if _, err := newTokenVerifier(tc.issuer, tc.audience, nil); err == nil {
			t.Errorf("issuer %q audience %q accepted", tc.issuer, tc.audience)
		}
	}
}
