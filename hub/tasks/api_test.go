package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	store *Store
	url   string
}

// newAPIHub serves the backlog's API over the harness's directory:
// alice admins infra (bwsalmon/silo, bwsalmon/grain), dave is a member
// of it, and bob is in web (bwsalmon/web).
func newAPIHub(t *testing.T) *apiHub {
	ctx := context.Background()
	dir, err := directory.Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { dir.Close() })
	check(t, dir.CreateTeam(ctx, "infra", ""))
	check(t, dir.CreateTeam(ctx, "web", ""))
	check(t, dir.SetMember(ctx, "infra", "alice@example.com", directory.Admin))
	check(t, dir.SetMember(ctx, "infra", "dave@example.com", directory.Member))
	check(t, dir.SetMember(ctx, "web", "bob@example.com", directory.Member))
	check(t, dir.AssignRepo(ctx, "infra", "bwsalmon/silo"))
	check(t, dir.AssignRepo(ctx, "web", "bwsalmon/web"))
	store, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { store.Close() })
	store.Now = ticking()
	api := &API{Store: store, Directory: &directory.API{Store: dir, Auth: headerAuth{}, HubAdmins: []string{"root@example.com"}}}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &apiHub{store: store, url: ts.URL}
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
		{"bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "x"}, http.StatusNotFound},
		{"bob@example.com", map[string]any{"team": "infra", "prompt": "x"}, http.StatusNotFound},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/unowned", "prompt": "x"}, http.StatusNotFound},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo", "team": "infra", "prompt": "x"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"prompt": "x"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo"}, http.StatusBadRequest},
		{"alice@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "x", "submitter": "bob@example.com"}, http.StatusBadRequest},
		{"root@example.com", map[string]any{"team": "nope", "prompt": "x"}, http.StatusNotFound},
	} {
		if code, b := h.do(t, c.user, "POST", TasksPath, c.body); code != c.want {
			t.Errorf("%s submitting %v: %d %s, want %d", c.user, c.body, code, b, c.want)
		}
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
	_, err := h.store.Offer(ctx, byAlice.ID, "ws-1")
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
	code, b = h.do(t, "alice@example.com", "POST", path+"/cancel", map[string]string{"reason": "changed my mind"})
	check(t, json.Unmarshal(b, &got))
	if code != http.StatusOK || got.State != Cancelled || got.Reason != "cancelled by alice@example.com: changed my mind" {
		t.Errorf("cancelled: %d %+v", code, got)
	}
}
