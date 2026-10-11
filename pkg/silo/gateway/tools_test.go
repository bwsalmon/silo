package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/toolchan"
	"github.com/bwsalmon/silo/pkg/silo/tools"
)

func (e *env) disconnected(ctx context.Context, agent string) error {
	e.discMu.Lock()
	e.discs = append(e.discs, agent)
	e.discMu.Unlock()
	return e.fence.ToolServerDisconnected(ctx, agent)
}

func (e *env) disconnects() int {
	e.discMu.Lock()
	defer e.discMu.Unlock()
	return len(e.discs)
}

// mkAgent creates an agent in silo a, whose tool VM the facade makes
// in a's subnet, and returns its id and its tool VM's address.
func (e *env) mkAgent(name string) (string, netip.Addr) {
	e.t.Helper()
	e.tx(func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(100)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(5)}}); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.a, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(50)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(3)}})
	})
	e.ok(e.a, "POST", "/silo/v1/self/agents", obj{"name": name, "harness": obj{"adapter": "claude-code", "credential": "anthropic-api"},
		"model": "claude-sonnet-5-5", "budget": obj{"usd": "10"}, "lease": obj{"ttl": "2h"},
		"tool_vm": obj{"zone": "us-central1-a", "subnet": "web", "machine_type": "e2-standard-2"}})
	e.fakeGCE.RunOperations()
	a := e.agent(name)
	ip, err := netip.ParseAddr(a["tool_vm"].(obj)["ip"].(string))
	if err != nil {
		e.t.Fatalf("agent %s has no tool VM address: %v", name, a)
	}
	return a["id"].(string), ip
}

func (e *env) agent(name string) obj {
	e.t.Helper()
	return e.ok(e.a, "GET", "/silo/v1/self/agents/"+name, nil)
}

// toolsClient is silo-tools' channel, dialing the port 80 listener of
// the gateway at web80 (and wherever it is redirected) as from.
func (e *env) toolsClient(from netip.Addr, web80 string, srv http.Handler) *toolchan.Client {
	return &toolchan.Client{URL: "http://" + web80 + ToolsAttachPath, Handler: srv,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, Logf: e.t.Logf,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(c, "%s\n", from)
			return c, nil
		}}
}

