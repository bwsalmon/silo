package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/bwsalmon/silo/hub/route"
)

// Paths the OIDC handler serves, besides the callback (RedirectURL's).
const (
	LoginPath  = "/auth/login"
	LogoutPath = "/auth/logout"
)

const (
	// SessionCookie holds a signed-in session, and FlowCookie a sign-in
	// in progress. Both are the hub's own: whatever the hub proxies to
	// (a node's UI, hub/nodeui) is not given them.
	SessionCookie = "silo_session"
	FlowCookie    = "silo_oidc_flow"
	flowTTL       = 10 * time.Minute
	// DefaultSessionTTL is how long a sign-in lasts when
	// OIDC.SessionTTL is zero.
	DefaultSessionTTL = 12 * time.Hour
)

// OIDC is the hub as an OpenID Connect relying party, for a hub that is
// not behind IAP. Handler signs users in with the authorization-code
// flow and PKCE and keeps a signed session cookie; Principal reads it.
// Principal also accepts the provider's ID token as a bearer token, for
// SDKs and command-line clients that hold one.
type OIDC struct {
	// Issuer is the provider; its discovery document is read from
	// Issuer + "/.well-known/openid-configuration".
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the hub's callback, registered with the provider.
	// Handler serves its path.
	RedirectURL string
	// Scopes are requested besides "openid"; nil is email and profile.
	Scopes []string
	// SessionKey signs session cookies: at least 32 random bytes, the
	// same on every replica.
	SessionKey []byte
	// SessionTTL bounds a sign-in; DefaultSessionTTL when zero.
	SessionTTL time.Duration
	// Client, when set, is the HTTP client to reach the provider with.
	Client *http.Client
	// Now, when set, is the clock.
	Now func() time.Time

	mu   sync.Mutex
	disc *discovery
}

type discovery struct {
	cfg  oauth2.Config
	keys *KeySet
}

func (o *OIDC) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OIDC) ctx(ctx context.Context) context.Context {
	if o.Client != nil {
		return context.WithValue(ctx, oauth2.HTTPClient, o.Client)
	}
	return ctx
}

