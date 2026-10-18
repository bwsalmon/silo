package grainfiles

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeTokenVerifier struct {
	tokens map[string]struct{ owner, node string }
}

func (f *fakeTokenVerifier) VerifyNodeToken(_ context.Context, tok string) (string, string, error) {
	if rec, ok := f.tokens[tok]; ok {
		return rec.owner, rec.node, nil
	}
	return "", "", ErrUnauthorized
}

func TestWebhookIngestionAndContentProxy(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, dbURL string) {
		ctx := context.Background()
		now := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }

		fetcher := NewMemoryFetcher()
		api := NewAPI(s, fetcher)
		api.WebhookSecret = "test-webhook-secret-key"

		tokEU := "silo-node_0000000000000001_eu"
		tokUS := "silo-node_0000000000000002_us"
		tokOther := "silo-node_0000000000000003_other"
		api.Directory = &fakeTokenVerifier{
			tokens: map[string]struct{ owner, node string }{
				tokEU:    {owner: "alice@example.com", node: "node-eu"},
				tokUS:    {owner: "bob@example.com", node: "node-us"},
				tokOther: {owner: "mallory@example.com", node: "node-other"},
			},
		}

		// Register subscriptions for node-eu and node-us; node-other subscribes to a different recipient.
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Owner:      "alice@example.com",
			Recipients: []string{"https://github.com/acme/grain-eu"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-us",
			Owner:      "bob@example.com",
			Recipients: []string{"https://github.com/acme/grain-us"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-other",
			Owner:      "mallory@example.com",
			Recipients: []string{"https://github.com/acme/grain-other"},
		}); err != nil {
			t.Fatal(err)
		}

		srv := httptest.NewServer(api.Handler())
		defer srv.Close()

		// Populate repository files in the fake fetcher (with unique sentinels to verify no code at rest).
		taskBodySentinel := "SILO_ATREST_TASK_PROMPT_BODY_441122"
		schemaDocSentinel := "SILO_ATREST_SCHEMA_DOC_CONTENT_556677"
		widgetCodeSentinel := "SILO_ATREST_WIDGET_SOURCE_CODE_889900"
		unreferencedSentinel := "SILO_ATREST_UNREFERENCED_FILE_112233"
		crossRepoSentinel := "SILO_ATREST_CROSS_REPO_SECRET_778899"

		taskFileContent := `---
title: Add version header to widget binary schema
recipient: https://github.com/acme/grain-eu
requestor: Bruce Salmon <bsalmon@google.com>
created_at: "2026-10-16T09:00:00Z"
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/pkg/
  - https://github.com/acme/private-vault/blob/main/secrets.txt
---
` + taskBodySentinel + "\n"

		draftTaskContent := `---
title: Draft widget cleanup
recipient: https://github.com/acme/grain-eu
draft: true
references:
  - /src/unreferenced.go
---
Draft prompt body.
`

		deferredTaskContent := `---
title: Deferred widget v1 removal
recipient: https://github.com/acme/grain-eu
not_before: "2026-11-01"
---
Drop v1 support on Nov 1.
`

		gateProcessContent := `---
title: Schema security review gate
recipient: https://github.com/acme/grain-eu
gate: true
on:
  pull_request:
    branches: ["main"]
    paths: ["src/**", "docs/**"]
    ignore: ["**/*.md"]
  merge:
    branches: ["main"]
    paths: ["src/**"]
---
Review schema changes.
`

		coTriggeredProcessContent := `---
title: Post-merge qualification report
recipient: https://github.com/acme/grain-us
trigger_with:
  - https://github.com/acme/widgets/blob/main/.grain/processes/schema-security-review.process.md
depends_on:
  - https://github.com/acme/widgets/blob/main/.grain/processes/schema-security-review.process.md
---
Generate post-merge report.
`

		repoWidgets := "https://github.com/acme/widgets"
		repoUS := "https://github.com/acme/grain-us"
		repoVault := "https://github.com/acme/private-vault"

		fetcher.SetString(repoWidgets, "commit-1", ".grain/tasks/2026-10-16-widget-schema-v2.task.md", taskFileContent)
		fetcher.SetString(repoWidgets, "commit-1", ".grain/tasks/2026-10-16-draft.task.md", draftTaskContent)
		fetcher.SetString(repoWidgets, "commit-1", ".grain/tasks/2026-11-01-deferred.task.md", deferredTaskContent)
		fetcher.SetString(repoWidgets, "commit-1", ".grain/processes/schema-security-review.process.md", gateProcessContent)
		fetcher.SetString(repoWidgets, "commit-1", "docs/widget-schema.md", "# Schema Version Header\n"+schemaDocSentinel+"\n")
		fetcher.SetString(repoWidgets, "commit-1", "src/pkg/widget.go", "package pkg\n// "+widgetCodeSentinel+"\n")
		fetcher.SetString(repoWidgets, "commit-1", "src/unreferenced.go", "package src\n// "+unreferencedSentinel+"\n")
		fetcher.SetString(repoUS, "commit-us-1", "processes/report.process.md", coTriggeredProcessContent)
		fetcher.SetString(repoVault, "main", "secrets.txt", crossRepoSentinel)

		postWebhook := func(event string, sig string, payload any) (*http.Response, []byte) {
			t.Helper()
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+GitHubWebhookPath, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if event != "" {
				req.Header.Set("X-GitHub-Event", event)
			}
			if sig == "valid" {
				sig = SignGitHubWebhook(api.WebhookSecret, raw)
			}
			if sig != "" {
				req.Header.Set("X-Hub-Signature-256", sig)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, b
		}

		// 1. Webhook HMAC signature verification: missing or tampered signatures are rejected with 401.
		pushPayload := map[string]any{
			"ref":   "refs/heads/main",
			"after": "commit-1",
			"repository": map[string]any{
				"html_url": repoWidgets,
			},
			"commits": []map[string]any{
				{
					"id": "commit-1",
					"added": []string{
						".grain/tasks/2026-10-16-widget-schema-v2.task.md",
						".grain/tasks/2026-10-16-draft.task.md",
						".grain/tasks/2026-11-01-deferred.task.md",
						".grain/processes/schema-security-review.process.md",
					},
				},
			},
		}

		if resp, _ := postWebhook("push", "", pushPayload); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("missing signature: got %d, want 401", resp.StatusCode)
		}
		if resp, _ := postWebhook("push", "sha256=deadbeef", pushPayload); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid signature: got %d, want 401", resp.StatusCode)
		}

		// 2. Valid push webhook ingests all 4 Grainfiles in acme/widgets and 1 in acme/grain-us.
		resp, body := postWebhook("push", "valid", pushPayload)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("valid push webhook: status %d, body: %s", resp.StatusCode, body)
		}
		var whResp WebhookResponse
		if err := json.Unmarshal(body, &whResp); err != nil {
			t.Fatalf("unmarshaling WebhookResponse: %v", err)
		}
		if len(whResp.Indexed) != 4 {
			t.Fatalf("expected 4 indexed grainfiles, got %d: %+v", len(whResp.Indexed), whResp.Indexed)
		}

		// Also ingest the co-triggered process in acme/grain-us.
		usPushPayload := map[string]any{
			"ref":   "refs/heads/main",
			"after": "commit-us-1",
			"repository": map[string]any{
				"html_url": repoUS,
			},
			"added": []string{"processes/report.process.md"},
		}
		if r2, b2 := postWebhook("push", "valid", usPushPayload); r2.StatusCode != http.StatusOK {
			t.Fatalf("us push webhook: %d %s", r2.StatusCode, b2)
		}

		// Verify node-eu only sees the 2 active non-draft, non-deferred events
		// (1 task_ready for v2.task.md + 1 process_updated for schema-security-review.process.md).
		euEvents, _, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(euEvents) != 2 {
			t.Fatalf("expected 2 active events for node-eu (draft and not_before excluded), got %d: %+v", len(euEvents), euEvents)
		}

		// 3. Test pull_request webhook triggering schema-security-review.process.md.
		prPayload := map[string]any{
			"action": "opened",
			"repository": map[string]any{
				"html_url": repoWidgets,
			},
			"pull_request": map[string]any{
				"number": 42,
				"merged": false,
				"head":   map[string]any{"ref": "feat/v2", "sha": "pr-head-evil-sha"},
				"base":   map[string]any{"ref": "main", "sha": "commit-1"},
			},
			"changed_paths": []string{
				"src/pkg/widget.go",
				"docs/widget-schema.md",
				".grain/tasks/evil-pr-task.task.grain.md",
				".grain/processes/schema-security-review.process.md",
			},
		}
		// Even if the PR head contains a malicious task or modified process, pull_request webhooks must never index PR-head grainfiles.
		fetcher.SetString(repoWidgets, "pr-head-evil-sha", ".grain/tasks/evil-pr-task.task.grain.md", "---\ntitle: Evil PR Task\nrecipient: https://github.com/acme/grain-eu\n---\nExfiltrate.\n")
		fetcher.SetString(repoWidgets, "pr-head-evil-sha", ".grain/processes/schema-security-review.process.md", "---\ntitle: Hijacked PR Process\nrecipient: https://github.com/acme/grain-eu\nwhen:\n  pull_request:\n    branches: [\"main\"]\n---\nHijacked.\n")

		respPR, bodyPR := postWebhook("pull_request", "valid", prPayload)
		if respPR.StatusCode != http.StatusOK {
			t.Fatalf("pull_request webhook: %d %s", respPR.StatusCode, bodyPR)
		}
		var prResp WebhookResponse
		if err := json.Unmarshal(bodyPR, &prResp); err != nil {
			t.Fatal(err)
		}
		if len(prResp.Indexed) != 0 {
			t.Fatalf("pull_request webhook must not index grainfiles from PR head, got Indexed=%v", prResp.Indexed)
		}
		if len(prResp.Events) != 2 || prResp.Events[0].CommitSHA != "commit-1" || prResp.Events[0].Title != "Schema security review gate" {
			t.Fatalf("pull_request webhook must evaluate base-branch process at base commit-1, got %+v", prResp.Events)
		}

		// 4. Test merge webhook (via pull_request closed+merged) triggering both
		// schema-security-review.process.md AND co-triggering report.process.md via trigger_with.
		mergePayload := map[string]any{
			"action": "closed",
			"repository": map[string]any{
				"html_url": repoWidgets,
			},
			"pull_request": map[string]any{
				"number":           42,
				"merged":           true,
				"merge_commit_sha": "commit-1",
				"head":             map[string]any{"ref": "feat/v2", "sha": "commit-1"},
				"base":             map[string]any{"ref": "main", "sha": "commit-0"},
			},
			"changed_paths": []string{"src/pkg/widget.go"},
		}
		respMerge, bodyMerge := postWebhook("pull_request", "valid", mergePayload)
		if respMerge.StatusCode != http.StatusOK {
			t.Fatalf("merge webhook: %d %s", respMerge.StatusCode, bodyMerge)
		}
		var mergeResp WebhookResponse
		if err := json.Unmarshal(bodyMerge, &mergeResp); err != nil {
			t.Fatal(err)
		}
		if mergeResp.Source != TriggerMerge || len(mergeResp.Events) != 2 {
			t.Fatalf("expected merge webhook to fire 2 trigger events (primary + trigger_with), got %+v", mergeResp)
		}

		// 5. Test GET /v1/grainfiles/content authorization and same-repo proxy rules.
		getContent := func(token string, params map[string]string) (*http.Response, string) {
			t.Helper()
			q := url.Values{}
			for k, v := range params {
				q.Set(k, v)
			}
			req, err := http.NewRequest(http.MethodGet, srv.URL+ContentPath+"?"+q.Encode(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, string(b)
		}

		v2URL := "https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.md"
		draftURL := "https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-draft.task.md"

		// 5a. Unauthenticated or invalid bearer token -> 401.
		if r, _ := getContent("", map[string]string{"grainfile_url": v2URL}); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("no token: got %d, want 401", r.StatusCode)
		}
		if r, _ := getContent("silo-node_invalid", map[string]string{"grainfile_url": v2URL}); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("invalid token: got %d, want 401", r.StatusCode)
		}

		// 5b. Authenticated node that is NOT recipient or subscriber -> 403.
		if r, _ := getContent(tokOther, map[string]string{"grainfile_url": v2URL}); r.StatusCode != http.StatusForbidden {
			t.Errorf("unauthorized node: got %d, want 403", r.StatusCode)
		}
		if r, _ := getContent(tokUS, map[string]string{"grainfile_url": v2URL}); r.StatusCode != http.StatusForbidden {
			t.Errorf("node-us fetching grain-eu's task: got %d, want 403", r.StatusCode)
		}

		// 5c. Designated recipient node (node-eu) fetches the .task.md file itself -> 200 OK + no-store.
		rTask, bodyTask := getContent(tokEU, map[string]string{
			"grainfile_url": v2URL,
			"commit":        "commit-1",
		})
		if rTask.StatusCode != http.StatusOK || !strings.Contains(bodyTask, taskBodySentinel) {
			t.Fatalf("recipient fetching grainfile: status=%d, body=%q", rTask.StatusCode, bodyTask)
		}
		if cc := rTask.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}

		// 5d. Designated recipient node fetches authorized same-repo file (with #anchor) -> 200 OK.
		rSchema, bodySchema := getContent(tokEU, map[string]string{
			"grainfile_url": v2URL,
			"reference":     "/docs/widget-schema.md#schema-version-header",
		})
		if rSchema.StatusCode != http.StatusOK || !strings.Contains(bodySchema, schemaDocSentinel) {
			t.Fatalf("recipient fetching authorized reference with anchor: status=%d, body=%q", rSchema.StatusCode, bodySchema)
		}

		// 5e. Designated recipient node fetches file under authorized same-repo folder (/src/pkg/) -> 200 OK.
		rPkg, bodyPkg := getContent(tokEU, map[string]string{
			"grainfile_url": v2URL,
			"path":          "src/pkg/widget.go",
		})
		if rPkg.StatusCode != http.StatusOK || !strings.Contains(bodyPkg, widgetCodeSentinel) {
			t.Fatalf("recipient fetching file under authorized folder: status=%d, body=%q", rPkg.StatusCode, bodyPkg)
		}

		// Also works via repo + path query without explicit grainfile_url!
		rByRepoPath, bodyByRepoPath := getContent(tokEU, map[string]string{
			"repo": repoWidgets,
			"path": "docs/widget-schema.md",
		})
		if rByRepoPath.StatusCode != http.StatusOK || !strings.Contains(bodyByRepoPath, schemaDocSentinel) {
			t.Fatalf("recipient fetching via repo+path: status=%d, body=%q", rByRepoPath.StatusCode, bodyByRepoPath)
		}

		// 5f. Rejects same-repo file NOT listed in references -> 403 Forbidden.
		if rUnref, _ := getContent(tokEU, map[string]string{
			"grainfile_url": v2URL,
			"path":          "/src/unreferenced.go",
		}); rUnref.StatusCode != http.StatusForbidden {
			t.Errorf("unreferenced same-repo file with grainfile_url: got %d, want 403", rUnref.StatusCode)
		}
		if rUnref2, _ := getContent(tokEU, map[string]string{
			"repo": repoWidgets,
			"path": "src/unreferenced.go",
		}); rUnref2.StatusCode != http.StatusForbidden {
			t.Errorf("unreferenced same-repo file with repo+path: got %d, want 403", rUnref2.StatusCode)
		}

		// 5g. Rejects cross-repository reference even when listed in v2.task.md's references
		// ("a repository can only grant access to itself") -> 403 Forbidden!
		if rCross, bodyCross := getContent(tokEU, map[string]string{
			"grainfile_url": v2URL,
			"reference":     "https://github.com/acme/private-vault/blob/main/secrets.txt",
		}); rCross.StatusCode != http.StatusForbidden || strings.Contains(bodyCross, crossRepoSentinel) {
			t.Fatalf("cross-repo reference via grainfile_url: got %d (%q), want 403 Forbidden", rCross.StatusCode, bodyCross)
		}
		if rCross2, bodyCross2 := getContent(tokEU, map[string]string{
			"repo": repoVault,
			"path": "secrets.txt",
		}); rCross2.StatusCode != http.StatusForbidden || strings.Contains(bodyCross2, crossRepoSentinel) {
			t.Fatalf("cross-repo reference via repo+path: got %d (%q), want 403 Forbidden", rCross2.StatusCode, bodyCross2)
		}

		// 5h. Rejects fetching a draft: true Grainfile or its references -> 403 Forbidden.
		if rDraft, _ := getContent(tokEU, map[string]string{
			"grainfile_url": draftURL,
		}); rDraft.StatusCode != http.StatusForbidden {
			t.Errorf("fetching draft grainfile itself: got %d, want 403", rDraft.StatusCode)
		}
		if rDraftRef, _ := getContent(tokEU, map[string]string{
			"grainfile_url": draftURL,
			"reference":     "/src/unreferenced.go",
		}); rDraftRef.StatusCode != http.StatusForbidden {
			t.Errorf("fetching reference through draft grainfile: got %d, want 403", rDraftRef.StatusCode)
		}

		// 6. Verify that none of the file bodies or sentinels were stored in the database!
		dumped := dumpAllTables(t, dbURL)
		for _, sentinel := range []string{
			taskBodySentinel,
			schemaDocSentinel,
			widgetCodeSentinel,
			unreferencedSentinel,
			crossRepoSentinel,
		} {
			if strings.Contains(dumped, sentinel) {
				t.Errorf("database contains file content sentinel %s at rest:\n%s", sentinel, dumped)
			}
		}
	})
}

