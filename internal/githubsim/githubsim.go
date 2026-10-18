// Package githubsim implements an in-process HTTP Git and GitHub simulator for
// end-to-end Grain Hub and Grainfile tests.
//
// Each repository created in the simulator is backed by a real bare git
// repository on disk (`git init --bare`) served over Smart HTTP via
// `git http-backend`, paired with a working tree for committing branches,
// opening pull requests, and merging pull requests.
//
// The simulator serves:
//   - Smart HTTP Git (`/git/{owner}/{repo}.git/...` via `git http-backend`)
//   - GitHub REST Contents API (`GET /repos/{owner}/{repo}/contents/{path}?ref={ref}`,
//     supporting both `Accept: application/vnd.github.raw+json` and the JSON
//     base64 envelope, read directly from `git show <ref>:<path>`)
//   - GitHub App installation lookup (`GET /repos/{owner}/{repo}/installation`)
//     and installation access token exchange (`POST /app/installations/{id}/access_tokens`)
//     verifying RS256 App JWTs, alongside static bearer tokens
//   - Repository and user metadata (`GET /user`, `GET /repos/{owner}/{repo}`,
//     at both `/` and `/api/v3/...`, matching `scripts/fake-github.py`)
//   - Automatic HMAC-SHA256 signed GitHub webhook emission (`X-Hub-Signature-256`,
//     `X-GitHub-Event: push | pull_request`) to a configured Grain Hub
//     `POST /v1/webhooks/github` endpoint whenever commits are pushed or pull
//     requests are opened or merged.
package githubsim

import (
	"bytes"
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
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
)

// Options configures a simulated Git + GitHub server.
type Options struct {
	// WebhookURL, when set, receives signed GitHub webhooks whenever commits
	// are pushed or pull requests are opened/merged.
	WebhookURL string
	// WebhookSecret signs emitted webhook payloads in X-Hub-Signature-256.
	WebhookSecret string
	// Token, when non-empty, is accepted as a static Bearer token on REST API
	// requests.
	Token string
	// AppID and AppPublicKey (or AppPrivateKey), when set, enable GitHub App
	// RS256 JWT verification on /repos/{owner}/{repo}/installation and
	// /app/installations/{id}/access_tokens.
	AppID         int64
	AppPrivateKey []byte
	AppPublicKey  *rsa.PublicKey
}

// Repo is a real git repository hosted by the simulator.
type Repo struct {
	Owner         string
	Name          string
	FullName      string
	HTMLURL       string
	DefaultBranch string
	BareDir       string
	WorkDir       string
	InstID        int64
}

// PullRequest is a pull request tracked by the simulator and backed by real
// commits in the bare git repository.
type PullRequest struct {
	Number         int      `json:"number"`
	Title          string   `json:"title"`
	BaseBranch     string   `json:"base_branch"`
	BaseSHA        string   `json:"base_sha"`
	HeadBranch     string   `json:"head_branch"`
	HeadSHA        string   `json:"head_sha"`
	Merged         bool     `json:"merged"`
	MergeCommitSHA string   `json:"merge_commit_sha,omitempty"`
	Added          []string `json:"added,omitempty"`
	Modified       []string `json:"modified,omitempty"`
	Removed        []string `json:"removed,omitempty"`
}

// WebhookDelivery records a webhook sent by the simulator to the Hub.
type WebhookDelivery struct {
	Event      string                      `json:"event"`
	Signature  string                      `json:"signature"`
	Payload    []byte                      `json:"payload"`
	StatusCode int                         `json:"status_code"`
	Response   *grainfiles.WebhookResponse `json:"response,omitempty"`
	RawBody    string                      `json:"raw_body,omitempty"`
}

// Server is an in-process HTTP Git and GitHub API simulator.
type Server struct {
	URL     string
	RootDir string

	srv            *httptest.Server
	gitHTTPBackend string

	mu            sync.Mutex
	opts          Options
	appPub        *rsa.PublicKey
	repos         map[string]*Repo
	prs           map[string]map[int]*PullRequest
	nextInstID    int64
	instByID      map[int64]string
	instTokens    map[string]time.Time
	deliveries    []WebhookDelivery
	mintedAppToks int
}

