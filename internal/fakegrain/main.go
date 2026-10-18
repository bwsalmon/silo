// Command fakegrain is a scripted Grain node for the hub UI's browser
// suite (ui/e2e): a rendezvous client that accepts every task the hub
// offers it and then does nothing on its own. What the task does is
// driven over a small HTTP control API on -control, so that a
// Playwright spec can say exactly which events the hub sees, and when:
//
//	GET  /state                 {"online","refused","connects","failures","last_error","tasks":[{"id","node_task","received":[...]}]}
//	POST /tasks/{id}/emit       a JSON array of events, without seq or task, which the node numbers and sends up
//	POST /tasks/{id}/finish     {"state":"done"|"failed","reason":"..."}: the task's result, on its task stream
//	PUT  /artifacts/{ref}       the body is what the view-through serves for ref, until the node goes offline
//	POST /offline, /online      drop the connection and stay away, or come back
//
// "received" is every message the hub wrote down the task's stream
// after the offer (approve, message, cancel), in order, which is how a
// spec sees an answer reach the node. A node the hub refuses in its
// hello stops trying and reports refused; one whose connection is
// turned away before that (a revoked token is answered 401) keeps
// trying, and counts its failures.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

type task struct {
	ID        string          `json:"id"`
	NodeTask  string          `json:"node_task"`
	HubTaskID string          `json:"hub_task_id"`
	HubURL    string          `json:"hub_url"`
	Repo      string          `json:"repo,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Title     string          `json:"title,omitempty"`
	State     tasks.State     `json:"state,omitempty"`
	Received  []tasks.Message `json:"received"`
	seq       int64
	stream    *tasks.Stream
	approveCh chan tasks.Message
}

type grain struct {
	cl       *rendezvous.Client
	uiTeam   string
	uiAccess string

	mu              sync.Mutex
	conn            *rendezvous.Conn
	offline         bool
	refused         bool
	connects        int
	failures        int // connection attempts failed since the last success
	lastError       string
	tasks           map[string]*task
	order           []string
	artifacts       map[string][]byte
	grainfileEvents []grainfiles.Event
	wake            chan struct{}
}

func normalizeUIAccess(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "read-write") {
		return "read-write"
	}
	return "read-only"
}

func hubTaskURL(hub, id string) string {
	base := strings.TrimSpace(hub)
	switch {
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	}
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, "/v1/nodes/rendezvous")
	base = strings.TrimSuffix(base, "/")
	if base == "" || id == "" {
		return ""
	}
	return base + "/tasks/" + id
}

func main() {
	hub := flag.String("hub", "", "the hub's URL")
	tokenFile := flag.String("token-file", "", "the file holding the node's token")
	id := flag.String("id", "", "the node's ID")
	repos := flag.String("repos", "", "comma-separated repositories the node serves")
	recipients := flag.String("recipients", "", "comma-separated recipient URLs the node subscribes to")
	folders := flag.String("folders", "", "comma-separated folder URLs the node subscribes to")
	grainfilesFlag := flag.String("grainfiles", "", "comma-separated grainfile URLs the node subscribes to")
	slots := flag.Int("slots", 2, "the node's slots")
	interactive := flag.Bool("interactive", false, "list the interactive feature")
	nodeuiFlag := flag.Bool("nodeui", false, "list the nodeui feature")
	uiTeam := flag.String("ui-team", "", "the team whose admins may open the node UI")
	uiAccess := flag.String("ui-access", "read-only", "the access level for the node UI (read-only or read-write)")
	control := flag.String("control", "127.0.0.1:0", "the address to serve the control API on")
	addrFile := flag.String("addr-file", "", "write the control API's address here once it listens")
	flag.Parse()
	tok, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	node := registry.Node{ID: *id, Repos: strings.Split(*repos, ","), Slots: *slots, Version: "fakegrain"}
	if *recipients != "" {
		node.Recipients = strings.Split(*recipients, ",")
	}
	if *folders != "" {
		node.Folders = strings.Split(*folders, ",")
	}
	if *grainfilesFlag != "" {
		node.Grainfiles = strings.Split(*grainfilesFlag, ",")
	}
	if len(node.Recipients) > 0 || len(node.Folders) > 0 || len(node.Grainfiles) > 0 {
		node.Features = append(node.Features, registry.FeatureGrainfiles)
	}
	if *interactive {
		node.Features = append(node.Features, registry.FeatureInteractive)
	}
	access := normalizeUIAccess(*uiAccess)
	if *nodeuiFlag || *uiTeam != "" {
		node.Features = append(node.Features, registry.FeatureNodeUI)
		node.UI = &registry.NodeUI{
			Offered: true,
			Team:    *uiTeam,
			Access:  access,
		}
	}
	g := &grain{
		cl: &rendezvous.Client{
			Hub:   *hub,
			Token: func(context.Context) (string, error) { return strings.TrimSpace(string(tok)), nil },
			Node:  node,
		},
		uiTeam:    *uiTeam,
		uiAccess:  access,
		tasks:     map[string]*task{},
		artifacts: map[string][]byte{},
		wake:      make(chan struct{}, 1),
	}
	g.cl.OnGrainfileEvent = func(ev grainfiles.Event) {
		g.mu.Lock()
		g.grainfileEvents = append(g.grainfileEvents, ev)
		g.mu.Unlock()
	}
	l, err := net.Listen("tcp", *control)
	if err != nil {
		log.Fatal(err)
	}
	if *addrFile != "" {
		if err := os.WriteFile(*addrFile+".tmp", []byte(l.Addr().String()), 0o644); err != nil {
			log.Fatal(err)
		}
		if err := os.Rename(*addrFile+".tmp", *addrFile); err != nil {
			log.Fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go http.Serve(l, g.routes())
	g.run(ctx)
}

// run keeps the node connected, unless it is told to stay offline or
// its token is refused.
func (g *grain) run(ctx context.Context) {
	for ctx.Err() == nil {
		g.mu.Lock()
		away := g.offline || g.refused
		g.mu.Unlock()
		if away {
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c, err := g.cl.Connect(cctx)
		cancel()
		if err != nil {
			log.Printf("fakegrain: connect: %v", err)
			g.mu.Lock()
			g.failures++
			g.lastError = err.Error()
			g.mu.Unlock()
			if errors.Is(err, rendezvous.ErrRefused) {
				g.mu.Lock()
				g.refused = true
				g.mu.Unlock()
				continue
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		g.mu.Lock()
		g.conn = c
		g.connects++
		g.failures, g.lastError = 0, ""
		g.mu.Unlock()
		stop := context.AfterFunc(ctx, func() { c.Close() })
		for {
			st, err := c.Accept()
			if err != nil {
				break
			}
			go g.handle(st)
		}
		stop()
		g.mu.Lock()
		g.conn = nil
		g.mu.Unlock()
		log.Printf("fakegrain: connection ended (goodbye %q)", c.Goodbye())
	}
}

// bufConn is a stream whose first line has been read through r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// handle answers one stream the hub opened: a task stream or a view.
func (g *grain) handle(c net.Conn) {
	r := bufio.NewReaderSize(c, 4096)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	json.Unmarshal(line, &head)
	if head.Type == live.TypeView {
		var m live.ViewMessage
		json.Unmarshal(line, &m)
		live.ServeView(c, m, g.view)
		return
	}
	if head.Type == nodeui.TypeHTTP {
		var m nodeui.Message
		if json.Unmarshal(line, &m) != nil {
			c.Close()
			return
		}
		g.serveNodeUI(bufConn{c, r}, m)
		return
	}
	var m tasks.Message
	if err := json.Unmarshal(line, &m); err != nil || m.Task == nil {
		c.Close()
		return
	}
	st := tasks.NewStream(bufConn{c, r})
	g.mu.Lock()
	t := g.tasks[m.Task.ID]
	switch m.Type {
	case tasks.TypeOffer, tasks.TypeResume:
		if t == nil {
			var hubURL string
			if g.cl != nil {
				hubURL = hubTaskURL(g.cl.Hub, m.Task.ID)
			}
			t = &task{
				ID:        m.Task.ID,
				NodeTask:  fmt.Sprintf("local-%d", len(g.tasks)+1),
				HubTaskID: m.Task.ID,
				HubURL:    hubURL,
				Repo:      m.Task.Repo,
				Kind:      m.Task.Kind,
				Title:     m.Task.Title,
				State:     tasks.Running,
				Received:  []tasks.Message{},
				approveCh: make(chan tasks.Message, 8),
			}
			g.tasks[t.ID] = t
			g.order = append(g.order, t.ID)
		}
		t.stream = st
	default:
		if t != nil {
			t.Received = append(t.Received, m)
		}
		g.mu.Unlock()
		st.Close()
		return
	}
	g.mu.Unlock()
	if err := st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: t.NodeTask}); err != nil {
		st.Close()
		return
	}
	for {
		m, err := st.Read()
		if err != nil {
			return
		}
		m.Attestation = ""
		g.mu.Lock()
		t.Received = append(t.Received, m)
		if m.Type == tasks.TypeApprove && t.approveCh != nil {
			select {
			case t.approveCh <- m:
			default:
			}
		}
		if m.Type == tasks.TypeCancel {
			t.State = tasks.Cancelled
		}
		g.mu.Unlock()
	}
}

func (g *grain) view(m live.ViewMessage) (io.ReadCloser, error) {
	g.mu.Lock()
	b, ok := g.artifacts[m.Ref]
	g.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no artifact %q", m.Ref)
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

// emit numbers evs and sends them up on an event stream of their own,
// waiting for the hub to have read them all.
func (g *grain) emit(id string, evs []map[string]any) error {
	g.mu.Lock()
	t, c := g.tasks[id], g.conn
	if t == nil || c == nil {
		g.mu.Unlock()
		return fmt.Errorf("no task %s here, or no connection", id)
	}
	var b strings.Builder
	for _, e := range evs {
		t.seq++
		e["v"], e["seq"], e["task"] = 1, t.seq, id
		if _, ok := e["time"]; !ok {
			e["time"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		line, err := json.Marshal(e)
		if err != nil {
			g.mu.Unlock()
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := c.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := io.WriteString(st, b.String()); err != nil {
		return err
	}
	if cw, ok := st.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, st); done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *grain) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		ts := []*task{}
		for _, id := range g.order {
			ts = append(ts, g.tasks[id])
		}
		writeJSON(w, map[string]any{"online": g.conn != nil, "refused": g.refused, "connects": g.connects,
			"failures": g.failures, "last_error": g.lastError, "tasks": ts, "grainfile_events": g.grainfileEvents})
	})
	mux.HandleFunc("POST /tasks/{id}/emit", func(w http.ResponseWriter, r *http.Request) {
		var evs []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&evs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := g.emit(r.PathValue("id"), evs); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("POST /tasks/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State  tasks.State `json:"state"`
			Reason string      `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		t := g.tasks[r.PathValue("id")]
		g.mu.Unlock()
		if t == nil || t.stream == nil {
			http.Error(w, "no task stream", http.StatusConflict)
			return
		}
		if err := t.stream.Write(tasks.Message{Type: tasks.TypeResult, State: req.State, Reason: req.Reason}); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("PUT /artifacts/{ref}", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.artifacts[r.PathValue("ref")] = b
		g.mu.Unlock()
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("POST /offline", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.offline = true
		c := g.conn
		g.mu.Unlock()
		if c != nil {
			c.Close()
		}
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		t := g.tasks[r.PathValue("id")]
		g.mu.Unlock()
		if t == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, t)
	})
	mux.HandleFunc("POST /online", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.offline = false
		g.mu.Unlock()
		select {
		case g.wake <- struct{}{}:
		default:
		}
		writeJSON(w, map[string]any{})
	})
	return mux
}

