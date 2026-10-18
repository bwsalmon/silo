package faulttest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/tasks"
)

// full is the nightly matrix (SILO_FAULT_FULL): longer faults, more
// tasks, more of each. Without it each scenario is its short version,
// for every pull request.
var full = os.Getenv("SILO_FAULT_FULL") != ""

// pick is short's value on a pull request and long's in the nightly
// matrix.
func pick[T any](short, long T) T {
	if full {
		return long
	}
	return short
}

// heartbeat is the scenarios' heartbeat: StaleAfter is three of it.
const heartbeat = time.Second

// staleAfter is the hub's registry.StaleAfter for heartbeat.
const staleAfter = 3 * heartbeat

// muxTimeout is long enough for the rendezvous mux's keepalive (a ping
// every 15s, closed 10s past the last pong) to notice a dead peer.
const muxTimeout = 35 * time.Second

func standard(t *testing.T, grains int) *Cluster {
	return NewCluster(t, Options{Teams: 1, GrainsPerTeam: grains, Proxied: true, Heartbeat: heartbeat,
		Grain: Work{Work: 4 * time.Second, Events: 20}})
}

// watchAll watches every task submitted so far.
func watchAll(c *Cluster) []*Watcher {
	var ws []*Watcher
	for _, id := range c.Ledger.Submitted() {
		ws = append(ws, c.Watch(id, 0))
	}
	return ws
}

// watchersEnd waits for every watcher to see its task's end.
func watchersEnd(t *testing.T, ws []*Watcher) {
	t.Helper()
	for _, w := range ws {
		select {
		case <-w.Done():
		case <-time.After(time.Minute):
			n, r, _, _ := w.Stats()
			t.Errorf("a subscriber to %s did not see its end (%d events, %d reconnects)", w.task, n, r)
		}
	}
}

// running waits until n tasks are running.
func running(c *Cluster, n int) {
	c.Eventually(fmt.Sprintf("%d tasks running", n), time.Minute, func() bool { return c.States()[tasks.Running] >= n })
}

// TestHubRestart restarts the hub with SIGTERM, as a rolled pod is,
// while tasks are being offered and while their events stream, with
// more tasks queued than the Grains have slots.
func TestHubRestart(t *testing.T) {
	c := standard(t, 3)
	c.SubmitN(pick(12, 40))
	ws := watchAll(c)
	for range pick(2, 4) {
		running(c, 3)
		time.Sleep(time.Second) // mid-stream
		if err := c.Hub.Stop(); err != nil {
			t.Fatalf("SIGTERM: %v", err)
		}
		c.Hub.Start(t)
		c.SubmitN(2) // and tasks arrive as it comes back: mid-dispatch
	}
	c.Settle(3 * time.Minute)
	watchersEnd(t, ws)
	if c.Ledger.Resumes() == 0 {
		t.Error("no Grain was asked to resume a task after the restarts")
	}
}

// TestHubCrash kills the hub with SIGKILL: no goodbye, nothing
// flushed. Each Grain notices its connection dropped, and reconnects
// to the restarted hub, which finds its offered tasks back in the
// queue and its running ones lost until their Grains resume them.
func TestHubCrash(t *testing.T) {
	c := standard(t, 3)
	c.SubmitN(pick(12, 40))
	ws := watchAll(c)
	for range pick(1, 4) {
		running(c, 3)
		time.Sleep(500 * time.Millisecond)
		c.Hub.Kill()
		c.Hub.Start(t)
	}
	c.Settle(3 * time.Minute)
	watchersEnd(t, ws)
}

