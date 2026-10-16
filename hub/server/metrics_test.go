package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bwsalmon/silo/hub/tasks"
)

func scrape(t *testing.T, h *Hub) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", MetricsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// TestMetrics checks the metrics a scrape reads from the hub's state
// and those its parts count, and that the hub's own listener, which the
// load balancer exposes, does not serve them.
func TestMetrics(t *testing.T) {
	ctx := context.Background()
	h, err := New(ctx, Config{Database: "sqlite::memory:", DevUserHeader: userHeader,
		HubAdmins: []string{"admin@example.com"}, Logf: logf(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	call(t, "POST", srv.URL+"/v1/teams", "admin@example.com", map[string]string{"id": "infra"}, &struct{}{})
	call(t, "PUT", srv.URL+"/v1/teams/infra/members/user:alice@example.com", "admin@example.com", map[string]string{"role": "admin"}, &struct{}{})
	call(t, "PUT", srv.URL+"/v1/teams/infra/repos/bwsalmon/silo", "admin@example.com", nil, &struct{}{})
	var queued, offered tasks.Task
	call(t, "POST", srv.URL+tasks.TasksPath, "alice@example.com", map[string]string{"repo": "bwsalmon/silo", "prompt": "one"}, &queued)
	call(t, "POST", srv.URL+tasks.TasksPath, "alice@example.com", map[string]string{"repo": "bwsalmon/silo", "prompt": "two"}, &offered)
	if _, err := h.Backlog.Offer(ctx, offered.ID, "ws-1"); err != nil {
		t.Fatal(err)
	}

	// The team's settings, through the API.
	var set struct {
		Seconds int64 `json:"lost_requeue_after_seconds"`
	}
	call(t, "PUT", srv.URL+"/v1/teams/infra/settings", "alice@example.com", map[string]int{"lost_requeue_after_seconds": 1800}, &set)
	call(t, "GET", srv.URL+"/v1/teams/infra/settings", "alice@example.com", nil, &set)
	if set.Seconds != 1800 {
		t.Errorf("settings: %+v", set)
	}
	if code := call(t, "PUT", srv.URL+"/v1/teams/infra/settings", "alice@example.com", map[string]int{"lost_requeue_after_seconds": 60}, nil); code != http.StatusBadRequest {
		t.Errorf("a grace of a minute: %d, want 400", code)
	}
	if code := call(t, "PUT", srv.URL+"/v1/teams/infra/settings", "bob@example.com", map[string]int{"lost_requeue_after_seconds": 0}, nil); code != http.StatusNotFound {
		t.Errorf("settings by a non-member: %d, want 404", code)
	}

	// Events streamed up by the node the task was offered to: one kept,
	// one rejected by its schema.
	hubEnd, nodeEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		h.Live.NodeStream("ws-1", hubEnd)
		close(done)
	}()
	io.WriteString(nodeEnd, `{"v":1,"seq":1,"type":"status","task":"`+offered.ID+`","status":{"phase":"setup","step":"tools","state":"done"}}`+"\n")
	io.WriteString(nodeEnd, `{"v":1,"seq":2,"type":"status","task":"`+offered.ID+`","status":{"phase":"setup","state":"done"},"diff":"package main"}`+"\n")
	nodeEnd.Close()
	<-done

	// Not on the hub's own routes, signed in or not.
	for _, user := range []string{"", "admin@example.com"} {
		if code := call(t, "GET", srv.URL+MetricsPath, user, nil, nil); code == http.StatusOK {
			t.Errorf("the hub's own listener served %s to %q", MetricsPath, user)
		}
	}

	body := scrape(t, h)
	for _, want := range []string{
		`silo_hub_ready 1`,
		`silo_hub_database_up 1`,
		`silo_hub_nodes{status="online"} 0`,
		`silo_hub_team_nodes{status="online",team="infra"} 0`,
		`silo_hub_team_free_slots{team="infra"} 0`,
		`silo_hub_backlog_tasks{state="queued",team="infra"} 1`,
		`silo_hub_backlog_tasks{state="offered",team="infra"} 1`,
		`silo_hub_backlog_oldest_queued_seconds{team="infra"}`,
		`silo_hub_event_store_events 1`,
		`silo_hub_events_ingested_total{class="lifecycle",result="kept"} 1`,
		`silo_hub_events_ingested_total{class="unknown",result="rejected"} 1`,
		`silo_hub_http_requests_total{code="201",route="POST /v1/tasks"} 2`,
		`silo_hub_http_requests_total{code="400",route="PUT /v1/teams/{team}/settings"} 1`,
		`silo_hub_http_requests_total{code="404",route="unmatched"}`,
		`silo_hub_store_errors_total{store="database"} 0`,
		`silo_hub_dispatch_events_total{event="lost_requeued"} 0`,
		`silo_hub_subscribers 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape has no %s", want)
		}
	}

	// Draining turns it unready.
	h.draining.Store(true)
	if body := scrape(t, h); !strings.Contains(body, "silo_hub_ready 0") {
		t.Error("a draining hub's scrape says it is ready")
	}
}

// metricNames are the names the hub's own collectors describe.
func metricNames(t *testing.T, h *Hub) map[string]bool {
	t.Helper()
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		for _, c := range h.metrics.own {
			c.Describe(ch)
		}
		close(ch)
	}()
	fq := regexp.MustCompile(`fqName: "([a-z_:]+)"`)
	names := map[string]bool{}
	for d := range ch {
		m := fq.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("no name in %s", d)
		}
		names[m[1]] = true
	}
	return names
}

// TestMonitoringNamesMetrics checks that every hub metric the alert
// policies and the dashboard (deploy/terraform/hub) query is one the
// hub exports, so that a renamed metric fails here rather than leaving
// an alert that never fires.
func TestMonitoringNamesMetrics(t *testing.T) {
	h, err := New(context.Background(), Config{Database: "sqlite::memory:", DevUserHeader: userHeader})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	have := metricNames(t, h)

	files, _ := filepath.Glob("../../deploy/terraform/hub/monitoring*")
	more, _ := filepath.Glob("../../deploy/terraform/hub/dashboards/*.json")
	files = append(files, more...)
	if len(files) < 2 {
		t.Fatalf("found only %v", files)
	}
	ref := regexp.MustCompile(`silo_hub_[a-z_]+`)
	used := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range ref.FindAllString(string(b), -1) {
			used[name] = true
			base := name
			for _, suffix := range []string{"_bucket", "_count", "_sum"} {
				base = strings.TrimSuffix(base, suffix)
			}
			if !have[name] && !have[base] {
				t.Errorf("%s queries %s, which the hub does not export", filepath.Base(f), name)
			}
		}
	}
	var unused []string
	for name := range have {
		if !used[name] && !used[name+"_bucket"] && !used[name+"_count"] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	t.Logf("metrics neither alerted on nor charted: %v", unused)
}
