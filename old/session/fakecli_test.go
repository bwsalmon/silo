package session_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/internal/mcptest"
	"github.com/bwsalmon/silo/old/mcp"
)

// The fake CLI is this test binary, run through a symlink named claude.
// It answers --version like Claude Code, reads the staged MCP config,
// calls the gateway, writes what it saw to $HOME/report.json, and then
// does what $HOME/fake.json says. HOME is the one channel a test has to
// it: everything else in the environment is scrubbed.

// fakeConfig is $HOME/fake.json.
type fakeConfig struct {
	// Call is a gateway tool to call, with Args.
	Call string         `json:"call,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	// Exit is the exit code once done, unless Wait is set.
	Exit int `json:"exit"`
	// Wait blocks until a signal: SIGINT exits IntExit unless IgnoreInt.
	Wait      bool `json:"wait,omitempty"`
	IntExit   int  `json:"int_exit,omitempty"`
	IgnoreInt bool `json:"ignore_int,omitempty"`
}

// fakeReport is $HOME/report.json.
type fakeReport struct {
	PID    int      `json:"pid"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	Tools  []string `json:"tools"`
	Result string   `json:"result,omitempty"`
	Error  string   `json:"error,omitempty"`
}

func TestMain(m *testing.M) {
	// The test MCP server of mcp_test.go, launched by silo.
	mcptest.MaybeServe()
	if filepath.Base(os.Args[0]) == "claude" {
		os.Exit(fakeCLI())
	}
	os.Exit(m.Run())
}

func fakeCLI() int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.197 (Claude Code)")
		return 0
	}
	ints := make(chan os.Signal, 4)
	signal.Notify(ints, syscall.SIGINT, syscall.SIGTERM)
	home := os.Getenv("HOME")
	var cfg fakeConfig
	if b, err := os.ReadFile(filepath.Join(home, "fake.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	rep := fakeReport{PID: os.Getpid(), Argv: os.Args, Env: os.Environ()}
	rep.Cwd, _ = os.Getwd()
	if st, _ := os.Stdin.Stat(); st != nil && st.Mode()&os.ModeCharDevice == 0 {
		b := make([]byte, 1<<16)
		n, _ := os.Stdin.Read(b)
		rep.Stdin = string(b[:n])
	}
	write := func() {
		b, _ := json.MarshalIndent(rep, "", "  ")
		os.WriteFile(filepath.Join(home, "report.tmp"), b, 0o600)
		os.Rename(filepath.Join(home, "report.tmp"), filepath.Join(home, "report.json"))
	}
	if err := callGateway(args, cfg, &rep); err != nil {
		rep.Error = err.Error()
	}
	write()
	if !cfg.Wait {
		return cfg.Exit
	}
	for sig := range ints {
		if sig == syscall.SIGTERM {
			return 143
		}
		if !cfg.IgnoreInt {
			return cfg.IntExit
		}
	}
	return 0
}

func callGateway(args []string, cfg fakeConfig, rep *fakeReport) error {
	var file string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			file = args[i+1]
		}
	}
	if file == "" {
		return fmt.Errorf("no --mcp-config")
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
	if len(conf.MCPServers) != 1 {
		return fmt.Errorf("MCP config names %d servers, want only the gateway", len(conf.MCPServers))
	}
	srv := conf.MCPServers["silo"]
	c := mcp.NewClient(srv.URL, strings.TrimPrefix(srv.Headers["Authorization"], "Bearer "))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		return err
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		return err
	}
	for _, t := range tools {
		rep.Tools = append(rep.Tools, t.Name)
	}
	if cfg.Call == "" {
		return nil
	}
	res, err := c.CallTool(ctx, cfg.Call, cfg.Args)
	if err != nil {
		return err
	}
	rep.Result = res.Text()
	return nil
}
