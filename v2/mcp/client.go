// Adapted from github.com/bwsalmon/grain pkg/mcp/client.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// CallResult is a tools/call response.
type CallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// Text concatenates every text content block.
func (r *CallResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Client speaks the client side of Streamable HTTP to one MCP server,
// POSTing each request and reading a JSON response. Responses a server
// sends as an SSE stream are read too, up to the response itself.
type Client struct {
	URL   string
	Token string
	// HTTP is the client used; http.DefaultClient when nil.
	HTTP *http.Client

	nextID    atomic.Int64
	sessionID atomic.Value // string: Mcp-Session-Id, when a server sets one
}

// NewClient returns a client for the server at url, sending token as a
// bearer token when it is not empty.
func NewClient(url, token string) *Client {
	return &Client{URL: url, Token: token}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if id, _ := c.sessionID.Load().(string); id != "" {
		req.Header.Set("Mcp-Session-Id", id)
	}
	return req, nil
}

func (c *Client) post(ctx context.Context, msg map[string]any) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodPost, marshal(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.sessionID.Store(id)
	}
	return resp, nil
}

// Call sends one request and returns its result, or the server's
// JSON-RPC error as an *Error.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	resp, err := c.post(ctx, msg)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("mcp: %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var line []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		line, err = readSSEResponse(resp.Body, strconv.FormatInt(id, 10))
	} else {
		line, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: reading %s response: %w", method, err)
	}
	var out struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &out); err != nil {
		return nil, fmt.Errorf("mcp: decoding %s response: %w", method, err)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	if got, want := string(out.ID), strconv.FormatInt(id, 10); got != want {
		return nil, fmt.Errorf("mcp: response id %s does not match request id %s", got, want)
	}
	return out.Result, nil
}

// readSSEResponse reads an SSE stream up to the message answering id.
func readSSEResponse(r io.Reader, id string) ([]byte, error) {
	var found []byte
	err := readSSE(r, func(data []byte) bool {
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(data, &m) == nil && string(m.ID) == id {
			found = data
			return false
		}
		return true
	})
	if found != nil {
		return found, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// readSSE calls fn with each event's data until fn returns false or the
// stream ends.
func readSSE(r io.Reader, fn func(data []byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxRequestBytes)
	var data []byte
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				if !fn(data) {
					return nil
				}
				data = nil
			}
		case strings.HasPrefix(line, "data:"):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
	}
	return sc.Err()
}

// Notify sends a notification, which has no response.
func (c *Client) Notify(ctx context.Context, method string) error {
	resp, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mcp: %s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// Initialize performs the initialize handshake and sends
// notifications/initialized, returning the server's protocol version.
func (c *Client) Initialize(ctx context.Context) (string, error) {
	raw, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "silo", "version": serverVersion},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mcp: decoding initialize result: %w", err)
	}
	return out.ProtocolVersion, c.Notify(ctx, "notifications/initialized")
}

// ListTools issues tools/list.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	raw, err := c.Call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: decoding tools/list result: %w", err)
	}
	return out.Tools, nil
}

// CallTool issues tools/call. A refused call is an *Error; a tool that
// ran and failed is a result with IsError set.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	raw, err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var out CallResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: decoding tools/call result: %w", err)
	}
	return &out, nil
}

// Notifications opens the server-to-client stream and sends the method
// of each notification on the returned channel, which is closed when
// the stream ends or ctx is done. It returns once the stream is open.
func (c *Client) Notifications(ctx context.Context) (<-chan string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("mcp: opening the notification stream: HTTP %d", resp.StatusCode)
	}
	ch := make(chan string, 8)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		_ = readSSE(resp.Body, func(data []byte) bool {
			var m struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(data, &m) == nil && m.Method != "" {
				select {
				case ch <- m.Method:
				case <-ctx.Done():
					return false
				}
			}
			return true
		})
	}()
	return ch, nil
}
