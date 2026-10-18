// Package simtest runs end-to-end integration tests for Grain Hub (hub/server)
// against real bare git repositories and the local Git + GitHub simulator
// (internal/githubsim), exercising both SQLite and PostgreSQL (internal/pgtest).
package simtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/internal/githubsim"
	"github.com/bwsalmon/silo/internal/pgtest"
)

const (
	testUserHeader    = "X-Test-User"
	testAdminEmail    = "admin@example.com"
	testWebhookSecret = "e2e-webhook-hmac-secret"
	testGitHubToken   = "ghp_e2e_sim_token"
)

type dbBackend struct {
	name string
	open func(t *testing.T) string
}

func dbBackends() []dbBackend {
	return []dbBackend{
		{
			name: "sqlite",
			open: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "hub-e2e.db")
			},
		},
		{
			name: "postgres",
			open: func(t *testing.T) string {
				return pgtest.Start(t).URL
			},
		},
	}
}

type e2eEnv struct {
	t           *testing.T
	sim         *githubsim.Server
	hub         *server.Hub
	hubSrv      *httptest.Server
	hubURL      string
	clockOffset atomic.Int64
}

func (e *e2eEnv) now() time.Time {
	return time.Now().UTC().Add(time.Duration(e.clockOffset.Load()))
}

func (e *e2eEnv) advanceClock(d time.Duration) time.Time {
	e.clockOffset.Add(int64(d))
	return e.now()
}

type e2eEnvOptions struct {
	useGitHubApp bool
}

func newE2EEnv(t *testing.T, dbURL string, opts e2eEnvOptions) *e2eEnv {
	t.Helper()

	var (
		privPEM []byte
		appID   int64
		tok     string
	)
	if opts.useGitHubApp {
		privPEM, _ = githubsim.GenerateAppKey(t)
		appID = 424242
	} else {
		tok = testGitHubToken
	}

	sim := githubsim.New(t, githubsim.Options{
		WebhookSecret: testWebhookSecret,
		Token:         tok,
		AppID:         appID,
		AppPrivateKey: privPEM,
	})

	var logMu sync.Mutex
	var testDone bool
	t.Cleanup(func() {
		logMu.Lock()
		testDone = true
		logMu.Unlock()
	})
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		if !testDone {
			t.Logf(format, args...)
		}
	}

	cfg := server.Config{
		Database:            dbURL,
		DevUserHeader:       testUserHeader,
		HubAdmins:           []string{"user:" + testAdminEmail},
		Heartbeat:           2 * time.Second,
		WebhookSecret:       testWebhookSecret,
		GitHubAPIURL:        sim.URL,
		GitHubToken:         tok,
		GitHubAppID:         appID,
		GitHubAppPrivateKey: privPEM,
		Logf:                logf,
	}

	ctx := context.Background()
	h, err := server.New(ctx, cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.Rendezvous.Shutdown(sctx, "test cleanup")
		srv.Close()
		_ = h.Close()
	})

	sim.SetWebhook(srv.URL+grainfiles.GitHubWebhookPath, testWebhookSecret)

	env := &e2eEnv{
		t:      t,
		sim:    sim,
		hub:    h,
		hubSrv: srv,
		hubURL: srv.URL,
	}
	h.Grainfiles.Now = env.now
	return env
}

func (e *e2eEnv) apiCall(method, pathOrURL, user string, body any, out any) int {
	e.t.Helper()
	fullURL := pathOrURL
	if !strings.HasPrefix(pathOrURL, "http://") && !strings.HasPrefix(pathOrURL, "https://") {
		fullURL = e.hubURL + pathOrURL
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fullURL, rd)
	if err != nil {
		e.t.Fatalf("http.NewRequest: %v", err)
	}
	if user != "" {
		req.Header.Set(testUserHeader, user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, fullURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 && out != nil {
		e.t.Fatalf("%s %s returned %s: %s", method, fullURL, resp.Status, string(raw))
	}
	if out != nil && resp.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("decode %s %s response: %v (body=%s)", method, fullURL, err, string(raw))
		}
	}
	return resp.StatusCode
}

func (e *e2eEnv) setupTeamAndMintNodeToken(teamID, memberEmail string, repos []string, nodeID string) string {
	e.t.Helper()
	_ = e.apiCall(http.MethodPost, "/v1/teams", testAdminEmail, map[string]string{
		"id":   teamID,
		"name": "Team " + teamID,
	}, nil)
	e.apiCall(http.MethodPut, "/v1/teams/"+teamID+"/members/user:"+memberEmail, testAdminEmail, map[string]string{
		"role": "member",
	}, &struct{}{})
	for _, r := range repos {
		e.apiCall(http.MethodPut, "/v1/teams/"+teamID+"/repos/"+r, testAdminEmail, nil, &struct{}{})
	}
	var minted struct {
		Token string `json:"token"`
	}
	e.apiCall(http.MethodPost, "/v1/node-tokens", memberEmail, map[string]string{
		"node": nodeID,
	}, &minted)
	if minted.Token == "" {
		e.t.Fatalf("minted empty token for node %s", nodeID)
	}
	return minted.Token
}

func (e *e2eEnv) connectNode(nodeID, token string, repos []string, subs *grainfiles.Subscriptions) *rendezvous.Conn {
	e.t.Helper()
	cl := &rendezvous.Client{
		Hub:           e.hubURL,
		Token:         func(context.Context) (string, error) { return token, nil },
		Node:          registry.Node{ID: nodeID, Repos: repos, Slots: 2},
		Subscriptions: subs,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := cl.Connect(ctx)
	if err != nil {
		e.t.Fatalf("connect node %s: %v", nodeID, err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (e *e2eEnv) queryGrainfiles(user string, params map[string]string) grainfiles.GrainfilesResponse {
	e.t.Helper()
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	u := grainfiles.GrainfilesPath
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	var resp grainfiles.GrainfilesResponse
	e.apiCall(http.MethodGet, u, user, nil, &resp)
	return resp
}

func waitForEvent(t *testing.T, ch <-chan grainfiles.Event, timeout time.Duration) grainfiles.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("grainfile event channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for grainfile_event", timeout)
		return grainfiles.Event{}
	}
}

func assertNoEvent(t *testing.T, ch <-chan grainfiles.Event, wait time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected grainfile_event received: %+v", ev)
		}
	case <-time.After(wait):
	}
}