const (
	headerHubUIUser   = "X-Grain-Hub-UI-User"
	headerHubUIRole   = "X-Grain-Hub-UI-Role"
	headerHubUITeam   = "X-Grain-Hub-UI-Team"
	headerHubUIAccess = "X-Grain-Hub-UI-Access"
)

func hasLeakedHubCredentials(r *http.Request) (string, bool) {
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-goog-authenticated-user-") || lk == "x-goog-iap-jwt-assertion" || lk == "authorization" {
			return "header " + k, true
		}
	}
	for _, c := range r.Cookies() {
		name := strings.ToLower(c.Name)
		if name == strings.ToLower(identity.SessionCookie) ||
			name == "__host-"+strings.ToLower(identity.SessionCookie) ||
			name == strings.ToLower(identity.FlowCookie) ||
			name == "__host-"+strings.ToLower(identity.FlowCookie) ||
			strings.HasPrefix(name, "gcp_iap_") ||
			strings.HasPrefix(name, "__host-gcp_iap_") {
			return "cookie " + c.Name, true
		}
	}
	return "", false
}

func (g *grain) serveNodeUI(c net.Conn, m nodeui.Message) {
	if g.cl != nil && g.cl.Node.UI == nil {
		_ = nodeui.Refuse(c, "node UI is not enabled")
		return
	}
	_ = nodeui.Serve(c, m, g.uiHandler(m))
}

