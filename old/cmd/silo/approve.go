package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/old/control"
	"github.com/bwsalmon/silo/old/session"
)

const approveUsage = "usage: silo approve <session> [--deny] [<call-id>]"

// approve is silo approve (design §12.3): it answers a local script's
// call waiting for approval, on the session's control socket. The
// session may be named by a unique prefix of its id; the call may be
// left out when only one is waiting.
func approve(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	deny := false
	var pos []string
	for _, a := range args {
		switch a {
		case "--deny", "-deny":
			deny = true
		case "-h", "-help", "--help":
			fmt.Fprintln(stdout, approveUsage)
			return exitOK
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(stderr, "silo approve: unknown flag %s\n%s\n", a, approveUsage)
				return exitUsage
			}
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		fmt.Fprintln(stderr, approveUsage)
		return exitUsage
	}
	id, err := findSession(getenv, pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "silo approve: %v\n", err)
		return 1
	}
	p := session.ApproveParams{Deny: deny}
	if len(pos) == 2 {
		p.ID = pos[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res session.ApproveResult
	if err := control.Call(ctx, session.ControlSocket(getenv, id), "approve", p, &res); err != nil {
		fmt.Fprintf(stderr, "silo approve: session %s: %v\n", id, err)
		return 1
	}
	verb := "approved"
	if !res.Approved {
		verb = "denied"
	}
	fmt.Fprintf(stdout, "%s call %s: %s %s\n", verb, res.ID, res.Tool, res.Summary())
	return exitOK
}

// findSession resolves a session id, or a unique prefix of one, among
// the sessions running under this user.
func findSession(getenv func(string) string, name string) (string, error) {
	entries, err := os.ReadDir(session.RuntimeRoot(getenv))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var matches []string
	for _, e := range entries {
		if !session.ValidID(e.Name()) {
			continue
		}
		if e.Name() == name {
			return name, nil
		}
		if strings.HasPrefix(e.Name(), name) {
			matches = append(matches, e.Name())
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no running session is named %s", strconv.Quote(name))
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%s names %d sessions: %s", strconv.Quote(name), len(matches), strings.Join(matches, ", "))
}
