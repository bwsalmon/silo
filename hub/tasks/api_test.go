package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
)

// headerAuth is a test sign-on: X-User is the email.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	u := r.Header.Get("X-User")
	if u == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{Email: u}, nil
}

type apiHub struct {
	api   *API
	store *Store
	dir   *directory.Store
	url   string
}

// newAPIHub serves the backlog's API, and the directory's, over the
// harness's directory: alice admins infra (bwsalmon/silo,
// bwsalmon/grain), dave is a member of it, bob is in web
// (bwsalmon/web), and root is the hub admin.
func newAPIHub(t *testing.T) *apiHub {
	ctx := context.Background()
	dir, err := directory.Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { dir.Close() })
	check(t, dir.CreateTeam(ctx, "infra", ""))
	check(t, dir.CreateTeam(ctx, "web", ""))
	check(t, dir.SetMember(ctx, "infra", "user:alice@example.com", directory.Admin))
	check(t, dir.SetMember(ctx, "infra", "user:dave@example.com", directory.Member))
	check(t, dir.SetMember(ctx, "web", "user:bob@example.com", directory.Member))
	check(t, dir.AssignRepo(ctx, "infra", "bwsalmon/silo"))
	check(t, dir.AssignRepo(ctx, "web", "bwsalmon/web"))
	store, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { store.Close() })
	store.Now = ticking()
	dirAPI := &directory.API{Store: dir, Auth: headerAuth{}, HubAdmins: []string{"root@example.com"}}
	api := &API{Store: store, Directory: dirAPI}
	m := http.NewServeMux()
	m.Handle(InboxPath, api.Handler())
	m.Handle(TasksPath, api.Handler())
	m.Handle(TasksPath+"/", api.Handler())
	m.Handle(ChatsPath, api.Handler())
	m.Handle(ChatsPath+"/", api.Handler())
	m.Handle("/", dirAPI.Handler())
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return &apiHub{api: api, store: store, dir: dir, url: ts.URL}
}

func (h *apiHub) do(t *testing.T, user, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	check(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *apiHub) submit(t *testing.T, user string, body map[string]any) Task {
	t.Helper()
	code, b := h.do(t, user, "POST", TasksPath, body)
	if code != http.StatusCreated {
		t.Fatalf("%s submitting %v: %d %s", user, body, code, b)
	}
	var task Task
	check(t, json.Unmarshal(b, &task))
	return task
}

func (h *apiHub) list(t *testing.T, user, query string) []Task {
	t.Helper()
	code, b := h.do(t, user, "GET", TasksPath+query, nil)
	if code != http.StatusOK {
		t.Fatalf("%s listing: %d %s", user, code, b)
	}
	var out struct{ Tasks []Task }
	check(t, json.Unmarshal(b, &out))
	return out.Tasks
}

func TestAPISubmit(t *testing.T) {
	h := newAPIHub(t)
	task := h.submit(t, "Alice@Example.com", map[string]any{
		"repo": "BWSalmon/silo", "title": "Fix it", "prompt": "fix the parser",
		"constraints": map[string]any{"labels": map[string]string{"zone": "eu"}},
	})
	if task.Team != "infra" || task.Repo != "bwsalmon/silo" || task.Submitter != "alice@example.com" ||
		task.State != Queued || task.Constraints.Labels["zone"] != "eu" {
		t.Errorf("submitted %+v", task)
	}
	team := h.submit(t, "bob@example.com", map[string]any{"team": "web", "prompt": "tidy up"})
	if team.Team != "web" || team.Repo != "" || team.Title != "tidy up" {
		t.Errorf("submitted to a team: %+v", team)
	}

	for _, c := range []struct {
		user string
		body map[string]any
		want int
	}{
		{"", map[string]any{"repo": "bwsalmon/silo", "prompt": "x"}, http.StatusUnauthorized},
		{"bob@example.com", map[string]any{"team": "infra", "prompt": "x"}, http.StatusNotFound},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/unowned", "prompt": "x"}, http.StatusNotFound},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo", "team": "infra", "prompt": "x"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"prompt": "x"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "x", "submitter": "bob@example.com"}, http.StatusBadRequest},
		// A submission cannot name its own approver (INV-17).
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "x", "approved_by": "bob@example.com"}, http.StatusBadRequest},
		{"root@example.com", map[string]any{"team": "nope", "prompt": "x"}, http.StatusNotFound},
	} {
		if code, b := h.do(t, c.user, "POST", TasksPath, c.body); code != c.want {
			t.Errorf("%s submitting %v: %d %s, want %d", c.user, c.body, code, b, c.want)
		}
	}
	// Outside the repository's team, a submission is a proposal.
	if p := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "x"}); p.State != Proposed || p.Team != "infra" || p.ApprovedBy != "" {
		t.Errorf("bob's proposal: %+v", p)
	}
	// A hub admin may submit anywhere.
	h.submit(t, "root@example.com", map[string]any{"team": "infra", "prompt": "x"})
}

func TestAPIVisibility(t *testing.T) {
	h := newAPIHub(t)
	mine := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "a"})
	theirs := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/web", "prompt": "b"})

	if ts := h.list(t, "dave@example.com", ""); len(ts) != 1 || ts[0].ID != mine.ID {
		t.Errorf("dave sees %v, want infra's task only", ts)
	}
	if ts := h.list(t, "bob@example.com", ""); len(ts) != 1 || ts[0].ID != theirs.ID {
		t.Errorf("bob sees %v, want web's task only", ts)
	}
	if ts := h.list(t, "bob@example.com", "?team=infra"); len(ts) != 0 {
		t.Errorf("bob asking for infra's tasks sees %v", ts)
	}
	if ts := h.list(t, "root@example.com", ""); len(ts) != 2 || ts[0].ID != theirs.ID {
		t.Errorf("a hub admin sees %v, want both, newest first", ts)
	}
	if ts := h.list(t, "root@example.com", "?state=running"); len(ts) != 0 {
		t.Errorf("running: %v", ts)
	}
	if code, _ := h.do(t, "alice@example.com", "GET", TasksPath+"?state=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("a bogus state: %d", code)
	}
	if code, _ := h.do(t, "alice@example.com", "GET", TasksPath+"/"+mine.ID, nil); code != http.StatusOK {
		t.Errorf("alice reading her task: %d", code)
	}
	if code, _ := h.do(t, "alice@example.com", "GET", TasksPath+"/"+theirs.ID, nil); code != http.StatusNotFound {
		t.Errorf("alice reading web's task: %d, want 404", code)
	}
}

