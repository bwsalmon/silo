// Adapted from github.com/bwsalmon/grain pkg/granule/granule.go and pkg/mcp/docker_exec_runner.go at 85fbb3f62a637606693dd3e965f1f64a5215227d, Apache License 2.0.

// Package granule is the granule sandbox backend (design §7.1, journey
// 7): a granule microVM booted from a granule VM image (an OCI image
// carrying both the VMM and the guest disk) on the local Docker, which
// is granule's model, "VMs in containers".
//
// It drives the granule binary -- `granule vm create`, `vm wait`, `vm
// exec`, `vm delete` -- and never imports granule's Go packages, whose
// module grain's module proxy cannot fetch. What it shares with granule
// is deliberately shallow, as grain's own driver's is: the subcommand
// names and flags (gke-labs/granule pkg/cli vm.go, vmexec.go,
// vmwait.go), the container name granule derives from a VM name, and
// the label granule puts on it. Labels of silo's own reach the VM's
// container through granule's -docker-run-opt, so `docker ps` finds a
// session's VMs the way it finds its containers.
//
// A guest has exactly one network interface, which takes over its
// container's address (granule's flat networking), so the VM's network
// is the network its container is started on: the session's private
// network when the sandbox has repos and network none, a private
// network of the sandbox's own when it has neither (a VM container on
// --network none never boots), and Docker's default bridge for
// network open, from which the session's git proxy is reachable too,
// on the host.
package granule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/internal/headtail"
	"github.com/bwsalmon/silo/v2/internal/procgroup"
	"github.com/bwsalmon/silo/v2/sandbox"
)

// BinaryEnv names the granule binary to use, overriding "granule" on
// PATH.
const BinaryEnv = "SILO_GRANULE"

// OutputLimit bounds each of a command's output streams.
const OutputLimit = 1 << 20

// VMLabel is the label granule's docker backend puts on every VM
// container, valued with the VM's name (granule's dockervm.Create).
const VMLabel = "granule.dev/vm"

// WorkdirLabel carries the sandbox's workdir, which a VM has no notion
// of, so that a sandbox found again runs commands in it.
const WorkdirLabel = "silo.workdir"

// NetworkLabel carries the private network Create made for the
// sandbox alone, which Destroy removes with it.
const NetworkLabel = "silo.network"

// ReadyTimeout is how long Create waits for a guest to answer.
const ReadyTimeout = 3 * time.Minute

// EnvFile is where the sandbox's environment lives in the guest: a VM
// has no environment of its own to give `vm exec`, and passing it as
// -e would put credentials in the argv of a process on the invoking
// machine.
const EnvFile = "/run/silo/env"

// Backend runs granule sandboxes with the granule and docker CLIs.
type Backend struct {
	// Granule is the granule binary: a path, or a name on PATH. Empty
	// means "granule".
	Granule string
	// Docker is the docker CLI granule runs VMs with. Empty means
	// "docker".
	Docker string
	// StateDir is passed as -state-dir when set; empty leaves
	// granule's own default (~/.local/state/granule/vms).
	StateDir string
}

var (
	_ sandbox.Backend       = (*Backend)(nil)
	_ sandbox.SessionLister = (*Backend)(nil)
)

// New returns a backend for the granule named by SILO_GRANULE, or else
// granule on PATH, and docker on PATH.
func New() (*Backend, error) { return NewFrom(os.Getenv) }

// NewFrom is New with SILO_GRANULE read through getenv.
func NewFrom(getenv func(string) string) (*Backend, error) {
	bin := getenv(BinaryEnv)
	if bin == "" {
		bin = "granule"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("granule sandboxes need the granule binary on PATH (go build ./cmd/granule in gke-labs/granule, or set %s): %w", BinaryEnv, err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("granule sandboxes need Docker: %w", err)
	}
	return &Backend{Granule: bin, Docker: "docker"}, nil
}

func (b *Backend) granule() string {
	if b.Granule == "" {
		return "granule"
	}
	return b.Granule
}

func (b *Backend) docker() string {
	if b.Docker == "" {
		return "docker"
	}
	return b.Docker
}

// run runs a CLI and returns its stdout, or an error carrying its
// output.
func run(ctx context.Context, bin string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	procgroup.Prepare(cmd)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		sub := args[0]
		if sub == "vm" && len(args) > 1 {
			sub += " " + args[1]
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %s", filepath.Base(bin), sub, msg)
	}
	return stdout.Bytes(), nil
}

// vm runs `granule vm <sub> <name> [-state-dir dir] <args...>`, the
// shape every `granule vm` subcommand shares.
func (b *Backend) vm(ctx context.Context, stdin []byte, sub, name string, args ...string) ([]byte, error) {
	return run(ctx, b.granule(), stdin, b.vmArgs(sub, name, args...)...)
}

func (b *Backend) vmArgs(sub, name string, args ...string) []string {
	full := []string{"vm", sub, name}
	if b.StateDir != "" {
		full = append(full, "-state-dir", b.StateDir)
	}
	return append(full, args...)
}

// VMName is the granule VM name of a session's sandbox. granule names
// a tap device after the VM ("tap-<name>"), which leaves eleven
// characters of a DNS-1123 label, so the name is a digest of the
// session and the sandbox rather than either; the labels carry both.
func VMName(session, name string) string {
	sum := sha256.Sum256([]byte(session + "/" + name))
	return "silo" + strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:7]
}

