package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bwsalmon/silo/v2/events"
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
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo run [--dry-run] [--json] [--prompt text] <Silofile|dir>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		if err == nil {
			fs.Usage()
		}
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
	opts.File, opts.Prompt = f, *prompt
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

// gc is silo gc: for now, its local part (design §3.1).
func gc(args []string, stdout, stderr io.Writer) int {
	if _, ok := flags("gc", "", args, stderr, 0, 0); !ok {
		return exitUsage
	}
	rep, err := session.GC(context.Background(), session.GCOptions{Getenv: os.Getenv})
	for _, line := range rep.Removed {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "silo gc: %v\n", err)
		return 1
	}
	return exitOK
}
