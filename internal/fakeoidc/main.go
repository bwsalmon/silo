// Command fakeoidc is an OIDC provider for scripts/grain-interop.sh: it
// serves a discovery document and a key set, and writes an ID token for
// one verified email, signed with a key made at start, so a hub run with
// -oidc-issuer pointed here takes that token as a bearer token
// (hub/identity's OIDC.Principal) -- as `grain hub login -id-token-file`
// presents it. It signs nobody in interactively; its authorization and
// token endpoints exist only because discovery must name them.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on")
	clientID := flag.String("client-id", "", "the audience of the ID token: the hub's -oidc-client-id")
	email := flag.String("email", "", "the verified email the ID token names")
	tokenFile := flag.String("token-file", "", "where to write the ID token")
	flag.Parse()
	if *clientID == "" || *email == "" || *tokenFile == "" {
		log.Fatal("fakeoidc: -client-id, -email and -token-file are required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	issuer := "http://" + l.Addr().String()
	b64 := base64.RawURLEncoding.EncodeToString
	segment := func(v any) string {
		b, _ := json.Marshal(v)
		return b64(b)
	}
	now := time.Now()
	signed := segment(map[string]string{"alg": "ES256", "kid": "k1", "typ": "JWT"}) + "." + segment(map[string]any{
		"iss": issuer, "sub": *email, "aud": *clientID, "email": *email, "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(2 * time.Hour).Unix(),
	})
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		log.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	if err := os.WriteFile(*tokenFile, []byte(signed+"."+b64(sig)+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	m := http.NewServeMux()
	m.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"jwks_uri":               issuer + "/jwks",
		})
	})
	m.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		x, y := make([]byte, 32), make([]byte, 32)
		key.X.FillBytes(x)
		key.Y.FillBytes(y)
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kid": "k1", "kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "x": b64(x), "y": b64(y)},
		}})
	})
	log.Printf("fakeoidc: issuer %s, an ID token for %s in %s", issuer, *email, *tokenFile)
	log.Fatal(http.Serve(l, m))
}