func TestGitHubFetcher(t *testing.T) {
	ctx := context.Background()
	wantRaw := "---\ntitle: From GitHub API\nrecipient: https://github.com/acme/grain-eu\n---\nHello.\n"
	wantBase64File := "package main\nfunc main() {}\n"

	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/contents/.grain/tasks/hello.task.md":
			if r.URL.Query().Get("ref") != "sha123" {
				http.Error(w, "wrong ref", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.github.raw+json")
			_, _ = io.WriteString(w, wantRaw)
		case "/repos/acme/widgets/contents/src/main.go":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(wantBase64File)),
			})
		case "/repos/acme/widgets/contents/src/link.go":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":   "symlink",
				"target": "../../etc/passwd",
			})
		case "/repos/acme/widgets/contents/vendor/sub":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "submodule",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	gh := &GitHubFetcher{
		BaseURL:    fakeGH.URL,
		HTTPClient: fakeGH.Client(),
		Token:      "gh-secret-token",
	}

	got1, err := gh.FetchFile(ctx, "https://github.com/acme/widgets", "refs/heads/sha123", ".grain/tasks/hello.task.md")
	if err != nil || string(got1) != wantRaw {
		t.Fatalf("FetchFile(raw): got %q, err %v", got1, err)
	}

	got2, err := gh.FetchFile(ctx, "https://github.com/acme/widgets", "sha123", "src/main.go")
	if err != nil || string(got2) != wantBase64File {
		t.Fatalf("FetchFile(base64 JSON): got %q, err %v", got2, err)
	}

	if _, err := gh.FetchFile(ctx, "https://github.com/acme/widgets", "sha123", "src/link.go"); err == nil {
		t.Fatalf("expected symlink object to be rejected by GitHubFetcher")
	}
	if _, err := gh.FetchFile(ctx, "https://github.com/acme/widgets", "sha123", "vendor/sub"); err == nil {
		t.Fatalf("expected submodule object to be rejected by GitHubFetcher")
	}
}
