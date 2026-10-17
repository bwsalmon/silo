package security_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// revokeBound is how soon a revoked token's connection must end.
// docs/hub.md §5.2 says "at once" (Server.Disconnect, called by the
// revoke itself); the bound allows for a loaded CI machine.
const revokeBound = 5 * time.Second

func dial(t *testing.T, hb *hub, token, id string, repos ...string) (*rendezvous.Conn, error) {
	t.Helper()
	cl := &rendezvous.Client{
		Hub:   hb.url,
		Token: func(context.Context) (string, error) { return token, nil },
		Node:  registry.Node{ID: id, Repos: repos, Slots: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := cl.Connect(ctx)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

// TestRevokedTokenDisconnects revokes a connected node's token, and
// then mints a fresh one over another live connection: each ends the
// connection it covered within revokeBound, and the old token is
// refused from then on.
func TestRevokedTokenDisconnects(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	for _, how := range []string{"revoke", "mint again", "release the node ID"} {
		t.Run(how, func(t *testing.T) {
			node := "ws-" + strings.ReplaceAll(how, " ", "-")
			tok, id := hb.mint(alice, node)
			c, err := dial(t, hb, tok, node, repoA)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			switch how {
			case "revoke":
				hb.must(hb.user(alice), "DELETE", "/v1/node-tokens/"+id, nil, http.StatusNoContent, nil)
			case "mint again":
				hb.mint(alice, node)
			default:
				hb.must(hb.user(alice), "DELETE", "/v1/node-tokens/nodes/"+node, nil, http.StatusNoContent, nil)
			}
			select {
			case <-c.Done():
				t.Logf("disconnected %v after the %s", time.Since(start), how)
			case <-time.After(revokeBound):
				t.Fatalf("the connection outlived the %s by %v", how, revokeBound)
			}
			if _, err := dial(t, hb, tok, node, repoA); err == nil {
				t.Errorf("the token connected again after the %s", how)
			}
		})
	}
}

// TestNodeIDOwnership: a node ID belongs to the first owner to mint for
// it (SA1-H1). Another owner can mint no token for it, a token for one
// ID cannot register another, and the registry itself refuses a second
// owner (registry.ErrOwner).
func TestNodeIDOwnership(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tokA, _ := hb.mint(alice, "ws-a")
	if code, b, _ := hb.do(hb.user(bob), "POST", "/v1/node-tokens", map[string]string{"node": "ws-a"}); code != http.StatusConflict {
		t.Errorf("bob minting for alice's ws-a: %d %s, want 409", code, b)
	}
	tokB, _ := hb.mint(bob, "ws-b")
	if _, err := dial(t, hb, tokB, "ws-a", repoB); err == nil {
		t.Error("bob's ws-b token registered ws-a")
	}
	if _, err := dial(t, hb, tokA, "ws-a", repoA); err != nil {
		t.Fatalf("alice's own node: %v", err)
	}

	r := registry.New()
	l, err := r.Register(registry.Node{ID: "ws-x", Repos: []string{repoA}, Slots: 1}, alice)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := r.Register(registry.Node{ID: "ws-x", Repos: []string{repoB}, Slots: 1}, bob); !errors.Is(err, registry.ErrOwner) {
		t.Errorf("a second owner registering ws-x: %v, want ErrOwner", err)
	}
}

// TestNodeCannotWriteForAnotherTask: a node posts events and results
// only for a task the hub placed on it. Another team's node, another
// node of the same owner, and the task's own node on another task's
// stream are all refused, and nothing they sent is kept or shown.
func TestNodeCannotWriteForAnotherTask(t *testing.T) {
	w := newWorld(t)
	hb := w.hb
	_, prior := hb.replay(hb.user(alice), w.runA.ID, 0, 100)

	forged := func(seq int64) []string {
		return []string{
			event(w.runA.ID, seq, events.TypeStatus, `{"phase":"running","state":"info","message":"FORGED"}`),
			event(w.runA.ID, seq+1, events.TypeResult, `{"state":"done","summary":"FORGED"}`),
		}
	}
	// Team B's node.
	w.nodeB.events(forged(100)...).Close()
	// Another node of alice's, with a token of her own.
	tok, _ := hb.mint(alice, "ws-a2")
	other := connectNode(t, hb, tok, "ws-a2", repoA)
	other.events(forged(200)...).Close()
	// The task's own node, on a stream it opened for team B's task:
	// refused at the first line, so the second is never read.
	w.nodeA.events(append([]string{event(w.runB.ID, 1, events.TypeStatus, `{"phase":"running","state":"info","message":"FORGED"}`)}, forged(300)...)...).Close()

	// A result for team A's task written down a stream of the node's
	// own making is not a task stream: the hub opens those.
	up := w.nodeB.events(`{"type":"result","state":"done","reason":"FORGED"}`)
	up.Close()

	eventually(t, "the refusals logged", func() bool {
		return strings.Count(hb.logs.all(), "refused node") >= 3
	})
	time.Sleep(500 * time.Millisecond)
	_, after := hb.replay(hb.user(alice), w.runA.ID, 0, len(prior)+1)
	if len(after) != len(prior) {
		t.Errorf("team A's task had %d events, has %d after the forgeries", len(prior), len(after))
	}
	for _, e := range after {
		if e.Status != nil && strings.Contains(e.Status.Message, "FORGED") || e.Result != nil {
			t.Errorf("a forged event was kept: %+v", e)
		}
	}
	for _, id := range []string{w.runA.ID, w.runB.ID} {
		if got, _ := hb.task(hb.user(root), id); got.State != tasks.Running {
			t.Errorf("task %s is %s after the forgeries, want running", id, got.State)
		}
	}
}

// TestCredentialsStayOnTheirSide: a node token is no human's sign-on,
// however it is presented, and a human's IAP assertion is no node's.
func TestCredentialsStayOnTheirSide(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tok, _ := hb.mint(alice, "ws-a")
	jwt := hb.key.sign(t, alice, nil)
	for _, who := range []as{
		bearer(tok),
		func(r *http.Request) { r.Header.Set(identity.IAPHeader, tok) },
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "silo_session", Value: tok}) },
	} {
		for _, p := range []string{"/v1/me", "/v1/tasks", "/v1/nodes", "/v1/node-tokens"} {
			if code, _, _ := hb.do(who, "GET", p, nil); code != http.StatusUnauthorized {
				t.Errorf("GET %s with a node token: %d, want 401", p, code)
			}
		}
		if code, _, _ := hb.do(who, "POST", "/v1/node-tokens", map[string]string{"node": "ws-z"}); code != http.StatusUnauthorized {
			t.Errorf("a node token minting another: %d, want 401", code)
		}
	}
	// A human's assertion, as the node's bearer token or beside it.
	for _, token := range []string{jwt, "silo-node_ws-a_" + jwt} {
		if _, err := dial(t, hb, token, "ws-a", repoA); err == nil {
			t.Errorf("a node connected with a human's assertion as its token")
		}
	}
	code, _ := try(t, func(r *http.Request) { r.Header.Set(identity.IAPHeader, jwt) }, "GET", hb.url+rendezvous.ConnectPath, "", nil, true)
	if code != http.StatusUnauthorized {
		t.Errorf("connecting with only a human's assertion: %d, want 401", code)
	}
}

