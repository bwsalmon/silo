package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// TestBackupRestore is the restore procedure of docs/hub-operations.md
// on throwaway servers: a hub's database is dumped with pg_dump, as
// `gcloud sql export sql` does, restored into an empty server, and a
// hub started on the copy. The team directory, the node tokens and the
// backlog survive; the registry, which is in memory, starts empty and
// is rebuilt as nodes reconnect; and a task that was running when the
// backup was taken is lost until its node resumes it.
func TestBackupRestore(t *testing.T) {
	ctx := context.Background()
	src := pgtest.Start(t)
	cfg := Config{
		Database:      src.URL,
		DevUserHeader: userHeader,
		HubAdmins:     []string{"admin@example.com"},
		Heartbeat:     time.Second,
		ShutdownGrace: 10 * time.Second,
		DispatchRetry: time.Second,
		Logf:          logf(t),
	}
	h, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			h.Close()
			t.Fatal(err)
		}
	}
	must(h.Directory.CreateTeam(ctx, "infra", "Infra"))
	must(h.Directory.SetMember(ctx, "infra", "alice@example.com", directory.Admin))
	must(h.Directory.AssignRepo(ctx, "infra", "bwsalmon/silo"))
	must(h.Directory.SetTeamSettings(ctx, "infra", directory.Settings{LostRequeueAfter: time.Hour}))
	token, _, err := h.Directory.MintNodeToken(ctx, "alice@example.com", "ws-1")
	must(err)
	spec := tasks.Spec{Team: "infra", Repo: "bwsalmon/silo", Submitter: "alice@example.com"}
	spec.Prompt = "still queued"
	queued, err := h.Backlog.Create(ctx, spec)
	must(err)
	spec.Prompt = "running when the backup was taken"
	running, err := h.Backlog.Create(ctx, spec)
	must(err)
	_, err = h.Backlog.Offer(ctx, running.ID, "ws-1")
	must(err)
	_, err = h.Backlog.Accept(ctx, running.ID, "local-1")
	must(err)
	e, class, err := live.Classify([]byte(`{"v":1,"seq":1,"type":"status","task":"` + running.ID + `","status":{"phase":"setup","step":"tools","state":"done"}}`))
	must(err)
	must(h.Events.Append(ctx, e, class))
	must(h.Close())

	dump := filepath.Join(t.TempDir(), "hub.dump")
	pgtest.Run(t, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--file="+dump, "--dbname="+src.URL)
	dst := pgtest.Start(t)
	pgtest.Run(t, "pg_restore", "--no-owner", "--no-privileges", "--exit-on-error", "--dbname="+dst.URL, dump)

	cfg.Database = dst.URL
	hub := start(t, cfg, "127.0.0.1:0")

	// The directory.
	var teams struct {
		Teams []directory.Team `json:"teams"`
	}
	call(t, "GET", hub.url+directory.TeamsPath, "alice@example.com", nil, &teams)
	if len(teams.Teams) != 1 || teams.Teams[0].ID != "infra" || len(teams.Teams[0].Repos) != 1 ||
		teams.Teams[0].Members["alice@example.com"] != directory.Admin {
		t.Errorf("restored teams: %+v", teams.Teams)
	}
	var set struct {
		Seconds int64 `json:"lost_requeue_after_seconds"`
	}
	call(t, "GET", hub.url+"/v1/teams/infra/settings", "alice@example.com", nil, &set)
	if set.Seconds != 3600 {
		t.Errorf("restored settings: %+v", set)
	}

	// The registry is empty until nodes reconnect.
	var dir struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	call(t, "GET", hub.url+rendezvous.DirectoryPath, "alice@example.com", nil, &dir)
	if len(dir.Nodes) != 0 {
		t.Errorf("a restored hub's registry: %+v", dir.Nodes)
	}

	// The backlog: the running task is lost, since no node holds a
	// stream to the restored hub, and the queued one is still queued.
	eventually(t, "the running task lost", func() bool { return taskState(t, hub.url, running.ID) == tasks.Lost })
	if st := taskState(t, hub.url, queued.ID); st != tasks.Queued {
		t.Errorf("the queued task is %s", st)
	}

	// The node's token still works: the node reconnects, is asked to
	// resume its task, and is offered the queued one.
	n := &node{}
	cl := &rendezvous.Client{
		Hub:   hub.url,
		Token: func(context.Context) (string, error) { return token, nil },
		Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 2},
	}
	nctx, ncancel := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() { ran <- cl.Run(nctx, n.handle) }()
	t.Cleanup(func() {
		ncancel()
		<-ran
	})
	eventually(t, "ws-1 online", func() bool { return online(t, hub.url, "ws-1") })
	eventually(t, "the lost task resumed", func() bool { return taskState(t, hub.url, running.ID) == tasks.Running })
	eventually(t, "the queued task placed", func() bool { return taskState(t, hub.url, queued.ID) == tasks.Running })

	// The stored events.
	events, err := live.Open(ctx, dst.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	got, err := events.Since(ctx, running.ID, 0, 10)
	if err != nil || len(got) != 1 || got[0].Type != "status" {
		t.Errorf("restored events: %+v, %v", got, err)
	}
}
