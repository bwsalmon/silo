package faulttest

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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
