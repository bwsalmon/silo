package server

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bwsalmon/silo/hub/lease"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// MetricsPath is where the metrics listener (Config.MetricsAddr) serves
// the hub's Prometheus metrics. It is never served on the hub's own
// listener, which the load balancer exposes (docs/hub-operations.md).
const MetricsPath = "/metrics"

// scrapeTimeout bounds the database reads a scrape makes.
const scrapeTimeout = 5 * time.Second

// metrics are the hub's Prometheus metrics (docs/hub-operations.md
// lists them). Counters and histograms are updated as things happen,
// through the parts' Observe hooks; the gauges that describe state
// (nodes, the backlog, the event store, the database) are read when
// the hub is scraped, by stateCollector.
type metrics struct {
	reg *prometheus.Registry
	// own are the hub's own collectors, without the Go runtime's and
	// the process's.
	own []prometheus.Collector

	connects, disconnects, refusals prometheus.Counter
	dispatch                        *prometheus.CounterVec
	dispatchLatency                 prometheus.Histogram
	ingested                        *prometheus.CounterVec
	subscribers                     prometheus.Gauge
	views                           *prometheus.CounterVec
	viewLatency                     prometheus.Histogram
	requests                        *prometheus.CounterVec
	storeErrors                     *prometheus.CounterVec
	leader                          prometheus.Gauge
	terms                           prometheus.Counter
}

func newMetrics(h *Hub) *metrics {
	m := &metrics{reg: prometheus.NewRegistry()}
	m.connects = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "silo_hub_node_connects_total", Help: "Node connections welcomed.",
	})
	m.disconnects = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "silo_hub_node_disconnects_total", Help: "Welcomed node connections that ended.",
	})
	m.refusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "silo_hub_node_refusals_total", Help: "Node connections refused: unauthenticated, not admitted, or another owner's node ID.",
	})
	m.dispatch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "silo_hub_dispatch_events_total",
		Help: "Dispatcher events: accepted, resumed, refused, timeout (an offer or resume unanswered), unreachable, lost, lost_requeued.",
	}, []string{"event"})
	m.dispatchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "silo_hub_dispatch_latency_seconds",
		Help:    "From a task's submission to a node accepting it.",
		Buckets: []float64{0.1, 0.5, 1, 5, 15, 30, 60, 300, 900, 1800, 3600, 4 * 3600, 24 * 3600},
	})
	m.ingested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "silo_hub_events_ingested_total",
		Help: "Events nodes streamed up, by class and result: kept (stored or broadcast), rejected (by schema), error (the store failed).",
	}, []string{"class", "result"})
	m.subscribers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "silo_hub_subscribers", Help: "Live event subscriptions (SSE and WebSocket) open now.",
	})
	m.views = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "silo_hub_view_requests_total", Help: "Artifact view-through requests, by status code.",
	}, []string{"code"})
	m.viewLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "silo_hub_view_latency_seconds",
		Help:    "From a view-through request to the hub answering it: the node's first answer, or the error.",
		Buckets: prometheus.DefBuckets,
	})
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "silo_hub_http_requests_total", Help: "HTTP requests answered, by route pattern and status code.",
	}, []string{"route", "code"})
	m.storeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "silo_hub_store_errors_total",
		Help: "Database errors, by store: backlog (the dispatcher's reads and updates), events (keeping streamed events), database (pings and the scrape's reads).",
	}, []string{"store"})
	m.leader = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "silo_hub_dispatcher_leader", Help: "1 while this pod holds the dispatcher's lease and runs the dispatcher, 0 otherwise.",
	})
	m.terms = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "silo_hub_dispatcher_terms_total", Help: "Terms of the dispatcher's lease this pod has taken: each one recovers the backlog.",
	})
	m.own = []prometheus.Collector{m.connects, m.disconnects, m.refusals, m.dispatch, m.dispatchLatency,
		m.ingested, m.subscribers, m.views, m.viewLatency, m.requests, m.storeErrors, m.leader, m.terms,
		&stateCollector{h: h, m: m}}
	m.reg.MustRegister(m.own...)
	m.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	// Zeroes, so that a rate over them exists before the first error.
	for _, s := range []string{"backlog", "events", "database"} {
		m.storeErrors.WithLabelValues(s)
	}
	for _, ev := range []tasks.Event{tasks.EventAccepted, tasks.EventResumed, tasks.EventRefused, tasks.EventTimeout,
		tasks.EventUnreachable, tasks.EventLost, tasks.EventLostRequeued} {
		m.dispatch.WithLabelValues(string(ev))
	}
	return m
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

