// Package cujtest exercises the 12 canonical Grain Hub Critical User Journeys
// (docs/cujs.md) end-to-end against a real in-process Hub server and simulated
// Grain nodes.
package cujtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/githubsim"
)

const (
	userHeader    = "X-Test-User"
	webhookSecret = "cuj-test-webhook-secret"
	githubToken   = "ghp_cuj_test_token"
	nodeDomain    = "nodes.hub.test"
)

func logf(t *testing.T) func(string, ...any) {
	var (
		mu   sync.Mutex
		done bool
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

type testHub struct {
	t   *testing.T
	url string
	gh  *githubsim.Server
}

func newTestHub(t *testing.T, modify func(*server.Config)) *testHub {
	t.Helper()
	gh := githubsim.New(t, githubsim.Options{
		WebhookSecret: webhookSecret,
		Token:         githubToken,
	})

	dbPath := t.TempDir() + "/hub.db"
	cfg := server.Config{
		Database:      "sqlite:" + dbPath,
		DevUserHeader: userHeader,
		HubAdmins:     []string{"user:sysadmin@example.com"},
		AcceptedUsers: []string{
			"alice@example.com",
			"bob@example.com",
			"carol@example.com",
			"eve@example.com",
		},
		HubURL:        "https://hub.test",
		NodeUIDomain:  nodeDomain,
		WebhookSecret: webhookSecret,
		GitHubToken:   githubToken,
		GitHubAPIURL:  gh.URL,
		Heartbeat:     time.Second,
		DispatchRetry: 50 * time.Millisecond,
		ShutdownGrace: time.Second,
		Logf:          logf(t),
	}
	if modify != nil {
		modify(&cfg)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	hubURL := "http://" + l.Addr().String()
	gh.SetWebhook(hubURL+grainfiles.GitHubWebhookPath, webhookSecret)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, l, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &testHub{
		t:   t,
		url: hubURL,
		gh:  gh,
	}
}

func (th *testHub) call(method, path, user string, body any, out any) (int, []byte) {
	th.t.Helper()
	var rd io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			rd = strings.NewReader(v)
		case []byte:
			rd = bytes.NewReader(v)
		default:
			b, err := json.Marshal(body)
			if err != nil {
				th.t.Fatalf("marshal body: %v", err)
			}
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, th.url+path, rd)
	if err != nil {
		th.t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.Header.Set(userHeader, user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		th.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			th.t.Fatalf("%s %s: unmarshal %v: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, raw
}

func (th *testHub) mustCall(method, path, user string, body any, out any) []byte {
	th.t.Helper()
	code, raw := th.call(method, path, user, body, out)
	if code >= 300 {
		th.t.Fatalf("%s %s (user=%s): HTTP %d: %s", method, path, user, code, raw)
	}
	return raw
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// simNode is a simulated Grain node connected to the Hub via rendezvous.
type simNode struct {
	t      *testing.T
	id     string
	token  string
	conn   *rendezvous.Conn
	cancel context.CancelFunc
	done   chan struct{}
	ui     http.Handler

	mu        sync.Mutex
	offers    []tasks.Spec
	resumes   []tasks.Spec
	depsDone  []tasks.Message
	cancels   []string
	approves  []tasks.Message
	messages  []events.Message
	streams   map[string]*tasks.Stream
	localIDs  map[string]string
	refuse    func(tasks.Spec) string
	artifacts map[string][]byte
}

func connectSimNode(
	t *testing.T,
	th *testHub,
	token string,
	node registry.Node,
	subs *grainfiles.Subscriptions,
	opts ...func(*simNode),
) *simNode {
	t.Helper()
	if node.Slots <= 0 {
		node.Slots = 4
	}
	sn := &simNode{
		t:         t,
		id:        node.ID,
		token:     token,
		done:      make(chan struct{}),
		streams:   make(map[string]*tasks.Stream),
		localIDs:  make(map[string]string),
		artifacts: make(map[string][]byte),
	}
	for _, opt := range opts {
		opt(sn)
	}
	rc := &rendezvous.Client{
		Hub:           th.url,
		Token:         func(context.Context) (string, error) { return token, nil },
		Node:          node,
		Subscriptions: subs,
		Logf:          logf(t),
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := rc.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatalf("connect node %s: %v", node.ID, err)
	}
	sn.conn = conn
	sn.cancel = cancel
	go sn.serveLoop(conn)
	t.Cleanup(func() {
		sn.Close()
	})
	return sn
}

func mintAndConnectNode(
	t *testing.T,
	th *testHub,
	owner string,
	node registry.Node,
	subs *grainfiles.Subscriptions,
	opts ...func(*simNode),
) *simNode {
	t.Helper()
	var minted struct {
		Token string `json:"token"`
	}
	th.mustCall("POST", directory.NodeTokensPath, owner, map[string]any{
		"node": node.ID,
	}, &minted)
	return connectSimNode(t, th, minted.Token, node, subs, opts...)
}

func (sn *simNode) Close() {
	sn.cancel()
	_ = sn.conn.Close()
	<-sn.done
}

func (sn *simNode) serveLoop(conn *rendezvous.Conn) {
	defer close(sn.done)
	for {
		c, err := conn.Accept()
		if err != nil {
			return
		}
		go sn.handleStream(c)
	}
}

type peekConn struct {
	net.Conn
	r *bufio.Reader
}

func (p peekConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func (sn *simNode) handleStream(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &head)

	if head.Type == live.TypeView {
		var vm live.ViewMessage
		_ = json.Unmarshal(line, &vm)
		sn.mu.Lock()
		data, ok := sn.artifacts[vm.Ref]
		sn.mu.Unlock()
		live.ServeView(c, vm, func(live.ViewMessage) (io.ReadCloser, error) {
			if !ok {
				return nil, fmt.Errorf("no such artifact %q", vm.Ref)
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		})
		return
	}

	if head.Type == nodeui.TypeHTTP {
		var m nodeui.Message
		if json.Unmarshal(line, &m) != nil {
			c.Close()
			return
		}
		h := sn.ui
		if h == nil {
			h = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, "<html><body>node=%s user=%s role=%s path=%s</body></html>",
					sn.id,
					m.Viewer,
					m.Role,
					req.URL.Path)
			})
		}
		_ = nodeui.Serve(peekConn{c, r}, m, h)
		return
	}

	var m tasks.Message
	if json.Unmarshal(line, &m) != nil {
		c.Close()
		return
	}
	st := tasks.NewStream(peekConn{c, r})
	switch m.Type {
	case tasks.TypeOffer:
		if m.Task == nil {
			st.Close()
			return
		}
		spec := *m.Task
		sn.mu.Lock()
		var refuseWhy string
		if sn.refuse != nil {
			refuseWhy = sn.refuse(spec)
		}
		if refuseWhy != "" {
			sn.mu.Unlock()
			_ = st.Write(tasks.Message{Type: tasks.TypeRefuse, Reason: refuseWhy})
			st.Close()
			return
		}
		sn.offers = append(sn.offers, spec)
		localID := fmt.Sprintf("local-%d", len(sn.offers))
		sn.localIDs[spec.ID] = localID
		sn.streams[spec.ID] = st
		sn.mu.Unlock()
		_ = st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: localID})
		sn.followTaskStream(spec.ID, st)

	case tasks.TypeResume:
		if m.Task == nil {
			st.Close()
			return
		}
		spec := *m.Task
		sn.mu.Lock()
		localID, ok := sn.localIDs[spec.ID]
		if !ok {
			sn.mu.Unlock()
			_ = st.Write(tasks.Message{Type: tasks.TypeRefuse, Reason: "unknown task"})
			st.Close()
			return
		}
		sn.resumes = append(sn.resumes, spec)
		sn.streams[spec.ID] = st
		sn.mu.Unlock()
		_ = st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: localID})
		sn.followTaskStream(spec.ID, st)

	case tasks.TypeDependencyDone:
		sn.mu.Lock()
		sn.depsDone = append(sn.depsDone, m)
		sn.mu.Unlock()
		st.Close()

	case tasks.TypeCancel:
		if m.Task != nil {
			sn.mu.Lock()
			sn.cancels = append(sn.cancels, m.Task.ID)
			sn.mu.Unlock()
		}
		st.Close()

	default:
		st.Close()
	}
}

