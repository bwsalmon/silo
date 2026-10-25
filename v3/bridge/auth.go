package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Default paths and cookie/header names for UI authentication (design §6.1, §6.5).
const (
	DefaultUITokenFile = "/var/lib/silo/ui/token.sha256"
	SessionCookieName  = "silo_session"
	CSRFHeaderName     = "X-Silo-CSRF"
	AltCSRFHeaderName  = "X-CSRF-Token"
	IAPJWTHeaderName   = "X-Goog-IAP-JWT-Assertion"

	uiTokenPrefix   = "silo_ui_"
	sessionIDPrefix = "silo_uis_"
	csrfTokenPrefix = "silo_csrf_"

	defaultSessionTTL = 12 * time.Hour
)

// Principal describes an authenticated human caller at the bridge HTTP/WS boundary.
type Principal struct {
	Subject   string `json:"subject"`
	Method    string `json:"method"` // "token" or "iap"
	CSRFToken string `json:"csrf_token,omitempty"`
}

// Authenticator authenticates HTTP and WebSocket requests to the bridge/UI and
// handles login session lifecycle. Keeping authentication behind this interface
// allows Local mode (login token from `silo ui token` + HttpOnly SameSite=Strict
// session cookies) and GCP mode (IAP JWT header `X-Goog-IAP-JWT-Assertion`) to
// plug into the same HTTP server.
type Authenticator interface {
	// Authenticate inspects r and returns the authenticated Principal if valid.
	Authenticate(r *http.Request) (*Principal, error)
	// Login exchanges a login token for a session and writes scoped session
	// cookies (`Path=/api` and `Path=/acp`) to w.
	Login(w http.ResponseWriter, r *http.Request, token string) (*Principal, error)
	// Logout revokes the active session on r and clears session cookies on w.
	Logout(w http.ResponseWriter, r *http.Request) error
	// RequireCSRF reports whether mutating requests authenticated by p must
	// carry a matching CSRF token header.
	RequireCSRF(p *Principal) bool
}

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrInvalidToken    = errors.New("invalid login token")
	ErrInvalidCSRF     = errors.New("missing or invalid CSRF token")
	ErrPageIsolation   = errors.New("requests from /p/ are isolated from /api and /acp")
)

type uiSession struct {
	ID        string
	Subject   string
	CSRFToken string
	ExpiresAt time.Time
}

// TokenAuthenticator implements Authenticator using a login token generated at
// install (`silo ui token`). Upon login it sets HttpOnly, SameSite=Strict
// session cookies scoped strictly to `Path=/api` and `Path=/acp` so proxied
// apps under `/p/<slug>/` never receive the session cookie, and requires a
// matching `X-Silo-CSRF` header on all mutating `/api/v1/*` requests.
type TokenAuthenticator struct {
	mu         sync.Mutex
	tokenHash  string
	sessionTTL time.Duration
	sessions   map[string]*uiSession
	now        func() time.Time
}

