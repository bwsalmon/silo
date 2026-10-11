// Package toolchan is an agent's tool channel (agents.md, "The tool
// channel", SV-106; gateway.md, "Agent tool channels", SV-84): the
// connection silo-tools dials out from the tool VM, through the
// gateway's port 80, to the silod that hosts the agent's session.
//
// The attach is an HTTP/1.1 GET to /silo/v1/agents/-/tools:attach with
// "Upgrade: silo-tools-h2". Once silod has bound it to an agent it
// answers 101, and the connection carries HTTP/2 (golang.org/x/net/
// http2) the other way round: silod is the HTTP/2 client and silo-tools
// the server, so that every tool call is its own stream on the one
// connection silo-tools opened. silo-tools serves MCP at /mcp on it and
// listens on nothing.
//
// Hub is silod's end, Client silo-tools'.
package toolchan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// Protocol is the Upgrade token of an attach.
const Protocol = "silo-tools-h2"

// pingEvery is how often silod pings a channel, so that a dead one is
// noticed while no call is on it.
const pingEvery = 15 * time.Second

// Retryable is the JSON-RPC error code of a tool call that didn't reach
// the tool server: the channel is down, or dropped during the call. The
// error's data says {"retryable": true}.
const Retryable = -32001

// Hub is silod's end: the tool channels attached to it, one an agent.
type Hub struct {
	// Connected and Disconnected record an agent's tool_server as
	// CONNECTED, the first time finishing its create, and DISCONNECTED
	// (fenceapi's Server.ToolServerConnected and
	// ToolServerDisconnected).
	Connected    func(ctx context.Context, agent string) error
	Disconnected func(ctx context.Context, agent string) error
	// PingEvery overrides how often a channel is pinged; a test
	// shortens it.
	PingEvery time.Duration

	mu    sync.Mutex
	chans map[string]*channel
	wait  map[string]chan struct{} // closed when the agent next attaches
}

type channel struct {
	agent string
	cc    *http2.ClientConn
	conn  net.Conn
	done  chan struct{}
}

