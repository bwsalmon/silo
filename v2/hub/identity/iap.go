package identity

import (
	"errors"
	"net/http"
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
	// Keys are IAP's keys; nil fetches them from IAPKeysURL.
	Keys *KeySet
	// Now, when set, is the clock tokens are checked against.
	Now func() time.Time

	once sync.Once
	keys *KeySet
}

// Principal verifies r's IAP assertion.
func (a *IAP) Principal(r *http.Request) (Principal, error) {
	tok := r.Header.Get(IAPHeader)
	if tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	if a.Audience == "" {
		return Principal{}, errors.New("identity: IAP audience is not configured")
	}
	a.once.Do(func() {
		a.keys = a.Keys
		if a.keys == nil {
			a.keys = &KeySet{URL: IAPKeysURL}
		}
	})
	want := Expect{Issuer: IAPIssuer, Audience: a.Audience}
	if a.Now != nil {
		want.Now = a.Now()
	}
	c, err := Verify(r.Context(), a.keys, tok, want)
	if err != nil {
		return Principal{}, err
	}
	return principal(c)
}