func toolsServer(t *testing.T) *tools.Server {
	t.Helper()
	s, err := tools.New(tools.Options{Proxy: "http://silo.internal:3128", NoProxy: "silo.internal", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mcpCall sends one tools/call down agent's channel.
func mcpCall(t *testing.T, hub *toolchan.Hub, agent, tool string, args obj) obj {
	t.Helper()
	msg, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": obj{"name": tool, "arguments": args}})
	out := obj{}
	if err := json.Unmarshal(hub.MCP(ctx, agent, msg), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func resultText(out obj) string {
	r, _ := out["result"].(obj)
	c, _ := r["content"].([]any)
	if len(c) == 0 {
		return ""
	}
	return c[0].(obj)["text"].(string)
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

// TestToolChannel attaches silo-tools to its agent through the
// gateway's port 80 (agents.md, "The tool channel", SV-106; gateway.md,
// "Agent tool channels", SV-84): only from the tool VM's address, with
// the instance silod made behind it; then tool calls go down it, it
// comes back after a drop, and calls while it is down are retryable.
func TestToolChannel(t *testing.T) {
	e := newEnv(t, envOptions{})
	id, vmIP := e.mkAgent("fixer")
	a := e.agent("fixer")
	if a["tool_vm"].(obj)["tool_server"] != agents.ToolServerPending || a["state"] != agents.StateProvisioning {
		t.Fatalf("before any attach: %v", a)
	}
	srv := toolsServer(t)

	// An address in a's subnet with no agent's tool VM: 403.
	other := e.ip[e.a]
	if other == vmIP {
		other = vmIP.Next()
	}
	if up, err := e.toolsClient(other, e.web80, srv).Once(ctx); up || !errors.Is(err, toolchan.ErrRefused) {
		t.Errorf("attach from an address with no agent: up=%v %v, want 403", up, err)
	}
	// An address in no silo at all: 403 at the gateway.
	if up, err := e.toolsClient(netip.MustParseAddr("192.0.2.1"), e.web80, srv).Once(ctx); up || !errors.Is(err, toolchan.ErrRefused) {
		t.Errorf("attach from outside every silo: up=%v %v, want 403", up, err)
	}
	// The tool VM's address, but another instance behind it than the
	// one silod made: 403.
	setInstanceID := func(v string) {
		e.tx(func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agents SET tool_vm = json_set(tool_vm, '$.instance_id', ?) WHERE id = ?`, v, id)
			return err
		})
	}
	recorded := a["tool_vm"].(obj)["instance_id"].(string)
	setInstanceID("12345")
	if up, err := e.toolsClient(vmIP, e.web80, srv).Once(ctx); up || !errors.Is(err, toolchan.ErrRefused) {
		t.Errorf("attach from the wrong instance at the right address: up=%v %v, want 403", up, err)
	}
	setInstanceID(recorded)
	if e.hub.Attached(id) {
		t.Fatal("a refused attach left a channel")
	}

	// The right address and instance: the channel is the agent's, and
	// its create finishes.
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	attached := make(chan string, 10)
	c := e.toolsClient(vmIP, e.web80, srv)
	c.Attached = func(u string) { attached <- u }
	done := make(chan struct{})
	go func() { c.Run(cctx); close(done) }()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.hub.WaitAttached(wctx, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent shows its tool server connected", func() bool {
		a := e.agent("fixer")
		return a["tool_vm"].(obj)["tool_server"] == agents.ToolServerConnected && a["state"] == agents.StateRunning
	})
	var opDone int
	e.tx(func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM silo_operations WHERE kind = 'agent.create' AND done`).Scan(&opDone)
	})
	if opDone != 1 {
		t.Errorf("the create's operation isn't done after the first connect")
	}

	// Tool calls go silod -> gateway -> channel, several at once.
	errc := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func(i int) {
			out := mcpCall(t, e.hub, id, "exec", obj{"command": fmt.Sprintf("sleep 0.2; echo call-%d", i)})
			if !strings.Contains(resultText(out), fmt.Sprintf("call-%d", i)) {
				errc <- fmt.Errorf("call %d: %v", i, out)
				return
			}
			errc <- nil
		}(i)
	}
	for i := 0; i < 5; i++ {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
	msg, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if b := string(e.hub.MCP(ctx, id, msg)); !strings.Contains(b, `"list_dir"`) || !strings.Contains(b, `"edit_file"`) {
		t.Errorf("tools/list: %s", b)
	}

	// The channel drops (as when the session moves): it is
	// DISCONNECTED, and silo-tools attaches again.
	<-attached
	e.hub.Close(id)
	eventually(t, "the drop is recorded", func() bool { return e.disconnects() >= 1 })
	select {
	case <-attached:
	case <-time.After(10 * time.Second):
		t.Fatal("silo-tools never attached again")
	}
	eventually(t, "the agent shows its tool server connected again", func() bool {
		return e.agent("fixer")["tool_vm"].(obj)["tool_server"] == agents.ToolServerConnected && e.hub.Attached(id)
	})
	if out := mcpCall(t, e.hub, id, "exec", obj{"command": "echo again"}); !strings.Contains(resultText(out), "again") {
		t.Errorf("after reattaching: %v", out)
	}

	// silo-tools stops: calls fail with a retryable MCP error.
	stop()
	<-done
	eventually(t, "the channel is gone", func() bool { return !e.hub.Attached(id) })
	eventually(t, "the agent shows its tool server disconnected", func() bool {
		return e.agent("fixer")["tool_vm"].(obj)["tool_server"] == agents.ToolServerDisconnected
	})
	out := mcpCall(t, e.hub, id, "exec", obj{"command": "echo down"})
	rpcErr, _ := out["error"].(obj)
	if rpcErr == nil || rpcErr["code"] != float64(toolchan.Retryable) || rpcErr["data"].(obj)["retryable"] != true {
		t.Errorf("a call while the channel is down: %v, want a retryable error", out)
	}
}

// TestToolChannelRedirect attaches through a gateway that isn't on the
// control VM hosting the session: it redirects to the host's gateway,
// and silo-tools follows.
func TestToolChannelRedirect(t *testing.T) {
	e := newEnv(t, envOptions{})
	id, vmIP := e.mkAgent("fixer")

	// A second silod on the same store, and its gateway, for which the
	// session's host is e's gateway.
	other := &ToolChannels{Agent: e.fence.ToolChannelAgent, Hub: &toolchan.Hub{},
		Host: func(ctx context.Context, agent string) (string, error) { return e.web80, nil }}
	web80 := e.secondGateway(other)

	srv := toolsServer(t)
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	attached := make(chan string, 1)
	c := e.toolsClient(vmIP, web80, srv)
	c.Attached = func(u string) {
		select {
		case attached <- u:
		default:
		}
	}
	go c.Run(cctx)
	select {
	case u := <-attached:
		if !strings.Contains(u, e.web80) {
			t.Errorf("attached at %s, want the host's gateway %s", u, e.web80)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never attached")
	}
	// The hub answers the upgrade before it records the channel, so the
	// client can hear it attached a moment before the hub has it.
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.hub.WaitAttached(wctx, id); err != nil {
		t.Fatalf("the host's hub never had the channel: %v", err)
	}
	if !e.hub.Attached(id) || other.Hub.Attached(id) {
		t.Errorf("the channel isn't on the host: host %v, other %v", e.hub.Attached(id), other.Hub.Attached(id))
	}
	if out := mcpCall(t, e.hub, id, "exec", obj{"command": "echo hosted"}); !strings.Contains(resultText(out), "hosted") {
		t.Errorf("a call on the host: %v", out)
	}
	// The redirect itself is a 307 naming the host's attach URL.
	conn, err := net.Dial("tcp", web80)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\nGET %s HTTP/1.1\r\nHost: silo.internal\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", vmIP, ToolsAttachPath, toolchan.Protocol)
	b := make([]byte, 512)
	n, _ := io.ReadAtLeast(conn, b, 12)
	if resp := string(b[:n]); !strings.HasPrefix(resp, "HTTP/1.1 307") || !strings.Contains(resp, "http://"+e.web80+ToolsAttachPath) {
		t.Errorf("the non-host gateway's answer: %q", resp)
	}
}

// secondGateway starts another silod on e's store, with tools, and a
// gateway in front of it, and returns that gateway's port 80 address
// (called as from a source IP, as e.web80 is).
func (e *env) secondGateway(tools *ToolChannels) string {
	t := e.t
	dir, err := os.MkdirTemp("", "gw2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(e.db)
	pctx, stopPub := context.WithCancel(ctx)
	go pub.Run(pctx)
	ssrv := &http.Server{Handler: Silod(e.db, SilodOptions{API: e.silod, Publisher: pub, Tools: tools})}
	go ssrv.Serve(l)
	gw := New(Options{Retry: 50 * time.Millisecond, Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}})
	fctx, stopFollow := context.WithCancel(ctx)
	go gw.Follow(fctx)
	xl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	xsrv := &http.Server{Handler: gw.HTTP()}
	go xsrv.Serve(&fakeSource{Listener: xl})
	t.Cleanup(func() { xsrv.Close(); stopFollow(); ssrv.Close(); stopPub() })
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := gw.WaitVersion(wctx, e.gw.Tables().Version); err != nil {
		t.Fatal(err)
	}
	return xl.Addr().String()
}

// TestToolsBinary serves silo-tools from port 80 to any silo's VM, for
// the architecture it asks for, sealed silos included.
func TestToolsBinary(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, arch := range ToolsArches {
		if err := os.WriteFile(filepath.Join(e.toolsDir, ToolsBinaryName(arch)), []byte("silo-tools for "+arch), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	get := func(from netip.Addr, query string) (int, string) {
		t.Helper()
		conn, err := net.Dial("tcp", e.web80)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s\nGET %s%s HTTP/1.1\r\nHost: silo.internal\r\nConnection: close\r\n\r\n", from, ToolsBinaryPath, query)
		resp, err := http.ReadResponse(bufioReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, s := range []string{e.a, e.ro, e.sealed} {
		for _, arch := range ToolsArches {
			if code, body := get(e.ip[s], "?arch="+arch+"&version=v1.0.0"); code != 200 || body != "silo-tools for "+arch {
				t.Errorf("%s from %s: %d %q", arch, s, code, body)
			}
		}
	}
	if code, _ := get(e.ip[e.a], "?arch=sparc"); code != 400 {
		t.Errorf("an unknown arch: %d, want 400", code)
	}
	if code, _ := get(netip.MustParseAddr("192.0.2.1"), "?arch=amd64"); code != 403 {
		t.Errorf("from outside every silo: %d, want 403", code)
	}
}

// TestToolsEndpointsIsolated shows the port 80 tool endpoints reach no
// part of the facade or the fence API: the gateway forwards an attach
// only to silod's ToolsSilodPath, whatever it is sent with, and silod
// serves that path only to a forwarded attach.
func TestToolsEndpointsIsolated(t *testing.T) {
	e := newEnv(t, envOptions{})
	var n atomic.Int32
	e.tools.Agent = func(ctx context.Context, silo string, ip netip.Addr) (string, error) {
		if silo != e.a || ip != e.ip[e.a] {
			t.Errorf("silod was asked about %s at %s, not the caller", silo, ip)
		}
		n.Add(1)
		return "", &fenceapi.Error{Code: fenceapi.PermissionDenied, Message: "no agent"}
	}
	send := func(from netip.Addr, raw string) string {
		t.Helper()
		conn, err := net.Dial("tcp", e.web80)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s\n%s", from, raw)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(conn)
		return string(b)
	}
	from := e.ip[e.a]
	// Whatever the attach carries -- a query, a Silo-Origin or address
	// of its own choosing, a body -- it is the caller's own silo and
	// address that silod checks, at ToolsSilodPath.
	resp := send(from, "POST "+ToolsAttachPath+"?x=/silo/v1/self HTTP/1.1\r\nHost: silo.internal\r\nSilo-Origin: "+FormatOrigin(e.root, 1)+
		"\r\nSilo-Origin-Addr: 10.9.9.9\r\nConnection: close\r\nContent-Length: 2\r\n\r\n{}")
	if !strings.HasPrefix(resp, "HTTP/1.1 403") || n.Load() != 1 {
		t.Errorf("an attach with forged headers: %q (silod reached %d times)", resp, n.Load())
	}
	// A path below either endpoint is not it, and not the API either.
	for _, p := range []string{ToolsAttachPath + "/x", ToolsBinaryPath + "/../../silos/a", "/silo/v1/agents/-/tools/binary/x"} {
		resp := send(from, "GET "+p+" HTTP/1.1\r\nHost: silo.internal\r\nConnection: close\r\n\r\n")
		if strings.HasPrefix(resp, "HTTP/1.1 200") || strings.Contains(resp, `"silos"`) {
			t.Errorf("GET %s on port 80: %q", p, resp)
		}
	}
	// silod serves ToolsSilodPath to nothing but a forwarded attach:
	// without Silo-Origin it is the gateway's own and unknown.
	r := httptest.NewRequest("GET", ToolsSilodPath, nil)
	w := httptest.NewRecorder()
	Silod(e.db, SilodOptions{API: e.silod, Tools: e.tools}).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("%s without an origin: %d", ToolsSilodPath, w.Code)
	}
	// And the API port doesn't forward to it: /silod/ isn't served there.
	req, _ := http.NewRequest("GET", "http://"+e.api+ToolsSilodPath, nil)
	conn, err := net.Dial("tcp", e.api)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", from)
	req.Write(conn)
	ar, err := http.ReadResponse(bufioReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	ar.Body.Close()
	if ar.StatusCode != 404 || n.Load() != 1 {
		t.Errorf("%s on the API port: %d (silod's tool channels reached %d times)", ToolsSilodPath, ar.StatusCode, n.Load())
	}
}

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }
