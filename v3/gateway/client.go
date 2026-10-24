package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
)

// Client calls the gateway over its socket.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	id   atomic.Int64
}

// Dial connects to the gateway socket.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, br: bufio.NewReaderSize(conn, 64<<10)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call makes one request and decodes its result into result (if non-nil).
// A JSON-RPC error is returned as *Error. Calls on one Client must not be
// concurrent.
func (c *Client) Call(method string, params, result any) error {
	id := c.id.Add(1)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := json.NewEncoder(c.conn).Encode(req); err != nil {
		return err
	}
	line, err := readLine(c.br)
	if err != nil {
		return err
	}
	var resp struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if resp.ID != id {
		return fmt.Errorf("response id %d, want %d", resp.ID, id)
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}
