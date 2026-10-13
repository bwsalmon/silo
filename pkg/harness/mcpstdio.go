package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// MCPStdio is silod mcp-stdio, an ambient session's MCP server
// (local-backend.md §10): the CLI starts it as a stdio server, and it
// carries each JSON-RPC message the CLI writes on in to the session
// socket's /mcp, writing each answer to out as one line. It is a child
// of the CLI, so it runs in the session's scope, and the socket's peer
// check (call L18) sees the connection come from there; a TCP forwarder
// on the host's loopback, as a container uses, would let any process on
// the host in.
//
// Requests are carried at once, each on its own connection, so that a
// long tool call doesn't hold up the next; answers are written as they
// come. A request the socket can't be reached for gets a JSON-RPC
// error, so the CLI isn't left waiting.
func MCPStdio(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	var mu sync.Mutex
	write := func(b []byte) {
		b = bytes.TrimRight(b, "\r\n")
		if len(b) == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out.Write(append(b, '\n'))
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		msg := bytes.TrimSpace(sc.Bytes())
		if len(msg) == 0 {
			continue
		}
		msg = bytes.Clone(msg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := postMCP(ctx, client, msg)
			if err != nil {
				if id := rpcID(msg); id != nil {
					e, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
						"error": map[string]any{"code": -32603, "message": "silo: the session socket: " + err.Error()}})
					write(e)
				}
				return
			}
			write(b)
		}()
	}
	return sc.Err()
}

func postMCP(ctx context.Context, c *http.Client, msg []byte) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://silo/mcp", bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

// rpcID is a JSON-RPC message's id, or nil for a notification.
func rpcID(msg []byte) json.RawMessage {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(msg, &m) != nil || len(m.ID) == 0 || string(m.ID) == "null" {
		return nil
	}
	return m.ID
}