func (m *metrics) observeRendezvous(ev rendezvous.Event) {
	switch ev {
	case rendezvous.EventConnected:
		m.connects.Inc()
	case rendezvous.EventDisconnected:
		m.disconnects.Inc()
	case rendezvous.EventRefused:
		m.refusals.Inc()
	}
}

// observeTerm is a term of the dispatcher's lease starting or ending.
func (m *metrics) observeTerm(_ lease.Term, started bool) {
	if started {
		m.terms.Inc()
		m.leader.Set(1)
		return
	}
	m.leader.Set(0)
}

func (m *metrics) observeDispatch(ev tasks.Event, t tasks.Task) {
	if ev == tasks.EventStoreError {
		m.storeErrors.WithLabelValues("backlog").Inc()
		return
	}
	m.dispatch.WithLabelValues(string(ev)).Inc()
	if ev == tasks.EventAccepted && !t.Created.IsZero() {
		m.dispatchLatency.Observe(time.Since(t.Created).Seconds())
	}
}

func (m *metrics) observeIngest(c live.Class, err error) {
	switch {
	case err == nil:
		m.ingested.WithLabelValues(c.String(), "kept").Inc()
	case errors.Is(err, live.ErrRejected):
		m.ingested.WithLabelValues("unknown", "rejected").Inc()
	default:
		m.ingested.WithLabelValues(c.String(), "error").Inc()
		m.storeErrors.WithLabelValues("events").Inc()
	}
}

// instrument counts every request the hub answers by the route pattern
// that served it ("POST /v1/tasks", "GET /v1/tasks/{id}"; the
// innermost mux's, where the parts' own muxes route further), which
// keeps the label's values to the hub's own routes, whatever paths
// callers ask for. routeOf, when it names a route, overrides the
// pattern.
func (m *metrics) instrument(next http.Handler, routeOf func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := routeOf(r)
		if route == "" {
			route = r.Pattern
		}
		if route == "" {
			route = "unmatched"
		}
		m.requests.WithLabelValues(route, strconv.Itoa(sw.status())).Inc()
	})
}

// subscriptions counts the event subscriptions open.
func (m *metrics) subscriptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.subscribers.Inc()
		defer m.subscribers.Dec()
		next.ServeHTTP(w, r)
	})
}

// viewThrough times the view-through to its answer, and counts its
// answers by code.
func (m *metrics) viewThrough(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		answered := func() { m.viewLatency.Observe(time.Since(start).Seconds()) }
		sw := &statusWriter{ResponseWriter: w, onHeader: answered}
		next.ServeHTTP(sw, r)
		if sw.code == 0 {
			answered()
		}
		m.views.WithLabelValues(strconv.Itoa(sw.status())).Inc()
	})
}

