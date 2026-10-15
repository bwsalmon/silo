package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CgroupFS is where the cgroup v2 hierarchy is mounted, on the host and
// in the holder's mount namespace, which inherits it.
const CgroupFS = "/sys/fs/cgroup"

// SandboxScope is the scope in the user's systemd manager, in
// RootSlice, that holds every sandbox-class container's cgroup:
// silo.slice/silo-sandboxes.scope/<root>/…/<silo>/<container>.
//
// runsc's own cgroup drivers can't make these. It runs as the root of
// its container's user namespace, a subordinate id, so its systemd
// driver dials the system manager and is refused, and its cgroupfs
// driver can write no cgroup its id doesn't own. A cgroup under a
// delegated service (silo-containerd.service) can't hold them either:
// enabling controllers in the service's cgroup for its children leaves
// systemd unable to put the service's next main process there, and
// containers outlive silo-containerd's restarts. So silod asks the
// user manager for a scope with Delegate=yes, whose one process of its
// own, a sleep, sits in a leaf (SandboxKeep) and keeps the scope from
// being collected while no container runs. The scope's cgroup is then
// silod's to manage, as cgroupfs: SandboxTree makes a cgroup per silo
// with the controllers enabled, and `silod runsc create` makes the
// container's, writes its limits (WriteLimits) and starts runsc in it,
// with the shim's options still saying ignore-cgroups.
const SandboxScope = "silo-sandboxes.scope"

// SandboxKeep is the leaf of SandboxScope that holds the scope's own
// process: a cgroup with controllers enabled for its children can hold
// no processes itself.
const SandboxKeep = "keep"

// SandboxTree is silod's handle on SandboxScope's cgroup. Its zero
// value finds or makes the scope on first use.
type SandboxTree struct {
	// Systemctl and SystemdRun run the commands of those names, against
	// the user's manager. Tests replace them.
	Systemctl  func(ctx context.Context, args ...string) ([]byte, error)
	SystemdRun func(args ...string) (*exec.Cmd, error)
	// FS is the cgroup mount, CgroupFS when empty.
	FS string

	mu    sync.Mutex
	scope string // the scope's cgroup, from the mount's root
}

func (t *SandboxTree) fs() string {
	if t.FS != "" {
		return t.FS
	}
	return CgroupFS
}

// managerArgs are the arguments that address the manager silod runs
// under: the system's as root, the user's otherwise.
func managerArgs() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	return []string{"--user"}
}

