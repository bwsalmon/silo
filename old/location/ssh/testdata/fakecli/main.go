// Command fakecli is a fake claude for the ssh location's tests, built
// statically and installed as /usr/local/bin/claude in the test sshd's
// image. It answers --version and `auth status` like Claude Code (logged
// in when $HOME/.fake-login exists), reads the staged MCP config, calls
// the gateway through the reverse forward, writes what it saw to
// $HOME/report.json, and then does what $HOME/fake.json says.
package main

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

	"github.com/bwsalmon/silo/old/mcp"
)

type config struct {
	Call  string         `json:"call,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	Exit  int            `json:"exit"`
	Wait  bool           `json:"wait,omitempty"`
	Sleep int            `json:"sleep,omitempty"` // seconds, before exiting
}

type report struct {
	PID    int      `json:"pid"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	TTY    bool     `json:"tty"`
	URL    string   `json:"url"`
	Tools  []string `json:"tools"`
	Result string   `json:"result,omitempty"`
	Error  string   `json:"error,omitempty"`
	Signal string   `json:"signal,omitempty"`
}

func main() {
	args := os.Args[1:]
	home := os.Getenv("HOME")
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.197 (Claude Code)")
		return
	}
	if len(args) == 2 && args[0] == "auth" && args[1] == "status" {
		if _, err := os.Stat(filepath.Join(home, ".fake-login")); err != nil {
			fmt.Println("Not logged in")
			os.Exit(1)
		}
		fmt.Println("Logged in")
		return
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	var cfg config
	if b, err := os.ReadFile(filepath.Join(home, "fake.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	rep := report{PID: os.Getpid(), Argv: os.Args, Env: os.Environ()}
	rep.Cwd, _ = os.Getwd()
	if st, _ := os.Stdin.Stat(); st != nil {
		rep.TTY = st.Mode()&os.ModeCharDevice != 0
		if !rep.TTY {
			b := make([]byte, 1<<16)
			n, _ := os.Stdin.Read(b)
			rep.Stdin = string(b[:n])
		}
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
	if rep.TTY {
		fmt.Println("fake claude on a tty")
	}
	if cfg.Sleep > 0 {
		time.Sleep(time.Duration(cfg.Sleep) * time.Second)
	}
	if !cfg.Wait {
		os.Exit(cfg.Exit)
	}
	sig := <-sigs
	rep.Signal = sig.String()
	write()
	os.Exit(128 + int(sig.(syscall.Signal)))
}

func callGateway(args []string, cfg config, rep *report) error {
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
	srv := conf.MCPServers["silo"]
	rep.URL = srv.URL
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
