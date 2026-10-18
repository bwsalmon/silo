package grainfiles

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/route"
)

// HTTP paths served by the Grainfile webhook receiver, content proxy, and
// human inspection endpoint.
const (
	GitHubWebhookPath = "/v1/webhooks/github"
	ContentPath       = "/v1/grainfiles/content"
	GrainfilesPath    = "/v1/grainfiles"
)

const maxWebhookBodyBytes = 10 * 1024 * 1024 // 10 MiB

// RepoFetcher fetches a file's raw bytes from a source git repository at a
// given ref or commit SHA on demand without storing repository contents at rest.
type RepoFetcher interface {
	FetchFile(ctx context.Context, repoURL, ref, filePath string) ([]byte, error)
}

// NodeTokenVerifier verifies a bearer node token and returns its owner and node ID.
// *directory.Store satisfies this interface.
type NodeTokenVerifier interface {
	VerifyNodeToken(ctx context.Context, tok string) (owner, node string, err error)
}

// GitHubFetcher is a production RepoFetcher backed by the GitHub REST Contents
// API (`GET {BaseURL}/repos/{owner}/{repo}/contents/{path}?ref={ref}`).
type GitHubFetcher struct {
	// BaseURL is the GitHub API root, defaulting to "https://api.github.com".
	BaseURL string
	// HTTPClient is the HTTP client used for API requests, defaulting to
	// http.DefaultClient.
	HTTPClient *http.Client
	// Token is an optional bearer token for authenticating to GitHub.
	Token string
	// AppID and AppPrivateKey configure GitHub App authentication when
	// Token and TokenFunc are unset.
	AppID         int64
	AppPrivateKey []byte
	// TokenFunc, when set, returns a bearer token for repoURL on demand.
	TokenFunc func(ctx context.Context, repoURL string) (string, error)

	mu         sync.Mutex
	instTokens map[string]cachedInstToken
}

type cachedInstToken struct {
	token     string
	expiresAt time.Time
}

