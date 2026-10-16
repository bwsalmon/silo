package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwsalmon/silo/hub/claims"
)

func claimsKey(t *testing.T) []byte {
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

// With a claims key the hub signs offers and publishes the key, to
// anyone, behind whatever sign-on the hub has: nodes fetch it as
// silo_keys_url without a human's sign-in.
func TestClaimsKeysPublished(t *testing.T) {
	cfg := Config{Database: "sqlite::memory:", IAPAudience: "/projects/1/global/backendServices/2", Logf: logf(t),
		ClaimsIssuer: "https://hub.example.com", ClaimsKeys: [][]byte{claimsKey(t)}}
	h, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Dispatcher.Claims == nil {
		t.Fatal("the dispatcher has no signer")
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", claims.KeysPath, nil))
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &set) != nil {
		t.Fatalf("%s: %d %s", claims.KeysPath, rec.Code, rec.Body)
	}
	if len(set.Keys) != 1 || set.Keys[0]["kid"] != h.Dispatcher.Claims.KeyID() || set.Keys[0]["d"] != "" {
		t.Errorf("published %v", set.Keys)
	}
}

func TestClaimsConfig(t *testing.T) {
	base := Config{Database: "sqlite::memory:", DevUserHeader: userHeader}
	key := claimsKey(t)
	for name, mod := range map[string]func(*Config){
		"a key without an issuer": func(c *Config) { c.ClaimsKeys = [][]byte{key} },
		"an issuer without a key": func(c *Config) { c.ClaimsIssuer = "https://hub.example.com" },
		"a bad key": func(c *Config) {
			c.ClaimsIssuer, c.ClaimsKeys = "https://hub.example.com", [][]byte{[]byte("not a key")}
		},
	} {
		cfg := base
		mod(&cfg)
		if h, err := New(context.Background(), cfg); err == nil {
			h.Close()
			t.Errorf("%s: New succeeded", name)
		}
	}
	// Without either, offers go unattested and nothing is published.
	h, err := New(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Dispatcher.Claims != nil {
		t.Error("a signer with no key configured")
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", claims.KeysPath, nil))
	if rec.Code == http.StatusOK {
		t.Errorf("%s served with no key: %s", claims.KeysPath, rec.Body)
	}
}
