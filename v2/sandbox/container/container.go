// Adapted from github.com/bwsalmon/grain pkg/mcp/docker_exec_runner.go and pkg/orchestrator/container_sandboxes.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package container is the container sandbox backend (design §7.1): an
// OCI container run by the local Docker or Podman, and the gvisor kind,
// which is the same container run with --runtime=runsc.
//
// A sandbox starts with nothing of the host: no host files but its
// declared mounts (read-only unless rw), no Docker socket, no host
// network (none unless network: open, which is the runtime's default
// bridge, never the host's), no new privileges. Its main process is a
// shell that sleeps until it is stopped; work runs through `exec`.
//
// Every operation shells out to the runtime's own CLI, as grain does,
// so that silo works with whatever Docker or Podman the user has,
// rootless or not, without speaking its API.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// RuntimeEnv names the runtime binary to use, overriding detection.
const RuntimeEnv = "SILO_CONTAINER_RUNTIME"

// OutputLimit bounds each of a command's output streams.
const OutputLimit = 1 << 20

// ErrNoRunsc is returned when a gvisor sandbox is asked for and the
// runtime has no runsc.
var ErrNoRunsc = errors.New("the gVisor runtime (runsc) is not installed")

// Backend runs sandboxes with one container runtime CLI.
type Backend struct {
	// Runtime is the CLI: a path, or "docker" or "podman" on PATH.
	Runtime string
}

var _ sandbox.Backend = (*Backend)(nil)

// New returns a backend for the runtime named by SILO_CONTAINER_RUNTIME,
// or else Podman, which runs rootless as an ordinary user, when it is
// installed, or else Docker.
func New() (*Backend, error) {
	if rt := os.Getenv(RuntimeEnv); rt != "" {
		if _, err := exec.LookPath(rt); err != nil {
			return nil, fmt.Errorf("%s=%s: %w", RuntimeEnv, rt, err)
		}
		return &Backend{Runtime: rt}, nil
	}
	for _, rt := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(rt); err == nil {
			return &Backend{Runtime: rt}, nil
		}
	}
	return nil, errors.New("container sandboxes need Docker or Podman, and neither is installed")
}

func (b *Backend) podman() bool {
	return strings.Contains(filepath.Base(b.Runtime), "podman")
}

// cli runs the runtime CLI and returns its stdout, or an error carrying
// its stderr.
func (b *Backend) cli(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.Runtime, args...)
	procgroup.Prepare(cmd)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %s", filepath.Base(b.Runtime), args[0], msg)
	}
	return stdout.Bytes(), nil
}

// HasRunsc reports whether the runtime can run containers with runsc.
func (b *Backend) HasRunsc(ctx context.Context) (bool, error) {
	if b.podman() {
		// Podman runs any OCI runtime it can find on PATH by name.
		_, err := exec.LookPath("runsc")
		return err == nil, nil
	}
	out, err := b.cli(ctx, nil, nil, "info", "--format", "{{range $k, $v := .Runtimes}}{{$k}}\n{{end}}")
	if err != nil {
		return false, err
	}
	for _, rt := range strings.Fields(string(out)) {
		if rt == "runsc" {
			return true, nil
		}
	}
	return false, nil
}

