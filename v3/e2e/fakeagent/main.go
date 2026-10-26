// Command fakeagent is the end-to-end tests' stand-in for a coding-agent
// CLI: it speaks ACP (JSON-RPC 2.0, one message per line) on stdio like
// `gemini --experimental-acp` does, so the whole stack can be driven
// without model credentials. Its "model" is a handful of directives:
//
//	mcp <tool> <json-args>   call a silo MCP tool through `silo mcp`, as a
//	                         real CLI does through its MCP configuration,
//	                         and reply with the tool's result
//	spawn <json-args>        register_agent with those args, then start the
//	                         child with `silo connect --token <token> --
//	                         <this binary>` as a subprocess (location
//	                         "subprocess"), and reply with the child's id
//	anything else            reply "echo: <text>", streamed in chunks
//
// Every reply is streamed as session/update agent_message_chunk
// notifications before the session/prompt response ends the turn.
//
// The silo binary is $SILO_BIN, or `silo` on PATH.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

var (
	outMu sync.Mutex
	out   = os.Stdout
)

func send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	outMu.Lock()
	defer outMu.Unlock()
	out.Write(append(b, '\n'))
}

func chunk(sid, text string) {
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	})
}

func siloBin() string {
	if b := os.Getenv("SILO_BIN"); b != "" {
		return b
	}
	return "silo"
}

var (
	childrenMu sync.Mutex
	children   []*exec.Cmd
)

func main() {
	sid := fmt.Sprintf("fake-%d", os.Getpid())
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.Method == "" {
			continue
		}
		switch m.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "fakeagent", "version": "1"},
			}})
		case "session/new", "session/load":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": sid}})
		case "session/prompt":
			go prompt(sid, m)
		case "session/cancel":
			if len(m.ID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
			}
		default:
			if len(m.ID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown method " + m.Method}})
			}
		}
	}
	// stdin closed: silo connect is stopping us. Take any children along,
	// as a parent's own process tree would be.
	childrenMu.Lock()
	for _, c := range children {
		if c.Process != nil {
			c.Process.Signal(os.Interrupt)
		}
	}
	childrenMu.Unlock()
}

func prompt(sid string, m msg) {
	var p struct {
		Prompt []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var parts []string
	for _, b := range p.Prompt {
		parts = append(parts, b.Text)
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	verb, rest, _ := strings.Cut(text, " ")

	var reply string
	var err error
	switch verb {
	case "mcp":
		tool, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
		chunk(sid, "calling "+tool+"\n")
		reply, err = callTool(tool, args)
	case "spawn":
		chunk(sid, "registering a child\n")
		reply, err = spawn(rest)
	default:
		chunk(sid, "echo: ")
		reply = text
	}
	if err != nil {
		reply = "error: " + err.Error()
	}
	chunk(sid, reply)
	send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"stopReason": "end_turn"}})
}

// callTool runs one MCP tools/call through a fresh `silo mcp` and returns
// the tool's text result.
func callTool(tool, args string) (string, error) {
	var a map[string]any
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, siloBin(), "mcp")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() { in.Close(); cmd.Wait() }()
	r := bufio.NewReader(stdout)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := in.Write(append(b, '\n')); err != nil {
			return nil, err
		}
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("silo mcp: %w", err)
			}
			var resp msg
			if json.Unmarshal(line, &resp) != nil || string(resp.ID) != fmt.Sprint(id) {
				continue
			}
			if len(resp.Error) > 0 && string(resp.Error) != "null" {
				return nil, fmt.Errorf("%s", resp.Error)
			}
			return resp.Result, nil
		}
	}
	if _, err := call(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "fakeagent", "version": "1"}}); err != nil {
		return "", err
	}
	res, err := call(2, "tools/call", map[string]any{"name": tool, "arguments": a})
	if err != nil {
		return "", err
	}
	var cr struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &cr); err != nil {
		return "", err
	}
	var texts []string
	for _, c := range cr.Content {
		texts = append(texts, c.Text)
	}
	if cr.IsError {
		return "", errors.New(strings.Join(texts, "\n"))
	}
	return strings.Join(texts, "\n"), nil
}

// spawn registers a child and starts it in a subprocess with silo connect.
func spawn(args string) (string, error) {
	res, err := callTool("register_agent", args)
	if err != nil {
		return "", err
	}
	var reg struct {
		Agent string `json:"agent"`
		Token string `json:"token"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(res), &reg); err != nil {
		return "", fmt.Errorf("register_agent result %q: %w", res, err)
	}
	if reg.Token == "" {
		return "", fmt.Errorf("register_agent returned no token (state %q)", reg.State)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(siloBin(), "connect", "--token", reg.Token, "--", self)
	// The child dials the same hub its parent did: silo connect gave us
	// SILO_BRIDGE_SOCKET. It must not inherit our own session token.
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SILO_AGENT_TOKEN=") && !strings.HasPrefix(kv, "SILO_AGENT=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	logf, err := os.CreateTemp("", "fakeagent-child-*.log")
	if err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	} else {
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	childrenMu.Lock()
	children = append(children, cmd)
	childrenMu.Unlock()
	go cmd.Wait()
	return fmt.Sprintf("started child %s (pid %d)", reg.Agent, cmd.Process.Pid), nil
}
