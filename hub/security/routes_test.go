package security_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/server"
	"github.com/bwsalmon/silo/hub/tasks"
)

// The callers every route is tried as (docs/security-tests.md §2).
const (
	cAnon        = "no-identity"
	cForged      = "forged-jwt"
	cExpired     = "expired-jwt"
	cWrongAud    = "wrong-audience-jwt"
	cNonMember   = "non-member"
	cMember      = "member"
	cTeamAdmin   = "team-admin"
	cOtherAdmin  = "other-team-admin"
	cNodeOwn     = "node-token-own-team"
	cNodeOther   = "node-token-other-team"
	cNodeRevoked = "node-token-revoked"
)

var callers = []string{cAnon, cForged, cExpired, cWrongAud, cNonMember, cMember, cTeamAdmin, cOtherAdmin, cNodeOwn, cNodeOther, cNodeRevoked}

// unsigned are the callers no human route may sign in: no identity, a
// JWT that does not verify for this hub, and a node token.
var unsigned = []string{cAnon, cForged, cExpired, cWrongAud, cNodeOwn, cNodeOther, cNodeRevoked}

// human is a human route's outcomes: 401 for every caller who is not
// a signed-in human, and the given codes for the four who are.
func human(nonMember, member, teamAdmin, otherAdmin int) map[string]int {
	m := map[string]int{cNonMember: nonMember, cMember: member, cTeamAdmin: teamAdmin, cOtherAdmin: otherAdmin}
	for _, c := range unsigned {
		m[c] = http.StatusUnauthorized
	}
	return m
}

// everyone is a route that answers code to every caller.
func everyone(code int) map[string]int {
	m := map[string]int{}
	for _, c := range callers {
		m[c] = code
	}
	return m
}

// world is the fixture the matrix runs in: two teams, a node of each
// serving a running task, and the credentials of every caller.
type world struct {
	hb    *hub
	as    map[string]as
	runA  tasks.Task // team A's interactive task, running on ws-a
	runB  tasks.Task // team B's task, running on ws-b
	nodeA *fakeNode
	nodeB *fakeNode

	spareToken string // alice's token for node spare-tok, to revoke
}