// discover reads the provider's discovery document once.
func (o *OIDC) discover(ctx context.Context) (*discovery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.disc != nil {
		return o.disc, nil
	}
	if len(o.SessionKey) < 32 {
		return nil, errors.New("identity: OIDC session key must be at least 32 bytes")
	}
	c := o.Client
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(o.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: OIDC discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: OIDC discovery: %s", resp.Status)
	}
	var d struct {
		Issuer string `json:"issuer"`
		Auth   string `json:"authorization_endpoint"`
		Token  string `json:"token_endpoint"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("identity: OIDC discovery: %w", err)
	}
	if d.Issuer != o.Issuer {
		return nil, fmt.Errorf("identity: OIDC discovery names issuer %q, want %q", d.Issuer, o.Issuer)
	}
	if d.Auth == "" || d.Token == "" || d.JWKS == "" {
		return nil, errors.New("identity: OIDC discovery lacks an endpoint")
	}
	scopes := o.Scopes
	if scopes == nil {
		scopes = []string{"email", "profile"}
	}
	o.disc = &discovery{
		cfg: oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  o.RedirectURL,
			Endpoint:     oauth2.Endpoint{AuthURL: d.Auth, TokenURL: d.Token},
			Scopes:       append([]string{"openid"}, scopes...),
		},
		keys: &KeySet{URL: d.JWKS, Client: o.Client},
	}
	return o.disc, nil
}

// verifyIDToken verifies an ID token the provider issued for this
// client, and that its email is verified.
func (o *OIDC) verifyIDToken(ctx context.Context, d *discovery, tok, nonce string) (Principal, error) {
	c, err := Verify(ctx, d.keys, tok, Expect{Issuer: o.Issuer, Audience: o.ClientID, Nonce: nonce, Now: o.now()})
	if err != nil {
		return Principal{}, err
	}
	if c.EmailVerified == nil || !*c.EmailVerified {
		return Principal{}, errors.New("identity: the provider has not verified this email")
	}
	return principal(c)
}

// session is a signed-in user's cookie.
type session struct {
	Principal
	Expires int64 `json:"exp"`
}

// flow is the state of one sign-in, between the redirect to the
// provider and the callback.
type flow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
	Expires  int64  `json:"exp"`
}

// seal signs v as "payload.mac", base64url, under a key for purpose, so
// a flow cookie can never be presented as a session.
func (o *OIDC) seal(purpose string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(o.mac(purpose, p)), nil
}

func (o *OIDC) open(purpose, s string, v any) error {
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return errors.New("identity: malformed cookie")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, o.mac(purpose, p)) {
		return errors.New("identity: cookie signature does not verify")
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (o *OIDC) mac(purpose, payload string) []byte {
	m := hmac.New(sha256.New, o.SessionKey)
	m.Write([]byte(purpose + "\x00" + payload))
	return m.Sum(nil)
}

// Principal reads r's session cookie, or verifies its bearer ID token.
func (o *OIDC) Principal(r *http.Request) (Principal, error) {
	if len(o.SessionKey) < 32 {
		return Principal{}, errors.New("identity: OIDC session key must be at least 32 bytes")
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		d, err := o.discover(r.Context())
		if err != nil {
			return Principal{}, err
		}
		return o.verifyIDToken(o.ctx(r.Context()), d, tok, "")
	}
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	var s session
	if err := o.open(SessionCookie, ck.Value, &s); err != nil {
		return Principal{}, err
	}
	if o.now().Unix() >= s.Expires {
		return Principal{}, errors.New("identity: session expired")
	}
	return s.Principal, nil
}

// Handler serves LoginPath, LogoutPath and RedirectURL's path.
func (o *OIDC) Handler() http.Handler {
	m := route.NewMux()
	m.HandleFunc("GET "+LoginPath, o.login)
	// Signing out is a write too: another origin on the hub's site
	// must not sign the viewer out.
	m.Handle("POST "+LogoutPath, SameOrigin(http.HandlerFunc(o.logout)))
	cb := "/auth/callback"
	if u, err := url.Parse(o.RedirectURL); err == nil && u.Path != "" {
		cb = u.Path
	}
	m.HandleFunc("GET "+cb, o.callback)
	return m
}

func (o *OIDC) secure() bool { return strings.HasPrefix(o.RedirectURL, "https://") }

func (o *OIDC) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	age := int(ttl / time.Second)
	if ttl < 0 {
		age = -1 // delete it
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		MaxAge: age, HttpOnly: true, Secure: o.secure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// localReturn is where to send the user after signing in: a path on
// this hub only, never another site. Browsers strip control
// characters and read `\` as `/`, so `/\t/evil.example` would reach
// another site: any of them, raw or percent-encoded, refuses the value.
func localReturn(s string) string {
	bad := func(v string) bool {
		return strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' })
	}
	if bad(s) {
		return "/"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || bad(u.Path) ||
		!strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return "/"
	}
	return s
}

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o *OIDC) login(w http.ResponseWriter, r *http.Request) {
	d, err := o.discover(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	f := flow{
		State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(),
		Return: localReturn(r.URL.Query().Get("return")), Expires: o.now().Add(flowTTL).Unix(),
	}
	v, err := o.seal(FlowCookie, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	o.setCookie(w, FlowCookie, v, flowTTL)
	http.Redirect(w, r, d.cfg.AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier), oauth2.SetAuthURLParam("nonce", f.Nonce)), http.StatusFound)
}

func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, err error) { http.Error(w, "sign-in failed: "+err.Error(), code) }
	d, err := o.discover(r.Context())
	if err != nil {
		fail(http.StatusServiceUnavailable, err)
		return
	}
	ck, err := r.Cookie(FlowCookie)
	if err != nil {
		fail(http.StatusBadRequest, errors.New("no sign-in in progress"))
		return
	}
	var f flow
	if err := o.open(FlowCookie, ck.Value, &f); err != nil {
		fail(http.StatusBadRequest, err)
		return
	}
	o.setCookie(w, FlowCookie, "", -1)
	q := r.URL.Query()
	switch {
	case o.now().Unix() >= f.Expires:
		fail(http.StatusBadRequest, errors.New("the sign-in took too long"))
		return
	case q.Get("error") != "":
		fail(http.StatusUnauthorized, fmt.Errorf("the provider says %s", q.Get("error")))
		return
	case q.Get("state") == "" || !hmac.Equal([]byte(q.Get("state")), []byte(f.State)):
		fail(http.StatusBadRequest, errors.New("state mismatch"))
		return
	}
	ctx := o.ctx(r.Context())
	tok, err := d.cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(http.StatusUnauthorized, err)
		return
	}
	idt, _ := tok.Extra("id_token").(string)
	if idt == "" {
		fail(http.StatusUnauthorized, errors.New("the provider returned no ID token"))
		return
	}
	p, err := o.verifyIDToken(ctx, d, idt, f.Nonce)
	if err != nil {
		fail(http.StatusUnauthorized, err)
		return
	}
	ttl := o.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	v, err := o.seal(SessionCookie, session{Principal: p, Expires: o.now().Add(ttl).Unix()})
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	o.setCookie(w, SessionCookie, v, ttl)
	http.Redirect(w, r, f.Return, http.StatusFound)
}

func (o *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	o.setCookie(w, SessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