func (sn *simNode) followTaskStream(taskID string, st *tasks.Stream) {
	for {
		m, err := st.Read()
		if err != nil {
			return
		}
		sn.mu.Lock()
		switch m.Type {
		case tasks.TypeCancel:
			sn.cancels = append(sn.cancels, taskID)
		case tasks.TypeApprove:
			sn.approves = append(sn.approves, m)
		case tasks.TypeMessage:
			if m.Message != nil {
				sn.messages = append(sn.messages, *m.Message)
			}
		}
		sn.mu.Unlock()
	}
}

func (sn *simNode) finishTask(taskID string, state tasks.State, reason string) {
	sn.t.Helper()
	sn.mu.Lock()
	st := sn.streams[taskID]
	sn.mu.Unlock()
	if st == nil {
		sn.t.Fatalf("node %s has no stream for task %s", sn.id, taskID)
	}
	if err := st.Write(tasks.Message{Type: tasks.TypeResult, State: state, Reason: reason}); err != nil {
		sn.t.Fatalf("write result for %s: %v", taskID, err)
	}
}

func (sn *simNode) emitEvents(taskID string, evs ...live.Event) {
	sn.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	up, err := sn.conn.Open(ctx)
	if err != nil {
		sn.t.Fatalf("open event stream from %s: %v", sn.id, err)
	}
	defer up.Close()
	for _, ev := range evs {
		ev.V = events.Version
		ev.Task = taskID
		if ev.Time.IsZero() {
			ev.Time = time.Now().UTC()
		}
		b, err := json.Marshal(ev)
		if err != nil {
			sn.t.Fatalf("marshal event: %v", err)
		}
		if _, err := up.Write(append(b, '\n')); err != nil {
			sn.t.Fatalf("write event: %v", err)
		}
	}
}

// TestCUJ01_SysAdminAllowlistAndDeploymentCreation verifies CUJ-1:
// SysAdmin allowlist management (`PUT /v1/users/{email}`), unauthorized outsider
// rejection (`403 Forbidden`), allowed user creating a deployment (`POST /v1/deployments`),
// assigning a repository, enrolling a Grain node, and preventing another deployment
// from hijacking an already-owned repository.
func TestCUJ01_SysAdminAllowlistAndDeploymentCreation(t *testing.T) {
	th := newTestHub(t, func(cfg *server.Config) {
		cfg.AcceptedUsers = nil
	})

	// 1. SysAdmin adds alice@example.com and bob@example.com to the accepted users allowlist.
	th.mustCall("PUT", "/v1/users/alice@example.com", "sysadmin@example.com", map[string]any{
		"sys_admin": false,
	}, nil)
	th.mustCall("PUT", "/v1/users/bob@example.com", "sysadmin@example.com", map[string]any{
		"sys_admin": false,
	}, nil)

	// 2. Outsider not on the allowlist is rejected with 403 Forbidden.
	code, _ := th.call("GET", "/v1/me", "outsider@other.org", nil, nil)
	if code != http.StatusForbidden {
		t.Fatalf("outsider GET /v1/me: got %d, want 403", code)
	}

	// 3. Allowed user Alice signs in, creates deployment "acme-payments", and claims acme/payments.
	var me struct {
		Email string `json:"email"`
	}
	th.mustCall("GET", "/v1/me", "alice@example.com", nil, &me)
	if me.Email != "alice@example.com" {
		t.Fatalf("me.Email = %q, want alice@example.com", me.Email)
	}

	var dep directory.Team
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "acme-payments",
		"name": "Acme Payments",
	}, &dep)
	if dep.ID != "acme-payments" || !strings.HasSuffix(dep.RecipientURL, "/deployments/acme-payments") {
		t.Fatalf("created deployment = %+v", dep)
	}

	th.mustCall("PUT", "/v1/deployments/acme-payments/repos/acme/payments", "alice@example.com", nil, nil)

	// 4. Alice mints a deployment token and connects node "acme-payments".
	var tokResp struct {
		Token string `json:"token"`
	}
	th.mustCall("POST", "/v1/deployments/acme-payments/token", "alice@example.com", nil, &tokResp)
	connectSimNode(t, th, tokResp.Token, registry.Node{
		ID:    "acme-payments",
		Repos: []string{"acme/payments"},
		Slots: 2,
	}, nil)

	eventually(t, "acme-payments node online", func() bool {
		var dir struct {
			Nodes []registry.Entry `json:"nodes"`
		}
		th.mustCall("GET", rendezvous.DirectoryPath, "alice@example.com", nil, &dir)
		return len(dir.Nodes) == 1 && dir.Nodes[0].ID == "acme-payments" && dir.Nodes[0].Status == registry.Online
	})

	// 5. Bob creates deployment "rogue-team" and attempts to claim acme/payments;
	// the Hub rejects the conflicting repository claim with 409 Conflict.
	th.mustCall("POST", directory.DeploymentsPath, "bob@example.com", map[string]any{
		"id":   "rogue-team",
		"name": "Rogue Team",
	}, nil)
	code, _ = th.call("PUT", "/v1/deployments/rogue-team/repos/acme/payments", "bob@example.com", nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("conflicting repo claim: got %d, want 409 Conflict", code)
	}
}

// TestCUJ02_MultiAdminTakeoverAndStateRecovery verifies CUJ-2:
// Multi-admin deployment membership, primary host failure while running a task
// and holding a Grainfile subscription, co-admin executing
// `POST /v1/deployments/{id}/replace-host` to revoke the dead host, reclaim
// in-flight tasks, and replay active Grainfile events to the replacement host.
func TestCUJ02_MultiAdminTakeoverAndStateRecovery(t *testing.T) {
	th := newTestHub(t, nil)

	// 1. Alice creates deployment "infra-core", adds Bob as co-admin and Carol as member,
	// and claims repository acme/infra.
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "infra-core",
		"name": "Infra Core",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/infra-core/members/user:bob@example.com", "alice@example.com", map[string]any{
		"role": "admin",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/infra-core/members/user:carol@example.com", "alice@example.com", map[string]any{
		"role": "member",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/infra-core/repos/acme/infra", "alice@example.com", nil, nil)

	// Carol (member, not admin) cannot mint deployment tokens or replace the host.
	if code, _ := th.call("POST", "/v1/deployments/infra-core/replace-host", "carol@example.com", nil, nil); code != http.StatusForbidden {
		t.Fatalf("member calling replace-host: got %d, want 403", code)
	}

	// 2. Author a team grainfile for infra-core and connect primary host (Alice).
	recipientURL := "https://hub.test/deployments/infra-core"
	gfContent := fmt.Sprintf(`---
title: Infra health check
recipient: %s
---
Verify infra health.
`, recipientURL)
	th.mustCall("PUT", "/v1/grainfiles/team/infra-core/tasks/health.task.md", "alice@example.com", map[string]any{
		"content": gfContent,
	}, nil)

	var tokAlpha struct {
		Token string `json:"token"`
	}
	th.mustCall("POST", "/v1/deployments/infra-core/token", "alice@example.com", nil, &tokAlpha)
	hostAlpha := connectSimNode(t, th, tokAlpha.Token, registry.Node{
		ID:         "infra-core",
		Repos:      []string{"acme/infra"},
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{recipientURL},
	}, &grainfiles.Subscriptions{
		Recipients: []string{recipientURL},
	})

	// Drain initial grainfile event on hostAlpha.
	select {
	case <-hostAlpha.conn.GrainfileEvents():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial grainfile event on hostAlpha")
	}

	// Carol files a task that begins running on hostAlpha.
	var created tasks.Task
	th.mustCall("POST", tasks.TasksPath, "carol@example.com", map[string]any{
		"repo":   "acme/infra",
		"title":  "Upgrade cluster",
		"prompt": "Run cluster upgrade",
	}, &created)

	eventually(t, "task running on hostAlpha", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+created.ID, "carol@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "infra-core"
	})

	// 3. Primary hostAlpha fails; co-admin Bob invokes POST /v1/deployments/infra-core/replace-host.
	var replaceResp struct {
		Token          string   `json:"token"`
		ReplacedHost   bool     `json:"replaced_host"`
		ReclaimedTasks []string `json:"reclaimed_tasks"`
		ReplayedEvents int      `json:"replayed_events"`
	}
	th.mustCall("POST", "/v1/deployments/infra-core/replace-host", "bob@example.com", nil, &replaceResp)
	if !replaceResp.ReplacedHost || len(replaceResp.ReclaimedTasks) != 1 || replaceResp.ReclaimedTasks[0] != created.ID {
		t.Fatalf("unexpected replace-host response: %+v", replaceResp)
	}
	if replaceResp.ReplayedEvents < 1 {
		t.Fatalf("expected at least 1 replayed grainfile event, got %d", replaceResp.ReplayedEvents)
	}

	// Verify old token is revoked and cannot reconnect.
	badClient := &rendezvous.Client{
		Hub:   th.url,
		Token: func(context.Context) (string, error) { return tokAlpha.Token, nil },
		Node:  registry.Node{ID: "infra-core", Repos: []string{"acme/infra"}, Slots: 1},
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
	if _, err := badClient.Connect(cctx); err == nil {
		t.Fatal("revoked hostAlpha token was allowed to reconnect")
	}
	ccancel()

	// 4. Bob connects replacement hostBeta with the newly minted token; it receives both
	// the replayed grainfile event and the reclaimed task, and finishes the task.
	hostBeta := connectSimNode(t, th, replaceResp.Token, registry.Node{
		ID:         "infra-core",
		Repos:      []string{"acme/infra"},
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{recipientURL},
	}, &grainfiles.Subscriptions{
		Recipients: []string{recipientURL},
	})

	select {
	case ev := <-hostBeta.conn.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady {
			t.Fatalf("hostBeta replayed grainfile event = %+v, want task_ready", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for replayed grainfile event on hostBeta")
	}

	eventually(t, "reclaimed task running on hostBeta", func() bool {
		hostBeta.mu.Lock()
		_, hasStream := hostBeta.streams[created.ID]
		hostBeta.mu.Unlock()
		return hasStream
	})

	hostBeta.finishTask(created.ID, tasks.Done, "cluster upgraded by replacement host")
	eventually(t, "reclaimed task done", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+created.ID, "carol@example.com", nil, &tk)
		return tk.State == tasks.Done && tk.Reason == "cluster upgraded by replacement host"
	})
}

