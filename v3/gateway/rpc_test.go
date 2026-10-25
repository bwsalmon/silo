package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve starts the server on a real Unix socket. cred decides who each
// connection is, keyed by the order in which connections are accepted.
func serve(t *testing.T, e *env, creds ...PeerCred) string {
	t.Helper()
	l, err := Listen(e.cfg.SocketPath, -1)
	if err != nil {
		t.Fatal(err)
	}
	next := make(chan PeerCred, len(creds))
	for _, c := range creds {
		next <- c
	}
	srv := &Server{G: e.g, Cred: func(net.Conn) (PeerCred, error) { return <-next, nil }}
	done := make(chan struct{})
	go func() { srv.Serve(context.Background(), l); close(done) }()
	t.Cleanup(func() { l.Close(); <-done })
	return e.cfg.SocketPath
}

func dial(t *testing.T, path string) *Client {
	t.Helper()
	c, err := Dial(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRPCEndToEnd(t *testing.T) {
	e := newEnv(t)
	sock := serve(t, e, PeerCred{UID: 1000, PID: 7}, PeerCred{UID: 4321, PID: 8}, PeerCred{UID: 4321, PID: 9})
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o660 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v %v", fi.Mode(), err)
	}

	c := dial(t, sock)
	var reg RegisterResult
	if err := c.Call("agent.register", map[string]any{"name": "w", "runtime": "claude", "brief": "hi", "limits": map[string]any{"memory_max": "1G"}}, &reg); err != nil {
		t.Fatal(err)
	}
	if ev := e.lastEvent(EvRegister); ev["_actor"] != "admin:uid=1000" {
		t.Fatalf("actor %v", ev["_actor"])
	}
	var list []AgentInfo
	if err := c.Call("agent.list", nil, &list); err != nil || len(list) != 3 {
		t.Fatalf("list %v %v", list, err)
	}

	// The second connection is a stranger: it may connect with the token
	// and, as the agent, list itself; with no token it is denied, audited.
	c2 := dial(t, sock)
	var cr ConnectResult
	if err := c2.Call("agent.connect", map[string]any{"token": reg.Token}, &cr); err != nil {
		t.Fatal(err)
	}
	if err := c2.Call("agent.list", map[string]any{"agent_token": cr.SessionToken}, &list); err != nil || len(list) != 1 {
		t.Fatalf("agent list %v %v", list, err)
	}
	if err := c2.Call("audit.read", map[string]any{"agent_token": cr.SessionToken}, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("agent audit.read: %v", err)
	}
	c3 := dial(t, sock)
	if err := c3.Call("agent.list", nil, nil); codeOf(err) != CodeForbidden {
		t.Fatalf("stranger: %v", err)
	}
	if ev := e.lastEvent(EvAuthDeny); ev["uid"] != float64(4321) || ev["method"] != "agent.list" {
		t.Fatalf("deny audit %v", ev)
	}

	var st AgentInfo
	if err := c.Call("agent.status", map[string]any{"agent": "w"}, &st); err != nil || st.Brief != "hi" || st.State != StateConnected {
		t.Fatalf("status %+v %v", st, err)
	}
	var stop StopResult
	if err := c.Call("agent.stop", map[string]any{"agent": "w", "mode": "graceful"}, &stop); err != nil || stop.Tombstone == nil {
		t.Fatalf("stop %+v %v", stop, err)
	}
	var vr VerifyResult
	if err := c.Call("audit.verify", nil, &vr); err != nil || !vr.OK {
		t.Fatalf("verify %+v %v", vr, err)
	}
	var entries []AuditEntry
	if err := c.Call("audit.read", map[string]any{"after": 0, "limit": 2}, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("audit.read %v %v", entries, err)
	}
	if err := c.Call("nope", nil, nil); codeOf(err) != CodeMethodNotFound {
		t.Errorf("nope: %v", err)
	}
	if err := c.Call("spawn", map[string]any{"name": "w"}, nil); codeOf(err) != CodeMethodNotFound {
		t.Errorf("spawn is gone: %v", err)
	}
	if err := c.Call("agent.register", map[string]any{"name": "w", "bogus": 1}, nil); codeOf(err) != CodeInvalidParams {
		t.Errorf("unknown param: %v", err)
	}
}

func TestRPCFraming(t *testing.T) {
	e := newEnv(t)
	sock := serve(t, e, PeerCred{UID: 0})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	send := func(s string) map[string]any {
		t.Helper()
		if _, err := conn.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		return m
	}
	errCode := func(m map[string]any) float64 {
		e, _ := m["error"].(map[string]any)
		c, _ := e["code"].(float64)
		return c
	}
	if m := send("{not json"); errCode(m) != CodeParseError {
		t.Errorf("parse error: %v", m)
	}
	if m := send(`{"id":1,"method":"agent.list"}`); errCode(m) != CodeInvalidRequest || m["id"] != float64(1) {
		t.Errorf("missing jsonrpc: %v", m)
	}
	// A notification gets no reply, so the next reply is for id "x".
	if m := send(`{"jsonrpc":"2.0","method":"agent.list"}` + "\n" + `{"jsonrpc":"2.0","id":"x","method":"agent.list"}`); m["id"] != "x" || m["result"] == nil {
		t.Errorf("after notification: %v", m)
	}
}

func TestPeerCredOf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := net.Dial("unix", path)
		if err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cred, err := PeerCredOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	if cred.UID != os.Getuid() || cred.PID != os.Getpid() {
		t.Fatalf("cred %+v, want uid %d pid %d", cred, os.Getuid(), os.Getpid())
	}
	if _, err := PeerCredOf(&net.TCPConn{}); err == nil {
		t.Fatal("PeerCredOf a TCP conn succeeded")
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.sock")
	l, err := Listen(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, -1); err == nil || !strings.Contains(err.Error(), "another gateway") {
		t.Fatalf("second listen: %v", err)
	}
	l.Close()
	// A stale socket file (left by a crash) is replaced.
	ul, _ := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	ul.SetUnlinkOnClose(false)
	ul.Close()
	l, err = Listen(path, -1)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	l.Close()
	os.WriteFile(path, nil, 0o600)
	if _, err := Listen(path, -1); err == nil {
		t.Fatal("replaced a regular file")
	}
}

func TestNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify")
	pc, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := pc.Read(buf)
	if string(buf[:n]) != "READY=1" {
		t.Fatalf("got %q", buf[:n])
	}
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
}