// TestPartition black-holes one Grain, so that neither end hears from
// the other, for less than StaleAfter (the hub need not notice), for
// longer (the Grain goes stale and is passed over, its tasks left
// running), and, nightly, for longer than the mux's keepalive takes
// to give up on a dead peer (both ends drop the connection; the hub
// marks the Grain's running tasks lost and asks it to resume them when
// it is back).
func TestPartition(t *testing.T) {
	durations := pick([]time.Duration{staleAfter / 2, 2 * staleAfter},
		[]time.Duration{staleAfter / 2, 2 * staleAfter, muxTimeout + 5*time.Second})
	for _, d := range durations {
		t.Run(d.String(), func(t *testing.T) {
			c := NewCluster(t, Options{Teams: 1, GrainsPerTeam: 3, Proxied: true, Heartbeat: heartbeat,
				Grain: Work{Work: d + 4*time.Second, Events: 20}})
			c.SubmitN(9)
			ws := watchAll(c)
			running(c, 6)
			g := c.Grains[0]
			p := c.Proxies[g.ID]
			p.Blackhole()
			if d > staleAfter {
				c.Eventually(g.ID+" stale", d, func() bool { return status(c, g.ID) == registry.Stale })
			}
			time.Sleep(d)
			p.Up()
			c.Settle(2 * time.Minute)
			watchersEnd(t, ws)
		})
	}
}

// status is node id's status in the hub's directory.
func status(c *Cluster, id string) registry.Status {
	nodes, _ := c.Nodes()
	for _, e := range nodes {
		if e.ID == id {
			return e.Status
		}
	}
	return ""
}

// TestLatencyAndLoss runs tasks with every Grain's link slowed and
// lossy, and the database's slowed.
func TestLatencyAndLoss(t *testing.T) {
	c := standard(t, 3)
	for _, p := range c.Proxies {
		p.Degrade(pick(30*time.Millisecond, 100*time.Millisecond), pick(0.02, 0.1))
	}
	c.PGProxy.Degrade(2*time.Millisecond, 0.01)
	c.SubmitN(pick(12, 30))
	ws := watchAll(c)
	c.Settle(3 * time.Minute)
	watchersEnd(t, ws)
}

// TestHalfOpen leaves one Grain's connection half-open, black-holed
// past the mux's keepalive, which is what must notice: no FIN or RST
// ever arrives. The hub drops the Grain and marks its tasks lost; the
// Grain, once the link is back, reconnects and resumes them.
func TestHalfOpen(t *testing.T) {
	c := NewCluster(t, Options{Teams: 1, GrainsPerTeam: 2, Proxied: true, Heartbeat: heartbeat,
		Grain: Work{Work: 6 * time.Second, Events: 20}})
	c.SubmitN(4)
	ws := watchAll(c)
	running(c, 4)
	g := c.Grains[0]
	p := c.Proxies[g.ID]
	p.Blackhole()
	c.Eventually(g.ID+" offline", muxTimeout+10*time.Second, func() bool { return status(c, g.ID) == registry.Offline })
	if lost := c.States()[tasks.Lost]; lost == 0 {
		t.Errorf("the half-open Grain's tasks were not marked lost: %v", c.States())
	}
	p.Up()
	c.Settle(2 * time.Minute)
	watchersEnd(t, ws)
	if c.Ledger.Resumes() == 0 {
		t.Error("the Grain was not asked to resume its tasks")
	}
}

// TestSupersession has a Grain's connection die without the hub
// noticing and the Grain come back, with the same ID, by another
// route: the new connection supersedes the old at once, rather than
// after the keepalive gives up on it, and the Grain resumes its tasks.
func TestSupersession(t *testing.T) {
	c := standard(t, 2)
	c.SubmitN(4)
	ws := watchAll(c)
	running(c, 4)
	g := c.Grains[0]
	c.Proxies[g.ID].Blackhole()
	before := g.Connects()
	g.Reconnect(c.Hub.URL)
	c.Eventually(g.ID+" reconnected", 15*time.Second, func() bool { return g.Connects() > before })
	c.Eventually(g.ID+" online", 15*time.Second, func() bool { return status(c, g.ID) == registry.Online })
	c.Settle(time.Minute)
	watchersEnd(t, ws)
	if c.Ledger.Resumes() == 0 {
		t.Error("the superseding connection was not asked to resume the Grain's tasks")
	}
	if strings.Count(c.Hub.Log(), `node "`+g.ID+`" of `+Ops+" online") < 2 {
		t.Errorf("the hub did not log %s registering again", g.ID)
	}
	c.Proxies[g.ID].Up()
}

