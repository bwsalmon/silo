package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/events"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/session"
	"github.com/bwsalmon/silo/v2/silofile"
)

// runSession is silo run (design §3). stdin is the terminal in
// interactive mode.
func runSession(args []string, stdin io.Reader, stdout, stderr io.Writer, opts session.Options) int {
	fs := flag.NewFlagSet("silo run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prompt := fs.String("prompt", "", "the prompt, replacing agent.prompt")
	dryRun := fs.Bool("dry-run", false, "check what the session needs, changing nothing, and print a checklist")
	jsonOut := fs.Bool("json", false, "write the session's events as JSON lines on stdout (docs/events.md); mode headless only")
	callers := &callerFlags{getenv: func(k string) string { return agentcliGetenv(opts.Environ, k) }}
	fs.Func("caller-mcp", "a caller-provided MCP server, `name=url`: an http(s) URL, or unix:<path> for a Unix socket; its token is read from $"+callerTokenPrefix+"<NAME> (repeatable)", callers.server)
	fs.Func("caller-mcp-tools", "the allowlist of caller server name's tools, `name=tool,...` (default: every tool it lists)", callers.tools)
	fs.Func("caller-mcp-blocking", "caller server name's tools that may block with no gateway timeout, `name=tool,...`", callers.blocking)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo run [--dry-run] [--json] [--prompt text] [--caller-mcp name=url ...] <Silofile|dir>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	callerMCP, err := callers.list()
	if err != nil {
		fmt.Fprintf(stderr, "silo run: %v\n", err)
		return exitUsage
	}
	// Under --json every exit, even before a session exists, ends
	// with a result event.
	fail := func(code int, msg string) int {
		fmt.Fprintln(stderr, msg)
		if *jsonOut {
			session.JSONSink{W: stdout}.Event(events.Event{
				V: events.Version, Seq: 1, Time: time.Now().UTC(), Type: events.TypeResult,
				Result: &events.Result{Exit: events.Exit{Code: code, Reason: events.ReasonSetupFailed, Detail: msg}},
			})
		}
		return code
	}
	f, err := silofile.Load(fs.Arg(0))
	if err != nil {
		var errs silofile.Errors
		if errors.As(err, &errs) {
			return fail(exitInvalid, errs.Error())
		}
		return fail(exitInvalid, fmt.Sprintf("silo run: %v", err))
	}
	res := f.Validate(validateOptions)
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "warning: %v\n", w)
	}
	var errs silofile.Errors
	for _, e := range res.Errors {
		// --prompt supplies what a headless Silofile without one lacks.
		if *prompt != "" && e.Path == "agent" && e.Msg == "mode headless needs a prompt" {
			continue
		}
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		return fail(exitInvalid, errs.Error())
	}
	if *dryRun {
		return dryRunChecks(f, stdout, opts.Environ)
	}
	opts.File, opts.Prompt, opts.CallerMCP = f, *prompt, callerMCP
	opts.Stdin, opts.Stdout, opts.Stderr = stdin, stdout, stderr
	if *jsonOut {
		if f.Agent.Mode != "headless" {
			return fail(exitUsage, "silo run: --json needs agent.mode: headless; the events and an interactive CLI cannot share stdout")
		}
		// Stdout carries the events alone; the CLI's own output is kept
		// in the session's raw transcript.
		opts.Sink, opts.Stdout = session.JSONSink{W: stdout}, io.Discard
	}
	code, err := session.Run(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "silo run: %v\n", err)
	}
	return code
}

// dryRunChecks is silo run --dry-run after validation (design §2.4):
// one line per check, and exit 69 if any failed.
func dryRunChecks(f *silofile.Silofile, stdout io.Writer, environ []string) int {
	fmt.Fprintf(stdout, "ok    silofile: %s is valid\n", f.File)
	code := exitOK
	for _, c := range session.DryRun(context.Background(), f, environ) {
		mark := "ok  "
		if !c.OK {
			mark, code = "FAIL", session.ExitPrerequisite
		}
		fmt.Fprintf(stdout, "%s  %s: %s\n", mark, c.Name, c.Detail)
	}
	return code
}

