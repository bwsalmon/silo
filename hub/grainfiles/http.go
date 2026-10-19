package grainfiles

import (
	"archive/tar"
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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// TreeEntry describes a single regular file inside a fetched repository directory tree.
type TreeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	SHA  string `json:"sha,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// TreeListing is the JSON directory listing returned by GET /v1/grainfiles/content
// (and mux fetch_reference) when an authorized reference resolves to a folder/tree.
type TreeListing struct {
	Kind    string      `json:"kind"`
	Repo    string      `json:"repo"`
	Commit  string      `json:"commit,omitempty"`
	Path    string      `json:"path"`
	Entries []TreeEntry `json:"entries"`
}

// RepoFetcher fetches a file's raw bytes or lists Grainfile paths from a source
// git repository at a given ref or commit SHA on demand without storing
// repository contents at rest.
type RepoFetcher interface {
	FetchFile(ctx context.Context, repoURL, ref, filePath string) ([]byte, error)
	ListGrainfiles(ctx context.Context, repoURL, ref string) (paths []string, commitSHA string, err error)
}

// TreeFetcher is an optional extension of RepoFetcher that fetches a directory
// tree inside a repository at a given ref or commit SHA, rejecting symlinks
// and git submodules.
type TreeFetcher interface {
	FetchTree(ctx context.Context, repoURL, ref, dirPath string) (entries []TreeEntry, commitSHA string, err error)
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
	// instead of raw bytes, verify it is a regular file (`type: "file"`, not a
	// symlink or submodule) and decode its base64 content field.
	var envelope struct {
		Type            string `json:"type"`
		Encoding        string `json:"encoding"`
		Content         string `json:"content"`
		Target          string `json:"target"`
		SubmoduleGitURL string `json:"submodule_git_url"`
	}
	isJSONEnvelope := strings.Contains(resp.Header.Get("Content-Type"), "application/json") &&
		!strings.Contains(resp.Header.Get("Content-Type"), "raw")
	if isJSONEnvelope {
		if err := json.Unmarshal(raw, &envelope); err == nil {
			if (envelope.Type != "" && envelope.Type != "file") || envelope.Target != "" || envelope.SubmoduleGitURL != "" {
				objKind := firstNonEmpty(envelope.Type, "non-file")
				return nil, fmt.Errorf("github contents API %q is a %s git object, not a regular file", filePath, objKind)
			}
			if envelope.Encoding == "base64" {
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
	} else if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &envelope); err == nil {
			if envelope.Type == "symlink" || envelope.Type == "submodule" || envelope.SubmoduleGitURL != "" {
				objKind := firstNonEmpty(envelope.Type, "submodule")
				return nil, fmt.Errorf("github contents API %q is a %s git object, not a regular file", filePath, objKind)
			}
		}
	}
	return raw, nil
}

type githubTreeResponse struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		Size int64  `json:"size"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

func (f *GitHubFetcher) fetchGitTree(ctx context.Context, repoURL, ref string) (*githubTreeResponse, string, string, string, string, error) {
	if ref != "" && (hasEncodedPathHazard(ref) || strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\r\n\\")) {
		return nil, "", "", "", "", fmt.Errorf("invalid git ref %q", ref)
	}
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, "", "", "", "", err
	}
	repoU, err := url.Parse(canonRepo)
	if err != nil {
		return nil, "", "", "", "", err
	}
	if f.BaseURL == "" && !strings.EqualFold(repoU.Hostname(), "github.com") {
		return nil, "", "", "", "", fmt.Errorf("github fetcher refuses non-github.com repository host %q", repoU.Hostname())
	}
	owner, repo, err := parseGitHubOwnerRepo(canonRepo)
	if err != nil {
		return nil, "", "", "", "", err
	}
	base, err := f.validatedBaseURL()
	if err != nil {
		return nil, "", "", "", "", err
	}

	tok := f.Token
	if f.TokenFunc != nil {
		t, err := f.TokenFunc(ctx, canonRepo)
		if err != nil {
			return nil, "", "", "", "", err
		}
		if t != "" {
			tok = t
		}
	} else if tok == "" && f.AppID > 0 && len(f.AppPrivateKey) > 0 {
		t, err := f.installationToken(ctx, base, owner, repo)
		if err != nil {
			return nil, "", "", "", "", err
		}
		tok = t
	}

	cleanRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	if cleanRef == "" {
		cleanRef = "main"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1",
		base, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(cleanRef))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, "", "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, canonRepo, base, owner, repo, os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, canonRepo, base, owner, repo, fmt.Errorf("github git trees API %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed githubTreeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&parsed); err != nil {
		return nil, canonRepo, base, owner, repo, fmt.Errorf("decoding github git tree response: %w", err)
	}
	return &parsed, canonRepo, base, owner, repo, nil
}

// ListGrainfiles discovers all Grainfile paths (`**/*.task.grain.md` and
// `**/*.process.grain.md`) in repoURL at ref via the GitHub Git Trees API
// (`GET {BaseURL}/repos/{owner}/{repo}/git/trees/{ref}?recursive=1`).
func (f *GitHubFetcher) ListGrainfiles(ctx context.Context, repoURL, ref string) ([]string, string, error) {
	treeResp, _, _, _, _, err := f.fetchGitTree(ctx, repoURL, ref)
	if err != nil {
		return nil, "", err
	}
	cleanRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	commitSHA := firstNonEmpty(treeResp.SHA, cleanRef, "main")

	var paths []string
	seen := map[string]bool{}
	for _, entry := range treeResp.Tree {
		if entry.Mode == "120000" || entry.Mode == "160000" || entry.Type == "symlink" || entry.Type == "commit" || entry.Type == "submodule" || entry.Type == "tree" {
			continue
		}
		if entry.Type != "" && entry.Type != "blob" && entry.Type != "file" {
			continue
		}
		p := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(entry.Path), "/"))
		if p == "" || seen[p] {
			continue
		}
		if _, _, isGF, _ := ClassifyPath(p); isGF {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, commitSHA, nil
}

