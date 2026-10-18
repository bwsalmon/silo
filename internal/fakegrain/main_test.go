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