// ContainerName is the container granule runs a VM in (granule's
// vmspec.ContainerName).
func ContainerName(vm string) string { return "granule-vm-" + vm }

// ResolveImage returns the granule VM image of spec as the daemon
// holds it, pulling it when it is missing, and what it resolved to.
func (b *Backend) ResolveImage(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Image, error) {
	if spec.Image == "" {
		return sandbox.Image{}, fmt.Errorf("sandbox %q: kind granule needs image (a granule VM image) or granulefile", spec.Name)
	}
	inspect := func() ([]byte, error) {
		return run(ctx, b.docker(), nil, "image", "inspect", "--format", "{{.Id}}{{range .RepoDigests}} {{.}}{{end}}", spec.Image)
	}
	out, err := inspect()
	if err != nil {
		if _, perr := run(ctx, b.docker(), nil, "pull", "-q", spec.Image); perr != nil {
			return sandbox.Image{}, fmt.Errorf("sandbox %q: pulling %s: %w", spec.Name, spec.Image, perr)
		}
		if out, err = inspect(); err != nil {
			return sandbox.Image{}, fmt.Errorf("sandbox %q: %w", spec.Name, err)
		}
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return sandbox.Image{}, fmt.Errorf("sandbox %q: %s has no id", spec.Name, spec.Image)
	}
	digest := fields[0]
	if len(fields) > 1 {
		digest = fields[1]
	}
	return sandbox.Image{Ref: spec.Image, Digest: digest}, nil
}

// networkName is the private network of a sandbox with network none
// and no session network to join.
func networkName(spec sandbox.SandboxSpec) string {
	return "silo-" + spec.Session + "-" + spec.Name
}