// FetchTree fetches all regular files under dirPath in repoURL at ref,
// rejecting symlinks (mode 120000 / type "symlink") and git submodules
// (mode 160000 / type "commit" or "submodule").
func (f *GitHubFetcher) FetchTree(ctx context.Context, repoURL, ref, dirPath string) ([]TreeEntry, string, error) {
	if hasEncodedPathHazard(dirPath) {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}
	if hasDot, hasDotDot := hasDotSegment(filepath.ToSlash(dirPath)); hasDot || hasDotDot {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}
	cleanDir := cleanPosixPath(strings.Trim(filepath.ToSlash(dirPath), "/"))
	if cleanDir == "." {
		cleanDir = ""
	}
	if cleanDir == ".." || strings.HasPrefix(cleanDir, "../") {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}

	treeResp, canonRepo, _, _, _, err := f.fetchGitTree(ctx, repoURL, ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f.fetchTreeViaContentsAPI(ctx, repoURL, ref, cleanDir)
		}
		return nil, "", err
	}
	cleanRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	commitSHA := firstNonEmpty(treeResp.SHA, cleanRef, "main")

	var matched []TreeEntry
	sawDir := cleanDir == ""
	for _, entry := range treeResp.Tree {
		p := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(entry.Path), "/"))
		if p == cleanDir {
			if entry.Mode == "120000" || entry.Type == "symlink" {
				return nil, "", fmt.Errorf("directory reference %q is a symlink git object", dirPath)
			}
			if entry.Mode == "160000" || entry.Type == "commit" || entry.Type == "submodule" {
				return nil, "", fmt.Errorf("directory reference %q is a submodule git object", dirPath)
			}
			sawDir = true
			continue
		}
		if cleanDir != "" && !strings.HasPrefix(p, cleanDir+"/") {
			continue
		}
		sawDir = true
		if entry.Mode == "120000" || entry.Type == "symlink" {
			return nil, "", fmt.Errorf("directory reference %q contains symlink git object %q", dirPath, p)
		}
		if entry.Mode == "160000" || entry.Type == "commit" || entry.Type == "submodule" {
			return nil, "", fmt.Errorf("directory reference %q contains submodule git object %q", dirPath, p)
		}
		if entry.Type == "tree" || entry.Type == "dir" {
			continue
		}
		if entry.Type != "" && entry.Type != "blob" && entry.Type != "file" {
			return nil, "", fmt.Errorf("directory reference %q contains unsupported git object %q (%s)", dirPath, p, entry.Type)
		}
		data, fErr := f.FetchFile(ctx, canonRepo, firstNonEmpty(ref, commitSHA), p)
		if fErr != nil {
			return nil, "", fErr
		}
		matched = append(matched, TreeEntry{
			Path: p,
			Size: int64(len(data)),
			SHA:  entry.SHA,
			Data: data,
		})
	}
	if !sawDir || len(matched) == 0 {
		return nil, "", os.ErrNotExist
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Path < matched[j].Path
	})
	return matched, commitSHA, nil
}

