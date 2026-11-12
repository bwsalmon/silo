// Command clock is an MCP server (stdio, newline-delimited JSON-RPC)
// with one tool, now, using only the standard library.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ProtocolVersion string `json:"protocolVersion"`
		Name            string `json:"name"`
		Arguments       struct {
			Zone string `json:"zone"`
		} `json:"arguments"`
	} `json:"params"`
}

func main() {
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var req request
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue // unparseable, or a notification
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": req.Params.ProtocolVersion,
				"serverInfo":      map[string]string{"name": "clock", "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "now",
				"description": "The current time in an IANA time zone (default UTC).",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"zone": map[string]string{"type": "string"}}},
			}}}
		case "tools/call":
			text, isErr := now(req.Params.Arguments.Zone)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": isErr}
		default:
			result = map[string]any{}
		}
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func now(zone string) (string, bool) {
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return err.Error(), true
	}
	return time.Now().In(loc).Format(time.RFC1123), false
}
