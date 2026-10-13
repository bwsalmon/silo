package nsd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RuntimeDir is $XDG_RUNTIME_DIR/silo, where nsd.sock and nsd.state
// live.
func RuntimeDir() (string, error) {
	d := os.Getenv("XDG_RUNTIME_DIR")
	if d == "" {
		return "", errors.New("nsd: XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(d, "silo"), nil
}

// DefaultPinDir is where containers' namespaces are pinned: in the
// holder's private /run, which a container's root can traverse (runsc
// opens its netns pin as that root), unlike $XDG_RUNTIME_DIR.
const DefaultPinDir = "/run/silo/netns"

// Main is `silod nsd`, run by silo-nsd.service: it checks the host's
// prerequisites, starts the holder in fresh namespaces (re-running this
// binary with the same arguments) and waits for it. args are the
// command's flags; exe is this binary.
func Main(ctx context.Context, exe string, args []string) error {
	rt, _ := RuntimeDir()
	fs := flag.NewFlagSet("nsd", flag.ContinueOnError)
	socket := fs.String("socket", filepath.Join(rt, "nsd.sock"), "the RPC socket")
	state := fs.String("state", filepath.Join(rt, "nsd.state"), "the holder's state file")
	pins := fs.String("pins", DefaultPinDir, "where containers' namespaces are pinned, in the holder's mount namespace")
	silos := fs.String("silos", "", "the data directory's silos/, served by the dir storage driver")
	bridge := fs.String("bridge", "gw0", "the gateway bridge in silo-net")
	addrs := fs.String("addr", "", "gw0's addresses, comma-separated prefixes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if rt == "" && (*socket == "" || *state == "" || *pins == "") {
		return errors.New("nsd: XDG_RUNTIME_DIR is not set; pass --socket, --state and --pins")
	}
	cfg := Config{Socket: *socket, State: *state, PinDir: *pins, Silos: *silos, Bridge: *bridge}
	if *addrs != "" {
		for _, a := range strings.Split(*addrs, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(a))
			if err != nil {
				return fmt.Errorf("--addr: %w", err)
			}
			cfg.Addrs = append(cfg.Addrs, p)
		}
	}
	if os.Getenv("_SILO_NSD_INNER") != "" {
		cfg.Adopt = os.Getenv("_SILO_NSD_ADOPT") != ""
		return Run(ctx, cfg)
	}
	if failed := Failed(Probe()); len(failed) > 0 {
		var msgs []string
		for _, c := range failed {
			msgs = append(msgs, fmt.Sprintf("%s: %s (fix: %s)", c.Name, c.Detail, c.Fix))
		}
		return fmt.Errorf("nsd: this host can't run the holder:\n  %s", strings.Join(msgs, "\n  "))
	}
	m, err := hostMapping()
	if err != nil {
		return err
	}
	cmd, err := startOrAdopt(exe, append([]string{"nsd"}, args...), cfg, m)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cmd.Process.Signal(os.Interrupt)
		return <-done
	}
}

// startOrAdopt starts the holder: inside the namespaces the previous
// holder left, if anything is still in them (a restart of silo-nsd,
// with containerd and its shims still running), and in fresh ones
// otherwise (the first start, or after a reboot) (call L33).
func startOrAdopt(exe string, args []string, cfg Config, m Mapping) (*exec.Cmd, error) {
	env := append(os.Environ(), "_SILO_NSD_INNER=1")
	if b, err := os.ReadFile(cfg.State); err == nil {
		var st State
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("nsd: %s: %w", cfg.State, err)
		}
		cmd, pid, err := Adopt(exe, args, env, st, cfg.Socket, m, os.Stdout, os.Stderr)
		if err == nil {
			log.Printf("nsd: adopting the namespaces of the previous holder (%d) through process %d", st.Pid, pid)
			return cmd, nil
		}
		if !errors.Is(err, ErrNothingToAdopt) {
			return nil, err
		}
		log.Printf("nsd: nothing is left in the previous holder's namespaces; making fresh ones")
	}
	return Start(exe, args, env, m, os.Stdout, os.Stderr)
}

// hostMapping is HolderMapping for the current user from /etc/subuid
// and /etc/subgid.
func hostMapping() (Mapping, error) {
	user := currentUser()
	read := func(file string, id int) (SubID, error) {
		f, err := os.Open(file)
		if err != nil {
			return SubID{}, err
		}
		defer f.Close()
		s, ok, err := ParseSubID(f, user, id)
		if err == nil && !ok {
			err = fmt.Errorf("nsd: no range for %s in %s", user, file)
		}
		return s, err
	}
	su, err1 := read("/etc/subuid", os.Getuid())
	sg, err2 := read("/etc/subgid", os.Getgid())
	if err := errors.Join(err1, err2); err != nil {
		return Mapping{}, err
	}
	return HolderMapping(os.Getuid(), os.Getgid(), su, sg), nil
}