func newWorld(t *testing.T) *world {
	t.Helper()
	hb := startHub(t, options{})
	hb.teams()
	r := hb.user(root)
	hb.must(r, "PUT", "/v1/teams/"+teamA+"/members/user:zed2@example.com", map[string]string{"role": "member"}, http.StatusNoContent, nil)
	hb.must(r, "PUT", "/v1/teams/"+teamA+"/repos/acme/spare2", nil, http.StatusNoContent, nil)
	for _, delID := range []string{"del-a", "del-dep-a"} {
		hb.must(hb.user(alice), "POST", "/v1/teams", map[string]string{"id": delID, "name": delID}, http.StatusCreated, nil)
		hb.must(hb.user(alice), "PUT", "/v1/teams/"+delID+"/members/user:"+dave, map[string]string{"role": "member"}, http.StatusNoContent, nil)
	}
	w := &world{hb: hb}

	tokA, _ := hb.mint(alice, "ws-a")
	tokB, _ := hb.mint(bob, "ws-b")
	w.nodeA = connectNode(t, hb, tokA, "ws-a", repoA)
	w.nodeB = connectNode(t, hb, tokB, "ws-b", repoB)
	probeA, _ := hb.mint(alice, "probe-a")
	probeB, _ := hb.mint(bob, "probe-b")
	revoked, revokedID := hb.mint(alice, "probe-r")
	hb.must(hb.user(alice), "DELETE", "/v1/node-tokens/"+revokedID, nil, http.StatusNoContent, nil)
	_, w.spareToken = hb.mint(alice, "spare-tok")
	hb.mint(alice, "spare-rel")

	forger := newIAPKey(t, hb.key.kid) // the same key ID, another key
	w.as = map[string]as{
		cAnon:   nobody,
		cForged: func(r *http.Request) { r.Header.Set("x-goog-iap-jwt-assertion", forger.sign(t, alice, nil)) },
		cExpired: func(r *http.Request) {
			r.Header.Set("x-goog-iap-jwt-assertion", hb.key.sign(t, alice, func(c map[string]any) {
				c["iat"], c["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
			}))
		},
		cWrongAud: func(r *http.Request) {
			r.Header.Set("x-goog-iap-jwt-assertion", hb.key.sign(t, alice, func(c map[string]any) {
				c["aud"] = "/projects/1/global/backendServices/999"
			}))
		},
		cNonMember:   hb.user(nina),
		cMember:      hb.user(dave),
		cTeamAdmin:   hb.user(alice),
		cOtherAdmin:  hb.user(carol),
		cNodeOwn:     bearer(probeA),
		cNodeOther:   bearer(probeB),
		cNodeRevoked: bearer(revoked),
	}

	w.runA = w.run(alice, repoA, w.nodeA, true)
	w.runB = w.run(bob, repoB, w.nodeB, false)
	// Team A's task asks one approval for each caller, and lists an
	// artifact.
	lines := []string{event(w.runA.ID, 1, events.TypeStatus, `{"phase":"running","state":"started","message":"go"}`)}
	for i, c := range callers {
		lines = append(lines, event(w.runA.ID, int64(i+2), events.TypeApprovalRequest, fmt.Sprintf(`{"id":"req-%s","tool":"push"}`, c)))
	}
	lines = append(lines, event(w.runA.ID, 50, live.TypeArtifact, `{"kind":"diff","ref":"diff-1","size":14}`))
	w.nodeA.events(lines...)
	w.nodeB.events(event(w.runB.ID, 1, events.TypeStatus, `{"phase":"running","state":"started","message":"go"}`))
	eventually(t, "team A's events stored", func() bool {
		_, evs := hb.replay(hb.user(alice), w.runA.ID, 0, len(lines))
		return len(evs) == len(lines)
	})
	return w
}

// run files a task as who in repo and waits for node to be running it.
func (w *world) run(who, repo string, n *fakeNode, interactive bool) tasks.Task {
	w.hb.t.Helper()
	body := map[string]string{"repo": repo, "prompt": "do the thing"}
	if interactive {
		body["kind"] = tasks.KindInteractive
	}
	tk := w.hb.file(who, body)
	eventually(w.hb.t, "task "+tk.ID+" running on "+n.id, func() bool {
		got, _ := w.hb.task(w.hb.user(who), tk.ID)
		return got.State == tasks.Running && n.taskStream(tk.ID) != nil
	})
	return tk
}

// queued files a task as who in team A's quiet repository, which no
// node serves: it stays queued (or proposed, from outside the team).
func (w *world) queued(who string) string {
	return w.hb.file(who, map[string]string{"repo": quietA, "prompt": "later"}).ID
}

// row is one route's entry in the matrix.
type row struct {
	route server.Route
	// method and path are the request: path's {task} is the row's
	// task, {caller} the caller's name.
	method, path string
	host         string // a Host header, for the node UI's route
	body         func(caller string) any
	upgrade      bool // a WebSocket handshake, for the rendezvous
	// task makes the row's task afresh for each caller, for a route
	// that changes it; nil uses team A's running task.
	task func(w *world) string
	want map[string]int
	// check, when set, checks a response the caller was let through.
	check func(t *testing.T, caller string, h http.Header)
}

func pub(p string) server.Route { return server.Route{Listener: server.ListenerPublic, Pattern: p} }
func internal(p string) server.Route {
	return server.Route{Listener: server.ListenerInternal, Pattern: p}
}

func noStore(t *testing.T, caller string, h http.Header) {
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("as %s: Cache-Control %q, want no-store", caller, cc)
	}
}

