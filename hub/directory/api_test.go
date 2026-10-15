package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
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
		Heartbeat:    time.Second,
		Logf:         t.Logf,
	}
	api := &API{Store: s, Auth: headerAuth{}, HubAdmins: []string{"hub-admins@example.com"}, Disconnect: rv.Disconnect}
	rv.Visible = api.Visible
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
	h.want(t, 403, alice, "POST", TeamsPath, map[string]string{"id": "infra"})
	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "infra", "name": "Infra"}, admins)
	h.want(t, 201, root, "POST", TeamsPath, map[string]string{"id": "web"}, admins)
	h.want(t, 409, root, "POST", TeamsPath, map[string]string{"id": "web"}, admins)
	h.want(t, 400, root, "POST", TeamsPath, map[string]string{"id": "Bad Team"}, admins)
	h.want(t, 400, root, "POST", TeamsPath, map[string]any{"id": "x", "extra": 1}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/infra/members/"+alice, map[string]string{"role": "admin"}, admins)
	h.want(t, 204, root, "PUT", TeamsPath+"/web/members/"+bob, map[string]string{"role": "admin"}, admins)

	// Alice administers infra, and only infra.
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/repos/bwsalmon/silo", nil)
	h.want(t, 204, alice, "PUT", TeamsPath+"/infra/members/carol@example.com", map[string]string{"role": "member"})
	h.want(t, 404, alice, "PUT", TeamsPath+"/web/repos/bwsalmon/x", nil)
	h.want(t, 403, "carol@example.com", "PUT", TeamsPath+"/infra/repos/bwsalmon/x", nil)
	h.want(t, 403, alice, "DELETE", TeamsPath+"/infra", nil)
	h.want(t, 204, bob, "PUT", TeamsPath+"/web/repos/bwsalmon/web", nil)
	h.want(t, 409, bob, "PUT", TeamsPath+"/web/repos/bwsalmon/silo", nil) // infra's

	var teams struct{ Teams []Team }
	json.Unmarshal(h.want(t, 200, alice, "GET", TeamsPath, nil), &teams)
	if len(teams.Teams) != 1 || teams.Teams[0].ID != "infra" || len(teams.Teams[0].Members) != 2 {
		t.Errorf("alice's teams %+v", teams)
	}
	json.Unmarshal(h.want(t, 200, root, "GET", TeamsPath, nil, admins), &teams)
	if len(teams.Teams) != 2 {
		t.Errorf("hub admin's teams %+v", teams)
	}
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
}
