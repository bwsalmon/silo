package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/internal/testlog"
)

// headerAuth is a test sign-on: X-User is the email, X-Groups a
// comma-separated list.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	u := r.Header.Get("X-User")
	if u == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	p := identity.Principal{Email: u}
	if g := r.Header.Get("X-Groups"); g != "" {
		p.Groups = strings.Split(g, ",")
	}
	return p, nil
}

// testHub is a hub with the directory wired into the rendezvous, as
// docs/hub.md §5 lays it out.
type testHub struct {
	store *Store
	rv    *rendezvous.Server
	url   string
}

func newTestHub(t *testing.T) *testHub {
	s, err := Open(context.Background(), "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	rv := &rendezvous.Server{
		Registry:     registry.New(),
		Authenticate: s.Authenticate,
		Admit:        s.Admit,
		ValidateUI:   s.ValidateNodeUI,
		Heartbeat:    time.Second,
		Logf:         testlog.Logf(t),
	}
	api := &API{Store: s, Auth: headerAuth{}, HubAdmins: []string{"group:hub-admins@example.com"}, Disconnect: rv.Disconnect, Registry: rv.Registry}
	rv.Visible = api.Visible
	rv.ResolveEntries = api.ResolveEntries
	m := http.NewServeMux()
	m.Handle(rendezvous.ConnectPath, rv.Handler())
	m.Handle(rendezvous.DirectoryPath, rv.Handler())
	m.Handle("/", api.Handler())
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return &testHub{store: s, rv: rv, url: ts.URL}
}