// TestRepoSquatting was audit SA1-M2's exploit: once infra released
// bwsalmon/silo, or was deleted, any team admin could claim it first
// and so see every task infra had filed in it. Only a hub admin assigns
// a repository now, and its earlier tasks go to the new team only when
// the hub admin transfers them with it.
func TestRepoSquatting(t *testing.T) {
	ctx := context.Background()
	h := newAPIHub(t)
	check(t, h.dir.CreateTeam(ctx, "squat", ""))
	check(t, h.dir.SetMember(ctx, "squat", "user:mallory@example.com", directory.Admin))
	old := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "secret"})
	want := func(code int, user, method, path string, body any) {
		t.Helper()
		if got, b := h.do(t, user, method, path, body); got != code {
			t.Fatalf("%s %s as %s: %d %s, want %d", method, path, user, got, b, code)
		}
	}
	sees := func(user string, task Task) bool {
		t.Helper()
		code, _ := h.do(t, user, "GET", TasksPath+"/"+task.ID, nil)
		return code == http.StatusOK
	}
	repo := "/v1/teams/%s/repos/bwsalmon/silo"
	at := func(team string) string { return strings.Replace(repo, "%s", team, 1) }

	// While infra owns it, Mallory cannot claim it; once infra releases it,
	// Mallory still cannot claim it with history (403) or see infra's task.
	want(http.StatusConflict, "mallory@example.com", "PUT", at("squat"), nil)
	want(http.StatusNoContent, "alice@example.com", "DELETE", at("infra"), nil)
	want(http.StatusForbidden, "mallory@example.com", "PUT", at("squat"), map[string]bool{"history": true})
	if owner, _ := h.dir.RepoOwner(ctx, "bwsalmon/silo"); owner != "" {
		t.Fatalf("bwsalmon/silo was claimed by %s", owner)
	}
	if sees("mallory@example.com", old) {
		t.Errorf("mallory sees infra's task")
	}

	// A member (not admin) of web cannot claim it; the hub admin (or team admin) assigns it to web.
	want(http.StatusForbidden, "bob@example.com", "PUT", at("web"), nil)
	want(http.StatusNoContent, "root@example.com", "PUT", at("web"), nil)
	want(http.StatusConflict, "mallory@example.com", "PUT", at("squat"), nil)
	want(http.StatusConflict, "root@example.com", "PUT", at("squat"), nil)
	mine := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "b"})
	if mine.Team != "web" || !sees("bob@example.com", mine) {
		t.Errorf("web's own task %+v", mine)
	}
	if sees("bob@example.com", old) {
		t.Errorf("assigning bwsalmon/silo to web showed it infra's task")
	}
	if !sees("alice@example.com", old) || sees("alice@example.com", mine) {
		t.Errorf("infra sees its own task: %t; web's: %t", sees("alice@example.com", old), sees("alice@example.com", mine))
	}
	if code, _ := h.do(t, "bob@example.com", "POST", TasksPath+"/"+old.ID+"/cancel", nil); code != http.StatusNotFound {
		t.Errorf("bob cancelling infra's task: %d, want 404", code)
	}

	// Transferred with its history, explicitly, web sees infra's task.
	want(http.StatusNoContent, "root@example.com", "PUT", at("web"), map[string]bool{"history": true})
	if !sees("bob@example.com", old) {
		t.Errorf("a transfer with history did not show web infra's task")
	}

	// A deleted team's repositories go to the next owner without their
	// history too.
	want(http.StatusNoContent, "root@example.com", "DELETE", "/v1/teams/web", nil)
	want(http.StatusNoContent, "root@example.com", "PUT", at("squat"), nil)
	if sees("mallory@example.com", mine) || sees("mallory@example.com", old) {
		t.Errorf("squat inherited a deleted team's tasks")
	}
}

func TestAPICancelAndRequeue(t *testing.T) {
	h := newAPIHub(t)
	ctx := context.Background()
	byDave := h.submit(t, "dave@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "a"})
	byAlice := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "b"})

	path := TasksPath + "/" + byAlice.ID
	if code, _ := h.do(t, "bob@example.com", "POST", path+"/cancel", nil); code != http.StatusNotFound {
		t.Errorf("bob cancelling infra's task: %d, want 404", code)
	}
	if code, _ := h.do(t, "dave@example.com", "POST", path+"/cancel", nil); code != http.StatusForbidden {
		t.Errorf("dave cancelling alice's task: %d, want 403", code)
	}
	// Its submitter, and a team admin, may.
	if code, b := h.do(t, "dave@example.com", "POST", TasksPath+"/"+byDave.ID+"/cancel", nil); code != http.StatusOK {
		t.Errorf("dave cancelling his own task: %d %s", code, b)
	}
	code, b := h.do(t, "alice@example.com", "POST", TasksPath+"/"+byDave.ID+"/cancel", map[string]string{"reason": "again"})
	if code != http.StatusConflict {
		t.Errorf("cancelling a cancelled task: %d %s, want 409", code, b)
	}

	// A lost task, requeued by a human.
	_, err := h.store.Offer(ctx, byAlice.ID, "ws-1", "alice@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, byAlice.ID, "")
	check(t, err)
	_, err = h.store.Lose(ctx, byAlice.ID, "gone")
	check(t, err)
	code, b = h.do(t, "alice@example.com", "POST", path+"/requeue", nil)
	if code != http.StatusOK {
		t.Fatalf("requeueing: %d %s", code, b)
	}
	var got Task
	check(t, json.Unmarshal(b, &got))
	if got.State != Queued || got.Node != "" {
		t.Errorf("requeued: %+v", got)
	}
	if code, _ := h.do(t, "alice@example.com", "POST", path+"/cancel", map[string]string{"reason": strings.Repeat("x", maxReason+1)}); code != http.StatusBadRequest {
		t.Errorf("cancelling with a %d-byte reason: %d, want 400", maxReason+1, code)
	}
	code, b = h.do(t, "alice@example.com", "POST", path+"/cancel", map[string]string{"reason": "changed my mind"})
	check(t, json.Unmarshal(b, &got))
	if code != http.StatusOK || got.State != Cancelled || got.Reason != "cancelled by alice@example.com: changed my mind" {
		t.Errorf("cancelled: %d %+v", code, got)
	}
}

