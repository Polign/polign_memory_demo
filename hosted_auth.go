package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// identity is who a verified ID token says is using the hosted demo. The
// subject is stable for the life of the account and is what the namespace is
// derived from; the email is only shown back to the person.
type identity struct {
	Subject string
	Email   string
}

var errUnauthorized = errors.New("unauthorized")

// tokenVerifier checks OpenID Connect ID tokens signed with RS256 by one
// issuer, which is what Amazon Cognito hands the polign.com sign-in. Only the
// standard library is involved: the token is split, its header names a key id,
// the issuer's JWKS document supplies that RSA public key, and the signature
// and claims are checked here. Keys are cached for an hour and refetched at
// most once a minute when an unknown key id shows up, so a rotation is picked
// up without letting a flood of bad tokens turn into a flood of fetches.
type tokenVerifier struct {
	issuer   string
	audience string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	skew     time.Duration

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newTokenVerifier(issuer, audience string, client *http.Client) (*tokenVerifier, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if !strings.HasPrefix(issuer, "https://") || strings.ContainsAny(issuer, "?#") {
		return nil, fmt.Errorf("token issuer must be a plain https URL, got %q", issuer)
	}
	audience = strings.TrimSpace(audience)
	if audience == "" {
		return nil, fmt.Errorf("token audience (the Cognito app client id) is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &tokenVerifier{issuer: issuer, audience: audience, jwksURL: issuer + "/.well-known/jwks.json", client: client, now: time.Now, skew: 30 * time.Second}, nil
}

// Verify returns the identity a token asserts, or errUnauthorized (wrapped
// with a reason that is safe to log but not worth showing a caller).
func (v *tokenVerifier) Verify(ctx context.Context, token string) (identity, error) {
	if len(token) > 8192 {
		return identity{}, fmt.Errorf("%w: token too long", errUnauthorized)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return identity{}, fmt.Errorf("%w: malformed token", errUnauthorized)
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return identity{}, fmt.Errorf("%w: malformed header", errUnauthorized)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Alg != "RS256" || header.Kid == "" {
		return identity{}, fmt.Errorf("%w: unsupported token header", errUnauthorized)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity{}, fmt.Errorf("%w: malformed payload", errUnauthorized)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, fmt.Errorf("%w: malformed signature", errUnauthorized)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return identity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return identity{}, fmt.Errorf("%w: bad signature", errUnauthorized)
	}

	var claims struct {
		Issuer   string          `json:"iss"`
		Audience json.RawMessage `json:"aud"`
		Subject  string          `json:"sub"`
		Email    string          `json:"email"`
		TokenUse string          `json:"token_use"`
		Expires  int64           `json:"exp"`
		IssuedAt int64           `json:"iat"`
	}
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return identity{}, fmt.Errorf("%w: malformed claims", errUnauthorized)
	}
	now := v.now()
	switch {
	case claims.Issuer != v.issuer:
		return identity{}, fmt.Errorf("%w: wrong issuer", errUnauthorized)
	case !audienceContains(claims.Audience, v.audience):
		return identity{}, fmt.Errorf("%w: wrong audience", errUnauthorized)
	case claims.TokenUse != "id":
		return identity{}, fmt.Errorf("%w: not an ID token", errUnauthorized)
	case claims.Expires == 0 || now.After(time.Unix(claims.Expires, 0).Add(v.skew)):
		return identity{}, fmt.Errorf("%w: token expired", errUnauthorized)
	case claims.IssuedAt == 0 || time.Unix(claims.IssuedAt, 0).After(now.Add(v.skew)):
		return identity{}, fmt.Errorf("%w: token issued in the future", errUnauthorized)
	case strings.TrimSpace(claims.Subject) == "":
		return identity{}, fmt.Errorf("%w: no subject", errUnauthorized)
	}
	return identity{Subject: claims.Subject, Email: strings.ToLower(strings.TrimSpace(claims.Email))}, nil
}

// audienceContains accepts the claim as either a string or an array of
// strings, both of which the specification allows.
func audienceContains(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, aud := range many {
			if aud == want {
				return true
			}
		}
	}
	return false
}

func (v *tokenVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.fetchedAt)
	if key := v.keys[kid]; key != nil && age < time.Hour {
		return key, nil
	}
	if v.keys != nil && age < time.Minute {
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
		return nil, fmt.Errorf("%w: unknown signing key", errUnauthorized)
	}
	keys, err := v.fetch(ctx)
	if err != nil {
		// Keep serving the cached set through a transient fetch failure.
		if key := v.keys[kid]; key != nil {
			return key, nil
		}
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	v.keys, v.fetchedAt = keys, v.now()
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key", errUnauthorized)
}

func (v *tokenVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", v.jwksURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", v.jwksURL, err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		exponent := new(big.Int).SetBytes(e)
		if !exponent.IsInt64() || exponent.Int64() < 3 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no usable RSA keys", v.jwksURL)
	}
	return keys, nil
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}
