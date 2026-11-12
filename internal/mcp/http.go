package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Streamable HTTP headers.
const (
	SessionHeader  = "Mcp-Session-Id"
	ProtocolHeader = "MCP-Protocol-Version"
)

// maxHTTPBody caps one response body or server-sent event.
const maxHTTPBody = 32 << 20

// HTTPClient is the calling side of an MCP connection over the
// Streamable HTTP transport: every message is a POST to one endpoint,
// answered with JSON or with a stream of server-sent events, and a GET
// on the same endpoint carries notifications the server sends on its
// own. The router uses one per tool-set server.
type HTTPClient struct {
	// URL is the MCP endpoint, e.g. http://127.0.0.1:4242/mcp.
	URL string
	// OnToolsChanged, when set before Start, is called when the server
	// sends notifications/tools/list_changed.
	OnToolsChanged func()

	hc       *http.Client
	mu       sync.Mutex
	nextID   int64
	session  string
	protocol string
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewHTTPClient returns a client for the endpoint url. It never uses a
// proxy and never follows redirects: a tool-set server is on loopback,
// and nothing it says should send silo's requests anywhere else.
func NewHTTPClient(url string) *HTTPClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &HTTPClient{
		URL: url,
		hc: &http.Client{
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start performs the initialize handshake and opens the GET stream for
// the server's own notifications.
func (c *HTTPClient) Start(ctx context.Context, name string) error {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": name, "version": "1"},
	}, &res)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.protocol = res.ProtocolVersion
	c.mu.Unlock()
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return err
	}
	go c.listen()
	return nil
}

// ListTools returns the server's tools.
func (c *HTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool calls one tool.
func (c *HTTPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res ToolResult
	if err := c.call(ctx, "tools/call", CallParams{Name: name, Arguments: args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Close stops the notification stream and ends the server's session
// (DELETE), without waiting long for a server that is going away.
func (c *HTTPClient) Close() error {
	c.cancel()
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.URL, nil); err == nil {
			c.headers(req)
			if resp, err := c.hc.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	c.hc.CloseIdleConnections()
	return nil
}

func (c *HTTPClient) headers(req *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		req.Header.Set(SessionHeader, c.session)
	}
	if c.protocol != "" {
		req.Header.Set(ProtocolHeader, c.protocol)
	}
}

func (c *HTTPClient) post(ctx context.Context, msg *Message) (*http.Response, error) {
	msg.JSONRPC = "2.0"
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.headers(req)
	return c.hc.Do(req)
}

// call sends a request and decodes its result into out (which may be nil).
func (c *HTTPClient) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	c.mu.Unlock()
	msg := &Message{ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = raw
	}
	resp, err := c.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(method, resp)
	}
	if method == "initialize" {
		if s := resp.Header.Get(SessionHeader); s != "" {
			c.mu.Lock()
			c.session = s
			c.mu.Unlock()
		}
	}
	var result *Message
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
		if err != nil {
			return err
		}
		result, err = findResponse(data, id)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
	case "text/event-stream":
		err := readEvents(resp.Body, func(data []byte) bool {
			if m, _ := findResponse(data, id); m != nil {
				result = m
				return true
			}
			c.dispatch(data)
			return false
		})
		if err != nil && result == nil {
			return fmt.Errorf("%s: reading event stream: %w", method, err)
		}
	default:
		return fmt.Errorf("%s: unexpected response content type %q", method, resp.Header.Get("Content-Type"))
	}
	if result == nil {
		return fmt.Errorf("%s: the server sent no response", method)
	}
	if result.Error != nil {
		return result.Error
	}
	if out != nil && len(result.Result) > 0 {
		if err := json.Unmarshal(result.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// notify sends a notification, or a response to a server's request.
func (c *HTTPClient) notify(ctx context.Context, method string, params any) error {
	msg := &Message{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = raw
	}
	return c.send(ctx, msg)
}

func (c *HTTPClient) send(ctx context.Context, msg *Message) error {
	resp, err := c.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPBody))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return statusError(msg.Method, resp)
	}
	return nil
}

// dispatch handles a message the server sent other than the response
// being waited for: a notification, or a request (only ping is
// answered).
func (c *HTTPClient) dispatch(data []byte) {
	var msg Message
	if json.Unmarshal(bytes.TrimSpace(data), &msg) != nil || msg.Method == "" {
		return
	}
	if len(msg.ID) > 0 {
		reply := &Message{ID: msg.ID}
		if msg.Method == "ping" {
			reply.Result = json.RawMessage("{}")
		} else {
			reply.Error = &Error{Code: MethodNotFound, Message: "method not found: " + msg.Method}
		}
		go c.send(c.ctx, reply)
		return
	}
	if msg.Method == "notifications/tools/list_changed" && c.OnToolsChanged != nil {
		go c.OnToolsChanged()
	}
}

// listen holds the GET stream open for the server's own notifications,
// reconnecting while the client is open. A server that does not offer
// the stream (405) is left alone.
func (c *HTTPClient) listen() {
	for c.ctx.Err() == nil {
		req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.URL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		c.headers(req)
		resp, err := c.hc.Do(req)
		if err == nil {
			ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			if resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
				resp.Body.Close()
				return
			}
			readEvents(resp.Body, func(data []byte) bool { c.dispatch(data); return false })
			resp.Body.Close()
		}
		select {
		case <-c.ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// findResponse returns the response with the given id in data, a JSON
// message or batch.
func findResponse(data []byte, id json.RawMessage) (*Message, error) {
	data = bytes.TrimSpace(data)
	var msgs []Message
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &msgs); err != nil {
			return nil, err
		}
	} else {
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		msgs = []Message{m}
	}
	for i := range msgs {
		if msgs[i].Method == "" && bytes.Equal(bytes.TrimSpace(msgs[i].ID), id) {
			return &msgs[i], nil
		}
	}
	return nil, errors.New("no response with the request's id")
}

// readEvents reads server-sent events from r, calling f with each
// event's data until f returns true or the stream ends.
func readEvents(r io.Reader, f func(data []byte) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if data.Len() > 0 {
					if f(data.Bytes()) {
						return nil
					}
					data.Reset()
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				if data.Len() > maxHTTPBody {
					return errors.New("event too large")
				}
			}
		}
		if err != nil {
			if data.Len() > 0 && f(data.Bytes()) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func statusError(method string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if msg != "" {
		msg = ": " + msg
	}
	return fmt.Errorf("%s: HTTP %s%s", method, resp.Status, msg)
}