// FetchFile fetches filePath at ref from repoURL via the GitHub Contents API.
func (f *GitHubFetcher) FetchFile(ctx context.Context, repoURL, ref, filePath string) ([]byte, error) {
	if hasEncodedPathHazard(filePath) {
		return nil, fmt.Errorf("invalid file path %q", filePath)
	}
	if hasDot, hasDotDot := hasDotSegment(filepath.ToSlash(filePath)); hasDot || hasDotDot {
		return nil, fmt.Errorf("invalid file path %q", filePath)
	}
	if ref != "" && (hasEncodedPathHazard(ref) || strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\r\n\\")) {
		return nil, fmt.Errorf("invalid git ref %q", ref)
	}
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, err
	}
	repoU, err := url.Parse(canonRepo)
	if err != nil {
		return nil, err
	}
	if f.BaseURL == "" && !strings.EqualFold(repoU.Hostname(), "github.com") {
		return nil, fmt.Errorf("github fetcher refuses non-github.com repository host %q", repoU.Hostname())
	}
	owner, repo, err := parseGitHubOwnerRepo(canonRepo)
	if err != nil {
		return nil, err
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	if cleanPath == "" || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return nil, fmt.Errorf("invalid file path %q", filePath)
	}

	base, err := f.validatedBaseURL()
	if err != nil {
		return nil, err
	}

	escapedSegs := strings.Split(cleanPath, "/")
	for i, seg := range escapedSegs {
		escapedSegs[i] = url.PathEscape(seg)
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/contents/%s",
		base, url.PathEscape(owner), url.PathEscape(repo), strings.Join(escapedSegs, "/"))
	if ref != "" {
		q := url.Values{}
		q.Set("ref", strings.TrimPrefix(ref, "refs/heads/"))
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.raw+json")

	tok := f.Token
	if f.TokenFunc != nil {
		t, err := f.TokenFunc(ctx, canonRepo)
		if err != nil {
			return nil, err
		}
		if t != "" {
			tok = t
		}
	} else if tok == "" && f.AppID > 0 && len(f.AppPrivateKey) > 0 {
		t, err := f.installationToken(ctx, base, owner, repo)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := f.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github contents API %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("file %q exceeds %d bytes", filePath, MaxFileBytes)
	}

	// If the server answered with a standard GitHub Contents JSON envelope
	// instead of raw bytes, decode its base64 content field.
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") &&
		!strings.Contains(resp.Header.Get("Content-Type"), "raw") {
		var envelope struct {
			Type     string `json:"type"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Encoding == "base64" {
			cleaned := strings.ReplaceAll(envelope.Content, "\n", "")
			cleaned = strings.ReplaceAll(cleaned, "\r", "")
			decoded, err := base64.StdEncoding.DecodeString(cleaned)
			if err != nil {
				return nil, fmt.Errorf("decoding base64 content for %q: %w", filePath, err)
			}
			if len(decoded) > MaxFileBytes {
				return nil, fmt.Errorf("file %q exceeds %d bytes", filePath, MaxFileBytes)
			}
			return decoded, nil
		}
	}
	return raw, nil
}

func (f *GitHubFetcher) validatedBaseURL() (string, error) {
	base := strings.TrimRight(strings.TrimSpace(f.BaseURL), "/")
	if base == "" {
		return "https://api.github.com", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid github BaseURL %q: %w", base, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("invalid github BaseURL scheme %q", u.Scheme)
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", fmt.Errorf("credentials not allowed in github BaseURL")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("missing host in github BaseURL")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
			(ip.To4() != nil && ip.To4()[0] == 169 && ip.To4()[1] == 254) {
			return "", fmt.Errorf("disallowed metadata/link-local IP %s in github BaseURL", host)
		}
	}
	return base, nil
}

func (f *GitHubFetcher) httpClient() *http.Client {
	noRedirect := func(*http.Request, []*http.Request) error {
		return errors.New("github fetcher refuses HTTP redirects")
	}
	if f.HTTPClient == nil {
		return &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: noRedirect,
		}
	}
	cp := *f.HTTPClient
	if cp.CheckRedirect == nil {
		cp.CheckRedirect = noRedirect
	}
	return &cp
}

func parseGitHubOwnerRepo(repoURL string) (owner, repo string, err error) {
	canon, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(canon)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !validRepoSegment(parts[0]) || !validRepoSegment(parts[1]) {
		return "", "", fmt.Errorf("cannot extract valid owner/repo from %q", repoURL)
	}
	return parts[0], parts[1], nil
}

func (f *GitHubFetcher) installationToken(ctx context.Context, base, owner, repo string) (string, error) {
	key := strings.ToLower(owner + "/" + repo)
	now := time.Now().UTC()

	f.mu.Lock()
	if f.instTokens == nil {
		f.instTokens = make(map[string]cachedInstToken)
	}
	if cached, ok := f.instTokens[key]; ok && cached.token != "" && now.Add(time.Minute).Before(cached.expiresAt) {
		f.mu.Unlock()
		return cached.token, nil
	}
	f.mu.Unlock()

	appJWT, err := mintGitHubAppJWT(f.AppID, f.AppPrivateKey, now)
	if err != nil {
		return "", err
	}

	client := f.httpClient()

	instEndpoint := fmt.Sprintf("%s/repos/%s/%s/installation", base, url.PathEscape(owner), url.PathEscape(repo))
	instReq, err := http.NewRequestWithContext(ctx, http.MethodGet, instEndpoint, nil)
	if err != nil {
		return "", err
	}
	instReq.Header.Set("Accept", "application/vnd.github+json")
	instReq.Header.Set("Authorization", "Bearer "+appJWT)
	instResp, err := client.Do(instReq)
	if err != nil {
		return "", err
	}
	defer instResp.Body.Close()
	if instResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(instResp.Body, 4096))
		return "", fmt.Errorf("github app installation lookup %s returned %d: %s", instEndpoint, instResp.StatusCode, strings.TrimSpace(string(b)))
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(instResp.Body, 1<<20)).Decode(&inst); err != nil || inst.ID <= 0 {
		return "", fmt.Errorf("decoding github app installation for %s/%s: %w", owner, repo, err)
	}

	tokEndpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", base, inst.ID)
	tokReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tokEndpoint, nil)
	if err != nil {
		return "", err
	}
	tokReq.Header.Set("Accept", "application/vnd.github+json")
	tokReq.Header.Set("Authorization", "Bearer "+appJWT)
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return "", err
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated && tokResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(tokResp.Body, 4096))
		return "", fmt.Errorf("github app token mint %s returned %d: %s", tokEndpoint, tokResp.StatusCode, strings.TrimSpace(string(b)))
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(tokResp.Body, 1<<20)).Decode(&minted); err != nil || minted.Token == "" {
		return "", fmt.Errorf("decoding github app installation token: %w", err)
	}
	exp := minted.ExpiresAt
	if exp.IsZero() {
		exp = now.Add(30 * time.Minute)
	}

	f.mu.Lock()
	f.instTokens[key] = cachedInstToken{token: minted.Token, expiresAt: exp}
	f.mu.Unlock()
	return minted.Token, nil
}

func mintGitHubAppJWT(appID int64, pemBytes []byte, now time.Time) (string, error) {
	priv, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return "", err
	}
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claimsBytes, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signingInput := hdr + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing github app JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app private key: invalid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app private key: %w", err)
	}
	rsaKey, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app private key: not an RSA key")
	}
	return rsaKey, nil
}

// MemoryFetcher is a thread-safe in-memory RepoFetcher for tests.
type MemoryFetcher struct {
	mu    sync.RWMutex
	files map[fileKey][]byte
}

type fileKey struct {
	repo string
	ref  string
	path string
}

// NewMemoryFetcher returns an initialized in-memory RepoFetcher.
func NewMemoryFetcher() *MemoryFetcher {
	return &MemoryFetcher{
		files: make(map[fileKey][]byte),
	}
}

// SetFile registers data at (repoURL, ref, filePath). If ref is empty, the file
// is served as a fallback for any ref in repoURL when no ref-specific entry
// matches.
func (m *MemoryFetcher) SetFile(repoURL, ref, filePath string, data []byte) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		canonRepo = strings.TrimSpace(repoURL)
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	normRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		m.files = make(map[fileKey][]byte)
	}
	cp := append([]byte(nil), data...)
	m.files[fileKey{repo: canonRepo, ref: normRef, path: cleanPath}] = cp
	if ref != normRef && ref != "" {
		m.files[fileKey{repo: canonRepo, ref: strings.TrimSpace(ref), path: cleanPath}] = cp
	}
}

// SetString registers UTF-8 string content at (repoURL, ref, filePath).
func (m *MemoryFetcher) SetString(repoURL, ref, filePath, content string) {
	m.SetFile(repoURL, ref, filePath, []byte(content))
}

// DeleteFile removes (repoURL, ref, filePath) from the in-memory store.
func (m *MemoryFetcher) DeleteFile(repoURL, ref, filePath string) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		canonRepo = strings.TrimSpace(repoURL)
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	normRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, fileKey{repo: canonRepo, ref: normRef, path: cleanPath})
	delete(m.files, fileKey{repo: canonRepo, ref: strings.TrimSpace(ref), path: cleanPath})
}

// FetchFile implements RepoFetcher.
func (m *MemoryFetcher) FetchFile(_ context.Context, repoURL, ref, filePath string) ([]byte, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		canonRepo = strings.TrimSpace(repoURL)
	}
	cleanPath := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	normRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidateRef := range []string{normRef, strings.TrimSpace(ref), "main", ""} {
		if data, ok := m.files[fileKey{repo: canonRepo, ref: candidateRef, path: cleanPath}]; ok {
			return append([]byte(nil), data...), nil
		}
	}
	return nil, os.ErrNotExist
}

// SignGitHubWebhook computes the X-Hub-Signature-256 header value
// ("sha256=<hex>") for body using secret.
func SignGitHubWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyGitHubSignature verifies the X-Hub-Signature-256 header against body
// using secret in constant time. An empty secret always fails closed.
func VerifyGitHubSignature(secret, signatureHeader string, body []byte) bool {
	if secret == "" {
		return false
	}
	sigHex, ok := strings.CutPrefix(strings.TrimSpace(signatureHeader), "sha256=")
	if !ok || sigHex == "" {
		return false
	}
	gotMAC, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(gotMAC, expectedMAC)
}

// Viewer describes what a signed-in human caller may see on GET /v1/grainfiles.
type Viewer struct {
	Email    string
	HubAdmin bool
	SeesRepo func(repo string) bool
}

// API serves the forge webhook ingestion endpoint (POST /v1/webhooks/github),
// the on-demand authorized content proxy (GET /v1/grainfiles/content), and the
// human read-only inspection endpoint (GET /v1/grainfiles).
type API struct {
	Store         *Store
	Fetcher       RepoFetcher
	WebhookSecret string
	// AllowUnsignedWebhooks, when true in isolated unit tests, permits
	// POST /v1/webhooks/github when WebhookSecret is empty. Production
	// deployments leave this false so unsigned webhooks fail closed (401).
	AllowUnsignedWebhooks bool

	// Authenticate, when set, verifies a bearer token and returns its node ID.
	Authenticate func(ctx context.Context, token string) (nodeID string, err error)
	// Directory, when set, verifies node tokens via VerifyNodeToken.
	Directory NodeTokenVerifier
	// AuthorizeOwnerRepo, when set, reports whether a node owner is in a team
	// that owns repoURL when authorizing non-recipient repo/folder/grainfile
	// subscriptions on GET /v1/grainfiles/content.
	AuthorizeOwnerRepo func(ctx context.Context, owner, repoURL string) bool
	// Access, when set, authenticates human callers for GET /v1/grainfiles
	// and scopes results to their teams and nodes.
	Access func(r *http.Request) (Viewer, error)
	// OnEvents, when set, is called after a webhook records one or more
	// discoverable grainfile events.
	OnEvents func(events []Event)
	// Logf, when set, logs non-fatal diagnostic messages.
	Logf func(format string, args ...any)

	mu         sync.RWMutex
	NodeTokens map[string]string
}

// NewAPI constructs an API bound to store and fetcher.
func NewAPI(store *Store, fetcher RepoFetcher) *API {
	return &API{
		Store:      store,
		Fetcher:    fetcher,
		NodeTokens: make(map[string]string),
	}
}

// SetNodeToken registers a bearer token for nodeID in memory (useful for tests
// when Directory is not configured).
func (a *API) SetNodeToken(token, nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.NodeTokens == nil {
		a.NodeTokens = make(map[string]string)
	}
	a.NodeTokens[token] = nodeID
}

// Register mounts the grainfile endpoints on mux.
func (a *API) Register(m *route.Mux) {
	m.HandleFunc("POST "+GitHubWebhookPath, a.serveGitHubWebhook)
	m.HandleFunc("GET "+ContentPath, a.serveContent)
	m.HandleFunc("GET "+GrainfilesPath, a.serveGrainfiles)
}

// Handler returns an http.Handler (and route.Lister) serving
// POST /v1/webhooks/github, GET /v1/grainfiles/content, and GET /v1/grainfiles.
func (a *API) Handler() http.Handler {
	m := route.NewMux()
	a.Register(m)
	return m
}

// ServeHTTP implements http.Handler directly on *API.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.Handler().ServeHTTP(w, r)
}

// WebhookResponse is the JSON response returned by POST /v1/webhooks/github.
type WebhookResponse struct {
	Source  string        `json:"source"`
	Repo    string        `json:"repo"`
	Ref     string        `json:"ref,omitempty"`
	Commit  string        `json:"commit,omitempty"`
	Indexed []IndexRecord `json:"indexed,omitempty"`
	Events  []Event       `json:"events,omitempty"`
}

type githubWebhookPayload struct {
	Event  string `json:"event"`
	Source string `json:"source"`
	Action string `json:"action"`
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
	After  string `json:"after"`
	Commit string `json:"commit"`
	SHA    string `json:"sha"`
	Repo   string `json:"repo"`
	Merged bool   `json:"merged"`

	Repository struct {
		HTMLURL  string `json:"html_url"`
		CloneURL string `json:"clone_url"`
		URL      string `json:"url"`
		FullName string `json:"full_name"`
	} `json:"repository"`

	HeadCommit *githubCommitPayload  `json:"head_commit"`
	Commits    []githubCommitPayload `json:"commits"`

	Added        []string        `json:"added"`
	Modified     []string        `json:"modified"`
	Removed      []string        `json:"removed"`
	ChangedPaths []string        `json:"changed_paths"`
	Files        json.RawMessage `json:"files"`

	PullRequest *struct {
		Number         int    `json:"number"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
		Added        []string `json:"added"`
		Modified     []string `json:"modified"`
		Removed      []string `json:"removed"`
		ChangedPaths []string `json:"changed_paths"`
	} `json:"pull_request"`
}

type githubCommitPayload struct {
	ID       string   `json:"id"`
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

type githubFileEntry struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Status   string `json:"status"`
}

func (a *API) serveGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		http.Error(w, "grainfiles store not configured", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		http.Error(w, "bad webhook body", http.StatusBadRequest)
		return
	}

	if a.WebhookSecret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !VerifyGitHubSignature(a.WebhookSecret, sig, body) {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
	} else if !a.AllowUnsignedWebhooks {
		http.Error(w, "webhook signature verification is not configured", http.StatusUnauthorized)
		return
	}

	eventHeader := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	if eventHeader == "ping" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "pong"})
		return
	}

	var pl githubWebhookPayload
	if err := json.Unmarshal(body, &pl); err != nil {
		http.Error(w, "malformed webhook JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	source := resolveWebhookSource(eventHeader, pl)
	if source != TriggerPush && source != TriggerPullRequest && source != TriggerMerge {
		http.Error(w, fmt.Sprintf("unsupported webhook event %q", source), http.StatusBadRequest)
		return
	}

	rawRepo := firstNonEmpty(
		pl.Repo,
		pl.Repository.HTMLURL,
		pl.Repository.CloneURL,
		pl.Repository.URL,
	)
	if rawRepo == "" && pl.Repository.FullName != "" {
		rawRepo = "https://github.com/" + strings.Trim(pl.Repository.FullName, "/")
	}
	canonRepo, err := normalizeSubRepoURL(rawRepo)
	if err != nil {
		http.Error(w, "invalid repository in webhook: "+err.Error(), http.StatusBadRequest)
		return
	}

	ref, commitSHA := extractRefAndCommit(source, pl)
	upsertPaths, removedPaths, allChangedPaths := extractWebhookPaths(pl)

	prevSeq, _ := a.Store.MaxEventSeq(r.Context())

	var indexed []IndexRecord
	for _, remPath := range removedPaths {
		if _, _, isGF, _ := ClassifyPath(remPath); isGF {
			if err := a.Store.RemoveGrainfile(r.Context(), canonRepo, remPath); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	webhookNow := a.Store.now()
	for _, p := range upsertPaths {
		_, _, isGF, _ := ClassifyPath(p)
		if !isGF {
			continue
		}
		if a.Fetcher == nil {
			http.Error(w, "repo fetcher not configured", http.StatusInternalServerError)
			return
		}
		fetchRef := firstNonEmpty(commitSHA, ref, "main")
		rawBytes, err := a.Fetcher.FetchFile(r.Context(), canonRepo, fetchRef, p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			http.Error(w, fmt.Sprintf("fetching %s: %v", p, err), http.StatusBadGateway)
			return
		}
		gf, err := Parse(p, rawBytes)
		if err != nil {
			http.Error(w, fmt.Sprintf("parsing %s: %v", p, err), http.StatusUnprocessableEntity)
			return
		}
		if err := gf.ResolveSource(Source{
			RepoURL: canonRepo,
			Ref:     ref,
			Commit:  commitSHA,
			Path:    p,
		}); err != nil {
			http.Error(w, fmt.Sprintf("resolving %s: %v", p, err), http.StatusUnprocessableEntity)
			return
		}
		rec, err := a.Store.indexGrainfileAt(r.Context(), gf, webhookNow)
		if err != nil {
			http.Error(w, fmt.Sprintf("indexing %s: %v", p, err), http.StatusInternalServerError)
			return
		}
		indexed = append(indexed, rec)
	}

	_, _ = a.Store.PromoteDeferred(r.Context(), a.Store.now())

	trigEvents, err := a.Store.EvaluateTriggers(r.Context(), canonRepo, commitSHA, ForgeEvent{
		Source:       source,
		Branch:       ref,
		ChangedPaths: allChangedPaths,
		At:           a.Store.now(),
	})
	if err != nil {
		http.Error(w, "evaluating triggers: "+err.Error(), http.StatusInternalServerError)
		return
	}

	emittedEvents := trigEvents
	if newEvents, err := a.Store.EventsSince(r.Context(), prevSeq); err == nil && len(newEvents) > 0 {
		emittedEvents = newEvents
	}
	if len(emittedEvents) > 0 && a.OnEvents != nil {
		a.OnEvents(emittedEvents)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(WebhookResponse{
		Source:  source,
		Repo:    canonRepo,
		Ref:     ref,
		Commit:  commitSHA,
		Indexed: indexed,
		Events:  emittedEvents,
	})
}

func resolveWebhookSource(header string, pl githubWebhookPayload) string {
	ev := strings.ToLower(firstNonEmpty(header, pl.Event, pl.Source))
	switch ev {
	case TriggerPush:
		return TriggerPush
	case TriggerMerge:
		return TriggerMerge
	case TriggerPullRequest:
		if pl.Merged || pl.Action == "merged" || (pl.PullRequest != nil && pl.PullRequest.Merged && (pl.Action == "closed" || pl.Action == "" || pl.Action == "merged")) {
			return TriggerMerge
		}
		return TriggerPullRequest
	case "":
		if pl.PullRequest != nil {
			if pl.PullRequest.Merged {
				return TriggerMerge
			}
			return TriggerPullRequest
		}
		return TriggerPush
	default:
		return ev
	}
}

func extractRefAndCommit(source string, pl githubWebhookPayload) (ref, commitSHA string) {
	ref = firstNonEmpty(pl.Ref, pl.Branch)
	commitSHA = firstNonEmpty(pl.Commit, pl.SHA, pl.After)
	if pl.HeadCommit != nil && commitSHA == "" {
		commitSHA = pl.HeadCommit.ID
	}
	if pl.PullRequest != nil {
		if ref == "" {
			ref = firstNonEmpty(pl.PullRequest.Base.Ref, pl.PullRequest.Head.Ref)
		}
		if commitSHA == "" {
			if source == TriggerMerge && pl.PullRequest.MergeCommitSHA != "" {
				commitSHA = pl.PullRequest.MergeCommitSHA
			} else {
				commitSHA = firstNonEmpty(pl.PullRequest.Head.SHA, pl.PullRequest.MergeCommitSHA)
			}
		}
	}
	if ref == "" {
		ref = "refs/heads/main"
	}
	if commitSHA == "" {
		commitSHA = normalizeURLRef(ref, "")
	}
	return ref, commitSHA
}

func extractWebhookPaths(pl githubWebhookPayload) (upsert, removed, allChanged []string) {
	state := map[string]string{} // path -> "upsert" | "removed"
	var order []string
	record := func(raw string, status string) {
		p := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(raw)), "/"))
		if p == "" || p == "." {
			return
		}
		if _, exists := state[p]; !exists {
			order = append(order, p)
		}
		state[p] = status
	}

	for _, c := range pl.Commits {
		for _, p := range c.Removed {
			record(p, "removed")
		}
		for _, p := range c.Added {
			record(p, "upsert")
		}
		for _, p := range c.Modified {
			record(p, "upsert")
		}
	}
	if pl.HeadCommit != nil {
		for _, p := range pl.HeadCommit.Removed {
			record(p, "removed")
		}
		for _, p := range pl.HeadCommit.Added {
			record(p, "upsert")
		}
		for _, p := range pl.HeadCommit.Modified {
			record(p, "upsert")
		}
	}
	if pl.PullRequest != nil {
		for _, p := range pl.PullRequest.Removed {
			record(p, "removed")
		}
		for _, p := range pl.PullRequest.Added {
			record(p, "upsert")
		}
		for _, p := range pl.PullRequest.Modified {
			record(p, "upsert")
		}
		for _, p := range pl.PullRequest.ChangedPaths {
			record(p, "upsert")
		}
	}
	for _, p := range pl.Removed {
		record(p, "removed")
	}
	for _, p := range pl.Added {
		record(p, "upsert")
	}
	for _, p := range pl.Modified {
		record(p, "upsert")
	}
	for _, p := range pl.ChangedPaths {
		record(p, "upsert")
	}

	if len(pl.Files) > 0 {
		var strFiles []string
		if err := json.Unmarshal(pl.Files, &strFiles); err == nil {
			for _, p := range strFiles {
				record(p, "upsert")
			}
		} else {
			var objFiles []githubFileEntry
			if err := json.Unmarshal(pl.Files, &objFiles); err == nil {
				for _, f := range objFiles {
					name := firstNonEmpty(f.Filename, f.Path)
					if strings.EqualFold(f.Status, "removed") {
						record(name, "removed")
					} else {
						record(name, "upsert")
					}
				}
			}
		}
	}

	for _, p := range order {
		allChanged = append(allChanged, p)
		if state[p] == "removed" {
			removed = append(removed, p)
		} else {
			upsert = append(upsert, p)
		}
	}
	return upsert, removed, allChanged
}

func (a *API) authenticateNode(r *http.Request) (string, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	tok, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || strings.TrimSpace(tok) == "" {
		return "", errors.New("missing bearer token")
	}
	tok = strings.TrimSpace(tok)

	if a.Authenticate != nil {
		nodeID, err := a.Authenticate(r.Context(), tok)
		if err != nil || strings.TrimSpace(nodeID) == "" {
			return "", errors.New("invalid node token")
		}
		return strings.TrimSpace(nodeID), nil
	}

	a.mu.RLock()
	nodeID, inMap := a.NodeTokens[tok]
	a.mu.RUnlock()
	if inMap && nodeID != "" {
		return nodeID, nil
	}

	if a.Directory != nil {
		_, node, err := a.Directory.VerifyNodeToken(r.Context(), tok)
		if err == nil && node != "" {
			return node, nil
		}
	}
	return "", errors.New("unknown or revoked node token")
}

func hasRawQueryEncodingHazard(rawQuery string) bool {
	lower := strings.ToLower(rawQuery)
	return strings.Contains(lower, "%2e") ||
		strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%00") ||
		strings.Contains(lower, "%252e") ||
		strings.Contains(lower, "%252f") ||
		strings.Contains(lower, "%255c")
}

func hasPathTraversalOrHazard(v string) bool {
	if v == "" {
		return false
	}
	if hasEncodedPathHazard(v) {
		return true
	}
	rest := v
	if idx := strings.IndexByte(rest, '#'); idx >= 0 {
		rest = rest[:idx]
	}
	if strings.HasPrefix(rest, "https://") {
		rest = rest[len("https://"):]
	} else if strings.HasPrefix(rest, "file://") {
		rest = strings.TrimLeft(rest[len("file://"):], "/")
	}
	if strings.Contains(rest, "//") || strings.Contains(rest, "@") {
		return true
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func (a *API) isNodeAuthorizedForRecord(ctx context.Context, sub Subscriptions, rec IndexRecord) bool {
	if !sub.MatchesRecord(rec) {
		return false
	}
	if a.AuthorizeOwnerRepo == nil {
		return true
	}
	// If the Grainfile names an explicit recipient and the node is subscribed to
	// that recipient, the Grainfile in rec.Repo is granting cross-deployment
	// capability access to that recipient (docs/grainfiles-spec.md §11.1).
	if rec.Recipient != "" {
		for _, r := range sub.Recipients {
			if strings.EqualFold(r, rec.Recipient) || IsSameRepo(r, rec.Recipient) {
				return true
			}
		}
	}
	// Otherwise the node matched via Repos, Folders, or Grainfiles subscription,
	// which requires the node's owner to have repository access to rec.Repo.
	return a.AuthorizeOwnerRepo(ctx, sub.Owner, rec.Repo)
}

func pinnedFetchRef(rec IndexRecord) string {
	return firstNonEmpty(rec.CommitSHA, rec.Ref)
}

func (a *API) serveContent(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil || a.Fetcher == nil {
		http.Error(w, "content proxy not configured", http.StatusInternalServerError)
		return
	}
	nodeID, err := a.authenticateNode(r)
	if err != nil {
		http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
		return
	}

	sub, err := a.Store.GetSubscriptions(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "forbidden: node has no registered subscriptions", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if hasRawQueryEncodingHazard(r.URL.RawQuery) {
		http.Error(w, "forbidden: encoded path traversal sequence in query", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	rawGrainfile := firstNonEmpty(q.Get("grainfile_url"), q.Get("grainfile"))
	rawURL := q.Get("url")
	rawRepo := q.Get("repo")
	rawCommit := firstNonEmpty(q.Get("commit"), q.Get("commit_sha"), q.Get("ref"))
	rawTarget := firstNonEmpty(q.Get("reference"), q.Get("path"), q.Get("target"))

	for _, param := range []string{rawGrainfile, rawURL, rawRepo, rawCommit, rawTarget} {
		if hasPathTraversalOrHazard(param) {
			http.Error(w, "forbidden: invalid path traversal or separator in content request", http.StatusForbidden)
			return
		}
	}

	// If `url` was passed without `grainfile_url`:
	// Check whether `url` is the Grainfile URL itself (or when `reference`/`path`
	// is also set, `url` is the authorizing Grainfile URL).
	if rawGrainfile == "" && rawURL != "" {
		if rawTarget != "" {
			rawGrainfile = rawURL
		} else if _, _, isGF, _ := ClassifyPath(rawURL); isGF {
			rawGrainfile = rawURL
		} else {
			rawTarget = rawURL
		}
	} else if rawGrainfile != "" && rawTarget == "" && rawURL != "" {
		rawTarget = rawURL
	}

	// Case 1: An explicit authorizing Grainfile was named (by URL or repo+grainfile path).
	if rawGrainfile != "" {
		rec, err := a.lookupGrainfileRecord(r.Context(), rawRepo, rawGrainfile, rawCommit)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				if a.grainfileExistsAtAnyRef(r.Context(), rawRepo, rawGrainfile) {
					http.Error(w, "forbidden: requested ref or commit does not match indexed grainfile", http.StatusForbidden)
					return
				}
				http.Error(w, "grainfile not found in index", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rawRepo != "" && !IsSameRepo(rawRepo, rec.Repo) {
			http.Error(w, "forbidden: repo does not match authorizing grainfile repository", http.StatusForbidden)
			return
		}
		if rawCommit != "" && !refMatchesIndexRecord(rawCommit, rec) {
			http.Error(w, "forbidden: requested ref or commit does not match indexed grainfile", http.StatusForbidden)
			return
		}
		if rec.Draft {
			http.Error(w, "forbidden: draft grainfile cannot authorize content access", http.StatusForbidden)
			return
		}
		if !a.isNodeAuthorizedForRecord(r.Context(), sub, rec) {
			http.Error(w, "forbidden: node is not recipient or subscriber of grainfile", http.StatusForbidden)
			return
		}

		// Subcase 1a: Fetching the .task.md or .process.md file itself.
		if rawTarget == "" || isSameSelfTarget(rec, rawTarget) {
			if strings.Contains(rawTarget, "://") {
				if parsedTarget, pErr := ParseRef(rawTarget); pErr != nil || !refMatchesIndexRecord(parsedTarget.GitRef, rec) {
					http.Error(w, "forbidden: target ref does not match indexed grainfile", http.StatusForbidden)
					return
				}
			}
			a.streamRepoFile(w, r, rec.Repo, pinnedFetchRef(rec), rec.Path)
			return
		}

		// Subcase 1b: Fetching a referenced file authorized by rec.
		targetRef, err := checkRepoOrRelativeRef(rec.ToGrainfile(), rec.Repo, rawTarget)
		if err != nil {
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}
		if !IsSameRepo(targetRef.Repo, rec.Repo) {
			http.Error(w, "forbidden: cross-origin reference not authorized by source repository", http.StatusForbidden)
			return
		}
		a.streamRepoFile(w, r, rec.Repo, pinnedFetchRef(rec), targetRef.Path)
		return
	}

	// Case 2: No explicit grainfile_url was provided; resolve from (repo, path) or target URL.
	canonRepo, targetPath, targetGitRef, err := resolveRepoAndPath(rawRepo, rawTarget)
	if err != nil {
		http.Error(w, "bad content request: "+err.Error(), http.StatusBadRequest)
		return
	}
	commit := firstNonEmpty(rawCommit, targetGitRef)

	// Subcase 2a: The requested path is itself an indexed .task.md or .process.md in canonRepo.
	if _, _, isGF, _ := ClassifyPath(targetPath); isGF {
		rec, err := a.Store.GetIndex(r.Context(), canonRepo, targetPath, commit)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				if commit != "" {
					if _, anyErr := a.Store.GetIndex(r.Context(), canonRepo, targetPath, ""); anyErr == nil {
						http.Error(w, "forbidden: requested ref or commit does not match indexed grainfile", http.StatusForbidden)
						return
					}
				}
				http.Error(w, "grainfile not found in index", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if rawCommit != "" && !refMatchesIndexRecord(rawCommit, rec) {
			http.Error(w, "forbidden: requested ref or commit does not match indexed grainfile", http.StatusForbidden)
			return
		}
		if targetGitRef != "" && !refMatchesIndexRecord(targetGitRef, rec) {
			http.Error(w, "forbidden: target ref does not match indexed grainfile", http.StatusForbidden)
			return
		}
		if rec.Draft {
			http.Error(w, "forbidden: draft grainfile cannot authorize content access", http.StatusForbidden)
			return
		}
		if !a.isNodeAuthorizedForRecord(r.Context(), sub, rec) {
			http.Error(w, "forbidden: node is not recipient or subscriber of grainfile", http.StatusForbidden)
			return
		}
		a.streamRepoFile(w, r, rec.Repo, pinnedFetchRef(rec), rec.Path)
		return
	}

	// Subcase 2b: The requested path is a non-Grainfile file in canonRepo.
	// Enforce the same-origin authority rule ("a repository can only grant access
	// to itself"): only a non-draft Grainfile indexed in canonRepo that the calling
	// node is authorized to receive can grant access to targetPath in canonRepo.
	records, err := a.Store.ListIndexByRepo(r.Context(), canonRepo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rootedTarget := "/" + strings.TrimPrefix(targetPath, "/")
	for _, rec := range records {
		if rec.Draft || !IsSameRepo(rec.Repo, canonRepo) {
			continue
		}
		if rawCommit != "" && !refMatchesIndexRecord(rawCommit, rec) {
			continue
		}
		if targetGitRef != "" && !refMatchesIndexRecord(targetGitRef, rec) {
			continue
		}
		if !a.isNodeAuthorizedForRecord(r.Context(), sub, rec) {
			continue
		}
		if targetRef, err := rec.ToGrainfile().CheckReferenceAccess(rec.Repo, rootedTarget); err == nil {
			a.streamRepoFile(w, r, rec.Repo, pinnedFetchRef(rec), targetRef.Path)
			return
		}
	}

	http.Error(w, "forbidden: path is not authorized by any grainfile in "+canonRepo, http.StatusForbidden)
}

func (a *API) grainfileExistsAtAnyRef(ctx context.Context, rawRepo, rawGrainfile string) bool {
	if strings.Contains(rawGrainfile, "://") {
		parsed, err := ParseRef(rawGrainfile)
		if err != nil || parsed.Repo == "" || parsed.Path == "" {
			return false
		}
		_, err = a.Store.GetIndex(ctx, parsed.Repo, parsed.Path, "")
		return err == nil
	}
	if rawRepo == "" {
		return false
	}
	_, err := a.Store.GetIndex(ctx, rawRepo, rawGrainfile, "")
	return err == nil
}

func (a *API) lookupGrainfileRecord(ctx context.Context, rawRepo, rawGrainfile, commit string) (IndexRecord, error) {
	if strings.Contains(rawGrainfile, "://") {
		return a.Store.GetIndexByURL(ctx, rawGrainfile, commit)
	}
	if rawRepo == "" {
		return IndexRecord{}, fmt.Errorf("repo is required when grainfile is a relative path")
	}
	return a.Store.GetIndex(ctx, rawRepo, rawGrainfile, commit)
}

func checkRepoOrRelativeRef(g *Grainfile, repoURL, rawTarget string) (Ref, error) {
	targetRef, err := g.CheckReferenceAccess(repoURL, rawTarget)
	if err == nil {
		return targetRef, nil
	}
	if !strings.Contains(rawTarget, "://") &&
		!strings.HasPrefix(rawTarget, "/") &&
		!strings.HasPrefix(rawTarget, "./") &&
		!strings.HasPrefix(rawTarget, "../") {
		if rootedRef, rErr := g.CheckReferenceAccess(repoURL, "/"+rawTarget); rErr == nil {
			return rootedRef, nil
		}
	}
	return Ref{}, err
}

func isSameSelfTarget(rec IndexRecord, rawTarget string) bool {
	if rawTarget == rec.URL {
		return true
	}
	if strings.Contains(rawTarget, "://") {
		return matchesGrainfileRef(rec, rawTarget)
	}
	cleanTarget := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(rawTarget), "/"))
	return cleanTarget == rec.Path
}

func resolveRepoAndPath(rawRepo, rawTarget string) (canonRepo, targetPath, gitRef string, err error) {
	if strings.Contains(rawTarget, "://") {
		r, err := ParseRef(rawTarget)
		if err != nil {
			return "", "", "", err
		}
		if r.Repo == "" || r.Path == "" {
			return "", "", "", fmt.Errorf("reference %q must specify a repository and file path", rawTarget)
		}
		if rawRepo != "" {
			explicitRepo, err := normalizeSubRepoURL(rawRepo)
			if err != nil {
				return "", "", "", err
			}
			if !IsSameRepo(explicitRepo, r.Repo) {
				return "", "", "", fmt.Errorf("cross-origin reference %q does not match repo %q", rawTarget, explicitRepo)
			}
		}
		cleanP := cleanPosixPath(strings.Trim(r.Path, "/"))
		return r.Repo, cleanP, r.GitRef, nil
	}
	if rawRepo == "" || rawTarget == "" {
		return "", "", "", fmt.Errorf("missing grainfile_url or repo and path query parameters")
	}
	canonRepo, err = normalizeSubRepoURL(rawRepo)
	if err != nil {
		return "", "", "", err
	}
	if idx := strings.IndexByte(rawTarget, '#'); idx >= 0 {
		rawTarget = rawTarget[:idx]
	}
	cleanP := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(rawTarget), "/"))
	if cleanP == "" || cleanP == "." || cleanP == ".." || strings.HasPrefix(cleanP, "../") {
		return "", "", "", fmt.Errorf("invalid file path %q", rawTarget)
	}
	return canonRepo, cleanP, "", nil
}

func (a *API) streamRepoFile(w http.ResponseWriter, r *http.Request, repoURL, ref, filePath string) {
	data, err := a.Fetcher.FetchFile(r.Context(), repoURL, ref, filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file not found in repository", http.StatusNotFound)
			return
		}
		http.Error(w, "fetching file from repository: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Grainfile-Repo", repoURL)
	w.Header().Set("X-Grainfile-Path", filePath)
	if ref != "" {
		w.Header().Set("X-Grainfile-Commit", ref)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// OverviewResponse is the JSON payload returned by GET /v1/grainfiles for
// human read-only inspection of indexed grainfiles, node subscriptions, and
// recent discoverable events.
type OverviewResponse struct {
	Grainfiles    []IndexRecord   `json:"grainfiles"`
	Subscriptions []Subscriptions `json:"subscriptions"`
	Events        []Event         `json:"events"`
}

// GrainfilesResponse is an alias for OverviewResponse.
type GrainfilesResponse = OverviewResponse

func (a *API) serveGrainfiles(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		http.Error(w, "grainfiles store not configured", http.StatusInternalServerError)
		return
	}
	var acc *Viewer
	if a.Access != nil {
		v, err := a.Access(r)
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		acc = &v
	}

	q := r.URL.Query()
	rawRepo := strings.TrimSpace(q.Get("repo"))
	rawRecipient := strings.TrimSpace(q.Get("recipient"))
	rawRequestor := strings.TrimSpace(q.Get("requestor"))
	rawKind := strings.TrimSpace(q.Get("kind"))
	rawNode := strings.TrimSpace(q.Get("node"))
	rawDraft := strings.ToLower(strings.TrimSpace(q.Get("draft")))
	rawGate := strings.ToLower(strings.TrimSpace(q.Get("gate")))
	rawURL := strings.TrimSpace(q.Get("url"))
	rawPath := strings.TrimSpace(q.Get("path"))
	includeDrafts := q.Get("include_drafts") != "false" && q.Get("include_drafts") != "0"

	var filterDraft *bool
	if rawDraft != "" {
		switch rawDraft {
		case "true", "1":
			t := true
			filterDraft = &t
			includeDrafts = true
		case "false", "0":
			f := false
			filterDraft = &f
		default:
			http.Error(w, fmt.Sprintf("invalid draft filter %q", rawDraft), http.StatusBadRequest)
			return
		}
	}

	var filterGate *bool
	if rawGate != "" {
		switch rawGate {
		case "true", "1":
			t := true
			filterGate = &t
		case "false", "0":
			f := false
			filterGate = &f
		default:
			http.Error(w, fmt.Sprintf("invalid gate filter %q", rawGate), http.StatusBadRequest)
			return
		}
	}

	var filterRequestor string
	if rawRequestor != "" {
		if IsUserIdentity(rawRequestor) {
			filterRequestor = rawRequestor
		} else if normReq, err := normalizeRecipientURL(rawRequestor); err == nil {
			filterRequestor = normReq
		} else {
			filterRequestor = rawRequestor
		}
	}

	var filterRepo string
	if rawRepo != "" {
		var err error
		filterRepo, err = normalizeSubRepoURL(rawRepo)
		if err != nil {
			http.Error(w, "invalid repo filter: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	var filterRecipient string
	if rawRecipient != "" {
		var err error
		filterRecipient, err = normalizeRecipientURL(rawRecipient)
		if err != nil {
			http.Error(w, "invalid recipient filter: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	records, err := a.Store.ListIndexByRepo(r.Context(), filterRepo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	subs, err := a.Store.ListSubscriptions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	events, err := a.Store.EventsSince(r.Context(), 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	visibleRecords := make([]IndexRecord, 0, len(records))
	for _, rec := range records {
		if !includeDrafts && rec.Draft {
			continue
		}
		if filterDraft != nil && rec.Draft != *filterDraft {
			continue
		}
		if filterGate != nil && rec.Gate != *filterGate {
			continue
		}
		if filterRecipient != "" && rec.Recipient != filterRecipient {
			continue
		}
		if filterRequestor != "" && rec.Requestor != filterRequestor && !strings.EqualFold(rec.Requestor, rawRequestor) {
			continue
		}
		if rawKind != "" && string(rec.Kind) != rawKind {
			continue
		}
		if rawURL != "" && !matchesGrainfileRef(rec, rawURL) && rec.URL != rawURL && rec.Path != rawURL {
			continue
		}
		if rawPath != "" && rec.Path != rawPath {
			continue
		}
		if !canSeeRecord(acc, rec) {
			continue
		}
		var subNodes []string
		for _, sub := range subs {
			if sub.MatchesRecord(rec) && canSeeSubscription(acc, sub) {
				subNodes = append(subNodes, sub.NodeID)
			}
		}
		slices.Sort(subNodes)
		rec.SubscribedNodes = slices.Compact(subNodes)

		var recEvents []Event
		for _, ev := range events {
			if IsSameRepo(ev.Repo, rec.Repo) && ev.Path == rec.Path && canSeeEvent(acc, ev) {
				recEvents = append(recEvents, ev)
			}
		}
		rec.RecentEvents = recEvents

		visibleRecords = append(visibleRecords, rec)
	}

	visibleSubs := make([]Subscriptions, 0, len(subs))
	for _, sub := range subs {
		if rawNode != "" && sub.NodeID != rawNode {
			continue
		}
		if filterRecipient != "" && !slices.Contains(sub.Recipients, filterRecipient) {
			continue
		}
		if filterRepo != "" && !subTouchesRepo(sub, filterRepo) {
			continue
		}
		if rawURL != "" {
			matched := false
			for _, rec := range visibleRecords {
				if sub.MatchesRecord(rec) {
					matched = true
					break
				}
			}
			if !matched {
				for _, gf := range sub.Grainfiles {
					if sameURLOrRepoPath(gf, rawURL) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		if !canSeeSubscription(acc, sub) {
			continue
		}
		visibleSubs = append(visibleSubs, sub)
	}

	visibleEvents := make([]Event, 0, len(events))
	for _, ev := range events {
		if filterRepo != "" && !IsSameRepo(ev.Repo, filterRepo) {
			continue
		}
		if filterRecipient != "" && ev.Recipient != filterRecipient {
			continue
		}
		if filterGate != nil && ev.Gate != *filterGate {
			continue
		}
		if filterRequestor != "" && ev.Requestor != "" && ev.Requestor != filterRequestor && !strings.EqualFold(ev.Requestor, rawRequestor) {
			continue
		}
		if rawKind != "" && string(ev.Kind) != rawKind {
			continue
		}
		if rawURL != "" && !sameURLOrRepoPath(ev.GrainfileURL, rawURL) && ev.GrainfileURL != rawURL && ev.Path != rawURL {
			continue
		}
		if rawPath != "" && ev.Path != rawPath {
			continue
		}
		if !canSeeEvent(acc, ev) {
			continue
		}
		visibleEvents = append(visibleEvents, ev)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(OverviewResponse{
		Grainfiles:    visibleRecords,
		Subscriptions: visibleSubs,
		Events:        visibleEvents,
	})
}

func canSeeRepoURL(acc *Viewer, rawURL string) bool {
	if acc == nil || acc.HubAdmin {
		return true
	}
	if acc.SeesRepo == nil {
		return false
	}
	owner, repo, err := parseGitHubOwnerRepo(rawURL)
	if err != nil {
		return false
	}
	return acc.SeesRepo(owner + "/" + repo)
}

func canSeeRecord(acc *Viewer, rec IndexRecord) bool {
	if acc == nil || acc.HubAdmin {
		return true
	}
	return canSeeRepoURL(acc, rec.Repo) || canSeeRepoURL(acc, rec.Recipient)
}

func canSeeEvent(acc *Viewer, ev Event) bool {
	if acc == nil || acc.HubAdmin {
		return true
	}
	return canSeeRepoURL(acc, ev.Repo) || canSeeRepoURL(acc, ev.Recipient)
}

func canSeeSubscription(acc *Viewer, sub Subscriptions) bool {
	if acc == nil || acc.HubAdmin {
		return true
	}
	if sub.Owner != "" && strings.EqualFold(sub.Owner, acc.Email) {
		return true
	}
	for _, u := range sub.Recipients {
		if canSeeRepoURL(acc, u) {
			return true
		}
	}
	for _, u := range sub.Repos {
		if canSeeRepoURL(acc, u) {
			return true
		}
	}
	for _, u := range sub.Folders {
		if canSeeRepoURL(acc, u) {
			return true
		}
	}
	for _, u := range sub.Grainfiles {
		if canSeeRepoURL(acc, u) {
			return true
		}
	}
	return false
}

func subTouchesRepo(sub Subscriptions, canonRepo string) bool {
	for _, r := range sub.Repos {
		if IsSameRepo(r, canonRepo) {
			return true
		}
	}
	for _, r := range sub.Recipients {
		if IsSameRepo(r, canonRepo) {
			return true
		}
	}
	for _, f := range sub.Folders {
		if parsed, err := ParseRef(f); err == nil && IsSameRepo(parsed.Repo, canonRepo) {
			return true
		}
	}
	for _, g := range sub.Grainfiles {
		if parsed, err := ParseRef(g); err == nil && IsSameRepo(parsed.Repo, canonRepo) {
			return true
		}
	}
	return false
}
