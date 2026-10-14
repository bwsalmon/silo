// Command silo is Silo v2's command line (design §3, §16). So far it has
// run, approve, gc, validate and schema.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bwsalmon/silo/v2/localexec"
	"github.com/bwsalmon/silo/v2/session"
	"github.com/bwsalmon/silo/v2/silofile"
)

// Exit codes silo owns (design §3.3).
const (
	exitOK      = 0
	exitUsage   = 64
	exitInvalid = 65
)

const usage = `usage: silo <command> [arguments]

commands:
  run [--json] [--prompt text] <Silofile|dir>
                            run one session (design §3)
  gc                        remove what crashed sessions left behind
  approve <session> [--deny] [<call-id>]
                            answer a local script's call waiting for approval
  validate <Silofile|dir>   check a Silofile or silo folder (design §2.4)
  schema [section]          print the Silofile's JSON Schema, or one section's
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runSession(args[1:], os.Stdin, stdout, stderr, session.Options{})
	case "gc":
		return gc(args[1:], stdout, stderr)
	case "approve":
		return approve(args[1:], os.Getenv, stdout, stderr)
	case "validate":
		return validate(args[1:], stdout, stderr)
	case "schema":
		return schema(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "silo: unknown command %q\n%s", args[0], usage)
	return exitUsage
}

// flags parses a command's flags, which are none so far, and returns its
// arguments, or false after reporting a usage error.
func flags(name, argsUsage string, args []string, stderr io.Writer, min, max int) ([]string, bool) {
	fs := flag.NewFlagSet("silo "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, "usage: silo %s %s\n", name, argsUsage) }
	if err := fs.Parse(args); err != nil {
		return nil, false
	}
	if fs.NArg() < min || fs.NArg() > max {
		fs.Usage()
		return nil, false
	}
	return fs.Args(), true
}

// validateOptions are silo validate's and silo run's: with the local
// executor's script checks and warnings (design §2.4, §12.4).
var validateOptions = silofile.Options{CheckScripts: localexec.Check}

func validate(args []string, stdout, stderr io.Writer) int {
	args, ok := flags("validate", "<Silofile|dir>", args, stderr, 1, 1)
	if !ok {
		return exitUsage
	}
	f, err := silofile.Load(args[0])
	if err != nil {
		var errs silofile.Errors
		if errors.As(err, &errs) {
			fmt.Fprintln(stderr, errs)
		} else {
			fmt.Fprintf(stderr, "silo validate: %v\n", err)
		}
		return exitInvalid
	}
	res := f.Validate(validateOptions)
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "warning: %v\n", w)
	}
	if len(res.Errors) > 0 {
		fmt.Fprintln(stderr, res.Errors)
		return exitInvalid
	}
	fmt.Fprintf(stdout, "%s: valid\n", args[0])
	return exitOK
}

func schema(args []string, stdout, stderr io.Writer) int {
	args, ok := flags("schema", "[section]", args, stderr, 0, 1)
	if !ok {
		return exitUsage
	}
	if len(args) == 0 {
		stdout.Write(silofile.Schema())
		return exitOK
	}
	b, err := silofile.SectionSchema(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "silo schema: %v\nsections: %s\n", err, strings.Join(silofile.Sections(), ", "))
		return exitUsage
	}
	stdout.Write(b)
	return exitOK
}
