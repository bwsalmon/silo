package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Conn is a client connection to one MCP server, whatever its transport:
// a launched server's stdio, Streamable HTTP, or the older HTTP+SSE
// (design §13.1). The gateway's proxy (gateway/proxy) speaks to every
// server it fronts through one.
type Conn interface {
	Initialize(ctx context.Context) (string, error)
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error)
	// Changed receives a value each time the server sends
	// notifications/tools/list_changed.
	Changed() <-chan struct{}
	// Done is closed when the connection is lost: a launched server's
	// stdout ended, or an SSE stream closed. Err then says why.
	Done() <-chan struct{}
	Err() error
	Close() error
}

// ErrClosed is the error of a call on a connection that has ended.
var ErrClosed = errors.New("mcp: connection closed")

// msgConn is a Conn over a message transport: one JSON-RPC message at a
// time each way, matched by id. stdio and HTTP+SSE are both this shape.
type msgConn struct {
	send    func(ctx context.Context, msg []byte) error
	closeFn func() error

	mu      sync.Mutex
	next    int64
	pending map[string]chan rpcReply
	err     error

	changed chan struct{}
	done    chan struct{}
	once    sync.Once
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

func newMsgConn(send func(context.Context, []byte) error, closeFn func() error) *msgConn {
	return &msgConn{
		send: send, closeFn: closeFn,
		pending: map[string]chan rpcReply{},
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// deliver handles one message from the server.
func (m *msgConn) deliver(data []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return // a server's stray log line on stdout
	}
	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case msg.Method == "notifications/tools/list_changed":
		select {
		case m.changed <- struct{}{}:
		default: // one pending "list again" is as good as several
		}
	case msg.Method != "" && hasID:
		// A request from the server. silo offers no client capabilities,
		// so it answers ping and refuses the rest.
		var resp []byte
		if msg.Method == "ping" {
			resp = marshal(rpcResponse{JSONRPC: "2.0", ID: msg.ID, Result: map[string]any{}})
		} else {
			resp = errorResponse(msg.ID, CodeMethodNotFound, "silo does not serve "+msg.Method)
		}
		go func() { _ = m.send(context.Background(), resp) }()
	case hasID:
		m.mu.Lock()
		ch := m.pending[string(msg.ID)]
		delete(m.pending, string(msg.ID))
		m.mu.Unlock()
		if ch != nil {
			r := rpcReply{result: msg.Result}
			if msg.Error != nil {
				r.err = msg.Error
			}
			ch <- r
		}
	}
}

// finish ends the connection with err, failing every call in flight.
func (m *msgConn) finish(err error) {
	m.once.Do(func() {
		if err == nil {
			err = ErrClosed
		}
		m.mu.Lock()
		m.err = err
		for id, ch := range m.pending {
			ch <- rpcReply{err: err}
			delete(m.pending, id)
		}
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *msgConn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcReply, 1)
	m.mu.Lock()
	if m.err != nil {
		err := m.err
		m.mu.Unlock()
		return nil, err
	}
	m.next++
	id := strconv.FormatInt(m.next, 10)
	m.pending[id] = ch
	m.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": m.next, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := m.send(ctx, marshal(msg)); err != nil {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("mcp: sending %s: %w", method, err)
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		// Tell the server to stop working on it; it may not listen.
		_ = m.send(context.Background(), marshal(map[string]any{
			"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": json.RawMessage(id), "reason": ctx.Err().Error()},
		}))
		return nil, ctx.Err()
	}
}

func (m *msgConn) Notify(ctx context.Context, method string) error {
	return m.send(ctx, marshal(map[string]any{"jsonrpc": "2.0", "method": method}))
}

func (m *msgConn) Initialize(ctx context.Context) (string, error) { return initialize(ctx, m) }
func (m *msgConn) ListTools(ctx context.Context) ([]ToolInfo, error) {
	return listTools(ctx, m)
}
func (m *msgConn) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	return callTool(ctx, m, name, args)
}
func (m *msgConn) Changed() <-chan struct{} { return m.changed }
func (m *msgConn) Done() <-chan struct{}    { return m.done }
func (m *msgConn) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *msgConn) Close() error {
	m.finish(ErrClosed)
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

// NewStdioConn speaks MCP's stdio transport: newline-delimited JSON-RPC
// written to w and read from r, a launched server's stdin and stdout.
// Close closes w, which a well-behaved server takes as its cue to exit;
// stopping the process is the caller's.
func NewStdioConn(r io.Reader, w io.WriteCloser) Conn {
	var wmu sync.Mutex
	m := newMsgConn(func(_ context.Context, msg []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := w.Write(append(msg, '\n'))
		return err
	}, w.Close)
	go func() {
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				m.deliver(line)
			}
			if err != nil {
				if err == io.EOF {
					err = errors.New("the server closed its stdout")
				}
				m.finish(err)
				return
			}
		}
	}()
	return m
}

