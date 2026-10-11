package session_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	facade "github.com/bwsalmon/silo/pkg/silo/facade/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/gateway"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/session"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

var ctx = context.Background()

type obj = map[string]any

const host = "host-project"

// fakeVMs makes every tool VM at the next of 10.9.0.x.
type fakeVMs struct {
	mu sync.Mutex
	n  int
}

func (f *fakeVMs) Create(_ context.Context, _ fenceapi.Caller, _ *tree.Silo, _, _ string, _ fenceapi.ToolVMSpec, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return fmt.Sprintf("10.9.0.%d", f.n), nil
}

func (f *fakeVMs) Delete(context.Context, fenceapi.Caller, *tree.Silo, string, string, string) error {
	return nil
}

// fakeTools is a tool server on every agent's tool channel: exec echoes
// its command, and says which agent's channel it came down.
type fakeTools struct{}

func (fakeTools) MCP(_ context.Context, agent string, msg []byte) []byte {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.Unmarshal(msg, &req)
	var result any
	switch req.Method {
	case "tools/list":
		result = obj{"tools": []obj{{"name": "exec", "description": "run", "inputSchema": obj{"type": "object"}}}}
	case "tools/call":
		result = obj{"content": []obj{{"type": "text", "text": "ran " + fmt.Sprint(req.Params.Arguments["command"]) + " on " + agent}}, "isError": false}
	}
	b, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": req.ID, "result": result})
	return b
}

type env struct {
	t       *testing.T
	db      store.Database
	api     *fenceapi.Server
	srv     *httptest.Server // the fence API, as the Silo-Caller header says
	runner  *agents.Fake
	sockets *session.Sockets
	root    string
	// team is control full, ro read and sealed none; each has an agent,
	// worker, and team a second, helper.
	team, ro, sealed string
	worker           map[string]string // silo id to its agent's id
	helper           string
	nextID           int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	// A Unix socket's path is short; t.TempDir's can be too long.
	dir, err := os.MkdirTemp("", "ss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	caller := func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
	}
	e := &env{t: t, db: db, runner: &agents.Fake{}, worker: map[string]string{}}
	e.sockets = &session.Sockets{Dir: dir, Tools: fakeTools{}, Model: fakeModel{}}
	fc := facade.New(db, facade.Options{GCE: fake.New(fake.Options{Project: host}), HostProject: host, Caller: caller})
	e.api = fenceapi.New(db, fenceapi.Options{Caller: caller, ToolVMs: &fakeVMs{},
		Runner:      &session.Runner{Inner: e.runner, Sockets: e.sockets},
		Credentials: agents.StaticCredentials{"anthropic-api": "anthropic"}})
	e.runner.Connected = e.api.ToolServerConnected
	e.sockets.API, e.sockets.Control, e.sockets.Facade = e.api, e.api, fc
	mux := http.NewServeMux()
	mux.Handle("/compute/v1/", fc)
	mux.Handle("/", e.api)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(func() { e.sockets.CloseAll(); e.srv.Close(); fc.Close(); e.api.Close() })

	root := e.ok(e.call("", "POST", "/silod/bootstrap", obj{
		"regions": []string{"us-central1"}, "capacity": obj{"us-central1": obj{"vcpu": 100}, "global": obj{"subnetworks": 20}},
		"model_budget_usd": "1000", "agent_policy": obj{"credentials": []string{"anthropic-api"}},
		"admins": []string{"user:admin@example.com"},
	}))
	e.root = root["id"].(string)
	if err := db.Tx(ctx, func(tx store.Tx) error {
		return ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{{Key: fenceapi.AgentSessions, Amount: ledger.Units(10)}})
	}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, control string) string {
		return e.ok(e.call(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": name, "address": obj{"prefix_length": 16},
			"control": control, "model_budget_usd": "100", "quota": obj{"global": obj{"agent_sessions": 3}}}))["id"].(string)
	}
	e.team, e.ro, e.sealed = mk("team", "full"), mk("ro", "read"), mk("sealed", "none")
	for _, s := range []string{e.team, e.ro, e.sealed} {
		e.worker[s] = e.agent(s, "worker")
	}
	e.helper = e.agent(e.team, "helper")
	return e
}