// matrix is every route the hub serves, and what each caller gets. A
// route missing here fails TestRouteMatrixCoversEveryRoute: add it,
// with every caller's outcome, when you add the route.
func matrix() []row {
	none := func(string) any { return nil }
	body := func(v any) func(string) any { return func(string) any { return v } }
	queuedByAlice := func(w *world) string { return w.queued(alice) }
	proposedByPat := func(w *world) string { return w.queued(pat) }
	connect := everyone(http.StatusUnauthorized)
	connect[cNodeOwn], connect[cNodeOther] = http.StatusSwitchingProtocols, http.StatusSwitchingProtocols
	grainfileContent := everyone(http.StatusUnauthorized)
	grainfileContent[cNodeOwn], grainfileContent[cNodeOther] = http.StatusForbidden, http.StatusForbidden
	nodeUI := human(http.StatusNotFound, http.StatusNotFound, http.StatusOK, http.StatusNotFound)
	return []row{
		// Probes, public keys and the web UI's page sign no one in and
		// hold nothing of anyone's.
		{route: pub("GET /healthz"), method: "GET", path: "/healthz", want: everyone(200)},
		{route: pub("GET /readyz"), method: "GET", path: "/readyz", want: everyone(200)},
		{route: pub("GET /v1/claims/keys"), method: "GET", path: "/v1/claims/keys", want: everyone(200)},
		{route: pub("/"), method: "GET", path: "/tasks/x", want: everyone(200)},

		// Grainfiles: webhook ingestion (HMAC required, so unsigned callers get 401),
		// node content proxy (node token + subscription check), and human inspection.
		{route: pub("POST " + grainfiles.GitHubWebhookPath), method: "POST", path: grainfiles.GitHubWebhookPath, body: body(map[string]string{"repo": "acme/app"}), want: everyone(http.StatusUnauthorized)},
		{route: pub("GET " + grainfiles.ContentPath), method: "GET", path: grainfiles.ContentPath + "?grainfile_url=https://github.com/acme/app/blob/main/tasks/fix.task.md", want: grainfileContent},
		{route: pub("GET " + grainfiles.GrainfilesPath), method: "GET", path: grainfiles.GrainfilesPath, want: human(200, 200, 200, 200)},

		// Nodes: a node token, never a human's sign-on.
		{route: pub("GET " + rendezvous.ConnectPath), method: "GET", path: rendezvous.ConnectPath, upgrade: true, want: connect},
		{route: pub("GET " + rendezvous.DirectoryPath), method: "GET", path: rendezvous.DirectoryPath, want: human(200, 200, 200, 200)},

		// The team / deployment directory and user acceptance allowlist.
		{route: pub("GET /v1/me"), method: "GET", path: "/v1/me", want: human(200, 200, 200, 200)},
		{route: pub("PUT /v1/me/theme"), method: "PUT", path: "/v1/me/theme", body: body(map[string]string{"theme": "dark"}), want: human(200, 200, 200, 200)},
		{route: pub("GET /v1/me/inbox"), method: "GET", path: "/v1/me/inbox", want: human(200, 200, 200, 200)},
		{route: pub("PUT /v1/me/inbox"), method: "PUT", path: "/v1/me/inbox", body: body(map[string][]string{"nodes": {}}), want: human(200, 200, 200, 200)},
		{route: pub("GET /v1/users"), method: "GET", path: "/v1/users", want: human(403, 403, 403, 403)},
		{route: pub("PUT /v1/users/{email}"), method: "PUT", path: "/v1/users/new@example.com", body: body(map[string]bool{"sys_admin": false}), want: human(403, 403, 403, 403)},
		{route: pub("DELETE /v1/users/{email}"), method: "DELETE", path: "/v1/users/" + nina, want: human(403, 403, 403, 403)},
		{route: pub("GET /v1/deployments"), method: "GET", path: "/v1/deployments", want: human(200, 200, 200, 200)},
		{route: pub("POST /v1/deployments"), method: "POST", path: "/v1/deployments", body: func(c string) any { return map[string]string{"id": "d-" + c} }, want: human(201, 201, 201, 201)},
		{route: pub("GET /v1/deployments/{id}"), method: "GET", path: "/v1/deployments/team-a", want: human(404, 200, 200, 404)},
		{route: pub("DELETE /v1/deployments/{id}"), method: "DELETE", path: "/v1/deployments/del-dep-a", want: human(404, 403, 204, 404)},
		{route: pub("PUT /v1/deployments/{id}/members/{principal}"), method: "PUT", path: "/v1/deployments/team-a/members/user:yan@example.com", body: body(map[string]string{"role": "member"}), want: human(404, 403, 204, 404)},
		{route: pub("DELETE /v1/deployments/{id}/members/{principal}"), method: "DELETE", path: "/v1/deployments/team-a/members/user:zed2@example.com", want: human(404, 403, 204, 404)},
		{route: pub("PUT /v1/deployments/{id}/repos/{owner}/{name}"), method: "PUT", path: "/v1/deployments/team-a/repos/acme/new-dep", want: human(404, 403, 204, 404)},
		{route: pub("DELETE /v1/deployments/{id}/repos/{owner}/{name}"), method: "DELETE", path: "/v1/deployments/team-a/repos/acme/spare2", want: human(404, 403, 204, 404)},
		{route: pub("GET /v1/deployments/{id}/settings"), method: "GET", path: "/v1/deployments/team-a/settings", want: human(404, 200, 200, 404)},
		{route: pub("PUT /v1/deployments/{id}/settings"), method: "PUT", path: "/v1/deployments/team-a/settings", body: body(map[string]int{"lost_requeue_after_seconds": 600}), want: human(404, 403, 200, 404)},
		{route: pub("POST /v1/deployments/{id}/token"), method: "POST", path: "/v1/deployments/team-a/token", want: human(404, 403, 201, 404), check: noStore},
		{route: pub("POST /v1/deployments/{id}/replace-host"), method: "POST", path: "/v1/deployments/team-a/replace-host", want: human(404, 403, 201, 404), check: noStore},
		{route: pub("GET /v1/teams"), method: "GET", path: "/v1/teams", want: human(200, 200, 200, 200)},
		{route: pub("POST /v1/teams"), method: "POST", path: "/v1/teams", body: func(c string) any { return map[string]string{"id": "t-" + c} }, want: human(201, 201, 201, 201)},
		{route: pub("GET /v1/teams/{team}"), method: "GET", path: "/v1/teams/team-a", want: human(404, 200, 200, 404)},
		{route: pub("DELETE /v1/teams/{team}"), method: "DELETE", path: "/v1/teams/del-a", want: human(404, 403, 204, 404)},
		{route: pub("PUT /v1/teams/{team}/members/{principal}"), method: "PUT", path: "/v1/teams/team-a/members/user:yan@example.com", body: body(map[string]string{"role": "member"}), want: human(404, 403, 204, 404)},
		{route: pub("DELETE /v1/teams/{team}/members/{principal}"), method: "DELETE", path: "/v1/teams/team-a/members/user:" + zed, want: human(404, 403, 204, 404)},
		{route: pub("PUT /v1/teams/{team}/repos/{owner}/{name}"), method: "PUT", path: "/v1/teams/team-a/repos/acme/new", want: human(404, 403, 204, 404)},
		{route: pub("DELETE /v1/teams/{team}/repos/{owner}/{name}"), method: "DELETE", path: "/v1/teams/team-a/repos/" + spareA, want: human(404, 403, 204, 404)},
		{route: pub("GET /v1/teams/{team}/settings"), method: "GET", path: "/v1/teams/team-a/settings", want: human(404, 200, 200, 404)},
		{route: pub("PUT /v1/teams/{team}/settings"), method: "PUT", path: "/v1/teams/team-a/settings", body: body(map[string]int{"lost_requeue_after_seconds": 600}), want: human(404, 403, 200, 404)},
		{route: pub("GET /v1/node-tokens"), method: "GET", path: "/v1/node-tokens", want: human(200, 200, 200, 200)},
		{route: pub("POST /v1/node-tokens"), method: "POST", path: "/v1/node-tokens", body: func(c string) any { return map[string]string{"node": "mint-" + c} }, want: human(201, 201, 201, 201),
			check: noStore},
		{route: pub("DELETE /v1/node-tokens/{id}"), method: "DELETE", path: "/v1/node-tokens/{spare}", want: human(404, 404, 204, 404)},
		{route: pub("DELETE /v1/node-tokens/nodes/{node}"), method: "DELETE", path: "/v1/node-tokens/nodes/spare-rel", want: human(404, 404, 204, 404)},

		// The backlog. Anyone signed in may propose a task to a team;
		// a task the caller may not see is a 404 (docs/hub.md §6.1).
		{route: pub("GET " + tasks.InboxPath), method: "GET", path: tasks.InboxPath, want: human(200, 200, 200, 200)},
		{route: pub("POST /v1/tasks"), method: "POST", path: "/v1/tasks", body: body(map[string]string{"repo": quietA, "prompt": "p"}), want: human(201, 201, 201, 201)},
		{route: pub("GET /v1/tasks"), method: "GET", path: "/v1/tasks", want: human(200, 200, 200, 200)},
		{route: pub("GET /v1/tasks/{id}"), method: "GET", path: "/v1/tasks/{task}", want: human(404, 200, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/cancel"), method: "POST", path: "/v1/tasks/{task}/cancel", body: body(map[string]string{"reason": "r"}), task: queuedByAlice, want: human(404, 403, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/requeue"), method: "POST", path: "/v1/tasks/{task}/requeue", body: none, task: queuedByAlice, want: human(404, 403, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/approve"), method: "POST", path: "/v1/tasks/{task}/approve", body: none, task: proposedByPat, want: human(404, 200, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/reject"), method: "POST", path: "/v1/tasks/{task}/reject", body: body(map[string]string{"reason": "r"}), task: proposedByPat, want: human(404, 200, 200, 404)},
		{route: pub("GET /v1/tasks/{id}/approvals"), method: "GET", path: "/v1/tasks/{task}/approvals", want: human(404, 200, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/approvals/{request}"), method: "POST", path: "/v1/tasks/{task}/approvals/req-{caller}", body: body(map[string]string{"decision": "allow"}), want: human(404, 200, 200, 404)},
		{route: pub("GET /v1/tasks/{id}/messages"), method: "GET", path: "/v1/tasks/{task}/messages", want: human(404, 200, 200, 404)},
		{route: pub("POST /v1/tasks/{id}/messages"), method: "POST", path: "/v1/tasks/{task}/messages", body: body(map[string]string{"text": "hello"}), want: human(404, 201, 201, 404)},
		{route: pub("GET " + live.EventsPath), method: "GET", path: "/v1/tasks/{task}/events", want: human(404, 200, 200, 404), check: noStore},
		{route: pub("GET " + live.ArtifactsPath), method: "GET", path: "/v1/tasks/{task}/artifacts?ref=diff-1", want: human(404, 200, 200, 404), check: func(t *testing.T, c string, h http.Header) {
			noStore(t, c, h)
			if h.Get("Content-Security-Policy") != "sandbox" || h.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("as %s: an artifact served as %q, CSP %q", c, h.Get("Content-Type"), h.Get("Content-Security-Policy"))
			}
		}},

		// The other pods' listener: their shared key, nobody else's
		// credential.
		{route: internal("GET " + rendezvous.InternalOpenPath), method: "GET", path: "/internal/v1/nodes/ws-a/open", want: everyone(401)},
		{route: internal("GET " + live.InternalEventsPath), method: "GET", path: "/internal/v1/tasks/{task}/events", want: everyone(401)},

		// The scraper's listener, which no load balancer reaches.
		{route: server.Route{Listener: server.ListenerMetrics, Pattern: "GET " + server.MetricsPath}, method: "GET", path: server.MetricsPath, want: everyone(200)},

		// A node's own UI: its owner only; anyone else, 404 whether or
		// not the node exists (docs/hub.md §11).
		{route: server.Route{Listener: server.ListenerNodeUI, Pattern: "/"}, method: "GET", host: "ws-a." + nodeDomain, path: "/", want: nodeUI},
	}
}

// TestRouteMatrixCoversEveryRoute fails when the hub serves a route
// the matrix has no entry for, or the matrix has one for a route the
// hub no longer serves. The routes come from the hub's own routers
// (server.Hub.Routes), so a new route cannot be missed.
func TestRouteMatrixCoversEveryRoute(t *testing.T) {
	hb := startHub(t, options{})
	served := map[server.Route]bool{}
	for _, r := range hb.h.Routes() {
		served[r] = true
	}
	inMatrix := map[server.Route]bool{}
	for _, r := range matrix() {
		if inMatrix[r.route] {
			t.Errorf("route %s %q is in the matrix twice", r.route.Listener, r.route.Pattern)
		}
		inMatrix[r.route] = true
		if !served[r.route] {
			t.Errorf("the matrix lists %s %q, which the hub does not serve: remove it", r.route.Listener, r.route.Pattern)
		}
		for _, c := range callers {
			if _, ok := r.want[c]; !ok {
				t.Errorf("route %s %q has no outcome for caller %s", r.route.Listener, r.route.Pattern, c)
			}
		}
	}
	for r := range served {
		if !inMatrix[r] {
			t.Errorf("the hub serves %s %q, which the security matrix (hub/security/routes_test.go) has no entry for: add one, with every caller's outcome (docs/security-tests.md)", r.Listener, r.Pattern)
		}
	}
}

// TestRouteMatrix asserts every cell: each route, as each caller.
func TestRouteMatrix(t *testing.T) {
	w := newWorld(t)
	internalSrv := httptest.NewServer(w.hb.h.InternalHandler())
	defer internalSrv.Close()
	metricsSrv := httptest.NewServer(w.hb.h.MetricsHandler())
	defer metricsSrv.Close()

	for _, r := range matrix() {
		t.Run(r.route.Listener+" "+r.route.Pattern, func(t *testing.T) {
			for _, c := range callers {
				task := w.runA.ID
				if r.task != nil {
					task = r.task(w)
				}
				path := strings.NewReplacer("{task}", task, "{caller}", c, "{spare}", w.spareToken).Replace(r.path)
				base := w.hb.url
				switch r.route.Listener {
				case server.ListenerInternal:
					base = internalSrv.URL
				case server.ListenerMetrics:
					base = metricsSrv.URL
				}
				var b any
				if r.body != nil {
					b = r.body(c)
				}
				code, h := try(t, w.as[c], r.method, base+path, r.host, b, r.upgrade)
				if code != r.want[c] {
					t.Errorf("%s %s as %s: %d, want %d", r.method, path, c, code, r.want[c])
					continue
				}
				if r.check != nil && code < 300 {
					r.check(t, c, h)
				}
			}
		})
	}

	// The metrics are on their own listener only: the public one's
	// catch-all is the web UI's page, never them.
	_, b, _ := w.hb.do(nobody, "GET", server.MetricsPath, nil)
	if strings.Contains(string(b), "# HELP") {
		t.Error("the public listener serves the metrics")
	}
}

// try makes one request and answers its status and headers, without
// waiting for a streamed body (events) to end.
func try(t *testing.T, who as, method, url, host string, body any, upgrade bool) (int, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if host != "" {
		req.Host = host
	}
	if upgrade {
		key := make([]byte, 16)
		rand.Read(key)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
		req.Header.Set("Sec-WebSocket-Protocol", rendezvous.Protocol)
	}
	who(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Every route is also listed by the hub with OIDC sign-on, whose own
// routes are the sign-in flow's: public by nature, and checked by
// hub/identity's tests. This keeps them from hiding a route of the
// hub's own behind the OIDC configuration.
func TestOIDCRoutesAreOnlyTheSignIn(t *testing.T) {
	h, err := server.New(context.Background(), server.Config{
		Database:        "sqlite::memory:",
		OIDCIssuer:      "https://accounts.example.com",
		OIDCClientID:    "hub",
		OIDCRedirectURL: "https://hub.example.com/auth/callback",
		OIDCSessionKey:  make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	iap := startHub(t, options{})
	known := map[server.Route]bool{}
	for _, r := range iap.h.Routes() {
		known[r] = true
	}
	var extra []string
	for _, r := range h.Routes() {
		if !known[r] && r.Listener != server.ListenerNodeUI {
			extra = append(extra, r.Pattern)
		}
	}
	slices.Sort(extra)
	want := []string{"GET /auth/callback", "GET /auth/login", "POST /auth/logout"}
	if !slices.Equal(extra, want) {
		t.Errorf("an OIDC hub's own routes are %q, want only the sign-in's %q: give a new one a matrix entry", extra, want)
	}
}