// TestCUJ03_PersonalHubGrainfileAuthoringAndCrossDeploymentDispatch verifies CUJ-3:
// Authoring a personal Hub-hosted grainfile (`PUT /v1/grainfiles/personal/{path...}`)
// without any external Git repository, targeting a shared deployment recipient,
// verifying privacy from other users, and delivering live `task_ready` events and
// content fetches to the target deployment node.
func TestCUJ03_PersonalHubGrainfileAuthoringAndCrossDeploymentDispatch(t *testing.T) {
	th := newTestHub(t, nil)

	// 1. Alice creates deployment "platform" and connects a node subscribed to its canonical recipient URL.
	var dep directory.Team
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "platform",
		"name": "Platform",
	}, &dep)
	th.mustCall("PUT", "/v1/deployments/platform/repos/acme/platform-api", "alice@example.com", nil, nil)

	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:         "node-platform-1",
		Repos:      []string{"acme/platform-api"},
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{dep.RecipientURL},
	}, &grainfiles.Subscriptions{
		Recipients: []string{dep.RecipientURL},
	})

	// 2. Alice authors a personal Hub-hosted grainfile targeting the platform deployment.
	gfSource := fmt.Sprintf(`---
title: Morning platform triage
recipient: %s
requestor: Alice <alice@example.com>
---
Review open issues on acme/platform-api.
`, dep.RecipientURL)

	var putResp grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/personal/tasks/morning-triage.task.md", "alice@example.com", map[string]any{
		"content": gfSource,
	}, &putResp)

	if putResp.Indexed == nil || putResp.Indexed.Title != "Morning platform triage" || len(putResp.Events) == 0 {
		t.Fatalf("unexpected personal grainfile PUT response: %+v", putResp)
	}

	// Verify GET returns the stored source to Alice.
	getRaw := th.mustCall("GET", "/v1/grainfiles/personal/tasks/morning-triage.task.md", "alice@example.com", nil, nil)
	if string(getRaw) != gfSource {
		t.Fatalf("GET personal grainfile content mismatch: got %q", getRaw)
	}

	// 3. Another user (Bob) cannot see Alice's personal grainfile in GET /v1/grainfiles.
	var bobList struct {
		Grainfiles []grainfiles.IndexRecord `json:"grainfiles"`
	}
	th.mustCall("GET", grainfiles.GrainfilesPath, "bob@example.com", nil, &bobList)
	if len(bobList.Grainfiles) != 0 {
		t.Fatalf("bob should not see alice's personal grainfile, got %+v", bobList.Grainfiles)
	}

	// 4. Node receives the live task_ready event over its rendezvous control stream
	// and fetches the Hub-hosted grainfile content via GET /v1/grainfiles/content.
	select {
	case ev := <-node.conn.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady || ev.GrainfileURL != putResp.Indexed.URL {
			t.Fatalf("unexpected grainfile event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for personal grainfile task_ready event")
	}

	q := url.Values{"grainfile_url": {putResp.Indexed.URL}}
	req, _ := http.NewRequest("GET", th.url+grainfiles.ContentPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+node.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != gfSource {
		t.Fatalf("node fetch personal grainfile content: status=%d body=%q", resp.StatusCode, body)
	}
}

// TestCUJ04_DeploymentHubGrainfileAuthoring verifies CUJ-4:
// Team member authoring a deployment-scoped Hub-hosted grainfile
// (`PUT /v1/grainfiles/team/{team}/{path...}`), team visibility, outsider isolation,
// and node fetching Hub-hosted source and same-repo references via `/v1/grainfiles/content`.
func TestCUJ04_DeploymentHubGrainfileAuthoring(t *testing.T) {
	th := newTestHub(t, nil)

	// 1. Alice creates deployment "payments" and adds Bob as a member.
	var dep directory.Team
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "payments",
		"name": "Payments",
	}, &dep)
	th.mustCall("PUT", "/v1/deployments/payments/members/user:bob@example.com", "alice@example.com", map[string]any{
		"role": "member",
	}, nil)

	teamRepoURL := "https://hub.test/team/payments"
	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:       "node-pay-1",
		Slots:    2,
		Features: []string{registry.FeatureGrainfiles},
	}, &grainfiles.Subscriptions{
		Folders: []string{teamRepoURL + "/tree/main/processes"},
	})

	// 2. Outsider Eve cannot write or read a team grainfile for "payments".
	code, _ := th.call("PUT", "/v1/grainfiles/team/payments/processes/sweep.process.md", "eve@example.com", map[string]any{
		"content": "---\ntitle: Sweep\nwhen:\n  manual: {}\n---\nSweep.\n",
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("outsider PUT team grainfile: got %d, want 404", code)
	}

	// 3. Team member Bob writes a reference doc and a standing process grainfile in "payments".
	playbookContent := "# Weekly Audit Playbook\nCheck all dependencies.\n"
	th.mustCall("PUT", "/v1/grainfiles/team/payments/docs/playbook.md", "bob@example.com", map[string]any{
		"content": playbookContent,
	}, nil)

	gfSource := `---
title: Weekly dependency audit
when:
  schedule:
    cron: "0 6 * * 1"
references:
  - /docs/playbook.md
---
Audit dependencies according to /docs/playbook.md.
`
	var putResp grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/payments/processes/sweep.process.md", "bob@example.com", map[string]any{
		"content": gfSource,
	}, &putResp)

	// 4. Both Alice and Bob see the grainfile in GET /v1/grainfiles; Eve does not.
	var listAlice struct {
		Grainfiles []grainfiles.IndexRecord `json:"grainfiles"`
	}
	th.mustCall("GET", grainfiles.GrainfilesPath, "alice@example.com", nil, &listAlice)
	if len(listAlice.Grainfiles) != 1 || listAlice.Grainfiles[0].URL != putResp.Indexed.URL {
		t.Fatalf("alice grainfiles = %+v, want %s", listAlice.Grainfiles, putResp.Indexed.URL)
	}

	var listEve struct {
		Grainfiles []grainfiles.IndexRecord `json:"grainfiles"`
	}
	th.mustCall("GET", grainfiles.GrainfilesPath, "eve@example.com", nil, &listEve)
	if len(listEve.Grainfiles) != 0 {
		t.Fatalf("eve should see 0 grainfiles, got %+v", listEve.Grainfiles)
	}

	// 5. Node receives process_updated and fetches both the process file and its /docs/playbook.md reference.
	select {
	case ev := <-node.conn.GrainfileEvents():
		if ev.Type != grainfiles.EventProcessUpdated {
			t.Fatalf("expected process_updated event, got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for team grainfile process_updated event")
	}

	fetch := func(params url.Values) (int, string) {
		req, _ := http.NewRequest("GET", th.url+grainfiles.ContentPath+"?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+node.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := fetch(url.Values{"grainfile_url": {putResp.Indexed.URL}}); code != http.StatusOK || body != gfSource {
		t.Fatalf("node fetch team grainfile: code=%d body=%q", code, body)
	}
	if code, body := fetch(url.Values{
		"grainfile_url": {putResp.Indexed.URL},
		"reference":     {"/docs/playbook.md"},
	}); code != http.StatusOK || body != playbookContent {
		t.Fatalf("node fetch team reference: code=%d body=%q", code, body)
	}
}

// TestCUJ05_GitHubGrainfileWebhookCrossRepoTriggerAndGate verifies CUJ-5:
// Repository-committed grainfiles discovered on signed GitHub push webhooks,
// pull_request gate process (`gate: true`, `when.pull_request`), PR-head untrusted
// grainfile isolation, and merge co-triggering (`depends_on` across processes).
func TestCUJ05_GitHubGrainfileWebhookCrossRepoTriggerAndGate(t *testing.T) {
	th := newTestHub(t, nil)

	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "security",
		"name": "Security",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/security/repos/acme/web-service", "alice@example.com", nil, nil)
	th.mustCall("PUT", "/v1/deployments/security/repos/acme/security-automation", "alice@example.com", nil, nil)

	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:       "node-sec-1",
		Repos:    []string{"acme/web-service", "acme/security-automation"},
		Slots:    2,
		Features: []string{registry.FeatureGrainfiles},
	}, &grainfiles.Subscriptions{
		Folders: []string{
			"https://github.com/acme/web-service/tree/main/.grain/processes",
			"https://github.com/acme/security-automation/tree/main/processes",
		},
	})

	gateProcess := `---
title: PR security gate
gate: true
when:
  pull_request:
    branches: ["main"]
    paths: ["src/**"]
  merge:
    branches: ["main"]
    paths: ["src/**"]
---
Audit PR changes under src/.
`
	coTriggeredProcess := `---
title: Post-merge compliance report
depends_on:
  - https://github.com/acme/web-service/blob/main/.grain/processes/pr-gate.process.md
when:
  manual: {}
---
Generate compliance report after PR security gate runs on merge.
`

	// 1. Commit and push the gate process to acme/web-service and the dependent process to acme/security-automation.
	_, whResp1 := th.gh.MustCommitAndPush(t, "acme", "web-service", "main", "Add PR security gate", map[string]string{
		".grain/processes/pr-gate.process.md": gateProcess,
		"src/auth.go":                         "package src\n",
	}, nil)
	if len(whResp1.Indexed) != 1 || !whResp1.Indexed[0].Gate {
		t.Fatalf("expected 1 indexed gate process, got %+v", whResp1)
	}

	_, whResp2 := th.gh.MustCommitAndPush(t, "acme", "security-automation", "main", "Add compliance report process", map[string]string{
		"processes/compliance.process.md": coTriggeredProcess,
	}, nil)
	if len(whResp2.Indexed) != 1 {
		t.Fatalf("expected 1 indexed compliance process, got %+v", whResp2)
	}

	// Drain initial process_updated events.
	for range 2 {
		select {
		case <-node.conn.GrainfileEvents():
		case <-time.After(5 * time.Second):
			t.Fatal("timed out draining process_updated events")
		}
	}

	// 2. Open a pull request on acme/web-service modifying src/auth.go (and attempting to sneak in a PR-head task).
	pr, prWhResp := th.gh.MustOpenPullRequest(t, "acme", "web-service", "main", "feat/oauth", "Add OAuth handler", map[string]string{
		"src/auth.go":                       "package src\n// oauth\n",
		".grain/tasks/sneaky.task.grain.md": "---\ntitle: Sneaky PR task\n---\nShould not be indexed from PR branch.\n",
	}, nil)

	// Verify the PR webhook did NOT index the untrusted PR-head task, and DID fire the base-branch gate process
	// (along with its co-triggered dependent compliance process).
	if len(prWhResp.Indexed) != 0 {
		t.Fatalf("PR webhook must not index PR-head grainfiles, got %+v", prWhResp.Indexed)
	}
	if prWhResp.Source != grainfiles.TriggerPullRequest || len(prWhResp.Events) != 2 {
		t.Fatalf("expected 2 pull_request trigger events (gate + co-triggered), got %+v", prWhResp)
	}

	for range 2 {
		select {
		case ev := <-node.conn.GrainfileEvents():
			if ev.Type != grainfiles.EventProcessTriggered || ev.PRNumber != pr.Number {
				t.Fatalf("expected process_triggered for PR #%d, got %+v", pr.Number, ev)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for PR gate process_triggered event")
		}
	}

	// 3. Merge the PR into main; verify both the primary gate process on acme/web-service
	// AND the dependent process on acme/security-automation co-trigger!
	_, mergeWhResp, err := th.gh.MergePullRequest("acme", "web-service", pr.Number)
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if mergeWhResp.Source != grainfiles.TriggerMerge {
		t.Fatalf("expected merge trigger source, got %+v", mergeWhResp)
	}
	var sawPrimaryMerge, sawDependentMerge bool
	for _, ev := range mergeWhResp.Events {
		if ev.Type == grainfiles.EventProcessTriggered && ev.Path == ".grain/processes/pr-gate.process.md" && ev.TriggerSource == grainfiles.TriggerMerge {
			sawPrimaryMerge = true
		}
		if ev.Type == grainfiles.EventProcessTriggered && ev.Path == "processes/compliance.process.md" && ev.TriggerSource == "depends_on" {
			sawDependentMerge = true
		}
	}
	if !sawPrimaryMerge || !sawDependentMerge {
		t.Fatalf("expected merge to fire primary and co-triggered processes, got %+v", mergeWhResp.Events)
	}
}

