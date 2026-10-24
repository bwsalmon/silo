package gateway

import (
	"os"
	"strings"
	"testing"
)

func TestPages(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid"})
	var pg Page
	e.mustCall(strangerCred, "page.register", with(kid, map[string]any{"slug": "app", "title": "My app", "port": 3000, "expose_port": 8443}), &pg)
	if pg.Owner != "kid" || pg.Host != "127.0.0.1" || pg.Port != 3000 || pg.ExposePort != 8443 || !pg.Active {
		t.Fatalf("page %+v", pg)
	}
	var ext Page
	e.mustCall(strangerCred, "page.register", with(main, map[string]any{"slug": "docs", "title": "Docs", "url": "https://example.com/docs"}), &ext)
	if pg = ext; pg.URL != "https://example.com/docs" || pg.Port != 0 {
		t.Fatalf("url page %+v", pg)
	}
	if ev := e.lastEvent(EvPageRegister); ev["slug"] != "docs" || ev["_actor"] != "agent:main" {
		t.Fatalf("audit %v", ev)
	}
	for _, bad := range []map[string]any{
		{"slug": "Bad", "title": "t", "port": 1},
		{"slug": "api", "title": "t", "port": 1},
		{"slug": "x", "title": "", "port": 1},
		{"slug": "x", "title": "t"},
		{"slug": "x", "title": "t", "port": 70000},
		{"slug": "x", "title": "t", "port": 80, "url": "https://a"},
		{"slug": "x", "title": "t", "url": "javascript:alert(1)"},
		{"slug": "x", "title": "t", "url": "https://u:p@example.com"},
		{"slug": "x", "title": "t", "url": "file:///etc/passwd"},
		{"slug": "x", "title": "t", "port": 80, "host": "bad host"},
		{"slug": "x", "title": "t", "port": 80, "expose_port": 22},
	} {
		if err := e.call(strangerCred, "page.register", with(kid, bad), nil); codeOf(err) != CodeInvalidParams {
			t.Errorf("%v: %v", bad, err)
		}
	}
	// Someone else's slug, and someone else's exposed port, conflict; the
	// owner may replace its own.
	if err := e.call(strangerCred, "page.register", with(main, map[string]any{"slug": "app", "title": "t", "port": 1}), nil); codeOf(err) != CodeConflict {
		t.Errorf("taken slug: %v", err)
	}
	if err := e.call(strangerCred, "page.register", with(main, map[string]any{"slug": "other", "title": "t", "port": 1, "expose_port": 8443}), nil); codeOf(err) != CodeConflict {
		t.Errorf("taken port: %v", err)
	}
	e.mustCall(strangerCred, "page.register", with(kid, map[string]any{"slug": "app", "title": "My app v2", "port": 3001, "expose_port": 8443}), nil)

	// The human sees every link; kid only its own; main its own and kid's.
	var list []Page
	e.mustCall(strangerCred, "page.list", with(kid, nil), &list)
	if len(list) != 1 || list[0].Title != "My app v2" {
		t.Fatalf("kid list %+v", list)
	}
	e.mustCall(strangerCred, "page.list", with(main, nil), &list)
	if len(list) != 2 {
		t.Fatalf("main list %+v", list)
	}
	// kid cannot remove its parent's link; main can remove kid's.
	if err := e.call(strangerCred, "page.unregister", with(kid, map[string]any{"slug": "docs"}), nil); codeOf(err) != CodeNotFound {
		t.Errorf("kid removed docs: %v", err)
	}
	e.mustCall(strangerCred, "page.unregister", with(main, map[string]any{"slug": "app"}), nil)
	e.mustCall(adminCred, "page.list", nil, &list)
	if len(list) != 1 || list[0].Slug != "docs" {
		t.Fatalf("admin list %+v", list)
	}
	// A stopped owner's link goes inactive, and its slug can be taken.
	e.mustCall(strangerCred, "page.register", with(kid, map[string]any{"slug": "app", "title": "t", "port": 1}), nil)
	e.mustCall(adminCred, "agent.stop", map[string]any{"agent": "kid"}, nil)
	e.mustCall(adminCred, "page.list", nil, &list)
	if len(list) != 1 {
		t.Fatalf("inactive link listed %+v", list)
	}
	e.mustCall(adminCred, "page.list", map[string]any{"all": true}, &list)
	if len(list) != 2 || list[0].Slug != "app" || list[0].Active {
		t.Fatalf("all %+v", list)
	}
	e.mustCall(strangerCred, "page.register", with(main, map[string]any{"slug": "app", "title": "t", "port": 1}), nil)
}