// ResolveImage returns the image to run for spec, pulling or building it
// as needed, and what it resolved to.
func (b *Backend) ResolveImage(ctx context.Context, spec sandbox.SandboxSpec) (string, sandbox.Image, error) {
	if spec.Image == "" {
		if spec.Build == "" {
			return "", sandbox.Image{}, fmt.Errorf("sandbox %q has neither image nor build", spec.Name)
		}
		out, err := b.cli(ctx, nil, nil, "build", "-q", "-f", spec.Build, filepath.Dir(spec.Build))
		if err != nil {
			return "", sandbox.Image{}, fmt.Errorf("sandbox %q: building %s: %w", spec.Name, spec.Build, err)
		}
		lines := strings.Fields(string(out))
		if len(lines) == 0 {
			return "", sandbox.Image{}, fmt.Errorf("sandbox %q: building %s printed no image id", spec.Name, spec.Build)
		}
		id := lines[len(lines)-1]
		if !strings.HasPrefix(id, "sha256:") {
			id = "sha256:" + id
		}
		return id, sandbox.Image{Ref: spec.Build, Digest: id}, nil
	}
	inspect := func() ([]byte, error) {
		return b.cli(ctx, nil, nil, "image", "inspect", "--format", "{{.Id}}{{range .RepoDigests}} {{.}}{{end}}", spec.Image)
	}
	out, err := inspect()
	if err != nil {
		if _, perr := b.cli(ctx, nil, nil, "pull", "-q", spec.Image); perr != nil {
			return "", sandbox.Image{}, fmt.Errorf("sandbox %q: pulling %s: %w", spec.Name, spec.Image, perr)
		}
		if out, err = inspect(); err != nil {
			return "", sandbox.Image{}, fmt.Errorf("sandbox %q: %w", spec.Name, err)
		}
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", sandbox.Image{}, fmt.Errorf("sandbox %q: %s has no id", spec.Name, spec.Image)
	}
	id, digest := fields[0], fields[0]
	if len(fields) > 1 {
		digest = fields[1]
	}
	return id, sandbox.Image{Ref: spec.Image, Digest: digest}, nil
}

// containerName is the sandbox's container name: silo-<session>-<name>.
func containerName(spec sandbox.SandboxSpec) string {
	return "silo-" + spec.Session + "-" + spec.Name
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envArgs returns -e NAME for each variable, and the NAME=value pairs to
// put in the CLI's own environment, so that values (credentials among
// them) never appear in a process listing's argv.
func envArgs(env map[string]string) (args, pairs []string, err error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !envName.MatchString(k) {
			return nil, nil, fmt.Errorf("%q is not a valid environment variable name", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k)
		pairs = append(pairs, k+"="+env[k])
	}
	return args, pairs, nil
}

// idleScript is the sandbox's main process: it does nothing until it is
// stopped, and exits promptly on SIGTERM.
const idleScript = `trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done`

// Create starts a sandbox for spec.
func (b *Backend) Create(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Sandbox, error) {
	if spec.Session == "" || spec.Name == "" {
		return nil, errors.New("a sandbox needs a session and a name")
	}
	var runtimeArgs []string
	switch spec.Kind {
	case "container", "":
	case "gvisor":
		ok, err := b.HasRunsc(ctx)
		if err != nil {
			return nil, fmt.Errorf("sandbox %q: checking for runsc: %w", spec.Name, err)
		}
		if !ok {
			return nil, fmt.Errorf("sandbox %q is kind gvisor, but %w for %s: install it and register it as the runtime runsc (gvisor.dev/docs/user_guide/install)",
				spec.Name, ErrNoRunsc, filepath.Base(b.Runtime))
		}
		runtimeArgs = []string{"--runtime=runsc"}
	default:
		return nil, fmt.Errorf("sandbox %q: kind %q is not a container kind", spec.Name, spec.Kind)
	}
	image, img, err := b.ResolveImage(ctx, spec)
	if err != nil {
		return nil, err
	}
	name := containerName(spec)
	args := []string{"run", "-d", "--name", name,
		"--label", sandbox.SessionLabel + "=" + spec.Session,
		"--label", sandbox.NameLabel + "=" + spec.Name,
		"--label", sandbox.UserLabel + "=" + strconv.Itoa(os.Getuid()),
		"--hostname", spec.Name,
		"--security-opt", "no-new-privileges",
	}
	args = append(args, runtimeArgs...)
	switch spec.Network {
	case "none", "":
		if spec.PrivateNetwork != "" {
			// The session's internal network, which reaches the
			// invoking machine's git proxy and nothing else.
			args = append(args, "--network", spec.PrivateNetwork)
		} else {
			args = append(args, "--network", "none")
		}
	case "open":
		// The runtime's default network: a bridge, or Podman's rootless
		// user-mode network. Never the host's. A PrivateNetwork is
		// connected once the container exists.
	default:
		return nil, fmt.Errorf("sandbox %q: network %q is not supported yet", spec.Name, spec.Network)
	}
	if spec.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64))
	}
	if spec.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(spec.Memory, 10))
	}
	if spec.Workdir != "" {
		args = append(args, "--workdir", spec.Workdir)
	}
	for _, m := range spec.Mounts {
		if !filepath.IsAbs(m.From) || !filepath.IsAbs(m.To) || strings.Contains(m.From+m.To, ",") {
			return nil, fmt.Errorf("sandbox %q: mount %s:%s must be two absolute paths without commas", spec.Name, m.From, m.To)
		}
		opt := "type=bind,source=" + m.From + ",target=" + m.To
		if !m.RW {
			opt += ",readonly"
		}
		args = append(args, "--mount", opt)
	}
	eargs, env, err := envArgs(spec.Env)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	args = append(args, eargs...)
	args = append(args, "--entrypoint", "/bin/sh", image, "-c", idleScript)
	out, err := b.cli(ctx, env, nil, args...)
	if err != nil {
		// A container that was created but did not start is still there.
		_, _ = b.cli(context.WithoutCancel(ctx), nil, nil, "rm", "-f", name)
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	id := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(id, '\n'); i >= 0 {
		id = id[i+1:]
	}
	if spec.Network == "open" && spec.PrivateNetwork != "" {
		if _, err := b.cli(ctx, nil, nil, "network", "connect", spec.PrivateNetwork, id); err != nil {
			_, _ = b.cli(context.WithoutCancel(ctx), nil, nil, "rm", "-f", id)
			return nil, fmt.Errorf("sandbox %q: joining the session's network: %w", spec.Name, err)
		}
	}
	return &Container{backend: b, ID: id, Name: spec.Name, Session: spec.Session, Image: img}, nil
}

