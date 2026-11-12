package mcp

import (
	"context"
	"encoding/json"
	"io"
)

// Client is the calling side of an MCP connection: the router uses one
// per child tool-set server.
type Client struct {
	conn *Conn
	// OnToolsChanged, when set before Start, is called when the server
	// sends notifications/tools/list_changed.
	OnToolsChanged func()
}

// NewClient returns a client speaking over r/w.
func NewClient(r io.Reader, w io.Writer) *Client {
	c := &Client{}
	c.conn = NewConn(r, w, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "notifications/tools/list_changed":
			if c.OnToolsChanged != nil {
				c.OnToolsChanged()
			}
			return nil, nil
		case "ping":
			return map[string]any{}, nil
		}
		return nil, &Error{Code: MethodNotFound, Message: "method not found: " + method}
	})
	return c
}

// Start runs the read loop and performs the initialize handshake.
func (c *Client) Start(ctx context.Context, name string) error {
	go c.conn.Run(context.Background())
	var res json.RawMessage
	err := c.conn.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": name, "version": "1"},
	}, &res)
	if err != nil {
		return err
	}
	return c.conn.Notify("notifications/initialized", nil)
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// ListTools returns the server's tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.conn.Call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool calls one tool.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var res ToolResult
	if err := c.conn.Call(ctx, "tools/call", CallParams{Name: name, Arguments: args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
