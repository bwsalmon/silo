// Command clock is an MCP server with one tool, now, serving the
// Streamable HTTP transport at http://127.0.0.1:<port>/mcp using only
// the standard library. Every request is answered with a single JSON
// response; it sends no notifications, so it offers no GET stream.
package main

import (
	"encoding/json"
	"log"
	"net/http"
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
	if len(os.Args) != 2 {
		log.Fatal("usage: clock <port>")
	}
	http.HandleFunc("/mcp", serve)
	// Loopback only: silo refuses a tool set that listens anywhere else.
	log.Fatal(http.ListenAndServe("127.0.0.1:"+os.Args[1], nil))
}

func serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON-RPC message", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted) // a notification
		return
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
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
