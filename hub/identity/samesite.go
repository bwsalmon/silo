package identity

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// SameOrigin refuses a state-changing request (any method but GET,
// HEAD and OPTIONS) that a browser sent from anywhere but the hub's own
// origin, before it reaches h (docs/threat-model.md INV-16, T-TB1-4).
//
// The hub's cookies, its OIDC session and IAP's, are SameSite=Lax, so a
// browser sends them on a POST from any page on the same site. A node's
// UI, at <node>.<NodeUIDomain>, is by default on the hub's own site
// (hub/nodeui): a check that refused only cross-site requests would let
// a Grain's own UI page write to the hub as its owner. So:
//
//   - Sec-Fetch-Site must be absent (not a browser) or same-origin;
//   - an Origin, when present, must be the hub's own scheme and host;
//   - a body must be Content-Type: application/json, which no form and
//     no fetch without a CORS preflight can send.
//
// The hub answers no CORS preflight, so a browser never sends the
// cross-origin JSON write these checks would otherwise have to catch.
func SameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckWrite(r); err != nil {
			code := http.StatusForbidden
			if errors.Is(err, errContentType) {
				code = http.StatusUnsupportedMediaType
			}
			http.Error(w, err.Error(), code)
			return
		}
		h.ServeHTTP(w, r)
	})
}

var errContentType = errors.New("identity: a request body must be Content-Type: application/json")

// CheckWrite is SameOrigin's check: nil for a safe method, or for a
// state-changing request the hub's own origin, or a client that is not
// a browser, sent.
func CheckWrite(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
		return errors.New("identity: refused a " + s + " request: the hub accepts writes only from its own origin")
	}
	if o := r.Header.Get("Origin"); o != "" && !hubOrigin(r, o) {
		return errors.New("identity: origin " + o + " is not this hub's")
	}
	if r.ContentLength != 0 {
		t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || t != "application/json" {
			return errContentType
		}
	}
	return nil
}

// hubOrigin is whether origin is the hub's own: its host is the one the
// request was sent to, and its scheme is https, or http when the
// request reached the hub over plain http (local development) rather
// than through TLS or a load balancer that terminated it.
func hubOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		!strings.EqualFold(u.Host, r.Host) {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		return r.TLS == nil && !strings.EqualFold(proto, "https")
	}
	return false
}