// TestRefusingGrain puts a Grain that refuses every offer beside
// Grains that take them: its tasks go to the others, never lost. A
// team whose only Grain refuses keeps its tasks queued, the refuser
// excluded, until a Grain that will run them arrives.
func TestRefusingGrain(t *testing.T) {
	c := NewCluster(t, Options{Teams: 2, Proxied: true, Heartbeat: heartbeat})
	c.Opts.Grain = Work{Work: 2 * time.Second, Events: 10}
	for team, refusers := range []int{1, 1} {
		for j := range refusers {
			c.AddGrain(&Grain{ID: fmt.Sprintf("refuser-%d-%d", team, j), Repos: []string{Repo(team)}, Slots: 2, Refuse: true})
		}
	}
	for j := range 2 {
		c.AddGrain(&Grain{ID: fmt.Sprintf("worker-0-%d", j), Repos: []string{Repo(0)}, Slots: 2, Work: 2 * time.Second, Events: 10})
	}
	c.Eventually("every Grain online", time.Minute, func() bool { return c.AllOnline() == nil })
	var team1 []string
	for range pick(8, 20) {
		c.Submit(0)
	}
	for range 2 {
		id, _ := c.Submit(1)
		team1 = append(team1, id)
	}
	c.Eventually("team 1's tasks refused", 30*time.Second, func() bool { return c.Ledger.Refusals("refuser-1-0") >= 2 })
	time.Sleep(2 * time.Second)
	for _, id := range team1 {
		var excluded string
		c.DB.QueryRow(`SELECT excluded FROM tasks WHERE id = $1`, id).Scan(&excluded)
		if s := c.State(id); s != tasks.Queued || !strings.Contains(excluded, "refuser-1-0") {
			t.Errorf("team 1's task %s, refused by its only Grain, is %s excluding %s; want queued, excluding it", id, s, excluded)
		}
	}
	c.AddGrain(&Grain{ID: "worker-1-0", Repos: []string{Repo(1)}, Slots: 2, Work: 2 * time.Second, Events: 10})
	c.Settle(2 * time.Minute)
	for _, id := range c.Ledger.Submitted() {
		for _, g := range c.Ledger.Runs(id) {
			if strings.HasPrefix(g, "refuser") {
				t.Errorf("task %s ran on %s, which refuses everything", id, g)
			}
		}
	}
}

// TestPostgresOutage takes the database away from the hub, cutting its
// connections and refusing new ones, while tasks are offered, run and
// stream, and brings it back. Nothing is lost: the hub keeps its
// tasks' transitions until it can write them, and the Grains replay the
// events the hub could not keep.
func TestPostgresOutage(t *testing.T) {
	c := NewCluster(t, Options{Teams: 1, GrainsPerTeam: 3, Proxied: true, Heartbeat: heartbeat,
		Grain: Work{Work: 3 * time.Second, Events: 20}})
	c.SubmitN(pick(12, 30))
	ws := watchAll(c)
	running(c, 3)
	c.PGProxy.Down()
	time.Sleep(pick(5*time.Second, 30*time.Second))
	c.PGProxy.Up()
	c.Settle(3 * time.Minute)
	watchersEnd(t, ws)
}

// TestSlowSubscriber has one subscriber that reads an event every
// 10ms beside several that keep up, on a task whose events come far
// faster: the slow one falls behind (and, once the hub's buffer and the
// sockets' are full, is dropped and reconnects from its cursor), while
// the others' events arrive as promptly as they would without it.
func TestSlowSubscriber(t *testing.T) {
	c := NewCluster(t, Options{Teams: 1, GrainsPerTeam: 1, Heartbeat: heartbeat,
		Grain: Work{Work: 4 * time.Second, Events: 200, Tokens: 4, Pad: 2048}})
	ids := c.SubmitN(1)
	slow := c.Watch(ids[0], 10*time.Millisecond)
	var fast []*Watcher
	for range 5 {
		fast = append(fast, c.Watch(ids[0], 0))
	}
	c.Settle(time.Minute)
	watchersEnd(t, fast)
	var lat []time.Duration
	for _, w := range fast {
		_, _, _, l := w.Stats()
		lat = append(lat, l...)
	}
	if p99 := Percentile(lat, 99); p99 > time.Second {
		t.Errorf("the fast subscribers' p99 fan-out latency was %s beside a slow one", p99)
	}
	select {
	case <-slow.Done():
	case <-time.After(3 * time.Minute):
	}
	n, reconnects, ended, _ := slow.Stats()
	if !ended {
		t.Errorf("the slow subscriber saw %d events over %d reconnects and not the end", n, reconnects)
	}
	t.Logf("the slow subscriber saw %d events over %d reconnects", n, reconnects)
}

