// Package claims signs what the hub attests about a task it offers a
// Grain node (docs/hub.md §6.4): who asked for it, for which team, and
// a digest of what it is, as a JWT the node's local policy checks
// against the keys the hub publishes (grain's docs/local-policy.md,
// "Signed claims"). The node, not the hub, decides whether to run the
// task (§1.2); these claims are what it decides on, in a form the
// rendezvous between them cannot alter.
package claims

import (
	"cmp"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

// KeysPath is where the hub publishes its signing keys, as a JWK set:
// the URL a node's grain_policy.yml names as silo_keys_url. It needs no
// sign-in, and is routed past IAP as the node connection is
// (docs/deploy.md).
const KeysPath = "/v1/claims/keys"

// DefaultTTL is how long a signed attestation is good for. A node
// checks it when it is offered the task, within the dispatcher's offer
// timeout, and again before each dispatch of it; a resume is signed
// afresh, and while the task's stream is open the dispatcher sends a
// fresh one well before the last lapses (tasks.TypeAttest).
const DefaultTTL = time.Hour

// MaxTTL is the longest an attestation may be good for: grain's local
// policy refuses one whose exp is more than 24 hours after its iat.
const MaxTTL = 24 * time.Hour

// Claims are an attestation's claims, named as grain reads them.
type Claims struct {
	Issuer string `json:"iss"`
	// Audience is the node the task is offered to.
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf,omitempty"`
	Expiry    int64  `json:"exp"`
	// HubTaskID is the task's ID at the hub, Spec.ID: grain records the
	// task as filed from it.
	HubTaskID string `json:"hub_task_id"`
	// Requester asked for the task: the node's grain records them as its
	// creator.
	Requester string `json:"requester"`
	Team      string `json:"team,omitempty"`
	// ApprovedBy approved the task: the task's approved_by, empty for
	// one nobody approved at the hub.
	ApprovedBy string `json:"approved_by"`
	// GrantedCapabilities are capabilities the hub grants the task;
	// none today.
	GrantedCapabilities []string `json:"granted_capabilities"`
	// NetworkAccess is "open", "none" or empty, which leaves it to the
	// node.
	NetworkAccess string `json:"network_access,omitempty"`
	// SpecDigest is the task's SpecDigest.
	SpecDigest string `json:"spec_digest"`
}

// Spec is what a task's digest covers.
type Spec struct {
	// Target is the repository, "owner/name", or "host/owner/name" for
	// one not on the default host; empty for a task that leaves the
	// repository to the node.
	Target string
	// Base is the branch the task works from; empty for the node's
	// default.
	Base string
	// Reads are repositories the task may read; Writes, "owner/name@base",
	// the ones it may push to.
	Reads, Writes []string
	Title, Body   string
}

// SpecDigest is "sha256:" and the hex SHA-256 of the spec's canonical
// JSON, as grain's policy.SpecDigest computes it: compact, with its keys
// in the order target, base, reads, writes, title, body; repositories
// lower-cased, reads and writes sorted, and a missing list [] rather
// than null.
func SpecDigest(s Spec) string {
	repos := func(rs []string) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			// A write's base branch keeps its case; only its
			// repository is lower-cased.
			repo, base, ok := strings.Cut(r, "@")
			r = strings.ToLower(repo)
			if ok {
				r += "@" + base
			}
			out = append(out, r)
		}
		slices.Sort(out)
		return out
	}
	b, err := json.Marshal(struct {
		Target string   `json:"target"`
		Base   string   `json:"base"`
		Reads  []string `json:"reads"`
		Writes []string `json:"writes"`
		Title  string   `json:"title"`
		Body   string   `json:"body"`
	}{strings.ToLower(s.Target), s.Base, repos(s.Reads), repos(s.Writes), s.Title, s.Body})
	if err != nil {
		panic(err) // strings and lists of them always marshal
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// key is one of the signer's keys.
type key struct {
	kid    string
	alg    string // ES256 or RS256
	signer crypto.Signer
	jwk    map[string]string
}

// Signer signs attestations with the first of its keys and publishes
// all of them. More than one is for rotation: a new key goes first,
// and the old one stays published until nothing it signed is still
// good (DefaultTTL).
type Signer struct {
	// Issuer is every attestation's "iss": the silo_issuer nodes'
	// policies name, conventionally the hub's own URL.
	Issuer string
	// TTL is how long an attestation is good for; DefaultTTL when zero,
	// and no more than MaxTTL.
	TTL time.Duration
	// Now is the clock; time.Now when nil. For tests.
	Now func() time.Time

	keys []key
}

// NewSigner parses PEM private keys, EC P-256 (SEC 1 or PKCS #8) or RSA
// of at least 2048 bits (PKCS #1 or #8); the first signs.
func NewSigner(issuer string, pems ...[]byte) (*Signer, error) {
	if issuer == "" {
		return nil, errors.New("claims: no issuer")
	}
	if len(pems) == 0 {
		return nil, errors.New("claims: no signing key")
	}
	s := &Signer{Issuer: issuer}
	for i, p := range pems {
		k, err := parseKey(p)
		if err != nil {
			return nil, fmt.Errorf("claims: key %d: %w", i+1, err)
		}
		if slices.ContainsFunc(s.keys, func(o key) bool { return o.kid == k.kid }) {
			return nil, fmt.Errorf("claims: key %d is given twice", i+1)
		}
		s.keys = append(s.keys, k)
	}
	return s, nil
}

func parseKey(p []byte) (key, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return key{}, errors.New("not PEM")
	}
	var priv any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		priv, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		priv, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		priv, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return key{}, fmt.Errorf("a PEM %q block, not a private key", block.Type)
	}
	if err != nil {
		return key{}, err
	}
	b64 := func(n *big.Int, size int) string {
		return base64.RawURLEncoding.EncodeToString(n.FillBytes(make([]byte, size)))
	}
	var k key
	switch priv := priv.(type) {
	case *ecdsa.PrivateKey:
		if priv.Curve != elliptic.P256() {
			return key{}, errors.New("an EC key not on P-256")
		}
		k = key{alg: "ES256", signer: priv, jwk: map[string]string{
			"kty": "EC", "crv": "P-256", "x": b64(priv.X, 32), "y": b64(priv.Y, 32),
		}}
	case *rsa.PrivateKey:
		if priv.N.BitLen() < 2048 {
			return key{}, errors.New("an RSA key under 2048 bits")
		}
		e := big.NewInt(int64(priv.E))
		k = key{alg: "RS256", signer: priv, jwk: map[string]string{
			"kty": "RSA", "n": b64(priv.N, (priv.N.BitLen()+7)/8), "e": b64(e, (e.BitLen()+7)/8),
		}}
	default:
		return key{}, fmt.Errorf("a %T, not an EC or RSA key", priv)
	}
	k.kid = thumbprint(k.jwk)
	k.jwk["kid"], k.jwk["alg"], k.jwk["use"] = k.kid, k.alg, "sig"
	return k, nil
}