// TestEndToEndSearchAndFiltering populates multiple real git repositories with
// .task.md and .process.md files across several recipient deployments,
// requestor identities (Name <email> and https:// URLs), draft: true/false,
// not_before, on triggers, and gate: true/false, then exercises every
// search/filter combination on GET /v1/grainfiles and verifies exact results,
// deterministic ordering, and detail metadata.
func TestEndToEndSearchAndFiltering(t *testing.T) {
	for _, backend := range dbBackends() {
		t.Run(backend.name, func(t *testing.T) {
			env := newE2EEnv(t, backend.open(t), e2eEnvOptions{})

			const (
				recipientAlpha = "https://github.com/acme/grain-alpha"
				recipientBeta  = "https://github.com/acme/grain-beta"
				recipientGamma = "https://github.com/acme/grain-gamma"
				reqHuman       = "Alice Smith <alice@example.com>"
				reqURL         = "https://github.com/acme/grain-orchestrator"
			)

			// Connect two simulated nodes declaring their recipient URLs and repo subscriptions
			// so subscribed_nodes is populated on GET /v1/grainfiles.
			tokAlpha := env.setupTeamAndMintNodeToken("acme-team", "alice@example.com", []string{"acme/platform", "acme/docs"}, "node-alpha")
			tokBeta := env.setupTeamAndMintNodeToken("beta-team", "bob@example.com", []string{"beta/service"}, "node-beta")

			env.connectNode("node-alpha", tokAlpha, []string{"acme/platform", "acme/docs"}, &grainfiles.Subscriptions{
				Recipients: []string{recipientAlpha},
				Repos:      []string{"https://github.com/acme/docs"},
			})
			env.connectNode("node-beta", tokBeta, []string{"beta/service"}, &grainfiles.Subscriptions{
				Recipients: []string{recipientBeta},
				Grainfiles: []string{"https://github.com/acme/platform/blob/main/processes/release.process.md"},
			})

			// 1. Populate Repo 1: acme/platform
			shaPlatform, whPlatform := env.sim.MustCommitAndPush(t, "acme", "platform", "main", "Add platform tasks and processes", map[string]string{
				"docs/spec.md":      "# Platform Spec\n",
				"tasks/schema.json": `{"type":"object"}`,
				"tasks/migrate.task.md": `---
title: Migrate Platform Schema
recipient: https://github.com/acme/grain-alpha
requestor: Alice Smith <alice@example.com>
draft: false
references:
  - /docs/spec.md
  - ./schema.json
  - https://github.com/org/repo-c/blob/main/secret.md
depends_on:
  - https://github.com/beta/service/blob/main/tasks/bootstrap.task.md
---
Run schema migration according to spec.
`,
				"tasks/draft-cleanup.task.md": `---
title: Draft Cleanup Task
recipient: https://github.com/acme/grain-beta
requestor: https://github.com/acme/grain-orchestrator
draft: true
not_before: 2099-01-01T00:00:00Z
---
Draft cleanup work.
`,
				"processes/release.process.md": `---
title: Release Gate Process
recipient: https://github.com/acme/grain-alpha
requestor: https://github.com/acme/grain-orchestrator
gate: true
trigger_with:
  - /processes/notify.process.md
on:
  push:
    branches: [main]
    paths: ["src/**"]
    ignore: ["src/**/*.md"]
  pull_request:
    branches: [main]
---
Verify release readiness.
`,
				"processes/notify.process.md": `---
title: Release Notification Process
recipient: https://github.com/acme/grain-beta
requestor: Alice Smith <alice@example.com>
gate: false
on:
  merge:
    branches: [main]
---
Post release notes.
`,
			}, nil)
			if len(whPlatform.Indexed) != 4 {
				t.Fatalf("expected 4 indexed grainfiles in acme/platform, got %d", len(whPlatform.Indexed))
			}

			// Sleep briefly so timestamps between repositories are strictly ordered.
			time.Sleep(15 * time.Millisecond)

			// 2. Populate Repo 2: acme/docs
			_, whDocs := env.sim.MustCommitAndPush(t, "acme", "docs", "main", "Add docs review process", map[string]string{
				"processes/spellcheck.process.md": `---
title: Docs Spellcheck Gate
recipient: https://github.com/acme/grain-gamma
requestor: Alice Smith <alice@example.com>
gate: true
draft: false
on:
  pull_request:
    branches: [main]
    paths: ["guides/**"]
---
Check spelling across guides.
`,
			}, nil)
			if len(whDocs.Indexed) != 1 {
				t.Fatalf("expected 1 indexed grainfile in acme/docs, got %d", len(whDocs.Indexed))
			}

			time.Sleep(15 * time.Millisecond)

			// 3. Populate Repo 3: beta/service
			_, whBeta := env.sim.MustCommitAndPush(t, "beta", "service", "main", "Add beta bootstrap task", map[string]string{
				"tasks/bootstrap.task.md": `---
title: Bootstrap Beta Service
recipient: https://github.com/acme/grain-beta
requestor: https://github.com/acme/grain-orchestrator
draft: false
not_before: 2099-06-15T12:00:00Z
---
Bootstrap beta service cluster.
`,
			}, nil)
			if len(whBeta.Indexed) != 1 {
				t.Fatalf("expected 1 indexed grainfile in beta/service, got %d", len(whBeta.Indexed))
			}

			// Verify unfiltered search as hub admin returns all 6 grainfiles in deterministic order:
			// newest updated_at first (beta/service -> acme/docs -> acme/platform), and within
			// acme/platform ordered by (repo ASC, path ASC).
			allResp := env.queryGrainfiles(testAdminEmail, nil)
			var gotPaths []string
			for _, g := range allResp.Grainfiles {
				gotPaths = append(gotPaths, g.Repo+":"+g.Path)
			}
			wantPaths := []string{
				"https://github.com/beta/service:tasks/bootstrap.task.md",
				"https://github.com/acme/docs:processes/spellcheck.process.md",
				"https://github.com/acme/platform:processes/notify.process.md",
				"https://github.com/acme/platform:processes/release.process.md",
				"https://github.com/acme/platform:tasks/draft-cleanup.task.md",
				"https://github.com/acme/platform:tasks/migrate.task.md",
			}
			if !slices.Equal(gotPaths, wantPaths) {
				t.Fatalf("unfiltered ordering mismatch:\n got: %v\nwant: %v", gotPaths, wantPaths)
			}

			// Verify team visibility: alice@example.com (in acme-team) sees only acme/docs and acme/platform (5 files).
			aliceResp := env.queryGrainfiles("alice@example.com", nil)
			if len(aliceResp.Grainfiles) != 5 {
				t.Fatalf("expected alice to see 5 acme grainfiles, got %d", len(aliceResp.Grainfiles))
			}

			// Filter by kind=task (3 tasks) and kind=process (3 processes).
			tasksOnly := env.queryGrainfiles(testAdminEmail, map[string]string{"kind": "task"})
			if len(tasksOnly.Grainfiles) != 3 {
				t.Fatalf("kind=task: expected 3, got %d", len(tasksOnly.Grainfiles))
			}
			for _, g := range tasksOnly.Grainfiles {
				if g.Kind != grainfiles.KindTask {
					t.Fatalf("expected KindTask, got %s", g.Kind)
				}
			}

			procsOnly := env.queryGrainfiles(testAdminEmail, map[string]string{"kind": "process"})
			if len(procsOnly.Grainfiles) != 3 {
				t.Fatalf("kind=process: expected 3, got %d", len(procsOnly.Grainfiles))
			}

			// Filter by recipient.
			alphaOnly := env.queryGrainfiles(testAdminEmail, map[string]string{"recipient": recipientAlpha})
			if len(alphaOnly.Grainfiles) != 2 {
				t.Fatalf("recipient=alpha: expected 2, got %d", len(alphaOnly.Grainfiles))
			}
			betaOnly := env.queryGrainfiles(testAdminEmail, map[string]string{"recipient": recipientBeta})
			if len(betaOnly.Grainfiles) != 3 {
				t.Fatalf("recipient=beta: expected 3, got %d", len(betaOnly.Grainfiles))
			}
			gammaOnly := env.queryGrainfiles(testAdminEmail, map[string]string{"recipient": recipientGamma})
			if len(gammaOnly.Grainfiles) != 1 || gammaOnly.Grainfiles[0].Path != "processes/spellcheck.process.md" {
				t.Fatalf("recipient=gamma: unexpected results %+v", gammaOnly.Grainfiles)
			}

			// Filter by repo (both full URL and owner/repo shorthand).
			repoPlatform := env.queryGrainfiles(testAdminEmail, map[string]string{"repo": "acme/platform"})
			if len(repoPlatform.Grainfiles) != 4 {
				t.Fatalf("repo=acme/platform: expected 4, got %d", len(repoPlatform.Grainfiles))
			}
			repoBeta := env.queryGrainfiles(testAdminEmail, map[string]string{"repo": "https://github.com/beta/service"})
			if len(repoBeta.Grainfiles) != 1 || repoBeta.Grainfiles[0].Path != "tasks/bootstrap.task.md" {
				t.Fatalf("repo=beta/service: unexpected results %+v", repoBeta.Grainfiles)
			}

			// Filter by draft=true (1) and draft=false (5).
			draftTrue := env.queryGrainfiles(testAdminEmail, map[string]string{"draft": "true"})
			if len(draftTrue.Grainfiles) != 1 || draftTrue.Grainfiles[0].Path != "tasks/draft-cleanup.task.md" {
				t.Fatalf("draft=true: unexpected results %+v", draftTrue.Grainfiles)
			}
			draftFalse := env.queryGrainfiles(testAdminEmail, map[string]string{"draft": "false"})
			if len(draftFalse.Grainfiles) != 5 {
				t.Fatalf("draft=false: expected 5, got %d", len(draftFalse.Grainfiles))
			}

			// Filter by gate=true (2) and gate=false (4).
			gateTrue := env.queryGrainfiles(testAdminEmail, map[string]string{"gate": "true"})
			if len(gateTrue.Grainfiles) != 2 {
				t.Fatalf("gate=true: expected 2, got %d", len(gateTrue.Grainfiles))
			}
			gateFalse := env.queryGrainfiles(testAdminEmail, map[string]string{"gate": "false"})
			if len(gateFalse.Grainfiles) != 4 {
				t.Fatalf("gate=false: expected 4, got %d", len(gateFalse.Grainfiles))
			}

			// Filter by requestor (both Name <email> and https:// URL).
			reqHumanResp := env.queryGrainfiles(testAdminEmail, map[string]string{"requestor": reqHuman})
			if len(reqHumanResp.Grainfiles) != 3 {
				t.Fatalf("requestor=%q: expected 3, got %d", reqHuman, len(reqHumanResp.Grainfiles))
			}
			reqURLResp := env.queryGrainfiles(testAdminEmail, map[string]string{"requestor": reqURL})
			if len(reqURLResp.Grainfiles) != 3 {
				t.Fatalf("requestor=%q: expected 3, got %d", reqURL, len(reqURLResp.Grainfiles))
			}

			// Combined filters: repo + kind + draft + recipient + gate.
			comb1 := env.queryGrainfiles(testAdminEmail, map[string]string{
				"repo":  "acme/platform",
				"kind":  "task",
				"draft": "false",
			})
			if len(comb1.Grainfiles) != 1 || comb1.Grainfiles[0].Path != "tasks/migrate.task.md" {
				t.Fatalf("combined repo+kind+draft: unexpected %+v", comb1.Grainfiles)
			}

			comb2 := env.queryGrainfiles(testAdminEmail, map[string]string{
				"recipient": recipientAlpha,
				"kind":      "process",
				"gate":      "true",
			})
			if len(comb2.Grainfiles) != 1 || comb2.Grainfiles[0].Path != "processes/release.process.md" {
				t.Fatalf("combined recipient+kind+gate: unexpected %+v", comb2.Grainfiles)
			}

			// Inspect exact detail metadata on tasks/migrate.task.md via url filter.
			migrateURL := fmt.Sprintf("https://github.com/acme/platform/blob/%s/tasks/migrate.task.md", shaPlatform)
			byURL := env.queryGrainfiles(testAdminEmail, map[string]string{"url": migrateURL})
			if len(byURL.Grainfiles) != 1 {
				t.Fatalf("url=%s: expected 1 result, got %d", migrateURL, len(byURL.Grainfiles))
			}
			mig := byURL.Grainfiles[0]
			if mig.Title != "Migrate Platform Schema" || mig.Recipient != recipientAlpha || mig.Requestor != reqHuman {
				t.Fatalf("unexpected migrate metadata: %+v", mig)
			}
			// references includes all 3 declared refs; authorized_references includes only the 2 same-repo refs.
			if len(mig.References) != 3 {
				t.Fatalf("expected 3 references on migrate.task.md, got %v", mig.References)
			}
			wantAuthRefs := []string{
				"https://github.com/acme/platform/blob/main/docs/spec.md",
				"https://github.com/acme/platform/blob/main/tasks/schema.json",
			}
			if !slices.Equal(mig.AuthorizedReferences, wantAuthRefs) {
				t.Fatalf("authorized_references = %v, want %v", mig.AuthorizedReferences, wantAuthRefs)
			}
			if len(mig.DependsOn) != 1 || !strings.Contains(mig.DependsOn[0], "beta/service/blob/main/tasks/bootstrap.task.md") {
				t.Fatalf("unexpected depends_on: %v", mig.DependsOn)
			}
			if !slices.Equal(mig.SubscribedNodes, []string{"node-alpha"}) {
				t.Fatalf("migrate subscribed_nodes = %v, want [node-alpha]", mig.SubscribedNodes)
			}
			if len(mig.RecentEvents) == 0 || mig.RecentEvents[0].Type != grainfiles.EventTaskReady {
				t.Fatalf("expected recent task_ready event on migrate.task.md, got %+v", mig.RecentEvents)
			}

			// Inspect exact detail metadata on processes/release.process.md.
			byPath := env.queryGrainfiles(testAdminEmail, map[string]string{
				"repo": "acme/platform",
				"path": "processes/release.process.md",
			})
			if len(byPath.Grainfiles) != 1 {
				t.Fatalf("expected 1 result for release.process.md, got %d", len(byPath.Grainfiles))
			}
			rel := byPath.Grainfiles[0]
			if !rel.Gate || len(rel.TriggerWith) != 1 || !strings.HasSuffix(rel.TriggerWith[0], "/processes/notify.process.md") {
				t.Fatalf("unexpected release.process.md detail: %+v", rel)
			}
			if rel.On.Push == nil || rel.On.PullRequest == nil {
				t.Fatalf("expected on.push and on.pull_request on release.process.md, got %+v", rel.On)
			}
			// Both node-alpha (by recipient) and node-beta (by explicit grainfile subscription) subscribe to release.process.md.
			if !slices.Equal(rel.SubscribedNodes, []string{"node-alpha", "node-beta"}) {
				t.Fatalf("release.process.md subscribed_nodes = %v, want [node-alpha node-beta]", rel.SubscribedNodes)
			}
		})
	}
}