// TestViewThroughDisconnect reads a node-only artifact through the hub
// while the Grain that holds it disconnects: the viewer's read ends
// rather than hanging, a view while the Grain is away is answered 503,
// and one once it is back is served whole.
func TestViewThroughDisconnect(t *testing.T) {
	c := NewCluster(t, Options{Teams: 1, GrainsPerTeam: 1, Proxied: true, Heartbeat: heartbeat,
		Grain: Work{Work: 15 * time.Second, Events: 30}})
	id := c.SubmitN(1)[0]
	running(c, 1)
	url := c.Hub.URL + "/v1/tasks/" + id + "/artifacts?ref=art-1"
	// art-1 as the Grain lists it: the hub views no other ref.
	if _, err := c.DB.Exec(`INSERT INTO task_refs (task, ref, seq) VALUES ($1, 'art-1', 1)`, id); err != nil {
		t.Fatal(err)
	}
	view := func() (int, int, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(UserHeader, Ops)
		resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
		if err != nil {
			return 0, 0, err
		}
		defer resp.Body.Close()
		n, err := io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, int(n), err
	}
	type res struct {
		code, n int
		err     error
		took    time.Duration
	}
	got := make(chan res, 1)
	go func() {
		start := time.Now()
		code, n, err := view()
		got <- res{code, n, err, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	g := c.Grains[0]
	c.Proxies[g.ID].Down()
	select {
	case r := <-got:
		if r.n >= 256<<10 && r.err == nil {
			t.Errorf("a view cut off mid-read came back whole: %d bytes, %d", r.n, r.code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a view whose Grain disconnected did not end")
	}
	c.Eventually(g.ID+" offline", 10*time.Second, func() bool { return status(c, g.ID) == registry.Offline })
	if code, _, err := view(); code != http.StatusServiceUnavailable {
		t.Errorf("a view with the Grain away: %d %v, want 503", code, err)
	}
	c.Proxies[g.ID].Up()
	c.Eventually(g.ID+" online", 15*time.Second, func() bool { return status(c, g.ID) == registry.Online })
	if code, n, err := view(); code != http.StatusOK || n != 256<<10 || err != nil {
		t.Errorf("a view with the Grain back: %d, %d bytes, %v; want 200 and 256 KiB", code, n, err)
	}
	c.Settle(time.Minute)
}

// TestGrainfileFault_DBOutageMidWebhookAndCatchup takes PostgreSQL down
// mid-webhook ingestion and mid-subscription catch-up while nodes hold
// active recipient subscriptions, then restores PostgreSQL and verifies
// that nodes catch up seamlessly from their cursors with zero lost or
// duplicated grainfile_events.
func TestGrainfileFault_DBOutageMidWebhookAndCatchup(t *testing.T) {
	c := NewCluster(t, Options{Teams: 1, Pods: 2, Heartbeat: heartbeat})
	const recipientA = "https://github.com/org/agent-a"

	g1 := &Grain{ID: "gf-db-1", Repos: []string{Repo(0)}, Recipients: []string{recipientA}, Slots: 2}
	g2 := &Grain{ID: "gf-db-2", Repos: []string{Repo(0)}, Recipients: []string{recipientA}, Slots: 2}
	c.AddGrain(g1)
	c.AddGrain(g2)
	c.Eventually("both Grainfile nodes online", 30*time.Second, func() bool { return c.AllOnline() == nil })

	// 1. Ingest initial webhook before DB outage -> both nodes receive task_ready.
	commit1 := "1111111111111111111111111111111111111111"
	c.SetForgeFile(Repo(0), "tasks/01-pre.task.md", commit1,
		"---\ntitle: Pre-outage task\nrecipient: "+recipientA+"\n---\nRun pre-outage check.\n")
	if code, body, err := c.SendWebhook(c.Pods[0].URL, Repo(0), "refs/heads/main", commit1, []string{"tasks/01-pre.task.md"}, nil, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(commit1): code=%d err=%v body=%s", code, err, body)
	}
	c.Eventually("g1 and g2 receive commit1 task_ready", 15*time.Second, func() bool {
		return len(g1.GrainfileEvents()) >= 1 && len(g2.GrainfileEvents()) >= 1
	})

	// 2. Take PostgreSQL down -> webhook ingestion during outage must fail with 5xx (never silently 200 OK).
	commit2 := "2222222222222222222222222222222222222222"
	c.SetForgeFile(Repo(0), "tasks/02-during.task.md", commit2,
		"---\ntitle: During-outage task\nrecipient: "+recipientA+"\n---\nRun during-outage check.\n")

	c.PGProxy.Down()
	if code, _, err := c.SendWebhook(c.Pods[1].URL, Repo(0), "refs/heads/main", commit2, []string{"tasks/02-during.task.md"}, nil, nil); err == nil && code < 500 {
		t.Fatalf("SendWebhook while DB is down returned %d, want 5xx or connection error", code)
	}
	// Disconnect g2 while DB is down so it must reconnect and catch up as DB recovers.
	g2.Reconnect(c.Pods[1].URL)
	time.Sleep(pick(2*time.Second, 8*time.Second))

	// 3. Bring PostgreSQL back up and redeliver commit2 + commit3 across pods.
	c.PGProxy.Up()
	c.Eventually("cluster healthy after DB recovery", 30*time.Second, func() bool { return c.AllOnline() == nil })

	var code2 int
	c.Eventually("SendWebhook(commit2) succeeds after DB recovery", 15*time.Second, func() bool {
		var err error
		code2, _, err = c.SendWebhook(c.Pods[1].URL, Repo(0), "refs/heads/main", commit2, []string{"tasks/02-during.task.md"}, nil, nil)
		return err == nil && code2 == http.StatusOK
	})

	commit3 := "3333333333333333333333333333333333333333"
	c.SetForgeFile(Repo(0), "processes/03-post.process.md", commit3,
		"---\ntitle: Post-outage process\nrecipient: "+recipientA+"\non:\n  push:\n    branches: [main]\n---\nRun post-outage process.\n")
	if code, body, err := c.SendWebhook(c.Pods[0].URL, Repo(0), "refs/heads/main", commit3, []string{"processes/03-post.process.md"}, nil, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(commit3): code=%d err=%v body=%s", code, err, body)
	}

	// Both g1 and g2 must receive all 4 events (task_ready 1, task_ready 2, process_updated 3, process_triggered 3)
	// in monotonic seq order with zero duplicates.
	c.Eventually("g1 and g2 catch up all 4 grainfile_events", 20*time.Second, func() bool {
		return len(g1.GrainfileEvents()) == 4 && len(g2.GrainfileEvents()) == 4
	})
	for _, g := range []*Grain{g1, g2} {
		if dups := g.DuplicateGrainfileEvents(); dups != 0 {
			t.Errorf("node %s received %d duplicate grainfile_events", g.ID, dups)
		}
		evs := g.GrainfileEvents()
		for i := 1; i < len(evs); i++ {
			if evs[i].Seq <= evs[i-1].Seq {
				t.Errorf("node %s events out of order at %d: %+v", g.ID, i, evs)
			}
		}
	}
}

// TestGrainfileFault_MultiPodRollingRestartAndPodKill performs rolling
// restarts (SIGTERM) and hard pod crashes (SIGKILL) across a 3-pod Hub
// cluster while 6 nodes hold active recipient subscriptions and webhooks
// are continuously ingested, verifying seamless reconnect and cursor
// catch-up with zero lost or duplicated events.
func TestGrainfileFault_MultiPodRollingRestartAndPodKill(t *testing.T) {
	c := NewCluster(t, Options{Teams: 2, Pods: 3, Heartbeat: heartbeat})
	const (
		recipient0 = "https://github.com/org/agent-team0"
		recipient1 = "https://github.com/org/agent-team1"
	)

	var nodes0, nodes1 []*Grain
	for i := range 3 {
		g0 := &Grain{ID: fmt.Sprintf("gf-rr-0-%d", i), Repos: []string{Repo(0)}, Recipients: []string{recipient0}, Slots: 2}
		g1 := &Grain{ID: fmt.Sprintf("gf-rr-1-%d", i), Repos: []string{Repo(1)}, Recipients: []string{recipient1}, Slots: 2}
		c.AddGrain(g0)
		c.AddGrain(g1)
		nodes0 = append(nodes0, g0)
		nodes1 = append(nodes1, g1)
	}
	c.Eventually("all 6 Grainfile nodes online across 3 pods", 30*time.Second, func() bool { return c.AllOnline() == nil })

	totalBatches := pick(12, 30)
	for i := range totalBatches {
		teamIdx := i % 2
		recipient := recipient0
		if teamIdx == 1 {
			recipient = recipient1
		}
		commit := fmt.Sprintf("%040d", i+1)
		path := fmt.Sprintf("tasks/batch-%02d.task.md", i)
		c.SetForgeFile(Repo(teamIdx), path, commit,
			fmt.Sprintf("---\ntitle: Rolling batch %d\nrecipient: %s\n---\nTask body %d.\n", i, recipient, i))

		// Fault a pod mid-stream on selected iterations: alternate SIGTERM rolling restart and SIGKILL crash.
		switch {
		case i == totalBatches/4:
			if err := c.Pods[0].Stop(); err != nil {
				t.Fatalf("Pods[0].Stop: %v", err)
			}
			c.Pods[0].Start(t)
		case i == totalBatches/2:
			if err := c.Pods[1].Kill(); err != nil {
				t.Fatalf("Pods[1].Kill: %v", err)
			}
			c.Pods[1].Start(t)
		case i == (3*totalBatches)/4:
			// Rolling restart of all 3 pods in sequence.
			for pIdx, pod := range c.Pods {
				if err := pod.Stop(); err != nil {
					t.Fatalf("Pods[%d].Stop: %v", pIdx, err)
				}
				pod.Start(t)
			}
		}

		// Deliver webhook to whichever pod is currently serving (round-robin with retry).
		delivered := false
		for attempt := range 20 {
			pod := c.Pods[(i+attempt)%len(c.Pods)]
			code, _, err := c.SendWebhook(pod.URL, Repo(teamIdx), "refs/heads/main", commit, []string{path}, nil, nil)
			if err == nil && code == http.StatusOK {
				delivered = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !delivered {
			t.Fatalf("failed to deliver webhook batch %d to any pod", i)
		}
	}

	want0 := (totalBatches + 1) / 2
	want1 := totalBatches / 2
	c.Eventually("all nodes catch up every event after pod kills and rolling restarts", 30*time.Second, func() bool {
		for _, g := range nodes0 {
			if len(g.GrainfileEvents()) < want0 {
				return false
			}
		}
		for _, g := range nodes1 {
			if len(g.GrainfileEvents()) < want1 {
				return false
			}
		}
		return c.AllOnline() == nil
	})

	for _, g := range append(nodes0, nodes1...) {
		if dups := g.DuplicateGrainfileEvents(); dups != 0 {
			t.Errorf("node %s received %d duplicate grainfile_events across pod kills/restarts", g.ID, dups)
		}
	}
	for _, g := range nodes0 {
		if got := len(g.GrainfileEvents()); got != want0 {
			t.Errorf("node %s (team 0) received %d events, want %d", g.ID, got, want0)
		}
	}
	for _, g := range nodes1 {
		if got := len(g.GrainfileEvents()); got != want1 {
			t.Errorf("node %s (team 1) received %d events, want %d", g.ID, got, want1)
		}
	}
}

// TestGrainfileFault_CrossDeploymentDependsOnAndTriggerWithAcrossPods
// verifies cross-deployment `depends_on` and `trigger_with` event
// delivery across pod boundaries and node reconnects when nodes belong
// to different teams/deployments and their holding pods are killed.
func TestGrainfileFault_CrossDeploymentDependsOnAndTriggerWithAcrossPods(t *testing.T) {
	c := NewCluster(t, Options{Teams: 3, Pods: 3, Heartbeat: heartbeat})
	const (
		alice      = "alice@example.com"
		bob        = "bob@example.com"
		carol      = "carol@example.com"
		recipientA = "https://github.com/org/deploy-a"
		recipientB = "https://github.com/org/deploy-b"
		recipientC = "https://github.com/org/deploy-c"
	)

	// Set up three distinct team owners for the three deployments.
	c.Call("PUT", "/v1/teams/"+Team(0)+"/members/user:"+alice, Admin, map[string]string{"role": "admin"}, &struct{}{})
	c.Call("PUT", "/v1/teams/"+Team(1)+"/members/user:"+bob, Admin, map[string]string{"role": "admin"}, &struct{}{})
	c.Call("PUT", "/v1/teams/"+Team(2)+"/members/user:"+carol, Admin, map[string]string{"role": "admin"}, &struct{}{})

	upstreamURL := "https://github.com/" + Repo(0) + "/blob/main/processes/upstream.process.md"
	downstreamURL := "https://github.com/" + Repo(1) + "/blob/main/processes/downstream.process.md"

	nodeA := &Grain{
		ID:         "node-deploy-a",
		Owner:      alice,
		Hub:        c.Pods[0].URL,
		Hubs:       []string{c.Pods[0].URL, c.Pods[1].URL, c.Pods[2].URL},
		Repos:      []string{Repo(0)},
		Recipients: []string{recipientA},
		Slots:      2,
	}
	nodeB := &Grain{
		ID:         "node-deploy-b",
		Owner:      bob,
		Hub:        c.Pods[1].URL,
		Hubs:       []string{c.Pods[1].URL, c.Pods[2].URL, c.Pods[0].URL},
		Repos:      []string{Repo(1)},
		Recipients: []string{recipientB},
		Grainfiles: []string{upstreamURL},
		Slots:      2,
	}
	nodeC := &Grain{
		ID:         "node-deploy-c",
		Owner:      carol,
		Hub:        c.Pods[2].URL,
		Hubs:       []string{c.Pods[2].URL, c.Pods[0].URL, c.Pods[1].URL},
		Repos:      []string{Repo(2)},
		Recipients: []string{recipientC},
		Grainfiles: []string{upstreamURL, downstreamURL},
		Slots:      2,
	}
	c.AddGrain(nodeA)
	c.AddGrain(nodeB)
	c.AddGrain(nodeC)
	c.Eventually("nodeA, nodeB, nodeC online across 3 pods", 30*time.Second, func() bool { return c.AllOnline() == nil })

	// 1. Index upstream.process.md in Repo(0) (Team 0, targeting recipientA).
	shaA1 := "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	c.SetForgeFile(Repo(0), "processes/upstream.process.md", shaA1, fmt.Sprintf(`---
title: Upstream qualification process
recipient: %s
on:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Run upstream qualification.
`, recipientA))
	if code, body, err := c.SendWebhook(c.Pods[0].URL, Repo(0), "refs/heads/main", shaA1, []string{"processes/upstream.process.md"}, nil, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(upstream): %d %v %s", code, err, body)
	}

	// 2. Index downstream.process.md in Repo(1) (Team 1, targeting recipientB) with trigger_with and depends_on -> upstreamURL.
	shaB1 := "bbbb1111bbbb1111bbbb1111bbbb1111bbbb1111"
	c.SetForgeFile(Repo(1), "processes/downstream.process.md", shaB1, fmt.Sprintf(`---
title: Downstream verification process
recipient: %s
trigger_with:
  - %s
depends_on:
  - %s
---
Run downstream verification after upstream completes.
`, recipientB, upstreamURL, upstreamURL))
	if code, body, err := c.SendWebhook(c.Pods[1].URL, Repo(1), "refs/heads/main", shaB1, []string{"processes/downstream.process.md"}, nil, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(downstream): %d %v %s", code, err, body)
	}

	// 3. Index companion.task.md in Repo(2) (Team 2, targeting recipientC) with depends_on -> upstreamURL.
	shaC1 := "cccc1111cccc1111cccc1111cccc1111cccc1111"
	c.SetForgeFile(Repo(2), "tasks/companion.task.md", shaC1, fmt.Sprintf(`---
title: Companion deployment task
recipient: %s
depends_on:
  - %s
---
Run companion deployment once upstream is done.
`, recipientC, upstreamURL))
	if code, body, err := c.SendWebhook(c.Pods[2].URL, Repo(2), "refs/heads/main", shaC1, []string{"tasks/companion.task.md"}, nil, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(companion): %d %v %s", code, err, body)
	}

	// 4. Kill Pod 1 and Pod 2 while nodes hold subscriptions, forcing nodeB and nodeC to reconnect across pods,
	// then restart Pod 1 and Pod 2.
	_ = c.Pods[1].Kill()
	_ = c.Pods[2].Kill()
	c.Pods[1].Start(t)
	c.Pods[2].Start(t)
	c.Eventually("all nodes reconnected after Pod 1 & 2 crash", 30*time.Second, func() bool { return c.AllOnline() == nil })

	// 5. Push a code change to Repo(0) ("src/app.go") on Pod 0 -> triggers upstream.process.md (recipientA)
	// AND co-triggers downstream.process.md (recipientB via trigger_with) across pods!
	shaA2 := "aaaa2222aaaa2222aaaa2222aaaa2222aaaa2222"
	c.SetForgeFile(Repo(0), "processes/upstream.process.md", shaA2, fmt.Sprintf(`---
title: Upstream qualification process
recipient: %s
on:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Run upstream qualification.
`, recipientA))
	if code, body, err := c.SendWebhook(c.Pods[0].URL, Repo(0), "refs/heads/main", shaA2, nil, []string{"src/app.go"}, nil); err != nil || code != http.StatusOK {
		t.Fatalf("SendWebhook(src/app.go trigger): %d %v %s", code, err, body)
	}

	c.Eventually("nodeA receives primary process_triggered and nodeB receives trigger_with process_triggered", 20*time.Second, func() bool {
		hasPrimary := false
		for _, ev := range nodeA.GrainfileEvents() {
			if ev.Type == grainfiles.EventProcessTriggered && ev.GrainfileURL == upstreamURL && ev.TriggerSource == grainfiles.TriggerPush {
				hasPrimary = true
			}
		}
		hasCoTrigger := false
		for _, ev := range nodeB.GrainfileEvents() {
			if ev.Type == grainfiles.EventProcessTriggered && ev.GrainfileURL == downstreamURL && ev.TriggerSource == grainfiles.TriggerWith && ev.TriggeredBy == upstreamURL {
				hasCoTrigger = true
			}
		}
		return hasPrimary && hasCoTrigger
	})

	// 6. Kill Pod 0, let nodeA reconnect, and have nodeA report completion of upstream.process.md ->
	// must fan out dependency_completed across pods to both nodeB (recipientB) and nodeC (recipientC)!
	_ = c.Pods[0].Kill()
	c.Pods[0].Start(t)
	c.Eventually("nodeA reconnected after Pod 0 kill", 30*time.Second, func() bool { return nodeA.Online() && c.AllOnline() == nil })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := nodeA.CompleteGrainfile(ctx, upstreamURL, shaA2, "done", "upstream passed"); err != nil {
		t.Fatalf("nodeA.CompleteGrainfile: %v", err)
	}

	c.Eventually("nodeB and nodeC receive cross-deployment dependency_completed", 20*time.Second, func() bool {
		bSaw, cSaw := false, false
		for _, ev := range nodeB.GrainfileEvents() {
			if ev.Type == grainfiles.EventDependencyCompleted && ev.DependencyURL == upstreamURL && ev.DependencyState == "done" {
				bSaw = true
			}
		}
		for _, ev := range nodeC.GrainfileEvents() {
			if ev.Type == grainfiles.EventDependencyCompleted && ev.DependencyURL == upstreamURL && ev.DependencyState == "done" {
				cSaw = true
			}
		}
		return bSaw && cSaw
	})

	for _, g := range []*Grain{nodeA, nodeB, nodeC} {
		if dups := g.DuplicateGrainfileEvents(); dups != 0 {
			t.Errorf("node %s received %d duplicate grainfile_events", g.ID, dups)
		}
	}
}