// gc is silo gc: its local part (design §3.1), and with --host, the
// stale remote session directories this machine left on that host
// (design §12.6).
func gc(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("silo gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "also remove stale remote session directories on this ssh host")
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: silo gc [--host <host>]") }
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	code := exitOK
	rep, err := session.GC(context.Background(), session.GCOptions{Getenv: os.Getenv})
	for _, line := range rep.Removed {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "silo gc: %v\n", err)
		code = 1
	}
	if *host != "" {
		t := session.SSHTransport(&silofile.SSH{Host: *host}, os.Getenv)
		removed, err := sshloc.GCHost(context.Background(), t, sshloc.GCOptions{Live: session.Live(os.Getenv)})
		for _, d := range removed {
			fmt.Fprintf(stdout, "removed stale remote session directory %s:%s\n", *host, d)
		}
		if err != nil {
			fmt.Fprintf(stderr, "silo gc --host %s: %v\n", *host, err)
			code = 1
		}
	}
	return code
}

// callerTokenPrefix names the environment variable a caller server's
// token is read from, so that it is never in silo's argv.
const callerTokenPrefix = "SILO_CALLER_MCP_TOKEN_"

// callerFlags collects --caller-mcp and the flags that qualify it.
type callerFlags struct {
	getenv   func(string) string
	order    []string
	servers  map[string]*session.CallerMCP
	tool     map[string][]string
	blocks   map[string][]string
	hasTools map[string]bool
}

func splitFlag(v string) (string, string, error) {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return "", "", fmt.Errorf("want name=value, not %q", v)
	}
	return name, rest, nil
}

func (c *callerFlags) server(v string) error {
	name, url, err := splitFlag(v)
	if err != nil {
		return err
	}
	if c.servers == nil {
		c.servers = map[string]*session.CallerMCP{}
	}
	if _, ok := c.servers[name]; ok {
		return fmt.Errorf("caller MCP server %s is given twice", name)
	}
	c.order = append(c.order, name)
	c.servers[name] = &session.CallerMCP{Name: name, URL: url, Token: c.getenv(callerTokenVar(name))}
	return nil
}

func (c *callerFlags) tools(v string) error {
	name, list, err := splitFlag(v)
	if err != nil {
		return err
	}
	if c.tool == nil {
		c.tool, c.hasTools = map[string][]string{}, map[string]bool{}
	}
	c.tool[name] = append(c.tool[name], strings.Split(list, ",")...)
	c.hasTools[name] = true
	return nil
}

func (c *callerFlags) blocking(v string) error {
	name, list, err := splitFlag(v)
	if err != nil {
		return err
	}
	if c.blocks == nil {
		c.blocks = map[string][]string{}
	}
	c.blocks[name] = append(c.blocks[name], strings.Split(list, ",")...)
	return nil
}

// list returns the servers in the order given.
func (c *callerFlags) list() ([]session.CallerMCP, error) {
	for _, m := range []map[string][]string{c.tool, c.blocks} {
		for name := range m {
			if _, ok := c.servers[name]; !ok {
				return nil, fmt.Errorf("--caller-mcp-tools and --caller-mcp-blocking name %s, which no --caller-mcp gives", name)
			}
		}
	}
	var out []session.CallerMCP
	for _, name := range c.order {
		m := *c.servers[name]
		if c.hasTools[name] {
			m.Tools = append([]string{}, c.tool[name]...)
		}
		m.Blocking = c.blocks[name]
		out = append(out, m)
	}
	return out, nil
}

// callerTokenVar is the environment variable caller server name's token
// is read from: SILO_CALLER_MCP_TOKEN_ and the name, upper-cased, with
// - as _.
func callerTokenVar(name string) string {
	return callerTokenPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func agentcliGetenv(environ []string, k string) string {
	if environ == nil {
		return os.Getenv(k)
	}
	return agentcli.Getenv(environ, k)
}
