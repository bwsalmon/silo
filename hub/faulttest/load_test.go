package faulttest

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
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
// docs/test-runs/2026-10-16-hub-load.md with room for a slower runner:
// about 100× the measured p99s, and 4× the peak RSS.
const (
	ciDispatchP99 = 500 * time.Millisecond
	ciFanoutP99   = 250 * time.Millisecond
	ciPeakRSS     = 256 << 20
)

// TestLoad runs the hub under sustained load: Grains across teams,
// tasks submitted at a steady rate, each streaming events, and
// subscribers following them. It reports dispatch and fan-out latency,
// the hub's CPU and memory, and the database's growth, and checks the
// same invariants as the fault scenarios.
//
// Without SILO_LOAD it is the CI-sized version, held to the ci*
// thresholds; with SILO_LOAD=1 it is the full size (500 Grains, 50
// teams, 10k events/s, 1k subscribers), and with SILO_LOAD=burst the
// same Grains taking 300 short tasks a second, past what a dispatcher
// making a round trip per task in turn could place; both only report.
// SILO_LOAD_DB_LATENCY (a duration) delays each chunk the hub and the
// database send each other by that much, as a database across a network
// would. With SILO_LOAD_REPORT set, the report is also written to that
// file.
func TestLoad(t *testing.T) {
	l := load{teams: 5, grainsPerTeam: 10, slots: 4, rate: 10, duration: 20 * time.Second,
		work: 10 * time.Second, events: 20, tokens: 4, subscribers: 100}
	big := os.Getenv("SILO_LOAD") != ""
	switch os.Getenv("SILO_LOAD") {
	case "":
	case "burst":
		l = load{teams: 50, grainsPerTeam: 10, slots: 4, rate: 300, duration: 20 * time.Second,
			work: 4 * time.Second, events: 4, tokens: 4, subscribers: 200}
	default:
		l = load{teams: 50, grainsPerTeam: 10, slots: 4, rate: 50, duration: 60 * time.Second,
			work: 20 * time.Second, events: 40, tokens: 4, subscribers: 1000}
	}
	start := time.Now()
	c := NewCluster(t, Options{Teams: l.teams, GrainsPerTeam: l.grainsPerTeam, Slots: l.slots, Heartbeat: 5 * time.Second,
		Grain: Work{Work: l.work, Events: l.events, Tokens: l.tokens, Pad: 64}})
	connected := time.Since(start)
	if v := os.Getenv("SILO_LOAD_DB_LATENCY"); v != "" {
		lat, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("SILO_LOAD_DB_LATENCY: %v", err)
		}
		c.PGProxy.Degrade(lat, 0)
	}
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