// GenerateAppKey generates a 2048-bit RSA keypair and PEM-encodes the private
// key for testing GitHub App authentication.
func GenerateAppKey(t testing.TB) (privPEM []byte, pub *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(priv)
	privPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: der,
	})
	return privPEM, &priv.PublicKey
}

// New starts a new Git + GitHub simulator server for t.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	var pub *rsa.PublicKey
	if opts.AppPublicKey != nil {
		pub = opts.AppPublicKey
	} else if len(opts.AppPrivateKey) > 0 {
		var err error
		pub, err = parseRSAPublicKeyFromPrivatePEM(opts.AppPrivateKey)
		if err != nil {
			t.Fatalf("parsing AppPrivateKey: %v", err)
		}
	}

	s := &Server{
		RootDir:        rootDir,
		gitHTTPBackend: findGitHTTPBackend(),
		opts:           opts,
		appPub:         pub,
		repos:          make(map[string]*Repo),
		prs:            make(map[string]map[int]*PullRequest),
		nextInstID:     1000,
		instByID:       make(map[int64]string),
		instTokens:     make(map[string]time.Time),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	s.URL = s.srv.URL
	t.Cleanup(func() { s.srv.Close() })
	return s
}

// Close stops the simulator's HTTP server.
func (s *Server) Close() {
	s.srv.Close()
}

// SetWebhook configures or updates the target Hub webhook URL and HMAC secret.
func (s *Server) SetWebhook(webhookURL, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.WebhookURL = webhookURL
	if secret != "" {
		s.opts.WebhookSecret = secret
	}
}

// Deliveries returns a copy of all webhook deliveries made by the simulator.
func (s *Server) Deliveries() []WebhookDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deliveries)
}

// MintedInstallationTokens returns how many GitHub App installation tokens have
// been minted via POST /app/installations/{id}/access_tokens.
func (s *Server) MintedInstallationTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintedAppToks
}

// GitHTTPURL returns the Smart HTTP clone/push URL for owner/name on this
// simulator (`http://127.0.0.1:PORT/git/{owner}/{name}.git`).
func (s *Server) GitHTTPURL(owner, name string) string {
	return fmt.Sprintf("%s/git/%s/%s.git", s.URL, owner, name)
}

// InitRepo initializes a real bare git repository and working tree for
// owner/name if it does not already exist.
func (s *Server) InitRepo(owner, name string) (*Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initRepoLocked(owner, name)
}

func (s *Server) initRepoLocked(owner, name string) (*Repo, error) {
	key := strings.ToLower(owner + "/" + name)
	if r, ok := s.repos[key]; ok {
		return r, nil
	}
	bareDir := filepath.Join(s.RootDir, "bare", owner, name+".git")
	workDir := filepath.Join(s.RootDir, "work", owner, name)
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}

	if err := runGit(bareDir, "init", "--bare", "--initial-branch=main"); err != nil {
		return nil, fmt.Errorf("git init --bare: %w", err)
	}
	_ = runGit(bareDir, "config", "http.receivepack", "true")
	_ = runGit(bareDir, "config", "uploadpack.allowReachableSHA1InWant", "true")

	if err := runGit(workDir, "init", "--initial-branch=main"); err != nil {
		return nil, fmt.Errorf("git init worktree: %w", err)
	}
	if err := runGit(workDir, "config", "user.name", "Grain Simulator"); err != nil {
		return nil, err
	}
	if err := runGit(workDir, "config", "user.email", "simulator@example.com"); err != nil {
		return nil, err
	}
	if err := runGit(workDir, "remote", "add", "origin", bareDir); err != nil {
		return nil, err
	}

	s.nextInstID++
	instID := s.nextInstID
	r := &Repo{
		Owner:         owner,
		Name:          name,
		FullName:      owner + "/" + name,
		HTMLURL:       "https://github.com/" + owner + "/" + name,
		DefaultBranch: "main",
		BareDir:       bareDir,
		WorkDir:       workDir,
		InstID:        instID,
	}
	s.repos[key] = r
	s.instByID[instID] = key
	if s.prs[key] == nil {
		s.prs[key] = make(map[int]*PullRequest)
	}
	return r, nil
}