// TestEndToEndLiveSubscriptionsDraftNotBeforeAndCatchup connects multiple
// simulated Grain deployments over real WebSocket connections, verifies live
// grainfile_event delivery to matching recipients only, draft: true -> false
// promotion, not_before deferral and release, and offline disconnect + cursor
// catch-up without duplicates or gaps.
func TestEndToEndLiveSubscriptionsDraftNotBeforeAndCatchup(t *testing.T) {
	for _, backend := range dbBackends() {
		t.Run(backend.name, func(t *testing.T) {
			env := newE2EEnv(t, backend.open(t), e2eEnvOptions{})

			const (
				recA = "https://github.com/acme/grain-a"
				recB = "https://github.com/acme/grain-b"
			)

			tokA := env.setupTeamAndMintNodeToken("team-a", "alice@example.com", []string{"acme/app"}, "node-a")
			tokB := env.setupTeamAndMintNodeToken("team-b", "bob@example.com", []string{"beta/app"}, "node-b")

			connA := env.connectNode("node-a", tokA, []string{"acme/app"}, &grainfiles.Subscriptions{
				Recipients: []string{recA},
			})
			connB := env.connectNode("node-b", tokB, []string{"beta/app"}, &grainfiles.Subscriptions{
				Recipients: []string{recB},
			})

			// 1. Push a ready task for Node A and a draft task for Node B.
			env.sim.MustCommitAndPush(t, "acme", "app", "main", "Push ready task A and draft task B", map[string]string{
				"tasks/ready-a.task.md": `---
title: Ready Task for A
recipient: https://github.com/acme/grain-a
draft: false
---
Execute immediately on A.
`,
				"tasks/staged-b.task.md": `---
title: Staged Draft Task for B
recipient: https://github.com/acme/grain-b
draft: true
---
Still in draft; do not emit task_ready yet.
`,
			}, nil)

			// Node A receives task_ready for tasks/ready-a.task.md; Node B receives nothing because staged-b is draft: true.
			evA1 := waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			if evA1.Type != grainfiles.EventTaskReady || evA1.Path != "tasks/ready-a.task.md" || evA1.Recipient != recA {
				t.Fatalf("unexpected event for Node A: %+v", evA1)
			}
			assertNoEvent(t, connB.GrainfileEvents(), 150*time.Millisecond)

			// 2. Promote tasks/staged-b.task.md from draft: true to draft: false in a subsequent commit.
			env.sim.MustCommitAndPush(t, "acme", "app", "main", "Undraft task B", map[string]string{
				"tasks/staged-b.task.md": `---
title: Staged Draft Task for B
recipient: https://github.com/acme/grain-b
draft: false
---
Now ready for B.
`,
			}, nil)

			evB1 := waitForEvent(t, connB.GrainfileEvents(), 5*time.Second)
			if evB1.Type != grainfiles.EventTaskReady || evB1.Path != "tasks/staged-b.task.md" || evB1.Recipient != recB {
				t.Fatalf("unexpected event for Node B after undrafting: %+v", evB1)
			}
			assertNoEvent(t, connA.GrainfileEvents(), 100*time.Millisecond)

			// 3. Push a task for Node A with not_before in the future; verify it is held until not_before is reached.
			notBeforeTime := env.now().Add(10 * time.Second).Truncate(time.Second)
			env.sim.MustCommitAndPush(t, "acme", "app", "main", "Schedule future task for A", map[string]string{
				"tasks/scheduled-a.task.md": fmt.Sprintf(`---
title: Scheduled Task for A
recipient: https://github.com/acme/grain-a
not_before: %s
---
Run after not_before timestamp.
`, notBeforeTime.Format(time.RFC3339)),
			}, nil)

			// While before notBeforeTime, no event is emitted.
			assertNoEvent(t, connA.GrainfileEvents(), 150*time.Millisecond)

			// Advance Hub clock past notBeforeTime and promote due grainfiles.
			nowAfter := env.advanceClock(15 * time.Second)
			promoted, err := env.hub.Grainfiles.PromoteDeferred(context.Background(), nowAfter)
			if err != nil {
				t.Fatalf("PromoteDeferred: %v", err)
			}
			if len(promoted) != 1 || promoted[0].Path != "tasks/scheduled-a.task.md" {
				t.Fatalf("expected 1 promoted not_before event, got %+v", promoted)
			}
			env.hub.Rendezvous.NotifyGrainfiles(promoted...)

			evA2 := waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			if evA2.Type != grainfiles.EventTaskReady || evA2.Path != "tasks/scheduled-a.task.md" {
				t.Fatalf("expected promoted task_ready for tasks/scheduled-a.task.md, got %+v", evA2)
			}

			// 4. Disconnect Node B, push 2 new commits for Node B while it is offline,
			// then reconnect Node B with its last cursor (evB1.Seq) and verify catch-up delivery
			// without duplicates or gaps.
			lastCursorB := evB1.Seq
			if err := connB.Close(); err != nil {
				t.Fatalf("connB.Close: %v", err)
			}
			time.Sleep(50 * time.Millisecond)

			env.sim.MustCommitAndPush(t, "acme", "app", "main", "Offline commit 1 for B", map[string]string{
				"tasks/offline-1.task.md": `---
title: Offline Task 1
recipient: https://github.com/acme/grain-b
---
First offline task.
`,
			}, nil)

			env.sim.MustCommitAndPush(t, "acme", "app", "main", "Offline commit 2 for B", map[string]string{
				"processes/offline-2.process.md": `---
title: Offline Process 2
recipient: https://github.com/acme/grain-b
on:
  push:
    branches: [main]
---
Second offline process (emits process_updated + process_triggered).
`,
			}, nil)

			// Reconnect Node B passing Cursor: lastCursorB in Hello.
			connB2 := env.connectNode("node-b", tokB, []string{"beta/app"}, &grainfiles.Subscriptions{
				Recipients: []string{recB},
				Cursor:     lastCursorB,
			})

			// Expect exactly 3 catch-up events in strictly increasing Seq order:
			// 1. task_ready for tasks/offline-1.task.md
			// 2. process_updated for processes/offline-2.process.md
			// 3. process_triggered for processes/offline-2.process.md
			evCatch1 := waitForEvent(t, connB2.GrainfileEvents(), 5*time.Second)
			evCatch2 := waitForEvent(t, connB2.GrainfileEvents(), 5*time.Second)
			evCatch3 := waitForEvent(t, connB2.GrainfileEvents(), 5*time.Second)

			if evCatch1.Type != grainfiles.EventTaskReady || evCatch1.Path != "tasks/offline-1.task.md" {
				t.Fatalf("catchup event 1 mismatch: %+v", evCatch1)
			}
			if evCatch2.Type != grainfiles.EventProcessUpdated || evCatch2.Path != "processes/offline-2.process.md" {
				t.Fatalf("catchup event 2 mismatch: %+v", evCatch2)
			}
			if evCatch3.Type != grainfiles.EventProcessTriggered || evCatch3.Path != "processes/offline-2.process.md" {
				t.Fatalf("catchup event 3 mismatch: %+v", evCatch3)
			}
			if !(lastCursorB < evCatch1.Seq && evCatch1.Seq < evCatch2.Seq && evCatch2.Seq < evCatch3.Seq) {
				t.Fatalf("expected strictly increasing sequence numbers: %d < %d < %d < %d",
					lastCursorB, evCatch1.Seq, evCatch2.Seq, evCatch3.Seq)
			}

			// Calling Catchup explicitly with evCatch3.Seq returns 0 events (no duplicates).
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			more, newCur, err := connB2.Catchup(ctx, evCatch3.Seq)
			if err != nil {
				t.Fatalf("connB2.Catchup: %v", err)
			}
			if len(more) != 0 || newCur != evCatch3.Seq {
				t.Fatalf("expected 0 duplicate events after catchup, got %d (cursor=%d)", len(more), newCur)
			}
		})
	}
}

