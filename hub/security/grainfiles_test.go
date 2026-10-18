package security_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

type recordingFetcher struct {
	mu    sync.Mutex
	files map[string][]byte
	calls []string
}

func (f *recordingFetcher) FetchFile(_ context.Context, repoURL, ref, filePath string) ([]byte, error) {
	f.mu.Lock()
	key := repoURL + "|" + filePath + "|" + ref
	f.calls = append(f.calls, key)
	b, ok := f.files[key]
	if !ok {
		b, ok = f.files[repoURL+"|"+filePath]
	}
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return b, nil
}

func (f *recordingFetcher) sawRef(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasSuffix(c, "|"+ref) {
			return true
		}
	}
	return false
}

// TestGrainfileSameOriginContentProxy verifies INV-23 and INV-24:
//   - A Grainfile in Repo A can only grant access to files inside Repo A that
//     are explicitly listed in that Grainfile's references (or the Grainfile itself).
//   - Path traversal, percent-encoded slashes/dots/nulls, URL authority confusion,
//     ref/commit confusion, draft bypass, and unauthorized callers are all rejected.
func TestGrainfileSameOriginContentProxy(t *testing.T) {
	const (
		commitA    = "1111111111111111111111111111111111111111"
		otherSHA   = "9999999999999999999999999999999999999999"
		recipientA = "https://github.com/acme/grain-a"
		recipientB = "https://github.com/beta/grain-b"
	)

	activeTaskMD := `---
title: Migrate Platform Schema
recipient: https://github.com/beta/grain-b
requestor: Alice <alice@example.com>
references:
  - /docs/spec.md
  - ./schema.json
  - /pkg/
  - https://github.com/beta/app/blob/main/secret.md
---
Run schema migration.
`
	draftTaskMD := `---
title: Draft Task
recipient: https://github.com/beta/grain-b
draft: true
references:
  - /docs/draft-only.md
---
Work in progress.
`
	fetcher := &recordingFetcher{
		files: map[string][]byte{
			"https://github.com/acme/app|tasks/migrate.task.md|" + commitA: []byte(activeTaskMD),
			"https://github.com/acme/app|tasks/draft.task.md|" + commitA:   []byte(draftTaskMD),
			"https://github.com/acme/app|docs/spec.md|" + commitA:          []byte("# Spec\n"),
			"https://github.com/acme/app|tasks/schema.json|" + commitA:     []byte(`{"ok":true}`),
			"https://github.com/acme/app|pkg/sub/helper.go|" + commitA:     []byte("package sub\n"),
			"https://github.com/acme/app|docs/draft-only.md|" + commitA:    []byte("DRAFT SECRET\n"),
			"https://github.com/acme/app|secret.env|" + commitA:            []byte("DB_PASS=secret\n"),
			"https://github.com/beta/app|secret.md|" + commitA:             []byte("CROSS REPO SECRET\n"),
		},
	}

	hb := startHub(t, options{grainfileFetcher: fetcher})
	hb.teams()

	tokA, _ := hb.mint(alice, "ws-a")
	tokB, _ := hb.mint(bob, "ws-b")
	tokRevoked, revokedID := hb.mint(bob, "ws-revoked")
	hb.must(hb.user(bob), "DELETE", "/v1/node-tokens/"+revokedID, nil, http.StatusNoContent, nil)

	ctx := context.Background()
	if _, err := hb.h.Grainfiles.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "ws-a",
		Owner:      alice,
		Recipients: []string{recipientA},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := hb.h.Grainfiles.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "ws-b",
		Owner:      bob,
		Recipients: []string{recipientB},
	}); err != nil {
		t.Fatal(err)
	}

	// Ingest both Grainfiles via a signed push webhook.
	whPayload, _ := jsonMarshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": commitA,
		"repository": map[string]any{
			"html_url": "https://github.com/acme/app",
		},
		"commits": []map[string]any{
			{
				"id":    commitA,
				"added": []string{"tasks/migrate.task.md", "tasks/draft.task.md"},
			},
		},
	})
	whReq, _ := http.NewRequest("POST", hb.url+grainfiles.GitHubWebhookPath, bytes.NewReader(whPayload))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-GitHub-Event", "push")
	whReq.Header.Set("X-Hub-Signature-256", signWebhook(webhookSecret, whPayload))
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatal(err)
	}
	whResp.Body.Close()
	if whResp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", whResp.StatusCode)
	}

	gfURL := "https://github.com/acme/app/blob/main/tasks/migrate.task.md"
	draftURL := "https://github.com/acme/app/blob/main/tasks/draft.task.md"

	// 1. Designated recipient node (ws-b) can fetch the Grainfile itself and its same-repo references.
	for _, tc := range []struct {
		name string
		q    url.Values
		want string
	}{
		{"grainfile body", url.Values{"grainfile_url": {gfURL}}, "Run schema migration."},
		{"rooted reference", url.Values{"grainfile_url": {gfURL}, "reference": {"/docs/spec.md#heading"}}, "# Spec"},
		{"relative reference", url.Values{"grainfile_url": {gfURL}, "reference": {"./schema.json"}}, `{"ok":true}`},
		{"folder reference", url.Values{"grainfile_url": {gfURL}, "path": {"pkg/sub/helper.go"}}, "package sub"},
		{"case-insensitive repo+path", url.Values{"repo": {"https://GitHub.com/ACME/APP.GIT"}, "path": {"docs/spec.md"}}, "# Spec"},
	} {
		code, body, hdr := hb.do(bearer(tokB), "GET", grainfiles.ContentPath+"?"+tc.q.Encode(), nil)
		if code != http.StatusOK || !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: got %d %q, want 200 containing %q", tc.name, code, body, tc.want)
		}
		if cc := hdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.name, cc)
		}
		if nosniff := hdr.Get("X-Content-Type-Options"); nosniff != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", tc.name, nosniff)
		}
		if csp := hdr.Get("Content-Security-Policy"); csp != "sandbox" {
			t.Errorf("%s: Content-Security-Policy = %q, want sandbox", tc.name, csp)
		}
	}

	// 2. Unauthorized callers are rejected (401 for unauthenticated/human/revoked, 403 for non-recipient node).
	for _, tc := range []struct {
		name string
		who  as
		want int
	}{
		{"unauthenticated", nobody, http.StatusUnauthorized},
		{"human IAP JWT", hb.user(alice), http.StatusUnauthorized},
		{"revoked node token", bearer(tokRevoked), http.StatusUnauthorized},
		{"non-recipient node token", bearer(tokA), http.StatusForbidden},
	} {
		code, _, _ := hb.do(tc.who, "GET", grainfiles.ContentPath+"?grainfile_url="+url.QueryEscape(gfURL), nil)
		if code != tc.want {
			t.Errorf("caller %s: got %d, want %d", tc.name, code, tc.want)
		}
	}

	// 3. Cross-repository reference (even when listed in references), unlisted same-repo files,
	// draft bypass, path traversal, URL authority confusion, and ref/commit confusion are all 403 Forbidden.
	rawForbiddenQueries := []struct {
		name string
		rawQ string
	}{
		{"unlisted same-repo via grainfile_url", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=/secret.env"},
		{"unlisted same-repo via repo+path", "repo=https://github.com/acme/app&path=secret.env"},
		{"cross-repo reference via grainfile_url", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=" + url.QueryEscape("https://github.com/beta/app/blob/main/secret.md")},
		{"cross-repo reference via repo+path", "repo=https://github.com/beta/app&path=secret.md"},
		{"draft grainfile itself", "grainfile_url=" + url.QueryEscape(draftURL)},
		{"draft grainfile reference", "grainfile_url=" + url.QueryEscape(draftURL) + "&reference=/docs/draft-only.md"},
		{"draft grainfile reference via repo+path", "repo=https://github.com/acme/app&path=docs/draft-only.md"},
		{"dot-dot path traversal in path", "grainfile_url=" + url.QueryEscape(gfURL) + "&path=docs/../../secret.env"},
		{"dot-dot path traversal in reference", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=/docs/../secret.env"},
		{"relative parent traversal", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=../secret.env"},
		{"percent-encoded dot-dot %2e%2e", "grainfile_url=" + url.QueryEscape(gfURL) + "&path=docs/%2e%2e/secret.env"},
		{"percent-encoded slash %2f", "grainfile_url=" + url.QueryEscape(gfURL) + "&path=docs%2f..%2fsecret.env"},
		{"backslash traversal", "grainfile_url=" + url.QueryEscape(gfURL) + "&path=docs%5c..%5csecret.env"},
		{"null byte in path", "grainfile_url=" + url.QueryEscape(gfURL) + "&path=docs/spec.md%00.txt"},
		{"protocol-relative reference", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=//evil.com/docs/spec.md"},
		{"URL authority confusion in repo", "repo=" + url.QueryEscape("https://github.com/acme/app@evil.com") + "&path=docs/spec.md"},
		{"URL authority confusion in reference", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=" + url.QueryEscape("https://github.com/acme/app@evil.com/blob/main/docs/spec.md")},
		{"ref confusion via query param", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=/docs/spec.md&ref=unindexed-branch"},
		{"commit confusion via query param", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=/docs/spec.md&commit=" + otherSHA},
		{"ref confusion in grainfile_url", "grainfile_url=" + url.QueryEscape("https://github.com/acme/app/blob/unindexed-branch/tasks/migrate.task.md")},
		{"ref confusion in reference URL", "grainfile_url=" + url.QueryEscape(gfURL) + "&reference=" + url.QueryEscape("https://github.com/acme/app/blob/unindexed-branch/docs/spec.md")},
	}
	for _, tc := range rawForbiddenQueries {
		code, body, _ := hb.do(bearer(tokB), "GET", grainfiles.ContentPath+"?"+tc.rawQ, nil)
		if code != http.StatusForbidden {
			t.Errorf("%s: got %d (%s), want 403 Forbidden", tc.name, code, body)
		}
	}
	if fetcher.sawRef("unindexed-branch") || fetcher.sawRef(otherSHA) {
		t.Errorf("upstream fetcher was called with unindexed ref or commit: %v", fetcher.calls)
	}
}

// TestWebhookReceiverAndGitHubFetcherHardening verifies INV-25:
//   - HMAC-SHA256 validation fails closed when secret is empty or signature is missing/invalid.
//   - GitHubFetcher refuses non-github.com hosts when BaseURL is empty, refuses metadata/link-local
//     BaseURLs, refuses embedded URL credentials, and refuses HTTP redirects.
//   - The YAML parser rejects anchors/aliases, merge keys, explicit tags (in block and flow
//     collections), deeply nested structures (> 32 levels), and oversized front matter or files.
func TestWebhookReceiverAndGitHubFetcherHardening(t *testing.T) {
	// 1. VerifyGitHubSignature fails closed on empty secret or bad signature.
	body := []byte(`{"ref":"refs/heads/main"}`)
	if grainfiles.VerifyGitHubSignature("", "", body) {
		t.Error("VerifyGitHubSignature accepted empty secret")
	}
	if grainfiles.VerifyGitHubSignature("secret", "", body) {
		t.Error("VerifyGitHubSignature accepted missing signature")
	}
	if grainfiles.VerifyGitHubSignature("secret", signWebhook("wrong", body), body) {
		t.Error("VerifyGitHubSignature accepted wrong secret")
	}

	// Hub with no webhook secret configured fails closed (401) on POST /v1/webhooks/github.
	hbNoSecret := startHub(t, options{noWebhookSecret: true})
	code, _, _ := hbNoSecret.do(nobody, "POST", grainfiles.GitHubWebhookPath, map[string]string{"ref": "refs/heads/main"})
	if code != http.StatusUnauthorized {
		t.Errorf("POST /v1/webhooks/github with unset secret: got %d, want 401", code)
	}

	// 2. GitHubFetcher SSRF, host-scoped token minting, and redirect defense.
	tokenCalled := false
	gfFetcher := &grainfiles.GitHubFetcher{
		TokenFunc: func(_ context.Context, repoURL string) (string, error) {
			tokenCalled = true
			return "ghs_secret_token", nil
		},
	}
	for _, badRepo := range []string{
		"https://evil.example.com/acme/app",
		"https://169.254.169.254/latest/meta-data",
		"https://github.com/acme/app@evil.com",
		"http://github.com/acme/app",
	} {
		if _, err := gfFetcher.FetchFile(context.Background(), badRepo, "main", "tasks/a.task.md"); err == nil {
			t.Errorf("GitHubFetcher accepted bad repoURL %q", badRepo)
		}
		if tokenCalled {
			t.Errorf("GitHubFetcher called TokenFunc for untrusted repoURL %q", badRepo)
		}
	}

	// Metadata IP or embedded credentials in BaseURL are rejected.
	for _, badBase := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://[fe80::1]/api",
		"https://user:pass@api.github.com",
	} {
		f := &grainfiles.GitHubFetcher{BaseURL: badBase, Token: "tok"}
		if _, err := f.FetchFile(context.Background(), "https://github.com/acme/app", "main", "tasks/a.task.md"); err == nil {
			t.Errorf("GitHubFetcher accepted unsafe BaseURL %q", badBase)
		}
	}

	// Redirects from upstream API server are refused.
	redirectTargetHit := false
	internalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer internalSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internalSrv.URL+"/latest/meta-data", http.StatusFound)
	}))
	defer redirectSrv.Close()

	redirFetcher := &grainfiles.GitHubFetcher{BaseURL: redirectSrv.URL, Token: "tok"}
	if _, err := redirFetcher.FetchFile(context.Background(), "https://github.com/acme/app", "main", "tasks/a.task.md"); err == nil {
		t.Error("GitHubFetcher followed HTTP redirect")
	}
	if redirectTargetHit {
		t.Error("GitHubFetcher dialled redirect target")
	}

	// 3. YAML parser safety against anchors/aliases, merge keys, explicit tags, deep nesting, and oversized inputs.
	deepFlow := strings.Repeat("[", 40) + `"x"` + strings.Repeat("]", 40)
	var deepBlock strings.Builder
	deepBlock.WriteString("---\ntitle: Deep\non:\n")
	for i := 0; i < 40; i++ {
		deepBlock.WriteString(strings.Repeat("  ", i+1) + fmt.Sprintf("k%d:\n", i))
	}
	deepBlock.WriteString("---\nBody\n")

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"YAML anchor and alias", "---\ntitle: &a Bomb\nrequestor: *a\n---\nBody\n"},
		{"YAML merge key", "---\ntitle: Merge\n<<: *base\n---\nBody\n"},
		{"YAML block tag", "---\ntitle: !custom Tag\n---\nBody\n"},
		{"YAML flow sequence tag", "---\ntitle: FlowTag\nreferences: [!custom /docs/a.md]\n---\nBody\n"},
		{"YAML flow mapping tag", "---\ntitle: FlowMapTag\non: {push: !!str main}\n---\nBody\n"},
		{"deeply nested flow collection", "---\ntitle: DeepFlow\nreferences: " + deepFlow + "\n---\nBody\n"},
		{"deeply nested block mapping", deepBlock.String()},
		{"oversized front matter (>64KiB)", "---\ntitle: " + strings.Repeat("a", 70*1024) + "\n---\nBody\n"},
		{"oversized file (>1MiB)", "---\ntitle: Big\n---\n" + strings.Repeat("b", (1<<20)+1024)},
	} {
		if _, err := grainfiles.Parse("tasks/bad.task.md", []byte(tc.raw)); err == nil {
			t.Errorf("Parse(%s): succeeded, want error", tc.name)
		}
	}
}

