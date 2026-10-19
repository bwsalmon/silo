package grainfiles_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/route"
)

// raceDynamicFetcher is a concurrency-safe RepoFetcher and TreeFetcher whose
// virtual commits and file trees can be mutated while webhook pushes,
// SyncRepository calls, HTTP content proxy reads, and mux fetches are in flight.
type raceDynamicFetcher struct {
	mu      sync.RWMutex
	trees   map[string]map[string][]byte // key: repoURL + "@" + ref
	commits map[string]string            // key: repoURL + "@" + ref -> resolved SHA
}

func newRaceDynamicFetcher() *raceDynamicFetcher {
	return &raceDynamicFetcher{
		trees:   make(map[string]map[string][]byte),
		commits: make(map[string]string),
	}
}

func (f *raceDynamicFetcher) setCommitTree(repoURL, ref, sha string, files map[string]string) {
	cloned := make(map[string][]byte, len(files))
	for k, v := range files {
		cloned[strings.TrimPrefix(k, "/")] = []byte(v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range []string{ref, "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/"), sha, ""} {
		key := repoURL + "@" + r
		f.trees[key] = cloned
		f.commits[key] = sha
	}
}

func (f *raceDynamicFetcher) setRefOnly(repoURL, ref, sha string, files map[string]string) {
	cloned := make(map[string][]byte, len(files))
	for k, v := range files {
		cloned[strings.TrimPrefix(k, "/")] = []byte(v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trees[repoURL+"@"+ref] = cloned
	f.commits[repoURL+"@"+ref] = sha
}

func (f *raceDynamicFetcher) ListGrainfiles(_ context.Context, repoURL, ref string) ([]string, string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	normRef := strings.TrimPrefix(ref, "refs/heads/")
	var (
		src map[string][]byte
		sha string
		ok  bool
	)
	for _, r := range []string{ref, normRef, "main", ""} {
		if src, ok = f.trees[repoURL+"@"+r]; ok {
			sha = f.commits[repoURL+"@"+r]
			break
		}
	}
	if !ok {
		return nil, "", os.ErrNotExist
	}
	var paths []string
	for p := range src {
		if _, _, isGF, _ := grainfiles.ClassifyPath(p); isGF {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if sha == "" {
		sha = normRef
	}
	return paths, sha, nil
}

func (f *raceDynamicFetcher) FetchFile(_ context.Context, repoURL, ref, filePath string) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	normRef := strings.TrimPrefix(ref, "refs/heads/")
	var (
		src map[string][]byte
		ok  bool
	)
	for _, r := range []string{ref, normRef, "main", ""} {
		if src, ok = f.trees[repoURL+"@"+r]; ok {
			break
		}
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	raw, ok := src[strings.TrimPrefix(filePath, "/")]
	if !ok {
		return nil, os.ErrNotExist
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
}

func (f *raceDynamicFetcher) FetchTree(_ context.Context, repoURL, ref, dirPath string) ([]grainfiles.TreeEntry, string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	normRef := strings.TrimPrefix(ref, "refs/heads/")
	var (
		src map[string][]byte
		sha string
		ok  bool
	)
	for _, r := range []string{ref, normRef, "main", ""} {
		if src, ok = f.trees[repoURL+"@"+r]; ok {
			sha = f.commits[repoURL+"@"+r]
			break
		}
	}
	if !ok {
		return nil, "", os.ErrNotExist
	}
	cleanDir := strings.Trim(dirPath, "/")
	var entries []grainfiles.TreeEntry
	for p, raw := range src {
		if cleanDir != "" && p != cleanDir && !strings.HasPrefix(p, cleanDir+"/") {
			continue
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		entries = append(entries, grainfiles.TreeEntry{
			Path: p,
			Size: int64(len(cp)),
			Data: cp,
		})
	}
	if len(entries) == 0 {
		return nil, "", os.ErrNotExist
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, sha, nil
}

// TestConcurrentWebhookSyncTickerAndDAGCompletionRace stress-tests concurrent:
//   - GitHub webhook pushes (POST /v1/webhooks/github),
//   - Repository scans/syncs (SyncRepository),
//   - Hub-hosted personal/team Grainfile edits (IndexGrainfile / RemoveGrainfile),
//   - not_before ticker ticks (PromoteDeferred),
//   - depends_on completion notifications (RecordDependencyCompleted & RecordDependencyCompletedByNode),
//   - Same-team concurrent subscription updates (SetSubscriptions / GetSubscriptions)
//
// all racing on the same (repo, path) DAG under -race.
func TestConcurrentWebhookSyncTickerAndDAGCompletionRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := grainfiles.OpenStore(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Configure SameTeam so two team members ("alice" and "bob") can concurrently
	// claim the same deployment recipient URL without a TOCTOU failure.
	store.SameTeam = func(_ context.Context, ownerA, ownerB string) bool {
		team := map[string]bool{"alice": true, "bob": true}
		return team[ownerA] && team[ownerB]
	}

	const (
		repoURL       = "https://github.com/acme/widgets"
		teamRepoURL   = "https://github.com/acme/team-hub-grainfiles"
		recipient     = "https://github.com/acme/grain-eu"
		webhookSecret = "race-webhook-secret"
	)

	fetcher := newRaceDynamicFetcher()
	api := grainfiles.NewAPI(store, fetcher)
	api.WebhookSecret = webhookSecret
	api.SetNodeToken("tok-alice", "node-alice")
	api.SetNodeToken("tok-bob", "node-bob")

	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	// Seed initial subscriptions for both team members.
	if _, err := store.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "node-alice",
		Owner:      "alice",
		Recipients: []string{recipient},
		Repos:      []string{repoURL, teamRepoURL},
	}); err != nil {
		t.Fatalf("SetSubscriptions seed alice: %v", err)
	}

	buildDAGFiles := func(iter int, nb string) map[string]string {
		return map[string]string{
			"docs/spec.md": fmt.Sprintf("# Spec rev %d\n\nDetails.\n", iter),
			"tasks/step1.task.grain.md": fmt.Sprintf(`---
grainfile: v1
title: "Step 1 rev %d"
recipient: %s
references:
  - /docs/spec.md
---
Execute step 1 iteration %d.
`, iter, recipient, iter),
			"tasks/step2.task.grain.md": fmt.Sprintf(`---
grainfile: v1
title: "Step 2 rev %d"
recipient: %s
not_before: %q
depends_on:
  - ./step1.task.grain.md
references:
  - /docs/spec.md
---
Execute step 2 iteration %d.
`, iter, recipient, nb, iter),
			"tasks/step3.task.grain.md.jinja2": fmt.Sprintf(`---
grainfile: v1
title: "Step 3 rev %d ({{ env | default('prod') | lower }})"
recipient: %s
depends_on:
  - ./step2.task.grain.md
references:
  - /docs/spec.md
---
Execute step 3 in {{ env | default('prod') | upper }} iteration %d.
`, iter, recipient, iter),
			"processes/deploy.process.grain.md.jinja2": fmt.Sprintf(`---
grainfile: v1
title: "Deploy process rev %d"
when:
  push:
    branches:
      - main
---
Run deployment checks for {{ service | default('widgets') }} rev %d.
`, iter, iter),
		}
	}

	fetcher.setCommitTree(repoURL, "main", "commit-0", buildDAGFiles(0, "2026-01-01T00:00:00Z"))

	const iterations = 10
	var wg sync.WaitGroup

	// 1. Concurrent GitHub webhook pushes evolving the DAG commits.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 1; i <= iterations; i++ {
				sha := fmt.Sprintf("commit-%d-%d", worker, i)
				nb := "2026-01-01T00:00:00Z"
				if i%2 == 0 {
					nb = "2026-12-31T00:00:00Z"
				}
				fetcher.setCommitTree(repoURL, "main", sha, buildDAGFiles(i, nb))

				payload := map[string]any{
					"ref":    "refs/heads/main",
					"before": fmt.Sprintf("commit-%d-%d", worker, i-1),
					"after":  sha,
					"repository": map[string]any{
						"html_url":       repoURL,
						"clone_url":      repoURL + ".git",
						"default_branch": "main",
					},
					"commits": []map[string]any{
						{
							"id":    sha,
							"added": []string{"tasks/step1.task.grain.md"},
							"modified": []string{
								"tasks/step2.task.grain.md",
								"tasks/step3.task.grain.md.jinja2",
								"processes/deploy.process.grain.md.jinja2",
							},
						},
					},
				}
				rawBody, _ := json.Marshal(payload)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+grainfiles.GitHubWebhookPath, bytes.NewReader(rawBody))
				if err != nil {
					return
				}
				req.Header.Set("X-GitHub-Event", "push")
				req.Header.Set("X-Hub-Signature-256", grainfiles.SignGitHubWebhook(webhookSecret, rawBody))
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(w)
	}

	// 2. Concurrent SyncRepository calls (simulating installation / repository rescans).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= iterations; i++ {
			sha := fmt.Sprintf("commit-sync-%d", i)
			fetcher.setCommitTree(repoURL, "main", sha, buildDAGFiles(i, "2026-06-01T00:00:00Z"))
			_, _ = api.SyncRepository(ctx, repoURL, "refs/heads/main")
		}
	}()

	// 3. Concurrent Hub-hosted team/personal Grainfile edits racing on the same DAG.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= iterations; i++ {
			sha := fmt.Sprintf("hub-edit-%d", i)
			raw := fmt.Sprintf(`---
grainfile: v1
title: "Team Hub Task %d"
recipient: %s
depends_on:
  - %s/blob/main/tasks/step1.task.grain.md
---
Team task body %d.
`, i, recipient, repoURL, i)
			doc, err := grainfiles.Parse("tasks/team.task.grain.md", []byte(raw))
			if err != nil {
				t.Errorf("unexpected parse error for team task: %v", err)
				return
			}
			doc.Repo = teamRepoURL
			doc.Ref = "refs/heads/main"
			doc.Commit = sha
			doc.Path = "tasks/team.task.grain.md"
			_, _ = store.IndexGrainfile(ctx, doc)
			if i%4 == 0 {
				_ = store.RemoveGrainfile(ctx, teamRepoURL, "tasks/team.task.grain.md")
			}
		}
	}()

	// 4. Concurrent not_before ticker ticks (PromoteDeferred).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= iterations*2; i++ {
			now := time.Date(2026, time.Month((i%12)+1), 15, 12, 0, 0, 0, time.UTC)
			_, _ = store.PromoteDeferred(ctx, now)
		}
	}()

	// 5. Concurrent depends_on completion notifications.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			step1URL := repoURL + "/blob/main/tasks/step1.task.grain.md"
			step2URL := repoURL + "/blob/main/tasks/step2.task.grain.md"
			for i := 1; i <= iterations; i++ {
				sha := fmt.Sprintf("commit-%d-%d", worker, i)
				_, _ = store.RecordDependencyCompleted(ctx, step1URL, sha, "done", "completed in worker")
				_, _ = store.RecordDependencyCompletedByNode(ctx, "node-alice", step2URL, sha, "done", "completed by node-alice")
			}
		}(w)
	}

	// 6. Concurrent same-team SetSubscriptions (testing the TOCTOU fix) & index/event readers.
	for _, member := range []struct {
		nodeID string
		owner  string
	}{
		{"node-alice", "alice"},
		{"node-bob", "bob"},
	} {
		wg.Add(1)
		go func(nodeID, owner string) {
			defer wg.Done()
			for i := 1; i <= iterations; i++ {
				if _, err := store.SetSubscriptions(ctx, grainfiles.Subscriptions{
					NodeID:     nodeID,
					Owner:      owner,
					Recipients: []string{recipient},
					Repos:      []string{repoURL, teamRepoURL},
				}); err != nil {
					t.Errorf("SetSubscriptions by %s (%s) failed: %v", nodeID, owner, err)
					return
				}
				_, _ = store.GetSubscriptions(ctx, nodeID)
				_, _ = store.ListIndexByRepo(ctx, repoURL)
				_, _ = store.EventsSince(ctx, 0)
			}
		}(member.nodeID, member.owner)
	}

	wg.Wait()

	// Final deterministic convergence check on a fresh DAG path after the concurrent storm:
	const finalSHA = "commit-final-converged"
	finalFiles := buildDAGFiles(999, "2026-01-01T00:00:00Z")
	finalFiles["tasks/final-a.task.grain.md"] = fmt.Sprintf(`---
grainfile: v1
title: "Final A"
recipient: %s
---
Step A.
`, recipient)
	finalFiles["tasks/final-b.task.grain.md"] = fmt.Sprintf(`---
grainfile: v1
title: "Final B"
recipient: %s
not_before: "2099-01-01T00:00:00Z"
depends_on:
  - ./final-a.task.grain.md
---
Step B.
`, recipient)
	fetcher.setCommitTree(repoURL, "main", finalSHA, finalFiles)
	if _, err := api.SyncRepository(ctx, repoURL, "refs/heads/main"); err != nil {
		t.Fatalf("final SyncRepository: %v", err)
	}

	recA, err := store.GetIndex(ctx, repoURL, "tasks/final-a.task.grain.md", finalSHA)
	if err != nil {
		t.Fatalf("GetIndex final-a: %v", err)
	}
	if recA.CommitSHA != finalSHA {
		t.Fatalf("final-a CommitSHA = %q, want %q", recA.CommitSHA, finalSHA)
	}

	// Complete final-a and verify dependency satisfaction.
	finalAURL := repoURL + "/blob/main/tasks/final-a.task.grain.md"
	finalBURL := repoURL + "/blob/main/tasks/final-b.task.grain.md"
	if _, err := store.RecordDependencyCompletedByNode(ctx, "node-alice", finalAURL, finalSHA, "done", "done"); err != nil {
		t.Fatalf("RecordDependencyCompletedByNode final-a: %v", err)
	}
	satisfied, err := store.DependenciesSatisfied(ctx, finalBURL)
	if err != nil || !satisfied {
		t.Fatalf("DependenciesSatisfied(final-b) = %v, %v; want true, nil", satisfied, err)
	}

	// Promote final-b when clock reaches 2099.
	promoted, err := store.PromoteDeferred(ctx, time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PromoteDeferred(2099): %v", err)
	}
	foundFinalB := false
	for _, ev := range promoted {
		if ev.Path == "tasks/final-b.task.grain.md" && ev.Type == grainfiles.EventTaskReady {
			foundFinalB = true
		}
	}
	if !foundFinalB {
		t.Fatalf("expected PromoteDeferred(2099) to emit task_ready for tasks/final-b.task.grain.md, got %+v", promoted)
	}
}