// thumbprint is a key's RFC 7638 thumbprint, its kid: the same key
// always has the same kid, on every pod.
func thumbprint(jwk map[string]string) string {
	var members []string
	switch jwk["kty"] {
	case "EC":
		members = []string{"crv", "kty", "x", "y"}
	case "RSA":
		members = []string{"e", "kty", "n"}
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%q", m, jwk[m])
	}
	b.WriteByte('}')
	sum := sha256.Sum256([]byte(b.String()))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Lifetime is how long the attestations s signs are good for: TTL, or
// DefaultTTL when that is zero.
func (s *Signer) Lifetime() time.Duration { return cmp.Or(s.TTL, DefaultTTL) }

// KeyID is the kid of the key the signer signs with.
func (s *Signer) KeyID() string { return s.keys[0].kid }

// Sign signs c with the signer's first key, filling in its issuer, its
// issue time and its expiry.
func (s *Signer) Sign(c Claims) (string, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ttl := s.Lifetime()
	if ttl < 0 || ttl > MaxTTL {
		return "", fmt.Errorf("claims: a TTL of %s is outside (0, %s]", ttl, MaxTTL)
	}
	t := now()
	c.Issuer, c.IssuedAt, c.Expiry = s.Issuer, t.Unix(), t.Add(ttl).Unix()
	if c.GrantedCapabilities == nil {
		c.GrantedCapabilities = []string{}
	}
	k := s.keys[0]
	header, err := json.Marshal(map[string]string{"alg": k.alg, "typ": "JWT", "kid": k.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	var sig []byte
	switch priv := k.signer.(type) {
	case *ecdsa.PrivateKey:
		r, ss, err := ecdsa.Sign(rand.Reader, priv, sum[:])
		if err != nil {
			return "", err
		}
		// JWS wants r and s, each 32 bytes, not ASN.1.
		sig = append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	case *rsa.PrivateKey:
		if sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:]); err != nil {
			return "", err
		}
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// KeySet is the signer's public keys as a JWK set, {"keys":[...]}.
func (s *Signer) KeySet() []byte {
	keys := make([]map[string]string, len(s.keys))
	for i, k := range s.keys {
		keys[i] = k.jwk
	}
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		panic(err)
	}
	return b
}

// Handler serves KeySet, to anyone: the keys are public.
func (s *Signer) Handler() http.Handler {
	set := s.KeySet()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/jwk-set+json")
		// Short, so a rotation is seen soon; a node that meets an
		// unknown kid fetches again anyway.
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write(set)
	})
}
