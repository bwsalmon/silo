package identity

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// idp is a fake OpenID provider: authorization code flow with PKCE.
type idp struct {
	t      *testing.T
	srv    *httptest.Server
	key    *signer
	claims map[string]any // merged into every ID token

	mu    sync.Mutex
	codes map[string][2]string // code → challenge, nonce
}

func newIDP(t *testing.T) *idp {
	ec, _ := newSigners(t)
	p := &idp{t: t, key: ec, codes: map[string][2]string{}, claims: map[string]any{
		"email": "bob@example.com", "email_verified": true, "groups": []string{"eng@example.com"},
	}}
	m := http.NewServeMux()
	m.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks",
		})
	})
	m.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{p.key.jwk()}})
	})
	m.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "hub" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
			q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") {
			http.Error(w, "bad authorize request: "+r.URL.RawQuery, 400)
			return
		}
		code := random()
		p.mu.Lock()
		p.codes[code] = [2]string{q.Get("code_challenge"), q.Get("nonce")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	m.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		c, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || b64.EncodeToString(sum[:]) != c[0] {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.idToken(c[1]),
		})
	})
	p.srv = httptest.NewServer(m)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) idToken(nonce string) string {
	now := time.Now()
	c := map[string]any{"iss": p.srv.URL, "aud": "hub", "sub": "1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	for k, v := range p.claims {
		c[k] = v
	}
	return p.key.sign(p.t, c)
}

// hubWithOIDC serves o's handler and /whoami, which answers the
// signed-in principal's email.
func hubWithOIDC(t *testing.T, p *idp) (*OIDC, *httptest.Server) {
	o := &OIDC{Issuer: p.srv.URL, ClientID: "hub", ClientSecret: "s", SessionKey: []byte(strings.Repeat("k", 32))}
	m := http.NewServeMux()
	m.Handle("/auth/", o.Handler())
	m.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		pr, err := o.Principal(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		io.WriteString(w, pr.Email+" "+strings.Join(pr.Groups, ","))
	})
	hub := httptest.NewServer(m)
	t.Cleanup(hub.Close)
	o.RedirectURL = hub.URL + "/auth/callback"
	return o, hub
}

func get(t *testing.T, c *http.Client, u string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestOIDCSignIn(t *testing.T) {
	p := newIDP(t)
	_, hub := hubWithOIDC(t, p)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if code, _ := get(t, c, hub.URL+"/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("before sign-in: %d, want 401", code)
	}
	code, body := get(t, c, hub.URL+LoginPath+"?return=/whoami")
	if code != 200 || body != "bob@example.com eng@example.com" {
		t.Fatalf("after sign-in: %d %q", code, body)
	}

	// The session cookie is signed: editing it signs nobody in.
	u, _ := url.Parse(hub.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == SessionCookie {
			payload, _, _ := strings.Cut(ck.Value, ".")
			b, _ := b64.DecodeString(payload)
			forged := strings.Replace(string(b), "bob@", "eve@", 1)
			ck.Value = b64.EncodeToString([]byte(forged)) + "." + strings.SplitN(ck.Value, ".", 2)[1]
			jar2, _ := cookiejar.New(nil)
			jar2.SetCookies(u, []*http.Cookie{ck})
			if code, body := get(t, &http.Client{Jar: jar2}, hub.URL+"/whoami"); code != http.StatusUnauthorized {
				t.Errorf("forged session: %d %q, want 401", code, body)
			}
		}
	}

	// Signing out drops the session.
	req, _ := http.NewRequest("POST", hub.URL+LogoutPath, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if code, _ := get(t, c, hub.URL+"/whoami"); code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d, want 401", code)
	}
}

func TestOIDCRefusesUnverifiedEmail(t *testing.T) {
	p := newIDP(t)
	p.claims["email_verified"] = false
	_, hub := hubWithOIDC(t, p)
	jar, _ := cookiejar.New(nil)
	if code, body := get(t, &http.Client{Jar: jar}, hub.URL+LoginPath); code != http.StatusUnauthorized || !strings.Contains(body, "verified") {
		t.Errorf("unverified email: %d %q, want 401", code, body)
	}
}

func TestOIDCStateMismatch(t *testing.T) {
	p := newIDP(t)
	_, hub := hubWithOIDC(t, p)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Start a sign-in, so there is a flow cookie, then arrive at the
	// callback with someone else's state: a login CSRF.
	if code, _ := get(t, c, hub.URL+LoginPath); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	if code, _ := get(t, c, hub.URL+"/auth/callback?code=x&state=attacker"); code != http.StatusBadRequest {
		t.Errorf("mismatched state: %d, want 400", code)
	}
	// And with no flow at all.
	if code, _ := get(t, http.DefaultClient, hub.URL+"/auth/callback?code=x&state=y"); code != http.StatusBadRequest {
		t.Errorf("no flow: %d, want 400", code)
	}
}

func TestOIDCBearerIDToken(t *testing.T) {
	p := newIDP(t)
	_, hub := hubWithOIDC(t, p)
	if code, body := get(t, http.DefaultClient, hub.URL+"/whoami", "Authorization", "Bearer "+p.idToken("")); code != 200 || !strings.HasPrefix(body, "bob@example.com") {
		t.Errorf("bearer ID token: %d %q", code, body)
	}
	other := newIDP(t) // a different provider's token, though for a client also called "hub"
	if code, _ := get(t, http.DefaultClient, hub.URL+"/whoami", "Authorization", "Bearer "+other.idToken("")); code != http.StatusUnauthorized {
		t.Errorf("another provider's ID token: %d, want 401", code)
	}
}
