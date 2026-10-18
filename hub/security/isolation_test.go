package security_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/tasks"
)

// TestCrossTeamIsolation is team A's people against team B's task and
// node, and team B's admin against team A's: nothing of the other
// team's is listed, filtered for, subscribed to (SSE or WebSocket, from
// any cursor), viewed, changed or talked to, and every refusal is the
// same 404 a task, team or node that does not exist gets, so that its
// existence is not learned either.
func TestCrossTeamIsolation(t *testing.T) {
	w := newWorld(t)
	hb := w.hb

	ctx := context.Background()
	if _, err := hb.h.Grainfiles.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "ws-a",
		Owner:      alice,
		Recipients: []string{"https://github.com/acme/grain-a"},
		Repos:      []string{"https://github.com/acme/app"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := hb.h.Grainfiles.SetSubscriptions(ctx, grainfiles.Subscriptions{
		NodeID:     "ws-b",
		Owner:      bob,
		Recipients: []string{"https://github.com/beta/grain-b"},
		Repos:      []string{"https://github.com/beta/app"},
	}); err != nil {
		t.Fatal(err)
	}
	gfA, err := grainfiles.Parse("tasks/a.task.md", []byte("---\ntitle: Team A Secret Task\nrecipient: https://github.com/acme/grain-a\n---\nTeam A body\n"))
	if err != nil {
		t.Fatal(err)
	}
	gfA.Repo, gfA.Ref, gfA.Commit = "https://github.com/acme/app", "main", "1111111111111111111111111111111111111111"
	if _, err := hb.h.Grainfiles.IndexGrainfile(ctx, gfA); err != nil {
		t.Fatal(err)
	}
	gfB, err := grainfiles.Parse("tasks/b.task.md", []byte("---\ntitle: Team B Secret Task\nrecipient: https://github.com/beta/grain-b\n---\nTeam B body\n"))
	if err != nil {
		t.Fatal(err)
	}
	gfB.Repo, gfB.Ref, gfB.Commit = "https://github.com/beta/app", "main", "2222222222222222222222222222222222222222"
	if _, err := hb.h.Grainfiles.IndexGrainfile(ctx, gfB); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		who        string
		theirs     tasks.Task
		theirTeam  string
		theirNode  string
		theirOwner string
	}{
		{alice, w.runB, teamB, "ws-b", bob},
		{dave, w.runB, teamB, "ws-b", bob},
		{carol, w.runA, teamA, "ws-a", alice},
		{bob, w.runA, teamA, "ws-a", alice},
	} {
		t.Run(tc.who, func(t *testing.T) {
			me := hb.user(tc.who)
			// The same request for a task that does not exist: what
			// every refusal must look like.
			same := func(method, path, ghostPath string, body any) {
				t.Helper()
				code, b, _ := hb.do(me, method, path, body)
				gcode, gb, _ := hb.do(me, method, ghostPath, body)
				if code != http.StatusNotFound {
					t.Errorf("%s %s: %d %s, want 404", method, path, code, b)
				}
				if code != gcode || strings.ReplaceAll(string(b), tc.theirs.ID, "X") != strings.ReplaceAll(string(gb), "t-ghost", "X") {
					t.Errorf("%s %s answers %d %q, but a task that does not exist %d %q: the difference tells it exists", method, path, code, b, gcode, gb)
				}
			}
			id := tc.theirs.ID
			for _, r := range []struct {
				method, sub string
				body        any
			}{
				{"GET", "", nil},
				{"GET", "/events", nil},
				{"GET", "/events?from=0", nil},
				{"GET", "/events?from=1", nil},
				{"GET", "/events?from=999", nil},
				{"GET", "/artifacts?ref=diff-1", nil},
				{"GET", "/approvals", nil},
				{"GET", "/messages", nil},
				{"POST", "/cancel", map[string]string{"reason": "mine now"}},
				{"POST", "/requeue", nil},
				{"POST", "/approve", nil},
				{"POST", "/reject", map[string]string{"reason": "no"}},
				{"POST", "/approvals/req-member", map[string]string{"decision": "allow"}},
				{"POST", "/messages", map[string]string{"text": "hello"}},
			} {
				same(r.method, tasks.TasksPath+"/"+id+r.sub, tasks.TasksPath+"/t-ghost"+r.sub, r.body)
			}

			// A replay from a cursor by Last-Event-ID, and a WebSocket
			// subscription, are refused before anything is sent.
			code, _ := try(t, func(r *http.Request) { me(r); r.Header.Set("Last-Event-ID", "1") }, "GET", hb.url+tasks.TasksPath+"/"+id+"/events", "", nil, false)
			if code != http.StatusNotFound {
				t.Errorf("replay by Last-Event-ID: %d, want 404", code)
			}
			code, _ = try(t, func(r *http.Request) {
				me(r)
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Sec-WebSocket-Version", "13")
				r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}, "GET", hb.url+tasks.TasksPath+"/"+id+"/events?from=0", "", nil, false)
			if code != http.StatusNotFound {
				t.Errorf("a WebSocket subscription: %d, want 404", code)
			}

			// Not listed, whatever the filter.
			for _, q := range []string{"", "?team=" + tc.theirTeam, "?team=" + tc.theirTeam + "&state=running,queued,done", "?state=running", "?limit=1000"} {
				var got struct {
					Tasks []tasks.Task `json:"tasks"`
				}
				hb.must(me, "GET", tasks.TasksPath+q, nil, http.StatusOK, &got)
				for _, tk := range got.Tasks {
					if tk.Team == tc.theirTeam {
						t.Errorf("GET /v1/tasks%s lists %s's task %s", q, tc.theirTeam, tk.ID)
					}
				}
			}

			// The other team, its settings, and filing to it by name.
			for _, sub := range []string{"", "/settings"} {
				code, b, _ := hb.do(me, "GET", "/v1/teams/"+tc.theirTeam+sub, nil)
				gcode, gb, _ := hb.do(me, "GET", "/v1/teams/no-such-team"+sub, nil)
				if code != http.StatusNotFound || code != gcode || string(b) != string(gb) {
					t.Errorf("GET /v1/teams/%s%s: %d %q; a team that does not exist: %d %q", tc.theirTeam, sub, code, b, gcode, gb)
				}
			}
			code, b, _ := hb.do(me, "POST", tasks.TasksPath, map[string]string{"team": tc.theirTeam, "prompt": "p"})
			gcode, gb, _ := hb.do(me, "POST", tasks.TasksPath, map[string]string{"team": "no-such-team", "prompt": "p"})
			if code != http.StatusNotFound || code != gcode || string(b) != string(gb) {
				t.Errorf("filing to team %s by name: %d %q; to a team that does not exist: %d %q", tc.theirTeam, code, b, gcode, gb)
			}

			// The node directory, and the other team's node's UI.
			var dir struct {
				Nodes []registry.Entry `json:"nodes"`
			}
			hb.must(me, "GET", "/v1/nodes", nil, http.StatusOK, &dir)
			for _, e := range dir.Nodes {
				if e.ID == tc.theirNode || e.Owner == tc.theirOwner {
					t.Errorf("the node directory shows %s: %+v", tc.who, e)
				}
			}
			code, h := try(t, me, "GET", hb.url+"/", tc.theirNode+"."+nodeDomain, nil, false)
			gcode, _ = try(t, me, "GET", hb.url+"/", "ghost."+nodeDomain, nil, false)
			if code != http.StatusNotFound || gcode != code {
				t.Errorf("node %s's UI: %d (%v); a node that does not exist: %d", tc.theirNode, code, h, gcode)
			}

			// The grainfiles index, subscriptions, and events: nothing of the other team's is visible.
			theirRepoURL := "https://github.com/" + tc.theirs.Repo
			for _, q := range []string{"", "?repo=" + theirRepoURL, "?node=" + tc.theirNode, "?include_drafts=true"} {
				var gfResp grainfiles.GrainfilesResponse
				hb.must(me, "GET", grainfiles.GrainfilesPath+q, nil, http.StatusOK, &gfResp)
				for _, g := range gfResp.Grainfiles {
					if g.Repo == theirRepoURL {
						t.Errorf("GET /v1/grainfiles%s as %s leaked %s's grainfile %s", q, tc.who, tc.theirTeam, g.URL)
					}
					for _, sn := range g.SubscribedNodes {
						if sn == tc.theirNode {
							t.Errorf("GET /v1/grainfiles%s as %s leaked subscribed node %s", q, tc.who, sn)
						}
					}
				}
				for _, sub := range gfResp.Subscriptions {
					if sub.NodeID == tc.theirNode || sub.Owner == tc.theirOwner {
						t.Errorf("GET /v1/grainfiles%s as %s leaked subscription %+v", q, tc.who, sub)
					}
				}
				for _, ev := range gfResp.Events {
					if ev.Repo == theirRepoURL {
						t.Errorf("GET /v1/grainfiles%s as %s leaked grainfile event %+v", q, tc.who, ev)
					}
				}
			}
		})
	}

	// A signed-in user in no team sees no grainfiles, no subscriptions, and no events.
	var ninaGF grainfiles.GrainfilesResponse
	hb.must(hb.user(nina), "GET", grainfiles.GrainfilesPath+"?include_drafts=true", nil, http.StatusOK, &ninaGF)
	if len(ninaGF.Grainfiles) != 0 || len(ninaGF.Subscriptions) != 0 || len(ninaGF.Events) != 0 {
		t.Errorf("non-member GET /v1/grainfiles leaked data: %+v", ninaGF)
	}

	// Through all of it, team B's task went on untouched: still
	// running, and its node was sent nothing but the offer.
	got, _ := hb.task(hb.user(bob), w.runB.ID)
	if got.State != tasks.Running || got.Reason != "" {
		t.Errorf("team B's task after the attempts: %+v", got)
	}
	for _, typ := range []string{tasks.TypeCancel, tasks.TypeMessage, tasks.TypeApprove} {
		if w.nodeB.taskStream(w.runB.ID).saw(typ) {
			t.Errorf("team B's node was sent a %s", typ)
		}
	}
	// And the answers carry nothing of B's to A: no task B event in a
	// subscription A may make.
	_, evs := hb.replay(hb.user(alice), w.runA.ID, 0, 100)
	for _, e := range evs {
		if e.Task != w.runA.ID {
			b, _ := json.Marshal(e)
			t.Errorf("team A's subscription carried %s", b)
		}
	}
}