// TestNodeFilingRateLimited: a node files at most tasks.DefaultFileBurst
// tasks at once (task 4233); past that the hub refuses, and the refusal
// does not end its connection.
func TestNodeFilingRateLimited(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tok, _ := hb.mint(alice, "ws-a")
	c, err := dial(t, hb, tok, "ws-a", repoA)
	if err != nil {
		t.Fatal(err)
	}
	file := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := c.FileTask(ctx, tasks.Filing{Repo: repoB, Prompt: "for team B"})
		return err
	}
	for i := range tasks.DefaultFileBurst {
		if err := file(); err != nil {
			t.Fatalf("filing %d: %v", i, err)
		}
	}
	err = file()
	if !errors.Is(err, rendezvous.ErrFileRefused) || !strings.Contains(err.Error(), "too fast") {
		t.Errorf("filing past the limit: %v, want refused as too fast", err)
	}
	select {
	case <-c.Done():
		t.Error("the refusal ended the node's connection")
	case <-time.After(500 * time.Millisecond):
	}
	ts, err := hb.h.Backlog.List(context.Background(), tasks.Filter{Teams: []string{teamB}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != tasks.DefaultFileBurst {
		t.Errorf("%d tasks filed, want %d", len(ts), tasks.DefaultFileBurst)
	}
}
