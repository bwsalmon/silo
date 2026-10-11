// Package frontdoortest signs front-door ID tokens with a local key,
// and builds a frontdoor.Google verifier that trusts that key in place
// of Google's certificates. The verifier is the real one: only where
// it fetches certificates from is replaced.
package frontdoortest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/frontdoor"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

// KeyID is the kid of a Signer's key in its certificate set.
const KeyID = "frontdoortest"

// Signer holds an RSA key it signs tokens with.
type Signer struct {
	t   testing.TB
	key *rsa.PrivateKey
}

// New returns a Signer with a fresh key.
func New(t testing.TB) *Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{t: t, key: key}
}

// Claims are a token's claims.
type Claims = jwt.MapClaims

// Valid is the claims of a token Google would mint for email, for
// audience, good for an hour.
func Valid(email, audience string) Claims {
	now := time.Now()
	return Claims{
		"iss": "https://accounts.google.com", "aud": audience, "sub": "1234567890",
		"email": email, "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// Sign signs claims as an RS256 ID token under KeyID.
func (s *Signer) Sign(c Claims) string {
	s.t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = KeyID
	out, err := tok.SignedString(s.key)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

// Token is Sign(Valid(email, audience)).
func (s *Signer) Token(email, audience string) string { return s.Sign(Valid(email, audience)) }

// Verifier is a frontdoor.Google for audience whose certificate fetches
// all answer with s's key.
func (s *Signer) Verifier(audience string) *frontdoor.Google {
	s.t.Helper()
	g, err := frontdoor.NewGoogle(context.Background(), audience,
		idtoken.ClientOption(option.WithHTTPClient(&http.Client{Transport: certs{s.key}})))
	if err != nil {
		s.t.Fatal(err)
	}
	return g
}

// certs answers every request with the key's public half as a JWK set,
// as Google's certificate endpoint would.
type certs struct{ key *rsa.PrivateKey }

func (c certs) RoundTrip(*http.Request) (*http.Response, error) {
	b64 := base64.RawURLEncoding.EncodeToString
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": KeyID,
		"n": b64(c.key.N.Bytes()), "e": b64(big.NewInt(int64(c.key.E)).Bytes()),
	}}})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"max-age=3600"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}, nil
}