func TestNodeUITeamAdminIsolation(t *testing.T) {
	w := newWorld(t)
	hb := w.hb
	ctx := context.Background()

	// Register a node owned by dave (member of teamA) offering UI to teamA.
	node := registry.Node{
		ID:    "ws-dave",
		Repos: []string{repoA},
		Slots: 1,
		UI:    &registry.NodeUI{Offered: true, Team: teamA, Access: "read-only"},
	}
	if err := hb.h.Directory.ValidateNodeUI(ctx, dave, &node); err != nil {
		t.Fatal(err)
	}
	lease, err := hb.h.Registry.Register(node, dave)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	// Owner (dave) and teamA admin (alice) pass authorization (reaching Open, which returns 503 because ws-dave has no live tunnel conn).
	for _, allowed := range []string{dave, alice} {
		code, _ := try(t, hb.user(allowed), "GET", hb.url+"/", "ws-dave."+nodeDomain, nil, false)
		if code == http.StatusNotFound || code == http.StatusUnauthorized {
			t.Errorf("allowed viewer %s on ws-dave got %d", allowed, code)
		}
	}

	// Hub admin (root, not in teamA), teamB admin (bob), teamB member (carol), and outsider (nina) all receive 404.
	for _, denied := range []string{root, bob, carol, nina} {
		code, _ := try(t, hb.user(denied), "GET", hb.url+"/", "ws-dave."+nodeDomain, nil, false)
		if code != http.StatusNotFound {
			t.Errorf("denied viewer %s on ws-dave got %d, want 404", denied, code)
		}
	}

	// If dave claims ui.team = teamB (which dave is not in), ValidateNodeUI drops ui.team and bob gets 404.
	badNode := registry.Node{
		ID:    "ws-dave-bad",
		Repos: []string{repoA},
		Slots: 1,
		UI:    &registry.NodeUI{Offered: true, Team: teamB},
	}
	if err := hb.h.Directory.ValidateNodeUI(ctx, dave, &badNode); err != nil {
		t.Fatal(err)
	}
	if badNode.UI.Team != "" || badNode.UI.Reason == "" {
		t.Fatalf("badNode UI = %+v; want Team=\"\" and Reason set", badNode.UI)
	}
	leaseBad, err := hb.h.Registry.Register(badNode, dave)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseBad.Close()
	if code, _ := try(t, hb.user(bob), "GET", hb.url+"/", "ws-dave-bad."+nodeDomain, nil, false); code != http.StatusNotFound {
		t.Errorf("bob on ws-dave-bad got %d, want 404", code)
	}
}