// TestCUJ06_CrossRepoTaskDispatchAndScopedReferenceFetch verifies CUJ-6:
// Cross-repository task dispatch (a `.task.md` in `acme/migration-control` targeting
// `recipient: https://github.com/acme/billing-service`) and scoped reference fetching
// via `GET /v1/grainfiles/content` (allowing listed same-repo files/folders while
// blocking unreferenced files, path traversal, and cross-repo secret fetches).
func TestCUJ06_CrossRepoTaskDispatchAndScopedReferenceFetch(t *testing.T) {
	th := newTestHub(t, nil)

	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "control-team",
		"name": "Control Team",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/control-team/repos/acme/migration-control", "alice@example.com", nil, nil)

	th.mustCall("POST", directory.DeploymentsPath, "bob@example.com", map[string]any{
		"id":   "billing-team",
		"name": "Billing Team",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/billing-team/repos/acme/billing-service", "bob@example.com", nil, nil)

	recipientBilling := "https://github.com/acme/billing-service"
	workerNode := mintAndConnectNode(t, th, "bob@example.com", registry.Node{
		ID:         "node-billing",
		Repos:      []string{"acme/billing-service"},
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{recipientBilling},
	}, &grainfiles.Subscriptions{
		Recipients: []string{recipientBilling},
	})

	taskMD := `---
title: Migrate billing schema
recipient: https://github.com/acme/billing-service
requestor: Alice <alice@example.com>
references:
  - /playbooks/billing.md
  - /migrations/
  - https://github.com/acme/secret-vault/blob/main/keys.txt
---
Migrate billing service according to /playbooks/billing.md.
`
	th.gh.MustCommitAndPush(t, "acme", "secret-vault", "main", "Add secret", map[string]string{
		"keys.txt": "TOP_SECRET_KEY",
	}, nil)

	commitSHA, _ := th.gh.MustCommitAndPush(t, "acme", "migration-control", "main", "Add billing migration task", map[string]string{
		".grain/tasks/migrate-billing.task.md": taskMD,
		"playbooks/billing.md":                 "# Billing Playbook\nRun 001_init.sql.",
		"migrations/001_init.sql":              "CREATE TABLE ledger (id INT);",
		"secret/internal.txt":                  "DO_NOT_EXPOSE",
	}, nil)

	// 1. Worker node (in billing-team) receives task_ready for the cross-repo task.
	var readyEv grainfiles.Event
	select {
	case readyEv = <-workerNode.conn.GrainfileEvents():
		if readyEv.Type != grainfiles.EventTaskReady {
			t.Fatalf("expected task_ready, got %+v", readyEv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-repo task_ready")
	}

	fetchContent := func(params url.Values) (int, string) {
		req, _ := http.NewRequest("GET", th.url+grainfiles.ContentPath+"?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+workerNode.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 2. Worker node fetches the .task.md itself and the authorized same-repo file & folder references.
	if code, body := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"commit":        {commitSHA},
	}); code != http.StatusOK || body != taskMD {
		t.Fatalf("fetch grainfile: code=%d body=%q", code, body)
	}

	if code, body := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"reference":     {"/playbooks/billing.md"},
	}); code != http.StatusOK || !strings.Contains(body, "Billing Playbook") {
		t.Fatalf("fetch authorized file reference: code=%d body=%q", code, body)
	}

	if code, body := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"path":          {"migrations/001_init.sql"},
	}); code != http.StatusOK || !strings.Contains(body, "CREATE TABLE ledger") {
		t.Fatalf("fetch authorized folder child: code=%d body=%q", code, body)
	}

	// 3. Security checks: unreferenced same-repo file, path traversal, and cross-repo reference are rejected.
	if code, _ := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"path":          {"secret/internal.txt"},
	}); code != http.StatusForbidden {
		t.Fatalf("unreferenced file fetch: got %d, want 403", code)
	}
	if code, _ := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"path":          {"../secret/internal.txt"},
	}); code != http.StatusForbidden && code != http.StatusBadRequest {
		t.Fatalf("path traversal fetch: got %d, want 400/403", code)
	}
	if code, body := fetchContent(url.Values{
		"grainfile_url": {readyEv.GrainfileURL},
		"reference":     {"https://github.com/acme/secret-vault/blob/main/keys.txt"},
	}); code != http.StatusForbidden || strings.Contains(body, "TOP_SECRET_KEY") {
		t.Fatalf("cross-repo secret fetch: got %d (%q), want 403", code, body)
	}
}