// TestTenantRecipientIsolationAndControlStreamSpoofing verifies INV-26 and INV-27:
//   - A node cannot claim a recipient or repo/folder subscription belonging to another team's
//     repository, nor claim a recipient URL already owned by another team's user.
//   - A node cannot spoof grainfile_completed / dependency_completed for a Grainfile it is not
//     the recipient of.
func TestTenantRecipientIsolationAndControlStreamSpoofing(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()

	tokA, _ := hb.mint(alice, "ws-a")
	tokB, _ := hb.mint(bob, "ws-b")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. Node A (alice, team-a) connects and claims recipient https://github.com/acme/grain-a.
	clA := &rendezvous.Client{
		Hub:   hb.url,
		Token: func(context.Context) (string, error) { return tokA, nil },
		Node: registry.Node{
			ID:         "ws-a",
			Repos:      []string{repoA},
			Slots:      2,
			Features:   []string{registry.FeatureGrainfiles},
			Recipients: []string{"https://github.com/acme/grain-a"},
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{"https://github.com/acme/grain-a"},
		},
	}
	connA, err := clA.Connect(ctx)
	if err != nil {
		t.Fatalf("connA.Connect: %v", err)
	}
	defer connA.Close()

	// 2. Node B (bob, team-b) is refused when connecting with team-a's recipient or repoA subscription.
	for _, badSub := range []struct {
		name string
		sub  grainfiles.Subscriptions
	}{
		{"recipient owned by team-a user", grainfiles.Subscriptions{Recipients: []string{"https://github.com/acme/grain-a"}}},
		{"recipient in team-a repository", grainfiles.Subscriptions{Recipients: []string{"https://github.com/acme/app"}}},
		{"repo subscription in team-a repository", grainfiles.Subscriptions{Repos: []string{"https://github.com/acme/app"}}},
		{"folder subscription in team-a repository", grainfiles.Subscriptions{Folders: []string{"https://github.com/acme/app/tree/main/tasks"}}},
	} {
		clBad := &rendezvous.Client{
			Hub:           hb.url,
			Token:         func(context.Context) (string, error) { return tokB, nil },
			Node:          registry.Node{ID: "ws-b", Repos: []string{repoB}, Slots: 2, Features: []string{registry.FeatureGrainfiles}},
			Subscriptions: &badSub.sub,
		}
		if c, err := clBad.Connect(ctx); err == nil {
			c.Close()
			t.Errorf("Node B connect with %s succeeded, want refusal", badSub.name)
		}
	}

	// 3. Node B connects legitimately with its own recipient https://github.com/beta/grain-b,
	// then attempts to hijack team-a's recipient or repo via Subscribe over the control stream.
	clB := &rendezvous.Client{
		Hub:   hb.url,
		Token: func(context.Context) (string, error) { return tokB, nil },
		Node: registry.Node{
			ID:       "ws-b",
			Repos:    []string{repoB},
			Slots:    2,
			Features: []string{registry.FeatureGrainfiles},
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{"https://github.com/beta/grain-b"},
		},
	}
	connB, err := clB.Connect(ctx)
	if err != nil {
		t.Fatalf("connB.Connect: %v", err)
	}
	defer connB.Close()

	if _, err := connB.Subscribe(ctx, grainfiles.Subscriptions{
		Recipients: []string{"https://github.com/acme/grain-a"},
	}); err == nil {
		t.Error("connB.Subscribe claiming team-a's recipient succeeded, want error")
	}
	if _, err := connB.Subscribe(ctx, grainfiles.Subscriptions{
		Repos: []string{"https://github.com/acme/app"},
	}); err == nil {
		t.Error("connB.Subscribe to team-a's repo succeeded, want error")
	}

	// 4. Control stream spoofing: index upstream task in acme/app (recipient grain-a)
	// and downstream dependent task in beta/app (recipient grain-b).
	upGF, err := grainfiles.Parse("tasks/01-build.task.md", []byte("---\ntitle: Upstream Build\nrecipient: https://github.com/acme/grain-a\n---\nBuild.\n"))
	if err != nil {
		t.Fatal(err)
	}
	upGF.Repo, upGF.Ref, upGF.Commit = "https://github.com/acme/app", "main", "1111111111111111111111111111111111111111"
	upRec, err := hb.h.Grainfiles.IndexGrainfile(ctx, upGF)
	if err != nil {
		t.Fatal(err)
	}

	downGF, err := grainfiles.Parse("tasks/02-deploy.task.md", []byte("---\ntitle: Downstream Deploy\nrecipient: https://github.com/beta/grain-b\ndepends_on:\n  - https://github.com/acme/app/blob/main/tasks/01-build.task.md\n---\nDeploy.\n"))
	if err != nil {
		t.Fatal(err)
	}
	downGF.Repo, downGF.Ref, downGF.Commit = "https://github.com/beta/app", "main", "2222222222222222222222222222222222222222"
	downRec, err := hb.h.Grainfiles.IndexGrainfile(ctx, downGF)
	if err != nil {
		t.Fatal(err)
	}

	// Node B (not recipient of 01-build.task.md) attempts to spoof completion of 01-build.task.md.
	if _, err := connB.CompleteGrainfile(ctx, upRec.URL, upRec.CommitSHA, "done", "spoofed by node B"); err == nil {
		t.Fatal("connB.CompleteGrainfile for Node A's grainfile succeeded, want forbidden error")
	}
	if sat, err := hb.h.Grainfiles.DependenciesSatisfied(ctx, downRec.URL); err != nil || sat {
		t.Fatalf("DependenciesSatisfied after spoofed completion = %v (err=%v), want false", sat, err)
	}

	// Node A (the true recipient) completes 01-build.task.md with an invalid state -> rejected.
	if _, err := connA.CompleteGrainfile(ctx, upRec.URL, upRec.CommitSHA, "hacked", "bad state"); err == nil {
		t.Fatal("connA.CompleteGrainfile with invalid state succeeded, want error")
	}

	// Node A completes 01-build.task.md legitimately -> succeeds and satisfies downstream dependency.
	evs, err := connA.CompleteGrainfile(ctx, upRec.URL, upRec.CommitSHA, "done", "built by node A")
	if err != nil || len(evs) != 1 {
		t.Fatalf("connA.CompleteGrainfile: evs=%+v err=%v, want 1 event", evs, err)
	}
	if sat, err := hb.h.Grainfiles.DependenciesSatisfied(ctx, downRec.URL); err != nil || !sat {
		t.Fatalf("DependenciesSatisfied after legitimate completion = %v (err=%v), want true", sat, err)
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