// TestEndToEndTriggersTriggerWithAndCrossDeploymentDependsOn verifies on: push,
// on: pull_request, and on: merge branch/path/ignore filters, trigger_with
// suite co-triggers in topological dependency order, and cross-deployment
// depends_on completion notifications.
func TestEndToEndTriggersTriggerWithAndCrossDeploymentDependsOn(t *testing.T) {
	for _, backend := range dbBackends() {
		t.Run(backend.name, func(t *testing.T) {
			env := newE2EEnv(t, backend.open(t), e2eEnvOptions{})

			const (
				recA = "https://github.com/acme/grain-a"
				recB = "https://github.com/acme/grain-b"
			)

			tokA := env.setupTeamAndMintNodeToken("team-a", "alice@example.com", []string{"acme/core", "acme/upstream"}, "node-a")
			tokB := env.setupTeamAndMintNodeToken("team-b", "bob@example.com", []string{"acme/downstream"}, "node-b")

			connA := env.connectNode("node-a", tokA, []string{"acme/core", "acme/upstream"}, &grainfiles.Subscriptions{
				Recipients: []string{recA},
			})
			connB := env.connectNode("node-b", tokB, []string{"acme/downstream"}, &grainfiles.Subscriptions{
				Recipients: []string{recB},
			})

			// 1. Commit processes with push, pull_request, and merge triggers, plus a trigger_with suite
			// where lint.process.md and smoke.process.md both declare trigger_with: [/processes/entry.process.md]
			// and smoke.process.md depends_on lint.process.md.
			env.sim.MustCommitAndPush(t, "acme", "core", "main", "Register trigger processes and suite", map[string]string{
				"processes/entry.process.md": `---
title: Entry Push Process
recipient: https://github.com/acme/grain-a
on:
  push:
    branches: ["main", "release/*"]
    paths: ["src/**"]
    ignore: ["src/**/*.gen.go", "docs/**"]
---
Run on matching push.
`,
				"processes/lint.process.md": `---
title: Companion Lint Process
recipient: https://github.com/acme/grain-a
trigger_with:
  - /processes/entry.process.md
---
Run lint when entry triggers.
`,
				"processes/smoke.process.md": `---
title: Companion Smoke Process
recipient: https://github.com/acme/grain-b
trigger_with:
  - /processes/entry.process.md
depends_on:
  - /processes/lint.process.md
---
Run smoke suite after lint when entry triggers.
`,
				"processes/pr-api.process.md": `---
title: PR API Review Process
recipient: https://github.com/acme/grain-a
on:
  pull_request:
    branches: ["main"]
    paths: ["api/**"]
---
Review API changes on pull requests.
`,
				"processes/post-merge.process.md": `---
title: Post Merge Deploy Process
recipient: https://github.com/acme/grain-b
on:
  merge:
    branches: ["main"]
---
Deploy merged PR to staging.
`,
			}, nil)

			// Drain initial indexing events (5 process_updated: 3 for Node A, 2 for Node B).
			for range 3 {
				_ = waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			}
			for range 2 {
				_ = waitForEvent(t, connB.GrainfileEvents(), 5*time.Second)
			}

			// 2. Push ignored files (docs/readme.md and src/types.gen.go) on main -> no triggers should fire.
			_, whIgnored := env.sim.MustCommitAndPush(t, "acme", "core", "main", "Touch ignored files", map[string]string{
				"docs/readme.md":   "updated docs\n",
				"src/types.gen.go": "// generated\n",
			}, nil)
			if len(whIgnored.Events) != 0 {
				t.Fatalf("expected 0 trigger events for ignored files, got %+v", whIgnored.Events)
			}
			assertNoEvent(t, connA.GrainfileEvents(), 100*time.Millisecond)
			assertNoEvent(t, connB.GrainfileEvents(), 100*time.Millisecond)

			// 3. Push matching file on non-matching branch (feat/experiment) -> no triggers should fire.
			_, whWrongBranch := env.sim.MustCommitAndPush(t, "acme", "core", "feat/experiment", "Touch src on feat branch", map[string]string{
				"src/handler.go": "package src\n",
			}, nil)
			if len(whWrongBranch.Events) != 0 {
				t.Fatalf("expected 0 trigger events on non-matching branch, got %+v", whWrongBranch.Events)
			}

			// 4. Push matching file (src/handler.go) on main -> entry.process.md fires, and trigger_with
			// co-triggers lint.process.md and smoke.process.md in dependency order:
			// entry.process.md -> lint.process.md -> smoke.process.md.
			_, whPush := env.sim.MustCommitAndPush(t, "acme", "core", "main", "Update src/handler.go on main", map[string]string{
				"src/handler.go": "package src // v2\n",
			}, nil)
			var firedPaths []string
			for _, ev := range whPush.Events {
				if ev.Type == grainfiles.EventProcessTriggered {
					firedPaths = append(firedPaths, ev.Path)
				}
			}
			wantSuiteOrder := []string{
				"processes/entry.process.md",
				"processes/lint.process.md",
				"processes/smoke.process.md",
			}
			if !slices.Equal(firedPaths, wantSuiteOrder) {
				t.Fatalf("trigger_with suite order = %v, want %v", firedPaths, wantSuiteOrder)
			}

			// Verify live WebSocket delivery of those triggered events to Node A and Node B.
			evTrigA1 := waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			evTrigA2 := waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			evTrigB1 := waitForEvent(t, connB.GrainfileEvents(), 5*time.Second)
			if evTrigA1.Path != "processes/entry.process.md" || evTrigA1.TriggerSource != grainfiles.TriggerPush {
				t.Fatalf("unexpected Node A primary triggered event: %+v", evTrigA1)
			}
			if evTrigA2.Path != "processes/lint.process.md" || evTrigA2.TriggerSource != grainfiles.TriggerWith {
				t.Fatalf("unexpected Node A co-triggered event: %+v", evTrigA2)
			}
			if evTrigB1.Path != "processes/smoke.process.md" || evTrigB1.TriggerSource != grainfiles.TriggerWith {
				t.Fatalf("unexpected Node B co-triggered event: %+v", evTrigB1)
			}

			// 5. Open a PR touching non-API file -> pr-api.process.md does NOT fire.
			_, whPRNoMatch := env.sim.MustOpenPullRequest(t, "acme", "core", "main", "feat/internal", "Internal refactor", map[string]string{
				"internal/util.go": "package internal\n",
			}, nil)
			if len(whPRNoMatch.Events) != 0 {
				t.Fatalf("expected 0 PR trigger events for internal/util.go, got %+v", whPRNoMatch.Events)
			}

			// 6. Open a PR touching api/service.proto -> pr-api.process.md fires on: pull_request.
			prAPI, whPRMatch := env.sim.MustOpenPullRequest(t, "acme", "core", "main", "feat/api-v2", "Add API v2", map[string]string{
				"api/service.proto": "syntax = \"proto3\";\n",
			}, nil)
			if len(whPRMatch.Events) != 1 || whPRMatch.Events[0].Path != "processes/pr-api.process.md" {
				t.Fatalf("expected pr-api.process.md to fire on PR, got %+v", whPRMatch.Events)
			}
			evPRA := waitForEvent(t, connA.GrainfileEvents(), 5*time.Second)
			if evPRA.Type != grainfiles.EventProcessTriggered || evPRA.TriggerSource != grainfiles.TriggerPullRequest {
				t.Fatalf("unexpected PR event on Node A: %+v", evPRA)
			}

			// 7. Merge PR #prAPI.Number into main -> post-merge.process.md fires on: merge.
			mergedPR, whMerge := env.sim.MustMergePullRequest(t, "acme", "core", prAPI.Number)
			if len(whMerge.Events) != 1 || whMerge.Events[0].Path != "processes/post-merge.process.md" {
				t.Fatalf("expected post-merge.process.md to fire on merge, got %+v", whMerge.Events)
			}
			evMergeB := waitForEvent(t, connB.GrainfileEvents(), 5*time.Second)
			if evMergeB.Type != grainfiles.EventProcessTriggered ||
				evMergeB.TriggerSource != grainfiles.TriggerMerge ||
				evMergeB.CommitSHA != mergedPR.MergeCommitSHA {
				t.Fatalf("unexpected merge event on Node B: %+v (want commit %s)", evMergeB, mergedPR.MergeCommitSHA)
			}

			// 8. Cross-deployment depends_on:
			// Repo acme/upstream commits tasks/build-artifact.task.md for Node A.
			// Repo acme/downstream commits tasks/deploy-artifact.task.md for Node B with depends_on pointing to acme/upstream's task.
			shaUp, _ := env.sim.MustCommitAndPush(t, "acme", "upstream", "main", "Add upstream build task", map[string]string{
				"tasks/build-artifact.task.md": `---
title: Build Upstream Artifact
recipient: https://github.com/acme/grain-a
---
Build shared container image.
`,
			}, nil)
			_ = waitForEvent(t, connA.GrainfileEvents(), 5*time.Second) // drain task_ready

			shaDown, _ := env.sim.MustCommitAndPush(t, "acme", "downstream", "main", "Add downstream deploy task", map[string]string{
				"tasks/deploy-artifact.task.md": `---
title: Deploy Downstream Service
recipient: https://github.com/acme/grain-b
depends_on:
  - https://github.com/acme/upstream/blob/main/tasks/build-artifact.task.md
---
Deploy once upstream artifact is built.
`,
			}, nil)
			_ = waitForEvent(t, connB.GrainfileEvents(), 5*time.Second) // drain task_ready

			// Node A reports completion of acme/upstream's tasks/build-artifact.task.md over its WebSocket connection.
			upstreamURL := fmt.Sprintf("https://github.com/acme/upstream/blob/%s/tasks/build-artifact.task.md", shaUp)
			canonUpstreamURL := "https://github.com/acme/upstream/blob/main/tasks/build-artifact.task.md"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			depEvents, err := connA.CompleteGrainfile(ctx, upstreamURL, shaUp, "done", "image built and pushed")
			if err != nil {
				t.Fatalf("connA.CompleteGrainfile: %v", err)
			}
			if len(depEvents) != 1 {
				t.Fatalf("expected 1 dependency_completed event, got %+v", depEvents)
			}

			// Node B receives the live dependency_completed notification over WebSocket!
			evDepB := waitForEvent(t, connB.GrainfileEvents(), 5*time.Second)
			if evDepB.Type != grainfiles.EventDependencyCompleted ||
				evDepB.Repo != "https://github.com/acme/downstream" ||
				evDepB.Path != "tasks/deploy-artifact.task.md" ||
				evDepB.CommitSHA != shaDown ||
				evDepB.DependencyURL != canonUpstreamURL ||
				evDepB.DependencyState != "done" {
				t.Fatalf("unexpected dependency_completed event on Node B: %+v", evDepB)
			}

			sat, err := env.hub.Grainfiles.DependenciesSatisfied(ctx, "https://github.com/acme/downstream/blob/main/tasks/deploy-artifact.task.md")
			if err != nil || !sat {
				t.Fatalf("expected downstream dependencies satisfied = true, got %v (err=%v)", sat, err)
			}
		})
	}
}