// agent creates an agent in silo as the root, its tool server
// connected, and returns its id.
func (e *env) agent(silo, name string) string {
	e.t.Helper()
	e.ok(e.call(e.root, "POST", "/silo/v1/silos/-"+silo+"/agents", obj{"name": name,
		"harness": obj{"adapter": "claude-code", "credential": "anthropic-api"}, "model": "claude-opus-5-5", "budget": obj{"usd": "10"},
		"tool_vm": obj{"zone": "us-central1-a", "subnet": "work"}, "lease": obj{"ttl": "4h"}, "prompt": "Mind the service."}))
	id := e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+silo+"/agents/"+name, nil))["id"].(string)
	if err := e.runner.Connect(ctx, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

type resp struct {
	code int
	body obj
	raw  string
}

func (e *env) ok(r resp) obj {
	e.t.Helper()
	if r.code != 200 {
		e.t.Fatalf("%d %s", r.code, r.raw)
	}
	return r.body
}

// call makes a call to the fence API as caller (a silo id), not on any
// socket.
func (e *env) call(caller, method, path string, body any) resp {
	e.t.Helper()
	req := e.req(method, e.srv.URL+path, body)
	if caller != "" {
		req.Header.Set("Silo-Caller", caller)
	}
	return e.send(http.DefaultClient, req)
}

func (e *env) req(method, u string, body any) *http.Request {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		e.t.Fatal(err)
	}
	return req
}

func (e *env) send(c *http.Client, req *http.Request) resp {
	e.t.Helper()
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := obj{}
	json.Unmarshal(b, &out)
	return resp{res.StatusCode, out, string(b)}
}

// client talks to agent's session socket.
func (e *env) client(agent string) *http.Client {
	path := e.sockets.Path(agent)
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

// on makes an HTTP call on agent's socket.
func (e *env) on(agent, method, path string, body any, hdr ...string) resp {
	e.t.Helper()
	req := e.req(method, "http://session"+path, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return e.send(e.client(agent), req)
}

// tool calls an MCP tool on agent's socket and returns its text and
// whether it is an error.
func (e *env) tool(agent, name string, args obj) (string, bool) {
	e.t.Helper()
	e.nextID++
	r := e.on(agent, "POST", "/mcp", obj{"jsonrpc": "2.0", "id": e.nextID, "method": "tools/call", "params": obj{"name": name, "arguments": args}})
	if r.code != 200 {
		e.t.Fatalf("tools/call %s: %d %s", name, r.code, r.raw)
	}
	res, _ := r.body["result"].(obj)
	if res == nil {
		e.t.Fatalf("tools/call %s: %s", name, r.raw)
	}
	text := ""
	for _, c := range res["content"].([]any) {
		text += c.(obj)["text"].(string)
	}
	return text, res["isError"] == true
}

func (e *env) audit(silo, kind string) []obj {
	e.t.Helper()
	var out []obj
	for _, r := range e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+silo+"/audit?kind="+kind, nil))["records"].([]any) {
		out = append(out, r.(obj))
	}
	return out
}

func (e *env) state(silo, agent string) string {
	e.t.Helper()
	return e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+silo+"/agents/-"+agent, nil))["state"].(string)
}