// CreateArgs returns the `granule vm create` arguments for spec after
// the VM's name and -state-dir: the image, its size where the Silofile gave one (the
// image's own otherwise), and the docker run options that label the
// VM's container and put it on network (none for Docker's default).
func (b *Backend) CreateArgs(spec sandbox.SandboxSpec, network string) []string {
	args := []string{"-kontur-image", spec.Image}
	if spec.CPUs > 0 {
		args = append(args, "-cpus", strconv.Itoa(int(math.Ceil(spec.CPUs))))
	}
	if spec.Memory > 0 {
		args = append(args, "-memory-mb", strconv.FormatInt(ceilDiv(spec.Memory, 1<<20), 10))
	}
	if spec.Disk > 0 {
		args = append(args, "-disk-size-mb", strconv.FormatInt(ceilDiv(spec.Disk, 1<<20), 10))
	}
	opts := []string{
		"--label=" + sandbox.SessionLabel + "=" + spec.Session,
		"--label=" + sandbox.NameLabel + "=" + spec.Name,
		"--label=" + sandbox.UserLabel + "=" + strconv.Itoa(os.Getuid()),
	}
	if spec.Workdir != "" {
		opts = append(opts, "--label="+WorkdirLabel+"="+spec.Workdir)
	}
	if network != "" {
		if network == networkName(spec) {
			opts = append(opts, "--label="+NetworkLabel+"="+network)
		}
		opts = append(opts, "--network="+network)
	}
	for _, o := range opts {
		args = append(args, "-docker-run-opt="+o)
	}
	return args
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Create boots a VM for spec and waits for its guest to answer.
func (b *Backend) Create(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Sandbox, error) {
	if spec.Session == "" || spec.Name == "" {
		return nil, errors.New("a sandbox needs a session and a name")
	}
	if spec.Kind != "granule" {
		return nil, fmt.Errorf("sandbox %q: kind %q is not granule", spec.Name, spec.Kind)
	}
	if spec.Image != "" && spec.Build != "" {
		return nil, fmt.Errorf("sandbox %q: give image or granulefile, not both", spec.Name)
	}
	if len(spec.Mounts) > 0 {
		return nil, fmt.Errorf("sandbox %q: kind granule takes no mounts: a host directory would have to cross into the guest over virtio-fs, which silo does not set up", spec.Name)
	}
	keys, err := envKeys(spec.Env)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	var img sandbox.Image
	if spec.Build != "" {
		// Booted by the tag it is kept under, which names its content.
		img, spec.Image, err = b.BuildImage(ctx, spec)
	} else {
		img, err = b.ResolveImage(ctx, spec)
	}
	if err != nil {
		return nil, err
	}
	vm := VMName(spec.Session, spec.Name)
	v := &VM{backend: b, VM: vm, Name: spec.Name, Session: spec.Session, Workdir: spec.Workdir, Image: img}
	cleanup := func() { _ = v.Destroy(context.WithoutCancel(ctx)) }

	var network string
	switch spec.Network {
	case "none", "":
		network = spec.PrivateNetwork
		if network == "" {
			// A VM container with no network never boots: give the
			// guest a network that reaches nothing but this machine.
			network = networkName(spec)
			if _, err := run(ctx, b.docker(), nil, "network", "create", "--internal",
				"--label", sandbox.SessionLabel+"="+spec.Session,
				"--label", sandbox.UserLabel+"="+strconv.Itoa(os.Getuid()),
				network); err != nil {
				return nil, fmt.Errorf("sandbox %q: creating its network: %w", spec.Name, err)
			}
			v.Network = network
		}
	case "open":
		// Docker's default bridge. The guest has one interface, so it
		// does not join the session's network as well; the git proxy
		// listens on the host, which the bridge reaches.
	default:
		return nil, fmt.Errorf("sandbox %q: network %q is not none or open", spec.Name, spec.Network)
	}
	if _, err := b.vm(ctx, nil, "create", vm, b.CreateArgs(spec, network)...); err != nil {
		cleanup()
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	if _, err := b.vm(ctx, nil, "wait", vm, "-timeout", ReadyTimeout.String()); err != nil {
		logs, _ := run(context.WithoutCancel(ctx), b.docker(), nil, "logs", "--tail", "20", ContainerName(vm))
		cleanup()
		return nil, fmt.Errorf("sandbox %q: %w\n%s", spec.Name, err, strings.TrimSpace(string(logs)))
	}
	setup := `set -e; if [ -n "$1" ]; then mkdir -p -- "$1"; fi
if [ "$2" = 1 ]; then umask 077; mkdir -p -- "$(dirname -- "$3")"; cat > "$3"; fi`
	hasEnv := "0"
	var envData []byte
	if len(keys) > 0 {
		hasEnv = "1"
		envData = envFile(spec.Env, keys)
	}
	if _, err := v.run(ctx, envData, setup, spec.Workdir, hasEnv, EnvFile); err != nil {
		cleanup()
		return nil, fmt.Errorf("sandbox %q: setting up the guest: %w", spec.Name, err)
	}
	return v, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func envKeys(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("%q is not a valid environment variable name", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// envFile is env as sh assignments, each value single-quoted.
func envFile(env map[string]string, keys []string) []byte {
	var buf bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&buf, "export %s=%s\n", k, shellQuote(env[k]))
	}
	return buf.Bytes()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// psFormat lists a VM container's VM, sandbox, workdir, network and
// session, tab-separated.
const psFormat = `{{.Label "` + VMLabel + `"}}	{{.Label "` + sandbox.NameLabel + `"}}	{{.Label "` + WorkdirLabel + `"}}	{{.Label "` + NetworkLabel + `"}}	{{.Label "` + sandbox.SessionLabel + `"}}`

// list returns the VMs, running or not, whose containers carry filter.
func (b *Backend) list(ctx context.Context, filter string) ([]*VM, error) {
	out, err := run(ctx, b.docker(), nil, "ps", "-a", "--filter", "label="+VMLabel, "--filter", "label="+filter, "--format", psFormat)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var vms []*VM
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 || f[0] == "" || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		vms = append(vms, &VM{backend: b, VM: f[0], Name: f[1], Workdir: f[2], Network: f[3], Session: f[4]})
	}
	return vms, nil
}

// Find lists the VMs, running or not, labelled with session.
func (b *Backend) Find(ctx context.Context, session string) ([]sandbox.Sandbox, error) {
	vms, err := b.list(ctx, sandbox.SessionLabel+"="+session)
	if err != nil {
		return nil, err
	}
	found := make([]sandbox.Sandbox, len(vms))
	for i, v := range vms {
		found[i] = v
	}
	return found, nil
}

// Sessions lists the sessions this user's VMs belong to, for silo gc.
func (b *Backend) Sessions(ctx context.Context) ([]string, error) {
	vms, err := b.list(ctx, sandbox.UserLabel+"="+strconv.Itoa(os.Getuid()))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var sessions []string
	for _, v := range vms {
		if v.Session != "" && !seen[v.Session] {
			seen[v.Session] = true
			sessions = append(sessions, v.Session)
		}
	}
	return sessions, nil
}

// VM is one granule sandbox.
type VM struct {
	backend *Backend
	// VM is granule's name for it.
	VM string
	// Name is the sandbox's name in the Silofile.
	Name    string
	Session string
	// Workdir is where a command with no Cwd runs; empty means the
	// guest account's home directory.
	Workdir string
	// Network is the private network made for this sandbox alone, if
	// any.
	Network string
	// Image is what the image resolved to, for .silo/lock.json. Empty
	// for a sandbox found rather than created.
	Image sandbox.Image
}

var (
	_ sandbox.Sandbox = (*VM)(nil)
	_ sandbox.Imaged  = (*VM)(nil)
)

// ResolvedImage implements sandbox.Imaged.
func (v *VM) ResolvedImage() sandbox.Image { return v.Image }

// killGrace is how long Exec waits, once its context is done and the
// command has been killed, for granule to return what it has before
// killing granule as well.
const killGrace = 3 * time.Second

// execScript loads the sandbox's environment, records the command's
// pid so that a cancelled Exec can kill it in the guest (killing
// granule alone would leave it running there), and becomes the
// command.
const execScript = `echo $$ > "/tmp/.silo-exec-$0" 2>/dev/null
if [ -r ` + EnvFile + ` ]; then set -a; . ` + EnvFile + `; set +a; fi
exec "$@"`

// killScript kills a recorded command and its descendants.
const killScript = `f="/tmp/.silo-exec-$0"; [ -f "$f" ] || exit 0
kill_tree() { for c in $(cat /proc/$1/task/*/children 2>/dev/null); do kill_tree "$c"; done; kill -KILL "$1" 2>/dev/null; }
kill_tree "$(cat "$f")"; rm -f "$f"`

// ExecArgs returns the granule arguments that run argv in the VM as
// req asks: in req.Cwd, or the sandbox's workdir, with req.Env on top
// of the sandbox's environment, which execScript loads.
func (v *VM) ExecArgs(req sandbox.ExecRequest, tag string) ([]string, error) {
	keys, err := envKeys(req.Env)
	if err != nil {
		return nil, err
	}
	var flags []string
	cwd := req.Cwd
	if cwd == "" {
		cwd = v.Workdir
	}
	if cwd != "" {
		if !strings.HasPrefix(cwd, "/") && v.Workdir != "" {
			cwd = v.Workdir + "/" + cwd
		}
		flags = append(flags, "-w", cwd)
	}
	argv := []string{"/bin/sh", "-c", execScript, tag}
	if len(keys) > 0 {
		// After the sandbox's environment, so that the request's wins.
		argv = append(argv, "env")
		for _, k := range keys {
			argv = append(argv, k+"="+req.Env[k])
		}
	}
	argv = append(argv, req.Argv...)
	return v.backend.vmArgs("exec", v.VM, append(append(flags, "--"), argv...)...), nil
}

// Exec runs req.Argv in the guest. When ctx is done first, the command
// is killed and Exec returns what it printed with ctx's error.
func (v *VM) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if len(req.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("exec: empty command")
	}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	args, err := v.ExecArgs(req, tag)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	cliCtx, cancelCLI := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCLI()
	cmd := exec.CommandContext(cliCtx, v.backend.granule(), args...)
	procgroup.Prepare(cmd)
	if req.Stdin != nil {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	stdout, stderr := headtail.New(OutputLimit), headtail.New(OutputLimit)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return sandbox.ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr, ctxErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		ctxErr = ctx.Err()
		killCtx, cancel := context.WithTimeout(context.Background(), killGrace)
		_, _ = v.backend.vm(killCtx, nil, "exec", v.VM, "--", "/bin/sh", "-c", killScript, tag)
		cancel()
		select {
		case runErr = <-done:
		case <-time.After(killGrace):
			cancelCLI()
			runErr = <-done
		}
	}
	res := sandbox.ExecResult{
		Stdout:  stdout.Bytes(),
		Stderr:  stderr.Bytes(),
		Dropped: stdout.Dropped() + stderr.Dropped(),
	}
	if ctxErr != nil {
		res.ExitCode = -1
		return res, ctxErr
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return res, runErr
		}
		// granule exits with the guest command's own status; its own
		// failures (no such VM, a guest that stopped answering) exit 1
		// with its message on stderr, which the caller sees as it
		// would the command's own.
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}

// run is Exec of a shell script with arguments, as a plain call: a
// non-zero exit is an error carrying the script's stderr.
func (v *VM) run(ctx context.Context, stdin []byte, script string, args ...string) ([]byte, error) {
	argv := append([]string{"/bin/sh", "-c", script, "silo"}, args...)
	res, err := v.Exec(ctx, sandbox.ExecRequest{Argv: argv, Stdin: stdin, Cwd: "/"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", res.ExitCode)
		}
		return nil, errors.New(msg)
	}
	return res.Stdout, nil
}

// abs makes a relative path relative to the workdir, as the container
// kinds' file tools take it.
func (v *VM) abs(path string) string {
	if strings.HasPrefix(path, "/") || v.Workdir == "" {
		return path
	}
	return v.Workdir + "/" + path
}

// ReadFile returns at most limit bytes of the file at path; a relative
// path is relative to the sandbox's workdir.
func (v *VM) ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	out, err := v.run(ctx, nil, `if [ -d "$1" ]; then echo "$1: is a directory" >&2; exit 1; fi; exec head -c "$2" -- "$1"`,
		v.abs(path), strconv.FormatInt(limit, 10))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// WriteFile writes data to path, creating its parent directories, and
// sets its mode.
func (v *VM) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	_, err := v.run(ctx, data, `mkdir -p -- "$(dirname -- "$1")" && cat > "$1" && chmod "$2" -- "$1"`,
		v.abs(path), fmt.Sprintf("%04o", mode.Perm()))
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// DeliverFile writes a credential file, mode 0400, by writing a
// temporary file beside it and renaming it into place, so that a reader
// sees the old file or the new one and never a partial one.
func (v *VM) DeliverFile(ctx context.Context, path string, data []byte) error {
	_, err := v.run(ctx, data, `umask 077; d="$(dirname -- "$1")"; mkdir -p -- "$d" || exit 1
t="$d/.silo-deliver.$$"
if cat > "$t" && chmod 0400 "$t" && mv -f -- "$t" "$1"; then exit 0; fi
rm -f -- "$t"; exit 1`, v.abs(path))
	if err != nil {
		return fmt.Errorf("delivering %s: %w", path, err)
	}
	return nil
}

// Destroy deletes the VM, its container and its saved state, and the
// network made for it alone. A VM that is already gone is not an
// error. Without granule (silo gc on a machine that no longer has it)
// the container is removed directly, which is what granule's own
// delete does to it.
func (v *VM) Destroy(ctx context.Context) error {
	var errs []error
	deleted := false
	if _, err := exec.LookPath(v.backend.granule()); err == nil {
		_, err = v.backend.vm(ctx, nil, "delete", v.VM)
		errs = append(errs, err)
		deleted = err == nil
	}
	if !deleted {
		if _, err := run(ctx, v.backend.docker(), nil, "rm", "-f", ContainerName(v.VM)); err != nil && !strings.Contains(err.Error(), "No such container") {
			errs = append(errs, err)
		}
	}
	if v.Network != "" {
		if _, err := run(ctx, v.backend.docker(), nil, "network", "rm", v.Network); err != nil {
			if msg := strings.ToLower(err.Error()); !strings.Contains(msg, "not found") && !strings.Contains(msg, "no such network") {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Doctor runs `granule doctor`, granule's own check that this machine
// can run its VMs (/dev/kvm above all), and returns what it printed. It
// fails when one of granule's required checks does.
func (b *Backend) Doctor(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, b.granule(), "doctor")
	procgroup.Prepare(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// CheckImage reports whether Create could boot ref without changing
// anything: the daemon has it, or its registry answers for it (docker
// buildx imagetools inspect, or docker manifest inspect without buildx),
// which is as near to "it can be pulled" as asking gets without pulling
// gigabytes of guest disk.
func (b *Backend) CheckImage(ctx context.Context, ref string) (string, error) {
	if _, err := run(ctx, b.docker(), nil, "image", "inspect", "--format", "{{.Id}}", ref); err == nil {
		return ref + " is present locally", nil
	}
	_, err := run(ctx, b.docker(), nil, "buildx", "imagetools", "inspect", ref)
	if err != nil && strings.Contains(err.Error(), "unknown command") {
		_, err = run(ctx, b.docker(), nil, "manifest", "inspect", ref)
	}
	if err != nil {
		return "", fmt.Errorf("%s is not local and its registry does not offer it: %w", ref, err)
	}
	return ref + " can be pulled", nil
}