// Find lists the sandboxes, running or not, labelled with session.
func (b *Backend) Find(ctx context.Context, session string) ([]sandbox.Sandbox, error) {
	out, err := b.cli(ctx, nil, nil, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+sandbox.SessionLabel+"="+session)
	if err != nil {
		return nil, err
	}
	var found []sandbox.Sandbox
	for _, id := range strings.Fields(string(out)) {
		out, err := b.cli(ctx, nil, nil, "inspect", "--format", `{{index .Config.Labels "`+sandbox.NameLabel+`"}}`+vmFormat, id)
		if err != nil {
			// Removed between the listing and now.
			continue
		}
		name, ok := notVM(out)
		if !ok {
			continue
		}
		found = append(found, &Container{backend: b, ID: id, Name: name, Session: session})
	}
	return found, nil
}

// Sessions lists the sessions this user's labelled containers belong
// to, running or not, for silo gc.
func (b *Backend) Sessions(ctx context.Context) ([]string, error) {
	out, err := b.cli(ctx, nil, nil, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+sandbox.UserLabel+"="+strconv.Itoa(os.Getuid()))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var sessions []string
	for _, id := range strings.Fields(string(out)) {
		s, err := b.cli(ctx, nil, nil, "inspect", "--format", `{{index .Config.Labels "`+sandbox.SessionLabel+`"}}`+vmFormat, id)
		if err != nil {
			continue
		}
		if name, ok := notVM(s); ok && name != "" && !seen[name] {
			seen[name] = true
			sessions = append(sessions, name)
		}
	}
	return sessions, nil
}

// vmFormat follows a label in an inspect format with the label granule
// puts on a VM's container (granule.dev/vm). A granule sandbox's VM
// runs in a container carrying silo's labels too, and is the granule
// backend's to find and destroy, with granule's own delete.
const vmFormat = `|{{index .Config.Labels "granule.dev/vm"}}`

// notVM splits what an inspect with vmFormat printed, and reports
// whether the container is not a granule VM's.
func notVM(out []byte) (string, bool) {
	label, vm, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	return label, vm == ""
}

// Container is one container sandbox.
type Container struct {
	backend *Backend
	// ID is the container's id.
	ID string
	// Name is the sandbox's name in the Silofile.
	Name    string
	Session string
	// Image is what the image resolved to, for .silo/lock.json. Empty for
	// a sandbox found rather than created.
	Image sandbox.Image
}

var (
	_ sandbox.Sandbox = (*Container)(nil)
	_ sandbox.Imaged  = (*Container)(nil)
)

// ResolvedImage implements sandbox.Imaged.
func (c *Container) ResolvedImage() sandbox.Image { return c.Image }

