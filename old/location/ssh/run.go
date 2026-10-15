package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bwsalmon/silo/old/internal/procgroup"
)

// result is what one remote command did.
type result struct {
	Stdout, Stderr string
	// Code is the remote command's exit code, or 255 for ssh's own
	// errors.
	Code int
}

func (r result) err(what string) error {
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	return fmt.Errorf("%s: exit %d: %s", what, r.Code, msg)
}

// runner runs one command line on the host, with stdin.
type runner func(ctx context.Context, remote string, stdin []byte) (result, error)

// run runs argv to completion. A non-zero exit is a result, not an
// error; the error is for a command that could not be run at all.
func run(ctx context.Context, argv []string, stdin []byte) (result, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = bytes.NewReader(stdin)
	procgroup.DieWithParent(cmd)
	err := cmd.Run()
	r := result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		r.Code = ee.ExitCode()
	default:
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		return r, err
	}
	return r, nil
}

// direct runs commands on their own connection, for silo gc --host and
// the dry-run checks, which have no master. BatchMode keeps a check from
// prompting.
func direct(t Transport, batch bool) runner {
	return func(ctx context.Context, remote string, stdin []byte) (result, error) {
		flags := []string{"-T"}
		if batch {
			flags = append(flags, "-o", "BatchMode=yes")
		}
		return run(ctx, t.Argv(flags, remote), stdin)
	}
}
