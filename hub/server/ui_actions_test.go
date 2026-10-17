package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebUIActions sends each write the web UI makes (ui/src/api.ts,
// docs/ui.md §6 U2) to a whole hub, as the browser sends it: same
// origin, the caller's own sign-on (here the dev user header, as IAP's
// cookie behind a real sign-on) and no other credential, JSON for a
// body. Each one reaches its endpoint and is allowed or refused there by
// the caller's role, so the UI's hiding of a button is never what keeps
// a caller out; a write from another origin, or signed out, never gets
// that far.
func TestWebUIActions(t *testing.T) {
	const root, alice, bob, carol = "root@example.com", "alice@example.com", "bob@example.com", "carol@example.com"
	h, err := New(context.Background(), Config{
		Database:      "sqlite::memory:",
		DevUserHeader: userHeader,
		HubAdmins:     []string{"user:" + root},
		Logf:          logf(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// send is the UI's fetch: Sec-Fetch-Site and Origin as a browser sets
	// them on a same-origin fetch (site overrides the first, "" drops
	// both, as a non-browser client would).
	send := func(user, site, method, path string, body any) (int, []byte, http.Header) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
			origin := srv.URL
			if site != "same-origin" {
				origin = "https://elsewhere.example"
			}
			req.Header.Set("Origin", origin)
		}
		if user != "" {
			req.Header.Set(userHeader, user)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	ui := func(want int, user, method, path string, body any) []byte {
		t.Helper()
		got, b, _ := send(user, "same-origin", method, path, body)
		if got != want {
			t.Fatalf("%s %s as %q: %d %s, want %d", method, path, user, got, strings.TrimSpace(string(b)), want)
		}
		return b
	}
	// refused checks a write never reaches its endpoint signed out (401),
	// or from another origin even signed in (403).
	refused := func(method, path string, body any) {
		t.Helper()
		if got, b, _ := send("", "same-origin", method, path, body); got != http.StatusUnauthorized {
			t.Errorf("%s %s signed out: %d %s, want 401", method, path, got, b)
		}
		if got, b, _ := send(root, "cross-site", method, path, body); got != http.StatusForbidden {
			t.Errorf("%s %s cross-site: %d %s, want 403", method, path, got, b)
		}
		if got, b, _ := send(root, "same-site", method, path, body); got != http.StatusForbidden {
			t.Errorf("%s %s same-site: %d %s, want 403", method, path, got, b)
		}
	}

	// Teams: only a hub admin creates one (the Teams page's form).
	refused("POST", "/v1/teams", map[string]string{"id": "infra", "name": "Infra"})
	ui(403, alice, "POST", "/v1/teams", map[string]string{"id": "infra", "name": "Infra"})
	ui(201, root, "POST", "/v1/teams", map[string]string{"id": "infra", "name": "Infra"})
	ui(409, root, "POST", "/v1/teams", map[string]string{"id": "infra", "name": "Infra"})

	// Members and roles: a team admin's, or a hub admin's.
	alicePath := "/v1/teams/infra/members/user%3Aalice%40example.com"
	bobPath := "/v1/teams/infra/members/user%3Abob%40example.com"
	refused("PUT", alicePath, map[string]string{"role": "admin"})
	ui(404, alice, "PUT", alicePath, map[string]string{"role": "admin"})
	ui(204, root, "PUT", alicePath, map[string]string{"role": "admin"})
	ui(204, alice, "PUT", bobPath, map[string]string{"role": "admin"})
	ui(204, alice, "PUT", bobPath, map[string]string{"role": "member"})
	ui(403, bob, "PUT", "/v1/teams/infra/members/user%3Acarol%40example.com", map[string]string{"role": "member"})

	// Repositories: a hub admin assigns, with or without history; a team
	// admin releases.
	refused("PUT", "/v1/teams/infra/repos/acme/app", nil)
	ui(403, alice, "PUT", "/v1/teams/infra/repos/acme/app", nil)
	ui(204, root, "PUT", "/v1/teams/infra/repos/acme/app", nil)
	ui(204, root, "PUT", "/v1/teams/infra/repos/acme/lib", map[string]bool{"history": true})
	ui(403, bob, "DELETE", "/v1/teams/infra/repos/acme/lib", nil)
	refused("DELETE", "/v1/teams/infra/repos/acme/lib", nil)
	ui(204, alice, "DELETE", "/v1/teams/infra/repos/acme/lib", nil)

	// The team page's read, and its settings.
	var team struct {
		Members map[string]string
		Repos   []string
	}
	json.Unmarshal(ui(200, bob, "GET", "/v1/teams/infra", nil), &team)
	if team.Members["user:"+alice] != "admin" || team.Members["user:"+bob] != "member" || len(team.Repos) != 1 || team.Repos[0] != "acme/app" {
		t.Errorf("infra as bob: %+v", team)
	}
	ui(404, carol, "GET", "/v1/teams/infra", nil)
	ui(200, bob, "GET", "/v1/teams/infra/settings", nil)
	refused("PUT", "/v1/teams/infra/settings", map[string]int{"lost_requeue_after_seconds": 600})
	ui(403, bob, "PUT", "/v1/teams/infra/settings", map[string]int{"lost_requeue_after_seconds": 600})
	ui(200, alice, "PUT", "/v1/teams/infra/settings", map[string]int{"lost_requeue_after_seconds": 600})

	// Filing: to a repository a team of the caller's owns, or to the team.
	file := func(want int, user string, body map[string]any) string {
		t.Helper()
		var task struct{ ID, Team string }
		json.Unmarshal(ui(want, user, "POST", "/v1/tasks", body), &task)
		if want == http.StatusCreated && (task.ID == "" || task.Team != "infra") {
			t.Fatalf("filed %+v", task)
		}
		return task.ID
	}
	refused("POST", "/v1/tasks", map[string]any{"repo": "acme/app", "prompt": "x"})
	byBob := file(201, bob, map[string]any{"repo": "acme/app", "title": "Fix", "prompt": "Fix it."})
	byAlice := file(201, alice, map[string]any{"team": "infra", "title": "Any", "prompt": "Anything.", "constraints": map[string]any{"node": "ws-1"}})
	file(404, carol, map[string]any{"team": "infra", "prompt": "x"})

	// Proposals: carol, outside infra, proposes in its repository; a
	// member of infra other than her approves or rejects, once.
	byCarol := file(201, carol, map[string]any{"repo": "acme/app", "title": "Idea", "prompt": "An idea."})
	other := file(201, carol, map[string]any{"repo": "acme/app", "title": "Other", "prompt": "Another."})
	if b := ui(200, carol, "GET", "/v1/tasks/"+byCarol, nil); !strings.Contains(string(b), `"state":"proposed"`) {
		t.Errorf("carol's proposal: %s", b)
	}
	refused("POST", "/v1/tasks/"+byCarol+"/approve", nil)
	refused("POST", "/v1/tasks/"+byCarol+"/reject", map[string]string{"reason": "x"})
	ui(403, carol, "POST", "/v1/tasks/"+byCarol+"/approve", nil)
	ui(403, root, "POST", "/v1/tasks/"+byCarol+"/approve", nil)
	if b := ui(200, bob, "POST", "/v1/tasks/"+byCarol+"/approve", nil); !strings.Contains(string(b), `"approved_by":"bob@example.com"`) {
		t.Errorf("approve: %s", b)
	}
	ui(409, alice, "POST", "/v1/tasks/"+byCarol+"/approve", nil)
	ui(409, alice, "POST", "/v1/tasks/"+byCarol+"/reject", nil)
	if b := ui(200, alice, "POST", "/v1/tasks/"+other+"/reject", map[string]string{"reason": "not now"}); !strings.Contains(string(b), "rejected by alice@example.com: not now") {
		t.Errorf("reject: %s", b)
	}
	// Answering an approval request: infra's members only, and only one
	// the task's node sent.
	refused("POST", "/v1/tasks/"+byBob+"/approvals/r-1", map[string]string{"decision": "allow"})
	ui(404, carol, "POST", "/v1/tasks/"+byBob+"/approvals/r-1", map[string]string{"decision": "allow"})
	ui(404, bob, "POST", "/v1/tasks/"+byBob+"/approvals/r-1", map[string]string{"decision": "allow"})
	ui(200, bob, "GET", "/v1/tasks/"+byBob+"/approvals", nil)

	// Sessions: starting one, and replying, its submitter or infra's
	// members; it is queued, so the message is kept for its node.
	session := file(201, bob, map[string]any{"team": "infra", "kind": "interactive", "prompt": "Pair with me."})
	refused("POST", "/v1/tasks/"+session+"/messages", map[string]string{"text": "hi"})
	ui(404, carol, "POST", "/v1/tasks/"+session+"/messages", map[string]string{"text": "hi"})
	ui(403, root, "POST", "/v1/tasks/"+session+"/messages", map[string]string{"text": "hi"})
	ui(201, alice, "POST", "/v1/tasks/"+session+"/messages", map[string]string{"id": "m-1", "text": "hi"})
	ui(200, alice, "POST", "/v1/tasks/"+session+"/messages", map[string]string{"id": "m-1", "text": "hi"})
	ui(409, bob, "POST", "/v1/tasks/"+byBob+"/messages", map[string]string{"text": "hi"})
	if b := ui(200, bob, "GET", "/v1/tasks/"+session+"/messages", nil); !strings.Contains(string(b), `"sender":"alice@example.com"`) {
		t.Errorf("messages: %s", b)
	}

	// Cancel and requeue: the submitter, a team admin or a hub admin.
	refused("POST", "/v1/tasks/"+byBob+"/cancel", map[string]string{"reason": "x"})
	refused("POST", "/v1/tasks/"+byBob+"/requeue", nil)
	ui(404, carol, "POST", "/v1/tasks/"+byBob+"/cancel", map[string]string{"reason": "x"})
	ui(403, bob, "POST", "/v1/tasks/"+byAlice+"/cancel", map[string]string{"reason": "x"})
	ui(403, bob, "POST", "/v1/tasks/"+byAlice+"/requeue", nil)
	ui(200, bob, "POST", "/v1/tasks/"+byBob+"/requeue", nil)
	if b := ui(200, bob, "POST", "/v1/tasks/"+byBob+"/cancel", map[string]string{"reason": "not needed"}); !strings.Contains(string(b), "cancelled by bob@example.com: not needed") {
		t.Errorf("cancel: %s", b)
	}
	ui(409, bob, "POST", "/v1/tasks/"+byBob+"/cancel", nil)
	ui(409, alice, "POST", "/v1/tasks/"+byBob+"/requeue", nil)
	ui(200, alice, "POST", "/v1/tasks/"+byAlice+"/cancel", nil)

	// Node tokens: minted for the caller, uncached; revoked by their
	// owner or a hub admin.
	refused("POST", "/v1/node-tokens", map[string]string{"node": "ws-1"})
	code, b, hdr := send(bob, "same-origin", "POST", "/v1/node-tokens", map[string]string{"node": "ws-1"})
	var minted struct {
		Token     string
		NodeToken struct{ ID, Owner, Node string } `json:"node_token"`
	}
	json.Unmarshal(b, &minted)
	if code != http.StatusCreated || !strings.HasPrefix(minted.Token, "silo-node_") || minted.NodeToken.Owner != bob || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("mint: %d %s, Cache-Control %q", code, b, hdr.Get("Cache-Control"))
	}
	ui(409, carol, "POST", "/v1/node-tokens", map[string]string{"node": "ws-1"})
	if b := ui(200, bob, "GET", "/v1/node-tokens", nil); strings.Contains(string(b), minted.Token) {
		t.Error("the token list shows the token")
	}
	tokPath := "/v1/node-tokens/" + minted.NodeToken.ID
	refused("DELETE", tokPath, nil)
	ui(404, carol, "DELETE", tokPath, nil)
	ui(204, bob, "DELETE", tokPath, nil)

	// Members out, and the team gone: a hub admin's.
	refused("DELETE", bobPath, nil)
	ui(204, alice, "DELETE", bobPath, nil)
	refused("DELETE", "/v1/teams/infra", nil)
	ui(403, alice, "DELETE", "/v1/teams/infra", nil)
	ui(204, root, "DELETE", "/v1/teams/infra", nil)
	ui(404, alice, "GET", "/v1/teams/infra", nil)
}