// The socket is the identity: each agent's socket is its silo, its
// harness, whatever the request says; it is made where the runner is
// told, and goes with the session.
func TestIdentityBySocket(t *testing.T) {
	e := newEnv(t)
	w := e.worker[e.team]
	if _, err := os.Stat(filepath.Join(e.sockets.Dir, w, session.SocketName)); err != nil {
		t.Fatalf("socket: %v", err)
	}
	for agent, silo := range map[string]string{w: e.team, e.worker[e.ro]: e.ro} {
		got := e.ok(e.on(agent, "GET", "/silo/v1/self", nil, "Silo-Caller", e.root, "Silo-Agent", "someone"))
		if got["id"] != silo {
			t.Errorf("agent %s's socket is %v, want silo %s", agent, got["id"], silo)
		}
	}
	// A sibling silo is out of reach, as it is from inside the cloud.
	if r := e.on(w, "GET", "/silo/v1/silos/-"+e.ro, nil, "Silo-Caller", e.root); r.code != 404 {
		t.Errorf("the team's agent reading /ro: %d %s", r.code, r.raw)
	}
	if text, isErr := e.tool(w, "silo_describe", obj{"silo": "-" + e.ro}); !isErr || !strings.Contains(text, "NOT_FOUND") {
		t.Errorf("silo_describe of a sibling: %v %s", isErr, text)
	}
	// The tool VM's tools come down the agent's own channel.
	if text, isErr := e.tool(w, "exec", obj{"command": "uptime"}); isErr || text != "ran uptime on "+w {
		t.Errorf("exec: %v %s", isErr, text)
	}
	// The model proxy is handed the socket's agent, the provider and
	// the rest of the path.
	if r := e.on(w, "POST", "/model/anthropic/v1/messages", obj{}); r.code != 200 || !strings.Contains(string(r.raw), w+" anthropic /v1/messages") {
		t.Errorf("/model/: %d %s", r.code, r.raw)
	}
	if r := e.on(w, "GET", "/elsewhere", nil); r.code != 404 {
		t.Errorf("/elsewhere: %d", r.code)
	}
	// Stopping the session closes its socket.
	e.ok(e.call(e.root, "POST", "/silo/v1/silos/-"+e.team+"/agents/worker:stop", obj{}))
	if _, err := os.Stat(e.sockets.Path(w)); !os.IsNotExist(err) {
		t.Errorf("after stop, the socket: %v", err)
	}
}

