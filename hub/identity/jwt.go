package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
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

// leeway is the clock skew allowed on a token's times.
const leeway = time.Minute

// Claims are the claims of a verified JWT that the hub reads.
type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
	Nonce     string   `json:"nonce"`
	Email     string   `json:"email"`
	// EmailVerified is the OIDC claim; IAP's tokens leave it out.
	EmailVerified *flexBool `json:"email_verified"`
	Groups        []string  `json:"groups"`
}

// audience is "aud", a string or an array of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(want string) bool {
	for _, s := range a {
		if s == want {
			return true
		}
	}
	return false
}

// flexBool is a boolean claim some providers write as a string.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "true":
		*f = true
	case "false":
		*f = false
	default:
		return fmt.Errorf("not a boolean: %s", b)
	}
	return nil
}

// Expect is what Verify checks a token's claims against.
type Expect struct {
	Issuer   string
	Audience string
	// Nonce, when set, must be the token's nonce.
	Nonce string
	Now   time.Time
}

// KeySet is a JSON Web Key Set fetched from URL: IAP's keys, or an OIDC
// provider's jwks_uri. Keys are cached, and fetched again when a token
// names a key the cache does not hold (at most once a minute) or after
// an hour.
type KeySet struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

const (
	keysMaxAge  = time.Hour
	keysRefetch = time.Minute
)

// key returns the key named kid.
func (k *KeySet) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	age := time.Since(k.fetched)
	if key, ok := k.keys[kid]; ok && age < keysMaxAge {
		return key, nil
	}
	if k.keys == nil || age >= keysRefetch {
		keys, err := k.fetch(ctx)
		if err != nil {
			if key, ok := k.keys[kid]; ok {
				return key, nil // the provider is down; the cached key still verifies
			}
			return nil, err
		}
		k.keys, k.fetched = keys, time.Now()
	}
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("identity: no key %q in %s", kid, k.URL)
}

func (k *KeySet) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	c := k.Client
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: fetching keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: fetching keys from %s: %s", k.URL, resp.Status)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("identity: reading keys from %s: %w", k.URL, err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if key, err := j.public(); err == nil && j.Kid != "" {
			keys[j.Kid] = key
		}
	}
	return keys, nil
}

// jwk is one JSON Web Key; only EC P-256 and RSA keys are read.
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j jwk) public() (crypto.PublicKey, error) {
	num := func(s string) (*big.Int, error) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(b) == 0 {
			return nil, errors.New("bad key")
		}
		return new(big.Int).SetBytes(b), nil
	}
	switch j.Kty {
	case "EC":
		if j.Crv != "P-256" {
			return nil, errors.New("unsupported curve")
		}
		x, err := num(j.X)
		if err != nil {
			return nil, err
		}
		y, err := num(j.Y)
		if err != nil {
			return nil, err
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !key.Curve.IsOnCurve(x, y) {
			return nil, errors.New("point not on curve")
		}
		return key, nil
	case "RSA":
		n, err := num(j.N)
		if err != nil {
			return nil, err
		}
		e, err := num(j.E)
		if err != nil || !e.IsInt64() || e.Int64() > 1<<31 {
			return nil, errors.New("bad exponent")
		}
		if n.BitLen() < 2048 {
			return nil, errors.New("RSA key too short")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	}
	return nil, errors.New("unsupported key type")
}

// Verify checks token's signature against keys (ES256 or RS256 only,
// so neither "none" nor a symmetric algorithm can be slipped in), then
// its issuer, audience, times and nonce, and returns its claims.
func Verify(ctx context.Context, keys *KeySet, token string, want Expect) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("identity: not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("identity: JWT header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("identity: JWT signature is not base64url")
	}
	key, err := keys.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch key := key.(type) {
	case *ecdsa.PublicKey:
		if hdr.Alg != "ES256" || len(sig) != 64 {
			return nil, fmt.Errorf("identity: algorithm %q for an EC key", hdr.Alg)
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(key, digest[:], r, s) {
			return nil, errors.New("identity: bad JWT signature")
		}
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" {
			return nil, fmt.Errorf("identity: algorithm %q for an RSA key", hdr.Alg)
		}
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
			return nil, errors.New("identity: bad JWT signature")
		}
	default:
		return nil, errors.New("identity: unsupported key")
	}

	var c Claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return nil, fmt.Errorf("identity: JWT claims: %w", err)
	}
	now := want.Now
	if now.IsZero() {
		now = time.Now()
	}
	switch {
	case c.Issuer != want.Issuer:
		return nil, fmt.Errorf("identity: issuer %q, want %q", c.Issuer, want.Issuer)
	case want.Audience == "" || !c.Audience.has(want.Audience):
		return nil, fmt.Errorf("identity: audience %v, want %q", []string(c.Audience), want.Audience)
	case c.Expiry == 0 || now.After(time.Unix(c.Expiry, 0).Add(leeway)):
		return nil, errors.New("identity: token expired")
	case c.NotBefore != 0 && now.Add(leeway).Before(time.Unix(c.NotBefore, 0)):
		return nil, errors.New("identity: token not yet valid")
	case c.IssuedAt != 0 && now.Add(leeway).Before(time.Unix(c.IssuedAt, 0)):
		return nil, errors.New("identity: token issued in the future")
	case want.Nonce != "" && c.Nonce != want.Nonce:
		return nil, errors.New("identity: nonce mismatch")
	}
	return &c, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