// TestAPISameOrigin: a page on a node's UI host, which is the same site
// as the hub, cannot file, cancel or requeue tasks as its viewer
// (SA1-M1), nor can another site, nor a no-cors text/plain body.
func TestAPISameOrigin(t *testing.T) {
	h := newAPIHub(t)
	task := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "one"})
	host := strings.TrimPrefix(h.url, "http://")
	send := func(path, contentType, body string, header ...string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", h.url+path, strings.NewReader(body))
		req.Header.Set("X-User", "alice@example.com")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for i := 0; i < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		check(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	submit := `{"repo":"bwsalmon/silo","prompt":"forged"}`
	nodeUI := "http://ws-1.nodes." + host
	for _, c := range []struct {
		path, ctype, body string
		header            []string
		want              int
	}{
		{TasksPath, "text/plain;charset=UTF-8", submit, []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{TasksPath, "application/json", submit, []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{TasksPath, "application/json", submit, []string{"Origin", nodeUI}, 403},
		{TasksPath, "application/json", submit, []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}, 403},
		{TasksPath, "text/plain", submit, nil, 415},
		{TasksPath + "/" + task.ID + "/cancel", "text/plain", `{"reason":"forged"}`, nil, 415},
		{TasksPath + "/" + task.ID + "/cancel", "", "", []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{TasksPath + "/" + task.ID + "/requeue", "", "", []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
	} {
		if got := send(c.path, c.ctype, c.body, c.header...); got != c.want {
			t.Errorf("POST %s %s %v: %d, want %d", c.path, c.ctype, c.header, got, c.want)
		}
	}
	if got := h.list(t, "alice@example.com", ""); len(got) != 1 || got[0].State != task.State {
		t.Errorf("after refused writes: %+v, want only %s, %s", got, task.ID, task.State)
	}
	// The hub's own page is let through.
	if got := send(TasksPath, "application/json", submit, "Sec-Fetch-Site", "same-origin", "Origin", h.url); got != http.StatusCreated {
		t.Errorf("same-origin submit: %d, want 201", got)
	}
}

// A submission may depend only on tasks its submitter sees, and the
// dependencies are kept with it.
func TestAPIDependsOn(t *testing.T) {
	h := newAPIHub(t)
	infra := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "first"})
	web := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/web", "prompt": "bob's"})

	got := h.submit(t, "dave@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "then", "depends_on": []string{infra.ID}})
	if len(got.DependsOn) != 1 || got.DependsOn[0] != infra.ID {
		t.Errorf("depends_on %v", got.DependsOn)
	}
	read, err := h.store.Get(context.Background(), got.ID)
	check(t, err)
	if len(read.DependsOn) != 1 || read.DependsOn[0] != infra.ID {
		t.Errorf("stored depends_on %v", read.DependsOn)
	}
	for name, deps := range map[string][]string{
		"unseen":    {web.ID},
		"missing":   {"t-0123456789abcdef"},
		"malformed": {"web"},
	} {
		if code, b := h.do(t, "dave@example.com", "POST", TasksPath, map[string]any{"repo": "bwsalmon/silo", "prompt": "x", "depends_on": deps}); code != http.StatusBadRequest {
			t.Errorf("%s dependency: %d %s, want 400", name, code, b)
		}
	}
}

func TestNodeOriginatedAndGrainfileRuns(t *testing.T) {
	ctx := context.Background()
	h := newAPIHub(t)

	reg := registry.New()
	lease, err := reg.Register(registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 2}, "alice@example.com")
	check(t, err)
	t.Cleanup(func() { lease.Close() })

	streamMsgs := make(chan Message, 16)
	disp := &Dispatcher{
		Store:     h.store,
		Registry:  reg,
		Directory: h.dir,
		Open: func(ctx context.Context, nodeID string) (net.Conn, error) {
			c1, c2 := net.Pipe()
			go func() {
				defer c2.Close()
				st := NewStream(c2)
				for {
					m, err := st.Read()
					if err != nil {
						return
					}
					streamMsgs <- m
				}
			}()
			return c1, nil
		},
	}
	h.api.Dispatcher = disp
	reqMap := requests{}
	h.api.Requests = reqMap

	var terminals []Task
	h.store.OnTerminal = func(ctx context.Context, t Task) {
		terminals = append(terminals, t)
	}

	filer := &Filer{
		Store:      h.store,
		Directory:  h.dir,
		Registry:   reg,
		Dispatcher: disp,
	}

	// 1. Unauthorized owner (bob is in web, not infra) cannot register a run for bwsalmon/silo.
	_, err = filer.RegisterRun(ctx, "ws-1", "bob@example.com", RunRegistration{
		ID:           "t-unauth",
		NodeTask:     "local-unauth",
		Repo:         "bwsalmon/silo",
		GrainfileURL: "https://github.com/bwsalmon/silo/blob/main/.grain/build.task.grain.md",
	})
	if !errors.Is(err, directory.ErrNotAdmitted) {
		t.Fatalf("expected ErrNotAdmitted for bob on bwsalmon/silo, got %v", err)
	}

	// 2. Register a Grainfile-triggered process run directly in Running state (without Dispatcher offer/accept).
	procRun, err := filer.RegisterRun(ctx, "ws-1", "alice@example.com", RunRegistration{
		ID:            "t-proc-1",
		NodeTask:      "local-proc-42",
		Repo:          "bwsalmon/silo",
		Title:         "Nightly benchmark",
		Kind:          KindProcess,
		GrainfileURL:  "https://github.com/bwsalmon/silo/blob/main/.grain/bench.process.grain.md",
		CommitSHA:     "deadbeef1234",
		TriggerSource: "push",
	})
	check(t, err)
	if procRun.State != Running || procRun.Node != "ws-1" || procRun.NodeOwner != "alice@example.com" || procRun.NodeTask != "local-proc-42" {
		t.Fatalf("unexpected registered run state/node: %+v", procRun)
	}
	if procRun.Kind != KindProcess || procRun.GrainfileURL != "https://github.com/bwsalmon/silo/blob/main/.grain/bench.process.grain.md" || procRun.CommitSHA != "deadbeef1234" || procRun.TriggerSource != "push" {
		t.Fatalf("unexpected provenance fields: %+v", procRun)
	}

	// 3. Recover leaves node-originated runs (Attempts == 0) in Running state.
	check(t, h.store.Recover(ctx, "hub restarted"))
	gotAfterRecover, err := h.store.Get(ctx, "t-proc-1")
	check(t, err)
	if gotAfterRecover.State != Running {
		t.Fatalf("Recover changed node-originated run state to %s, want running", gotAfterRecover.State)
	}

	// 4. Multi-tenant visibility in GET /v1/tasks: infra members (alice, dave) see it; web member (bob) does not.
	if ts := h.list(t, "dave@example.com", ""); len(ts) != 1 || ts[0].ID != "t-proc-1" || ts[0].GrainfileURL != procRun.GrainfileURL {
		t.Fatalf("dave list: %+v", ts)
	}
	if ts := h.list(t, "bob@example.com", ""); len(ts) != 0 {
		t.Fatalf("bob should not see infra's run, got %+v", ts)
	}

	// 5. Interactive actions: approve, message, and cancel routed over reverse tunnel with node_task.
	reqMap["t-proc-1"] = map[string]events.ApprovalRequest{
		"req-deploy": {ID: "req-deploy", Tool: "deploy", Message: "Deploy to staging?"},
	}
	code, b := h.do(t, "dave@example.com", "POST", TasksPath+"/t-proc-1/approvals/req-deploy", map[string]string{
		"decision": "allow",
		"reason":   "looks good",
	})
	if code != http.StatusOK {
		t.Fatalf("POST approval: %d %s", code, b)
	}
	select {
	case m := <-streamMsgs:
		if m.Type != TypeApprove || m.Task == nil || m.Task.ID != "t-proc-1" || m.NodeTask != "local-proc-42" || m.Request != "req-deploy" || m.Decision != Allow {
			t.Fatalf("unexpected approve stream message: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approve message on node stream")
	}

	code, b = h.do(t, "dave@example.com", "POST", TasksPath+"/t-proc-1/messages", map[string]any{
		"id":   "msg-1",
		"text": "Also check memory allocations",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST message: %d %s", code, b)
	}
	select {
	case m := <-streamMsgs:
		if m.Type != TypeMessage || m.Task == nil || m.Task.ID != "t-proc-1" || m.NodeTask != "local-proc-42" || m.Message == nil || m.Message.Text != "Also check memory allocations" {
			t.Fatalf("unexpected message stream message: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for user message on node stream")
	}

	code, b = h.do(t, "alice@example.com", "POST", TasksPath+"/t-proc-1/cancel", map[string]string{
		"reason": "stopping benchmark",
	})
	if code != http.StatusOK {
		t.Fatalf("POST cancel: %d %s", code, b)
	}
	select {
	case m := <-streamMsgs:
		if m.Type != TypeCancel || m.Task == nil || m.Task.ID != "t-proc-1" || m.NodeTask != "local-proc-42" || !strings.Contains(m.Reason, "stopping benchmark") {
			t.Fatalf("unexpected cancel stream message: %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancel message on node stream")
	}

	if len(terminals) != 1 || terminals[0].ID != "t-proc-1" || terminals[0].State != Cancelled {
		t.Fatalf("expected OnTerminal to fire once for cancelled run, got %+v", terminals)
	}
}

func TestRolesAndTeamSubmissionPolicies(t *testing.T) {
	ctx := context.Background()
	h := newAPIHub(t)
	const (
		alice = "alice@example.com" // infra admin
		dave  = "dave@example.com"  // infra member
		pete  = "pete@example.com"  // infra proposer
		vera  = "vera@example.com"  // infra viewer
		bob   = "bob@example.com"   // web member (outside infra)
	)
	check(t, h.dir.SetMember(ctx, "infra", "user:"+pete, directory.Proposer))
	check(t, h.dir.SetMember(ctx, "infra", "user:"+vera, directory.Viewer))

	// 1. Default policy (anyone_may_propose=true, require_approval=false):
	//    - member/admin submit -> queued
	//    - proposer/viewer/outsider submit -> proposed
	mTask := h.submit(t, dave, map[string]any{"repo": "bwsalmon/silo", "prompt": "member task"})
	if mTask.State != Queued {
		t.Fatalf("member task state = %s, want queued", mTask.State)
	}
	pTask := h.submit(t, pete, map[string]any{"repo": "bwsalmon/silo", "prompt": "proposer task"})
	if pTask.State != Proposed {
		t.Fatalf("proposer task state = %s, want proposed", pTask.State)
	}
	vTask := h.submit(t, vera, map[string]any{"repo": "bwsalmon/silo", "prompt": "viewer task"})
	if vTask.State != Proposed {
		t.Fatalf("viewer task state = %s, want proposed", vTask.State)
	}
	vTeamTask := h.submit(t, vera, map[string]any{"team": "infra", "prompt": "viewer team task"})
	if vTeamTask.State != Proposed {
		t.Fatalf("viewer team task state = %s, want proposed", vTeamTask.State)
	}

	// 2. Read visibility: viewer and proposer see infra's tasks.
	for _, u := range []string{pete, vera} {
		if ts := h.list(t, u, ""); len(ts) != 4 {
			t.Errorf("%s list count = %d, want 4", u, len(ts))
		}
		if code, _ := h.do(t, u, "GET", TasksPath+"/"+mTask.ID, nil); code != http.StatusOK {
			t.Errorf("%s GET %s: %d, want 200", u, mTask.ID, code)
		}
	}

	// 3. Approval & interactive session restrictions for viewer and proposer:
	//    - Neither viewer nor proposer can approve or reject proposals.
	for _, u := range []string{pete, vera} {
		if code, b := h.do(t, u, "POST", TasksPath+"/"+vTask.ID+"/approve", nil); code != http.StatusForbidden {
			t.Errorf("%s approving proposal: %d %s, want 403", u, code, b)
		}
		if code, b := h.do(t, u, "POST", TasksPath+"/"+vTask.ID+"/reject", map[string]string{"reason": "no"}); code != http.StatusForbidden {
			t.Errorf("%s rejecting proposal: %d %s, want 403", u, code, b)
		}
	}

	//    - Neither viewer nor proposer can post to interactive sessions they did not submit.
	sess := h.submit(t, alice, map[string]any{"repo": "bwsalmon/silo", "kind": "interactive", "prompt": "pair"})
	for _, u := range []string{pete, vera} {
		if code, b := h.do(t, u, "POST", TasksPath+"/"+sess.ID+"/messages", map[string]string{"text": "hi"}); code != http.StatusForbidden {
			t.Errorf("%s posting to alice's session: %d %s, want 403", u, code, b)
		}
	}
	//    - Submitter (proposer) CAN post to their own interactive session once approved.
	peteSess := h.submit(t, pete, map[string]any{"repo": "bwsalmon/silo", "kind": "interactive", "prompt": "pete session"})
	if code, b := h.do(t, dave, "POST", TasksPath+"/"+peteSess.ID+"/approve", nil); code != http.StatusOK {
		t.Fatalf("dave approving pete's session: %d %s", code, b)
	}
	if code, b := h.do(t, pete, "POST", TasksPath+"/"+peteSess.ID+"/messages", map[string]string{"text": "my own session"}); code != http.StatusCreated {
		t.Errorf("pete posting to own approved session: %d %s, want 201", code, b)
	}

	//    - Neither viewer nor proposer can answer in-run approval_requests.
	h.api.Requests = requests{
		mTask.ID: {
			"req-1": {ID: "req-1", Tool: "bash", Message: "rm -rf?"},
		},
	}
	for _, u := range []string{pete, vera} {
		if code, b := h.do(t, u, "POST", TasksPath+"/"+mTask.ID+"/approvals/req-1", map[string]string{"decision": "allow"}); code != http.StatusForbidden {
			t.Errorf("%s answering approval request: %d %s, want 403", u, code, b)
		}
	}

	// 4. Per-team policy: anyone_may_propose = false
	check(t, h.dir.SetTeamSettings(ctx, "infra", directory.Settings{AnyoneMayPropose: false, RequireApproval: false}))
	for _, u := range []string{bob, vera} {
		if code, b := h.do(t, u, "POST", TasksPath, map[string]any{"repo": "bwsalmon/silo", "prompt": "blocked"}); code != http.StatusForbidden {
			t.Errorf("%s submitting to repo when anyone_may_propose=false: %d %s, want 403", u, code, b)
		}
		if code, b := h.do(t, u, "POST", TasksPath, map[string]any{"team": "infra", "prompt": "blocked"}); code != http.StatusForbidden {
			t.Errorf("%s submitting to team when anyone_may_propose=false: %d %s, want 403", u, code, b)
		}
	}
	// Proposer is still permitted to submit as proposed when anyone_may_propose=false.
	pTask2 := h.submit(t, pete, map[string]any{"repo": "bwsalmon/silo", "prompt": "proposer allowed"})
	if pTask2.State != Proposed {
		t.Errorf("proposer task when anyone_may_propose=false: state = %s, want proposed", pTask2.State)
	}
	pTeamTask := h.submit(t, pete, map[string]any{"team": "infra", "prompt": "proposer team allowed"})
	if pTeamTask.State != Proposed {
		t.Errorf("proposer team task when anyone_may_propose=false: state = %s, want proposed", pTeamTask.State)
	}

	// 5. Per-team policy: require_approval = true (two-person review)
	check(t, h.dir.SetTeamSettings(ctx, "infra", directory.Settings{AnyoneMayPropose: false, RequireApproval: true}))
	adminTask := h.submit(t, alice, map[string]any{"repo": "bwsalmon/silo", "prompt": "needs two-person review"})
	if adminTask.State != Proposed || adminTask.ApprovedBy != "" {
		t.Fatalf("admin task when require_approval=true: %+v, want state=proposed and empty approved_by", adminTask)
	}
	// Alice cannot self-approve her own task.
	if code, b := h.do(t, alice, "POST", TasksPath+"/"+adminTask.ID+"/approve", nil); code != http.StatusForbidden {
		t.Errorf("alice self-approving: %d %s, want 403", code, b)
	}
	// Dave (another member of infra) can approve Alice's task, moving it to queued.
	code, b := h.do(t, dave, "POST", TasksPath+"/"+adminTask.ID+"/approve", nil)
	if code != http.StatusOK {
		t.Fatalf("dave approving alice's task: %d %s, want 200", code, b)
	}
	var approved Task
	check(t, json.Unmarshal(b, &approved))
	if approved.State != Queued || approved.ApprovedBy != dave {
		t.Errorf("approved task: %+v, want state=queued approved_by=%s", approved, dave)
	}
}

type fakeInboxEvents struct {
	awaiting      map[string]string
	awaitingTimes map[string]time.Time
	approvals     map[string][]events.ApprovalRequest
	approvalTimes map[string][]time.Time
}

func (f *fakeInboxEvents) ApprovalRequest(ctx context.Context, task, id string) (events.ApprovalRequest, bool, error) {
	for _, a := range f.approvals[task] {
		if a.ID == id {
			return a, true, nil
		}
	}
	return events.ApprovalRequest{}, false, nil
}

func (f *fakeInboxEvents) TaskInboxState(ctx context.Context, task string) (bool, string, time.Time, []events.ApprovalRequest, []time.Time, error) {
	q, ok := f.awaiting[task]
	return ok, q, f.awaitingTimes[task], f.approvals[task], f.approvalTimes[task], nil
}

func TestAPIUnifiedInbox(t *testing.T) {
	ctx := context.Background()
	h := newAPIHub(t)
	check(t, h.dir.SetMember(ctx, "infra", "user:pete@example.com", directory.Proposer))
	check(t, h.dir.SetMember(ctx, "web", "user:alice@example.com", directory.Member))

	reg := registry.New()
	h.api.Registry = reg
	h.api.Directory.Registry = reg
	h.api.NodeUIDomain = "nodes.grain.example"

	nodeInfra := registry.Node{
		ID:    "ws-infra",
		Repos: []string{"bwsalmon/silo"},
		Slots: 2,
		UI:    &registry.NodeUI{Offered: true, Team: "infra"},
	}
	leaseInfra, err := reg.Register(nodeInfra, "alice@example.com")
	check(t, err)
	defer leaseInfra.Close()

	nodeWeb := registry.Node{
		ID:    "ws-web",
		Repos: []string{"bwsalmon/web"},
		Slots: 2,
		UI:    &registry.NodeUI{Offered: true, Team: "web"},
	}
	leaseWeb, err := reg.Register(nodeWeb, "bob@example.com")
	check(t, err)
	defer leaseWeb.Close()

	// 1. Create a running interactive session awaiting a reply on ws-infra.
	tWait := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "kind": "interactive", "title": "Pair debug", "prompt": "debug auth"})
	_, err = h.store.Offer(ctx, tWait.ID, "ws-infra", "alice@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, tWait.ID, "local-1")
	check(t, err)

	// 2. Create a running task with an approval_request on ws-infra.
	tApp := h.submit(t, "alice@example.com", map[string]any{"repo": "bwsalmon/silo", "title": "Deploy prod", "prompt": "deploy"})
	_, err = h.store.Offer(ctx, tApp.ID, "ws-infra", "alice@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, tApp.ID, "local-2")
	check(t, err)

	// 3. Create a proposed task on infra by pete.
	tProp := h.submit(t, "pete@example.com", map[string]any{"repo": "bwsalmon/silo", "title": "Fix typo", "prompt": "fix typo in readme"})

	// 4. Create a failed task on ws-web.
	tFail := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/web", "title": "Web build", "prompt": "build web"})
	_, err = h.store.Offer(ctx, tFail.ID, "ws-web", "bob@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, tFail.ID, "local-3")
	check(t, err)
	_, err = h.store.Finish(ctx, tFail.ID, Failed, "lint failed")
	check(t, err)

	evStore := &fakeInboxEvents{
		awaiting:      map[string]string{tWait.ID: "Which environment should I target?"},
		awaitingTimes: map[string]time.Time{tWait.ID: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)},
		approvals: map[string][]events.ApprovalRequest{
			tApp.ID: {{ID: "req-1", Tool: "kubectl_apply", Message: "Apply prod manifest?"}},
		},
		approvalTimes: map[string][]time.Time{
			tApp.ID: {time.Date(2026, 10, 15, 12, 1, 0, 0, time.UTC)},
		},
	}
	h.api.Requests = evStore
	h.api.Events = evStore

	getInbox := func(user, query string) ([]InboxItem, []string) {
		t.Helper()
		code, b := h.do(t, user, "GET", InboxPath+query, nil)
		if code != http.StatusOK {
			t.Fatalf("%s GET %s%s: %d %s", user, InboxPath, query, code, b)
		}
		var out struct {
			Items []InboxItem `json:"items"`
			Nodes []string    `json:"nodes"`
		}
		check(t, json.Unmarshal(b, &out))
		return out.Items, out.Nodes
	}

	// Alice is owner of ws-infra and member of web (ws-web): default inbox includes both deployments and all 4 items.
	items, nodes := getInbox("alice@example.com", "")
	if len(nodes) != 2 || len(items) != 4 {
		t.Fatalf("alice default inbox: nodes=%v items=%+v; want 2 nodes and 4 items", nodes, items)
	}

	// Filter by ?nodes=ws-infra returns only the 3 ws-infra items with node_ui_url populated.
	infraItems, _ := getInbox("alice@example.com", "?nodes=ws-infra")
	if len(infraItems) != 3 {
		t.Fatalf("alice ?nodes=ws-infra items=%+v; want 3", infraItems)
	}
	for _, it := range infraItems {
		if it.NodeID != "ws-infra" || it.NodeUIURL != "https://ws-infra.nodes.grain.example" {
			t.Fatalf("unexpected node/url on item: %+v", it)
		}
	}

	// Filter by ?kind=proposal returns only the proposal item.
	propItems, _ := getInbox("alice@example.com", "?kind=proposal")
	if len(propItems) != 1 || propItems[0].TaskID != tProp.ID || !propItems[0].CanAct {
		t.Fatalf("alice ?kind=proposal = %+v", propItems)
	}

	// Inline quick actions clear each item from the inbox:
	// a) Reply to awaiting_reply session:
	if code, b := h.do(t, "alice@example.com", "POST", TasksPath+"/"+tWait.ID+"/messages", map[string]string{"id": "m-1", "text": "staging"}); code != http.StatusCreated {
		t.Fatalf("reply to awaiting session: %d %s", code, b)
	}
	// b) Answer approval_request:
	if code, b := h.do(t, "alice@example.com", "POST", TasksPath+"/"+tApp.ID+"/approvals/req-1", map[string]string{"decision": "allow"}); code != http.StatusOK {
		t.Fatalf("answer approval: %d %s", code, b)
	}
	// c) Approve proposal:
	if code, b := h.do(t, "alice@example.com", "POST", TasksPath+"/"+tProp.ID+"/approve", nil); code != http.StatusOK {
		t.Fatalf("approve proposal: %d %s", code, b)
	}
	// d) Retry failed task on ws-web as bob:
	if code, b := h.do(t, "bob@example.com", "POST", TasksPath+"/"+tFail.ID+"/requeue", nil); code != http.StatusOK {
		t.Fatalf("requeue failed task: %d %s", code, b)
	}

	remaining, _ := getInbox("alice@example.com", "")
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining inbox items after resolving all 4, got %+v", remaining)
	}
}

type concurrentInboxEvents struct {
	mu            sync.RWMutex
	awaiting      map[string]string
	awaitingTimes map[string]time.Time
	approvals     map[string][]events.ApprovalRequest
	approvalTimes map[string][]time.Time
}

func (c *concurrentInboxEvents) setAwaiting(task, q string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q == "" {
		delete(c.awaiting, task)
		delete(c.awaitingTimes, task)
		return
	}
	c.awaiting[task] = q
	c.awaitingTimes[task] = at
}

func (c *concurrentInboxEvents) addApproval(task string, ar events.ApprovalRequest, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approvals[task] = append(c.approvals[task], ar)
	c.approvalTimes[task] = append(c.approvalTimes[task], at)
}

func (c *concurrentInboxEvents) ApprovalRequest(_ context.Context, task, id string) (events.ApprovalRequest, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.approvals[task] {
		if a.ID == id {
			return a, true, nil
		}
	}
	return events.ApprovalRequest{}, false, nil
}

func (c *concurrentInboxEvents) TaskInboxState(_ context.Context, task string) (awaiting bool, question string, awaitingAt time.Time, approvals []events.ApprovalRequest, approvalTimes []time.Time, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.awaiting[task]
	apps := append([]events.ApprovalRequest(nil), c.approvals[task]...)
	times := append([]time.Time(nil), c.approvalTimes[task]...)
	return ok, q, c.awaitingTimes[task], apps, times, nil
}

func TestConcurrentUnifiedInboxStressAndSpoofing(t *testing.T) {
	ctx := context.Background()
	h := newAPIHub(t)
	check(t, h.dir.SetMember(ctx, "infra", "user:pete@example.com", directory.Proposer))
	check(t, h.dir.SetMember(ctx, "infra", "user:vera@example.com", directory.Viewer))
	check(t, h.dir.SetMember(ctx, "web", "user:alice@example.com", directory.Member))
	check(t, h.dir.CreateTeam(ctx, "secret", "Secret"))
	check(t, h.dir.SetMember(ctx, "secret", "user:sec@example.com", directory.Admin))
	check(t, h.dir.AssignRepo(ctx, "secret", "bwsalmon/secret"))
	check(t, h.dir.SetTeamSettings(ctx, "secret", directory.Settings{AnyoneMayPropose: false}))

	reg := registry.New()
	h.api.Registry = reg
	h.api.Directory.Registry = reg
	h.api.NodeUIDomain = "nodes.grain.example"

	for _, n := range []struct {
		node  registry.Node
		owner string
	}{
		{
			node: registry.Node{
				ID:       "ws-infra-1",
				Repos:    []string{"bwsalmon/silo"},
				Slots:    4,
				Features: []string{registry.FeatureInteractive, registry.FeatureNodeUI},
				UI:       &registry.NodeUI{Offered: true, Team: "infra"},
			},
			owner: "alice@example.com",
		},
		{
			node: registry.Node{
				ID:       "ws-infra-2",
				Repos:    []string{"bwsalmon/silo"},
				Slots:    4,
				Features: []string{registry.FeatureInteractive, registry.FeatureNodeUI},
				UI:       &registry.NodeUI{Offered: true},
			},
			owner: "dave@example.com",
		},
		{
			node: registry.Node{
				ID:       "ws-web-1",
				Repos:    []string{"bwsalmon/web"},
				Slots:    4,
				Features: []string{registry.FeatureInteractive, registry.FeatureNodeUI},
				UI:       &registry.NodeUI{Offered: true, Team: "web"},
			},
			owner: "bob@example.com",
		},
		{
			node: registry.Node{
				ID:    "ws-secret-1",
				Repos: []string{"bwsalmon/secret"},
				Slots: 2,
				UI:    &registry.NodeUI{Offered: true, Team: "secret"},
			},
			owner: "sec@example.com",
		},
	} {
		lease, err := reg.Register(n.node, n.owner)
		check(t, err)
		t.Cleanup(func() { lease.Close() })
	}

	evStore := &concurrentInboxEvents{
		awaiting:      map[string]string{},
		awaitingTimes: map[string]time.Time{},
		approvals:     map[string][]events.ApprovalRequest{},
		approvalTimes: map[string][]time.Time{},
	}
	h.api.Requests = evStore
	h.api.Events = evStore

	// Seed a secret task on team "secret" that non-secret users must never see in their inbox.
	secretTask := h.submit(t, "sec@example.com", map[string]any{"repo": "bwsalmon/secret", "title": "Secret op", "prompt": "top secret"})
	_, err := h.store.Offer(ctx, secretTask.ID, "ws-secret-1", "sec@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, secretTask.ID, "local-sec-1")
	check(t, err)
	evStore.addApproval(secretTask.ID, events.ApprovalRequest{ID: "sec-req", Tool: "vault", Message: "unseal"}, time.Now().UTC())

	doSpoofed := func(user, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, h.url+path, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Origin", h.url)
		if user != "" {
			req.Header.Set("X-User", user)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// Attach spoofed nodeui & admin-mode headers.
		req.Header.Set("X-Grain-Hub-UI-Viewer", "root@example.com")
		req.Header.Set("X-Grain-Hub-UI-User", "root@example.com")
		req.Header.Set("X-Grain-Hub-UI-Role", "owner")
		req.Header.Set("X-Grain-Hub-UI-Theme", "spoofed")
		req.Header.Set(directory.AdminModeHeader, "on")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, b, err
	}

	const iterations = 10
	errCh := make(chan error, 256)
	recordErr := func(err error) {
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}

	var wg sync.WaitGroup

	// 1. Concurrent task lifecycle writer: proposed -> queued -> running (awaiting_reply & approval_request) -> lost -> requeued -> failed / done.
	for wIdx := 0; wIdx < 2; wIdx++ {
		wIdx := wIdx
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// File a proposal as pete (proposer on infra), approve as dave, run on ws-infra-1 or ws-infra-2, transition states.
				code, b, err := doSpoofed("pete@example.com", "POST", TasksPath, map[string]any{
					"repo":   "bwsalmon/silo",
					"kind":   "interactive",
					"title":  fmt.Sprintf("Lifecycle-%d-%d", wIdx, i),
					"prompt": "pair",
				})
				if err != nil {
					recordErr(err)
					return
				}
				if code != http.StatusCreated {
					recordErr(fmt.Errorf("pete submit proposal: %d %s", code, b))
					return
				}
				var tk Task
				if err := json.Unmarshal(b, &tk); err != nil {
					recordErr(err)
					return
				}
				if tk.State != Proposed {
					recordErr(fmt.Errorf("pete task state = %s, want proposed", tk.State))
					return
				}

				// Submitter (pete) and viewer (vera) must not be allowed to approve the proposal even with spoofed headers.
				for _, deniedUser := range []string{"pete@example.com", "vera@example.com", "bob@example.com"} {
					acode, _, err := doSpoofed(deniedUser, "POST", TasksPath+"/"+tk.ID+"/approve", nil)
					if err != nil {
						recordErr(err)
						return
					}
					if acode != http.StatusForbidden && acode != http.StatusNotFound {
						recordErr(fmt.Errorf("%s approve proposal: got %d, want 403/404", deniedUser, acode))
						return
					}
				}

				// Approve as dave (member of infra).
				acode, ab, err := doSpoofed("dave@example.com", "POST", TasksPath+"/"+tk.ID+"/approve", nil)
				if err != nil {
					recordErr(err)
					return
				}
				if acode != http.StatusOK {
					recordErr(fmt.Errorf("dave approve: %d %s", acode, ab))
					return
				}

				nodeID := "ws-infra-1"
				nodeOwner := "alice@example.com"
				if i%2 == 1 {
					nodeID = "ws-infra-2"
					nodeOwner = "dave@example.com"
				}
				if _, err := h.store.Offer(ctx, tk.ID, nodeID, nodeOwner); err != nil {
					recordErr(fmt.Errorf("Offer: %w", err))
					return
				}
				if _, err := h.store.Accept(ctx, tk.ID, fmt.Sprintf("local-%d-%d", wIdx, i)); err != nil {
					recordErr(fmt.Errorf("Accept: %w", err))
					return
				}

				now := h.store.Now()
				evStore.setAwaiting(tk.ID, "Ready to proceed?", now)
				reqID := fmt.Sprintf("req-%d-%d", wIdx, i)
				evStore.addApproval(tk.ID, events.ApprovalRequest{ID: reqID, Tool: "bash", Message: "run build"}, now)

				// Post a reply from alice and answer the approval request from dave.
				mcode, mb, err := doSpoofed("alice@example.com", "POST", TasksPath+"/"+tk.ID+"/messages", map[string]any{
					"id":   fmt.Sprintf("msg-%d-%d", wIdx, i),
					"text": "proceed",
				})
				if err != nil {
					recordErr(err)
					return
				}
				if mcode != http.StatusCreated && mcode != http.StatusOK {
					recordErr(fmt.Errorf("post message: %d %s", mcode, mb))
					return
				}
				evStore.setAwaiting(tk.ID, "", time.Time{})

				apCode, apB, err := doSpoofed("dave@example.com", "POST", TasksPath+"/"+tk.ID+"/approvals/"+reqID, map[string]any{
					"decision": "allow",
				})
				if err != nil {
					recordErr(err)
					return
				}
				if apCode != http.StatusOK {
					recordErr(fmt.Errorf("answer approval: %d %s", apCode, apB))
					return
				}

				// Transition through lost -> retry -> failed or done.
				if _, err := h.store.Lose(ctx, tk.ID, "node connection lost"); err != nil {
					recordErr(fmt.Errorf("Lose: %w", err))
					return
				}
				if _, err := h.store.Retry(ctx, tk.ID); err != nil {
					recordErr(fmt.Errorf("Retry: %w", err))
					return
				}
				if _, err := h.store.Offer(ctx, tk.ID, nodeID, nodeOwner); err != nil {
					recordErr(fmt.Errorf("Re-Offer: %w", err))
					return
				}
				if _, err := h.store.Accept(ctx, tk.ID, fmt.Sprintf("local-r-%d-%d", wIdx, i)); err != nil {
					recordErr(fmt.Errorf("Re-Accept: %w", err))
					return
				}
				endState := Done
				reason := ""
				if i%2 == 0 {
					endState = Failed
					reason = "transient failure"
				}
				if _, err := h.store.Finish(ctx, tk.ID, endState, reason); err != nil {
					recordErr(fmt.Errorf("Finish: %w", err))
					return
				}
			}
		}()
	}

	// 2. Concurrent preference mutator: rapidly update PUT /v1/me/inbox and PUT /v1/me/theme for alice and dave.
	wg.Add(1)
	go func() {
		defer wg.Done()
		nodeSelections := [][]string{
			{"ws-infra-1", "ws-infra-2", "ws-web-1"},
			{"ws-infra-1"},
			{"ws-infra-1", "ws-web-1"},
			{},
			{"ws-infra-1", "ws-infra-2"},
		}
		for i := 0; i < iterations*2; i++ {
			sel := nodeSelections[i%len(nodeSelections)]
			code, b, err := doSpoofed("alice@example.com", "PUT", "/v1/me/inbox", map[string]any{"nodes": sel})
			if err != nil {
				recordErr(err)
				return
			}
			if code != http.StatusOK {
				recordErr(fmt.Errorf("alice PUT /v1/me/inbox: %d %s", code, b))
				return
			}
			theme := "light"
			if i%2 == 1 {
				theme = "dark"
			}
			tcode, tb, err := doSpoofed("alice@example.com", "PUT", "/v1/me/theme", map[string]any{"theme": theme})
			if err != nil {
				recordErr(err)
				return
			}
			if tcode != http.StatusOK {
				recordErr(fmt.Errorf("alice PUT /v1/me/theme: %d %s", tcode, tb))
				return
			}

			// Verify alice cannot select a node she has no access to (ws-secret-1).
			scode, _, err := doSpoofed("alice@example.com", "PUT", "/v1/me/inbox", map[string]any{"nodes": []string{"ws-secret-1"}})
			if err != nil {
				recordErr(err)
				return
			}
			if scode != http.StatusForbidden {
				recordErr(fmt.Errorf("alice PUT /v1/me/inbox ws-secret-1: got %d, want 403", scode))
				return
			}
		}
	}()

	// 3. Concurrent GET /v1/inbox and GET /v1/me readers across all roles.
	queries := []string{
		"",
		"?nodes=ws-infra-1",
		"?nodes=ws-infra-1,ws-infra-2",
		"?kind=proposal",
		"?kind=awaiting_reply,approval_request",
		"?kind=failed",
	}
	readers := []string{"alice@example.com", "dave@example.com", "pete@example.com", "vera@example.com", "bob@example.com"}
	for _, rUser := range readers {
		rUser := rUser
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations*2; i++ {
				q := queries[i%len(queries)]
				code, b, err := doSpoofed(rUser, "GET", InboxPath+q, nil)
				if err != nil {
					recordErr(err)
					return
				}
				if code != http.StatusOK {
					recordErr(fmt.Errorf("%s GET %s%s: %d %s", rUser, InboxPath, q, code, b))
					return
				}
				var out struct {
					Items []InboxItem `json:"items"`
					Nodes []string    `json:"nodes"`
				}
				if err := json.Unmarshal(b, &out); err != nil {
					recordErr(fmt.Errorf("%s inbox json: %w", rUser, err))
					return
				}
				for _, item := range out.Items {
					// None of these non-secret readers may ever see team "secret" items despite X-Silo-Admin-Mode: on.
					if item.Team == "secret" || item.TaskID == secretTask.ID {
						recordErr(fmt.Errorf("%s leaked secret inbox item: %+v", rUser, item))
						return
					}
					if item.State == Done || item.State == Cancelled || item.State == Rejected {
						recordErr(fmt.Errorf("%s inbox returned terminal item: %+v", rUser, item))
						return
					}
					if rUser == "vera@example.com" && item.CanAct {
						recordErr(fmt.Errorf("vera (viewer) got CanAct=true on item: %+v", item))
						return
					}
					if rUser == "pete@example.com" && (item.Kind == InboxProposal || item.Kind == InboxApprovalRequest) && item.CanAct {
						recordErr(fmt.Errorf("pete (proposer/submitter) got CanAct=true on %s item: %+v", item.Kind, item))
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