// CommitAndPush creates a real git commit on branch in owner/name (adding or
// modifying files in upsertFiles and removing paths in removeFiles), pushes the
// commit to the bare git repository, and (when WebhookURL is set) emits an
// HMAC-SHA256 signed push webhook to the Hub.
func (s *Server) CommitAndPush(owner, name, branch, message string, upsertFiles map[string]string, removeFiles []string) (string, *grainfiles.WebhookResponse, error) {
	if branch == "" {
		branch = "main"
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")

	s.mu.Lock()
	repo, err := s.initRepoLocked(owner, name)
	if err != nil {
		s.mu.Unlock()
		return "", nil, err
	}

	// Check out or create the target branch in the working tree.
	hasCommits := runGit(repo.BareDir, "rev-parse", "--verify", "HEAD") == nil
	if hasCommits {
		_ = runGit(repo.WorkDir, "fetch", "origin")
		if runGit(repo.BareDir, "rev-parse", "--verify", "refs/heads/"+branch) == nil {
			if err := runGit(repo.WorkDir, "checkout", "-B", branch, "origin/"+branch); err != nil {
				s.mu.Unlock()
				return "", nil, fmt.Errorf("checkout existing branch %s: %w", branch, err)
			}
		} else {
			if err := runGit(repo.WorkDir, "checkout", "-B", branch); err != nil {
				s.mu.Unlock()
				return "", nil, fmt.Errorf("checkout new branch %s: %w", branch, err)
			}
		}
	} else {
		if err := runGit(repo.WorkDir, "checkout", "-B", branch); err != nil {
			s.mu.Unlock()
			return "", nil, fmt.Errorf("checkout initial branch %s: %w", branch, err)
		}
	}

	if err := applyWorktreeChanges(repo.WorkDir, upsertFiles, removeFiles); err != nil {
		s.mu.Unlock()
		return "", nil, err
	}
	if message == "" {
		message = "Update files on " + branch
	}
	if err := runGit(repo.WorkDir, "commit", "--allow-empty", "-m", message); err != nil {
		s.mu.Unlock()
		return "", nil, fmt.Errorf("git commit: %w", err)
	}
	if err := runGit(repo.WorkDir, "push", "-f", "origin", "HEAD:refs/heads/"+branch); err != nil {
		s.mu.Unlock()
		return "", nil, fmt.Errorf("git push: %w", err)
	}
	commitSHA, err := gitOutput(repo.BareDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		s.mu.Unlock()
		return "", nil, fmt.Errorf("git rev-parse: %w", err)
	}
	added, modified, removed, err := commitDiffStatus(repo.BareDir, commitSHA)
	if err != nil {
		s.mu.Unlock()
		return "", nil, err
	}
	whURL := s.opts.WebhookURL
	s.mu.Unlock()

	if whURL == "" {
		return commitSHA, nil, nil
	}
	payload := map[string]any{
		"ref":   "refs/heads/" + branch,
		"after": commitSHA,
		"repository": map[string]any{
			"full_name": repo.FullName,
			"html_url":  repo.HTMLURL,
			"clone_url": s.GitHTTPURL(owner, name),
		},
		"head_commit": map[string]any{
			"id":       commitSHA,
			"message":  message,
			"added":    added,
			"modified": modified,
			"removed":  removed,
		},
		"commits": []map[string]any{
			{
				"id":       commitSHA,
				"message":  message,
				"added":    added,
				"modified": modified,
				"removed":  removed,
			},
		},
	}
	whResp, code, raw, err := s.DeliverWebhook("push", payload)
	if err != nil {
		return commitSHA, nil, err
	}
	if code >= 400 {
		return commitSHA, whResp, fmt.Errorf("webhook push returned HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	return commitSHA, whResp, nil
}

// MustCommitAndPush calls CommitAndPush and fails t on error.
func (s *Server) MustCommitAndPush(t testing.TB, owner, name, branch, message string, upsertFiles map[string]string, removeFiles []string) (string, *grainfiles.WebhookResponse) {
	t.Helper()
	sha, resp, err := s.CommitAndPush(owner, name, branch, message, upsertFiles, removeFiles)
	if err != nil {
		t.Fatalf("CommitAndPush(%s/%s@%s): %v", owner, name, branch, err)
	}
	return sha, resp
}

// OpenPullRequest creates a commit on headBranch branched from baseBranch in
// owner/name, pushes refs/heads/<headBranch> and refs/pull/<number>/head to the
// bare repository, and emits a signed `pull_request` (action: "opened") webhook.
func (s *Server) OpenPullRequest(owner, name, baseBranch, headBranch, title string, upsertFiles map[string]string, removeFiles []string) (*PullRequest, *grainfiles.WebhookResponse, error) {
	if baseBranch == "" {
		baseBranch = "main"
	}
	baseBranch = strings.TrimPrefix(baseBranch, "refs/heads/")
	headBranch = strings.TrimPrefix(headBranch, "refs/heads/")
	if headBranch == "" {
		return nil, nil, errors.New("headBranch is required")
	}

	s.mu.Lock()
	repo, err := s.initRepoLocked(owner, name)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	_ = runGit(repo.WorkDir, "fetch", "origin")
	baseSHA, err := gitOutput(repo.BareDir, "rev-parse", "refs/heads/"+baseBranch)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("resolving base branch %s: %w", baseBranch, err)
	}
	if err := runGit(repo.WorkDir, "checkout", "-B", headBranch, baseSHA); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("checkout head branch %s from %s: %w", headBranch, baseBranch, err)
	}
	if err := applyWorktreeChanges(repo.WorkDir, upsertFiles, removeFiles); err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	if title == "" {
		title = "PR from " + headBranch + " into " + baseBranch
	}
	if err := runGit(repo.WorkDir, "commit", "--allow-empty", "-m", title); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("git commit PR head: %w", err)
	}
	if err := runGit(repo.WorkDir, "push", "-f", "origin", "HEAD:refs/heads/"+headBranch); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("git push PR head: %w", err)
	}
	headSHA, err := gitOutput(repo.BareDir, "rev-parse", "refs/heads/"+headBranch)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}

	added, modified, removed, err := rangeDiffStatus(repo.BareDir, baseSHA, headSHA)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}

	repoKey := strings.ToLower(owner + "/" + name)
	prNum := len(s.prs[repoKey]) + 1
	_ = runGit(repo.WorkDir, "push", "-f", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", prNum))

	pr := &PullRequest{
		Number:     prNum,
		Title:      title,
		BaseBranch: baseBranch,
		BaseSHA:    baseSHA,
		HeadBranch: headBranch,
		HeadSHA:    headSHA,
		Merged:     false,
		Added:      added,
		Modified:   modified,
		Removed:    removed,
	}
	s.prs[repoKey][prNum] = pr
	whURL := s.opts.WebhookURL
	s.mu.Unlock()

	if whURL == "" {
		return pr, nil, nil
	}
	payload := map[string]any{
		"action": "opened",
		"number": pr.Number,
		"merged": false,
		"repository": map[string]any{
			"full_name": repo.FullName,
			"html_url":  repo.HTMLURL,
			"clone_url": s.GitHTTPURL(owner, name),
		},
		"pull_request": map[string]any{
			"number": pr.Number,
			"title":  pr.Title,
			"merged": false,
			"head": map[string]any{
				"ref": pr.HeadBranch,
				"sha": pr.HeadSHA,
			},
			"base": map[string]any{
				"ref": pr.BaseBranch,
				"sha": pr.BaseSHA,
			},
			"added":    pr.Added,
			"modified": pr.Modified,
			"removed":  pr.Removed,
		},
	}
	whResp, code, raw, err := s.DeliverWebhook("pull_request", payload)
	if err != nil {
		return pr, nil, err
	}
	if code >= 400 {
		return pr, whResp, fmt.Errorf("webhook pull_request returned HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	return pr, whResp, nil
}

// MustOpenPullRequest calls OpenPullRequest and fails t on error.
func (s *Server) MustOpenPullRequest(t testing.TB, owner, name, baseBranch, headBranch, title string, upsertFiles map[string]string, removeFiles []string) (*PullRequest, *grainfiles.WebhookResponse) {
	t.Helper()
	pr, resp, err := s.OpenPullRequest(owner, name, baseBranch, headBranch, title, upsertFiles, removeFiles)
	if err != nil {
		t.Fatalf("OpenPullRequest(%s/%s %s->%s): %v", owner, name, headBranch, baseBranch, err)
	}
	return pr, resp
}

// MergePullRequest performs a real git merge (`git merge --no-ff`) of the pull
// request's head branch into its base branch, pushes the merge commit to the
// bare git repository, and emits a signed `pull_request` (action: "closed",
// merged: true) webhook to the Hub.
func (s *Server) MergePullRequest(owner, name string, number int) (*PullRequest, *grainfiles.WebhookResponse, error) {
	s.mu.Lock()
	repo, err := s.initRepoLocked(owner, name)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	repoKey := strings.ToLower(owner + "/" + name)
	pr, ok := s.prs[repoKey][number]
	if !ok {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("pull request #%d not found in %s/%s", number, owner, name)
	}
	if pr.Merged {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("pull request #%d already merged", number)
	}

	_ = runGit(repo.WorkDir, "fetch", "origin")
	if err := runGit(repo.WorkDir, "checkout", "-B", pr.BaseBranch, "origin/"+pr.BaseBranch); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("checkout base branch %s: %w", pr.BaseBranch, err)
	}
	mergeMsg := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pr.Number, pr.HeadBranch, pr.Title)
	if err := runGit(repo.WorkDir, "merge", "--no-ff", "-m", mergeMsg, "origin/"+pr.HeadBranch); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("git merge PR #%d: %w", pr.Number, err)
	}
	if err := runGit(repo.WorkDir, "push", "origin", "HEAD:refs/heads/"+pr.BaseBranch); err != nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("git push merged base branch %s: %w", pr.BaseBranch, err)
	}
	mergeSHA, err := gitOutput(repo.BareDir, "rev-parse", "refs/heads/"+pr.BaseBranch)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	pr.Merged = true
	pr.MergeCommitSHA = mergeSHA
	cp := *pr
	whURL := s.opts.WebhookURL
	s.mu.Unlock()

	if whURL == "" {
		return &cp, nil, nil
	}
	payload := map[string]any{
		"action": "closed",
		"number": cp.Number,
		"merged": true,
		"repository": map[string]any{
			"full_name": repo.FullName,
			"html_url":  repo.HTMLURL,
			"clone_url": s.GitHTTPURL(owner, name),
		},
		"pull_request": map[string]any{
			"number":           cp.Number,
			"title":            cp.Title,
			"merged":           true,
			"merge_commit_sha": cp.MergeCommitSHA,
			"head": map[string]any{
				"ref": cp.HeadBranch,
				"sha": cp.HeadSHA,
			},
			"base": map[string]any{
				"ref": cp.BaseBranch,
				"sha": cp.BaseSHA,
			},
			"added":    cp.Added,
			"modified": cp.Modified,
			"removed":  cp.Removed,
		},
	}
	whResp, code, raw, err := s.DeliverWebhook("pull_request", payload)
	if err != nil {
		return &cp, nil, err
	}
	if code >= 400 {
		return &cp, whResp, fmt.Errorf("webhook merge pull_request returned HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	return &cp, whResp, nil
}

// MustMergePullRequest calls MergePullRequest and fails t on error.
func (s *Server) MustMergePullRequest(t testing.TB, owner, name string, number int) (*PullRequest, *grainfiles.WebhookResponse) {
	t.Helper()
	pr, resp, err := s.MergePullRequest(owner, name, number)
	if err != nil {
		t.Fatalf("MergePullRequest(%s/%s #%d): %v", owner, name, number, err)
	}
	return pr, resp
}

// SignWebhook computes the GitHub `X-Hub-Signature-256` header (`sha256=<hex>`)
// for body using secret.
func SignWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// DeliverWebhook serializes payload as JSON, signs it with WebhookSecret, and
// POSTs it to WebhookURL with X-GitHub-Event: event.
func (s *Server) DeliverWebhook(event string, payload any) (*grainfiles.WebhookResponse, int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, err
	}
	s.mu.Lock()
	whURL := s.opts.WebhookURL
	secret := s.opts.WebhookSecret
	s.mu.Unlock()
	if whURL == "" {
		return nil, 0, nil, errors.New("githubsim: WebhookURL is not set")
	}

	sig := SignWebhook(secret, body)
	req, err := http.NewRequest(http.MethodPost, whURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)

	var parsed *grainfiles.WebhookResponse
	if resp.StatusCode == http.StatusOK && len(rawResp) > 0 {
		var wr grainfiles.WebhookResponse
		if json.Unmarshal(rawResp, &wr) == nil {
			parsed = &wr
		}
	}

	s.mu.Lock()
	s.deliveries = append(s.deliveries, WebhookDelivery{
		Event:      event,
		Signature:  sig,
		Payload:    body,
		StatusCode: resp.StatusCode,
		Response:   parsed,
		RawBody:    string(rawResp),
	})
	s.mu.Unlock()

	return parsed, resp.StatusCode, rawResp, nil
}

