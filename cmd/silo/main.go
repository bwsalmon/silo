// Command silo runs an agent CLI (Claude Code, Codex or Gemini CLI)
// against a context repository, with only the MCP tools silo grants.
//
//	silo run [--config FILE] [--branch BRANCH] <repo>
//	silo vendor [--config FILE] [--into DIR] <repo>[@<ref>] [--toolsets a,b] --as <alias>
//	silo vendor [--config FILE] [--into DIR] --update <alias> [@<ref>]
//	silo mcp-bridge <socket>     (started by the agent CLI, not by people)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/session"
	"github.com/bwsalmon/silo/internal/vendor"
)

const usage = `usage:
  silo run [--config FILE] [--branch BRANCH] <repo>
      Check out <repo> (relative to the configured repos root) and start an
      interactive session with the configured agent CLI.
  silo vendor [--config FILE] [--into DIR] <repo>[@<ref>] [--toolsets a,b] --as <alias>
      Copy tool sets from <repo> (relative to the configured repos root) at
      <ref> (default: its default branch) into this context repository's
      .silo/tools/vendor/<alias>/, with a VENDOR.lock recording the commit.
      --toolsets picks sets (default: all). --into is the context
      repository (default: the git repository around the current
      directory). Review and commit the result yourself.
  silo vendor [--config FILE] [--into DIR] --update <alias> [@<ref>]
      Re-copy an alias's tool sets from its VENDOR.lock's repository, at
      <ref> if given, else the locked ref.
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
	case "vendor":
		err = vendorCmd(os.Args[2:])
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

func vendorCmd(args []string) error {
	fs := flag.NewFlagSet("vendor", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file")
	into := fs.String("into", "", "context repository to vendor into (default: the one around the current directory)")
	toolsets := fs.String("toolsets", "", "comma-separated tool sets to vendor (default: all)")
	alias := fs.String("as", "", "alias the sets are namespaced under: <alias>.<set>.<tool>")
	update := fs.String("update", "", "re-vendor this alias from its VENDOR.lock")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	var repo, ref string
	switch {
	case *update != "":
		if *alias != "" {
			return errors.New("--update takes the alias itself; drop --as")
		}
		*alias, ref = vendor.ParseSpec(*update)
		if len(pos) > 1 {
			return errors.New("--update takes at most one @<ref>")
		}
		if len(pos) == 1 {
			if !strings.HasPrefix(pos[0], "@") || ref != "" {
				return fmt.Errorf("unexpected %q; give the new ref as @<ref>", pos[0])
			}
			ref = strings.TrimPrefix(pos[0], "@")
		}
	default:
		if len(pos) != 1 || *alias == "" {
			return errors.New("usage: silo vendor <repo>[@<ref>] [--toolsets a,b] --as <alias>, or silo vendor --update <alias> [@<ref>]")
		}
		repo, ref = vendor.ParseSpec(pos[0])
	}
	var sets []string
	for _, s := range strings.Split(*toolsets, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sets = append(sets, s)
		}
	}
	cfg, err := config.Load(firstNonEmpty(*cfgPath, os.Getenv("SILO_CONFIG"), config.DefaultPath()))
	if err != nil {
		return err
	}
	ctx := context.Background()
	checkout, err := repoRoot(ctx, *into)
	if err != nil {
		return err
	}
	req, err := vendor.NewRequest(checkout, repo, ref, *alias, sets, *update != "")
	if err != nil {
		return err
	}
	up, err := gitrepo.Open(ctx, cfg, req.Repo)
	if err != nil {
		return err
	}
	plan, err := vendor.Build(ctx, up, req)
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		fmt.Fprintln(os.Stderr, "silo: warning:", w)
	}
	if err := vendor.Apply(checkout, plan); err != nil {
		return err
	}
	fmt.Printf("Vendored %s into %s/.\n", plan.Summary(), vendor.Dir(plan.Alias))
	fmt.Printf("Review it (git -C %s add -A %s && git -C %s diff --cached), then commit it: sessions load committed tool sets.\n", checkout, vendor.Dir(plan.Alias), checkout)
	return nil
}

// repoRoot is the top of the git repository around dir (default: the
// current directory).
func repoRoot(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	out, err := (&gitrepo.Repo{Dir: dir}).Git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository; run silo vendor in a context repository, or pass --into", dir)
	}
	return strings.TrimSpace(out), nil
}

// parseInterspersed parses flags that may come before or after the
// positional arguments, which it returns.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
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