// TestCUJ07_Jinja2TemplatesAndDependsOnDAG verifies CUJ-7:
// Parameterized Jinja2 template grainfiles (`.task.grain.md.jinja2`) rendered via
// `?var.<name>=<value>`, combined with multi-task `depends_on` DAG ordering both
// in the task backlog (`Spec.DependsOn`) and across grainfiles (`CompleteGrainfile`).
func TestCUJ07_Jinja2TemplatesAndDependsOnDAG(t *testing.T) {
	th := newTestHub(t, nil)

	var dep directory.Team
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "release",
		"name": "Release",
	}, &dep)
	th.mustCall("PUT", "/v1/deployments/release/repos/acme/monorepo", "alice@example.com", nil, nil)

	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:         "node-rel-1",
		Repos:      []string{"acme/monorepo"},
		Slots:      4,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{dep.RecipientURL},
	}, &grainfiles.Subscriptions{
		Recipients: []string{dep.RecipientURL},
	})

	// 1. Author a prerequisite task and a Jinja2 template task that depends on it.
	prereqSource := fmt.Sprintf(`---
title: Prepare schema migration
recipient: %s
---
Run schema preparation.
`, dep.RecipientURL)
	var prereqResp grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/release/tasks/01-prep.task.grain.md", "alice@example.com", map[string]any{
		"content": prereqSource,
	}, &prereqResp)

	tplSource := fmt.Sprintf(`---
title: Roll out shard {{ shard | default('auth') | upper }}
recipient: %s
depends_on:
  - ./01-prep.task.grain.md
---
{%% if env %%}
Deploy shard {{ shard }} to {{ env | trim }}.
{%% else %%}
Deploy shard {{ shard }} to staging.
{%% endif %%}
`, dep.RecipientURL)
	var tplResp grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/release/tasks/02-rollout.task.grain.md.jinja2", "alice@example.com", map[string]any{
		"content": tplSource,
	}, &tplResp)

	if tplResp.Indexed == nil || !tplResp.Indexed.IsTemplate || len(tplResp.Indexed.TemplateVars) != 2 {
		t.Fatalf("expected indexed Jinja2 template with 2 vars, got %+v", tplResp.Indexed)
	}

	// 2. Node renders the Jinja2 template with ?var.shard=ledger&var.env=production.
	q := url.Values{
		"grainfile_url": {tplResp.Indexed.URL},
		"var.shard":     {"ledger"},
		"var.env":       {" production "},
	}
	req, _ := http.NewRequest("GET", th.url+grainfiles.ContentPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+node.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	renderedBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	rendered := string(renderedBytes)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(rendered, "title: Roll out shard LEDGER") ||
		!strings.Contains(rendered, "Deploy shard ledger to production.") ||
		!strings.Contains(rendered, "from_template: "+tplResp.Indexed.URL) {
		t.Fatalf("unexpected rendered template (status %d):\n%s", resp.StatusCode, rendered)
	}

	// 3. Complete 01-prep.task.grain.md via CompleteGrainfile and verify dependency_completed fires for 02-rollout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	depEvs, err := node.conn.CompleteGrainfile(ctx, prereqResp.Indexed.URL, prereqResp.Commit, "done", "prep complete")
	if err != nil || len(depEvs) != 1 || depEvs[0].Type != grainfiles.EventDependencyCompleted {
		t.Fatalf("CompleteGrainfile: err=%v events=%+v", err, depEvs)
	}

	// 4. Also verify backlog task DAG ordering (`depends_on` holding verify-all until both shards finish).
	var shard1, shard2, verifyAll tasks.Task
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":   "acme/monorepo",
		"title":  "Roll out shard AUTH",
		"prompt": "Deploy shard auth to production.",
	}, &shard1)
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":   "acme/monorepo",
		"title":  "Roll out shard LEDGER",
		"prompt": "Deploy shard ledger to production.",
	}, &shard2)
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":       "acme/monorepo",
		"title":      "Verify all shards",
		"prompt":     "Verify auth and ledger in production.",
		"depends_on": []string{shard1.ID, shard2.ID},
	}, &verifyAll)

	eventually(t, "shard1 and shard2 running while verifyAll stays queued", func() bool {
		var t1, t2, tv tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+shard1.ID, "alice@example.com", nil, &t1)
		th.mustCall("GET", tasks.TasksPath+"/"+shard2.ID, "alice@example.com", nil, &t2)
		th.mustCall("GET", tasks.TasksPath+"/"+verifyAll.ID, "alice@example.com", nil, &tv)
		if tv.State != tasks.Queued {
			t.Fatalf("verifyAll should be held queued while dependencies run, got %s", tv.State)
		}
		return t1.State == tasks.Running && t2.State == tasks.Running
	})

	node.finishTask(shard1.ID, tasks.Done, "auth deployed")
	node.finishTask(shard2.ID, tasks.Done, "ledger deployed")

	eventually(t, "verifyAll unblocked and running", func() bool {
		var tv tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+verifyAll.ID, "alice@example.com", nil, &tv)
		return tv.State == tasks.Running
	})
	node.finishTask(verifyAll.ID, tasks.Done, "all shards verified")
}