// DialHTTP connects to an MCP server at a URL, as Streamable HTTP, or
// as the older HTTP+SSE transport when the server refuses a POST of
// initialize (the backwards-compatibility rule of the MCP spec), and
// performs the initialize handshake.
func DialHTTP(ctx context.Context, rawURL string, headers map[string]string) (Conn, error) {
	c := &Client{URL: rawURL, Headers: headers}
	hc := &httpConn{c: c, changed: make(chan struct{}, 1), done: make(chan struct{})}
	if _, err := c.Initialize(ctx); err == nil {
		hc.watch(ctx)
		return hc, nil
	} else if !isHTTP4xx(err) {
		return nil, err
	} else if sc, serr := dialSSE(ctx, rawURL, headers); serr == nil {
		return sc, nil
	} else {
		return nil, fmt.Errorf("%w (and as HTTP+SSE: %v)", err, serr)
	}
}

func isHTTP4xx(err error) bool {
	s := err.Error()
	for _, code := range []string{"HTTP 400", "HTTP 404", "HTTP 405"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// httpConn is a Conn over Streamable HTTP.
type httpConn struct {
	c       *Client
	changed chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	once    sync.Once
}

// watch opens the notification stream, when the server offers one, and
// forwards tools/list_changed. A server that offers none is fine: its
// tool list is then fixed.
func (h *httpConn) watch(ctx context.Context) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.cancel = cancel
	ch, err := h.c.Notifications(sctx)
	if err != nil {
		return
	}
	go func() {
		for method := range ch {
			if method == "notifications/tools/list_changed" {
				select {
				case h.changed <- struct{}{}:
				default:
				}
			}
		}
	}()
}

func (h *httpConn) Initialize(ctx context.Context) (string, error) { return h.c.Initialize(ctx) }
func (h *httpConn) ListTools(ctx context.Context) ([]ToolInfo, error) {
	return h.c.ListTools(ctx)
}
func (h *httpConn) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	return h.c.CallTool(ctx, name, args)
}
func (h *httpConn) Changed() <-chan struct{} { return h.changed }
func (h *httpConn) Done() <-chan struct{}    { return h.done }
func (h *httpConn) Err() error               { return nil }
func (h *httpConn) Close() error {
	h.once.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		close(h.done)
	})
	return nil
}

// dialSSE connects over HTTP+SSE (MCP 2024-11-05): a GET opens the
// stream, whose first event names the endpoint to POST messages to, and
// responses arrive on the stream.
func dialSSE(ctx context.Context, rawURL string, headers map[string]string) (Conn, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("mcp: GET %s: HTTP %d, %s", rawURL, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	endpoint := make(chan string, 1)
	var post atomic.Pointer[string]
	m := newMsgConn(func(ctx context.Context, msg []byte) error {
		p := post.Load()
		if p == nil {
			return errors.New("no endpoint yet")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *p, bytes.NewReader(msg))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}, func() error { cancel(); return nil })
	go func() {
		defer resp.Body.Close()
		first := true
		err := readSSE(resp.Body, func(data []byte) bool {
			if first && !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
				first = false
				endpoint <- string(bytes.TrimSpace(data))
				return true
			}
			m.deliver(data)
			return true
		})
		if err == nil {
			err = errors.New("the server closed its event stream")
		}
		m.finish(err)
	}()
	select {
	case ep := <-endpoint:
		u, err := base.Parse(ep)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("mcp: endpoint %q: %w", ep, err)
		}
		ps := u.String()
		post.Store(&ps)
	case <-m.Done():
		return nil, m.Err()
	case <-ctx.Done():
		m.Close()
		return nil, ctx.Err()
	}
	if _, err := m.Initialize(ctx); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}
