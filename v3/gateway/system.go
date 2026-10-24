package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// The gateway no longer starts agents itself (they register and dial in
// with `silo connect`, design §8), so the host is reached only for the
// clock and for the socket's group.

// Clock is the gateway's source of time.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Runner runs a command and returns its combined output. It is a variable
// in tests of the real implementations.
type Runner func(ctx context.Context, argv ...string) ([]byte, error)

func execRunner(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv[:min(len(argv), 2)], " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// Groups looks up and creates Unix groups.
type Groups struct {
	Run    Runner
	Lookup func(name string) (*user.Group, error)
}

// NewGroups returns Groups backed by groupadd and the system's group db.
func NewGroups() *Groups { return &Groups{Run: execRunner, Lookup: user.LookupGroup} }

// EnsureGroup returns the gid of group name, creating it as a system group
// if it does not exist.
func (gr *Groups) EnsureGroup(name string) (int, error) {
	g, err := gr.Lookup(name)
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		if _, err := gr.Run(context.Background(), "groupadd", "--system", "--", name); err != nil {
			return 0, err
		}
		g, err = gr.Lookup(name)
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}