// MCP: initialize, and the list: the tool VM's tools and the socket's
// own.
func TestMCPList(t *testing.T) {
	e := newEnv(t)
	w := e.worker[e.team]
	init := e.ok(e.on(w, "POST", "/mcp", obj{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": obj{"protocolVersion": "2025-03-26"}}))
	if get(init, "result", "protocolVersion") != "2025-03-26" || get(init, "result", "serverInfo", "name") != "silo" {
		t.Errorf("initialize: %v", init)
	}
	if r := e.on(w, "POST", "/mcp", obj{"jsonrpc": "2.0", "method": "notifications/initialized"}); r.code != 202 {
		t.Errorf("a notification: %d", r.code)
	}
	list := e.ok(e.on(w, "POST", "/mcp", obj{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}))
	var names []string
	for _, tl := range get(list, "result", "tools").([]any) {
		names = append(names, tl.(obj)["name"].(string))
	}
	want := "exec silo_describe silo_create_child silo_delete silo_patch silo_export silo_import agent_create agent_delete agent_message compute ask request_approval"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("tools:\n got %s\nwant %s", got, want)
	}
}

// Every call is made as the silo, audited via harness with the agent;
// call 38 holds on the socket; and a call from an agent's tool VM, at
// the gateway, is noted from_tool_vm.
func TestHarnessAudit(t *testing.T) {
	e := newEnv(t)
	w := e.worker[e.team]
	if text, isErr := e.tool(w, "agent_message", obj{"agent": "helper", "text": "Check the logs."}); isErr {
		t.Fatalf("agent_message: %s", text)
	}
	recs := e.audit(e.team, fenceapi.KindAgentMessage)
	if len(recs) != 1 || get(recs[0], "actor", "silo") != e.team || recs[0]["via"] != "harness" || get(recs[0], "fields", "agent") != w ||
		get(recs[0], "fields", "target_agent") != e.helper {
		t.Errorf("agent.message: %v", recs)
	}
	// The facade, over the compute tool and over HTTP.
	if text, isErr := e.tool(w, "compute", obj{"path": "projects/silo-" + e.team}); isErr || !strings.Contains(text, "silo-"+e.team) {
		t.Errorf("compute: %v %s", isErr, text)
	}
	if r := e.on(w, "GET", "/compute/v1/projects/silo-"+e.team, nil); r.code != 200 {
		t.Errorf("/compute/v1/: %d %s", r.code, r.raw)
	}
	// Call 38.
	if r := e.on(w, "POST", "/silo/v1/self/agents/worker:renew", obj{"ttl": "8h"}); r.code != 403 || get(r.body, "error", "code") != "PERMISSION_DENIED" {
		t.Errorf("renewing its own lease: %d %s", r.code, r.raw)
	}
	if r := e.on(w, "PATCH", "/silo/v1/self/agents/worker", obj{"budget": obj{"usd": "20"}}); r.code != 403 {
		t.Errorf("raising its own budget: %d %s", r.code, r.raw)
	}
	e.ok(e.on(w, "POST", "/silo/v1/self/agents/helper:renew", obj{"ttl": "8h"}))
	// What its session creates is its own (call 43).
	if text, isErr := e.tool(w, "silo_create_child", obj{"body": obj{"name": "scratch", "address": obj{"prefix_length": 24}}}); isErr {
		t.Fatalf("silo_create_child: %s", text)
	}
	if d := e.ok(e.call(e.root, "GET", "/silo/v1/silos/team/scratch", nil)); d["owner_agent"] != w {
		t.Errorf("scratch's owner_agent: %v", d["owner_agent"])
	}

	// From the tool VM, at the gateway's API port.
	toolVM := e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+e.team+"/agents/-"+w, nil))
	ip := get(toolVM, "tool_vm", "ip").(string)
	gw := httptest.NewServer(gateway.Silod(e.db, gateway.SilodOptions{API: e.api, ToolVMAgent: e.api.ToolVMAgentAt}))
	defer gw.Close()
	for addr, want := range map[string]any{ip: w, "10.9.9.9": nil} {
		req := e.req("POST", gw.URL+"/silo/v1/self/agents/helper/messages", obj{"text": "from " + addr})
		req.Header.Set(gateway.OriginHeader, gateway.FormatOrigin(e.team, 1<<62))
		req.Header.Set(gateway.OriginAddressHeader, addr)
		e.ok(e.send(http.DefaultClient, req))
		recs := e.audit(e.team, fenceapi.KindAgentMessage)
		last := recs[len(recs)-1]
		if get(last, "fields", "from_tool_vm") != want || get(last, "fields", "agent") != e.helper || last["via"] != nil {
			t.Errorf("from %s: %v, want from_tool_vm %v", addr, last, want)
		}
	}
}

