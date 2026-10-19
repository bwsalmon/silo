package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/tasks"
)

func TestFakegrainUIAndU5TaskLinks(t *testing.T) {
	g := &grain{
		uiTeam:   "infra",
		uiAccess: "read-only",
		tasks: map[string]*task{
			"t-100": {
				ID:        "t-100",
				NodeTask:  "local-1",
				HubTaskID: "t-100",
				HubURL:    hubTaskURL("ws://hub.example.com/v1/nodes/rendezvous", "t-100"),
				Repo:      "bwsalmon/silo",
				Kind:      "interactive",
				State:     tasks.Running,
			},
		},
		order: []string{"t-100"},
	}

	h := g.uiHandler(nodeui.Message{
		Viewer: "alice@example.com",
		Role:   nodeui.RoleTeamAdmin,
		Team:   "infra",
	})

	// 1. U5: GET /api/tasks/t-100 returns hub_task_id and hub_url.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/t-100", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks/t-100: %d %s", rec.Code, rec.Body.String())
	}
	var got task
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HubTaskID != "t-100" || got.HubURL != "http://hub.example.com/tasks/t-100" {
		t.Errorf("U5 task response: %+v", got)
	}

	// 2. G3-G4: team-admin with read-only access can HEAD, but POST/PUT/DELETE/PATCH get 403 Forbidden.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/tasks", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD /api/tasks: %d, want 200", rec.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/action", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team-admin %s with read-only access: %d, want 403", method, rec.Code)
		}
	}

	// 3. G4: Leaked X-Goog-Authenticated-User-* header or Hub session cookie is rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:alice@example.com")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("expected leaked X-Goog-Authenticated-User-Email header to be rejected")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: "leaked"})
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("expected leaked Hub session cookie to be rejected")
	}

	// 4. G3: When uiAccess == "read-write", team-admin is allowed POST.
	g.uiAccess = "read-write"
	rwHandler := g.uiHandler(nodeui.Message{
		Viewer: "alice@example.com",
		Role:   nodeui.RoleTeamAdmin,
		Team:   "infra",
	})
	rec = httptest.NewRecorder()
	rwHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/action", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("team-admin POST with read-write access: %d, want 200", rec.Code)
	}
}

func TestFakegrainFiveRolePermissionsAndTheme(t *testing.T) {
	g := &grain{
		uiTeam: "infra",
		tasks:  map[string]*task{},
		order:  []string{},
	}

	// 1. Proposer: GET /api/me exposes theme="dark", role="proposer", require_approval=true;
	// POST /api/tasks creates a task in state "proposed"; other POST routes return 403.
	propH := g.uiHandler(nodeui.Message{
		Viewer:          "pete@example.com",
		Role:            nodeui.RoleProposer,
		Team:            "infra",
		RequireApproval: true,
		Theme:           "dark",
	})
	rec := httptest.NewRecorder()
	propH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proposer GET /api/me: %d %s", rec.Code, rec.Body.String())
	}
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["theme"] != "dark" || me["role"] != "proposer" || me["require_approval"] != true {
		t.Fatalf("unexpected /api/me = %+v", me)
	}

	rec = httptest.NewRecorder()
	propH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("proposer POST /api/tasks: %d %s", rec.Code, rec.Body.String())
	}
	var propTask task
	if err := json.Unmarshal(rec.Body.Bytes(), &propTask); err != nil {
		t.Fatal(err)
	}
	if propTask.State != tasks.Proposed || propTask.Proposer != "pete@example.com" {
		t.Fatalf("proposer created task = %+v, want state=proposed proposer=pete@example.com", propTask)
	}

	rec = httptest.NewRecorder()
	propH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+propTask.ID+"/approve", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("proposer POST approve: %d, want 403", rec.Code)
	}

	// 2. Member: can file tasks (lands in proposed when RequireApproval=true), cannot self-approve,
	// can approve another user's proposed task, and gets 403 on /api/settings.
	memH := g.uiHandler(nodeui.Message{
		Viewer:          "dave@example.com",
		Role:            nodeui.RoleMember,
		Team:            "infra",
		RequireApproval: true,
		Theme:           "light",
	})
	rec = httptest.NewRecorder()
	memH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST /api/tasks: %d %s", rec.Code, rec.Body.String())
	}
	var memTask task
	if err := json.Unmarshal(rec.Body.Bytes(), &memTask); err != nil {
		t.Fatal(err)
	}
	if memTask.State != tasks.Proposed {
		t.Fatalf("member task with RequireApproval=true: state=%s, want proposed", memTask.State)
	}
	// Self-approval blocked:
	rec = httptest.NewRecorder()
	memH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+memTask.ID+"/approve", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member self-approval: %d, want 403", rec.Code)
	}
	// Approving pete's proposal succeeds:
	rec = httptest.NewRecorder()
	memH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+propTask.ID+"/approve", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("member approving pete's proposal: %d %s", rec.Code, rec.Body.String())
	}
	// Settings mutation blocked for member:
	rec = httptest.NewRecorder()
	memH.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member PUT /api/settings: %d, want 403", rec.Code)
	}

	// 3. Viewer: strictly read-only (POST /api/tasks returns 403).
	viewH := g.uiHandler(nodeui.Message{
		Viewer: "vera@example.com",
		Role:   nodeui.RoleViewer,
		Team:   "infra",
		Theme:  "dark",
	})
	rec = httptest.NewRecorder()
	viewH.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST /api/tasks: %d, want 403", rec.Code)
	}
}