// ReadBlob reads a file's raw bytes and blob SHA directly from owner/name's
// bare git repository at ref (`git show <ref>:<filePath>`).
func (s *Server) ReadBlob(owner, name, ref, filePath string) ([]byte, string, error) {
	s.mu.Lock()
	repo, ok := s.repos[strings.ToLower(owner+"/"+name)]
	s.mu.Unlock()
	if !ok {
		return nil, "", os.ErrNotExist
	}
	return readGitBlob(repo.BareDir, ref, filePath)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Smart HTTP Git endpoints (`/git/{owner}/{repo}.git/...`).
	if strings.HasPrefix(r.URL.Path, "/git/") {
		s.serveGitHTTP(w, r)
		return
	}

	// 2. GitHub REST API endpoints (at `/` and `/api/v3/...`).
	reqPath := r.URL.Path
	if strings.HasPrefix(reqPath, "/api/v3/") {
		reqPath = strings.TrimPrefix(reqPath, "/api/v3")
	} else if reqPath == "/api/v3" {
		reqPath = "/"
	}

	if reqPath == "/user" {
		writeJSON(w, http.StatusOK, map[string]any{
			"login": "githubsim",
			"id":    1,
			"name":  "githubsim",
			"type":  "User",
		})
		return
	}

	segs := splitCleanSegments(reqPath)

	// POST /app/installations/{id}/access_tokens
	if len(segs) == 4 && segs[0] == "app" && segs[1] == "installations" && segs[3] == "access_tokens" && r.Method == http.MethodPost {
		s.serveInstallationTokenExchange(w, r, segs[2])
		return
	}

	if len(segs) >= 3 && segs[0] == "repos" {
		owner, repoName := segs[1], strings.TrimSuffix(segs[2], ".git")

		// GET /repos/{owner}/{repo}
		if len(segs) == 3 {
			writeJSON(w, http.StatusOK, map[string]any{
				"id":             1,
				"name":           repoName,
				"full_name":      owner + "/" + repoName,
				"owner":          map[string]any{"login": owner},
				"private":        false,
				"default_branch": "main",
				"permissions": map[string]any{
					"admin":    true,
					"maintain": true,
					"push":     true,
					"triage":   true,
					"pull":     true,
				},
			})
			return
		}

		// GET /repos/{owner}/{repo}/installation
		if len(segs) == 4 && segs[3] == "installation" && r.Method == http.MethodGet {
			s.serveRepoInstallation(w, r, owner, repoName)
			return
		}

		// GET /repos/{owner}/{repo}/contents/{path...}
		if len(segs) >= 5 && segs[3] == "contents" && r.Method == http.MethodGet {
			filePath := strings.Join(segs[4:], "/")
			s.serveRepoContents(w, r, owner, repoName, filePath)
			return
		}
	}

	http.NotFound(w, r)
}

