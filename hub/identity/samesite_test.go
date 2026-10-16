package identity

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	const hub = "hub.example"
	cases := []struct {
		name   string
		method string
		body   string
		header map[string]string
		tls    bool
		want   int
	}{
		{"a read from anywhere", "GET", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, true, 200},
		{"a non-browser client", "POST", "{}", map[string]string{"Content-Type": "application/json"}, true, 200},
		{"the hub's own page", "POST", "{}", map[string]string{"Content-Type": "application/json; charset=utf-8", "Sec-Fetch-Site": "same-origin", "Origin": "https://hub.example"}, true, 200},
		{"no body, no content type", "DELETE", "", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://HUB.example"}, true, 200},
		{"plain http in development", "POST", "{}", map[string]string{"Content-Type": "application/json", "Origin": "http://hub.example"}, false, 200},

		// SA1-M1: a node's UI is the same site as the hub.
		{"a node UI host, same-site", "POST", "{}", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site", "Origin": "https://ws-1.nodes.hub.example"}, true, 403},
		{"a node UI host's origin alone", "POST", "{}", map[string]string{"Content-Type": "application/json", "Origin": "https://ws-1.nodes.hub.example"}, true, 403},
		{"a node UI host's no-cors text/plain", "POST", `{"team":"infra"}`, map[string]string{"Content-Type": "text/plain;charset=UTF-8", "Sec-Fetch-Site": "same-site", "Origin": "https://ws-1.nodes.hub.example"}, true, 403},
		{"cross-site", "POST", "{}", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, true, 403},
		{"a cross-site origin alone", "PUT", "{}", map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, true, 403},
		{"an opaque origin", "POST", "{}", map[string]string{"Content-Type": "application/json", "Origin": "null"}, true, 403},
		{"user-initiated", "POST", "{}", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "none"}, true, 403},
		{"http origin on an https hub", "POST", "{}", map[string]string{"Content-Type": "application/json", "Origin": "http://hub.example"}, true, 403},
		{"http origin behind a TLS load balancer", "POST", "{}", map[string]string{"Content-Type": "application/json", "Origin": "http://hub.example", "X-Forwarded-Proto": "https"}, false, 403},
		{"a text/plain body", "POST", `{"team":"infra"}`, map[string]string{"Content-Type": "text/plain"}, true, 415},
		{"a form body", "POST", "a=b", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin"}, true, 415},
		{"a body with no content type", "POST", "{}", nil, true, 415},
	}
	h := SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://"+hub+"/v1/tasks", strings.NewReader(c.body))
			if !c.tls {
				r.TLS = nil
			} else {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range c.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%d %s, want %d", w.Code, strings.TrimSpace(w.Body.String()), c.want)
			}
		})
	}
}