// do calls the API as user (with groups after a comma-free list).
func (h *testHub) do(t *testing.T, user, method, path string, body any, groups ...string) (int, []byte) {
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
	if len(groups) > 0 {
		req.Header.Set("X-Groups", strings.Join(groups, ","))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (h *testHub) want(t *testing.T, code int, user, method, path string, body any, groups ...string) []byte {
	t.Helper()
	got, b := h.do(t, user, method, path, body, groups...)
	if got != code {
		t.Fatalf("%s %s %s as %s: %d %s, want %d", method, path, body, user, got, b, code)
	}
	return b
}

func TestAPI(t *testing.T) {
	h := newTestHub(t)
	const root, alice, bob = "root@example.com", "alice@example.com", "bob@example.com"
	admins := "hub-admins@example.com"

	h.want(t, 401, "", "GET", MePath, nil)
	h.want(t, 201, alice, "POST", TeamsPath, map[string]string{"id": "infra", "name": "Infra"})
	h.want(t, 201, bob, "POST", TeamsPath, map[string]string{"id": "web"})
	h.want(t, 409, root, "POST", TeamsPath, map[string]string{"id": "web"}, admins)
	h.want(t, 400, root, "POST", TeamsPath, map[string]string{"id": "Bad Team"}, admins)
	h.want(t, 400, root, "POST", TeamsPath, map[string]any{"id": "x", "extra": 1}, admins)

	// A deployment admin may claim an unowned repository (without history);
	// history transfer requires a hub admin, and claiming another team's repo is 409.
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/repos/bwsalmon/silo", nil)
	h.want(t, 204, root, "PUT", TeamsPath+"/web/repos/bwsalmon/web", map[string]bool{"history": true}, admins)
	h.want(t, 409, bob, "PUT", TeamsPath+"/web/repos/bwsalmon/silo", nil)          // infra's
	h.want(t, 409, root, "PUT", TeamsPath+"/web/repos/bwsalmon/silo", nil, admins) // infra's
	h.want(t, 400, root, "PUT", TeamsPath+"/web/repos/bwsalmon/x", map[string]any{"transfer": true}, admins)
	h.want(t, 404, root, "PUT", TeamsPath+"/nope/repos/bwsalmon/x", nil, admins)

	// Alice administers infra, and only infra; Carol is only a member.
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/members/user:carol@example.com", map[string]string{"role": "member"})
	h.want(t, 403, alice, "PUT", TeamsPath+"/infra/repos/bwsalmon/x", map[string]bool{"history": true})
	h.want(t, 404, alice, "PUT", TeamsPath+"/web/repos/bwsalmon/x", nil)
	h.want(t, 403, "carol@example.com", "PUT", TeamsPath+"/infra/repos/bwsalmon/x", nil)
	h.want(t, 403, "carol@example.com", "DELETE", TeamsPath+"/infra", nil)
	if owner, _ := h.store.RepoOwner(context.Background(), "bwsalmon/x"); owner != "" {
		t.Errorf("bwsalmon/x was assigned to %q", owner)
	}

	var teams struct{ Teams []Team }
	json.Unmarshal(h.want(t, 200, alice, "GET", TeamsPath, nil), &teams)
	if len(teams.Teams) != 1 || teams.Teams[0].ID != "infra" || len(teams.Teams[0].Members) != 2 {
		t.Errorf("alice's teams %+v", teams)
	}
	json.Unmarshal(h.want(t, 200, root, "GET", TeamsPath, nil, admins), &teams)
	if len(teams.Teams) != 2 {
		t.Errorf("hub admin's teams %+v", teams)
	}
	// One team, to its members and a hub admin; to anyone else it does
	// not exist.
	var one Team
	json.Unmarshal(h.want(t, 200, "carol@example.com", "GET", TeamsPath+"/infra", nil), &one)
	if one.ID != "infra" || one.Members["user:"+alice] != Admin || len(one.Repos) != 1 || one.Repos[0] != "bwsalmon/silo" {
		t.Errorf("infra as carol %+v", one)
	}
	h.want(t, 200, root, "GET", TeamsPath+"/web", nil, admins)
	h.want(t, 404, alice, "GET", TeamsPath+"/web", nil)
	h.want(t, 404, root, "GET", TeamsPath+"/nope", nil, admins)
	var me struct {
		Email    string
		HubAdmin bool `json:"hub_admin"`
		Teams    map[string]Role
	}
	json.Unmarshal(h.want(t, 200, alice, "GET", MePath, nil), &me)
	if me.Email != alice || me.HubAdmin || me.Teams["infra"] != Admin {
		t.Errorf("alice's /v1/me %+v", me)
	}

	// grain hub login: Alice mints a token for her node.
	mint := func(user, node string) string {
		var r struct {
			Token     string
			NodeToken NodeToken `json:"node_token"`
		}
		json.Unmarshal(h.want(t, 201, user, "POST", NodeTokensPath, map[string]string{"node": node}), &r)
		return r.Token
	}
	aliceTok, bobTok := mint(alice, "ws-alice"), mint(bob, "ws-bob")
	h.want(t, 409, bob, "POST", NodeTokensPath, map[string]string{"node": "ws-alice"})

	connect := func(tok string, n registry.Node) (*rendezvous.Conn, error) {
		cl := &rendezvous.Client{Hub: h.url, Token: func(context.Context) (string, error) { return tok, nil }, Node: n}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return cl.Connect(ctx)
	}
	// Alice's node may serve infra's repository, not web's, and not one
	// nobody owns; nor may her token register another node ID.
	_, err := connect(aliceTok, registry.Node{ID: "ws-alice", Repos: []string{"bwsalmon/silo", "bwsalmon/web"}, Slots: 1})
	if !errors.Is(err, rendezvous.ErrRefused) || !strings.Contains(err.Error(), "bwsalmon/web") {
		t.Errorf("alice serving web's repo: %v", err)
	}
	if _, err = connect(aliceTok, registry.Node{ID: "ws-alice", Repos: []string{"nobody/owns-this"}, Slots: 1}); !errors.Is(err, rendezvous.ErrRefused) {
		t.Errorf("alice serving an unowned repo: %v", err)
	}
	if _, err = connect(aliceTok, registry.Node{ID: "ws-bob", Slots: 1}); !errors.Is(err, rendezvous.ErrRefused) {
		t.Errorf("alice's token for bob's node ID: %v", err)
	}
	if _, err = connect("silo-node_forged", registry.Node{ID: "ws-alice", Slots: 1}); err == nil {
		t.Error("a forged token connected")
	}
	ca, err := connect(aliceTok, registry.Node{ID: "ws-alice", Repos: []string{"bwsalmon/silo"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := connect(bobTok, registry.Node{ID: "ws-bob", Repos: []string{"bwsalmon/web"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	// The directory is filtered by the caller's teams.
	nodes := func(user string, groups ...string) []string {
		var d struct{ Nodes []registry.Entry }
		json.Unmarshal(h.want(t, 200, user, "GET", rendezvous.DirectoryPath, nil, groups...), &d)
		var ids []string
		for _, e := range d.Nodes {
			ids = append(ids, e.ID)
		}
		return ids
	}
	h.want(t, 401, "", "GET", rendezvous.DirectoryPath, nil)
	if got := nodes("carol@example.com"); len(got) != 1 || got[0] != "ws-alice" {
		t.Errorf("carol (infra) sees %v", got)
	}
	if got := nodes(bob); len(got) != 1 || got[0] != "ws-bob" {
		t.Errorf("bob (web) sees %v", got)
	}
	if got := nodes("dave@example.com"); len(got) != 0 {
		t.Errorf("dave (no team) sees %v", got)
	}
	if got := nodes(root, admins); len(got) != 2 {
		t.Errorf("hub admin sees %v", got)
	}

	// Revoking a token ends its node's connection, and the next
	// handshake with it is refused.
	var toks struct {
		NodeTokens []NodeToken `json:"node_tokens"`
	}
	json.Unmarshal(h.want(t, 200, alice, "GET", NodeTokensPath, nil), &toks)
	if len(toks.NodeTokens) != 1 || toks.NodeTokens[0].Node != "ws-alice" {
		t.Fatalf("alice's tokens %+v", toks)
	}
	id := toks.NodeTokens[0].ID
	h.want(t, 404, bob, "DELETE", NodeTokensPath+"/"+id, nil)
	h.want(t, 204, alice, "DELETE", NodeTokensPath+"/"+id, nil)
	select {
	case <-ca.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked node is still connected")
	}
	if _, err := connect(aliceTok, registry.Node{ID: "ws-alice", Slots: 1}); err == nil {
		t.Error("a revoked token connected")
	}
	// The node ID stays alice's until she releases it (SA1-H1).
	h.want(t, 409, bob, "POST", NodeTokensPath, map[string]string{"node": "ws-alice"})
	h.want(t, 404, bob, "DELETE", NodeTokensPath+"/nodes/ws-alice", nil)
	h.want(t, 204, alice, "DELETE", NodeTokensPath+"/nodes/ws-alice", nil)
	h.want(t, 201, bob, "POST", NodeTokensPath, map[string]string{"node": "ws-alice"})

	// Minting again revokes the earlier token, and so ends the
	// connection it holds (SA1-L2): a thief's connection with a stolen
	// token does not outlive the owner's recovery from it.
	bobTok2 := mint(bob, "ws-bob")
	select {
	case <-cb.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a re-minted node's old connection is still up")
	}
	if _, err := connect(bobTok, registry.Node{ID: "ws-bob", Slots: 1}); err == nil {
		t.Error("a re-minted node's old token connected")
	}
	cb2, err := connect(bobTok2, registry.Node{ID: "ws-bob", Repos: []string{"bwsalmon/web"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	cb2.Close()
}

// TestIsHubAdmin: each HubAdmins entry matches only its own claim
// (SA1-L6), and one with no prefix is a user.
func TestIsHubAdmin(t *testing.T) {
	a := &API{HubAdmins: []string{"group:Admins@example.com", "user:root@example.com", "legacy@example.com"}}
	for _, c := range []struct {
		p    identity.Principal
		want bool
	}{
		{identity.Principal{Email: "root@example.com"}, true},
		{identity.Principal{Email: "legacy@example.com"}, true},
		{identity.Principal{Email: "x@example.com", Groups: []string{"admins@example.com"}}, true},
		{identity.Principal{Email: "admins@example.com"}, false},
		{identity.Principal{Email: "x@example.com", Groups: []string{"root@example.com"}}, false},
		{identity.Principal{Email: "x@example.com", Groups: []string{"legacy@example.com"}}, false},
	} {
		if got := a.isHubAdmin(c.p); got != c.want {
			t.Errorf("isHubAdmin(%+v) = %v, want %v", c.p, got, c.want)
		}
	}
}

// TestAPISameOrigin: a page on a node's UI host, which is the same site
// as the hub, cannot mint or revoke its viewer's node tokens or
// administer teams as them (SA1-M1), nor can another site, nor a
// no-cors text/plain body.
func TestAPISameOrigin(t *testing.T) {
	h := newTestHub(t)
	const root, admins = "root@example.com", "hub-admins@example.com"
	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "infra"}, admins)
	nodeUI := "http://ws-1.nodes." + strings.TrimPrefix(h.url, "http://")
	send := func(method, path, contentType, body string, header ...string) int {
		t.Helper()
		req, _ := http.NewRequest(method, h.url+path, strings.NewReader(body))
		req.Header.Set("X-User", root)
		req.Header.Set("X-Groups", admins)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for i := 0; i < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		method, path, ctype, body string
		header                    []string
		want                      int
	}{
		{"POST", NodeTokensPath, "text/plain;charset=UTF-8", `{"node":"ws-2"}`, []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{"POST", NodeTokensPath, "application/json", `{"node":"ws-2"}`, []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{"POST", NodeTokensPath, "text/plain", `{"node":"ws-2"}`, nil, 415},
		{"POST", TeamsPath, "application/json", `{"id":"forged"}`, []string{"Origin", nodeUI}, 403},
		{"POST", TeamsPath, "application/json", `{"id":"forged"}`, []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}, 403},
		{"DELETE", TeamsPath + "/infra", "", "", []string{"Sec-Fetch-Site", "same-site", "Origin", nodeUI}, 403},
		{"PUT", TeamsPath + "/infra/members/user:eve@example.com", "text/plain", `{"role":"admin"}`, nil, 415},
	} {
		if got := send(c.method, c.path, c.ctype, c.body, c.header...); got != c.want {
			t.Errorf("%s %s %s %v: %d, want %d", c.method, c.path, c.ctype, c.header, got, c.want)
		}
	}
	var tokens struct {
		NodeTokens []json.RawMessage `json:"node_tokens"`
	}
	json.Unmarshal(h.want(t, 200, root, "GET", NodeTokensPath, nil, admins), &tokens)
	var teams struct{ Teams []json.RawMessage }
	json.Unmarshal(h.want(t, 200, root, "GET", TeamsPath, nil, admins), &teams)
	if len(tokens.NodeTokens) != 0 || len(teams.Teams) != 1 {
		t.Errorf("after refused writes: %d tokens and %d teams, want 0 and 1", len(tokens.NodeTokens), len(teams.Teams))
	}
	// The hub's own page is let through.
	if got := send("POST", NodeTokensPath, "application/json", `{"node":"ws-2"}`, "Sec-Fetch-Site", "same-origin", "Origin", h.url); got != http.StatusCreated {
		t.Errorf("same-origin mint: %d, want 201", got)
	}
}

// TestAdminModeHeader verifies can_hub_admin vs hub_admin when
// X-Silo-Admin-Mode is set to off/on or omitted, and ensures a
// non-admin cannot escalate privileges by sending X-Silo-Admin-Mode: on.
func TestAdminModeHeader(t *testing.T) {
	h := newTestHub(t)
	const root, alice, bob = "root@example.com", "alice@example.com", "bob@example.com"
	const admins = "hub-admins@example.com"

	h.want(t, 201, alice, "POST", TeamsPath, map[string]string{"id": "infra", "name": "Infra"})
	h.want(t, 201, bob, "POST", TeamsPath, map[string]string{"id": "web", "name": "Web"})
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/members/user:"+root, map[string]string{"role": "member"})
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/repos/bwsalmon/silo", nil)
	h.want(t, 204, bob, "PUT", TeamsPath+"/web/repos/bwsalmon/web", nil)

	// Mint node tokens for root and bob.
	h.want(t, 201, root, "POST", NodeTokensPath, map[string]string{"node": "ws-root"}, admins)
	h.want(t, 201, bob, "POST", NodeTokensPath, map[string]string{"node": "ws-bob"})

	doMode := func(user, group, mode, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, h.url+path, rd)
		req.Header.Set("X-User", user)
		if group != "" {
			req.Header.Set("X-Groups", group)
		}
		if mode != "" {
			req.Header.Set(AdminModeHeader, mode)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	type meResp struct {
		Email       string          `json:"email"`
		HubAdmin    bool            `json:"hub_admin"`
		CanHubAdmin bool            `json:"can_hub_admin"`
		Teams       map[string]Role `json:"teams"`
	}

	// 1. Hub admin with header omitted or "on": both can_hub_admin and hub_admin are true.
	for _, mode := range []string{"", "on", "ON", "true", "1"} {
		code, b := doMode(root, admins, mode, "GET", MePath, nil)
		if code != 200 {
			t.Fatalf("GET /v1/me mode=%q: %d %s", mode, code, b)
		}
		var me meResp
		json.Unmarshal(b, &me)
		if !me.CanHubAdmin || !me.HubAdmin || me.Teams["infra"] != Member {
			t.Errorf("GET /v1/me mode=%q: got %+v, want CanHubAdmin=true HubAdmin=true", mode, me)
		}
		var teams struct{ Teams []Team }
		_, tb := doMode(root, admins, mode, "GET", TeamsPath, nil)
		json.Unmarshal(tb, &teams)
		if len(teams.Teams) != 2 {
			t.Errorf("GET /v1/teams mode=%q: got %d teams, want 2", mode, len(teams.Teams))
		}
	}

	// 2. Hub admin with X-Silo-Admin-Mode: off/false/0: can_hub_admin is true, hub_admin is false.
	for _, mode := range []string{"off", "OFF", "false", "0"} {
		code, b := doMode(root, admins, mode, "GET", MePath, nil)
		if code != 200 {
			t.Fatalf("GET /v1/me mode=%q: %d %s", mode, code, b)
		}
		var me meResp
		json.Unmarshal(b, &me)
		if !me.CanHubAdmin || me.HubAdmin {
			t.Errorf("GET /v1/me mode=%q: got %+v, want CanHubAdmin=true HubAdmin=false", mode, me)
		}

		// Sees only infra (member), not web.
		var teams struct{ Teams []Team }
		_, tb := doMode(root, admins, mode, "GET", TeamsPath, nil)
		json.Unmarshal(tb, &teams)
		if len(teams.Teams) != 1 || teams.Teams[0].ID != "infra" {
			t.Errorf("GET /v1/teams mode=%q: got %+v, want only infra", mode, teams.Teams)
		}
		if c, _ := doMode(root, admins, mode, "GET", TeamsPath+"/web", nil); c != 404 {
			t.Errorf("GET /v1/teams/web mode=%q: got %d, want 404", mode, c)
		}

		// Hub-admin-only and team-admin actions are refused when admin mode is off.
		if c, _ := doMode(root, admins, mode, "GET", UsersPath, nil); c != 403 {
			t.Errorf("GET /v1/users mode=%q: got %d, want 403", mode, c)
		}
		if c, _ := doMode(root, admins, mode, "PUT", TeamsPath+"/infra/repos/bwsalmon/extra", nil); c != 403 {
			t.Errorf("PUT repo mode=%q: got %d, want 403", mode, c)
		}
		if c, _ := doMode(root, admins, mode, "DELETE", TeamsPath+"/infra", nil); c != 403 {
			t.Errorf("DELETE /v1/teams/infra mode=%q: got %d, want 403", mode, c)
		}

		// Node tokens list only root's own token, not bob's.
		var toks struct {
			NodeTokens []NodeToken `json:"node_tokens"`
		}
		_, tokB := doMode(root, admins, mode, "GET", NodeTokensPath, nil)
		json.Unmarshal(tokB, &toks)
		if len(toks.NodeTokens) != 1 || toks.NodeTokens[0].Node != "ws-root" {
			t.Errorf("GET /v1/node-tokens mode=%q: got %+v, want only ws-root", mode, toks.NodeTokens)
		}
	}

	// 3. Non-admin sending X-Silo-Admin-Mode: on gains nothing.
	for _, mode := range []string{"", "on", "off"} {
		code, b := doMode(alice, "", mode, "GET", MePath, nil)
		if code != 200 {
			t.Fatalf("alice GET /v1/me mode=%q: %d %s", mode, code, b)
		}
		var me meResp
		json.Unmarshal(b, &me)
		if me.CanHubAdmin || me.HubAdmin {
			t.Errorf("alice GET /v1/me mode=%q: got %+v, want CanHubAdmin=false HubAdmin=false", mode, me)
		}
		if c, _ := doMode(alice, "", mode, "GET", UsersPath, nil); c != 403 {
			t.Errorf("alice GET /v1/users mode=%q: got %d, want 403", mode, c)
		}
		if c, _ := doMode(alice, "", mode, "GET", TeamsPath+"/web", nil); c != 404 {
			t.Errorf("alice GET /v1/teams/web mode=%q: got %d, want 404", mode, c)
		}
	}
}

func TestAPIViewerProposerAndSettings(t *testing.T) {
	h := newTestHub(t)
	const root, alice, vera, pete = "root@example.com", "alice@example.com", "vera@example.com", "pete@example.com"
	const admins = "hub-admins@example.com"

	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "infra", "name": "Infra"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/members/user:"+alice, map[string]string{"role": "admin"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/repos/bwsalmon/silo", nil, admins)

	// Team admin adds viewer and proposer.
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/members/user:"+vera, map[string]string{"role": "viewer"})
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/members/user:"+pete, map[string]string{"role": "proposer"})

	// Both viewer and proposer can read GET /v1/teams/infra and GET /v1/teams/infra/settings,
	// but neither can modify members or settings.
	for _, user := range []string{vera, pete} {
		h.want(t, 200, user, "GET", TeamsPath+"/infra", nil)
		b := h.want(t, 200, user, "GET", TeamsPath+"/infra/settings", nil)
		var set Settings
		if err := json.Unmarshal(b, &set); err != nil {
			t.Fatal(err)
		}
		if !set.AnyoneMayPropose || set.RequireApproval {
			t.Errorf("%s default settings: %+v", user, set)
		}
		h.want(t, 403, user, "PUT", TeamsPath+"/infra/settings", map[string]any{"anyone_may_propose": false})
		h.want(t, 403, user, "PUT", TeamsPath+"/infra/members/user:other@example.com", map[string]string{"role": "viewer"})
	}

	// Team admin updates anyone_may_propose and require_approval.
	b := h.want(t, 200, alice, "PUT", TeamsPath+"/infra/settings", map[string]any{
		"lost_requeue_after_seconds": 300,
		"anyone_may_propose":         false,
		"require_approval":           true,
	})
	var got Settings
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.LostRequeueAfter != 5*time.Minute || got.AnyoneMayPropose || !got.RequireApproval {
		t.Errorf("PUT settings: %+v", got)
	}

	// Neither viewer nor proposer can register a node for infra's repo.
	for _, user := range []string{vera, pete} {
		var r struct{ Token string }
		json.Unmarshal(h.want(t, 201, user, "POST", NodeTokensPath, map[string]string{"node": "ws-" + user[:4]}), &r)
		cl := &rendezvous.Client{
			Hub:   h.url,
			Token: func(context.Context) (string, error) { return r.Token, nil },
			Node:  registry.Node{ID: "ws-" + user[:4], Repos: []string{"bwsalmon/silo"}, Slots: 1},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := cl.Connect(ctx)
		cancel()
		if !errors.Is(err, rendezvous.ErrRefused) {
			t.Errorf("%s connecting node for infra repo: %v, want ErrRefused", user, err)
		}
	}
}

func TestUserThemeInboxPreferencesAndDirectoryRoles(t *testing.T) {
	h := newTestHub(t)
	const (
		root     = "root@example.com"
		alice    = "alice@example.com"    // owner of ws-infra, admin of infra
		dave     = "dave@example.com"     // member of infra
		vera     = "vera@example.com"     // viewer of infra
		bob      = "bob@example.com"      // owner of ws-web, admin of web
		outsider = "outsider@example.com" // in no team, gets proposer on infra via anyone_may_propose
		admins   = "hub-admins@example.com"
	)

	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "infra", "name": "Infra"}, admins)
	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "web", "name": "Web"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/members/user:"+alice, map[string]string{"role": "admin"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/members/user:"+dave, map[string]string{"role": "member"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/members/user:"+vera, map[string]string{"role": "viewer"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/web/members/user:"+bob, map[string]string{"role": "admin"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/repos/bwsalmon/silo", nil, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/web/repos/bwsalmon/web", nil, admins)

	// Set web's anyone_may_propose = false, infra's anyone_may_propose = true & require_approval = true.
	h.want(t, 200, alice, "PUT", TeamsPath+"/infra/settings", map[string]any{
		"anyone_may_propose": true,
		"require_approval":   true,
	})
	h.want(t, 200, bob, "PUT", TeamsPath+"/web/settings", map[string]any{
		"anyone_may_propose": false,
		"require_approval":   false,
	})

	// Register ws-infra (offers UI with inferred team "infra") and ws-web (offers UI for "web").
	nodeInfra := registry.Node{
		ID:    "ws-infra",
		Repos: []string{"bwsalmon/silo"},
		Slots: 2,
		UI:    &registry.NodeUI{Offered: true},
	}
	if err := h.store.ValidateNodeUI(context.Background(), alice, &nodeInfra); err != nil {
		t.Fatal(err)
	}
	leaseInfra, err := h.rv.Registry.Register(nodeInfra, alice)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseInfra.Close()

	nodeWeb := registry.Node{
		ID:    "ws-web",
		Repos: []string{"bwsalmon/web"},
		Slots: 2,
		UI:    &registry.NodeUI{Offered: true, Team: "web"},
	}
	if err := h.store.ValidateNodeUI(context.Background(), bob, &nodeWeb); err != nil {
		t.Fatal(err)
	}
	leaseWeb, err := h.rv.Registry.Register(nodeWeb, bob)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseWeb.Close()

	// 1. Theme defaults to "light" and updates via PUT /v1/me/theme.
	var me struct {
		Theme           string   `json:"theme"`
		InboxNodes      []string `json:"inbox_nodes"`
		InboxConfigured bool     `json:"inbox_configured"`
	}
	json.Unmarshal(h.want(t, 200, dave, "GET", MePath, nil), &me)
	if me.Theme != "light" || me.InboxConfigured || len(me.InboxNodes) != 1 || me.InboxNodes[0] != "ws-infra" {
		t.Fatalf("dave initial GET /v1/me = %+v", me)
	}
	h.want(t, 400, dave, "PUT", MePath+"/theme", map[string]string{"theme": "auto"})
	h.want(t, 200, dave, "PUT", MePath+"/theme", map[string]string{"theme": "dark"})
	json.Unmarshal(h.want(t, 200, dave, "GET", MePath, nil), &me)
	if me.Theme != "dark" {
		t.Fatalf("dave GET /v1/me after PUT theme = %q, want dark", me.Theme)
	}

	// 2. GET /v1/nodes returns viewer_role and require_approval; outsider sees ws-infra as proposer.
	var nodesResp struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	json.Unmarshal(h.want(t, 200, dave, "GET", rendezvous.DirectoryPath, nil), &nodesResp)
	if len(nodesResp.Nodes) != 1 || nodesResp.Nodes[0].ID != "ws-infra" || nodesResp.Nodes[0].ViewerRole != "member" || !nodesResp.Nodes[0].RequireApproval {
		t.Fatalf("dave GET /v1/nodes = %+v", nodesResp.Nodes)
	}
	json.Unmarshal(h.want(t, 200, outsider, "GET", rendezvous.DirectoryPath, nil), &nodesResp)
	if len(nodesResp.Nodes) != 1 || nodesResp.Nodes[0].ID != "ws-infra" || nodesResp.Nodes[0].ViewerRole != "proposer" || !nodesResp.Nodes[0].RequireApproval {
		t.Fatalf("outsider GET /v1/nodes = %+v; want ws-infra with viewer_role=proposer", nodesResp.Nodes)
	}

	// 3. GET/PUT /v1/me/inbox: viewer defaults to empty inbox_nodes, can opt in to ws-infra, cannot select ws-web (403).
	var inboxPrefs struct {
		Nodes      []string `json:"nodes"`
		Configured bool     `json:"configured"`
	}
	json.Unmarshal(h.want(t, 200, vera, "GET", MePath+"/inbox", nil), &inboxPrefs)
	if inboxPrefs.Configured || len(inboxPrefs.Nodes) != 0 {
		t.Fatalf("vera default inbox prefs = %+v, want empty unconfigured", inboxPrefs)
	}
	h.want(t, 403, vera, "PUT", MePath+"/inbox", map[string]any{"nodes": []string{"ws-web"}})
	json.Unmarshal(h.want(t, 200, vera, "PUT", MePath+"/inbox", map[string]any{"nodes": []string{"ws-infra"}}), &inboxPrefs)
	if !inboxPrefs.Configured || len(inboxPrefs.Nodes) != 1 || inboxPrefs.Nodes[0] != "ws-infra" {
		t.Fatalf("vera saved inbox prefs = %+v", inboxPrefs)
	}
}

func TestAcceptedUsersAndDynamicSysAdmins(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	const root, alice, bob, stranger = "root@example.com", "alice@example.com", "bob@example.com", "stranger@example.com"
	if err := s.SeedUsers(ctx, []string{"user:" + root}, []string{alice}); err != nil {
		t.Fatal(err)
	}

	api := &API{
		Store:                s,
		Auth:                 headerAuth{},
		EnforceAcceptedUsers: true,
		HubURL:               "https://hub.example.com",
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	h := &testHub{store: s, url: ts.URL}

	// 1. Unaccepted user is refused with 403 Forbidden.
	_, b := h.do(t, stranger, "GET", MePath, nil)
	h.want(t, http.StatusForbidden, stranger, "GET", MePath, nil)
	if !strings.Contains(string(b), "user is not on the hub accepted users list") {
		t.Fatalf("unexpected 403 body: %s", b)
	}

	// 2. Seeded accepted user (alice) can sign in, but is not a sys-admin.
	var me struct {
		Email       string `json:"email"`
		HubAdmin    bool   `json:"hub_admin"`
		CanHubAdmin bool   `json:"can_hub_admin"`
	}
	json.Unmarshal(h.want(t, http.StatusOK, alice, "GET", MePath, nil), &me)
	if me.Email != alice || me.HubAdmin || me.CanHubAdmin {
		t.Fatalf("alice /v1/me = %+v", me)
	}
	h.want(t, http.StatusForbidden, alice, "GET", UsersPath, nil)
	h.want(t, http.StatusForbidden, alice, "PUT", UsersPath+"/"+bob, map[string]bool{"sys_admin": false})

	// 3. Seeded sys-admin (root) manages accepted users and dynamic sys-admins.
	var usersResp struct {
		Users []AcceptedUser `json:"users"`
	}
	json.Unmarshal(h.want(t, http.StatusOK, root, "GET", UsersPath, nil), &usersResp)
	if len(usersResp.Users) != 2 {
		t.Fatalf("initial users = %+v, want 2", usersResp.Users)
	}

	// Refuse demoting or deleting the last sys-admin (409 Conflict).
	h.want(t, http.StatusConflict, root, "PUT", UsersPath+"/"+root, map[string]bool{"sys_admin": false})
	h.want(t, http.StatusConflict, root, "DELETE", UsersPath+"/"+root, nil)

	// Root adds bob as an accepted user and promotes alice to sys-admin.
	h.want(t, http.StatusOK, root, "PUT", UsersPath+"/"+bob, map[string]bool{"sys_admin": false})
	h.want(t, http.StatusOK, root, "PUT", UsersPath+"/"+alice, map[string]bool{"sys_admin": true})

	// Alice now has dynamic sys-admin rights and can manage users.
	json.Unmarshal(h.want(t, http.StatusOK, alice, "GET", MePath, nil), &me)
	if !me.HubAdmin || !me.CanHubAdmin {
		t.Fatalf("promoted alice /v1/me = %+v, want HubAdmin=true", me)
	}
	// Alice demotes root (allowed because alice is also a sys-admin), then removes bob.
	h.want(t, http.StatusOK, alice, "PUT", UsersPath+"/"+root, map[string]bool{"sys_admin": false})
	h.want(t, http.StatusNoContent, alice, "DELETE", UsersPath+"/"+bob, nil)
	h.want(t, http.StatusForbidden, bob, "GET", MePath, nil)
	// Alice is now the last sys-admin and cannot demote or delete herself.
	h.want(t, http.StatusConflict, alice, "PUT", UsersPath+"/"+alice, map[string]bool{"sys_admin": false})
	h.want(t, http.StatusConflict, alice, "DELETE", UsersPath+"/"+alice, nil)
}

func TestDeploymentsCanonicalURLAndHostReplacement(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	var reclaimedNode, reclaimedReason string
	var replayedNode string
	rv := &rendezvous.Server{
		Registry:     registry.New(),
		Authenticate: s.Authenticate,
		Admit:        s.Admit,
		ValidateUI:   s.ValidateNodeUI,
		Heartbeat:    time.Second,
		HubURL:       "https://hub.example.com",
	}
	api := &API{
		Store:      s,
		Auth:       headerAuth{},
		HubURL:     "https://hub.example.com",
		Disconnect: rv.Disconnect,
		Unbind:     rv.Registry.Unbind,
		Registry:   rv.Registry,
		ReclaimTasks: func(_ context.Context, nodeID, reason string) ([]string, error) {
			reclaimedNode, reclaimedReason = nodeID, reason
			return []string{"t-1", "t-2"}, nil
		},
		ReplayGrainfiles: func(_ context.Context, nodeID string) (int, error) {
			replayedNode = nodeID
			return 3, nil
		},
	}
	rv.Visible = api.Visible
	rv.ResolveEntries = api.ResolveEntries
	m := http.NewServeMux()
	m.Handle(rendezvous.ConnectPath, rv.Handler())
	m.Handle(rendezvous.DirectoryPath, rv.Handler())
	m.Handle("/", api.Handler())
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	h := &testHub{store: s, rv: rv, url: ts.URL}

	const alice, bob, carol = "alice@example.com", "bob@example.com", "carol@example.com"

	// 1. Self-service deployment creation via POST /v1/deployments defaults name to id and sets canonical URL.
	var dep Team
	json.Unmarshal(h.want(t, http.StatusCreated, alice, "POST", DeploymentsPath, map[string]string{"id": "payments-prod"}), &dep)
	wantURL := "https://hub.example.com/deployments/payments-prod"
	if dep.ID != "payments-prod" || dep.Name != "payments-prod" || dep.URL != wantURL || dep.RecipientURL != wantURL || dep.Members["user:"+alice] != Admin {
		t.Fatalf("created deployment = %+v, want url=%s", dep, wantURL)
	}
	// Name is taken (409 Conflict).
	h.want(t, http.StatusConflict, bob, "POST", DeploymentsPath, map[string]string{"id": "payments-prod"})

	// 2. Alice claims an open repo and adds Bob as a second deployment admin.
	h.want(t, http.StatusNoContent, alice, "PUT", DeploymentsPath+"/payments-prod/repos/acme/payments", nil)
	h.want(t, http.StatusNoContent, alice, "PUT", DeploymentsPath+"/payments-prod/members/user:"+bob, map[string]string{"role": "admin"})

	// Carol (not an admin of payments-prod) cannot mint a token for payments-prod.
	h.want(t, http.StatusConflict, carol, "POST", NodeTokensPath, map[string]string{"node": "payments-prod"})
	h.want(t, http.StatusNotFound, carol, "POST", DeploymentsPath+"/payments-prod/token", nil)

	// Alice mints initial deployment token.
	var mint1 struct {
		Token     string    `json:"token"`
		NodeToken NodeToken `json:"node_token"`
	}
	json.Unmarshal(h.want(t, http.StatusCreated, alice, "POST", DeploymentsPath+"/payments-prod/token", nil), &mint1)
	if mint1.Token == "" || mint1.NodeToken.Node != "payments-prod" {
		t.Fatalf("alice mint = %+v", mint1)
	}

	// Bob (another admin of payments-prod) can replace the host and mint a new token for payments-prod!
	var mint2 struct {
		Token          string    `json:"token"`
		NodeToken      NodeToken `json:"node_token"`
		ReplacedHost   bool      `json:"replaced_host"`
		ReclaimedTasks []string  `json:"reclaimed_tasks"`
		ReplayedEvents int       `json:"replayed_events"`
	}
	json.Unmarshal(h.want(t, http.StatusCreated, bob, "POST", DeploymentsPath+"/payments-prod/replace-host", nil), &mint2)
	if !mint2.ReplacedHost || len(mint2.ReclaimedTasks) != 2 || mint2.ReplayedEvents != 3 || reclaimedNode != "payments-prod" || replayedNode != "payments-prod" || reclaimedReason == "" {
		t.Fatalf("bob replace-host = %+v (reclaimedNode=%q replayedNode=%q)", mint2, reclaimedNode, replayedNode)
	}
	if _, _, err := s.VerifyNodeToken(ctx, mint1.Token); err == nil {
		t.Fatal("prior token was not revoked by replace-host")
	}
	if _, node, err := s.VerifyNodeToken(ctx, mint2.Token); err != nil || node != "payments-prod" {
		t.Fatalf("replacement token verify: node=%q err=%v", node, err)
	}
}

func TestConcurrentLastSysAdminMutualDemoteAndDeleteRace(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const adminA, adminB, regular = "admin-a@example.com", "admin-b@example.com", "regular@example.com"

		// Seed adminA via HubAdmins (just as server.New does) and regular as an accepted user.
		if err := s.SeedUsers(ctx, []string{"user:" + adminA}, []string{regular}); err != nil {
			t.Fatal(err)
		}

		api := &API{
			Store:                s,
			Auth:                 headerAuth{},
			HubAdmins:            []string{"user:" + adminA},
			EnforceAcceptedUsers: true,
			HubURL:               "https://hub.example.com",
		}
		ts := httptest.NewServer(api.Handler())
		t.Cleanup(ts.Close)
		h := &testHub{store: s, url: ts.URL}

		assertAtLeastOneSysAdmin := func(iter int, phase string) {
			t.Helper()
			users, err := s.Users(ctx)
			if err != nil {
				t.Fatalf("iter %d (%s): Users: %v", iter, phase, err)
			}
			sysCount := 0
			for _, u := range users {
				if u.SysAdmin {
					sysCount++
				}
			}
			if sysCount < 1 {
				t.Fatalf("iter %d (%s): zero sys admins remaining in accepted_users: %+v", iter, phase, users)
			}
		}

		for iter := 0; iter < 25; iter++ {
			// Ensure both adminA and adminB are sys_admin = true before each race.
			if err := s.SetUser(ctx, adminA, true, "bootstrap"); err != nil {
				t.Fatalf("iter %d reset adminA: %v", iter, err)
			}
			if err := s.SetUser(ctx, adminB, true, adminA); err != nil {
				t.Fatalf("iter %d reset adminB: %v", iter, err)
			}

			var (
				wg         sync.WaitGroup
				codeA      int
				codeB      int
				startGate  = make(chan struct{})
				deleteMode = iter%3 == 1
				mixedMode  = iter%3 == 2
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-startGate
				if deleteMode {
					codeA, _ = h.do(t, adminA, "DELETE", UsersPath+"/"+adminB, nil)
				} else {
					codeA, _ = h.do(t, adminA, "PUT", UsersPath+"/"+adminB, map[string]bool{"sys_admin": false})
				}
			}()
			go func() {
				defer wg.Done()
				<-startGate
				if deleteMode || mixedMode {
					codeB, _ = h.do(t, adminB, "DELETE", UsersPath+"/"+adminA, nil)
				} else {
					codeB, _ = h.do(t, adminB, "PUT", UsersPath+"/"+adminA, map[string]bool{"sys_admin": false})
				}
			}()
			close(startGate)
			wg.Wait()

			// Exactly one must fail with 409 Conflict (ErrLastSysAdmin) or 403 Forbidden
			// (if its own admin rights were revoked before its request ran), and at least
			// one sys admin must remain.
			if (codeA == http.StatusOK || codeA == http.StatusNoContent) &&
				(codeB == http.StatusOK || codeB == http.StatusNoContent) {
				t.Fatalf("iter %d: both concurrent sys-admin removals succeeded (codeA=%d, codeB=%d)", iter, codeA, codeB)
			}
			if codeA != http.StatusConflict && codeA != http.StatusForbidden &&
				codeB != http.StatusConflict && codeB != http.StatusForbidden {
				t.Fatalf("iter %d: expected at least one 409/403, got codeA=%d codeB=%d", iter, codeA, codeB)
			}
			assertAtLeastOneSysAdmin(iter, "http-race")

			// If adminA (originally in HubAdmins) was deleted during the race, verify
			// adminA is immediately blocked with 403 Forbidden despite being in api.HubAdmins.
			users, _ := s.Users(ctx)
			hasAdminA := false
			for _, u := range users {
				if u.Email == adminA {
					hasAdminA = true
				}
			}
			if !hasAdminA {
				h.want(t, http.StatusForbidden, adminA, "GET", MePath, nil)
			}
		}

		// Explicitly verify removing regular and seeded adminA blocks their subsequent API requests with 403 Forbidden.
		if err := s.SetUser(ctx, adminA, true, "bootstrap"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetUser(ctx, adminB, true, adminA); err != nil {
			t.Fatal(err)
		}
		h.want(t, http.StatusOK, regular, "GET", MePath, nil)
		h.want(t, http.StatusNoContent, adminB, "DELETE", UsersPath+"/"+regular, nil)
		h.want(t, http.StatusForbidden, regular, "GET", MePath, nil)

		// Demote and then remove adminA (who was seeded from HubAdmins) via adminB.
		h.want(t, http.StatusOK, adminB, "PUT", UsersPath+"/"+adminA, map[string]bool{"sys_admin": false})
		h.want(t, http.StatusForbidden, adminA, "GET", UsersPath, nil)
		h.want(t, http.StatusNoContent, adminB, "DELETE", UsersPath+"/"+adminA, nil)
		h.want(t, http.StatusForbidden, adminA, "GET", MePath, nil)
	})
}

func TestConcurrentSelfServiceDeploymentClaimAndGrainfilesReplayRace(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const root = "root@example.com"
		users := []string{"alice@example.com", "bob@example.com", "carol@example.com", "dave@example.com"}
		if err := s.SeedUsers(ctx, []string{"user:" + root}, users); err != nil {
			t.Fatal(err)
		}

		gfStore, err := grainfiles.Open(ctx, "sqlite::memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gfStore.Close() })
		gfStore.HubURL = "https://hub.example.com"

		rv := &rendezvous.Server{
			Registry:     registry.New(),
			Authenticate: s.Authenticate,
			Admit:        s.Admit,
			ValidateUI:   s.ValidateNodeUI,
			Grainfiles:   gfStore,
			Heartbeat:    time.Second,
			HubURL:       "https://hub.example.com",
			Logf:         testlog.Logf(t),
		}
		var reclaimedCount atomic.Int64
		api := &API{
			Store:                s,
			Auth:                 headerAuth{},
			HubAdmins:            []string{"user:" + root},
			EnforceAcceptedUsers: true,
			HubURL:               "https://hub.example.com",
			Disconnect:           rv.Disconnect,
			Unbind:               rv.Registry.Unbind,
			Registry:             rv.Registry,
			ReclaimTasks: func(_ context.Context, nodeID, _ string) ([]string, error) {
				reclaimedCount.Add(1)
				return []string{"t-reclaimed-" + nodeID}, nil
			},
			ReplayGrainfiles: func(ctx context.Context, nodeID string) (int, error) {
				evs, err := gfStore.ReplayForNode(ctx, nodeID)
				if err == nil && len(evs) > 0 {
					rv.NotifyGrainfiles(evs...)
				}
				return len(evs), err
			},
		}
		rv.Visible = api.Visible
		rv.ResolveEntries = api.ResolveEntries

		m := http.NewServeMux()
		m.Handle(rendezvous.ConnectPath, rv.Handler())
		m.Handle(rendezvous.DirectoryPath, rv.Handler())
		m.Handle("/", api.Handler())
		ts := httptest.NewServer(m)
		t.Cleanup(ts.Close)
		h := &testHub{store: s, rv: rv, url: ts.URL}

		// 1. Race accepted users concurrently claiming the same deployment name via POST /v1/deployments.
		for iter := 0; iter < 20; iter++ {
			sharedID := fmt.Sprintf("shared-name-%d", iter)
			var (
				wg        sync.WaitGroup
				startGate = make(chan struct{})
				codes     = make([]int, len(users))
				bodies    = make([][]byte, len(users))
			)
			for i, u := range users {
				wg.Add(1)
				go func(idx int, userEmail string) {
					defer wg.Done()
					<-startGate
					codes[idx], bodies[idx] = h.do(t, userEmail, "POST", DeploymentsPath, map[string]string{"id": sharedID})
				}(i, u)
			}
			close(startGate)
			wg.Wait()

			createdCount := 0
			conflictCount := 0
			var winner string
			for i, code := range codes {
				switch code {
				case http.StatusCreated:
					createdCount++
					winner = users[i]
					var dep Team
					if err := json.Unmarshal(bodies[i], &dep); err != nil {
						t.Fatalf("iter %d unmarshal created deployment: %v", iter, err)
					}
					wantURL := "https://hub.example.com/deployments/" + sharedID
					if dep.ID != sharedID || dep.URL != wantURL || dep.Members["user:"+winner] != Admin {
						t.Fatalf("iter %d winner %s deployment = %+v", iter, winner, dep)
					}
				case http.StatusConflict:
					conflictCount++
				default:
					t.Fatalf("iter %d user %s unexpected status %d: %s", iter, users[i], code, bodies[i])
				}
			}
			if createdCount != 1 || conflictCount != len(users)-1 {
				t.Fatalf("iter %d: got created=%d conflict=%d, want 1 and %d", iter, createdCount, conflictCount, len(users)-1)
			}
		}

		// 2. Verify host replacement cleanly replays standing .process.grain.md and uncompleted
		// .task.grain.md notifications for https://<hub-host>/deployments/{id}, while skipping
		// already-completed .task.grain.md files.
		const depID = "shared-name-0"
		const depURL = "https://hub.example.com/deployments/" + depID
		doneTask := &grainfiles.Grainfile{
			Kind:      grainfiles.KindTask,
			Repo:      "https://github.com/acme/caller",
			Ref:       "refs/heads/main",
			Commit:    "c100",
			Path:      ".grain/tasks/2026-10-19-done.task.grain.md",
			Title:     "Already completed task",
			Recipient: depURL,
		}
		if _, err := gfStore.IndexGrainfile(ctx, doneTask); err != nil {
			t.Fatalf("IndexGrainfile(doneTask): %v", err)
		}
		if _, err := gfStore.RecordDependencyCompleted(ctx, "https://github.com/acme/caller/blob/main/.grain/tasks/2026-10-19-done.task.grain.md", "c100", "done", ""); err != nil {
			t.Fatalf("RecordDependencyCompleted: %v", err)
		}
		openTask := &grainfiles.Grainfile{
			Kind:      grainfiles.KindTask,
			Repo:      "https://github.com/acme/caller",
			Ref:       "refs/heads/main",
			Commit:    "c101",
			Path:      ".grain/tasks/2026-10-19-open.task.grain.md",
			Title:     "Uncompleted task",
			Recipient: depURL,
		}
		if _, err := gfStore.IndexGrainfile(ctx, openTask); err != nil {
			t.Fatalf("IndexGrainfile(openTask): %v", err)
		}
		standingProc, err := grainfiles.Parse(".grain/processes/gate.process.grain.md", []byte("---\ngrainfile: v1\ntitle: Standing gate\ngate: true\nwhen:\n  pull_request:\n    branches: [\"main\"]\n---\n"))
		if err != nil {
			t.Fatalf("Parse(standingProc): %v", err)
		}
		standingProc.Repo = "https://github.com/acme/caller"
		standingProc.Ref = "refs/heads/main"
		standingProc.Commit = "c102"
		standingProc.Recipient = depURL
		if _, err := gfStore.IndexGrainfile(ctx, standingProc); err != nil {
			t.Fatalf("IndexGrainfile(standingProc): %v", err)
		}

		// Old host subscribes and catches up past all existing events.
		if _, err := gfStore.SetSubscriptions(ctx, grainfiles.Subscriptions{
			NodeID: depID,
			Owner:  users[0],
		}); err != nil {
			t.Fatalf("SetSubscriptions: %v", err)
		}
		if _, _, err := gfStore.CatchupNode(ctx, depID, true); err != nil {
			t.Fatalf("initial CatchupNode: %v", err)
		}

		// Trigger POST /v1/deployments/{id}/token {"replace_host": true} as root/admin.
		var replaceResp struct {
			Token          string   `json:"token"`
			ReplacedHost   bool     `json:"replaced_host"`
			ReclaimedTasks []string `json:"reclaimed_tasks"`
			ReplayedEvents int      `json:"replayed_events"`
		}
		json.Unmarshal(h.want(t, http.StatusCreated, root, "POST", DeploymentsPath+"/"+depID+"/token", map[string]bool{"replace_host": true}), &replaceResp)
		if !replaceResp.ReplacedHost || replaceResp.ReplayedEvents != 2 || len(replaceResp.ReclaimedTasks) != 1 {
			t.Fatalf("replace-host response = %+v, want ReplayedEvents=2 ReclaimedTasks=1", replaceResp)
		}

		// Replacement host catches up and receives ONLY the standing process and uncompleted task.
		replayedEvs, newCur, err := gfStore.CatchupNode(ctx, depID, true)
		if err != nil {
			t.Fatalf("replacement CatchupNode: %v", err)
		}
		if len(replayedEvs) != 2 || newCur == 0 {
			t.Fatalf("replacement CatchupNode got %d events (cursor=%d), want 2: %+v", len(replayedEvs), newCur, replayedEvs)
		}
		gotTypes := map[string]string{}
		for _, ev := range replayedEvs {
			gotTypes[ev.Path] = ev.Type
		}
		if gotTypes[".grain/tasks/2026-10-19-open.task.grain.md"] != grainfiles.EventTaskReady ||
			gotTypes[".grain/processes/gate.process.grain.md"] != grainfiles.EventProcessUpdated {
			t.Fatalf("unexpected replayed event paths/types: %+v", gotTypes)
		}
	})
}
