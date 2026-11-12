// Command silo runs an agent CLI (Claude Code, Codex or Gemini CLI)
// against a context repository, with only the MCP tools silo grants.
//
//	silo run [--config FILE] [--branch BRANCH] <repo>
//	silo mcp-bridge <socket>     (started by the agent CLI, not by people)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/session"
)

const usage = `usage:
  silo run [--config FILE] [--branch BRANCH] <repo>
      Check out <repo> (relative to the configured repos root) and start an
      interactive session with the configured agent CLI.
  silo mcp-bridge <socket>
      Connect stdio to a session's MCP router. The agent CLI runs this.

The config file is --config, else $SILO_CONFIG, else ~/.silo/config.yaml.
See docs/config.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "mcp-bridge":
		err = bridgeCmd(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "silo: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "silo:", err)
		os.Exit(1)
	}
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file")
	branch := fs.String("branch", "", "base branch (default: the repository's default branch)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("run takes one repository name, e.g. silo run team/payments-context")
	}
	path := firstNonEmpty(*cfgPath, os.Getenv("SILO_CONFIG"), config.DefaultPath())
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	// Ctrl-C belongs to the CLI; silo itself goes when the CLI does.
	signal.Ignore(syscall.SIGINT)
	return session.Run(context.Background(), session.Options{Config: cfg, Repo: fs.Arg(0), Branch: *branch})
}

// bridgeCmd relays the CLI's stdio MCP connection to the session's router.
func bridgeCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("mcp-bridge takes the router's socket path")
	}
	return session.Bridge(args[0], os.Stdin, os.Stdout)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