// NewTokenAuthenticator creates a TokenAuthenticator expecting a login token
// whose hex-encoded SHA-256 digest is tokenHash.
func NewTokenAuthenticator(tokenHash string) *TokenAuthenticator {
	return &TokenAuthenticator{
		tokenHash:  strings.TrimSpace(strings.ToLower(tokenHash)),
		sessionTTL: defaultSessionTTL,
		sessions:   make(map[string]*uiSession),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// NewTokenAuthenticatorFromRaw creates a TokenAuthenticator from a raw login token.
func NewTokenAuthenticatorFromRaw(rawToken string) *TokenAuthenticator {
	return NewTokenAuthenticator(hashString(strings.TrimSpace(rawToken)))
}

// LoadTokenAuthenticator reads a token hash (or raw `silo_ui_...` token) from path.
func LoadTokenAuthenticator(path string) (*TokenAuthenticator, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil, fmt.Errorf("%s: empty token file", path)
	}
	if strings.HasPrefix(content, uiTokenPrefix) || len(content) != 64 {
		return NewTokenAuthenticatorFromRaw(content), nil
	}
	return NewTokenAuthenticator(content), nil
}

// GenerateTokenFile generates a fresh high-entropy login token (`silo_ui_<hex>`),
// atomically writes its SHA-256 hex digest to path (mode 0600), and returns the
// raw token to be printed once by `silo ui token`.
func GenerateTokenFile(path string) (string, error) {
	if path == "" {
		path = DefaultUITokenFile
	}
	rawTok := randomPrefixed(uiTokenPrefix, 32)
	hash := hashString(rawTok)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".ui-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(hash + "\n"); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return rawTok, nil
}

func (a *TokenAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	sid := readSessionCookie(r)
	if sid == "" {
		return nil, ErrUnauthenticated
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[sid]
	if !ok {
		return nil, ErrUnauthenticated
	}
	if a.now().After(sess.ExpiresAt) {
		delete(a.sessions, sid)
		return nil, ErrUnauthenticated
	}
	return &Principal{
		Subject:   sess.Subject,
		Method:    "token",
		CSRFToken: sess.CSRFToken,
	}, nil
}

func (a *TokenAuthenticator) Login(w http.ResponseWriter, r *http.Request, token string) (*Principal, error) {
	candidate := hashString(strings.TrimSpace(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tokenHash == "" || subtle.ConstantTimeCompare([]byte(candidate), []byte(a.tokenHash)) != 1 {
		return nil, ErrInvalidToken
	}
	sid := randomPrefixed(sessionIDPrefix, 32)
	csrf := randomPrefixed(csrfTokenPrefix, 32)
	sess := &uiSession{
		ID:        sid,
		Subject:   "admin",
		CSRFToken: csrf,
		ExpiresAt: a.now().Add(a.sessionTTL),
	}
	a.sessions[sid] = sess
	writeScopedSessionCookies(w, r, sid, int(a.sessionTTL.Seconds()))
	return &Principal{
		Subject:   sess.Subject,
		Method:    "token",
		CSRFToken: sess.CSRFToken,
	}, nil
}

func (a *TokenAuthenticator) Logout(w http.ResponseWriter, r *http.Request) error {
	sid := readSessionCookie(r)
	if sid != "" {
		a.mu.Lock()
		delete(a.sessions, sid)
		a.mu.Unlock()
	}
	clearScopedSessionCookies(w, r)
	return nil
}

func (a *TokenAuthenticator) RequireCSRF(p *Principal) bool {
	return p != nil && p.Method == "token"
}

// IAPVerifyFunc verifies a Google Cloud IAP JWT assertion (`X-Goog-IAP-JWT-Assertion`)
// and returns the authenticated user's email address.
type IAPVerifyFunc func(ctx context.Context, assertion string) (email string, err error)

// IAPAuthenticator implements Authenticator for GCP Cloud Run + IAP mode
// (design §6.1, §12.4) by verifying the `X-Goog-IAP-JWT-Assertion` header and
// checking against configured owner email(s). It optionally delegates to a
// fallback Authenticator (such as TokenAuthenticator) when needed.
type IAPAuthenticator struct {
	Verify        IAPVerifyFunc
	AllowedOwners []string
	CSRFSecret    string
	Fallback      Authenticator
}

func (ia *IAPAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	assertion := strings.TrimSpace(r.Header.Get(IAPJWTHeaderName))
	if assertion == "" {
		if ia.Fallback != nil {
			return ia.Fallback.Authenticate(r)
		}
		return nil, ErrUnauthenticated
	}
	if ia.Verify == nil {
		return nil, errors.New("IAP JWT verifier not configured")
	}
	email, err := ia.Verify(r.Context(), assertion)
	if err != nil {
		return nil, fmt.Errorf("IAP JWT verification failed: %w", err)
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, ErrUnauthenticated
	}
	if len(ia.AllowedOwners) > 0 {
		allowed := false
		for _, o := range ia.AllowedOwners {
			if strings.EqualFold(strings.TrimSpace(o), email) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("IAP user %q is not an authorized owner", email)
		}
	}
	csrf := hashString("iap-csrf:" + ia.CSRFSecret + ":" + email + ":" + assertion)
	return &Principal{
		Subject:   email,
		Method:    "iap",
		CSRFToken: csrf,
	}, nil
}

func (ia *IAPAuthenticator) Login(w http.ResponseWriter, r *http.Request, token string) (*Principal, error) {
	if ia.Fallback != nil {
		return ia.Fallback.Login(w, r, token)
	}
	return ia.Authenticate(r)
}

func (ia *IAPAuthenticator) Logout(w http.ResponseWriter, r *http.Request) error {
	if ia.Fallback != nil {
		return ia.Fallback.Logout(w, r)
	}
	clearScopedSessionCookies(w, r)
	return nil
}

func (ia *IAPAuthenticator) RequireCSRF(p *Principal) bool {
	return p != nil && p.CSRFToken != ""
}

// writeScopedSessionCookies writes the admin session cookie scoped exclusively
// to `Path=/api` and `Path=/acp` with `HttpOnly` and `SameSite=Strict` so that
// proxied pages under `/p/<slug>/` can never read or receive it.
func writeScopedSessionCookies(w http.ResponseWriter, r *http.Request, sid string, maxAge int) {
	secure := isHTTPSRequest(r)
	for _, p := range []string{"/api", "/acp"} {
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    sid,
			Path:     p,
			MaxAge:   maxAge,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func clearScopedSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := isHTTPSRequest(r)
	for _, p := range []string{"/api", "/acp"} {
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     p,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func readSessionCookie(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == SessionCookieName && strings.TrimSpace(c.Value) != "" {
			return strings.TrimSpace(c.Value)
		}
	}
	return ""
}

func readCSRFHeader(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(CSRFHeaderName)); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get(AltCSRFHeaderName))
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func isHTTPSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// isFromProxiedPage reports whether r's Referer points to `/p/...` (an
// agent-controlled page link). Requests originating from `/p/` are strictly
// forbidden from calling `/api/*` or `/acp/*`.
func isFromProxiedPage(r *http.Request) bool {
	ref := strings.TrimSpace(r.Header.Get("Referer"))
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	return u.Path == "/p" || strings.HasPrefix(u.Path, "/p/")
}

func verifyCSRFToken(r *http.Request, expected string) bool {
	got := readCSRFHeader(r)
	if got == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomPrefixed(prefix string, nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}