// killGrace is how long Exec waits, once its context is done and the
// command has been killed, for the runtime CLI to return what it has
// before killing the CLI as well.
const killGrace = 3 * time.Second

// execScript records the command's pid so that a cancelled Exec can kill
// it inside the sandbox (killing the runtime CLI alone would leave it
// running there), then becomes the command.
const execScript = `echo $$ > "/tmp/.silo-exec-$0" 2>/dev/null; exec "$@"`

// killScript kills a recorded command and, where /proc lists them, its
// descendants.
const killScript = `f="/tmp/.silo-exec-$0"; [ -f "$f" ] || exit 0
kill_tree() { for c in $(cat /proc/$1/task/*/children 2>/dev/null); do kill_tree "$c"; done; kill -KILL "$1" 2>/dev/null; }
kill_tree "$(cat "$f")"; rm -f "$f"`

// Exec runs req.Argv in the sandbox. When ctx is done first, the
// command is killed and Exec returns what it printed with ctx's error.
func (c *Container) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if len(req.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("exec: empty command")
	}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{"exec"}
	if req.Stdin != nil {
		args = append(args, "-i")
	}
	if req.Cwd != "" {
		args = append(args, "--workdir", req.Cwd)
	}
	eargs, env, err := envArgs(req.Env)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	args = append(args, eargs...)
	args = append(args, c.ID, "/bin/sh", "-c", execScript, tag)
	args = append(args, req.Argv...)

	// The CLI runs under its own context: on ctx's end the command is
	// killed inside the sandbox first, so that the CLI returns with the
	// output so far rather than being cut off.
	cliCtx, cancelCLI := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCLI()
	cmd := exec.CommandContext(cliCtx, c.backend.Runtime, args...)
	procgroup.Prepare(cmd)
	cmd.Env = append(os.Environ(), env...)
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
		_, _ = c.backend.cli(killCtx, nil, nil, "exec", c.ID, "/bin/sh", "-c", killScript, tag)
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
		// The runtime's own failures (no such container, no /bin/sh)
		// exit 125 to 127 with its message on stderr, which the caller
		// sees as it would the command's own.
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}

// run is Exec of a shell script with arguments, as a plain call: a
// non-zero exit is an error carrying the script's stderr.
func (c *Container) run(ctx context.Context, stdin []byte, script string, args ...string) ([]byte, error) {
	argv := append([]string{"/bin/sh", "-c", script, "silo"}, args...)
	res, err := c.Exec(ctx, sandbox.ExecRequest{Argv: argv, Stdin: stdin})
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

// File operations run through exec with the sandbox's own sh, cat and
// head rather than the runtime's copy API, because gVisor keeps a
// container's writes in its own overlay, where the runtime cannot see
// them.

// ReadFile returns at most limit bytes of the file at path; a relative
// path is relative to the sandbox's workdir.
func (c *Container) ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	out, err := c.run(ctx, nil, `if [ -d "$1" ]; then echo "$1: is a directory" >&2; exit 1; fi; exec head -c "$2" -- "$1"`,
		path, strconv.FormatInt(limit, 10))
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
func (c *Container) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	_, err := c.run(ctx, data, `mkdir -p -- "$(dirname -- "$1")" && cat > "$1" && chmod "$2" -- "$1"`,
		path, fmt.Sprintf("%04o", mode.Perm()))
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// DeliverFile writes a credential file, mode 0400, by writing a
// temporary file beside it and renaming it into place, so that a reader
// sees the old file or the new one and never a partial one.
func (c *Container) DeliverFile(ctx context.Context, path string, data []byte) error {
	_, err := c.run(ctx, data, `umask 077; d="$(dirname -- "$1")"; mkdir -p -- "$d" || exit 1
t="$d/.silo-deliver.$$"
if cat > "$t" && chmod 0400 "$t" && mv -f -- "$t" "$1"; then exit 0; fi
rm -f -- "$t"; exit 1`, path)
	if err != nil {
		return fmt.Errorf("delivering %s: %w", path, err)
	}
	return nil
}

// Destroy removes the container and its anonymous volumes. A container
// that is already gone is not an error.
func (c *Container) Destroy(ctx context.Context) error {
	_, err := c.backend.cli(ctx, nil, nil, "rm", "-f", "-v", c.ID)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}
	return err
}
