package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
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
	m.Handle(TasksPath, api.Handler())
	m.Handle(TasksPath+"/", api.Handler())
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

	// infra releases it, for a transfer to web.
	want(http.StatusNoContent, "alice@example.com", "DELETE", at("infra"), nil)
	// Mallory, an admin of an unrelated team, cannot claim it, nor can
	// infra's or web's own admins.
	want(http.StatusForbidden, "mallory@example.com", "PUT", at("squat"), nil)
	want(http.StatusForbidden, "mallory@example.com", "PUT", at("squat"), map[string]bool{"history": true})
	want(http.StatusForbidden, "alice@example.com", "PUT", at("infra"), nil)
	if owner, _ := h.dir.RepoOwner(ctx, "bwsalmon/silo"); owner != "" {
		t.Fatalf("bwsalmon/silo was claimed by %s", owner)
	}
	if sees("mallory@example.com", old) {
		t.Errorf("mallory sees infra's task")
	}

	// The hub admin assigns it to web: web files tasks in it from now
	// on, but infra's tasks stay infra's.
	want(http.StatusNoContent, "root@example.com", "PUT", at("web"), nil)
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