// Under none and read, on the socket: what is refused, and what is
// kept.
func TestSealedAndRead(t *testing.T) {
	e := newEnv(t)
	sealed, ro := e.worker[e.sealed], e.worker[e.ro]
	child := obj{"body": obj{"name": "c", "address": obj{"prefix_length": 24}}}
	fenceTools := []struct {
		name  string
		args  obj
		reads bool
	}{
		{"silo_describe", obj{}, true},
		{"silo_create_child", child, false},
		{"silo_delete", obj{"silo": "/elsewhere"}, false},
		{"silo_patch", obj{"body": obj{"labels": obj{"a": "b"}}}, false},
		{"silo_export", obj{"body": obj{"name": "x"}}, false},
		{"silo_import", obj{"body": obj{"export": "x"}}, false},
		{"agent_create", obj{"body": obj{"name": "more"}}, false},
		{"agent_delete", obj{"agent": "worker"}, false},
	}
	for _, ft := range fenceTools {
		text, isErr := e.tool(sealed, ft.name, ft.args)
		if !isErr || !strings.Contains(text, "CONTROL_DENIED") {
			t.Errorf("none: %s: %v %s", ft.name, isErr, text)
		}
		text, isErr = e.tool(ro, ft.name, ft.args)
		if ft.reads && isErr {
			t.Errorf("read: %s refused: %s", ft.name, text)
		}
		if !ft.reads && (!isErr || !strings.Contains(text, "CONTROL_DENIED")) {
			t.Errorf("read: %s: %v %s", ft.name, isErr, text)
		}
	}
	if len(e.audit(e.sealed, fenceapi.KindControlDenied)) == 0 {
		t.Errorf("no control.denied in /sealed")
	}
	// compute, and the facade over HTTP: a GCE-shaped 403.
	if text, isErr := e.tool(sealed, "compute", obj{"path": "projects/silo-" + e.sealed}); !isErr || !strings.Contains(text, "HTTP 403") {
		t.Errorf("none: compute: %v %s", isErr, text)
	}
	if r := e.on(sealed, "GET", "/compute/v1/projects/silo-"+e.sealed, nil); r.code != 403 || get(r.body, "error", "code") != float64(403) {
		t.Errorf("none: /compute/v1/: %d %s", r.code, r.raw)
	}
	if text, isErr := e.tool(ro, "compute", obj{"path": "projects/silo-" + e.ro}); isErr {
		t.Errorf("read: compute get: %s", text)
	}
	if text, isErr := e.tool(ro, "compute", obj{"method": "POST", "path": "projects/silo-" + e.ro + "/zones/us-central1-a/instances",
		"body": obj{"name": "x"}}); !isErr || !strings.Contains(text, "HTTP 403") {
		t.Errorf("read: compute insert: %v %s", isErr, text)
	}
	// The fence API over HTTP.
	if r := e.on(sealed, "GET", "/silo/v1/self", nil); r.code != 403 || get(r.body, "error", "code") != "CONTROL_DENIED" {
		t.Errorf("none: GET self: %d %s", r.code, r.raw)
	}
	e.ok(e.on(ro, "GET", "/silo/v1/self", nil))
	// Its lifecycle is its ancestors'.
	for _, a := range []string{sealed, ro} {
		if r := e.on(a, "POST", "/silo/v1/self/agents/worker:stop", obj{}); r.code != 403 || get(r.body, "error", "code") != "CONTROL_DENIED" {
			t.Errorf("stopping itself: %d %s", r.code, r.raw)
		}
	}

	// Kept under both: the tools, the model proxy's path, and the
	// control surface of its own silo's agents.
	for silo, a := range map[string]string{e.sealed: sealed, e.ro: ro} {
		if text, isErr := e.tool(a, "exec", obj{"command": "systemctl restart app"}); isErr {
			t.Errorf("exec: %s", text)
		}
		if r := e.on(a, "GET", "/model/anthropic/", nil); r.code != 200 {
			t.Errorf("/model/: %d", r.code)
		}
		e.ok(e.on(a, "GET", "/silo/v1/self/agents", nil))
		e.ok(e.on(a, "GET", "/silo/v1/self/agents/worker", nil))
		e.ok(e.on(a, "GET", "/silo/v1/agents/-"+a, nil))
		e.ok(e.on(a, "GET", "/silo/v1/self/agents/worker/session", nil))
		e.ok(e.on(a, "GET", "/silo/v1/silos/-"+silo+"/agents/worker/messages", nil))
		e.ok(e.on(a, "GET", "/silo/v1/self/agents/worker/requests?state=open", nil))
		if text, isErr := e.tool(a, "agent_message", obj{"agent": "worker", "text": "note to self"}); isErr {
			t.Errorf("agent_message: %s", text)
		}
		// ask, answered by an ancestor.
		done := make(chan string)
		go func() {
			text, _ := e.tool(a, "ask", obj{"question": "Restart the app?"})
			done <- text
		}()
		id := e.openRequest(silo, a)
		e.ok(e.call(e.root, "POST", "/silo/v1/silos/-"+silo+"/agents/worker/requests/"+id+":answer", obj{"text": "Yes."}))
		if got := <-done; got != "Yes." {
			t.Errorf("ask: %q", got)
		}
	}
	// Not another silo's agents, even by id.
	if r := e.on(sealed, "GET", "/silo/v1/agents/-"+ro, nil); r.code != 403 || get(r.body, "error", "code") != "CONTROL_DENIED" {
		t.Errorf("none: another silo's agent: %d %s", r.code, r.raw)
	}
}

// openRequest waits for agent's one open request and returns its id.
func (e *env) openRequest(silo, agent string) string {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list := e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+silo+"/agents/-"+agent+"/requests?state=open", nil))["requests"].([]any)
		if len(list) == 1 {
			return list[0].(obj)["id"].(string)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("agent %s has no open request", agent)
	return ""
}

// request_approval: the agent is WAITING until it is answered, and
// may not answer itself.
func TestApproval(t *testing.T) {
	e := newEnv(t)
	w := e.worker[e.team]
	type res struct {
		text  string
		isErr bool
	}
	done := make(chan res)
	go func() {
		text, isErr := e.tool(w, "request_approval", obj{"action": "Drop the cache table.", "reason": "It is corrupt."})
		done <- res{text, isErr}
	}()
	id := e.openRequest(e.team, w)
	if s := e.state(e.team, w); s != agents.StateWaiting {
		t.Errorf("asking: %s", s)
	}
	path := "/silo/v1/self/agents/worker/requests/" + id + ":answer"
	if r := e.on(w, "POST", path, obj{"approve": true}); r.code != 403 || get(r.body, "error", "code") != "PERMISSION_DENIED" {
		t.Errorf("answering its own: %d %s", r.code, r.raw)
	}
	// Its silo, by another agent's session, may.
	if r := e.on(e.helper, "POST", path, obj{"text": "no approve"}); r.code != 400 {
		t.Errorf("an approval answered with text alone: %d %s", r.code, r.raw)
	}
	e.ok(e.on(e.helper, "POST", path, obj{"approve": false, "text": "Restore it from the backup instead."}))
	if got := <-done; got.isErr || got.text != "Not approved. Restore it from the backup instead." {
		t.Errorf("request_approval: %+v", got)
	}
	if s := e.state(e.team, w); s != agents.StateRunning {
		t.Errorf("answered: %s", s)
	}
	recs := e.audit(e.team, fenceapi.KindAgentRequest)
	if len(recs) != 1 || recs[0]["via"] != "harness" || get(recs[0], "fields", "agent") != w || get(recs[0], "fields", "request_kind") != "approval" {
		t.Errorf("agent.request: %v", recs)
	}
	var ok []obj
	for _, r := range e.audit(e.team, fenceapi.KindAgentAnswer) {
		if r["result"] == "OK" {
			ok = append(ok, r)
		}
	}
	if len(ok) != 1 || get(ok[0], "fields", "agent") != e.helper || get(ok[0], "fields", "target_agent") != w || get(ok[0], "fields", "approve") != false {
		t.Errorf("agent.answer: %v", ok)
	}
	// The session has it all, tool calls and results included.
	var kinds []string
	for _, ev := range e.ok(e.call(e.root, "GET", "/silo/v1/silos/-"+e.team+"/agents/worker/session", nil))["events"].([]any) {
		kinds = append(kinds, ev.(obj)["kind"].(string))
	}
	if got := strings.Join(kinds, " "); got != "turn tool_call request answer tool_result" {
		t.Errorf("events: %s", got)
	}
}

// The session follows as server-sent events on the socket too.
func TestFollowOnSocket(t *testing.T) {
	e := newEnv(t)
	w := e.worker[e.sealed]
	req := e.req("GET", "http://session/silo/v1/self/agents/worker/session?follow=true", nil)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res, err := e.client(w).Do(req.WithContext(cctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("follow: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	e.tool(w, "exec", obj{"command": "df -h"})
	sc := bufio.NewScanner(res.Body)
	var got []string
	for len(got) < 3 && sc.Scan() {
		if k, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			got = append(got, k)
		}
	}
	// The turn on its prompt, then the call.
	if strings.Join(got, " ") != "turn tool_call tool_result" {
		t.Errorf("followed: %v", got)
	}
}

func get(o obj, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(obj)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// fakeModel is a model proxy that says what it was asked.
type fakeModel struct{}

func (fakeModel) ServeModel(w http.ResponseWriter, r *http.Request, agent, provider string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"got": agent + " " + provider + " " + r.URL.Path})
}