// TestEndToEndCrossRepoContentProxyAndGitHubAppAuth verifies GET /v1/grainfiles/content
// against real git repositories served via GitHub App installation token
// exchange: Node B can fetch its Grainfile body and same-repo references
// (/docs/spec.md, ./schema.json) from Repo A, while fetching a cross-repo
// reference (https://github.com/org/repo-c/blob/main/secret.md) or an
// unreferenced file in Repo A (/secret.env) is rejected with 403 Forbidden.
func TestEndToEndCrossRepoContentProxyAndGitHubAppAuth(t *testing.T) {
	for _, backend := range dbBackends() {
		t.Run(backend.name, func(t *testing.T) {
			env := newE2EEnv(t, backend.open(t), e2eEnvOptions{useGitHubApp: true})

			const (
				recA = "https://github.com/acme/grain-a"
				recB = "https://github.com/acme/grain-b"
			)

			tokA := env.setupTeamAndMintNodeToken("team-a", "alice@example.com", []string{"acme/repo-a"}, "node-a")
			tokB := env.setupTeamAndMintNodeToken("team-b", "bob@example.com", []string{"beta/repo-b"}, "node-b")

			env.connectNode("node-a", tokA, []string{"acme/repo-a"}, &grainfiles.Subscriptions{
				Recipients: []string{recA},
			})
			env.connectNode("node-b", tokB, []string{"beta/repo-b"}, &grainfiles.Subscriptions{
				Recipients: []string{recB},
			})

			// Populate org/repo-c with secret.md.
			env.sim.MustCommitAndPush(t, "org", "repo-c", "main", "Add secret in repo-c", map[string]string{
				"secret.md": "TOP SECRET FROM REPO C\n",
			}, nil)

			// Populate acme/repo-a with a Grainfile for Node B referencing /docs/spec.md, ./schema.json,
			// and cross-repo https://github.com/org/repo-c/blob/main/secret.md, plus unreferenced /secret.env.
			grainfileBody := `---
title: Audit Repo A Spec
recipient: https://github.com/acme/grain-b
references:
  - /docs/spec.md
  - ./schema.json
  - https://github.com/org/repo-c/blob/main/secret.md
---
Audit the specification and schema.
`
			specBody := "# Repo A Specification\nAuthorized content.\n"
			schemaBody := "{\n  \"$schema\": \"https://json-schema.org/draft/2020-12/schema\"\n}\n"

			shaA, _ := env.sim.MustCommitAndPush(t, "acme", "repo-a", "main", "Commit grainfile and refs in repo-a", map[string]string{
				"tasks/audit.task.md": grainfileBody,
				"docs/spec.md":        specBody,
				"tasks/schema.json":   schemaBody,
				"secret.env":          "DB_PASSWORD=supersecret\n",
			}, nil)

			grainfileURL := fmt.Sprintf("https://github.com/acme/repo-a/blob/%s/tasks/audit.task.md", shaA)

			fetchContent := func(tok string, params map[string]string) (int, string) {
				t.Helper()
				q := url.Values{}
				for k, v := range params {
					q.Set(k, v)
				}
				req, err := http.NewRequest(http.MethodGet, env.hubURL+grainfiles.ContentPath+"?"+q.Encode(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if tok != "" {
					req.Header.Set("Authorization", "Bearer "+tok)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}

			// 1. Node B can fetch the Grainfile body itself.
			code, body := fetchContent(tokB, map[string]string{
				"grainfile_url": grainfileURL,
			})
			if code != http.StatusOK || body != grainfileBody {
				t.Fatalf("Node B fetch grainfile body: status=%d body=%q", code, body)
			}

			// 2. Node B can fetch same-repo reference /docs/spec.md.
			code, body = fetchContent(tokB, map[string]string{
				"grainfile_url": grainfileURL,
				"reference":     "/docs/spec.md",
			})
			if code != http.StatusOK || body != specBody {
				t.Fatalf("Node B fetch /docs/spec.md: status=%d body=%q", code, body)
			}

			// 3. Node B can fetch relative same-repo reference ./schema.json (resolved to tasks/schema.json).
			code, body = fetchContent(tokB, map[string]string{
				"grainfile_url": grainfileURL,
				"reference":     "./schema.json",
			})
			if code != http.StatusOK || body != schemaBody {
				t.Fatalf("Node B fetch ./schema.json: status=%d body=%q", code, body)
			}

			// 4. Node B is REJECTED (403 Forbidden) when fetching the cross-repo reference to org/repo-c.
			code, _ = fetchContent(tokB, map[string]string{
				"grainfile_url": grainfileURL,
				"reference":     "https://github.com/org/repo-c/blob/main/secret.md",
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for cross-repo reference with grainfile_url, got %d", code)
			}
			code, _ = fetchContent(tokB, map[string]string{
				"repo": "https://github.com/org/repo-c",
				"path": "secret.md",
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for direct cross-repo fetch of org/repo-c/secret.md, got %d", code)
			}

			// 5. Node B is REJECTED (403 Forbidden) when fetching unreferenced file /secret.env in Repo A.
			code, _ = fetchContent(tokB, map[string]string{
				"grainfile_url": grainfileURL,
				"reference":     "/secret.env",
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for unreferenced /secret.env via grainfile_url, got %d", code)
			}
			code, _ = fetchContent(tokB, map[string]string{
				"repo": "https://github.com/acme/repo-a",
				"path": "/secret.env",
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for direct unreferenced /secret.env fetch, got %d", code)
			}

			// 6. Node A (not the recipient of tasks/audit.task.md) is REJECTED (403 Forbidden)
			// when attempting to fetch tasks/audit.task.md or /docs/spec.md.
			code, _ = fetchContent(tokA, map[string]string{
				"grainfile_url": grainfileURL,
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for non-recipient Node A fetching grainfile, got %d", code)
			}
			code, _ = fetchContent(tokA, map[string]string{
				"grainfile_url": grainfileURL,
				"reference":     "/docs/spec.md",
			})
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden for non-recipient Node A fetching /docs/spec.md, got %d", code)
			}

			// 7. Verify GitHub App installation token exchange was used by the Hub's GitHubFetcher.
			if env.sim.MintedInstallationTokens() < 1 {
				t.Fatalf("expected GitHub App installation token to be minted, got %d", env.sim.MintedInstallationTokens())
			}
		})
	}
}