func (g *grain) uiHandler(m nodeui.Message) http.Handler {
	access := normalizeUIAccess(g.uiAccess)
	mux := http.NewServeMux()
	tasksHandler := func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		ts := make([]*task, 0, len(g.order))
		for _, id := range g.order {
			ts = append(ts, g.tasks[id])
		}
		g.mu.Unlock()
		writeJSON(w, map[string]any{"tasks": ts})
	}
	taskByIDHandler := func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		t := g.tasks[r.PathValue("id")]
		g.mu.Unlock()
		if t == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, t)
	}
	mux.HandleFunc("GET /tasks", tasksHandler)
	mux.HandleFunc("GET /api/tasks", tasksHandler)
	mux.HandleFunc("GET /tasks/{id}", taskByIDHandler)
	mux.HandleFunc("GET /api/tasks/{id}", taskByIDHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		ts := make([]*task, 0, len(g.order))
		for _, id := range g.order {
			ts = append(ts, g.tasks[id])
		}
		g.mu.Unlock()
		writeJSON(w, map[string]any{
			"method": r.Method,
			"path":   r.URL.Path,
			"viewer": r.Header.Get(headerHubUIUser),
			"role":   r.Header.Get(headerHubUIRole),
			"team":   r.Header.Get(headerHubUITeam),
			"access": r.Header.Get(headerHubUIAccess),
			"tasks":  ts,
		})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if leaked, bad := hasLeakedHubCredentials(r); bad {
			http.Error(w, "forbidden: hub credential leaked to node handler: "+leaked, http.StatusBadRequest)
			return
		}
		if m.Viewer != "" && r.Header.Get(headerHubUIUser) == "" {
			r.Header.Set(headerHubUIUser, m.Viewer)
		}
		if m.Role != "" && r.Header.Get(headerHubUIRole) == "" {
			r.Header.Set(headerHubUIRole, m.Role)
		}
		if m.Team != "" && r.Header.Get(headerHubUITeam) == "" {
			r.Header.Set(headerHubUITeam, m.Team)
		}
		if r.Header.Get(headerHubUIAccess) == "" {
			r.Header.Set(headerHubUIAccess, access)
		}
		role := r.Header.Get(headerHubUIRole)
		if role == nodeui.RoleTeamAdmin && access == "read-only" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "forbidden: team-admin access is read-only", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