// TestCUJ08_DraftNotBeforeAndGrainfileRemovalLifecycle verifies CUJ-8:
// Draft grainfiles (`draft: true`), deferred grainfiles (`not_before`), promotion
// from draft/deferred to active, and removal (`DELETE` / git rm) emitting
// `grainfile_removed`.
func TestCUJ08_DraftNotBeforeAndGrainfileRemovalLifecycle(t *testing.T) {
	th := newTestHub(t, nil)

	var dep directory.Team
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "lifecycle",
		"name": "Lifecycle",
	}, &dep)

	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:         "node-life-1",
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{dep.RecipientURL},
	}, &grainfiles.Subscriptions{
		Recipients: []string{dep.RecipientURL},
	})

	// 1. Author a draft grainfile (`draft: true`) and a future `not_before` grainfile.
	draftSource := fmt.Sprintf(`---
title: Draft RFC implementation
recipient: %s
draft: true
---
Implement RFC once design is approved.
`, dep.RecipientURL)
	var draftPut grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/lifecycle/tasks/rfc.task.md", "alice@example.com", map[string]any{
		"content": draftSource,
	}, &draftPut)
	if len(draftPut.Events) != 0 {
		t.Fatalf("draft grainfile must not emit task_ready events, got %+v", draftPut.Events)
	}

	// Verify content proxy refuses to serve a draft grainfile for execution (403 Forbidden).
	q := url.Values{"grainfile_url": {draftPut.Indexed.URL}}
	req, _ := http.NewRequest("GET", th.url+grainfiles.ContentPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+node.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fetching draft grainfile content: got %d, want 403", resp.StatusCode)
	}

	futureDate := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	deferredSource := fmt.Sprintf(`---
title: Scheduled production cutover
recipient: %s
not_before: %q
---
Execute production cutover.
`, dep.RecipientURL, futureDate)
	var deferredPut grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/lifecycle/tasks/cutover.task.md", "alice@example.com", map[string]any{
		"content": deferredSource,
	}, &deferredPut)
	if len(deferredPut.Events) != 0 {
		t.Fatalf("future not_before grainfile must not emit task_ready before timestamp, got %+v", deferredPut.Events)
	}

	// 2. Promote the draft grainfile to `draft: false`; verify `task_ready` is emitted immediately.
	promotedSource := fmt.Sprintf(`---
title: Draft RFC implementation
recipient: %s
draft: false
---
Implement RFC once design is approved.
`, dep.RecipientURL)
	var promotedPut grainfiles.HostedGrainfileResponse
	th.mustCall("PUT", "/v1/grainfiles/team/lifecycle/tasks/rfc.task.md", "alice@example.com", map[string]any{
		"content": promotedSource,
	}, &promotedPut)
	if len(promotedPut.Events) != 1 || promotedPut.Events[0].Type != grainfiles.EventTaskReady {
		t.Fatalf("promoting draft=false should emit task_ready, got %+v", promotedPut.Events)
	}

	select {
	case ev := <-node.conn.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady || ev.Path != "tasks/rfc.task.md" {
			t.Fatalf("unexpected event after draft promotion: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for promoted task_ready event")
	}

	// 3. Delete the grainfile (`DELETE /v1/grainfiles/team/lifecycle/tasks/rfc.task.md`);
	// verify `task_removed` is emitted to the subscribed node.
	var delResp grainfiles.HostedGrainfileResponse
	th.mustCall("DELETE", "/v1/grainfiles/team/lifecycle/tasks/rfc.task.md", "alice@example.com", nil, &delResp)
	if len(delResp.Events) != 1 || delResp.Events[0].Type != grainfiles.EventTaskRemoved {
		t.Fatalf("expected task_removed event on DELETE, got %+v", delResp)
	}

	select {
	case ev := <-node.conn.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskRemoved || ev.Path != "tasks/rfc.task.md" {
			t.Fatalf("unexpected event after DELETE: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task_removed event")
	}
}

// TestCUJ09_CapabilityRoutingAndLostNodeReclamation verifies CUJ-9:
// Label/capability-constrained scheduling (`constraints.labels`), node refusal
// fallback when a node declines an offer, and lost-node task reclamation (`requeue`).
func TestCUJ09_CapabilityRoutingAndLostNodeReclamation(t *testing.T) {
	th := newTestHub(t, func(cfg *server.Config) {
		cfg.Heartbeat = 100 * time.Millisecond
	})

	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "compute",
		"name": "Compute",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/compute/repos/acme/ml-train", "alice@example.com", nil, nil)

	// Connect three nodes:
	// - node-cpu: labels {"accel": "cpu"}
	// - node-gpu-busy: labels {"accel": "gpu", "arch": "arm64"}, refuses offers ("gpu maintenance")
	// - node-gpu-ready: labels {"accel": "gpu", "arch": "arm64"}, accepts the offer
	nodeCPU := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:     "node-cpu",
		Repos:  []string{"acme/ml-train"},
		Labels: map[string]string{"accel": "cpu"},
	}, nil)

	mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:     "node-gpu-busy",
		Repos:  []string{"acme/ml-train"},
		Labels: map[string]string{"accel": "gpu", "arch": "arm64"},
	}, nil, func(sn *simNode) {
		sn.refuse = func(spec tasks.Spec) string {
			return "gpu maintenance"
		}
	})

	nodeGPUReady := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:     "node-gpu-ready",
		Repos:  []string{"acme/ml-train"},
		Labels: map[string]string{"accel": "gpu", "arch": "arm64"},
	}, nil)

	var created tasks.Task
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":   "acme/ml-train",
		"title":  "Fine-tune model",
		"prompt": "Run training on arm64 GPU",
		"constraints": map[string]any{
			"labels": map[string]string{"accel": "gpu", "arch": "arm64"},
		},
	}, &created)

	eventually(t, "task routed to node-gpu-ready", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+created.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "node-gpu-ready"
	})

	// Verify node-cpu never received an offer for the GPU-constrained task.
	nodeCPU.mu.Lock()
	cpuOffers := len(nodeCPU.offers)
	nodeCPU.mu.Unlock()
	if cpuOffers != 0 {
		t.Fatalf("node-cpu received %d offers for GPU task, want 0", cpuOffers)
	}

	// Simulate node-gpu-ready crashing and losing its heartbeat; task transitions to Lost.
	nodeGPUReady.Close()
	eventually(t, "task transitions to lost after node-gpu-ready disconnects", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+created.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Lost
	})

	// Connect a replacement GPU node (node-gpu-recovery) and requeue the lost task.
	nodeGPURecovery := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:     "node-gpu-recovery",
		Repos:  []string{"acme/ml-train"},
		Labels: map[string]string{"accel": "gpu", "arch": "arm64"},
	}, nil)

	th.mustCall("POST", tasks.TasksPath+"/"+created.ID+"/requeue", "alice@example.com", nil, nil)

	eventually(t, "requeued task running on node-gpu-recovery", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+created.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "node-gpu-recovery"
	})

	nodeGPURecovery.finishTask(created.ID, tasks.Done, "training completed on recovery GPU node")
}

