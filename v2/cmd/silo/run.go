package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

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
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo run [--prompt text] <Silofile|dir>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	f, err := silofile.Load(fs.Arg(0))
	if err != nil {
		var errs silofile.Errors
		if errors.As(err, &errs) {
			fmt.Fprintln(stderr, errs)
		} else {
			fmt.Fprintf(stderr, "silo run: %v\n", err)
		}
		return exitInvalid
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
		fmt.Fprintln(stderr, errs)
		return exitInvalid
	}
	opts.File, opts.Prompt = f, *prompt
	opts.Stdin, opts.Stdout, opts.Stderr = stdin, stdout, stderr
	code, err := session.Run(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(stderr, "silo run: %v\n", err)
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
