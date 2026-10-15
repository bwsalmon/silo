// Package identity signs humans in to the Silo hub (docs/hub.md §5.1).
// There are two ways in, and both produce one Principal:
//
//   - IAP, behind Google Cloud's Identity-Aware Proxy: every request
//     carries the x-goog-iap-jwt-assertion header, whose signature,
//     issuer, audience and expiry are verified against IAP's published
//     keys. The unsigned x-goog-authenticated-user-email header is never
//     read: a request that bypassed the load balancer could forge it.
//   - OIDC, without IAP: the hub is a relying party for the company's
//     SSO, signing users in with the authorization-code flow and PKCE
//     and keeping a signed session cookie.
//
// Nodes do not sign in here: they present node tokens, which package
// directory mints (§5.2).
package identity

import (
	"errors"
	"net/http"
	"strings"
)

// Principal is a signed-in human.
type Principal struct {
	// Email is the verified email address, lower-cased: the identity
	// the team directory knows the human by.
	Email string `json:"email"`
	// Groups are the groups the identity provider says the human is in,
	// if it says; the team directory can name a group as a member.
	Groups []string `json:"groups,omitempty"`
}

// Authenticator proves the Principal behind an HTTP request.
type Authenticator interface {
	Principal(r *http.Request) (Principal, error)
}

// ErrUnauthenticated is a request that proves no principal.
var ErrUnauthenticated = errors.New("identity: unauthenticated")

// principal makes a Principal from verified claims.
func principal(c *Claims) (Principal, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" || !strings.Contains(email, "@") {
		return Principal{}, errors.New("identity: the token carries no email")
	}
	return Principal{Email: email, Groups: c.Groups}, nil
}
