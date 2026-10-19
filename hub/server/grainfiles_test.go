package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

func signGitHubWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGrainfileEndToEndWebhookNotifyProxyAndCompletion(t *testing.T) {
	const (
		webhookSecret = "test-hub-webhook-secret"
		githubToken   = "ghp_hub_forge_token"
		commitA       = "1111111111111111111111111111111111111111"
		commitC       = "2222222222222222222222222222222222222222"
		recipientB    = "https://github.com/acme/node-b"
		recipientC    = "https://github.com/acme/node-c"
	)

	taskMD := `---
grainfile: v1
title: Deploy service to staging
recipient: https://github.com/acme/node-b
requestor: Alice <alice@example.com>
references:
  - /docs/arch.md
  - https://github.com/acme/repo-c/blob/main/secret.md
---
Deploy the staging service according to docs/arch.md.
`
	processMD := `---
grainfile: v1
title: Continuous verification process
recipient: https://github.com/acme/node-b
on:
  push:
    branches:
      - main
references:
  - /docs/arch.md
---
Run verification checks on every push to main.
`
	archMD := "# Architecture\n\nStaging deployment architecture notes.\n"
	unlistedMD := "# Unlisted\n\nThis file is not listed in references.\n"

	dependentTaskMD := `---
grainfile: v1
title: Post-deploy verification
recipient: https://github.com/acme/node-c
depends_on:
  - https://github.com/acme/repo-a/blob/main/tasks/01-deploy.task.md
---
Run post-deploy verification once 01-deploy.task.md completes.
`

	forgeFiles := map[string]string{
		"/repos/acme/repo-a/contents/tasks/01-deploy.task.md@" + commitA:        taskMD,
		"/repos/acme/repo-a/contents/.grain/processes/ci.process.md@" + commitA: processMD,
		"/repos/acme/repo-a/contents/docs/arch.md@" + commitA:                   archMD,
		"/repos/acme/repo-a/contents/docs/unlisted.md@" + commitA:               unlistedMD,
		"/repos/acme/repo-c/contents/tasks/02-verify.task.md@" + commitC:        dependentTaskMD,
		"/repos/acme/repo-c/contents/secret.md@" + commitA:                      "cross-repo secret",
	}

	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+githubToken {
			http.Error(w, "unauthorized forge token", http.StatusUnauthorized)
			return
		}
		key := r.URL.Path + "@" + r.URL.Query().Get("ref")
		content, ok := forgeFiles[key]
		if !ok {
			http.Error(w, "not found: "+key, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(content),
			"sha":      "blobsha",
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		})
	}))
	defer forge.Close()

	dbURL := "sqlite:" + t.TempDir() + "/hub.db"
	h := start(t, Config{
		Database:      dbURL,
		DevUserHeader: userHeader,
		HubAdmins:     []string{"user:root@example.com"},
		AcceptedUsers: []string{"alice@example.com", "bob@example.com", "carol@example.com", "outsider@example.com"},
		WebhookSecret: webhookSecret,
		GitHubToken:   githubToken,
		GitHubAPIURL:  forge.URL,
		Heartbeat:     2 * time.Second,
		DispatchRetry: time.Second,
		ShutdownGrace: 2 * time.Second,
		Logf:          logf(t),
	}, "127.0.0.1:0")

	// Set up Team A (owns acme/repo-a) and Team B (node-b's owner bob) and Team C (node-c's owner carol).
	call(t, "POST", h.url+"/v1/teams", "root@example.com", map[string]string{"id": "team-a", "name": "Team A"}, nil)
	call(t, "PUT", h.url+"/v1/teams/team-a/members/user:alice@example.com", "root@example.com", map[string]string{"role": "admin"}, nil)
	call(t, "PUT", h.url+"/v1/teams/team-a/repos/acme/repo-a", "root@example.com", nil, nil)

	call(t, "POST", h.url+"/v1/teams", "root@example.com", map[string]string{"id": "team-b", "name": "Team B"}, nil)
	call(t, "PUT", h.url+"/v1/teams/team-b/members/user:bob@example.com", "root@example.com", map[string]string{"role": "admin"}, nil)
	call(t, "PUT", h.url+"/v1/teams/team-b/members/user:carol@example.com", "root@example.com", map[string]string{"role": "member"}, nil)
	call(t, "PUT", h.url+"/v1/teams/team-b/repos/acme/repo-c", "root@example.com", nil, nil)
	call(t, "PUT", h.url+"/v1/teams/team-b/repos/acme/node-b", "root@example.com", nil, nil)

	var mintB, mintC struct {
		Token string `json:"token"`
	}
	call(t, "POST", h.url+"/v1/node-tokens", "bob@example.com", map[string]string{"node": "node-b"}, &mintB)
	call(t, "POST", h.url+"/v1/node-tokens", "carol@example.com", map[string]string{"node": "node-c"}, &mintC)

	// Connect Node B with initial subscriptions in hello targeting recipientB.
	// Note that Repo A belongs to Team A while Node B belongs to Bob (Team B),
	// exercising cross-repo / cross-team Grainfile targeting via `recipient`.
	clientB := &rendezvous.Client{
		Hub:   h.url,
		Token: func(context.Context) (string, error) { return mintB.Token, nil },
		Node: registry.Node{
			ID:         "node-b",
			Repos:      []string{"acme/repo-c"},
			Slots:      2,
			Features:   []string{registry.FeatureGrainfiles},
			Recipients: []string{recipientB},
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{recipientB},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connB, err := clientB.Connect(ctx)
	if err != nil {
		t.Fatalf("Node B Connect: %v", err)
	}
	defer connB.Close()

	if subs := connB.Subscriptions(); subs == nil || len(subs.Recipients) != 1 || subs.Recipients[0] != recipientB {
		t.Fatalf("Node B welcome subscriptions = %+v, want recipient %q", subs, recipientB)
	}

	// Send webhook for commitA in acme/repo-a adding tasks/01-deploy.task.md and .grain/processes/ci.process.md.
	webhookPayload, _ := json.Marshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": commitA,
		"repository": map[string]any{
			"full_name": "acme/repo-a",
			"html_url":  "https://github.com/acme/repo-a",
		},
		"commits": []map[string]any{
			{
				"id":    commitA,
				"added": []string{"tasks/01-deploy.task.md", ".grain/processes/ci.process.md", "docs/arch.md"},
			},
		},
	})

	// 1. Reject webhook with invalid signature.
	badReq, _ := http.NewRequest("POST", h.url+grainfiles.GitHubWebhookPath, bytes.NewReader(webhookPayload))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("X-GitHub-Event", "push")
	badReq.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("webhook with bad signature = %d, want 401", badResp.StatusCode)
	}

	// 2. Accept webhook with valid signature.
	goodReq, _ := http.NewRequest("POST", h.url+grainfiles.GitHubWebhookPath, bytes.NewReader(webhookPayload))
	goodReq.Header.Set("Content-Type", "application/json")
	goodReq.Header.Set("X-GitHub-Event", "push")
	goodReq.Header.Set("X-Hub-Signature-256", signGitHubWebhook(webhookSecret, webhookPayload))
	goodResp, err := http.DefaultClient.Do(goodReq)
	if err != nil {
		t.Fatal(err)
	}
	goodBody, _ := io.ReadAll(goodResp.Body)
	goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusOK {
		t.Fatalf("webhook with valid signature = %d (%s), want 200", goodResp.StatusCode, goodBody)
	}

	// 3. Verify Node B receives task_ready, process_updated, and process_triggered over its rendezvous connection.
	seenTypes := map[string]grainfiles.Event{}
	deadline := time.After(10 * time.Second)
	for len(seenTypes) < 3 {
		select {
		case ev := <-connB.GrainfileEvents():
			seenTypes[ev.Type] = ev
		case <-deadline:
			t.Fatalf("timed out waiting for Node B grainfile events; got %v", seenTypes)
		}
	}
	for _, want := range []string{
		grainfiles.EventTaskReady,
		grainfiles.EventProcessUpdated,
		grainfiles.EventProcessTriggered,
	} {
		if _, ok := seenTypes[want]; !ok {
			t.Errorf("Node B did not receive %q event; got %v", want, seenTypes)
		}
	}
	if got := seenTypes[grainfiles.EventTaskReady]; got.Recipient != recipientB || got.Path != "tasks/01-deploy.task.md" {
		t.Errorf("task_ready event = %+v", got)
	}

	// 4. Node B fetches the Grainfile and its same-repo reference through the Hub content proxy,
	// while unauthorized unreferenced or cross-repo fetches are rejected.
	fetchContent := func(token string, params url.Values) (int, string, http.Header) {
		req, err := http.NewRequest("GET", h.url+grainfiles.ContentPath+"?"+params.Encode(), nil)
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
		return resp.StatusCode, string(b), resp.Header
	}

	// 4a. Fetch the .task.md itself.
	code, bodyStr, hdr := fetchContent(mintB.Token, url.Values{
		"repo": {"https://github.com/acme/repo-a"},
		"path": {"tasks/01-deploy.task.md"},
		"ref":  {commitA},
	})
	if code != http.StatusOK || bodyStr != taskMD {
		t.Fatalf("Node B fetch Grainfile = %d, body=%q, want 200", code, bodyStr)
	}
	if hdr.Get("X-Grainfile-Path") != "tasks/01-deploy.task.md" || hdr.Get("X-Grainfile-Repo") != "https://github.com/acme/repo-a" {
		t.Errorf("Grainfile headers = %v", hdr)
	}

	// 4b. Fetch same-repo reference docs/arch.md declared in tasks/01-deploy.task.md.
	code, bodyStr, _ = fetchContent(mintB.Token, url.Values{
		"repo":      {"https://github.com/acme/repo-a"},
		"path":      {"docs/arch.md"},
		"ref":       {commitA},
		"grainfile": {"tasks/01-deploy.task.md"},
	})
	if code != http.StatusOK || bodyStr != archMD {
		t.Fatalf("Node B fetch authorized same-repo reference = %d, body=%q, want 200", code, bodyStr)
	}

	// 4c. Reject unreferenced same-repo file docs/unlisted.md.
	code, _, _ = fetchContent(mintB.Token, url.Values{
		"repo":      {"https://github.com/acme/repo-a"},
		"path":      {"docs/unlisted.md"},
		"ref":       {commitA},
		"grainfile": {"tasks/01-deploy.task.md"},
	})
	if code != http.StatusForbidden {
		t.Errorf("Node B fetch unlisted file = %d, want 403", code)
	}

	// 4d. Reject cross-repo reference fetch even when listed in frontmatter references.
	code, _, _ = fetchContent(mintB.Token, url.Values{
		"url":       {"https://github.com/acme/repo-c/blob/main/secret.md"},
		"ref":       {commitA},
		"grainfile": {"https://github.com/acme/repo-a/blob/main/tasks/01-deploy.task.md"},
	})
	if code != http.StatusForbidden {
		t.Errorf("Node B cross-repo reference fetch = %d, want 403", code)
	}

	// 4e. Reject fetch from Node C (not subscribed to acme/repo-a and not recipientB).
	code, _, _ = fetchContent(mintC.Token, url.Values{
		"repo": {"https://github.com/acme/repo-a"},
		"path": {"tasks/01-deploy.task.md"},
		"ref":  {commitA},
	})
	if code != http.StatusForbidden {
		t.Errorf("Node C unauthorized Grainfile fetch = %d, want 403", code)
	}

	// 5. Index dependent task in acme/repo-c while Node C is offline, then connect Node C
	// to verify catch-up and depends_on completion notification.
	webhookPayloadC, _ := json.Marshal(map[string]any{
		"ref":   "refs/heads/main",
		"after": commitC,
		"repository": map[string]any{
			"full_name": "acme/repo-c",
			"html_url":  "https://github.com/acme/repo-c",
		},
		"commits": []map[string]any{
			{
				"id":    commitC,
				"added": []string{"tasks/02-verify.task.md"},
			},
		},
	})
	reqC, _ := http.NewRequest("POST", h.url+grainfiles.GitHubWebhookPath, bytes.NewReader(webhookPayloadC))
	reqC.Header.Set("Content-Type", "application/json")
	reqC.Header.Set("X-GitHub-Event", "push")
	reqC.Header.Set("X-Hub-Signature-256", signGitHubWebhook(webhookSecret, webhookPayloadC))
	respC, err := http.DefaultClient.Do(reqC)
	if err != nil {
		t.Fatal(err)
	}
	respC.Body.Close()
	if respC.StatusCode != http.StatusOK {
		t.Fatalf("webhook C = %d, want 200", respC.StatusCode)
	}

	clientC := &rendezvous.Client{
		Hub:   h.url,
		Token: func(context.Context) (string, error) { return mintC.Token, nil },
		Node: registry.Node{
			ID:       "node-c",
			Repos:    []string{"acme/repo-c"},
			Slots:    1,
			Features: []string{registry.FeatureGrainfiles},
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{recipientC},
		},
	}
	connC, err := clientC.Connect(ctx)
	if err != nil {
		t.Fatalf("Node C Connect: %v", err)
	}
	defer connC.Close()

	// Node C should receive the catch-up task_ready event for tasks/02-verify.task.md.
	select {
	case ev := <-connC.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady || ev.Path != "tasks/02-verify.task.md" {
			t.Fatalf("Node C initial catch-up event = %+v, want task_ready for tasks/02-verify.task.md", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Node C catch-up event")
	}

	// Node B reports completion of tasks/01-deploy.task.md.
	depEvs, err := connB.CompleteGrainfile(
		ctx,
		"https://github.com/acme/repo-a/blob/main/tasks/01-deploy.task.md",
		commitA,
		"done",
		"staging deployment succeeded",
	)
	if err != nil {
		t.Fatalf("CompleteGrainfile: %v", err)
	}
	if len(depEvs) == 0 {
		t.Fatal("CompleteGrainfile emitted 0 events, want at least 1 dependency_completed event")
	}

	// Node C should receive live dependency_completed notification over its rendezvous connection.
	select {
	case ev := <-connC.GrainfileEvents():
		if ev.Type != grainfiles.EventDependencyCompleted {
			t.Fatalf("Node C event = %+v, want dependency_completed", ev)
		}
		if ev.DependencyURL != "https://github.com/acme/repo-a/blob/main/tasks/01-deploy.task.md" || ev.DependencyState != "done" {
			t.Errorf("dependency_completed event = %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Node C dependency_completed notification")
	}

	// 6. Verify human read-only inspection endpoint GET /v1/grainfiles scopes visibility by team and node ownership.
	var aliceView, bobView, outsiderView struct {
		Grainfiles    []grainfiles.IndexRecord   `json:"grainfiles"`
		Subscriptions []grainfiles.Subscriptions `json:"subscriptions"`
		Events        []grainfiles.Event         `json:"events"`
	}
	if code := call(t, "GET", h.url+grainfiles.GrainfilesPath+"?repo=acme/repo-a", "alice@example.com", nil, &aliceView); code != http.StatusOK {
		t.Fatalf("alice GET /v1/grainfiles = %d", code)
	}
	if len(aliceView.Grainfiles) != 2 {
		t.Errorf("alice grainfiles = %d, want 2", len(aliceView.Grainfiles))
	}

	if code := call(t, "GET", h.url+grainfiles.GrainfilesPath, "bob@example.com", nil, &bobView); code != http.StatusOK {
		t.Fatalf("bob GET /v1/grainfiles = %d", code)
	}
	if len(bobView.Subscriptions) != 1 || bobView.Subscriptions[0].NodeID != "node-b" {
		t.Errorf("bob subscriptions = %+v, want node-b", bobView.Subscriptions)
	}
	if len(bobView.Grainfiles) < 2 {
		t.Errorf("bob grainfiles (including recipientB targets) = %d, want >= 2", len(bobView.Grainfiles))
	}

	if code := call(t, "GET", h.url+grainfiles.GrainfilesPath, "outsider@example.com", nil, &outsiderView); code != http.StatusOK {
		t.Fatalf("outsider GET /v1/grainfiles = %d", code)
	}
	if len(outsiderView.Grainfiles) != 0 || len(outsiderView.Subscriptions) != 0 || len(outsiderView.Events) != 0 {
		t.Errorf("outsider view leaked data: %+v", outsiderView)
	}

	// 7. Verify GET /v1/grainfiles query filters (kind, draft, url) and GET /v1/nodes recipient & subscription enrichment.
	var kindFiltered, urlFiltered struct {
		Grainfiles    []grainfiles.IndexRecord   `json:"grainfiles"`
		Subscriptions []grainfiles.Subscriptions `json:"subscriptions"`
		Events        []grainfiles.Event         `json:"events"`
	}
	if code := call(t, "GET", h.url+grainfiles.GrainfilesPath+"?kind=process&draft=false", "alice@example.com", nil, &kindFiltered); code != http.StatusOK {
		t.Fatalf("alice GET /v1/grainfiles?kind=process&draft=false = %d", code)
	}
	if len(kindFiltered.Grainfiles) != 1 || kindFiltered.Grainfiles[0].Kind != grainfiles.KindProcess {
		t.Errorf("process grainfiles = %+v, want 1 process", kindFiltered.Grainfiles)
	}
	if len(kindFiltered.Grainfiles[0].On.Push.Branches) != 1 || kindFiltered.Grainfiles[0].On.Push.Branches[0] != "main" {
		t.Errorf("process grainfile On = %+v, want push branch main", kindFiltered.Grainfiles[0].On)
	}

	if code := call(t, "GET", h.url+grainfiles.GrainfilesPath+"?url=https://github.com/acme/repo-a/blob/main/tasks/01-deploy.task.md", "alice@example.com", nil, &urlFiltered); code != http.StatusOK {
		t.Fatalf("alice GET /v1/grainfiles?url=... = %d", code)
	}
	if len(urlFiltered.Grainfiles) != 1 || urlFiltered.Grainfiles[0].Path != "tasks/01-deploy.task.md" {
		t.Errorf("urlFiltered grainfiles = %+v, want tasks/01-deploy.task.md", urlFiltered.Grainfiles)
	}

	var nodesResp struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	if code := call(t, "GET", h.url+rendezvous.DirectoryPath, "bob@example.com", nil, &nodesResp); code != http.StatusOK {
		t.Fatalf("bob GET /v1/nodes = %d", code)
	}
	if len(nodesResp.Nodes) != 2 || nodesResp.Nodes[0].ID != "node-b" || nodesResp.Nodes[1].ID != "node-c" {
		t.Fatalf("bob nodes = %+v, want [node-b, node-c]", nodesResp.Nodes)
	}
	if len(nodesResp.Nodes[0].Recipients) != 1 || nodesResp.Nodes[0].Recipients[0] != recipientB {
		t.Errorf("node-b recipients on GET /v1/nodes = %v, want [%s]", nodesResp.Nodes[0].Recipients, recipientB)
	}
	if len(nodesResp.Nodes[1].Recipients) != 1 || nodesResp.Nodes[1].Recipients[0] != recipientC {
		t.Errorf("node-c recipients on GET /v1/nodes = %v, want [%s]", nodesResp.Nodes[1].Recipients, recipientC)
	}
}