func (s *Server) serveGitHTTP(w http.ResponseWriter, r *http.Request) {
	if s.gitHTTPBackend == "" {
		http.Error(w, "git-http-backend not found", http.StatusInternalServerError)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/git/")
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) >= 2 {
		owner := parts[0]
		repoName := strings.TrimSuffix(parts[1], ".git")
		_, _ = s.InitRepo(owner, repoName)
	}

	h := &cgi.Handler{
		Path: s.gitHTTPBackend,
		Dir:  filepath.Join(s.RootDir, "bare"),
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(s.RootDir, "bare"),
			"GIT_HTTP_EXPORT_ALL=1",
		},
		Root: "/git",
	}
	h.ServeHTTP(w, r)
}

func (s *Server) serveRepoInstallation(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	if err := s.verifyAppJWT(r); err != nil {
		http.Error(w, "unauthorized github app JWT: "+err.Error(), http.StatusUnauthorized)
		return
	}
	repo, err := s.InitRepo(owner, repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      repo.InstID,
		"app_id":  s.opts.AppID,
		"account": map[string]any{"login": owner},
	})
}

func (s *Server) serveInstallationTokenExchange(w http.ResponseWriter, r *http.Request, rawInstID string) {
	if err := s.verifyAppJWT(r); err != nil {
		http.Error(w, "unauthorized github app JWT: "+err.Error(), http.StatusUnauthorized)
		return
	}
	instID, err := strconv.ParseInt(rawInstID, 10, 64)
	if err != nil || instID <= 0 {
		http.Error(w, "invalid installation id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, known := s.instByID[instID]
	if !known {
		s.mu.Unlock()
		http.Error(w, "unknown installation", http.StatusNotFound)
		return
	}
	var randBytes [12]byte
	_, _ = rand.Read(randBytes[:])
	tok := "ghs_sim_" + hex.EncodeToString(randBytes[:])
	exp := time.Now().UTC().Add(time.Hour)
	s.instTokens[tok] = exp
	s.mintedAppToks++
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      tok,
		"expires_at": exp.Format(time.RFC3339),
	})
}