func (f *GitHubFetcher) fetchTreeViaContentsAPI(ctx context.Context, repoURL, ref, cleanDir string) ([]TreeEntry, string, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, "", err
	}
	owner, repo, err := parseGitHubOwnerRepo(canonRepo)
	if err != nil {
		return nil, "", err
	}
	base, err := f.validatedBaseURL()
	if err != nil {
		return nil, "", err
	}
	tok := f.Token
	if f.TokenFunc != nil {
		t, err := f.TokenFunc(ctx, canonRepo)
		if err != nil {
			return nil, "", err
		}
		if t != "" {
			tok = t
		}
	} else if tok == "" && f.AppID > 0 && len(f.AppPrivateKey) > 0 {
		t, err := f.installationToken(ctx, base, owner, repo)
		if err != nil {
			return nil, "", err
		}
		tok = t
	}

	var entries []TreeEntry
	var walk func(subDir string) error
	walk = func(subDir string) error {
		var endpoint string
		if subDir == "" {
			endpoint = fmt.Sprintf("%s/repos/%s/%s/contents", base, url.PathEscape(owner), url.PathEscape(repo))
		} else {
			segs := strings.Split(subDir, "/")
			for i, seg := range segs {
				segs[i] = url.PathEscape(seg)
			}
			endpoint = fmt.Sprintf("%s/repos/%s/%s/contents/%s", base, url.PathEscape(owner), url.PathEscape(repo), strings.Join(segs, "/"))
		}
		if ref != "" {
			q := url.Values{}
			q.Set("ref", strings.TrimPrefix(ref, "refs/heads/"))
			endpoint += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := f.httpClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return os.ErrNotExist
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("github contents API %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var items []struct {
			Type            string `json:"type"`
			Path            string `json:"path"`
			Name            string `json:"name"`
			Mode            string `json:"mode"`
			SHA             string `json:"sha"`
			Size            int64  `json:"size"`
			Target          string `json:"target"`
			SubmoduleGitURL string `json:"submodule_git_url"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&items); err != nil {
			return os.ErrNotExist
		}
		for _, item := range items {
			p := cleanPosixPath(strings.TrimPrefix(filepath.ToSlash(firstNonEmpty(item.Path, subDir+"/"+item.Name)), "/"))
			if item.Type == "symlink" || item.Mode == "120000" || item.Target != "" {
				return fmt.Errorf("directory reference %q contains symlink git object %q", cleanDir, p)
			}
			if item.Type == "submodule" || item.Type == "commit" || item.Mode == "160000" || item.SubmoduleGitURL != "" {
				return fmt.Errorf("directory reference %q contains submodule git object %q", cleanDir, p)
			}
			if item.Type == "dir" || item.Type == "tree" {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			if item.Type != "" && item.Type != "file" && item.Type != "blob" {
				return fmt.Errorf("directory reference %q contains unsupported git object %q (%s)", cleanDir, p, item.Type)
			}
			data, err := f.FetchFile(ctx, canonRepo, ref, p)
			if err != nil {
				return err
			}
			entries = append(entries, TreeEntry{
				Path: p,
				Size: int64(len(data)),
				SHA:  item.SHA,
				Data: data,
			})
		}
		return nil
	}
	if err := walk(cleanDir); err != nil {
		return nil, "", err
	}
	if len(entries) == 0 {
		return nil, "", os.ErrNotExist
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries, strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/"), nil
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
	modes map[fileKey]string
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
		modes: make(map[fileKey]string),
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
	if m.modes == nil {
		m.modes = make(map[fileKey]string)
	}
	cp := append([]byte(nil), data...)
	k1 := fileKey{repo: canonRepo, ref: normRef, path: cleanPath}
	m.files[k1] = cp
	delete(m.modes, k1)
	if ref != normRef && ref != "" {
		k2 := fileKey{repo: canonRepo, ref: strings.TrimSpace(ref), path: cleanPath}
		m.files[k2] = cp
		delete(m.modes, k2)
	}
}

// SetString registers UTF-8 string content at (repoURL, ref, filePath).
func (m *MemoryFetcher) SetString(repoURL, ref, filePath, content string) {
	m.SetFile(repoURL, ref, filePath, []byte(content))
}

// SetSymlink registers a git symlink (mode 120000) at (repoURL, ref, filePath)
// for testing symlink rejection in FetchFile and FetchTree.
func (m *MemoryFetcher) SetSymlink(repoURL, ref, filePath, target string) {
	m.setSpecialMode(repoURL, ref, filePath, "120000", []byte(target))
}

// SetSubmodule registers a git submodule (mode 160000) at (repoURL, ref, filePath)
// for testing submodule rejection in FetchFile and FetchTree.
func (m *MemoryFetcher) SetSubmodule(repoURL, ref, filePath, subURL string) {
	m.setSpecialMode(repoURL, ref, filePath, "160000", []byte(subURL))
}

func (m *MemoryFetcher) setSpecialMode(repoURL, ref, filePath, mode string, data []byte) {
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
	if m.modes == nil {
		m.modes = make(map[fileKey]string)
	}
	cp := append([]byte(nil), data...)
	k1 := fileKey{repo: canonRepo, ref: normRef, path: cleanPath}
	m.files[k1] = cp
	m.modes[k1] = mode
	if ref != normRef && ref != "" {
		k2 := fileKey{repo: canonRepo, ref: strings.TrimSpace(ref), path: cleanPath}
		m.files[k2] = cp
		m.modes[k2] = mode
	}
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
	k1 := fileKey{repo: canonRepo, ref: normRef, path: cleanPath}
	k2 := fileKey{repo: canonRepo, ref: strings.TrimSpace(ref), path: cleanPath}
	delete(m.files, k1)
	delete(m.files, k2)
	delete(m.modes, k1)
	delete(m.modes, k2)
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
		k := fileKey{repo: canonRepo, ref: candidateRef, path: cleanPath}
		if mode := m.modes[k]; mode == "120000" {
			return nil, fmt.Errorf("file %q is a symlink git object (mode 120000), not a regular file", filePath)
		} else if mode == "160000" {
			return nil, fmt.Errorf("file %q is a submodule git object (mode 160000), not a regular file", filePath)
		}
		if data, ok := m.files[k]; ok {
			return append([]byte(nil), data...), nil
		}
	}
	return nil, os.ErrNotExist
}

// ListGrainfiles implements RepoFetcher.
func (m *MemoryFetcher) ListGrainfiles(_ context.Context, repoURL, ref string) ([]string, string, error) {
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		canonRepo = strings.TrimSpace(repoURL)
	}
	normRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")

	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := map[string]bool{}
	var paths []string
	resolvedRef := normRef
	for _, candidateRef := range []string{normRef, strings.TrimSpace(ref), "main", ""} {
		for k := range m.files {
			if !strings.EqualFold(k.repo, canonRepo) || k.ref != candidateRef {
				continue
			}
			if m.modes[k] != "" {
				continue
			}
			if _, _, isGF, _ := ClassifyPath(k.path); isGF && !seen[k.path] {
				seen[k.path] = true
				paths = append(paths, k.path)
				if resolvedRef == "" && k.ref != "" {
					resolvedRef = k.ref
				}
			}
		}
		if len(paths) > 0 && candidateRef != "" {
			resolvedRef = candidateRef
			break
		}
	}
	sort.Strings(paths)
	if resolvedRef == "" {
		resolvedRef = "main"
	}
	return paths, resolvedRef, nil
}

// FetchTree implements TreeFetcher.
func (m *MemoryFetcher) FetchTree(_ context.Context, repoURL, ref, dirPath string) ([]TreeEntry, string, error) {
	if hasEncodedPathHazard(dirPath) {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}
	if hasDot, hasDotDot := hasDotSegment(filepath.ToSlash(dirPath)); hasDot || hasDotDot {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}
	cleanDir := cleanPosixPath(strings.Trim(filepath.ToSlash(dirPath), "/"))
	if cleanDir == "." {
		cleanDir = ""
	}
	if cleanDir == ".." || strings.HasPrefix(cleanDir, "../") {
		return nil, "", fmt.Errorf("invalid directory path %q", dirPath)
	}
	canonRepo, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		canonRepo = strings.TrimSpace(repoURL)
	}
	normRef := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")

	m.mu.RLock()
	defer m.mu.RUnlock()

	byPath := map[string]TreeEntry{}
	resolvedRef := normRef
	for _, candidateRef := range []string{normRef, strings.TrimSpace(ref), "main", ""} {
		for k, data := range m.files {
			if !strings.EqualFold(k.repo, canonRepo) || k.ref != candidateRef {
				continue
			}
			if k.path == cleanDir {
				if m.modes[k] == "120000" {
					return nil, "", fmt.Errorf("directory reference %q is a symlink git object", dirPath)
				}
				if m.modes[k] == "160000" {
					return nil, "", fmt.Errorf("directory reference %q is a submodule git object", dirPath)
				}
				continue
			}
			if cleanDir != "" && !strings.HasPrefix(k.path, cleanDir+"/") {
				continue
			}
			if m.modes[k] == "120000" {
				return nil, "", fmt.Errorf("directory reference %q contains symlink git object %q", dirPath, k.path)
			}
			if m.modes[k] == "160000" {
				return nil, "", fmt.Errorf("directory reference %q contains submodule git object %q", dirPath, k.path)
			}
			if _, exists := byPath[k.path]; !exists {
				cp := append([]byte(nil), data...)
				byPath[k.path] = TreeEntry{
					Path: k.path,
					Size: int64(len(cp)),
					Data: cp,
				}
				if resolvedRef == "" && k.ref != "" {
					resolvedRef = k.ref
				}
			}
		}
	}
	if len(byPath) == 0 {
		return nil, "", os.ErrNotExist
	}
	entries := make([]TreeEntry, 0, len(byPath))
	for _, e := range byPath {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	if resolvedRef == "" {
		resolvedRef = "main"
	}
	return entries, resolvedRef, nil
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

type githubRepoEntry struct {
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	URL           string `json:"url"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
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

	Repository githubRepoEntry `json:"repository"`

	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repositories      []githubRepoEntry `json:"repositories"`
	RepositoriesAdded []githubRepoEntry `json:"repositories_added"`

	HeadCommit *githubCommitPayload  `json:"head_commit"`
	Commits    []githubCommitPayload `json:"commits"`

	Added        []string        `json:"added"`
	Modified     []string        `json:"modified"`
	Removed      []string        `json:"removed"`
	ChangedPaths []string        `json:"changed_paths"`
	Files        json.RawMessage `json:"files"`

	Conclusion    string          `json:"conclusion"`
	WorkflowName  string          `json:"workflow_name"`
	WorkflowPath  string          `json:"workflow_path"`
	WorkflowEvent string          `json:"workflow_event"`
	WorkflowURL   string          `json:"workflow_url"`
	Workflow      json.RawMessage `json:"workflow"`

	WorkflowRun *struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Event      string `json:"event"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		URL        string `json:"url"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	} `json:"workflow_run"`

	CheckSuite *struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		URL        string `json:"url"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		App        *struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_suite"`

	CheckRun *struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		URL        string `json:"url"`
		HeadSHA    string `json:"head_sha"`
		CheckSuite *struct {
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
		} `json:"check_suite"`
	} `json:"check_run"`

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

// SyncRepository performs an initial tree scan of repoURL at ref (defaulting to
// refs/heads/main), discovering all existing **/*.grain.md files via
// Fetcher.ListGrainfiles, fetching and parsing each file, and indexing them in
// Store without waiting for a future commit to touch each file.
func (a *API) SyncRepository(ctx context.Context, repoURL, ref string) (*WebhookResponse, error) {
	if a.Store == nil {
		return nil, errors.New("grainfiles store not configured")
	}
	if a.Fetcher == nil {
		return nil, errors.New("repo fetcher not configured")
	}
	canonRepo, err := normalizeSubRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "refs/heads/main"
	} else if !strings.HasPrefix(ref, "refs/") && len(ref) != 40 {
		ref = "refs/heads/" + ref
	}

	prevSeq, _ := a.Store.MaxEventSeq(ctx)
	paths, treeSHA, err := a.Fetcher.ListGrainfiles(ctx, canonRepo, ref)
	if err != nil {
		return nil, err
	}
	commitSHA := firstNonEmpty(treeSHA, normalizeURLRef(ref, ""), "main")
	syncNow := a.Store.now()

	var indexed []IndexRecord
	for _, p := range paths {
		_, _, isGF, _ := ClassifyPath(p)
		if !isGF {
			continue
		}
		fetchRef := firstNonEmpty(treeSHA, ref, "main")
		rawBytes, err := a.Fetcher.FetchFile(ctx, canonRepo, fetchRef, p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("fetching %s: %w", p, err)
		}
		gf, err := Parse(p, rawBytes)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		if err := gf.ResolveSource(Source{
			RepoURL: canonRepo,
			Ref:     ref,
			Commit:  commitSHA,
			Path:    p,
		}); err != nil {
			return nil, fmt.Errorf("resolving %s: %w", p, err)
		}
		rec, err := a.Store.indexGrainfileAt(ctx, gf, syncNow)
		if err != nil {
			return nil, fmt.Errorf("indexing %s: %w", p, err)
		}
		indexed = append(indexed, rec)
	}

	_, _ = a.Store.PromoteDeferred(ctx, a.Store.now())
	emittedEvents, _ := a.Store.EventsSince(ctx, prevSeq)
	if len(emittedEvents) > 0 && a.OnEvents != nil {
		a.OnEvents(emittedEvents)
	}
	return &WebhookResponse{
		Source:  "installation",
		Repo:    canonRepo,
		Ref:     ref,
		Commit:  commitSHA,
		Indexed: indexed,
		Events:  emittedEvents,
	}, nil
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
	if source == "installation" || source == "installation_repositories" {
		a.serveInstallationWebhook(w, r, source, pl)
		return
	}
	if source != TriggerPush && source != TriggerPullRequest && source != TriggerMerge && source != TriggerWorkflow {
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
	isDefaultBranch := isDefaultOrProtectedBranch(ref, pl.Repository.DefaultBranch)

	prevSeq, _ := a.Store.MaxEventSeq(r.Context())

	var indexed []IndexRecord
	// Security invariant (§9, §11.1):
	//   - On pull_request and workflow events, never index or remove Grainfile
	//     definitions from the untrusted head; evaluate triggers against the
	//     indexed .process.grain.md definitions.
	//   - On push events to non-default/unprotected branches, never index or
	//     execute .task.grain.md files.
	if source != TriggerPullRequest && source != TriggerWorkflow {
		for _, remPath := range removedPaths {
			if kind, _, isGF, _ := ClassifyPath(remPath); isGF {
				if kind == KindTask && !isDefaultBranch {
					continue
				}
				if err := a.Store.RemoveGrainfile(r.Context(), canonRepo, remPath); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}

		webhookNow := a.Store.now()
		for _, p := range upsertPaths {
			kind, _, isGF, _ := ClassifyPath(p)
			if !isGF {
				continue
			}
			if kind == KindTask && !isDefaultBranch {
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
	}

	_, _ = a.Store.PromoteDeferred(r.Context(), a.Store.now())

	if a.Store.TeamResolver == nil {
		if tr, ok := a.Directory.(RepoTeamResolver); ok {
			a.Store.TeamResolver = tr
		}
	}
	prNum := 0
	if pl.PullRequest != nil {
		prNum = pl.PullRequest.Number
	}
	var wfName, wfPath, wfEvent, wfURL string
	if source == TriggerWorkflow {
		wfName, wfPath, wfEvent, wfURL = extractWorkflowInfo(pl)
	}
	trigEvents, err := a.Store.EvaluateForgeEvent(r.Context(), canonRepo, commitSHA, ForgeEvent{
		Source:        source,
		Repo:          canonRepo,
		Branch:        ref,
		CommitSHA:     commitSHA,
		PRNumber:      prNum,
		ChangedPaths:  allChangedPaths,
		WorkflowName:  wfName,
		WorkflowPath:  wfPath,
		WorkflowEvent: wfEvent,
		WorkflowURL:   wfURL,
		At:            a.Store.now(),
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

func (a *API) serveInstallationWebhook(w http.ResponseWriter, r *http.Request, source string, pl githubWebhookPayload) {
	action := strings.ToLower(strings.TrimSpace(pl.Action))
	if source == "installation" && action != "" && action != "created" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(WebhookResponse{Source: source})
		return
	}
	if source == "installation_repositories" && action != "" && action != "added" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(WebhookResponse{Source: source})
		return
	}

	var repos []githubRepoEntry
	repos = append(repos, pl.RepositoriesAdded...)
	repos = append(repos, pl.Repositories...)
	if len(repos) == 0 {
		rawSingle := firstNonEmpty(pl.Repo, pl.Repository.HTMLURL, pl.Repository.CloneURL, pl.Repository.URL, pl.Repository.FullName)
		if rawSingle != "" {
			repos = append(repos, githubRepoEntry{
				HTMLURL:       firstNonEmpty(pl.Repo, pl.Repository.HTMLURL),
				CloneURL:      pl.Repository.CloneURL,
				URL:           pl.Repository.URL,
				FullName:      pl.Repository.FullName,
				DefaultBranch: pl.Repository.DefaultBranch,
			})
		}
	}

	var (
		allIndexed []IndexRecord
		allEvents  []Event
		firstRepo  string
		firstRef   string
		firstSHA   string
	)
	for _, repo := range repos {
		rawRepo := firstNonEmpty(repo.HTMLURL, repo.CloneURL, repo.URL)
		if rawRepo == "" && repo.FullName != "" {
			rawRepo = "https://github.com/" + strings.Trim(repo.FullName, "/")
		}
		if rawRepo == "" {
			continue
		}
		ref := firstNonEmpty(pl.Ref, pl.Branch, repo.DefaultBranch, pl.Repository.DefaultBranch, "refs/heads/main")
		res, err := a.SyncRepository(r.Context(), rawRepo, ref)
		if err != nil {
			http.Error(w, fmt.Sprintf("syncing repository %s: %v", rawRepo, err), http.StatusBadGateway)
			return
		}
		if firstRepo == "" {
			firstRepo = res.Repo
			firstRef = res.Ref
			firstSHA = res.Commit
		}
		allIndexed = append(allIndexed, res.Indexed...)
		allEvents = append(allEvents, res.Events...)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(WebhookResponse{
		Source:  source,
		Repo:    firstRepo,
		Ref:     firstRef,
		Commit:  firstSHA,
		Indexed: allIndexed,
		Events:  allEvents,
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
	case TriggerWorkflow, "workflow_run", "check_suite", "check_run":
		return TriggerWorkflow
	case "":
		if pl.WorkflowRun != nil || pl.CheckSuite != nil || pl.CheckRun != nil || pl.WorkflowName != "" || len(pl.Workflow) > 0 {
			return TriggerWorkflow
		}
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

func extractWorkflowInfo(pl githubWebhookPayload) (wfName, wfPath, wfEvent, wfURL string) {
	var rawWf struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	var rawWfStr string
	if len(pl.Workflow) > 0 {
		if err := json.Unmarshal(pl.Workflow, &rawWfStr); err != nil {
			_ = json.Unmarshal(pl.Workflow, &rawWf)
		}
	}

	wfName = firstNonEmpty(pl.WorkflowName, rawWfStr, rawWf.Name)
	wfPath = firstNonEmpty(pl.WorkflowPath, rawWf.Path)
	wfURL = firstNonEmpty(pl.WorkflowURL, rawWf.HTMLURL, rawWf.URL)
	conclusion := pl.Conclusion

	if pl.WorkflowRun != nil {
		wfName = firstNonEmpty(wfName, pl.WorkflowRun.Name)
		wfPath = firstNonEmpty(wfPath, pl.WorkflowRun.Path)
		wfURL = firstNonEmpty(wfURL, pl.WorkflowRun.HTMLURL, pl.WorkflowRun.URL)
		conclusion = firstNonEmpty(conclusion, pl.WorkflowRun.Conclusion)
	}
	if pl.CheckRun != nil {
		wfName = firstNonEmpty(wfName, pl.CheckRun.Name)
		wfURL = firstNonEmpty(wfURL, pl.CheckRun.HTMLURL, pl.CheckRun.URL)
		conclusion = firstNonEmpty(conclusion, pl.CheckRun.Conclusion)
	}
	if pl.CheckSuite != nil {
		if wfName == "" && pl.CheckSuite.App != nil {
			wfName = firstNonEmpty(pl.CheckSuite.App.Name, pl.CheckSuite.App.Slug)
		}
		wfURL = firstNonEmpty(wfURL, pl.CheckSuite.HTMLURL, pl.CheckSuite.URL)
		conclusion = firstNonEmpty(conclusion, pl.CheckSuite.Conclusion)
	}

	if pl.WorkflowEvent != "" {
		if norm, ok := NormalizeWorkflowEvent(pl.WorkflowEvent); ok {
			wfEvent = norm
		}
	}
	if wfEvent == "" {
		action := strings.ToLower(strings.TrimSpace(pl.Action))
		conc := strings.ToLower(strings.TrimSpace(conclusion))
		switch action {
		case "requested", "in_progress", "triggered", "started":
			wfEvent = "triggered"
		case "succeeded", "success":
			wfEvent = "succeeded"
		case "failed", "failure":
			wfEvent = "failed"
		case "completed", "":
			switch conc {
			case "success", "succeeded":
				wfEvent = "succeeded"
			case "failure", "failed", "timed_out", "startup_failure":
				wfEvent = "failed"
			}
		}
	}
	return strings.TrimSpace(wfName), strings.TrimSpace(wfPath), wfEvent, strings.TrimSpace(wfURL)
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
	if pl.WorkflowRun != nil {
		if ref == "" {
			ref = pl.WorkflowRun.HeadBranch
		}
		if commitSHA == "" {
			commitSHA = pl.WorkflowRun.HeadSHA
		}
	}
	if pl.CheckSuite != nil {
		if ref == "" {
			ref = pl.CheckSuite.HeadBranch
		}
		if commitSHA == "" {
			commitSHA = pl.CheckSuite.HeadSHA
		}
	}
	if pl.CheckRun != nil {
		if ref == "" && pl.CheckRun.CheckSuite != nil {
			ref = pl.CheckRun.CheckSuite.HeadBranch
		}
		if commitSHA == "" {
			commitSHA = pl.CheckRun.HeadSHA
			if commitSHA == "" && pl.CheckRun.CheckSuite != nil {
				commitSHA = pl.CheckRun.CheckSuite.HeadSHA
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
	for _, part := range strings.Split(rawQuery, "&") {
		k, _, _ := strings.Cut(part, "=")
		lower := strings.ToLower(part)
		if strings.Contains(lower, "%00") {
			return true
		}
		if strings.HasPrefix(k, "var.") {
			continue
		}
		if strings.Contains(lower, "%2e") ||
			strings.Contains(lower, "%5c") ||
			strings.Contains(lower, "%252e") ||
			strings.Contains(lower, "%252f") ||
			strings.Contains(lower, "%255c") {
			return true
		}
	}
	return false
}

func extractQueryTemplateVars(q url.Values) map[string]string {
	var vars map[string]string
	for k, vals := range q {
		if name, ok := strings.CutPrefix(k, "var."); ok && name != "" && len(vals) > 0 {
			if vars == nil {
				vars = make(map[string]string)
			}
			vars[name] = vals[0]
		}
	}
	return vars
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

type repoOwnerDirectory interface {
	RepoOwner(ctx context.Context, repo string) (string, error)
}

func (a *API) isNodeAuthorizedForRecord(ctx context.Context, sub Subscriptions, rec IndexRecord) bool {
	if !sub.MatchesRecord(rec) {
		return false
	}
	authorize := a.AuthorizeOwnerRepo
	if authorize == nil && a.Store != nil {
		authorize = a.Store.AuthorizeOwnerRepo
	}
	if authorize == nil || sub.Owner == "" {
		return true
	}
	// If the Grainfile names a recipient and the node is subscribed to that
	// recipient, the Grainfile in rec.Repo is granting cross-deployment
	// capability access to that recipient, provided the node's owner is
	// admitted for the recipient repository (docs/grainfiles-spec.md §11.1, §11.2).
	if rec.Recipient != "" {
		if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
			return true
		}
		for _, r := range sub.Recipients {
			if sameRecipient(r, rec.Recipient) {
				if depID := DeploymentIDFromRecipient(rec.Recipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
					return true
				}
				recRepo, err := canonicalRepoFromAnyRef(rec.Recipient)
				if err != nil {
					return false
				}
				if dir, ok := a.Directory.(repoOwnerDirectory); ok {
					owner, repoName, pErr := parseGitHubOwnerRepo(recRepo)
					if pErr == nil {
						team, tErr := dir.RepoOwner(ctx, owner+"/"+repoName)
						if tErr != nil {
							return false
						}
						if team == "" {
							return true
						}
					}
				}
				return authorize(ctx, sub.Owner, recRepo)
			}
		}
	}
	// A .process.grain.md is a 1-to-many reusable/standing process that any
	// number of Grain deployments may subscribe to (by repo, enclosing folder, or
	// explicit .process.grain.md URL). When subscribed by explicit Grainfile URL
	// (or when rec.Repo has no exclusive team owner in Directory), allow access.
	if rec.Kind == KindProcess {
		for _, gf := range sub.Grainfiles {
			if matchesGrainfileRef(rec, gf) {
				return true
			}
		}
		if dir, ok := a.Directory.(repoOwnerDirectory); ok {
			owner, repoName, pErr := parseGitHubOwnerRepo(rec.Repo)
			if pErr == nil {
				team, tErr := dir.RepoOwner(ctx, owner+"/"+repoName)
				if tErr != nil {
					return false
				}
				if team == "" {
					return true
				}
			}
		}
	}
	// Otherwise the node matched via Repos, Folders, or Grainfiles subscription,
	// which requires the node's owner to have repository access to rec.Repo.
	return authorize(ctx, sub.Owner, rec.Repo)
}

func pinnedFetchRef(rec IndexRecord) string {
	return firstNonEmpty(rec.CommitSHA, rec.Ref)
}

func wantsTarFormat(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("format")), "tar") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/x-tar") || strings.Contains(accept, "application/tar")
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

		// Subcase 1b: Fetching a referenced file or directory tree authorized by rec.
		targetRef, err := checkRepoOrRelativeRef(rec.ToGrainfile(), rec.Repo, rawTarget)
		if err != nil {
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}
		if !IsSameRepo(targetRef.Repo, rec.Repo) {
			http.Error(w, "forbidden: cross-origin reference not authorized by source repository", http.StatusForbidden)
			return
		}
		a.streamRepoReference(w, r, rec.Repo, pinnedFetchRef(rec), targetRef, rawTarget)
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

	// Subcase 2b: The requested path is a non-Grainfile file or directory tree in canonRepo.
	// Enforce the same-origin authority rule ("a repository can only grant access
	// to itself"): only a non-draft Grainfile indexed in canonRepo that the calling
	// node is authorized to receive can grant access to targetPath in canonRepo.
	records, err := a.Store.ListIndexByRepo(r.Context(), canonRepo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rootedTarget := "/" + strings.TrimPrefix(targetPath, "/")
	if strings.HasSuffix(strings.TrimSpace(rawTarget), "/") || (strings.Contains(rawTarget, "://") && strings.Contains(rawTarget, "/tree/")) {
		if !strings.HasSuffix(rootedTarget, "/") {
			rootedTarget += "/"
		}
	}
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
			a.streamRepoReference(w, r, rec.Repo, pinnedFetchRef(rec), targetRef, rawTarget)
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
	q := r.URL.Query()
	vars := extractQueryTemplateVars(q)
	wantsRender := len(vars) > 0 || q.Get("render") == "true" || q.Get("render") == "1"
	if IsTemplatePath(filePath) && wantsRender {
		if vars == nil {
			vars = map[string]string{}
		}
		rendered, _, err := RenderTemplate(string(data), vars)
		if err != nil {
			http.Error(w, "rendering template: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		gf, err := ParseWithVars(filePath, data, vars)
		if err != nil {
			http.Error(w, "validating rendered template: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		urlRef := ref
		if a.Store != nil {
			if idxRec, gErr := a.Store.GetIndex(r.Context(), repoURL, filePath, ref); gErr == nil && idxRec.Ref != "" {
				urlRef = idxRec.Ref
			}
		}
		strippedPath, _ := StripTemplateSuffix(filePath)
		if err := gf.ResolveSource(Source{
			RepoURL: repoURL,
			Ref:     urlRef,
			Commit:  ref,
			Path:    strippedPath,
		}); err != nil {
			http.Error(w, "resolving rendered template: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		rendered = EnsureFromTemplate(rendered, gf.FromTemplate)
		data = []byte(rendered)
		w.Header().Set("X-Grainfile-Rendered", "true")
		if gf.FromTemplate != "" {
			w.Header().Set("X-Grainfile-From-Template", gf.FromTemplate)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Grainfile-Kind", "file")
	w.Header().Set("X-Grainfile-Repo", repoURL)
	w.Header().Set("X-Grainfile-Path", filePath)
	if ref != "" {
		w.Header().Set("X-Grainfile-Commit", ref)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *API) streamRepoReference(w http.ResponseWriter, r *http.Request, repoURL, ref string, targetRef Ref, rawTarget string) {
	isExplicitTree := targetRef.Mode == "tree" || targetRef.TrailingSlash || strings.HasSuffix(strings.TrimSpace(rawTarget), "/")
	if !isExplicitTree {
		data, err := a.Fetcher.FetchFile(r.Context(), repoURL, ref, targetRef.Path)
		if err == nil {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "sandbox")
			w.Header().Set("X-Grainfile-Kind", "file")
			w.Header().Set("X-Grainfile-Repo", repoURL)
			w.Header().Set("X-Grainfile-Path", targetRef.Path)
			if ref != "" {
				w.Header().Set("X-Grainfile-Commit", ref)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "fetching file from repository: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	entries, treeCommit, err := a.fetchRepoTree(r.Context(), repoURL, ref, targetRef.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file or directory not found in repository", http.StatusNotFound)
			return
		}
		http.Error(w, "fetching directory from repository: "+err.Error(), http.StatusBadGateway)
		return
	}
	commitHdr := firstNonEmpty(treeCommit, ref)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Grainfile-Kind", "tree")
	w.Header().Set("X-Grainfile-Repo", repoURL)
	w.Header().Set("X-Grainfile-Path", targetRef.Path)
	if commitHdr != "" {
		w.Header().Set("X-Grainfile-Commit", commitHdr)
	}

	if wantsTarFormat(r) {
		tarBytes, err := encodeTreeTar(targetRef.Path, entries)
		if err != nil {
			http.Error(w, "encoding tar stream: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(TreeListing{
		Kind:    "tree",
		Repo:    repoURL,
		Commit:  commitHdr,
		Path:    targetRef.Path,
		Entries: entries,
	})
}

func (a *API) fetchRepoTree(ctx context.Context, repoURL, ref, dirPath string) ([]TreeEntry, string, error) {
	if tf, ok := a.Fetcher.(TreeFetcher); ok {
		return tf.FetchTree(ctx, repoURL, ref, dirPath)
	}
	paths, commitSHA, err := a.Fetcher.ListGrainfiles(ctx, repoURL, ref)
	if err != nil {
		return nil, "", err
	}
	cleanDir := cleanPosixPath(strings.Trim(filepath.ToSlash(dirPath), "/"))
	if cleanDir == "." {
		cleanDir = ""
	}
	var entries []TreeEntry
	for _, p := range paths {
		if cleanDir != "" && p != cleanDir && !strings.HasPrefix(p, cleanDir+"/") {
			continue
		}
		data, err := a.Fetcher.FetchFile(ctx, repoURL, firstNonEmpty(commitSHA, ref), p)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, TreeEntry{
			Path: p,
			Size: int64(len(data)),
			Data: data,
		})
	}
	if len(entries) == 0 {
		return nil, "", os.ErrNotExist
	}
	return entries, firstNonEmpty(commitSHA, ref), nil
}

func encodeTreeTar(baseDir string, entries []TreeEntry) ([]byte, error) {
	cleanBase := cleanPosixPath(strings.Trim(filepath.ToSlash(baseDir), "/"))
	if cleanBase == "." {
		cleanBase = ""
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		name := e.Path
		if cleanBase != "" && strings.HasPrefix(name, cleanBase+"/") {
			name = strings.TrimPrefix(name, cleanBase+"/")
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(e.Data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FetchAuthorizedContent verifies that nodeID is subscribed and authorized to
// access grainfileURL at commitSHA (and optional reference), then fetches the
// content on demand without storing it at rest. It returns kind ("file" or
// "tree"), the resolved commit SHA, and the payload bytes (for a tree, a JSON
// TreeListing or a tar archive when format == "tar").
func (a *API) FetchAuthorizedContent(ctx context.Context, nodeID, grainfileURL, commitSHA, reference, format string) (kind, resolvedCommit string, data []byte, err error) {
	return a.FetchAuthorizedContentWithVars(ctx, nodeID, grainfileURL, commitSHA, reference, format, nil)
}

// FetchAuthorizedContentWithVars is like FetchAuthorizedContent, and when vars
// is non-empty and the target is a .jinja2 Grainfile template, renders the
// template with vars and validates the rendered Grainfile before returning it.
func (a *API) FetchAuthorizedContentWithVars(ctx context.Context, nodeID, grainfileURL, commitSHA, reference, format string, vars map[string]string) (kind, resolvedCommit string, data []byte, err error) {
	if a.Store == nil || a.Fetcher == nil {
		return "", "", nil, errors.New("content proxy not configured")
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return "", "", nil, fmt.Errorf("%w: empty node_id", ErrUnauthorized)
	}
	for _, param := range []string{grainfileURL, commitSHA, reference} {
		if hasPathTraversalOrHazard(param) {
			return "", "", nil, fmt.Errorf("%w: invalid path traversal or separator in content request", ErrUnauthorized)
		}
	}
	sub, err := a.Store.GetSubscriptions(ctx, nodeID)
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: node has no registered subscriptions", ErrUnauthorized)
	}
	rec, err := a.lookupGrainfileRecord(ctx, "", grainfileURL, commitSHA)
	if err != nil {
		if errors.Is(err, ErrNotFound) && a.grainfileExistsAtAnyRef(ctx, "", grainfileURL) {
			return "", "", nil, fmt.Errorf("%w: requested ref or commit does not match indexed grainfile", ErrUnauthorized)
		}
		return "", "", nil, err
	}
	if commitSHA != "" && !refMatchesIndexRecord(commitSHA, rec) {
		return "", "", nil, fmt.Errorf("%w: requested ref or commit does not match indexed grainfile", ErrUnauthorized)
	}
	if rec.Draft {
		return "", "", nil, fmt.Errorf("%w: draft grainfile cannot authorize content access", ErrUnauthorized)
	}
	if !a.isNodeAuthorizedForRecord(ctx, sub, rec) {
		return "", "", nil, fmt.Errorf("%w: node is not recipient or subscriber of grainfile", ErrUnauthorized)
	}

	pinnedRef := pinnedFetchRef(rec)
	if reference == "" || isSameSelfTarget(rec, reference) {
		if strings.Contains(reference, "://") {
			if parsedTarget, pErr := ParseRef(reference); pErr != nil || !refMatchesIndexRecord(parsedTarget.GitRef, rec) {
				return "", "", nil, fmt.Errorf("%w: target ref does not match indexed grainfile", ErrUnauthorized)
			}
		}
		raw, err := a.Fetcher.FetchFile(ctx, rec.Repo, pinnedRef, rec.Path)
		if err != nil {
			return "", "", nil, err
		}
		if IsTemplatePath(rec.Path) && len(vars) > 0 {
			rendered, _, err := RenderTemplate(string(raw), vars)
			if err != nil {
				return "", "", nil, err
			}
			gf, err := ParseWithVars(rec.Path, raw, vars)
			if err != nil {
				return "", "", nil, err
			}
			strippedPath, _ := StripTemplateSuffix(rec.Path)
			if err := gf.ResolveSource(Source{
				RepoURL: rec.Repo,
				Ref:     rec.Ref,
				Commit:  pinnedRef,
				Path:    strippedPath,
			}); err != nil {
				return "", "", nil, err
			}
			rendered = EnsureFromTemplate(rendered, gf.FromTemplate)
			raw = []byte(rendered)
		}
		return "file", pinnedRef, raw, nil
	}

	targetRef, err := checkRepoOrRelativeRef(rec.ToGrainfile(), rec.Repo, reference)
	if err != nil {
		return "", "", nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !IsSameRepo(targetRef.Repo, rec.Repo) {
		return "", "", nil, fmt.Errorf("%w: cross-origin reference not authorized by source repository", ErrUnauthorized)
	}

	isExplicitTree := targetRef.Mode == "tree" || targetRef.TrailingSlash || strings.HasSuffix(strings.TrimSpace(reference), "/")
	if !isExplicitTree {
		raw, fErr := a.Fetcher.FetchFile(ctx, rec.Repo, pinnedRef, targetRef.Path)
		if fErr == nil {
			if IsTemplatePath(targetRef.Path) && len(vars) > 0 {
				rendered, _, err := RenderTemplate(string(raw), vars)
				if err != nil {
					return "", "", nil, err
				}
				gf, err := ParseWithVars(targetRef.Path, raw, vars)
				if err != nil {
					return "", "", nil, err
				}
				strippedPath, _ := StripTemplateSuffix(targetRef.Path)
				if err := gf.ResolveSource(Source{
					RepoURL: rec.Repo,
					Ref:     rec.Ref,
					Commit:  pinnedRef,
					Path:    strippedPath,
				}); err != nil {
					return "", "", nil, err
				}
				rendered = EnsureFromTemplate(rendered, gf.FromTemplate)
				raw = []byte(rendered)
			}
			return "file", pinnedRef, raw, nil
		}
		if !errors.Is(fErr, os.ErrNotExist) {
			return "", "", nil, fErr
		}
	}

	entries, treeCommit, tErr := a.fetchRepoTree(ctx, rec.Repo, pinnedRef, targetRef.Path)
	if tErr != nil {
		return "", "", nil, tErr
	}
	commitOut := firstNonEmpty(treeCommit, pinnedRef)
	if strings.EqualFold(strings.TrimSpace(format), "tar") {
		tarBytes, err := encodeTreeTar(targetRef.Path, entries)
		if err != nil {
			return "", "", nil, err
		}
		return "tree", commitOut, tarBytes, nil
	}
	listingBytes, err := json.Marshal(TreeListing{
		Kind:    "tree",
		Repo:    rec.Repo,
		Commit:  commitOut,
		Path:    targetRef.Path,
		Entries: entries,
	})
	if err != nil {
		return "", "", nil, err
	}
	return "tree", commitOut, listingBytes, nil
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
			if strings.Contains(err.Error(), "not on the hub accepted users list") {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
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

	visibleSubsAll := make([]Subscriptions, 0, len(subs))
	for _, sub := range subs {
		if canSeeSubscription(acc, sub) {
			visibleSubsAll = append(visibleSubsAll, sub)
		}
	}
	eventsByRepoPath := make(map[string][]Event, len(records))
	for _, ev := range events {
		if canSeeEvent(acc, ev) {
			key := strings.ToLower(strings.TrimSuffix(ev.Repo, "/")) + "\x00" + ev.Path
			eventsByRepoPath[key] = append(eventsByRepoPath[key], ev)
		}
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
		if filterRecipient != "" && !sameRecipient(rec.Recipient, filterRecipient) {
			continue
		}
		if filterRequestor != "" && !sameRecipient(rec.Requestor, filterRequestor) && rec.Requestor != filterRequestor && !strings.EqualFold(rec.Requestor, rawRequestor) {
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
		for _, sub := range visibleSubsAll {
			if sub.MatchesRecord(rec) {
				subNodes = append(subNodes, sub.NodeID)
			}
		}
		slices.Sort(subNodes)
		rec.SubscribedNodes = slices.Compact(subNodes)

		key := strings.ToLower(strings.TrimSuffix(rec.Repo, "/")) + "\x00" + rec.Path
		rec.RecentEvents = eventsByRepoPath[key]

		visibleRecords = append(visibleRecords, rec)
	}

	visibleSubs := make([]Subscriptions, 0, len(subs))
	for _, sub := range subs {
		if rawNode != "" && sub.NodeID != rawNode {
			continue
		}
		if filterRecipient != "" {
			matchedRec := false
			if depID := DeploymentIDFromRecipient(filterRecipient); depID != "" && strings.EqualFold(depID, sub.NodeID) {
				matchedRec = true
			}
			for _, r := range sub.Recipients {
				if sameRecipient(r, filterRecipient) {
					matchedRec = true
					break
				}
			}
			if !matchedRec {
				continue
			}
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
		if filterRecipient != "" && !sameRecipient(ev.Recipient, filterRecipient) {
			continue
		}
		if filterGate != nil && ev.Gate != *filterGate {
			continue
		}
		if filterRequestor != "" && ev.Requestor != "" && !sameRecipient(ev.Requestor, filterRequestor) && ev.Requestor != filterRequestor && !strings.EqualFold(ev.Requestor, rawRequestor) {
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
