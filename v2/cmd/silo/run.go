package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bwsalmon/silo/v2/gcp"
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
	res := f.Validate(silofile.Options{})
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

// gc is silo gc: the local part (design §3.1), then the user's
// journey 2 accounts whose ttl has passed (design §11.5). GCP is only
// contacted when local state lists an account or --project is given.
func gc(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("silo gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "also look for the user's silo accounts in this project")
	account := fs.String("account", "", "with --project: delete this one account of the user's, whatever its ttl")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: silo gc [--project p [--account email]]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*account != "" && *project == "") {
		if err == nil {
			fs.Usage()
		}
		return exitUsage
	}
	code := exitOK
	if *account == "" {
		rep, err := session.GC(context.Background(), session.GCOptions{Getenv: os.Getenv})
		for _, line := range rep.Removed {
			fmt.Fprintln(stdout, line)
		}
		if err != nil {
			fmt.Fprintf(stderr, "silo gc: %v\n", err)
			code = 1
		}
	}
	if err := gcAccounts(context.Background(), *project, *account, stdout); err != nil {
		fmt.Fprintf(stderr, "silo gc: %v\n", err)
		code = 1
	}
	return code
}

// gcAccounts is silo gc for journey 2's accounts, as the user's own
// gcloud login.
func gcAccounts(ctx context.Context, project, account string, stdout io.Writer) error {
	store := &gcp.Store{Path: gcp.StatePath(os.Getenv)}
	st, err := store.Load()
	if err != nil {
		return err
	}
	if len(st.Accounts) == 0 && project == "" {
		return nil
	}
	api, user, err := gcp.Ambient(ctx)
	if err != nil {
		return err
	}
	c := &gcp.Client{API: api, Store: store, Out: stdout}
	opts := gcp.GCOptions{User: user, Account: account}
	if project != "" {
		opts.Projects = []string{project}
	}
	_, err = c.GC(ctx, opts)
	return err
}