func (t *SandboxTree) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	if t.Systemctl != nil {
		return t.Systemctl(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "systemctl", append(managerArgs(), args...)...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// startScope starts the scope's sleep: systemd-run --scope execs it in
// place once the scope is made, so the command's process is the sleep.
func (t *SandboxTree) startScope() (*exec.Cmd, error) {
	args := []string{"--scope", "--unit=" + SandboxScope, "--slice=" + RootSlice,
		"--property=Delegate=yes", "--collect", "--quiet", "--", "sleep", "infinity"}
	if t.SystemdRun != nil {
		return t.SystemdRun(args...)
	}
	cmd := exec.Command("systemd-run", append(managerArgs(), args...)...)
	// Its own session, so that nothing sent to silod's reaches it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	return cmd, nil
}

// Path is the cgroup, from the mount's root, of container in the silo
// at the end of siloPath (the silo ids from the root down). It makes
// the scope if there is none, and each silo's cgroup with the
// controllers enabled for its children; the container's own cgroup is
// left to `silod runsc create`, which writes its limits.
func (t *SandboxTree) Path(ctx context.Context, container string, siloPath ...string) (string, error) {
	for _, id := range append([]string{container}, siloPath...) {
		if id == "" || id == "." || id == ".." || strings.ContainsRune(id, '/') || id == SandboxKeep {
			return "", fmt.Errorf("rootless: %q can't name a cgroup", id)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	scope, err := t.ensureScope(ctx)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.fs(), scope)
	for _, id := range siloPath {
		dir = filepath.Join(dir, id)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := enableControllers(dir); err != nil {
			return "", err
		}
	}
	rel, _ := filepath.Rel(t.fs(), filepath.Join(dir, container))
	return "/" + rel, nil
}

// ensureScope returns the scope's cgroup, finding it in the manager or
// starting it, with its own process in SandboxKeep and the controllers
// enabled for the silos' cgroups.
func (t *SandboxTree) ensureScope(ctx context.Context) (string, error) {
	if t.scope != "" {
		if _, err := os.Stat(filepath.Join(t.fs(), t.scope, SandboxKeep)); err == nil {
			return t.scope, nil
		}
		// Collected: no container ran and its sleep was killed.
		t.scope = ""
	}
	scope := t.activeScope(ctx)
	if scope == "" {
		cmd, err := t.startScope()
		if err != nil {
			return "", fmt.Errorf("rootless: starting %s: %w", SandboxScope, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for scope == "" {
			if time.Now().After(deadline) {
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
				return "", fmt.Errorf("rootless: %s didn't start", SandboxScope)
			}
			time.Sleep(50 * time.Millisecond)
			if cmd.ProcessState != nil {
				return "", fmt.Errorf("rootless: systemd-run for %s exited: %v", SandboxScope, cmd.ProcessState)
			}
			scope = t.activeScope(ctx)
		}
	}
	dir := filepath.Join(t.fs(), scope)
	keep := filepath.Join(dir, SandboxKeep)
	if err := os.Mkdir(keep, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	// The scope's process may still be systemd-run, which execs the
	// sleep: moving it moves the sleep it becomes.
	pids, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return "", err
	}
	for _, pid := range strings.Fields(string(pids)) {
		if err := os.WriteFile(filepath.Join(keep, "cgroup.procs"), []byte(pid), 0o644); err != nil && !errors.Is(err, syscall.ESRCH) {
			return "", fmt.Errorf("rootless: moving %s's process %s to %s: %w", SandboxScope, pid, SandboxKeep, err)
		}
	}
	if err := enableControllers(dir); err != nil {
		return "", err
	}
	t.scope = scope
	return scope, nil
}

// activeScope is the scope's cgroup if the manager has it running, or
// "".
func (t *SandboxTree) activeScope(ctx context.Context) string {
	out, err := t.systemctl(ctx, "show", "--property=ActiveState,ControlGroup", SandboxScope)
	if err != nil {
		return ""
	}
	var active bool
	var cg string
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "ActiveState="); ok {
			active = v == "active"
		}
		if v, ok := strings.CutPrefix(l, "ControlGroup="); ok {
			cg = v
		}
	}
	if !active || cg == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(t.fs(), cg)); err != nil {
		return ""
	}
	return cg
}

// enableControllers enables, for dir's children, every Wanted
// controller dir has.
func enableControllers(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return err
	}
	have := strings.Fields(string(b))
	var want []string
	for _, c := range Wanted {
		if slices.Contains(have, c) {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	f := filepath.Join(dir, "cgroup.subtree_control")
	if cur, err := os.ReadFile(f); err == nil && len(strings.Fields(string(cur))) >= len(want) {
		return nil
	}
	if err := os.WriteFile(f, []byte(strings.Join(want, " ")), 0o644); err != nil {
		return fmt.Errorf("rootless: enabling %s in %s: %w", strings.Join(want, " "), dir, err)
	}
	return nil
}

// Limits are the parts of a spec's linux.resources a container's
// cgroup enforces.
type Limits struct {
	Memory *struct {
		Limit *int64 `json:"limit"`
	} `json:"memory"`
	CPU *struct {
		Quota  *int64  `json:"quota"`
		Period *uint64 `json:"period"`
	} `json:"cpu"`
	Pids *struct {
		Limit int64 `json:"limit"`
	} `json:"pids"`
}

// BundleCgroup reads a bundle's AnnotationCgroup and its spec's limits.
// cgroup is "" when the container has none.
func BundleCgroup(bundle string) (cgroup string, l Limits, err error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return "", l, err
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
		Linux       struct {
			Resources *Limits `json:"resources"`
		} `json:"linux"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return "", l, fmt.Errorf("rootless: %s: %w", bundle, err)
	}
	if spec.Linux.Resources != nil {
		l = *spec.Linux.Resources
	}
	return spec.Annotations[AnnotationCgroup], l, nil
}

// WriteLimits writes l to the cgroup at dir: memory.max, with no swap
// beside it, so that a silo's memory is what it was granted; cpu.max;
// and pids.max. A limit the spec doesn't set is left at the cgroup's
// default, no limit.
func WriteLimits(dir string, l Limits) error {
	write := func(name, v string) error {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
			return fmt.Errorf("rootless: %s: %w", name, err)
		}
		return nil
	}
	if l.Memory != nil && l.Memory.Limit != nil && *l.Memory.Limit > 0 {
		if err := write("memory.max", strconv.FormatInt(*l.Memory.Limit, 10)); err != nil {
			return err
		}
		// Without swap accounting there is no file, and no swap to
		// limit.
		if _, err := os.Stat(filepath.Join(dir, "memory.swap.max")); err == nil {
			if err := write("memory.swap.max", "0"); err != nil {
				return err
			}
		}
	}
	if l.CPU != nil && l.CPU.Quota != nil && *l.CPU.Quota > 0 {
		period := uint64(100000)
		if l.CPU.Period != nil && *l.CPU.Period > 0 {
			period = *l.CPU.Period
		}
		if err := write("cpu.max", fmt.Sprintf("%d %d", *l.CPU.Quota, period)); err != nil {
			return err
		}
	}
	if l.Pids != nil && l.Pids.Limit > 0 {
		if err := write("pids.max", strconv.FormatInt(l.Pids.Limit, 10)); err != nil {
			return err
		}
	}
	return nil
}

// containerCgroup makes a bundle's container cgroup, if it names one,
// with the spec's limits, and opens it for CLONE_INTO_CGROUP. dir is ""
// and f nil when the container has none.
func containerCgroup(bundle string) (dir string, f *os.File, err error) {
	cg, l, err := BundleCgroup(bundle)
	if err != nil || cg == "" {
		return "", nil, err
	}
	dir = filepath.Join(CgroupFS, filepath.Clean("/"+cg))
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, fmt.Errorf("rootless: the container's cgroup: %w", err)
	}
	if err := WriteLimits(dir, l); err != nil {
		return "", nil, err
	}
	f, err = os.Open(dir)
	if err != nil {
		return "", nil, err
	}
	return dir, f, nil
}
