package identity

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// IAPHeader carries IAP's signed assertion on every request it lets
	// through.
	IAPHeader = "x-goog-iap-jwt-assertion"
	// IAPIssuer is the issuer of IAP's assertions.
	IAPIssuer = "https://cloud.google.com/iap"
	// IAPKeysURL is where IAP publishes the keys it signs with.
	IAPKeysURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
)

// IAP authenticates requests that came through Google Cloud's
// Identity-Aware Proxy, from the signed assertion IAP adds.
type IAP struct {
	// Audience is the hub's backend: "/projects/NUMBER/global/
	// backendServices/ID" behind a load balancer, or "/projects/NUMBER/
	// apps/PROJECT" on App Engine. An assertion for any other backend
	// is refused, so a token lifted from another IAP-protected app
	// cannot be replayed here.
	Audience string
	// NodeUIAudience, when set alongside NodeUIDomain, is the dedicated
	// GCLB backend service IAP audience required on node UI hosts
	// (*.<NodeUIDomain>), while Audience is required on the hub's own
	// host. When empty, Audience is accepted on both.
	NodeUIAudience string
	// NodeUIDomain is the domain suffix under which node UI hosts are
	// served (e.g. "nodes.hub.example.com").
	NodeUIDomain string
	// Keys are IAP's keys; nil fetches them from IAPKeysURL.
	Keys *KeySet
	// Now, when set, is the clock tokens are checked against.
	Now func() time.Time

	once sync.Once
	keys *KeySet
}

func (a *IAP) audienceForHost(host string) string {
	if a.NodeUIAudience != "" && a.NodeUIDomain != "" && isNodeUIHost(host, a.NodeUIDomain) {
		return a.NodeUIAudience
	}
	return a.Audience
}

func isNodeUIHost(host, domain string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return false
	}
	id, ok := strings.CutSuffix(host, "."+domain)
	return ok && id != "" && !strings.Contains(id, ".")
}

// Principal verifies r's IAP assertion.
func (a *IAP) Principal(r *http.Request) (Principal, error) {
	tok := r.Header.Get(IAPHeader)
	if tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	aud := a.audienceForHost(r.Host)
	if aud == "" {
		return Principal{}, errors.New("identity: IAP audience is not configured")
	}
	a.once.Do(func() {
		a.keys = a.Keys
		if a.keys == nil {
			a.keys = &KeySet{URL: IAPKeysURL}
		}
	})
	want := Expect{Issuer: IAPIssuer, Audience: aud}
	if a.Now != nil {
		want.Now = a.Now()
	}
	c, err := Verify(r.Context(), a.keys, tok, want)
	if err != nil {
		return Principal{}, err
	}
	return principal(c)
}
