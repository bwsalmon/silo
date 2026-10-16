package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// TestSharedRegistry runs two hubs on one PostgreSQL database, as two
// pods would (docs/hub.md §2.2.2): a node connected to one is online in
// the other's directory, and giving its ID up through the other hub
// unbinds it on both, ending its connection, so that another owner's
// node can take the ID.
func TestSharedRegistry(t *testing.T) {
	pg := pgtest.Start(t)
	cfg := func(pod string) Config {
		return Config{
			Database:      pg.URL,
			DevUserHeader: userHeader,
			HubAdmins:     []string{"admin@example.com"},
			Heartbeat:     200 * time.Millisecond,
			ShutdownGrace: 10 * time.Second,
			Holder:        pod,
			Pod:           pod,
			PodLease:      600 * time.Millisecond,
			Logf:          logf(t),
		}
	}
	// One after the other: concurrent CREATE TABLE IF NOT EXISTS can
	// fail on PostgreSQL.
	a := start(t, cfg("pod-a"), "127.0.0.1:0")
	eventually(t, "pod-a ready", func() bool { return call(t, "GET", a.url+ReadyzPath, "", nil, nil) == http.StatusOK })
	b := start(t, cfg("pod-b"), "127.0.0.1:0")
	eventually(t, "pod-b ready", func() bool { return call(t, "GET", b.url+ReadyzPath, "", nil, nil) == http.StatusOK })

	call(t, "POST", a.url+"/v1/teams", "admin@example.com", map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
	for _, u := range []string{"alice@example.com", "bob@example.com"} {
		call(t, "PUT", a.url+"/v1/teams/infra/members/user:"+u, "admin@example.com", map[string]string{"role": "member"}, &struct{}{})
	}
	call(t, "PUT", a.url+"/v1/teams/infra/repos/bwsalmon/silo", "admin@example.com", nil, &struct{}{})
	connect := func(hub *running, owner string) {
		var minted struct {
			Token string `json:"token"`
		}
		call(t, "POST", hub.url+"/v1/node-tokens", owner, map[string]string{"node": "ws-1"}, &minted)
		if minted.Token == "" {
			t.Fatalf("no node token minted for %s", owner)
		}
		n := &node{}
		cl := &rendezvous.Client{
			Hub:   hub.url,
			Token: func(context.Context) (string, error) { return minted.Token, nil },
			Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1},
		}
		ctx, cancel := context.WithCancel(context.Background())
		ran := make(chan error, 1)
		go func() { ran <- cl.Run(ctx, n.handle) }()
		t.Cleanup(func() {
			cancel()
			<-ran
		})
	}
	connect(a, "alice@example.com")
	eventually(t, "ws-1 online on pod-a", func() bool { return online(t, a.url, "ws-1") })
	eventually(t, "ws-1 online on pod-b", func() bool { return online(t, b.url, "ws-1") })

	// Alice gives the ID up through pod-b, which does not hold the
	// node's connection: pod-a's next heartbeat finds the row gone and
	// drops it.
	if code := call(t, "DELETE", b.url+"/v1/node-tokens/nodes/ws-1", "alice@example.com", nil, nil); code != http.StatusNoContent {
		t.Fatalf("releasing ws-1: %d", code)
	}
	eventually(t, "ws-1 gone from pod-a", func() bool { return !online(t, a.url, "ws-1") })

	connect(b, "bob@example.com")
	var dir struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	eventually(t, "bob's ws-1 online on pod-a", func() bool {
		call(t, "GET", a.url+rendezvous.DirectoryPath, "admin@example.com", nil, &dir)
		for _, e := range dir.Nodes {
			if e.ID == "ws-1" && e.Owner == "bob@example.com" && e.Status == registry.Online {
				return true
			}
		}
		return false
	})

	// pod-b shuts down: its node is offline on pod-a, which pod-b's
	// deleted lease says without waiting for it to lapse.
	b.shutdown(t)
	eventually(t, "bob's ws-1 offline on pod-a", func() bool {
		call(t, "GET", a.url+rendezvous.DirectoryPath, "admin@example.com", nil, &dir)
		for _, e := range dir.Nodes {
			if e.ID == "ws-1" && e.Owner == "bob@example.com" {
				return e.Status == registry.Offline
			}
		}
		return false
	})
}
