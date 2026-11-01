package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPServer_Lifecycle(t *testing.T) {
	server := NewServer("test-server", "1.0.0")

	// Register dummy tool
	server.RegisterTool(
		Tool{
			Name:        "echo",
			Description: "Echoes input text",
			InputSchema: map[string]interface{}{"type": "object"},
		},
		func(ctx context.Context, args json.RawMessage) (*ToolResult, error) {
			var parsed struct {
				Msg string `json:"msg"`
			}
			json.Unmarshal(args, &parsed)
			return &ToolResult{
				Content: []ToolContent{{Type: "text", Text: "echo: " + parsed.Msg}},
			}, nil
		},
	)

	// Simulate client stdio requests
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hello silo"}}}
`
	in := strings.NewReader(input)
	out := &bytes.Buffer{}

	ctx := context.Background()
	if err := server.Serve(ctx, in, out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 { // initialize, tools/list, tools/call
		t.Fatalf("expected 3 responses, got %d:\n%s", len(lines), out.String())
	}

	// Verify initialize response
	var initResp JSONRPCMessage
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("parse init resp: %v", err)
	}
	resMap := initResp.Result.(map[string]interface{})
	if resMap["protocolVersion"] != "2024-11-05" {
		t.Errorf("unexpected protocolVersion: %v", resMap["protocolVersion"])
	}

	// Verify tools/call response
	var callResp JSONRPCMessage
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil {
		t.Fatalf("parse call resp: %v", err)
	}
	callResult := callResp.Result.(map[string]interface{})
	contentArr := callResult["content"].([]interface{})
	firstContent := contentArr[0].(map[string]interface{})
	if firstContent["text"] != "echo: hello silo" {
		t.Errorf("unexpected tool output: %v", firstContent["text"])
	}
}