// TestConcurrentContentProxyAndMuxFetchDuringDraftAndRemovalTransitions stress-tests
// concurrent GET /v1/grainfiles/content?var.x=... and WebSocket mux FetchGrainfileWithVars /
// FetchReference calls while a grainfile transitions between active (draft: false),
// reference revocation, draft: true, and deletion (RemoveGrainfile), verifying that:
//   - AuthorizedReferences never leak unauthorized files in the repository,
//   - Commit-SHA pinning never serves a grainfile or reference from a stale commit
//     after a newer commit transitions to draft: true, revokes a reference, or removes
//     the grainfile,
//   - Template rendering with var.<name> query parameters and mux Vars is race-free.
func TestConcurrentContentProxyAndMuxFetchDuringDraftAndRemovalTransitions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := grainfiles.OpenStore(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const (
		repoURL               = "https://github.com/acme/secure-service"
		authorizedRecipient   = "https://github.com/acme/grain-eu"
		unauthorizedRecipient = "https://github.com/acme/grain-us"
		webhookSecret         = "proxy-secret-token"
		grainfilePath         = "tasks/rollout.task.grain.md.jinja2"
		grainfileBlobURL      = repoURL + "/blob/main/" + grainfilePath
		forbiddenContent      = "TOP_SECRET_NEVER_LEAK_CREDENTIALS"
		revokedRefContent     = "REVOKED_REF_SECRET_FROM_COMMIT_V1"
		draftBodySentinel     = "DRAFT_ONLY_SENTINEL_NEVER_SERVE"
	)

	fetcher := newRaceDynamicFetcher()
	api := grainfiles.NewAPI(store, fetcher)
	api.WebhookSecret = webhookSecret
	api.SetNodeToken("tok-authorized", "node-authorized")
	api.SetNodeToken("tok-unauthorized", "node-unauthorized")

	reg := registry.New()
	reg.StaleAfter = time.Minute
	rv := &rendezvous.Server{
		Registry: reg,
		Authenticate: func(r *http.Request) (rendezvous.Identity, error) {
			switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
			case "tok-authorized":
				return rendezvous.Identity{Owner: "alice", Node: "node-authorized"}, nil
			case "tok-unauthorized":
				return rendezvous.Identity{Owner: "mallory", Node: "node-unauthorized"}, nil
			default:
				return rendezvous.Identity{}, fmt.Errorf("bad token")
			}
		},
		Grainfiles:    store,
		GrainfilesAPI: api,
	}

	m := route.NewMux()
	api.Register(m)
	m.Handle("GET "+rendezvous.ConnectPath, rv.Handler())
	srv := httptest.NewServer(m)
	defer srv.Close()

	if _, err := store.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "node-authorized",
		Owner:      "alice",
		Recipients: []string{authorizedRecipient},
	}); err != nil {
		t.Fatalf("SetSubscriptions authorized: %v", err)
	}
	if _, err := store.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "node-unauthorized",
		Owner:      "mallory",
		Recipients: []string{unauthorizedRecipient},
	}); err != nil {
		t.Fatalf("SetSubscriptions unauthorized: %v", err)
	}

	v1Files := map[string]string{
		grainfilePath: `---
grainfile: v1
title: "Rollout {{ service | default('api') | upper }} to {{ env | default('staging') | lower }}"
recipient: https://github.com/acme/grain-eu
draft: false
references:
  - /docs/allowed.md
  - /docs/revoked-later.md
---
Active rollout for {{ service | default('api') | upper }} in {{ env | default('staging') | lower }}.
`,
		"docs/allowed.md":          "# Allowed Reference\nSafe documentation content.\n",
		"docs/revoked-later.md":    revokedRefContent,
		"docs/forbidden-secret.md": forbiddenContent,
	}

	v2Files := map[string]string{
		grainfilePath: `---
grainfile: v1
title: "Rollout {{ service | default('api') | upper }} to {{ env | default('staging') | lower }}"
recipient: https://github.com/acme/grain-eu
draft: false
references:
  - /docs/allowed.md
---
Active v2 rollout for {{ service | default('api') | upper }} in {{ env | default('staging') | lower }}.
`,
		"docs/allowed.md":          "# Allowed Reference\nSafe documentation content.\n",
		"docs/revoked-later.md":    revokedRefContent,
		"docs/forbidden-secret.md": forbiddenContent,
	}

	v3DraftFiles := map[string]string{
		grainfilePath: `---
grainfile: v1
title: "Draft Rollout {{ service | default('api') | upper }}"
recipient: https://github.com/acme/grain-eu
draft: true
references:
  - /docs/allowed.md
---
` + draftBodySentinel + "\n",
		"docs/allowed.md":          "# Allowed Reference\nSafe documentation content.\n",
		"docs/revoked-later.md":    revokedRefContent,
		"docs/forbidden-secret.md": forbiddenContent,
	}

	// Keep commit-v1, commit-v2, commit-v3 trees available in the fetcher by SHA so
	// we can verify that pinning commit=commit-v1 is rejected by the store index once
	// commit-v2 / commit-v3 / removal supersedes it.
	fetcher.setRefOnly(repoURL, "commit-v1", "commit-v1", v1Files)
	fetcher.setRefOnly(repoURL, "commit-v2", "commit-v2", v2Files)
	fetcher.setRefOnly(repoURL, "commit-v3", "commit-v3", v3DraftFiles)
	fetcher.setCommitTree(repoURL, "main", "commit-v1", v1Files)

	if _, err := api.SyncRepository(ctx, repoURL, "refs/heads/main"); err != nil {
		t.Fatalf("initial SyncRepository: %v", err)
	}

	// Connect a live WebSocket mux client for the authorized recipient node.
	cl := &rendezvous.Client{
		Hub: srv.URL,
		Token: func(context.Context) (string, error) {
			return "tok-authorized", nil
		},
		Node: registry.Node{
			ID:    "node-authorized",
			Slots: 2,
		},
		Subscriptions: &grainfiles.Subscriptions{
			Recipients: []string{authorizedRecipient},
		},
	}
	muxConn, err := cl.Connect(ctx)
	if err != nil {
		t.Fatalf("cl.Connect: %v", err)
	}
	defer muxConn.Close()

	// 1. Verify commit-v2 immediately revokes commit-v1's /docs/revoked-later.md even if
	// the caller explicitly pins commit=commit-v1!
	fetcher.setCommitTree(repoURL, "main", "commit-v2", v2Files)
	if _, err := api.SyncRepository(ctx, repoURL, "refs/heads/main"); err != nil {
		t.Fatalf("v2 SyncRepository: %v", err)
	}
	if _, _, _, err := api.FetchAuthorizedContentWithVars(ctx, "node-authorized", grainfileBlobURL, "commit-v1", "/docs/revoked-later.md", "", nil); err == nil {
		t.Fatalf("SECURITY BUG: FetchAuthorizedContentWithVars allowed fetching revoked reference /docs/revoked-later.md by pinning superseded commit-v1")
	}

	// 2. Verify commit-v3 (draft: true) immediately blocks fetching commit-v1 or commit-v2 by SHA!
	fetcher.setCommitTree(repoURL, "main", "commit-v3", v3DraftFiles)
	if _, err := api.SyncRepository(ctx, repoURL, "refs/heads/main"); err != nil {
		t.Fatalf("v3 SyncRepository: %v", err)
	}
	for _, oldSHA := range []string{"commit-v1", "commit-v2", "commit-v3", ""} {
		if _, _, _, err := api.FetchAuthorizedContentWithVars(ctx, "node-authorized", grainfileBlobURL, oldSHA, "", "", map[string]string{"env": "prod"}); err == nil {
			t.Fatalf("SECURITY BUG: FetchAuthorizedContentWithVars served grainfile at %q while latest index is draft: true", oldSHA)
		}
		if _, _, _, err := api.FetchAuthorizedContentWithVars(ctx, "node-authorized", grainfileBlobURL, oldSHA, "/docs/allowed.md", "", nil); err == nil {
			t.Fatalf("SECURITY BUG: FetchAuthorizedContentWithVars served reference /docs/allowed.md at %q while latest index is draft: true", oldSHA)
		}
	}

	// 3. Stress-test concurrent state transitions (active -> draft -> removed -> active)
	// against concurrent HTTP GET /v1/grainfiles/content?var.x=... and mux FetchGrainfileWithVars / FetchReference calls.
	var (
		wg             sync.WaitGroup
		validRenders   atomic.Int64
		blockedFetches atomic.Int64
	)

	const loops = 12

	// State-transition writer goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < loops; i++ {
			switch i % 3 {
			case 0:
				fetcher.setCommitTree(repoURL, "main", "commit-v2", v2Files)
				_, _ = api.SyncRepository(ctx, repoURL, "refs/heads/main")
			case 1:
				fetcher.setCommitTree(repoURL, "main", "commit-v3", v3DraftFiles)
				_, _ = api.SyncRepository(ctx, repoURL, "refs/heads/main")
			case 2:
				_ = store.RemoveGrainfile(ctx, repoURL, grainfilePath)
			}
		}
	}()

	// Concurrent HTTP GET /v1/grainfiles/content readers (template rendering + reference checks).
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < loops*2; i++ {
				// 1. Authorized template render with var.env and var.service.
				u := fmt.Sprintf("%s%s?repo=%s&path=%s&var.env=PROD&var.service=billing",
					srv.URL, grainfiles.ContentPath, repoURL, grainfilePath)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					return
				}
				req.Header.Set("Authorization", "Bearer tok-authorized")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						text := string(body)
						if strings.Contains(text, draftBodySentinel) {
							t.Errorf("LEAK: draft grainfile body was served via HTTP content proxy: %s", text)
						}
						if !strings.Contains(text, "Rollout BILLING to prod") {
							t.Errorf("unexpected rendered template content: %s", text)
						}
						validRenders.Add(1)
					} else {
						blockedFetches.Add(1)
					}
				}

				// 2. Unauthorized file in the same repo or revoked reference at commit-v1 must NEVER be served.
				for _, badPath := range []string{
					"/docs/forbidden-secret.md",
					"/docs/revoked-later.md",
					"/docs/../docs/forbidden-secret.md",
					"/docs/%2e%2e/docs/forbidden-secret.md",
				} {
					badURL := fmt.Sprintf("%s%s?repo=%s&grainfile=%s&reference=%s&commit=commit-v1",
						srv.URL, grainfiles.ContentPath, repoURL, grainfilePath, badPath)
					badReq, err := http.NewRequestWithContext(ctx, http.MethodGet, badURL, nil)
					if err != nil {
						return
					}
					badReq.Header.Set("Authorization", "Bearer tok-authorized")
					badResp, err := http.DefaultClient.Do(badReq)
					if err == nil {
						badBody, _ := io.ReadAll(badResp.Body)
						badResp.Body.Close()
						if badResp.StatusCode == http.StatusOK ||
							strings.Contains(string(badBody), forbiddenContent) ||
							strings.Contains(string(badBody), revokedRefContent) {
							t.Errorf("SECURITY LEAK on %s: status=%d body=%q", badPath, badResp.StatusCode, string(badBody))
						}
					}
				}

				// 3. Unauthorized node must NEVER be served (403).
				unauthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
				if err != nil {
					return
				}
				unauthReq.Header.Set("Authorization", "Bearer tok-unauthorized")
				unauthResp, err := http.DefaultClient.Do(unauthReq)
				if err == nil {
					unauthBody, _ := io.ReadAll(unauthResp.Body)
					unauthResp.Body.Close()
					if unauthResp.StatusCode == http.StatusOK {
						t.Errorf("SECURITY LEAK to unauthorized node: body=%q", string(unauthBody))
					}
				}
			}
		}()
	}

	// Concurrent WebSocket Mux FetchGrainfileWithVars & FetchReference readers.
	for mIdx := 0; mIdx < 2; mIdx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < loops; i++ {
				_, data, err := muxConn.FetchGrainfileWithVars(ctx, grainfileBlobURL, "", map[string]string{
					"env":     "PROD",
					"service": "ledger",
				})
				if err == nil {
					text := string(data)
					if strings.Contains(text, draftBodySentinel) {
						t.Errorf("LEAK: draft grainfile content served over mux: %s", text)
					}
					if !strings.Contains(text, "Rollout LEDGER to prod") {
						t.Errorf("unexpected mux rendered template: %s", text)
					}
					validRenders.Add(1)
				} else {
					blockedFetches.Add(1)
				}

				// Authorized reference fetch over mux (succeeds when active, fails when draft/removed).
				_, refData, err := muxConn.FetchReference(ctx, grainfileBlobURL, "", "/docs/allowed.md", "")
				if err == nil && !strings.Contains(string(refData), "Allowed Reference") {
					t.Errorf("unexpected mux reference content: %s", string(refData))
				}

				// Forbidden reference fetch over mux (must ALWAYS fail).
				if _, badData, err := muxConn.FetchReference(ctx, grainfileBlobURL, "commit-v1", "/docs/forbidden-secret.md", ""); err == nil || strings.Contains(string(badData), forbiddenContent) {
					t.Errorf("SECURITY LEAK over mux for forbidden-secret.md: err=%v content=%q", err, string(badData))
				}

				// Revoked reference pinned to superseded commit-v1 over mux (must ALWAYS fail).
				if _, revData, err := muxConn.FetchReference(ctx, grainfileBlobURL, "commit-v1", "/docs/revoked-later.md", ""); err == nil || strings.Contains(string(revData), revokedRefContent) {
					t.Errorf("SECURITY LEAK over mux for revoked-later.md at commit-v1: err=%v content=%q", err, string(revData))
				}
			}
		}()
	}

	wg.Wait()
}