func (s *Server) serveRepoContents(w http.ResponseWriter, r *http.Request, owner, repoName, filePath string) {
	if !s.authorizeAPIRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	data, blobSHA, err := s.ReadBlob(owner, repoName, ref, filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/vnd.github.raw") {
		w.Header().Set("Content-Type", "application/vnd.github.raw+json; charset=utf-8")
		w.Header().Set("X-GitHub-Blob-SHA", blobSHA)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"name":     path.Base(filePath),
		"path":     filePath,
		"sha":      blobSHA,
		"size":     len(data),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(data),
	})
}

func (s *Server) authorizeAPIRequest(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	requireAuth := s.opts.Token != "" || s.opts.AppID > 0 || s.appPub != nil
	if !requireAuth {
		return true
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	var tok string
	if after, ok := strings.CutPrefix(authHeader, "Bearer "); ok {
		tok = strings.TrimSpace(after)
	} else if after, ok := strings.CutPrefix(authHeader, "token "); ok {
		tok = strings.TrimSpace(after)
	}
	if tok == "" {
		return false
	}
	if s.opts.Token != "" && tok == s.opts.Token {
		return true
	}
	if exp, ok := s.instTokens[tok]; ok && time.Now().UTC().Before(exp) {
		return true
	}
	return false
}

func (s *Server) verifyAppJWT(r *http.Request) error {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	jwtStr, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || strings.TrimSpace(jwtStr) == "" {
		return errors.New("missing Bearer JWT")
	}
	jwtStr = strings.TrimSpace(jwtStr)
	parts := strings.Split(jwtStr, ".")
	if len(parts) != 3 {
		return errors.New("malformed JWT")
	}
	s.mu.Lock()
	pub := s.appPub
	wantAppID := s.opts.AppID
	s.mu.Unlock()

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decoding JWT payload: %w", err)
	}
	var claims struct {
		Iss int64 `json:"iss"`
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return fmt.Errorf("parsing JWT claims: %w", err)
	}
	if wantAppID > 0 && claims.Iss != wantAppID {
		return fmt.Errorf("JWT iss = %d, want %d", claims.Iss, wantAppID)
	}
	if claims.Exp > 0 && time.Now().UTC().Unix() > claims.Exp {
		return errors.New("JWT expired")
	}
	if pub != nil {
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			return fmt.Errorf("decoding JWT signature: %w", err)
		}
		signingInput := parts[0] + "." + parts[1]
		digest := sha256.Sum256([]byte(signingInput))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
			return fmt.Errorf("invalid RS256 signature: %w", err)
		}
	}
	return nil
}

func parseRSAPublicKeyFromPrivatePEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &k.PublicKey, nil
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return &rsaKey.PublicKey, nil
}

func applyWorktreeChanges(workDir string, upsertFiles map[string]string, removeFiles []string) error {
	for _, rem := range removeFiles {
		clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(rem), "/"))
		if clean == "." || clean == "" || strings.HasPrefix(clean, "../") {
			continue
		}
		_ = runGit(workDir, "rm", "-f", "--ignore-unmatch", "--", clean)
		_ = os.Remove(filepath.Join(workDir, filepath.FromSlash(clean)))
	}

	keys := make([]string, 0, len(upsertFiles))
	for k := range upsertFiles {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, relPath := range keys {
		content := upsertFiles[relPath]
		clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(relPath), "/"))
		if clean == "." || clean == "" || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("invalid file path %q", relPath)
		}
		fullPath := filepath.Join(workDir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return err
		}
		if err := runGit(workDir, "add", "--", clean); err != nil {
			return fmt.Errorf("git add %s: %w", clean, err)
		}
	}
	return nil
}

func commitDiffStatus(bareDir, commitSHA string) (added, modified, removed []string, err error) {
	out, err := gitOutput(bareDir, "diff-tree", "--no-commit-id", "--name-status", "-r", "--root", commitSHA)
	if err != nil {
		return nil, nil, nil, err
	}
	a, m, r := splitNameStatus(out)
	return a, m, r, nil
}

