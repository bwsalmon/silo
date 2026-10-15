// Package fakecli is a stand-in for Claude Code in headless mode, for
// tests that run whole sessions: a test binary whose TestMain calls Main
// when it is run through a symlink named claude (or codex, for the
// stand-in codex of Codex). It answers --version,
// reads the staged MCP config, optionally calls the gateway, writes
// recorded stream-json to stdout, and then does what $HOME/fake.json
// says. HOME is a test's one channel to it.
package fakecli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/old/mcp"
)

// Config is $HOME/fake.json.
type Config struct {
	// Call is a gateway tool to call, with Args, before Stream.
	Call string         `json:"call,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	// Stream is written to stdout, a line each.
	Stream []string `json:"stream,omitempty"`
	// Exit is the exit code once done, unless Wait is set.
	Exit int `json:"exit"`
	// Wait blocks until SIGINT or SIGTERM, and exits 130 or 143.
	Wait bool `json:"wait,omitempty"`
	// Echo makes the fake read stream-json user messages from stdin
	// until it ends, answering each with assistant text "got: <text>",
	// and with a result after the Answers'th; it then exits 0.
	Echo    bool `json:"echo,omitempty"`
	Answers int  `json:"answers,omitempty"`
}

// Report is $HOME/report.json: what the fake saw.
type Report struct {
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin"`
	Token string   `json:"token"`
	Error string   `json:"error,omitempty"`
}

// Is reports whether this process was started as the fake.
func Is() bool {
	n := filepath.Base(os.Args[0])
	return n == "claude" || n == "codex"
}

// Main runs the fake and returns its exit code.
func Main() int {
	if filepath.Base(os.Args[0]) == "codex" {
		return codexMain()
	}
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.197 (Claude Code)")
		return 0
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	home := os.Getenv("HOME")
	var cfg Config
	if b, err := os.ReadFile(filepath.Join(home, "fake.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	rep := Report{Argv: os.Args}
	if cfg.Echo {
		return echo(home, rep, cfg)
	}
	b := make([]byte, 1<<16)
	n, _ := os.Stdin.Read(b)
	rep.Stdin = string(b[:n])
	if err := callGateway(args, cfg, &rep); err != nil {
		rep.Error = err.Error()
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	os.WriteFile(filepath.Join(home, "report.json"), out, 0o600)
	for _, l := range cfg.Stream {
		fmt.Println(l)
	}
	if !cfg.Wait {
		return cfg.Exit
	}
	if sig := <-sigs; sig == syscall.SIGTERM {
		return 143
	}
	return 130
}

func callGateway(args []string, cfg Config, rep *Report) error {
	var file string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			file = args[i+1]
		}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var conf struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &conf); err != nil {
		return err
	}
	srv := conf.MCPServers["silo"]
	rep.Token = strings.TrimPrefix(srv.Headers["Authorization"], "Bearer ")
	if cfg.Call == "" {
		return nil
	}
	c := mcp.NewClient(srv.URL, rep.Token)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		return err
	}
	_, err = c.CallTool(ctx, cfg.Call, cfg.Args)
	return err
}

// echo is Config.Echo: a claude that takes messages on stdin mid-run.
func echo(home string, rep Report, cfg Config) int {
	session := "echo-1"
	for i, a := range os.Args {
		if a == "--resume" && i+1 < len(os.Args) {
			session = os.Args[i+1]
		}
	}
	out := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	out(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": "claude-opus-5-5"})
	sc := bufio.NewScanner(os.Stdin)
	n := 0
	for sc.Scan() {
		var m struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.Message.Content) == 0 {
			continue
		}
		text := "got: " + m.Message.Content[0].Text
		rep.Stdin += sc.Text() + "\n"
		n++
		out(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{
			"id": fmt.Sprintf("m%d", n), "content": []map[string]string{{"type": "text", "text": text}}}})
		if n >= cfg.Answers {
			out(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "session_id": session})
		}
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	os.WriteFile(filepath.Join(home, "report.json"), b, 0o600)
	return 0
}

// GoFile, created in the fake codex's HOME, ends its first turn.
const GoFile = "go"

// codexMain is a stand-in for codex exec --json: the first run names
// thread th-1, says "working" and ends its turn once $HOME/go exists;
// codex exec resume th-1 - answers "resumed: <stdin>". Each run appends
// its argv to $HOME/codex-runs.
func codexMain() int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 0.153.0")
		return 0
	}
	home := os.Getenv("HOME")
	in, _ := io.ReadAll(os.Stdin)
	if f, err := os.OpenFile(filepath.Join(home, "codex-runs"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	msg := func(text string) {
		b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "i1", "type": "agent_message", "text": text}})
		fmt.Println(string(b))
	}
	resume := len(args) > 1 && args[0] == "exec" && args[1] == "resume"
	if !resume {
		fmt.Println(`{"type":"thread.started","thread_id":"th-1"}`)
	}
	fmt.Println(`{"type":"turn.started"}`)
	if resume {
		msg("resumed: " + string(in))
	} else {
		msg("working")
		for {
			if _, err := os.Stat(filepath.Join(home, GoFile)); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		msg("done")
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	return 0
}
