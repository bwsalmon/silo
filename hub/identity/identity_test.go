package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var b64 = base64.RawURLEncoding

// signer is a fake identity provider's signing key, published as a
// JWKS by its server.
type signer struct {
	kid string
	ec  *ecdsa.PrivateKey
	rsa *rsa.PrivateKey
}

func newSigners(t *testing.T) (ec, rs *signer) {
	t.Helper()
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: "ec-1", ec: ek}, &signer{kid: "rsa-1", rsa: rk}
}

func (s *signer) jwk() map[string]string {
	if s.ec != nil {
		return map[string]string{"kid": s.kid, "kty": "EC", "crv": "P-256",
			"x": b64.EncodeToString(s.ec.X.FillBytes(make([]byte, 32))),
			"y": b64.EncodeToString(s.ec.Y.FillBytes(make([]byte, 32)))}
	}
	return map[string]string{"kid": s.kid, "kty": "RSA",
		"n": b64.EncodeToString(s.rsa.N.Bytes()),
		"e": b64.EncodeToString(big.NewInt(int64(s.rsa.E)).Bytes())}
}

func (s *signer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	alg := "RS256"
	if s.ec != nil {
		alg = "ES256"
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": s.kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	d := sha256.Sum256([]byte(in))
	var sig []byte
	if s.ec != nil {
		r, ss, err := ecdsa.Sign(rand.Reader, s.ec, d[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	} else {
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, d[:]); err != nil {
			t.Fatal(err)
		}
	}
	return in + "." + b64.EncodeToString(sig)
}

func jwks(t *testing.T, signers ...*signer) *httptest.Server {
	var keys []map[string]string
	for _, s := range signers {
		keys = append(keys, s.jwk())
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(ts.Close)
	return ts
}

const aud = "/projects/123/global/backendServices/456"

func iapClaims(mod func(map[string]any)) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss": IAPIssuer, "aud": aud, "sub": "accounts.google.com:1",
		"email": "Alice@Example.com", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func iapRequest(tok string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if tok != "" {
		r.Header.Set(IAPHeader, tok)
	}
	return r
}

func TestIAP(t *testing.T) {
	ec, rs := newSigners(t)
	other, _ := newSigners(t)
	keys := jwks(t, ec, rs)
	a := &IAP{Audience: aud, Keys: &KeySet{URL: keys.URL}}

	for _, s := range []*signer{ec, rs} {
		p, err := a.Principal(iapRequest(s.sign(t, iapClaims(nil))))
		if err != nil {
			t.Fatalf("%s: %v", s.kid, err)
		}
		if p.Email != "alice@example.com" {
			t.Errorf("email %q, want lower-cased", p.Email)
		}
	}

	none := b64.EncodeToString([]byte(`{"alg":"none","kid":"ec-1"}`)) + "." +
		b64.EncodeToString(must(json.Marshal(iapClaims(nil)))) + "."
	forged := iapRequest("")
	forged.Header.Set("x-goog-authenticated-user-email", "accounts.google.com:alice@example.com")
	for name, r := range map[string]*http.Request{
		"no assertion":         iapRequest(""),
		"unsigned email alone": forged,
		"wrong audience":       iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { c["aud"] = "/projects/123/global/backendServices/999" }))),
		"wrong issuer":         iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { c["iss"] = "https://accounts.google.com" }))),
		"expired":              iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }))),
		"no expiry":            iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { delete(c, "exp") }))),
		"future":               iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }))),
		"no email":             iapRequest(ec.sign(t, iapClaims(func(c map[string]any) { delete(c, "email") }))),
		"unknown key":          iapRequest((&signer{kid: "ec-9", ec: other.ec}).sign(t, iapClaims(nil))),
		"other key, same kid":  iapRequest((&signer{kid: "ec-1", ec: other.ec}).sign(t, iapClaims(nil))),
		"alg none":             iapRequest(none),
		"garbage":              iapRequest("a.b.c"),
	} {
		if p, err := a.Principal(r); err == nil {
			t.Errorf("%s: accepted as %+v", name, p)
		}
	}
	if _, err := (&IAP{Keys: &KeySet{URL: keys.URL}}).Principal(iapRequest(ec.sign(t, iapClaims(nil)))); err == nil {
		t.Error("an IAP with no audience configured accepted a token")
	}
}

func TestAlgorithmMustMatchKey(t *testing.T) {
	ec, rs := newSigners(t)
	keys := jwks(t, ec, rs)
	// An RS256 signature presented under the EC key's kid.
	tok := (&signer{kid: "ec-1", rsa: rs.rsa}).sign(t, iapClaims(nil))
	if _, err := (&IAP{Audience: aud, Keys: &KeySet{URL: keys.URL}}).Principal(iapRequest(tok)); err == nil {
		t.Error("an RS256 token verified against an EC key's kid")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestAudienceArray(t *testing.T) {
	var c Claims
	if err := json.Unmarshal([]byte(`{"aud":["a","b"],"email_verified":"true"}`), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Audience.has("b") || c.Audience.has("c") || c.EmailVerified == nil || !bool(*c.EmailVerified) {
		t.Errorf("claims %+v", c)
	}
}

func TestLocalReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/tasks?x=1": "/tasks?x=1", "": "/", "https://evil.example": "/", "//evil.example": "/", "/\\evil.example": "/",
		"/\t/x": "/", "/%09/x": "/", "/\n/x": "/", "/\\x": "/", "/%5Cx": "/", "/%2F/x": "/", "/x\x7f": "/",
		"/a/b#frag": "/a/b#frag", "/%20x": "/%20x",
	} {
		if got := localReturn(in); got != want {
			t.Errorf("localReturn(%q) = %q, want %q", in, got, want)
		}
	}
}
