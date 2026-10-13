package nsd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// Start runs exe with args as the holder: the nsexec constructor
// unshares a user namespace before the Go runtime starts, Start maps it
// with newuidmap and newgidmap (root inside is the caller, 1 up is the
// subordinate range), and the child then unshares its private mount
// namespace and silo-net. The returned command is running; its program
// is expected to call Run.
func Start(exe string, args, env []string, m Mapping, stdout, stderr io.Writer) (*exec.Cmd, error) {
	if !nsexec.Available {
		return nil, errors.New("nsd: this binary was built without cgo, which the holder needs")
	}
	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent, child := os.NewFile(uintptr(sp[0]), "sync"), os.NewFile(uintptr(sp[1]), "sync")
	defer parent.Close()
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append([]string{}, env...), "_SILO_NSEXEC=create", "_SILO_NSEXEC_SYNC=3")
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Start()
	child.Close()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*exec.Cmd, error) {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(parent, b); err != nil || b[0] != 'u' {
		return fail(fmt.Errorf("nsd: the holder did not create its user namespace (%v)", err))
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	for _, t := range []struct {
		tool string
		rs   []Range
	}{{"newuidmap", m.UID}, {"newgidmap", m.GID}} {
		argv := []string{pid}
		for _, r := range t.rs {
			argv = append(argv, strconv.FormatUint(uint64(r.Inside), 10), strconv.FormatUint(uint64(r.Outside), 10), strconv.FormatUint(uint64(r.Size), 10))
		}
		if out, err := exec.Command(t.tool, argv...).CombinedOutput(); err != nil {
			return fail(fmt.Errorf("nsd: %s %v: %v: %s", t.tool, argv, err, out))
		}
	}
	if _, err := parent.Write([]byte{'m'}); err != nil {
		return fail(err)
	}
	return cmd, nil
}

// Config is what the holder needs to run.
type Config struct {
	// Socket is nsd.sock, normally $XDG_RUNTIME_DIR/silo/nsd.sock.
	Socket string
	// State is nsd.state, where silo unshare and a restarted holder
	// find this one's pid.
	State string
	// PinDir is where containers' namespaces are pinned. The holder
	// mounts a tmpfs over it in its private mount namespace, so the
	// pins are invisible, and harmless, outside. runsc, started inside
	// a container's user namespace, opens the netns pin as a subuid:
	// for it, every ancestor of PinDir must be searchable by others.
	PinDir string
	// Silos is the data directory's silos/, kept by the dir driver.
	// Empty serves no storage.
	Silos string
	// Bridge and Addrs are gw0 and its addresses in silo-net.
	Bridge string
	Addrs  []netip.Prefix
	// AllowUID is the peer uid served, in the holder's namespace: 0,
	// silod's user.
	AllowUID uint32
}

// State is what nsd.state holds.
type State struct {
	Pid int `json:"pid"`
}

// Run is the holder, inside the namespaces Start made: it pins
// containers' namespaces on a private tmpfs, brings up silo-net's
// loopback and gw0, and serves the RPC until ctx is done.
func Run(ctx context.Context, cfg Config) error {
	m, err := ReadMapping(os.Getpid())
	if err != nil {
		return err
	}
	if len(m.UID) == 0 || m.UID[0].Inside != 0 || os.Getuid() != 0 {
		return errors.New("nsd: Run must be started by Start, as root of its own user namespace")
	}
	if err := os.MkdirAll(cfg.PinDir, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", cfg.PinDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("nsd: tmpfs on %s: %w", cfg.PinDir, err)
	}
	if cfg.Silos != "" {
		if err := os.MkdirAll(cfg.Silos, 0o700); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	s := &Server{AllowUID: cfg.AllowUID, Mapping: m, PinDir: cfg.PinDir, Self: self}
	if cfg.Silos != "" {
		s.Storage = &storage.Dir{Root: cfg.Silos}
	}
	if err := s.withRtnl(func(r *rtnl) error { return r.up("lo") }); err != nil {
		return err
	}
	if cfg.Bridge != "" {
		if err := s.bridge(cfg.Bridge, cfg.Addrs); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return err
	}
	os.Remove(cfg.Socket)
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: cfg.Socket, Net: "unixpacket"})
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Socket)
	if cfg.State != "" {
		b, _ := json.Marshal(State{Pid: os.Getpid()})
		if err := os.WriteFile(cfg.State, b, 0o600); err != nil {
			return err
		}
	}
	return s.Serve(ctx, l)
}

// ReadState reads nsd.state and checks that its pid is still a holder:
// a process in another user namespace than the caller's.
func ReadState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if err != nil {
		return st, fmt.Errorf("nsd: no holder (%w); is silo-nsd running?", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("nsd: %s: %w", path, err)
	}
	theirs, err1 := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", st.Pid))
	ours, err2 := os.Readlink("/proc/self/ns/user")
	if err := errors.Join(err1, err2); err != nil {
		return st, fmt.Errorf("nsd: holder %d is gone: %w", st.Pid, err)
	}
	if theirs == ours {
		return st, fmt.Errorf("nsd: process %d in %s is not a holder", st.Pid, path)
	}
	return st, nil
}

// JoinCommand returns a command that runs exe inside the holder's
// user, mount and network namespaces as their root: the nsexec
// constructor joins them before exe's Go runtime starts. exe must
// carry package nsexec.
func JoinCommand(holderPid int, exe string, args ...string) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "_SILO_NSEXEC=join", "_SILO_NSEXEC_PID="+strconv.Itoa(holderPid))
	return cmd
}