func TestSecretRequests(t *testing.T) {
	e := newEnv(t)
	main := e.standing(UserAgentID)
	kid := e.spawn(strangerCred, main, map[string]any{"name": "kid"})
	var s SecretRequest
	e.mustCall(strangerCred, "secret.request", with(kid, map[string]any{"name": "OPENAI_KEY", "reason": "to call the API"}), &s)
	if s.ID != "sec-1" || s.Path != "~/.config/silo/secrets/OPENAI_KEY" || s.State != SecretPending || s.Agent != "kid" {
		t.Fatalf("request %+v", s)
	}
	e.mustCall(strangerCred, "secret.request", with(main, map[string]any{"name": "gh", "reason": "push", "path": "~/b/./gh-token"}), &s)
	if s.Path != "~/b/gh-token" {
		t.Fatalf("path %q", s.Path)
	}
	for _, bad := range []map[string]any{
		{"name": "bad name", "reason": "r"},
		{"name": "x", "reason": ""},
		{"name": "x", "reason": "r", "path": "relative"},
		{"name": "x", "reason": "r", "path": "/etc/../shadow"},
		{"name": "x", "reason": "r", "path": "~/../other/x"},
		{"name": "x", "reason": "r", "path": "/dir/"},
		{"name": "x", "reason": "r", "value": "hunter2"},
	} {
		if err := e.call(strangerCred, "secret.request", with(kid, bad), nil); codeOf(err) != CodeInvalidParams {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if err := e.call(strangerCred, "secret.request", with(kid, map[string]any{"name": "OPENAI_KEY", "reason": "again"}), nil); codeOf(err) != CodeConflict {
		t.Errorf("duplicate: %v", err)
	}
	if err := e.call(adminCred, "secret.request", map[string]any{"name": "x", "reason": "r"}, nil); codeOf(err) != CodeInvalidParams {
		t.Errorf("human request: %v", err)
	}
	var hb HeartbeatResult
	e.mustCall(strangerCred, "agent.heartbeat", with(kid, nil), &hb)
	if hb.Secrets != 1 {
		t.Fatalf("heartbeat %+v", hb)
	}

	// Only the human answers, and never with the value itself.
	if err := e.call(strangerCred, "secret.provide", with(main, map[string]any{"id": "sec-1"}), nil); codeOf(err) != CodeForbidden {
		t.Errorf("agent provided: %v", err)
	}
	if err := e.call(bridgeCred, "secret.provide", map[string]any{"id": "sec-1", "value": "hunter2"}, nil); codeOf(err) != CodeInvalidParams {
		t.Errorf("value accepted: %v", err)
	}
	e.mustCall(bridgeCred, "secret.provide", map[string]any{"id": "sec-1"}, &s)
	if s.State != SecretStored || s.Resolved == nil {
		t.Fatalf("provided %+v", s)
	}
	if err := e.call(bridgeCred, "secret.reject", map[string]any{"id": "sec-1"}, nil); codeOf(err) != CodeConflict {
		t.Errorf("answered twice: %v", err)
	}
	e.mustCall(bridgeCred, "secret.reject", map[string]any{"id": "sec-2", "note": "use the deploy key"}, &s)
	if s.State != SecretRejected || s.Note != "use the deploy key" {
		t.Fatalf("rejected %+v", s)
	}
	if err := e.call(bridgeCred, "secret.reject", map[string]any{"id": "sec-9"}, nil); codeOf(err) != CodeNotFound {
		t.Errorf("unknown id: %v", err)
	}
	if ev := e.lastEvent(EvSecretProvide); ev["name"] != "OPENAI_KEY" || ev["path"] != "~/.config/silo/secrets/OPENAI_KEY" {
		t.Fatalf("audit %v", ev)
	}

	var list []SecretRequest
	e.mustCall(strangerCred, "secret.list", with(kid, nil), &list)
	if len(list) != 1 || list[0].ID != "sec-1" {
		t.Fatalf("kid list %+v", list)
	}
	e.mustCall(adminCred, "secret.list", map[string]any{"pending": true}, &list)
	if len(list) != 0 {
		t.Fatalf("pending %+v", list)
	}
	log, _ := os.ReadFile(e.cfg.AuditLog)
	if strings.Contains(string(log), "hunter2") {
		t.Fatal("a secret value reached the audit log")
	}
}
