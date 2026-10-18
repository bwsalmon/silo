package githubsim

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/hub/grainfiles"
)

func TestGitHubSimGitHTTPContentsAppTokensAndWebhooks(t *testing.T) {
	privPEM, pub := GenerateAppKey(t)

	const (
		webhookSecret = "sim-webhook-secret"
		bearerToken   = "ghp_sim_bearer_token"
		appID         = int64(777)
	)

	type recordedHook struct {
		event     string
		signature string
		body      []byte
	}
	var hooks []recordedHook
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hooks = append(hooks, recordedHook{
			event:     r.Header.Get("X-GitHub-Event"),
			signature: r.Header.Get("X-Hub-Signature-256"),
			body:      body,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"indexed":[{"repo":"acme/platform","path":"tasks/migrate.task.md"}]}`))
	}))
	defer hookSrv.Close()

	sim := New(t, Options{
		WebhookURL:    hookSrv.URL,
		WebhookSecret: webhookSecret,
		Token:         bearerToken,
		AppID:         appID,
		AppPublicKey:  pub,
	})

	// 1. CommitAndPush creates bare repo, working tree commit, pushes to bare repo, and emits signed push webhook.
	sha1, whResp1, err := sim.CommitAndPush("acme", "platform", "main", "initial grainfiles and docs", map[string]string{
		"tasks/migrate.task.md": "---\nrecipient: https://grain-a.example.com\n---\nMigrate DB.\n",
		"docs/spec.md":          "# Spec v1\nDetails here.\n",
	}, nil)
	if err != nil {
		t.Fatalf("CommitAndPush 1: %v", err)
	}
	if len(sha1) != 40 {
		t.Fatalf("expected 40-char commit SHA, got %q", sha1)
	}
	if whResp1 == nil || len(whResp1.Indexed) != 1 {
		t.Fatalf("expected parsed webhook response len(Indexed)=1, got %+v", whResp1)
	}
	if len(hooks) != 1 || hooks[0].event != "push" {
		t.Fatalf("expected 1 push webhook, got %+v", hooks)
	}
	if wantSig := SignWebhook(webhookSecret, hooks[0].body); hooks[0].signature != wantSig {
		t.Fatalf("webhook signature = %q, want %q", hooks[0].signature, wantSig)
	}

	// 2. Clone over Smart HTTP (`git http-backend`), commit locally, and push back over HTTP.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	gitURL := sim.GitHTTPURL("acme", "platform")
	runCmd := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runCmd(t.TempDir(), "clone", gitURL, cloneDir)
	specBytes, err := os.ReadFile(filepath.Join(cloneDir, "docs", "spec.md"))
	if err != nil || string(specBytes) != "# Spec v1\nDetails here.\n" {
		t.Fatalf("unexpected cloned docs/spec.md: %q (err=%v)", string(specBytes), err)
	}
	runCmd(cloneDir, "config", "user.name", "Clone User")
	runCmd(cloneDir, "config", "user.email", "clone@example.com")
	if err := os.WriteFile(filepath.Join(cloneDir, "docs", "from-http.md"), []byte("pushed over git-http-backend\n"), 0o644); err != nil {
		t.Fatalf("write from-http.md: %v", err)
	}
	runCmd(cloneDir, "add", "docs/from-http.md")
	runCmd(cloneDir, "commit", "-m", "commit pushed over smart http")
	runCmd(cloneDir, "push", "origin", "main")

	// 3. Verify Contents API serves the newly pushed file both in raw mode and JSON base64 mode.
	reqRaw, _ := http.NewRequest(http.MethodGet, sim.URL+"/repos/acme/platform/contents/docs/from-http.md?ref=main", nil)
	reqRaw.Header.Set("Accept", "application/vnd.github.raw+json")
	reqRaw.Header.Set("Authorization", "Bearer "+bearerToken)
	respRaw, err := http.DefaultClient.Do(reqRaw)
	if err != nil {
		t.Fatalf("GET raw contents: %v", err)
	}
	rawBody, _ := io.ReadAll(respRaw.Body)
	respRaw.Body.Close()
	if respRaw.StatusCode != http.StatusOK || string(rawBody) != "pushed over git-http-backend\n" {
		t.Fatalf("raw contents status=%d body=%q", respRaw.StatusCode, string(rawBody))
	}

	reqJSON, _ := http.NewRequest(http.MethodGet, sim.URL+"/repos/acme/platform/contents/docs/from-http.md?ref=main", nil)
	reqJSON.Header.Set("Authorization", "Bearer "+bearerToken)
	respJSON, err := http.DefaultClient.Do(reqJSON)
	if err != nil {
		t.Fatalf("GET JSON contents: %v", err)
	}
	var meta struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
	}
	_ = json.NewDecoder(respJSON.Body).Decode(&meta)
	respJSON.Body.Close()
	decoded, _ := base64.StdEncoding.DecodeString(meta.Content)
	if respJSON.StatusCode != http.StatusOK || string(decoded) != "pushed over git-http-backend\n" || meta.SHA == "" {
		t.Fatalf("JSON contents status=%d decoded=%q sha=%q", respJSON.StatusCode, string(decoded), meta.SHA)
	}

	// Unauthorized request without token is rejected with 401.
	reqUnauth, _ := http.NewRequest(http.MethodGet, sim.URL+"/repos/acme/platform/contents/docs/from-http.md?ref=main", nil)
	respUnauth, err := http.DefaultClient.Do(reqUnauth)
	if err != nil {
		t.Fatalf("GET unauth: %v", err)
	}
	respUnauth.Body.Close()
	if respUnauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", respUnauth.StatusCode)
	}

	// 4. Verify grainfiles.GitHubFetcher works with GitHub App installation token exchange.
	appFetcher := &grainfiles.GitHubFetcher{
		BaseURL:       sim.URL,
		AppID:         appID,
		AppPrivateKey: privPEM,
	}
	fetched, err := appFetcher.FetchFile(context.Background(), "https://github.com/acme/platform", sha1, "tasks/migrate.task.md")
	if err != nil {
		t.Fatalf("GitHubFetcher with App token exchange failed: %v", err)
	}
	if string(fetched) != "---\nrecipient: https://grain-a.example.com\n---\nMigrate DB.\n" {
		t.Fatalf("unexpected fetched file content: %q", string(fetched))
	}
	if sim.MintedInstallationTokens() != 1 {
		t.Fatalf("expected 1 minted app installation token, got %d", sim.MintedInstallationTokens())
	}

	// 5. OpenPullRequest and MergePullRequest emit pull_request and merge webhooks.
	pr, _, err := sim.OpenPullRequest("acme", "platform", "main", "feat/review", "Add review process", map[string]string{
		"processes/review.process.md": "---\nrecipient: https://grain-a.example.com\non:\n  pull_request:\n    branches: [main]\n---\nReview PR.\n",
	}, nil)
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if len(pr.HeadSHA) != 40 || len(pr.BaseSHA) != 40 {
		t.Fatalf("expected 40-char PR SHAs, got head=%q base=%q", pr.HeadSHA, pr.BaseSHA)
	}

	mergedPR, _, err := sim.MergePullRequest("acme", "platform", pr.Number)
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if len(mergedPR.MergeCommitSHA) != 40 {
		t.Fatalf("expected 40-char merge commit SHA, got %q", mergedPR.MergeCommitSHA)
	}
	if len(hooks) != 3 {
		t.Fatalf("expected 3 total webhooks (push, pr opened, pr closed/merged), got %d", len(hooks))
	}
}