// TestCUJ10_InteractiveTaskStreamingArtifactsApprovalsAndInbox verifies CUJ-10:
// Interactive task live event streaming, inbox aggregation (`awaiting_reply` and
// `approval_request`), approval decision forwarding (`POST /v1/tasks/{id}/approvals/{req}`),
// user message reply forwarding (`POST /v1/tasks/{id}/messages`), and on-demand
// node artifact view-through (`GET /v1/tasks/{id}/artifacts?ref=...`).
func TestCUJ10_InteractiveTaskStreamingArtifactsApprovalsAndInbox(t *testing.T) {
	th := newTestHub(t, nil)

	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "interactive-team",
		"name": "Interactive Team",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/interactive-team/repos/acme/webapp", "alice@example.com", nil, nil)

	diffBytes := []byte("--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n-old\n+new\n")
	node := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:       "node-int-1",
		Repos:    []string{"acme/webapp"},
		Features: []string{registry.FeatureInteractive, "approvals"},
	}, nil, func(sn *simNode) {
		sn.artifacts["diff-1"] = diffBytes
	})

	// 1. Alice files an interactive task.
	var task tasks.Task
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":   "acme/webapp",
		"title":  "Interactive refactor",
		"prompt": "Refactor app.go interactively",
		"kind":   tasks.KindInteractive,
	}, &task)

	eventually(t, "interactive task running on node-int-1", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+task.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "node-int-1"
	})

	// 2. Node emits status, artifact ("diff-1"), approval_request ("req-1"), and awaiting_reply.
	node.emitEvents(task.ID,
		live.Event{
			Event: events.Event{Seq: 1, Type: events.TypeStatus, Status: &events.Status{Phase: "running", State: "info", Message: "starting interactive session"}},
		},
		live.Event{
			Event:    events.Event{Seq: 2, Type: live.TypeArtifact},
			Artifact: &live.Artifact{Kind: "diff", Ref: "diff-1", Size: int64(len(diffBytes))},
		},
		live.Event{
			Event: events.Event{
				Seq:             3,
				Type:            events.TypeApprovalRequest,
				ApprovalRequest: &events.ApprovalRequest{ID: "req-1", Tool: "deploy", Message: "Allow staging deploy?"},
			},
		},
		live.Event{
			Event: events.Event{
				Seq:           4,
				Type:          events.TypeAwaitingReply,
				AwaitingReply: &events.AwaitingReply{State: events.AwaitingWaiting, Question: "Which region should we target?"},
			},
		},
	)

	// 3. Verify GET /v1/inbox surfaces both the pending approval_request and awaiting_reply items.
	eventually(t, "inbox shows approval_request and awaiting_reply", func() bool {
		var inbox struct {
			Items []tasks.InboxItem `json:"items"`
		}
		th.mustCall("GET", tasks.InboxPath, "alice@example.com", nil, &inbox)
		var sawApproval, sawAwaiting bool
		for _, item := range inbox.Items {
			if item.TaskID != task.ID {
				continue
			}
			if item.Kind == tasks.InboxApprovalRequest && item.ApprovalID == "req-1" {
				sawApproval = true
			}
			if item.Kind == tasks.InboxAwaitingReply && item.Question == "Which region should we target?" {
				sawAwaiting = true
			}
		}
		return sawApproval && sawAwaiting
	})

	// 4. Alice approves "req-1" and sends a reply message "Target us-east-1".
	th.mustCall("POST", tasks.TasksPath+"/"+task.ID+"/approvals/req-1", "alice@example.com", map[string]any{
		"decision": "allow",
		"reason":   "Staging is safe",
	}, nil)

	th.mustCall("POST", tasks.TasksPath+"/"+task.ID+"/messages", "alice@example.com", map[string]any{
		"text": "Target us-east-1",
	}, nil)

	eventually(t, "node received approval and user message", func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return len(node.approves) == 1 && node.approves[0].Decision == "allow" &&
			len(node.messages) == 1 && node.messages[0].Text == "Target us-east-1"
	})

	// 5. Alice views the node-held artifact ("diff-1") through the Hub.
	code, artBody := th.call("GET", tasks.TasksPath+"/"+task.ID+"/artifacts?ref=diff-1", "alice@example.com", nil, nil)
	if code != http.StatusOK || !bytes.Equal(artBody, diffBytes) {
		t.Fatalf("artifact view-through: code=%d body=%q, want %q", code, artBody, diffBytes)
	}

	// Unlisted artifact refs are rejected with 404 before touching the node.
	if code, _ := th.call("GET", tasks.TasksPath+"/"+task.ID+"/artifacts?ref=unlisted-ref", "alice@example.com", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unlisted artifact ref: got %d, want 404", code)
	}

	node.finishTask(task.ID, tasks.Done, "interactive refactor completed")
}