func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// TestLoad_Grainfiles exercises concurrent signed GitHub webhooks indexing
// hundreds/thousands of .task.md and .process.md files across multiple
// repositories, concurrent /v1/grainfiles search and filter queries
// (kind, recipient, repo, draft), fan-out of grainfile_event notifications
// to connected Grain nodes, and concurrent /v1/grainfiles/content same-repo
// reference fetches, asserting latency p50/p95/p99 and zero errors or
// leaked goroutines/file descriptors.
func TestLoad_Grainfiles(t *testing.T) {
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseFDs := countOpenFDs()

	big := os.Getenv("SILO_LOAD") != ""
	teams := 5
	nodesPerTeam := 8 // 40 connected Grain nodes in CI, 200 under SILO_LOAD=1
	webhooks := 25    // 25 webhooks * 8 files = 200 Grainfiles in CI; 100 * 10 = 1,000 under SILO_LOAD=1
	filesPerWebhook := 8
	searchQueries := 100
	contentFetches := 60
	concurrency := 8
	if big {
		teams = 10
		nodesPerTeam = 20
		webhooks = 100
		filesPerWebhook = 10
		searchQueries = 400
		contentFetches = 200
		concurrency = 16
	}

	var (
		webhookLatencies []time.Duration
		searchLatencies  []time.Duration
		contentLatencies []time.Duration
		fanoutLatencies  []time.Duration
		webhookErrs      atomic.Int64
		searchErrs       atomic.Int64
		contentErrs      atomic.Int64
		reportText       string
	)

	// Run the cluster inside a sub-scope or explicit cleanup so we can verify
	// goroutine and file-descriptor reclamation after the cluster shuts down.
	func() {
		c := NewCluster(t, Options{Teams: teams, Pods: 2, Heartbeat: 5 * time.Second})
		defer func() {
			c.cancel()
			c.wg.Wait()
			for _, p := range c.Pods {
				_ = p.Stop()
			}
			c.DB.Close()
			c.PGProxy.Close()
			c.Forge.Close()
		}()

		recipients := make([]string, teams)
		teamNodes := make([][]*Grain, teams)
		for tm := range teams {
			recipients[tm] = fmt.Sprintf("https://github.com/org/agent-team-%02d", tm)
			c.SetForgeFile(Repo(tm), "docs/spec.md", "", fmt.Sprintf("# Specification for %s\nShared reference document.\n", Repo(tm)))
			for j := range nodesPerTeam {
				g := &Grain{
					ID:         fmt.Sprintf("gf-load-%02d-%02d", tm, j),
					Repos:      []string{Repo(tm)},
					Recipients: []string{recipients[tm]},
					Slots:      2,
				}
				c.AddGrain(g)
				teamNodes[tm] = append(teamNodes[tm], g)
			}
		}
		c.Eventually("all Grainfile load nodes online", 2*time.Minute, func() bool { return c.AllOnline() == nil })

		var (
			latMu sync.Mutex
			wg    sync.WaitGroup
		)

		// 1. Concurrent signed GitHub webhooks indexing .task.md and .process.md files across repositories.
		type indexedTarget struct {
			team      int
			repo      string
			commit    string
			grainfile string
		}
		var (
			targetsMu sync.Mutex
			targets   []indexedTarget
		)

		whSem := make(chan struct{}, concurrency)
		for wIdx := range webhooks {
			wg.Add(1)
			go func(wIdx int) {
				defer wg.Done()
				whSem <- struct{}{}
				defer func() { <-whSem }()

				tm := wIdx % teams
				repo := Repo(tm)
				recipient := recipients[tm]
				commit := fmt.Sprintf("%040d", wIdx+1)
				var added []string
				for fIdx := range filesPerWebhook {
					isDraft := fIdx == filesPerWebhook-1 // 1 draft per webhook, rest active
					if fIdx%2 == 0 {
						path := fmt.Sprintf("tasks/w%03d-f%02d.task.md", wIdx, fIdx)
						content := fmt.Sprintf("---\ntitle: Load task %d-%d\nrecipient: %s\ndraft: %v\nreferences:\n  - /docs/spec.md\n---\nTask body %d-%d.\n",
							wIdx, fIdx, recipient, isDraft, wIdx, fIdx)
						c.SetForgeFile(repo, path, commit, content)
						added = append(added, path)
						if !isDraft {
							targetsMu.Lock()
							targets = append(targets, indexedTarget{
								team:      tm,
								repo:      repo,
								commit:    commit,
								grainfile: fmt.Sprintf("https://github.com/%s/blob/main/%s", repo, path),
							})
							targetsMu.Unlock()
						}
					} else {
						path := fmt.Sprintf("processes/w%03d-f%02d.process.md", wIdx, fIdx)
						content := fmt.Sprintf("---\ntitle: Load process %d-%d\nrecipient: %s\ndraft: %v\nreferences:\n  - /docs/spec.md\non:\n  manual: {}\n---\nProcess body %d-%d.\n",
							wIdx, fIdx, recipient, isDraft, wIdx, fIdx)
						c.SetForgeFile(repo, path, commit, content)
						added = append(added, path)
					}
				}
				c.SetForgeFile(repo, "docs/spec.md", commit, fmt.Sprintf("# Specification for %s at %s\n", repo, commit))

				podURL := c.Pods[wIdx%len(c.Pods)].URL
				t0 := time.Now()
				code, _, err := c.SendWebhook(podURL, repo, "refs/heads/main", commit, added, nil, nil)
				dur := time.Since(t0)
				if err != nil || code != http.StatusOK {
					webhookErrs.Add(1)
					return
				}
				latMu.Lock()
				webhookLatencies = append(webhookLatencies, dur)
				latMu.Unlock()
			}(wIdx)
		}

		// 2. Concurrent /v1/grainfiles search and filter queries (kind, recipient, repo, draft).
		qSem := make(chan struct{}, concurrency)
		for qIdx := range searchQueries {
			wg.Add(1)
			go func(qIdx int) {
				defer wg.Done()
				qSem <- struct{}{}
				defer func() { <-qSem }()

				tm := qIdx % teams
				v := url.Values{}
				switch qIdx % 4 {
				case 0:
					v.Set("kind", "task")
					v.Set("repo", Repo(tm))
				case 1:
					v.Set("kind", "process")
					v.Set("recipient", recipients[tm])
				case 2:
					v.Set("draft", "false")
					v.Set("repo", Repo(tm))
				case 3:
					v.Set("draft", "true")
					v.Set("include_drafts", "true")
				}
				podURL := c.Pods[qIdx%len(c.Pods)].URL
				t0 := time.Now()
				code, _, err := call("GET", podURL+grainfiles.GrainfilesPath+"?"+v.Encode(), Ops, nil)
				dur := time.Since(t0)
				if err != nil || code != http.StatusOK {
					searchErrs.Add(1)
					return
				}
				latMu.Lock()
				searchLatencies = append(searchLatencies, dur)
				latMu.Unlock()
			}(qIdx)
		}

		wg.Wait()

		// 3. Concurrent /v1/grainfiles/content same-repo reference fetches by connected nodes.
		targetsMu.Lock()
		snapTargets := append([]indexedTarget(nil), targets...)
		targetsMu.Unlock()
		if len(snapTargets) == 0 {
			t.Fatal("no non-draft task targets indexed during webhook load phase")
		}

		var contentWG sync.WaitGroup
		cSem := make(chan struct{}, concurrency)
		for cIdx := range contentFetches {
			contentWG.Add(1)
			go func(cIdx int) {
				defer contentWG.Done()
				cSem <- struct{}{}
				defer func() { <-cSem }()

				tgt := snapTargets[cIdx%len(snapTargets)]
				node := teamNodes[tgt.team][cIdx%len(teamNodes[tgt.team])]
				podURL := c.Pods[cIdx%len(c.Pods)].URL
				v := url.Values{}
				v.Set("grainfile_url", tgt.grainfile)
				v.Set("commit", tgt.commit)
				v.Set("target", "/docs/spec.md")

				req, err := http.NewRequest("GET", podURL+grainfiles.ContentPath+"?"+v.Encode(), nil)
				if err != nil {
					contentErrs.Add(1)
					return
				}
				req.Header.Set("Authorization", "Bearer "+node.Token)
				t0 := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					contentErrs.Add(1)
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				dur := time.Since(t0)
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Specification for") {
					contentErrs.Add(1)
					return
				}
				latMu.Lock()
				contentLatencies = append(contentLatencies, dur)
				latMu.Unlock()
			}(cIdx)
		}
		contentWG.Wait()

		// 4. Verify fan-out of grainfile_event notifications to all connected Grain nodes.
		nonDraftPerWebhook := filesPerWebhook - 1
		webhooksPerTeam := webhooks / teams
		wantEventsPerNode := webhooksPerTeam * nonDraftPerWebhook
		c.Eventually(fmt.Sprintf("all %d nodes receive %d grainfile_events each", teams*nodesPerTeam, wantEventsPerNode), 60*time.Second, func() bool {
			for tm := range teams {
				for _, g := range teamNodes[tm] {
					if len(g.GrainfileEvents()) < wantEventsPerNode {
						return false
					}
				}
			}
			return true
		})

		totalDelivered := 0
		for tm := range teams {
			for _, g := range teamNodes[tm] {
				if dups := g.DuplicateGrainfileEvents(); dups != 0 {
					t.Errorf("node %s received %d duplicate grainfile_events", g.ID, dups)
				}
				evs := g.GrainfileEvents()
				totalDelivered += len(evs)
				fanoutLatencies = append(fanoutLatencies, g.GrainfileLatencies()...)
			}
		}

		var b strings.Builder
		fmt.Fprintf(&b, "\n--- Grainfiles Load Report ---\n")
		fmt.Fprintf(&b, "indexed %d grainfiles across %d repos (%d webhooks × %d files); %d connected Grain nodes; %d total events fanned out\n",
			webhooks*filesPerWebhook, teams, webhooks, filesPerWebhook, teams*nodesPerTeam, totalDelivered)
		fmt.Fprintf(&b, "webhook ingest latency: p50 %s p95 %s p99 %s over %d webhooks (%d errors)\n",
			Percentile(webhookLatencies, 50).Round(time.Millisecond),
			Percentile(webhookLatencies, 95).Round(time.Millisecond),
			Percentile(webhookLatencies, 99).Round(time.Millisecond),
			len(webhookLatencies), webhookErrs.Load())
		fmt.Fprintf(&b, "search /v1/grainfiles latency: p50 %s p95 %s p99 %s over %d queries (%d errors)\n",
			Percentile(searchLatencies, 50).Round(time.Millisecond),
			Percentile(searchLatencies, 95).Round(time.Millisecond),
			Percentile(searchLatencies, 99).Round(time.Millisecond),
			len(searchLatencies), searchErrs.Load())
		fmt.Fprintf(&b, "content /v1/grainfiles/content latency: p50 %s p95 %s p99 %s over %d fetches (%d errors)\n",
			Percentile(contentLatencies, 50).Round(time.Millisecond),
			Percentile(contentLatencies, 95).Round(time.Millisecond),
			Percentile(contentLatencies, 99).Round(time.Millisecond),
			len(contentLatencies), contentErrs.Load())
		fmt.Fprintf(&b, "grainfile_event fan-out latency: p50 %s p95 %s p99 %s over %d deliveries\n",
			Percentile(fanoutLatencies, 50).Round(time.Millisecond),
			Percentile(fanoutLatencies, 95).Round(time.Millisecond),
			Percentile(fanoutLatencies, 99).Round(time.Millisecond),
			len(fanoutLatencies))
		reportText = b.String()
		t.Log(reportText)
	}()

	if p := os.Getenv("SILO_LOAD_REPORT"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = f.WriteString(reportText)
			_ = f.Close()
		}
	}

	if webhookErrs.Load() > 0 {
		t.Errorf("webhook ingestion had %d errors, want 0", webhookErrs.Load())
	}
	if searchErrs.Load() > 0 {
		t.Errorf("/v1/grainfiles search had %d errors, want 0", searchErrs.Load())
	}
	if contentErrs.Load() > 0 {
		t.Errorf("/v1/grainfiles/content fetches had %d errors, want 0", contentErrs.Load())
	}

	maxSearchP99 := 2 * time.Second
	maxContentP99 := 2 * time.Second
	maxFanoutP99 := 10 * time.Second
	if big {
		maxSearchP99 = 5 * time.Second
		maxContentP99 = 5 * time.Second
		maxFanoutP99 = 30 * time.Second
	}
	if p50, p95, p99 := Percentile(searchLatencies, 50), Percentile(searchLatencies, 95), Percentile(searchLatencies, 99); p50 <= 0 || p95 <= 0 || p99 > maxSearchP99 {
		t.Errorf("/v1/grainfiles search latency out of bounds: p50=%s p95=%s p99=%s (max p99 %s)", p50, p95, p99, maxSearchP99)
	}
	if p50, p95, p99 := Percentile(contentLatencies, 50), Percentile(contentLatencies, 95), Percentile(contentLatencies, 99); p50 <= 0 || p95 <= 0 || p99 > maxContentP99 {
		t.Errorf("/v1/grainfiles/content fetch latency out of bounds: p50=%s p95=%s p99=%s (max p99 %s)", p50, p95, p99, maxContentP99)
	}
	if p50, p95, p99 := Percentile(fanoutLatencies, 50), Percentile(fanoutLatencies, 95), Percentile(fanoutLatencies, 99); p50 <= 0 || p95 <= 0 || p99 > maxFanoutP99 {
		t.Errorf("grainfile_event fan-out latency out of bounds: p50=%s p95=%s p99=%s (max p99 %s)", p50, p95, p99, maxFanoutP99)
	}

	// Assert zero leaked goroutines or file descriptors after cluster shutdown.
	client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseGoroutines+12 && countOpenFDs() <= baseFDs+16 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	afterGoroutines := runtime.NumGoroutine()
	afterFDs := countOpenFDs()
	if afterGoroutines > baseGoroutines+12 {
		t.Errorf("leaked goroutines: before=%d after=%d", baseGoroutines, afterGoroutines)
	}
	if baseFDs > 0 && afterFDs > baseFDs+16 {
		t.Errorf("leaked file descriptors: before=%d after=%d", baseFDs, afterFDs)
	}
}
