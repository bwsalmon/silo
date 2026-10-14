// Package fakecli is a stand-in for Claude Code in headless mode, for
// tests that run whole sessions: a test binary whose TestMain calls Main
// when it is run through a symlink named claude. It answers --version,
// reads the staged MCP config, optionally calls the gateway, writes
// recorded stream-json to stdout, and then does what $HOME/fake.json
// says. HOME is a test's one channel to it.
package fakecli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/mcp"
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
}

// Report is $HOME/report.json: what the fake saw.
type Report struct {
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin"`
	Token string   `json:"token"`
	Error string   `json:"error,omitempty"`
}

// Is reports whether this process was started as the fake.
func Is() bool { return filepath.Base(os.Args[0]) == "claude" }

// Main runs the fake and returns its exit code.
func Main() int {
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