func rangeDiffStatus(bareDir, baseSHA, headSHA string) (added, modified, removed []string, err error) {
	out, err := gitOutput(bareDir, "diff", "--name-status", "-r", baseSHA, headSHA)
	if err != nil {
		return nil, nil, nil, err
	}
	a, m, r := splitNameStatus(out)
	return a, m, r, nil
}

func splitNameStatus(out string) (added, modified, removed []string) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		filePath := fields[len(fields)-1]
		switch {
		case strings.HasPrefix(status, "A"):
			added = append(added, filePath)
		case strings.HasPrefix(status, "D"):
			removed = append(removed, filePath)
		default:
			modified = append(modified, filePath)
		}
	}
	return added, modified, removed
}

func readGitBlob(bareDir, ref, filePath string) ([]byte, string, error) {
	cleanPath := path.Clean(strings.TrimPrefix(filepath.ToSlash(filePath), "/"))
	if cleanPath == "" || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return nil, "", os.ErrNotExist
	}
	resolvedRef := resolveGitRef(bareDir, ref)
	spec := resolvedRef + ":" + cleanPath

	typ, err := gitOutput(bareDir, "cat-file", "-t", spec)
	if err != nil || typ != "blob" {
		return nil, "", os.ErrNotExist
	}
	blobSHA, err := gitOutput(bareDir, "rev-parse", spec)
	if err != nil {
		return nil, "", os.ErrNotExist
	}
	raw, err := gitRawOutput(bareDir, "show", spec)
	if err != nil {
		return nil, "", os.ErrNotExist
	}
	return raw, blobSHA, nil
}

func resolveGitRef(bareDir, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "HEAD"
	}
	for _, candidate := range []string{
		ref,
		"refs/heads/" + strings.TrimPrefix(ref, "refs/heads/"),
		"refs/tags/" + strings.TrimPrefix(ref, "refs/tags/"),
	} {
		if runGit(bareDir, "rev-parse", "--verify", candidate) == nil {
			return candidate
		}
	}
	return ref
}

func findGitHTTPBackend() string {
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, p := range []string{
		"/usr/lib/git-core/git-http-backend",
		"/usr/libexec/git-core/git-http-backend",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func runGit(dir string, args ...string) error {
	_, err := gitRawOutput(dir, args...)
	return err
}

func gitOutput(dir string, args ...string) (string, error) {
	b, err := gitRawOutput(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func gitRawOutput(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func splitCleanSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
