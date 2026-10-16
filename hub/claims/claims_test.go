package claims

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/identity"
)

func ecPEM(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func rsaPEM(t *testing.T, bits int) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// The digest is of exactly the JSON grain's policy.SpecDigest hashes;
// the expected sums are sha256sum's of that JSON, written out by hand.
func TestSpecDigest(t *testing.T) {
	for _, c := range []struct {
		name string
		spec Spec
		// json is what is hashed, for the reader.
		json, want string
	}{{
		name: "empty",
		spec: Spec{},
		json: `{"target":"","base":"","reads":[],"writes":[],"title":"","body":""}`,
		want: "sha256:6727d1e717c1460ec77d8744d5577c0c9a21fa34021d6f4942375ee1e4824894",
	}, {
		name: "full, unsorted and mixed case",
		spec: Spec{
			Target: "Acme/Widgets",
			Base:   "main",
			Reads:  []string{"GHE.example.com/acme/lib", "acme/Docs"},
			Writes: []string{"ACME/widgets@Release-1"},
			Title:  "Fix the parser",
			Body:   "Fix it.\nPlease.",
		},
		json: `{"target":"acme/widgets","base":"main","reads":["acme/docs","ghe.example.com/acme/lib"],"writes":["acme/widgets@Release-1"],"title":"Fix the parser","body":"Fix it.\nPlease."}`,
		want: "sha256:0c79c8741a3cf89d93ffcc8555f973f467610062f83f8a72deeb112f98337352",
	}} {
		t.Run(c.name, func(t *testing.T) {
			if got := SpecDigest(c.spec); got != c.want {
				t.Errorf("SpecDigest = %s, want %s (of %s)", got, c.want, c.json)
			}
		})
	}
	// The caller's lists are not reordered.
	reads := []string{"b/b", "a/a"}
	SpecDigest(Spec{Reads: reads})
	if reads[0] != "b/b" {
		t.Errorf("SpecDigest sorted its caller's reads: %v", reads)
	}
}

// What the hub signs verifies against what it publishes, with a JWT
// verifier that is not its own.
func TestSignVerifies(t *testing.T) {
	for _, c := range []struct {
		name string
		pems [][]byte
	}{
		{"ES256", [][]byte{ecPEM(t)}},
		{"RS256", [][]byte{rsaPEM(t, 2048)}},
		{"rotated", [][]byte{ecPEM(t), rsaPEM(t, 2048)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			s, err := NewSigner("https://hub.example.com", c.pems...)
			if err != nil {
				t.Fatal(err)
			}
			s.Now = func() time.Time { return now }
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			digest := SpecDigest(Spec{Target: "acme/widgets", Title: "t", Body: "b"})
			tok, err := s.Sign(Claims{Audience: "ws-1", HubTaskID: "t-1", Requester: "ann@example.com", Team: "infra", SpecDigest: digest})
			if err != nil {
				t.Fatal(err)
			}
			keys := &identity.KeySet{URL: srv.URL}
			if _, err := identity.Verify(context.Background(), keys, tok, identity.Expect{Issuer: s.Issuer, Audience: "ws-1", Now: now}); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if _, err := identity.Verify(context.Background(), keys, tok, identity.Expect{Issuer: s.Issuer, Audience: "ws-2", Now: now}); err == nil {
				t.Error("Verify accepted another node's attestation")
			}
			if _, err := identity.Verify(context.Background(), keys, tok, identity.Expect{Issuer: s.Issuer, Audience: "ws-1", Now: now.Add(2 * DefaultTTL)}); err == nil {
				t.Error("Verify accepted an expired attestation")
			}

			parts := strings.Split(tok, ".")
			var hdr map[string]string
			decode(t, parts[0], &hdr)
			if hdr["kid"] != s.KeyID() || hdr["kid"] == "" {
				t.Errorf("kid %q, want %q", hdr["kid"], s.KeyID())
			}
			var got map[string]any
			decode(t, parts[1], &got)
			want := map[string]any{
				"iss": "https://hub.example.com", "aud": "ws-1",
				"iat": float64(now.Unix()), "exp": float64(now.Add(DefaultTTL).Unix()), "hub_task_id": "t-1",
				"requester": "ann@example.com", "team": "infra", "approved_by": "",
				"granted_capabilities": []any{}, "spec_digest": digest,
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("claims\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

func decode(t *testing.T, seg string, v any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// The same key has the same kid wherever it is loaded, so every pod
// signs with the kid the published set names.
func TestKeyIDStable(t *testing.T) {
	p := ecPEM(t)
	a, err := NewSigner("i", p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSigner("i", p)
	if err != nil {
		t.Fatal(err)
	}
	if a.KeyID() != b.KeyID() {
		t.Errorf("kids %q and %q for one key", a.KeyID(), b.KeyID())
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(a.KeySet(), &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 1 || set.Keys[0]["kid"] != a.KeyID() || set.Keys[0]["d"] != "" {
		t.Errorf("key set %v", set.Keys)
	}
}

func TestNewSignerRefuses(t *testing.T) {
	p := ecPEM(t)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(p384)
	for _, c := range []struct {
		name   string
		issuer string
		pems   [][]byte
	}{
		{"no issuer", "", [][]byte{p}},
		{"no key", "i", nil},
		{"not PEM", "i", [][]byte{[]byte("key")}},
		{"a public key", "i", [][]byte{pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}})}},
		{"P-384", "i", [][]byte{pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})}},
		{"short RSA", "i", [][]byte{rsaPEM(t, 1024)}},
		{"the same key twice", "i", [][]byte{p, p}},
	} {
		if _, err := NewSigner(c.issuer, c.pems...); err == nil {
			t.Errorf("%s: NewSigner accepted it", c.name)
		}
	}
}

// An attestation is good for at most a day, as grain requires.
func TestSignRefusesLongTTL(t *testing.T) {
	s, err := NewSigner("https://hub.example.com", ecPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	s.TTL = MaxTTL
	if _, err := s.Sign(Claims{Audience: "ws-1"}); err != nil {
		t.Errorf("a TTL of MaxTTL: %v", err)
	}
	for _, ttl := range []time.Duration{MaxTTL + time.Second, -time.Second} {
		s.TTL = ttl
		if _, err := s.Sign(Claims{Audience: "ws-1"}); err == nil {
			t.Errorf("signed with a TTL of %s", ttl)
		}
	}
}