// Accept upgrades an attach that silod has bound to agent, and holds
// the channel until it drops. It replaces a channel the agent already
// has: the newest attach is the tool server that is running.
func (h *Hub) Accept(w http.ResponseWriter, r *http.Request, agent string) {
	if !upgrading(r) {
		http.Error(w, "silo: an attach must ask to upgrade to "+Protocol, http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "silo: this connection can't carry a tool channel", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	c := &channel{agent: agent, done: make(chan struct{})}
	c.conn = &watched{Conn: conn, r: brw.Reader, done: c.done}
	c.cc, err = (&http2.Transport{}).NewClientConn(c.conn)
	if err != nil {
		conn.Close()
		return
	}
	h.mu.Lock()
	if h.chans == nil {
		h.chans, h.wait = map[string]*channel{}, map[string]chan struct{}{}
	}
	old := h.chans[agent]
	h.chans[agent] = c
	if ch := h.wait[agent]; ch != nil {
		close(ch)
		delete(h.wait, agent)
	}
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
	ctx := context.WithoutCancel(r.Context())
	if h.Connected != nil {
		if err := h.Connected(ctx, agent); err != nil {
			log.Printf("tool channel: recording agent %s's tool server connected: %v", agent, err)
		}
	}
	h.hold(ctx, c)
}

// hold pings c until it fails or its connection ends, then forgets it.
func (h *Hub) hold(ctx context.Context, c *channel) {
	every := h.PingEvery
	if every == 0 {
		every = pingEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
loop:
	for {
		select {
		case <-c.done:
			break loop
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, every)
			err := c.cc.Ping(pctx)
			cancel()
			if err != nil {
				break loop
			}
		}
	}
	c.close()
	h.mu.Lock()
	current := h.chans[c.agent] == c
	if current {
		delete(h.chans, c.agent)
	}
	h.mu.Unlock()
	// A channel another attach replaced isn't a disconnect.
	if current && h.Disconnected != nil {
		if err := h.Disconnected(ctx, c.agent); err != nil {
			log.Printf("tool channel: recording agent %s's tool server disconnected: %v", c.agent, err)
		}
	}
}

func (c *channel) close() {
	c.cc.Close()
	c.conn.Close()
}

// Close drops agent's channel, as when its session moves to another
// control VM; the tool server's next attach is redirected there.
func (h *Hub) Close(agent string) {
	h.mu.Lock()
	c := h.chans[agent]
	h.mu.Unlock()
	if c != nil {
		c.close()
	}
}

// Attached reports whether agent has a channel here.
func (h *Hub) Attached(agent string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.chans[agent] != nil
}

// WaitAttached waits until agent has a channel, or ctx ends.
func (h *Hub) WaitAttached(ctx context.Context, agent string) error {
	for {
		h.mu.Lock()
		if h.chans[agent] != nil {
			h.mu.Unlock()
			return nil
		}
		if h.wait == nil {
			h.chans, h.wait = map[string]*channel{}, map[string]chan struct{}{}
		}
		ch := h.wait[agent]
		if ch == nil {
			ch = make(chan struct{})
			h.wait[agent] = ch
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// MCP sends one MCP JSON-RPC message down agent's channel and returns
// the tool server's answer. A request that can't reach the tool server
// -- no channel, or one that drops during the call -- is answered with
// a JSON-RPC error, code Retryable, that the harness can retry; a
// notification gets nil.
func (h *Hub) MCP(ctx context.Context, agent string, msg []byte) []byte {
	var id struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(msg, &id)
	h.mu.Lock()
	c := h.chans[agent]
	h.mu.Unlock()
	if c == nil {
		return retryable(id.ID, "silo: the agent's tool server isn't connected; retry")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://silo-tools/mcp", bytes.NewReader(msg))
	if err != nil {
		return retryable(id.ID, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.cc.RoundTrip(req)
	if err != nil {
		return retryable(id.ID, "silo: the tool channel dropped during the call; retry: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return retryable(id.ID, "silo: the tool channel dropped during the call; retry: "+err.Error())
	}
	if resp.StatusCode == http.StatusAccepted || len(id.ID) == 0 {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return rpcError(id.ID, -32603, fmt.Sprintf("silo: the tool server answered %s: %s", resp.Status, bytes.TrimSpace(body)), nil)
	}
	return body
}

func retryable(id json.RawMessage, msg string) []byte {
	return rpcError(id, Retryable, msg, map[string]any{"retryable": true})
}

func rpcError(id json.RawMessage, code int, msg string, data any) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
	return b
}

func upgrading(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHas(r.Header, "Connection", "upgrade") && headerHas(r.Header, "Upgrade", Protocol)
}

func headerHas(h http.Header, k, token string) bool {
	for _, v := range h.Values(k) {
		for _, t := range bytes.Split([]byte(v), []byte(",")) {
			if string(bytes.ToLower(bytes.TrimSpace(t))) == string(bytes.ToLower([]byte(token))) {
				return true
			}
		}
	}
	return false
}

// watched is a hijacked connection that reads first what the server
// had buffered, and closes done when it fails or is closed.
type watched struct {
	net.Conn
	r    *bufio.Reader
	done chan struct{}
	once sync.Once
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if err != nil {
		w.end()
	}
	return n, err
}

func (w *watched) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err != nil {
		w.end()
	}
	return n, err
}

func (w *watched) Close() error {
	w.end()
	return w.Conn.Close()
}

func (w *watched) end() { w.once.Do(func() { close(w.done) }) }

// ErrRefused is an attach the gateway or silod refused: 403, no agent's
// tool VM is at this address. The client goes on retrying, slowly: the
// agent's row may not have its address yet.
var ErrRefused = errors.New("attach refused")
