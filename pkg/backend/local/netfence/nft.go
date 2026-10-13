package netfence

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// NFT loads a ruleset into silo-net and lists what is there.
type NFT interface {
	// Load replaces silo's tables with rs, as one transaction.
	Load(ctx context.Context, rs *Ruleset) error
	// List is silo's tables as the kernel has them, in a form that two
	// listings of the same tables give alike.
	List(ctx context.Context) (string, error)
}

// Exec is NFT through the nft command.
type Exec struct {
	// Command makes the command that runs nft with args in silo-net.
	Command func(ctx context.Context, args ...string) (*exec.Cmd, error)
}

// Nsenter runs nft (a path, or "nft") in the namespace holder's user
// and network namespaces: silod's user owns the holder's user
// namespace, so it may join it, and is root in silo-net there. holder
// is the holder's pid, read at each call, since a restarted holder has
// a new one.
func Nsenter(holder func() (int, error), nft string) Exec {
	if nft == "" {
		nft = "nft"
	}
	return Exec{Command: func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		pid, err := holder()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, "nsenter", append([]string{"-t", strconv.Itoa(pid), "-U", "-n", "--", nft}, args...)...), nil
	}}
}

func (e Exec) run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd, err := e.Command(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("netfence: nft %s: %w", strings.Join(args, " "), err)
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("netfence: nft %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Load implements NFT with `nft -f -`.
func (e Exec) Load(ctx context.Context, rs *Ruleset) error {
	_, err := e.run(ctx, rs.Render(), "-f", "-")
	return err
}

// List implements NFT with `nft list table`, for each of silo's tables.
// A missing table lists as nothing.
func (e Exec) List(ctx context.Context) (string, error) {
	out, err := e.run(ctx, "", "list", "tables")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, family := range []string{"inet", "bridge"} {
		if !strings.Contains(out, "table "+family+" "+TableName+"\n") {
			continue
		}
		t, err := e.run(ctx, "", "list", "table", family, TableName)
		if err != nil {
			return "", err
		}
		b.WriteString(t)
	}
	return b.String(), nil
}

// Fake is an NFT in memory, for tests: it keeps the ruleset last
// loaded, which tests can evaluate, and lists it rendered.
type Fake struct {
	mu     sync.Mutex
	loaded *Ruleset
	extra  string
	loads  int
	err    error
}

// Fail makes every Load fail with err, until Fail(nil).
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Load implements NFT.
func (f *Fake) Load(_ context.Context, rs *Ruleset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.loaded, f.extra = rs, ""
	f.loads++
	return nil
}

// List implements NFT.
func (f *Fake) List(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded == nil {
		return f.extra, nil
	}
	return f.loaded.Render() + f.extra, nil
}

// Loaded is the ruleset last loaded, or nil.
func (f *Fake) Loaded() *Ruleset {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}

// Loads is how many times Load succeeded.
func (f *Fake) Loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// Tamper changes the tables behind silod's back, as someone running nft
// by hand would: the next List differs from what was loaded.
func (f *Fake) Tamper(rule string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extra += rule + "\n"
}
