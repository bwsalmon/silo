package faulttest

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// load is one load test's shape.
type load struct {
	teams, grainsPerTeam, slots int
	// rate is the tasks submitted per second, for duration.
	rate     int
	duration time.Duration
	// Each task runs for work, emitting events status events with
	// tokens transcript events after each.
	work           time.Duration
	events, tokens int
	// subscribers is how many tasks are each followed by a subscriber.
	subscribers int
}

// eventsPerSecond is the ingest the shape aims at, once its intake has
// filled the Grains.
func (l load) eventsPerSecond() float64 {
	perTask := float64(l.events*(1+l.tokens)+1) / l.work.Seconds()
	return perTask * float64(l.rate) * l.work.Seconds()
}

// The thresholds the CI-sized run is held to, set from the results in
// docs/test-runs/2026-10-16-hub-load.md with room for a slower runner.
const (
	ciDispatchP99 = 2 * time.Second
	ciFanoutP99   = time.Second
	ciPeakRSS     = 512 << 20
)

// TestLoad runs the hub under sustained load: Grains across teams,
// tasks submitted at a steady rate, each streaming events, and
// subscribers following them. It reports dispatch and fan-out latency,
// the hub's CPU and memory, and the database's growth, and checks the
// same invariants as the fault scenarios.
//
// Without SILO_LOAD it is the CI-sized version, held to the ci*
// thresholds; with SILO_LOAD=1 it is the full size (500 Grains, 50
// teams, 10k events/s, 1k subscribers), which only reports. With
// SILO_LOAD_REPORT set, the report is also written to that file.
func TestLoad(t *testing.T) {
	l := load{teams: 5, grainsPerTeam: 10, slots: 4, rate: 10, duration: 20 * time.Second,
		work: 10 * time.Second, events: 20, tokens: 4, subscribers: 100}
	big := os.Getenv("SILO_LOAD") != ""
	if big {
		l = load{teams: 50, grainsPerTeam: 10, slots: 4, rate: 50, duration: 60 * time.Second,
			work: 20 * time.Second, events: 40, tokens: 4, subscribers: 1000}
	}
	start := time.Now()
	c := NewCluster(t, Options{Teams: l.teams, GrainsPerTeam: l.grainsPerTeam, Slots: l.slots, Heartbeat: 5 * time.Second,
		Grain: Work{Work: l.work, Events: l.events, Tokens: l.tokens, Pad: 64}})
	connected := time.Since(start)
	dbBefore := c.DBSize()
	cpuBefore, _, _ := c.Hub.Usage()

	// Intake: rate tasks a second round the teams, from a few
	// submitters, each task followed by a subscriber until there are
	// enough.
	var (
		mu       sync.Mutex
		watchers []*Watcher
		wg       sync.WaitGroup
		subErrs  int
	)
	intakeStart := time.Now()
	total := l.rate * int(l.duration.Seconds())
	tick := time.NewTicker(time.Second / time.Duration(l.rate))
	for i := range total {
		<-tick.C
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := c.Submit(i % l.teams)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				subErrs++
				return
			}
			if len(watchers) < l.subscribers {
				watchers = append(watchers, c.Watch(id, 0))
			}
		}()
	}
	tick.Stop()
	wg.Wait()
	intake := time.Since(intakeStart)
	cpuIntake, rssIntake, _ := c.Hub.Usage()
	c.Settle(l.work + 3*time.Minute)
	wall := time.Since(intakeStart)
	cpuAfter, _, peak := c.Hub.Usage()
	dbAfter := c.DBSize()
	watchersEnd(t, watchers)

	var fanout []time.Duration
	for _, w := range watchers {
		_, _, _, lat := w.Stats()
		fanout = append(fanout, lat...)
	}
	disp := c.Ledger.DispatchLatencies()
	tasksDone := len(c.Ledger.Submitted())
	var b strings.Builder
	fmt.Fprintf(&b, "grains %d (%d teams × %d, %d slots each), connected in %s\n", l.teams*l.grainsPerTeam, l.teams, l.grainsPerTeam, l.slots, connected.Round(time.Millisecond))
	fmt.Fprintf(&b, "intake %d tasks at %d/s over %s (%d submit errors); each %s, %d events (%d stored)\n",
		tasksDone, l.rate, intake.Round(time.Millisecond), subErrs, l.work, l.events*(1+l.tokens)+1, l.events+1)
	fmt.Fprintf(&b, "target ingest %.0f events/s at steady state; %d events stored, %.0f events/s ingested on average over %s\n",
		l.eventsPerSecond(), tasksDone*(l.events+1), float64(tasksDone*(l.events*(1+l.tokens)+1))/wall.Seconds(), wall.Round(time.Second))
	fmt.Fprintf(&b, "dispatch latency (submit → offer at the Grain): p50 %s p99 %s max %s over %d tasks\n",
		Percentile(disp, 50).Round(time.Millisecond), Percentile(disp, 99).Round(time.Millisecond), Percentile(disp, 100).Round(time.Millisecond), len(disp))
	fmt.Fprintf(&b, "fan-out latency (Grain write → subscriber read): p50 %s p99 %s max %s over %d events, %d subscribers\n",
		Percentile(fanout, 50).Round(time.Millisecond), Percentile(fanout, 99).Round(time.Millisecond), Percentile(fanout, 100).Round(time.Millisecond), len(fanout), len(watchers))
	fmt.Fprintf(&b, "hub CPU: %.2f cores during intake, %.2f cores overall; RSS %d MiB at the end of intake, peak %d MiB\n",
		(cpuIntake-cpuBefore).Seconds()/intake.Seconds(), (cpuAfter-cpuBefore).Seconds()/wall.Seconds(), rssIntake>>20, peak>>20)
	fmt.Fprintf(&b, "database: %d MiB before, %d MiB after: %.1f KiB per task\n", dbBefore>>20, dbAfter>>20, float64(dbAfter-dbBefore)/1024/float64(max(tasksDone, 1)))
	t.Log("\n" + b.String())
	if p := os.Getenv("SILO_LOAD_REPORT"); p != "" {
		os.WriteFile(p, []byte(b.String()), 0o644)
	}
	if big {
		return
	}
	if p := Percentile(disp, 99); p > ciDispatchP99 {
		t.Errorf("dispatch p99 %s, over the %s threshold", p, ciDispatchP99)
	}
	if p := Percentile(fanout, 99); p > ciFanoutP99 {
		t.Errorf("fan-out p99 %s, over the %s threshold", p, ciFanoutP99)
	}
	if peak > ciPeakRSS {
		t.Errorf("hub peak RSS %d MiB, over the %d MiB threshold", peak>>20, ciPeakRSS>>20)
	}
	if subErrs > 0 {
		t.Errorf("%d submissions failed", subErrs)
	}
}