// TestCUJ11_HubUIDeploymentSwitcherThemeAndRoleProxy verifies CUJ-11:
// Hub Web UI shell, multi-deployment switcher (`GET /v1/deployments`), theme
// preference persistence (`PUT /v1/me/theme`), inbox node filter (`PUT /v1/me/inbox`),
// and role-aware node UI reverse proxy (`https://<node>.nodes.<hub>/`).
func TestCUJ11_HubUIDeploymentSwitcherThemeAndRoleProxy(t *testing.T) {
	th := newTestHub(t, nil)

	// 1. Verify the Web UI serves its HTML shell with strict CSP on UI routes.
	for _, route := range []string{"/", "/grainfiles", "/nodes", "/teams", "/file"} {
		req, _ := http.NewRequest("GET", th.url+route, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "<html") {
			t.Fatalf("GET %s: status=%d", route, resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "worker-src 'none'") {
			t.Fatalf("GET %s missing expected CSP: %q", route, csp)
		}
	}

	// 2. Alice creates two deployments ("team-alpha" and "team-beta"), adds Bob as a member
	// of "team-alpha", and updates her theme preference (`PUT /v1/me/theme`).
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "team-alpha",
		"name": "Team Alpha",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/team-alpha/members/user:bob@example.com", "alice@example.com", map[string]any{
		"role": "member",
	}, nil)
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "team-beta",
		"name": "Team Beta",
	}, nil)

	th.mustCall("PUT", "/v1/me/theme", "alice@example.com", map[string]any{
		"theme": "light",
	}, nil)

	var me struct {
		Theme string                    `json:"theme"`
		Teams map[string]directory.Role `json:"teams"`
	}
	th.mustCall("GET", "/v1/me", "alice@example.com", nil, &me)
	if me.Theme != "light" || len(me.Teams) != 2 {
		t.Fatalf("GET /v1/me: theme=%q teams=%v, want light and 2 teams", me.Theme, me.Teams)
	}

	// 3. Connect node "team-alpha" under team-alpha and verify role-aware node UI reverse proxy.
	th.mustCall("PUT", "/v1/deployments/team-alpha/repos/acme/alpha", "alice@example.com", nil, nil)
	var tokResp struct {
		Token string `json:"token"`
	}
	th.mustCall("POST", "/v1/deployments/team-alpha/token", "alice@example.com", nil, &tokResp)
	connectSimNode(t, th, tokResp.Token, registry.Node{
		ID:       "team-alpha",
		Repos:    []string{"acme/alpha"},
		Slots:    2,
		Features: []string{registry.FeatureNodeUI},
		UI: &registry.NodeUI{
			Offered: true,
			Team:    "team-alpha",
		},
	}, nil)

	eventually(t, "team-alpha node online", func() bool {
		var dir struct {
			Nodes []registry.Entry `json:"nodes"`
		}
		th.mustCall("GET", rendezvous.DirectoryPath, "alice@example.com", nil, &dir)
		for _, n := range dir.Nodes {
			if n.ID == "team-alpha" && n.Status == registry.Online {
				return true
			}
		}
		return false
	})

	// Alice configures her inbox filter for team-alpha.
	th.mustCall("PUT", "/v1/me/inbox", "alice@example.com", map[string]any{
		"nodes": []string{"team-alpha"},
	}, nil)

	proxyGet := func(user string) (int, string) {
		req, _ := http.NewRequest("GET", th.url+"/dashboard", nil)
		req.Host = "team-alpha." + nodeDomain
		if user != "" {
			req.Header.Set(userHeader, user)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Deployment owner Alice reaches team-alpha's local UI with role=owner.
	code, body := proxyGet("alice@example.com")
	if code != http.StatusOK || !strings.Contains(body, "node=team-alpha user=alice@example.com role=owner path=/dashboard") {
		t.Fatalf("alice node UI proxy: code=%d body=%q", code, body)
	}

	// Deployment member Bob reaches team-alpha's local UI with role=member.
	code, body = proxyGet("bob@example.com")
	if code != http.StatusOK || !strings.Contains(body, "node=team-alpha user=bob@example.com role=member path=/dashboard") {
		t.Fatalf("bob node UI proxy: code=%d body=%q", code, body)
	}

	// By default (anyone_may_propose=true), outsider Eve reaches team-alpha's local UI with role=proposer.
	code, body = proxyGet("eve@example.com")
	if code != http.StatusOK || !strings.Contains(body, "node=team-alpha user=eve@example.com role=proposer path=/dashboard") {
		t.Fatalf("outsider proposer node UI proxy: code=%d body=%q", code, body)
	}

	// When team-alpha disables anyone_may_propose, outsiders and non-member SysAdmins get 404 Not Found.
	th.mustCall("PUT", "/v1/deployments/team-alpha/settings", "alice@example.com", map[string]any{
		"require_approval":   true,
		"anyone_may_propose": false,
	}, nil)

	// Outsider Eve (not in team-alpha) gets 404 Not Found.
	if code, _ := proxyGet("eve@example.com"); code != http.StatusNotFound {
		t.Fatalf("outsider node UI proxy: got %d, want 404", code)
	}

	// Hub SysAdmin (not a member of team-alpha) is ALSO denied access to the node's local UI (404).
	if code, _ := proxyGet("sysadmin@example.com"); code != http.StatusNotFound {
		t.Fatalf("sysadmin non-member node UI proxy: got %d, want 404", code)
	}
}

// TestCUJ12_TaskProposalAndApprovalMatrix verifies CUJ-12:
// Running agent filing child tasks upward via `conn.FileTask` across all four
// cases of the Hub task proposal & approval matrix:
//
//	Case 1: Same-team repository (`acme/service-a`) filed from a team member's task ->
//	        Queued immediately and dispatched automatically.
//	Case 2: Cross-team repository (`acme/service-b` owned by `team-b`) -> held in
//	        `Proposed` state until a member of `team-b` (Bob) approves it via
//	        `POST /v1/tasks/{id}/approve` (while the submitter Alice is forbidden
//	        from self-approving).
//	Case 3: No approval laundering: a task proposed by an outsider (`eve@example.com`)
//	        and approved by Alice runs on `node-a`; when that task files a follow-up
//	        child task in `acme/service-a`, the child task is still `Proposed` (not
//	        auto-approved) because parent approval does not launder onto child filings.
//	Case 4: Unowned repository (`other/unclaimed`) -> refused by the Hub.
func TestCUJ12_TaskProposalAndApprovalMatrix(t *testing.T) {
	th := newTestHub(t, nil)

	// Set up team-a (Alice) owning acme/service-a, and team-b (Bob) owning acme/service-b.
	th.mustCall("POST", directory.DeploymentsPath, "alice@example.com", map[string]any{
		"id":   "team-a",
		"name": "Team A",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/team-a/repos/acme/service-a", "alice@example.com", nil, nil)

	th.mustCall("POST", directory.DeploymentsPath, "bob@example.com", map[string]any{
		"id":   "team-b",
		"name": "Team B",
	}, nil)
	th.mustCall("PUT", "/v1/deployments/team-b/repos/acme/service-b", "bob@example.com", nil, nil)

	nodeA := mintAndConnectNode(t, th, "alice@example.com", registry.Node{
		ID:       "node-a",
		Repos:    []string{"acme/service-a"},
		Slots:    4,
		Features: []string{"filing"},
	}, nil)
	nodeB := mintAndConnectNode(t, th, "bob@example.com", registry.Node{
		ID:       "node-b",
		Repos:    []string{"acme/service-b"},
		Slots:    4,
		Features: []string{"filing"},
	}, nil)

	// Start a parent task submitted by Alice on node-a.
	var parent tasks.Task
	th.mustCall("POST", tasks.TasksPath, "alice@example.com", map[string]any{
		"repo":   "acme/service-a",
		"title":  "Parent orchestrator task",
		"prompt": "Coordinate sub-tasks",
	}, &parent)

	var parentLocalID string
	eventually(t, "parent task running on node-a", func() bool {
		nodeA.mu.Lock()
		parentLocalID = nodeA.localIDs[parent.ID]
		nodeA.mu.Unlock()
		if parentLocalID == "" {
			return false
		}
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+parent.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running
	})

	fileFrom := func(originID, localID string, f tasks.Filing) (*tasks.Task, error) {
		f.OriginTask = originID
		f.NodeTask = localID
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		raw, err := nodeA.conn.FileTask(ctx, f)
		if err != nil {
			return nil, err
		}
		var child tasks.Task
		if err := json.Unmarshal(raw, &child); err != nil {
			return nil, err
		}
		return &child, nil
	}

	// Case 1: Same-team repo (acme/service-a) from Alice's task -> Queued & dispatched immediately.
	child1, err := fileFrom(parent.ID, parentLocalID, tasks.Filing{
		Repo:   "acme/service-a",
		Title:  "Child 1: same team repo",
		Prompt: "Update unit tests in service-a",
	})
	if err != nil {
		t.Fatalf("Case 1 FileTask failed: %v", err)
	}
	eventually(t, "child1 running on node-a", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+child1.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "node-a"
	})
	nodeA.finishTask(child1.ID, tasks.Done, "child1 done")

	// Case 2: Cross-team repo (acme/service-b owned by team-b) -> Proposed until Bob approves.
	child2, err := fileFrom(parent.ID, parentLocalID, tasks.Filing{
		Repo:   "acme/service-b",
		Title:  "Child 2: cross-team repo",
		Prompt: "Update client SDK in service-b",
	})
	if err != nil {
		t.Fatalf("Case 2 FileTask failed: %v", err)
	}
	if child2.State != tasks.Proposed || child2.Team != "team-b" {
		t.Fatalf("Case 2 child2 = %+v, want Proposed in team-b", child2)
	}

	// Submitter Alice cannot approve her own proposal (403 Forbidden).
	if code, _ := th.call("POST", tasks.TasksPath+"/"+child2.ID+"/approve", "alice@example.com", nil, nil); code != http.StatusForbidden {
		t.Fatalf("submitter self-approving proposal: got %d, want 403", code)
	}

	// Bob (member of team-b) sees child2 in his inbox and approves it.
	var bobInbox struct {
		Items []tasks.InboxItem `json:"items"`
	}
	th.mustCall("GET", tasks.InboxPath, "bob@example.com", nil, &bobInbox)
	sawProposal := false
	for _, item := range bobInbox.Items {
		if item.TaskID == child2.ID && item.Kind == tasks.InboxProposal {
			sawProposal = true
		}
	}
	if !sawProposal {
		t.Fatalf("expected child2 proposal in Bob's inbox, got %+v", bobInbox.Items)
	}

	th.mustCall("POST", tasks.TasksPath+"/"+child2.ID+"/approve", "bob@example.com", nil, nil)
	eventually(t, "approved child2 running on node-b", func() bool {
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+child2.ID, "bob@example.com", nil, &tk)
		return tk.State == tasks.Running && tk.Node == "node-b" && tk.ApprovedBy == "bob@example.com"
	})
	nodeB.finishTask(child2.ID, tasks.Done, "child2 done")

	// Case 3: No approval laundering: outsider Eve proposes a task to acme/service-a,
	// Alice approves it, and while running on node-a it files a follow-up task.
	var eveParent tasks.Task
	th.mustCall("POST", tasks.TasksPath, "eve@example.com", map[string]any{
		"repo":   "acme/service-a",
		"title":  "Eve's proposed parent task",
		"prompt": "Run Eve's parent task",
	}, &eveParent)
	if eveParent.State != tasks.Proposed {
		t.Fatalf("eveParent.State = %q, want proposed", eveParent.State)
	}
	th.mustCall("POST", tasks.TasksPath+"/"+eveParent.ID+"/approve", "alice@example.com", nil, nil)

	var eveLocalID string
	eventually(t, "eveParent running on node-a", func() bool {
		nodeA.mu.Lock()
		eveLocalID = nodeA.localIDs[eveParent.ID]
		nodeA.mu.Unlock()
		if eveLocalID == "" {
			return false
		}
		var tk tasks.Task
		th.mustCall("GET", tasks.TasksPath+"/"+eveParent.ID, "alice@example.com", nil, &tk)
		return tk.State == tasks.Running
	})

	child3, err := fileFrom(eveParent.ID, eveLocalID, tasks.Filing{
		Repo:   "acme/service-a",
		Title:  "Child 3: filed from Eve's task",
		Prompt: "Follow-up from Eve's task",
	})
	if err != nil {
		t.Fatalf("Case 3 FileTask failed: %v", err)
	}
	if child3.State != tasks.Proposed || child3.Submitter != "eve@example.com" || child3.ApprovedBy != "" {
		t.Fatalf("Case 3 approval laundering check failed: child3 = %+v, want Proposed with empty ApprovedBy", child3)
	}

	// Case 4: Unowned repository outside any team -> refused by the Hub.
	if _, err := fileFrom(parent.ID, parentLocalID, tasks.Filing{
		Repo:   "other/unclaimed",
		Title:  "Child 4: unowned repo",
		Prompt: "Should be refused",
	}); err == nil {
		t.Fatalf("Case 4 FileTask to unowned repo succeeded, want refusal error")
	}

	// Verify parent task on node-a received dependency_done notifications for child1 and child2.
	eventually(t, "parent task received dependency_done for child1 and child2", func() bool {
		nodeA.mu.Lock()
		defer nodeA.mu.Unlock()
		return len(nodeA.depsDone) >= 2
	})

	nodeA.finishTask(eveParent.ID, tasks.Done, "eveParent done")
	nodeA.finishTask(parent.ID, tasks.Done, "all child tasks orchestrated")
}