// statusWriter records the status a handler answered with. It passes
// Flush and Hijack through: the event streams flush, and the node and
// WebSocket connections hijack.
type statusWriter struct {
	http.ResponseWriter
	code     int
	hijacked bool
	onHeader func()
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 && code >= 200 {
		w.code = code
		if w.onHeader != nil {
			w.onHeader()
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) status() int {
	switch {
	case w.code != 0:
		return w.code
	case w.hijacked:
		return http.StatusSwitchingProtocols
	}
	return http.StatusOK
}

// stateCollector reads the hub's state when it is scraped: the
// registry's nodes, the backlog's depth, the event store's size and
// whether the database answers.
type stateCollector struct {
	h *Hub
	m *metrics
}

var (
	descReady = prometheus.NewDesc("silo_hub_ready",
		"1 while the hub is ready (/readyz): the database answers and the hub is not shutting down.", nil, nil)
	descDatabaseUp = prometheus.NewDesc("silo_hub_database_up",
		"1 if the database answered a ping at this scrape.", nil, nil)
	descNodes = prometheus.NewDesc("silo_hub_nodes",
		"Nodes in the registry, by status: online, stale, offline.", []string{"status"}, nil)
	descTeamNodes = prometheus.NewDesc("silo_hub_team_nodes",
		"Nodes serving at least one of a team's repositories, by status.", []string{"team", "status"}, nil)
	descTeamFree = prometheus.NewDesc("silo_hub_team_free_slots",
		"Unclaimed slots on the online nodes that serve a team's repositories.", []string{"team"}, nil)
	descBacklog = prometheus.NewDesc("silo_hub_backlog_tasks",
		"Unfinished tasks in the backlog, by team and state: proposed, queued, offered, running, lost.", []string{"team", "state"}, nil)
	descOldestQueued = prometheus.NewDesc("silo_hub_backlog_oldest_queued_seconds",
		"How long a team's longest-waiting queued task has been queued.", []string{"team"}, nil)
	descEventRows = prometheus.NewDesc("silo_hub_event_store_events",
		"Events the event store holds.", nil, nil)
	descEventBytes = prometheus.NewDesc("silo_hub_event_store_bytes",
		"Bytes of event bodies the event store holds.", nil, nil)
	descDBOpen = prometheus.NewDesc("silo_hub_db_open_connections",
		"Open database connections, by store.", []string{"store"}, nil)
	descDBWait = prometheus.NewDesc("silo_hub_db_wait_count_total",
		"Waits for a database connection, by store.", []string{"store"}, nil)
)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descReady, descDatabaseUp, descNodes, descTeamNodes, descTeamFree,
		descBacklog, descOldestQueued, descEventRows, descEventBytes, descDBOpen, descDBWait} {
		ch <- d
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

var statuses = []registry.Status{registry.Online, registry.Stale, registry.Offline}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	h := c.h
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	failed := func(err error) bool {
		if err != nil {
			c.m.storeErrors.WithLabelValues("database").Inc()
			h.cfg.logf("hub: metrics: %v", err)
			return true
		}
		return false
	}

	dbUp := !failed(h.Directory.Ping(ctx))
	gauge(ch, descDatabaseUp, boolValue(dbUp))
	gauge(ch, descReady, boolValue(dbUp && !h.draining.Load()))

	nodes := h.Registry.List()
	byStatus := map[registry.Status]int{}
	for _, e := range nodes {
		byStatus[e.Status]++
	}
	for _, st := range statuses {
		gauge(ch, descNodes, float64(byStatus[st]), string(st))
	}

	for _, s := range []struct {
		name  string
		stats sql.DBStats
	}{
		{"directory", h.Directory.Stats()},
		{"backlog", h.Backlog.Stats()},
		{"events", h.Events.Stats()},
	} {
		gauge(ch, descDBOpen, float64(s.stats.OpenConnections), s.name)
		ch <- prometheus.MustNewConstMetric(descDBWait, prometheus.CounterValue, float64(s.stats.WaitCount), s.name)
	}

	if !dbUp {
		return
	}
	if teams, err := h.Directory.Teams(ctx); !failed(err) {
		for _, t := range teams {
			serves := map[string]bool{}
			for _, r := range t.Repos {
				serves[r] = true
			}
			counts := map[registry.Status]int{}
			free := 0
			for _, e := range nodes {
				for _, r := range e.Repos {
					if serves[r] {
						counts[e.Status]++
						if e.Status == registry.Online {
							free += e.Available()
						}
						break
					}
				}
			}
			for _, st := range statuses {
				gauge(ch, descTeamNodes, float64(counts[st]), t.ID, string(st))
			}
			gauge(ch, descTeamFree, float64(free), t.ID)
		}
	}
	if counts, err := h.Backlog.Counts(ctx); !failed(err) {
		for team, byState := range counts {
			for _, st := range []tasks.State{tasks.Proposed, tasks.Queued, tasks.Offered, tasks.Running, tasks.Lost} {
				gauge(ch, descBacklog, float64(byState[st]), team, string(st))
			}
		}
	}
	if oldest, err := h.Backlog.OldestQueued(ctx); !failed(err) {
		for team, since := range oldest {
			gauge(ch, descOldestQueued, max(0, time.Since(since).Seconds()), team)
		}
	}
	if rows, bytes, err := h.Events.Size(ctx); !failed(err) {
		gauge(ch, descEventRows, float64(rows))
		gauge(ch, descEventBytes, float64(bytes))
	}
}
