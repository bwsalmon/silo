package gvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup1/stats"
	stats2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// The labels the driver adds to silod's own.
const (
	LabelClass      = "dev.silo.class"
	LabelStopSignal = "dev.silo.stopsignal"
)

// AnnotationContainerType is the annotation by which gVisor's shim
// (specutils.SpecContainerType) tells a sandbox's root container from a
// subcontainer.
const AnnotationContainerType = "io.kubernetes.cri.container-type"

// ToolboxPath is where the toolbox busybox is mounted in a container.
const ToolboxPath = "/.silo/busybox"

// DefaultSnapshotter is the snapshotter containers' roots are made by.
const DefaultSnapshotter = "overlayfs"

// Config is how the driver reaches silo-containerd and the holder.
type Config struct {
	// Address is silo-containerd's socket (rootless.Paths.ContainerdSocket).
	Address string
	// Namespace is the containerd namespace, rootless.ContainerdNamespace
	// when empty.
	Namespace string
	// Shim is the runsc shim: containerd's runtime name
	// (rootless.RuntimeRunsc, found on containerd's PATH) or the
	// absolute path of containerd-shim-runsc-v1.
	Shim string
	// Options is the shim's options file (rootless.WriteRunsc), which
	// names `silod runsc` as its runsc.
	Options string
	// Dir holds each container's FIFOs and log file. The shim opens the
	// FIFOs inside the holder, so the path must name the same directory
	// there as on the host: a directory under the data directory or
	// $XDG_RUNTIME_DIR, not /run.
	Dir string
	// Volumes is the storage driver's root (runtime.Spec.Validate).
	Volumes string
	// Logger is the command, argv, that runs LogMain: silod and
	// LogArg. LoggerEnv is added to its environment.
	Logger    []string
	LoggerEnv []string
	// InHolder runs silod with args inside the holder and returns its
	// combined output: RemapMain runs through it.
	InHolder func(ctx context.Context, args ...string) ([]byte, error)
	// HolderCmd is InHolder's command, not yet started, for output that
	// is streamed rather than collected (HolderCommand).
	HolderCmd func(args ...string) *exec.Cmd
	// Runsc is `silod runsc` up to its runsc command line, after silod,
	// as rootless.RunscWrapperScript runs it: "runsc", "--runsc" and the
	// pinned runsc. RunscRoot is the shim's options' root
	// (rootless.Paths.RunscRoot), under which the shim gives runsc the
	// namespace's directory as its --root. RootfsUpper runs runsc with
	// them through HolderCmd; without all three, it refuses.
	Runsc     []string
	RunscRoot string
	// Toolbox is a static busybox on the host, mounted read-only at
	// ToolboxPath in every container for CopyIn and CopyOut. Without
	// it, they need tar in the image.
	Toolbox string
	// SiloTools is silod's static silo-tools on the host, mounted
	// read-only at runtime.ToolsPath in an agent's tool container
	// (Spec.SiloTools). Without it, no tool container is made.
	SiloTools string
	// CgroupsPath gives a container's linux.cgroupsPath
	// (rootless.CgroupsPath), or "" for none, when cgroups aren't
	// delegated and the shim's options say ignore-cgroups.
	CgroupsPath func(runtime.Spec) string
	// Snapshotter is DefaultSnapshotter when empty.
	Snapshotter string
}

// Driver is the sandbox class.
type Driver struct {
	cfg    Config
	client *containerd.Client
	execs  atomic.Uint64
}

// New dials silo-containerd. It doesn't wait for it: Probe says whether
// it answers.
func New(cfg Config) (*Driver, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = rootless.ContainerdNamespace
	}
	if cfg.Snapshotter == "" {
		cfg.Snapshotter = DefaultSnapshotter
	}
	if cfg.Shim == "" {
		cfg.Shim = rootless.RuntimeRunsc
	}
	if cfg.Dir == "" || cfg.Address == "" || len(cfg.Logger) == 0 || cfg.InHolder == nil {
		return nil, errors.New("gvisor: a driver needs containerd's address, a directory, a log copier and the holder")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	c, err := containerd.New(cfg.Address, containerd.WithDefaultNamespace(cfg.Namespace), containerd.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("gvisor: silo-containerd: %w", err)
	}
	return &Driver{cfg: cfg, client: c}, nil
}

// Close closes the driver's connection. Containers keep running.
func (d *Driver) Close() error { return d.client.Close() }

// Client is the driver's containerd client, for importing images.
func (d *Driver) Client() *containerd.Client { return d.client }

func (d *Driver) ns(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, d.cfg.Namespace)
}

// Class implements runtime.Driver.
func (d *Driver) Class() runtime.Class { return runtime.Sandbox }

// Probe implements runtime.Driver: silo-containerd answers and the shim
// is there.
func (d *Driver) Probe(ctx context.Context) runtime.Probe {
	if _, err := d.client.Version(d.ns(ctx)); err != nil {
		return runtime.Probe{Why: "silo-containerd doesn't answer at " + d.cfg.Address + ": " + err.Error()}
	}
	if filepath.IsAbs(d.cfg.Shim) {
		if _, err := os.Stat(d.cfg.Shim); err != nil {
			return runtime.Probe{Why: "the runsc shim: " + err.Error()}
		}
	}
	if d.cfg.Options != "" {
		if _, err := os.Stat(d.cfg.Options); err != nil {
			return runtime.Probe{Why: "the runsc shim's options: " + err.Error()}
		}
	}
	return runtime.Probe{OK: true}
}

// image finds an image by its digest: its manifest's or index's (the
// target), or its config's, which is what Docker calls its id.
func (d *Driver) image(ctx context.Context, digest string) (containerd.Image, error) {
	is := d.client.ImageService()
	if l, err := is.List(ctx, "target.digest=="+digest); err == nil && len(l) > 0 {
		return containerd.NewImage(d.client, l[0]), nil
	}
	l, err := is.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, i := range l {
		img := containerd.NewImage(d.client, i)
		if c, err := img.Config(ctx); err == nil && c.Digest.String() == digest {
			return img, nil
		}
	}
	return nil, fmt.Errorf("%w: image %s is not in silo-containerd", runtime.ErrNotFound, digest)
}

// Prepare implements runtime.Driver: the sandbox class runs images as
// they are, so it only unpacks them.
func (d *Driver) Prepare(ctx context.Context, image string) error {
	ctx = d.ns(ctx)
	img, err := d.image(ctx, image)
	if err != nil {
		return err
	}
	if ok, err := img.IsUnpacked(ctx, d.cfg.Snapshotter); err == nil && ok {
		return nil
	}
	return img.Unpack(ctx, d.cfg.Snapshotter)
}

// dir is a container's directory: its FIFOs, log and copier pid file.
func (d *Driver) dir(id string) string { return filepath.Join(d.cfg.Dir, id) }

// LogPath is a container's log file, in Docker's json-file format.
func (d *Driver) LogPath(id string) string { return filepath.Join(d.dir(id), "log.json") }

func (d *Driver) fifo(id, name string) string { return filepath.Join(d.dir(id), name) }

// Create implements runtime.Driver. The container's netns and user
// namespace exist and are wired already (Wire): the spec names them,
// and the sandbox comes up on them. Create makes the container's
// snapshot, its spec, the log copier and the task, and leaves it
// created.
func (d *Driver) Create(ctx context.Context, spec runtime.Spec) (err error) {
	if err := spec.Validate(d.cfg.Volumes); err != nil {
		return err
	}
	if spec.UserNS == "" || spec.Root.Outside == 0 || spec.Root.Size == 0 {
		return errors.New("gvisor: the sandbox class runs runsc in the container's own user namespace, and the spec names none")
	}
	if d.cfg.Toolbox == "" {
		for _, m := range spec.Mounts {
			if m.Target == path.Dir(ToolboxPath) || strings.HasPrefix(m.Target, path.Dir(ToolboxPath)+"/") {
				return fmt.Errorf("gvisor: mount target %s is reserved", m.Target)
			}
		}
	}
	if spec.SiloTools && d.cfg.SiloTools == "" {
		return errors.New("gvisor: an agent's tool container needs silo-tools, and this driver was given none")
	}
	ctx = d.ns(ctx)
	if _, err := d.client.LoadContainer(ctx, spec.ID); err == nil {
		return fmt.Errorf("gvisor: container %s exists", spec.ID)
	}
	if err := d.Prepare(ctx, spec.Image); err != nil {
		return err
	}
	img, err := d.image(ctx, spec.Image)
	if err != nil {
		return err
	}
	ic, err := img.Spec(ctx)
	if err != nil {
		return fmt.Errorf("gvisor: image config: %w", err)
	}
	opts, err := d.specOpts(spec, ic.Config.Env, ic.Config.WorkingDir, ic.Config.User)
	if err != nil {
		return err
	}

	// The lease keeps the remapped snapshot from containerd's GC until
	// the container holds it.
	ctx, done, err := d.client.WithLease(ctx)
	if err != nil {
		return err
	}
	defer done(context.WithoutCancel(ctx))
	lease, _ := leases.FromContext(ctx)
	if out, err := d.cfg.InHolder(ctx, RemapArg, "--address", d.cfg.Address, "--namespace", d.cfg.Namespace,
		"--lease", lease, "--image", img.Name(), "--snapshotter", d.cfg.Snapshotter, "--key", spec.ID,
		"--outside", strconv.FormatUint(uint64(spec.Root.Outside), 10), "--size", strconv.FormatUint(uint64(spec.Root.Size), 10)); err != nil {
		return fmt.Errorf("gvisor: remapping %s's snapshot: %v: %s", spec.ID, err, strings.TrimSpace(string(out)))
	}
	labels := map[string]string{LabelClass: string(runtime.Sandbox)}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	if spec.StopSignal != 0 {
		labels[LabelStopSignal] = strconv.Itoa(int(spec.StopSignal))
	}
	c, err := d.client.NewContainer(ctx, spec.ID,
		containerd.WithSnapshotter(d.cfg.Snapshotter),
		containerd.WithSnapshot(spec.ID),
		containerd.WithImageName(img.Name()),
		containerd.WithContainerLabels(labels),
		containerd.WithRuntime(d.cfg.Shim, &runtimeoptions.Options{TypeUrl: "io.containerd.runsc.v1.options", ConfigPath: d.cfg.Options}),
		containerd.WithNewSpec(opts...),
	)
	if err != nil {
		d.client.SnapshotService(d.cfg.Snapshotter).Remove(ctx, spec.ID)
		return fmt.Errorf("gvisor: %w", err)
	}
	defer func() {
		if err != nil {
			d.destroy(context.WithoutCancel(ctx), c)
		}
	}()
	if err := os.MkdirAll(d.dir(spec.ID), 0o700); err != nil {
		return err
	}
	ioc := cio.Config{Stdout: d.fifo(spec.ID, "stdout"), Terminal: spec.Process.TTY}
	if !spec.Process.TTY {
		ioc.Stderr = d.fifo(spec.ID, "stderr")
	}
	if spec.Process.Stdin {
		ioc.Stdin = d.fifo(spec.ID, "stdin")
	}
	for _, f := range []string{ioc.Stdin, ioc.Stdout, ioc.Stderr} {
		if f != "" {
			if err := unix.Mkfifo(f, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("gvisor: %s: %w", f, err)
			}
		}
	}
	if err := d.startCopier(spec.ID, ioc); err != nil {
		return err
	}
	if _, err := c.NewTask(ctx, func(string) (cio.IO, error) { return fixedIO{ioc}, nil }); err != nil {
		return fmt.Errorf("gvisor: creating %s's sandbox: %w", spec.ID, err)
	}
	return nil
}

// specOpts builds a container's OCI spec: the process, the image's env
// and workdir under the spec's, its resources and mounts, then the
// sandbox class's rootless patch.
func (d *Driver) specOpts(spec runtime.Spec, imageEnv []string, imageWorkdir, imageUser string) ([]oci.SpecOpts, error) {
	user := spec.Process.User
	if user == "" {
		user = imageUser
	}
	uid, gid, err := numericUser(user)
	if err != nil {
		return nil, err
	}
	wd := spec.Process.Workdir
	if wd == "" {
		wd = imageWorkdir
	}
	if wd == "" {
		wd = "/"
	}
	host := spec.ID
	if len(host) > 12 {
		host = host[:12]
	}
	opts := []oci.SpecOpts{
		oci.WithProcessArgs(spec.Process.Args...),
		oci.WithEnv(imageEnv),
		oci.WithEnv(spec.Process.Env),
		oci.WithProcessCwd(wd),
		oci.WithUIDGID(uid, gid),
		oci.WithHostname(host),
		// Each container is a sandbox of its own. Without saying so,
		// the shim takes it for a subcontainer, gives runsc create no
		// stdio, and hands the pipes to runsc start, which a sandbox's
		// root container ignores: its workload writes to a closed pipe.
		oci.WithAnnotations(map[string]string{AnnotationContainerType: "sandbox"}),
	}
	if spec.Process.TTY {
		opts = append(opts, oci.WithTTY)
	}
	r := spec.Resources
	if r.MemoryMB > 0 {
		opts = append(opts, oci.WithMemoryLimit(uint64(r.MemoryMB)<<20))
	}
	if r.VCPUm > 0 {
		// A period of 100ms, so a thousandth of a vCPU is 100µs of it.
		opts = append(opts, oci.WithCPUCFS(r.VCPUm*100, 100000))
	}
	if r.PIDs > 0 {
		opts = append(opts, oci.WithPidsLimit(r.PIDs))
	}
	var ms []specs.Mount
	for _, m := range spec.Mounts {
		switch m.Kind {
		case runtime.MountVolume:
			o := []string{"rbind", "rw"}
			if m.ReadOnly {
				o[1] = "ro"
			}
			ms = append(ms, specs.Mount{Destination: m.Target, Type: "bind", Source: m.Source, Options: o})
		case runtime.MountTmpfs:
			o := []string{"nosuid", "nodev", "mode=1777"}
			if m.SizeMB > 0 {
				o = append(o, fmt.Sprintf("size=%dm", m.SizeMB))
			}
			if m.ReadOnly {
				o = append(o, "ro")
			}
			ms = append(ms, specs.Mount{Destination: m.Target, Type: "tmpfs", Source: "tmpfs", Options: o})
		}
	}
	if spec.SiloTools {
		ms = append(ms, specs.Mount{Destination: runtime.ToolsPath, Type: "bind", Source: d.cfg.SiloTools, Options: []string{"rbind", "ro"}})
	}
	if d.cfg.Toolbox != "" {
		ms = append(ms, specs.Mount{Destination: ToolboxPath, Type: "bind", Source: d.cfg.Toolbox, Options: []string{"rbind", "ro"}})
	}
	opts = append(opts, oci.WithMounts(ms))
	cg := ""
	if d.cfg.CgroupsPath != nil {
		cg = d.cfg.CgroupsPath(spec)
	}
	patch := rootless.SandboxPatch(nsd.ContainerNS{User: spec.UserNS, Net: spec.NetNS,
		Range: nsd.Range{Outside: spec.Root.Outside, Size: spec.Root.Size}}, cg)
	opts = append(opts, withPatch(patch))
	return opts, nil
}

// withPatch applies a rootless.Patch to the spec being built.
func withPatch(p rootless.Patch) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if b, err = p.Apply(b); err != nil {
			return err
		}
		var out oci.Spec
		if err := json.Unmarshal(b, &out); err != nil {
			return err
		}
		*s = out
		return nil
	}
}

// numericUser parses a Docker user, "uid[:gid]", "root" or "". Names
// would need the image's /etc/passwd, which only the holder can mount;
// the facade resolves them first.
func numericUser(u string) (uint32, uint32, error) {
	if u == "" || u == "root" {
		return 0, 0, nil
	}
	us, gs, hasG := strings.Cut(u, ":")
	if us == "root" {
		us = "0"
	}
	uid, err := strconv.ParseUint(us, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("gvisor: user %q is not numeric", u)
	}
	gid := uid
	if hasG {
		if gs == "root" {
			gs = "0"
		}
		if gid, err = strconv.ParseUint(gs, 10, 32); err != nil {
			return 0, 0, fmt.Errorf("gvisor: group in %q is not numeric", u)
		}
	}
	return uint32(uid), uint32(gid), nil
}

// fixedIO is a task's stdio as FIFO paths that the log copier, not
// silod, reads.
type fixedIO struct{ cfg cio.Config }

func (f fixedIO) Config() cio.Config { return f.cfg }
func (fixedIO) Cancel()              {}
func (fixedIO) Wait()                {}
func (fixedIO) Close() error         { return nil }

func (d *Driver) pidFile(id string) string { return filepath.Join(d.dir(id), "copier.pid") }

// copierAlive is whether a container's log copier is running.
func (d *Driver) copierAlive(id string) bool {
	b, err := os.ReadFile(d.pidFile(id))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return alive(pid)
}

// startCopier starts a container's log copier in a session of its own,
// so that it outlives silod.
func (d *Driver) startCopier(id string, ioc cio.Config) error {
	args := append(append([]string{}, d.cfg.Logger[1:]...), "--log", d.LogPath(id), "--pid", d.pidFile(id), "--stdout", ioc.Stdout)
	if ioc.Stderr != "" {
		args = append(args, "--stderr", ioc.Stderr)
	}
	if ioc.Stdin != "" {
		args = append(args, "--stdin", ioc.Stdin)
	}
	cmd := exec.Command(d.cfg.Logger[0], args...)
	cmd.Env = append(os.Environ(), d.cfg.LoggerEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// What the copier itself says goes beside the log, for silod to
	// read when the log stays empty.
	if f, err := os.OpenFile(filepath.Join(d.dir(id), "copier.err"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("gvisor: log copier: %w", err)
	}
	// Reaped here while silod lives; init (or the subreaper) reaps it
	// after.
	go cmd.Wait()
	// The pid file is the copier's; wait for it, so that an adoption
	// right after sees it.
	for i := 0; i < 100 && !d.copierAlive(id); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// load is a container and its task, or a nil task when it has none.
func (d *Driver) load(ctx context.Context, id string) (containerd.Container, containerd.Task, error) {
	c, err := d.client.LoadContainer(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil, nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	}
	if err != nil {
		return nil, nil, err
	}
	if l, err := c.Labels(ctx); err != nil || l[LabelClass] != string(runtime.Sandbox) {
		return nil, nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	}
	t, err := c.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return c, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return c, t, nil
}

func (d *Driver) task(ctx context.Context, id string) (containerd.Task, error) {
	_, t, err := d.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("%w: %s has no sandbox", runtime.ErrState, id)
	}
	return t, nil
}

func state(ctx context.Context, t containerd.Task) (containerd.Status, error) {
	if t == nil {
		return containerd.Status{Status: containerd.Stopped}, nil
	}
	return t.Status(ctx)
}

// stateErr maps containerd's refusals to runtime.ErrState.
func stateErr(err error) error {
	if errdefs.IsFailedPrecondition(err) || errdefs.IsNotFound(err) || errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("%w: %v", runtime.ErrState, err)
	}
	return err
}

// Start implements runtime.Driver.
func (d *Driver) Start(ctx context.Context, id string) error {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return err
	}
	if st, err := t.Status(ctx); err != nil {
		return err
	} else if st.Status != containerd.Created {
		return fmt.Errorf("%w: %s is %s", runtime.ErrState, id, st.Status)
	}
	return stateErr(t.Start(ctx))
}

// Stop implements runtime.Driver: the stop signal, then SIGKILL after
// timeout.
func (d *Driver) Stop(ctx context.Context, id string, timeout time.Duration) error {
	ctx = d.ns(ctx)
	c, t, err := d.load(ctx, id)
	if err != nil {
		return err
	}
	st, err := state(ctx, t)
	if err != nil {
		return err
	}
	switch st.Status {
	case containerd.Stopped:
		return nil
	case containerd.Paused, containerd.Pausing:
		if err := t.Resume(ctx); err != nil {
			return err
		}
	}
	exit, err := t.Wait(ctx)
	if err != nil {
		return err
	}
	sig := syscall.SIGTERM
	if l, err := c.Labels(ctx); err == nil {
		if n, err := strconv.Atoi(l[LabelStopSignal]); err == nil && n > 0 {
			sig = syscall.Signal(n)
		}
	}
	if st.Status == containerd.Created {
		sig = syscall.SIGKILL
	}
	if err := t.Kill(ctx, sig); err != nil && !errdefs.IsNotFound(err) {
		return stateErr(err)
	}
	select {
	case <-exit:
		return nil
	case <-time.After(timeout):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := t.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return stateErr(err)
	}
	select {
	case <-exit:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill implements runtime.Driver.
func (d *Driver) Kill(ctx context.Context, id string, sig syscall.Signal) error {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return err
	}
	if st, err := t.Status(ctx); err != nil {
		return err
	} else if st.Status != containerd.Running {
		return fmt.Errorf("%w: %s is %s", runtime.ErrState, id, st.Status)
	}
	return stateErr(t.Kill(ctx, sig))
}

// Pause implements runtime.Driver.
func (d *Driver) Pause(ctx context.Context, id string) error {
	return d.move(ctx, id, containerd.Running, func(ctx context.Context, t containerd.Task) error { return t.Pause(ctx) })
}

// Resume implements runtime.Driver.
func (d *Driver) Resume(ctx context.Context, id string) error {
	return d.move(ctx, id, containerd.Paused, func(ctx context.Context, t containerd.Task) error { return t.Resume(ctx) })
}

func (d *Driver) move(ctx context.Context, id string, from containerd.ProcessStatus, fn func(context.Context, containerd.Task) error) error {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return err
	}
	if st, err := t.Status(ctx); err != nil {
		return err
	} else if st.Status != from {
		return fmt.Errorf("%w: %s is %s", runtime.ErrState, id, st.Status)
	}
	return stateErr(fn(ctx, t))
}

// Delete implements runtime.Driver: a created or stopped container, its
// sandbox, snapshot, FIFOs and log.
func (d *Driver) Delete(ctx context.Context, id string) error {
	ctx = d.ns(ctx)
	c, t, err := d.load(ctx, id)
	if err != nil {
		return err
	}
	st, err := state(ctx, t)
	if err != nil {
		return err
	}
	switch st.Status {
	case containerd.Created:
		// A created sandbox has nothing running in it to lose.
		if err := t.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsFailedPrecondition(err) {
			return err
		}
	case containerd.Stopped:
	default:
		return fmt.Errorf("%w: %s is %s", runtime.ErrState, id, st.Status)
	}
	return d.destroy(ctx, c)
}

func (d *Driver) destroy(ctx context.Context, c containerd.Container) error {
	if t, err := c.Task(ctx, nil); err == nil {
		if _, err := t.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	// The copier ends when the shim closes the FIFOs; one left behind
	// (it never saw a writer) is killed.
	if b, err := os.ReadFile(d.pidFile(c.ID())); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return os.RemoveAll(d.dir(c.ID()))
}

// Wait implements runtime.Driver.
func (d *Driver) Wait(ctx context.Context, id string) (runtime.ExitStatus, error) {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	ch, err := t.Wait(ctx)
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	select {
	case s := <-ch:
		return exitStatus(s)
	case <-ctx.Done():
		return runtime.ExitStatus{}, ctx.Err()
	}
}

func exitStatus(s containerd.ExitStatus) (runtime.ExitStatus, error) {
	code, at, err := s.Result()
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	e := runtime.ExitStatus{Code: int(code), Finished: at}
	// runsc reports a signalled workload as 128+signal, as a shell
	// does.
	if code > 128 && code <= 128+64 {
		e.Signal = syscall.Signal(code - 128)
	}
	return e, nil
}

// Exec implements runtime.Driver: p in the container, with the
// container's environment under p's, its user and workdir unless p
// names its own.
func (d *Driver) Exec(ctx context.Context, id string, p runtime.Process, stdio runtime.ExecIO) (int, error) {
	ctx = d.ns(ctx)
	c, t, err := d.load(ctx, id)
	if err != nil {
		return 0, err
	}
	if st, err := state(ctx, t); err != nil {
		return 0, err
	} else if st.Status != containerd.Running {
		return 0, fmt.Errorf("%w: %s is %s", runtime.ErrState, id, st.Status)
	}
	if len(p.Args) == 0 {
		return 0, errors.New("gvisor: exec of nothing")
	}
	s, err := c.Spec(ctx)
	if err != nil {
		return 0, err
	}
	ps := *s.Process
	ps.Args = p.Args
	ps.Env = append(append([]string{}, s.Process.Env...), p.Env...)
	ps.Terminal = p.TTY
	ps.ConsoleSize = nil
	if p.Workdir != "" {
		ps.Cwd = p.Workdir
	}
	if p.User != "" {
		uid, gid, err := numericUser(p.User)
		if err != nil {
			return 0, err
		}
		ps.User = specs.User{UID: uid, GID: gid}
	}
	// The shim holds the stdin FIFO open itself until CloseIO, so the
	// process sees EOF only once it is asked for, after the copy.
	var stdin io.Reader
	stdinDone := make(chan struct{})
	if p.Stdin && stdio.Stdin != nil {
		stdin = &eofReader{r: stdio.Stdin, done: stdinDone}
	}
	streams := []cio.Opt{cio.WithStreams(stdin, stdio.Stdout, stdio.Stderr), cio.WithFIFODir(d.dir(id))}
	if p.TTY {
		streams = append(streams, cio.WithTerminal)
	}
	execID := fmt.Sprintf("exec-%d-%d", time.Now().UnixNano(), d.execs.Add(1))
	proc, err := t.Exec(ctx, execID, &ps, cio.NewCreator(streams...))
	if err != nil {
		return 0, stateErr(err)
	}
	defer proc.Delete(context.WithoutCancel(ctx))
	exit, err := proc.Wait(ctx)
	if err != nil {
		return 0, err
	}
	if err := proc.Start(ctx); err != nil {
		return 0, err
	}
	if stdin != nil {
		go func() {
			select {
			case <-stdinDone:
				proc.CloseIO(ctx, containerd.WithStdinCloser)
			case <-ctx.Done():
			}
		}()
	}
	if p.TTY && stdio.Resize != nil {
		go func() {
			for sz := range stdio.Resize {
				proc.Resize(ctx, uint32(sz[0]), uint32(sz[1]))
			}
		}()
	}
	select {
	case s := <-exit:
		// The output is copied by the time the process's IO closes.
		proc.IO().Wait()
		code, _, err := s.Result()
		return int(code), err
	case <-ctx.Done():
		proc.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		return 0, ctx.Err()
	}
}

// eofReader closes done when r is exhausted.
type eofReader struct {
	r    io.Reader
	done chan struct{}
	once sync.Once
}

func (e *eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err != nil {
		e.once.Do(func() { close(e.done) })
	}
	return n, err
}

// Streams implements runtime.Driver: the workload's output so far, from
// its log file, and its stdin when it asked for one.
func (d *Driver) Streams(ctx context.Context, id string) (runtime.Streams, error) {
	c, _, err := d.load(d.ns(ctx), id)
	if err != nil {
		return runtime.Streams{}, err
	}
	out, errb, err := ReadLog(d.LogPath(c.ID()))
	if err != nil {
		return runtime.Streams{}, err
	}
	s := runtime.Streams{Stdout: io.NopCloser(strings.NewReader(string(out))), Stderr: io.NopCloser(strings.NewReader(string(errb)))}
	if in := d.fifo(id, "stdin"); fileExists(in) {
		// The copier holds the FIFO open, so this doesn't block.
		f, err := os.OpenFile(in, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return runtime.Streams{}, err
		}
		// Writes block as on a pipe from here on.
		unix.SetNonblock(int(f.Fd()), false)
		s.Stdin = f
	}
	return s, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Stats implements runtime.Driver, from the shim's cgroup metrics.
func (d *Driver) Stats(ctx context.Context, id string) (runtime.Stats, error) {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return runtime.Stats{}, err
	}
	return TaskStats(ctx, t)
}

// TaskStats is a task's use, from its shim's cgroup metrics: zero once
// it has stopped. The vm class's driver reads its VM containers' the
// same way.
func TaskStats(ctx context.Context, t containerd.Task) (runtime.Stats, error) {
	st := runtime.Stats{SampledAt: time.Now()}
	if s, err := t.Status(ctx); err != nil || (s.Status != containerd.Running && s.Status != containerd.Paused) {
		return st, err
	}
	m, err := t.Metrics(ctx)
	if err != nil {
		return st, fmt.Errorf("metrics: %w", err)
	}
	v, err := typeurl.UnmarshalAny(m.Data)
	if err != nil {
		return st, fmt.Errorf("metrics: %w", err)
	}
	switch m := v.(type) {
	case *stats.Metrics:
		if c := m.GetCPU().GetUsage(); c != nil {
			st.CPUNanos = c.Total
		}
		if mem := m.GetMemory().GetUsage(); mem != nil {
			st.MemoryBytes = mem.Usage
		}
		for _, e := range m.GetBlkio().GetIoServiceBytesRecursive() {
			switch strings.ToLower(e.Op) {
			case "read":
				st.ReadBytes += e.Value
			case "write":
				st.WriteBytes += e.Value
			}
		}
		st.Processes = m.GetPids().GetCurrent()
	case *stats2.Metrics:
		st.CPUNanos = m.GetCPU().GetUsageUsec() * 1000
		st.MemoryBytes = m.GetMemory().GetUsage()
		for _, e := range m.GetIo().GetUsage() {
			st.ReadBytes += e.Rbytes
			st.WriteBytes += e.Wbytes
		}
		st.Processes = m.GetPids().GetCurrent()
	}
	if st.Processes == 0 {
		if ps, err := t.Pids(ctx); err == nil {
			st.Processes = uint64(len(ps))
		}
	}
	return st, nil
}

// tar is the tar command in a container: the toolbox's, or the image's.
func (d *Driver) tar(args ...string) []string {
	if d.cfg.Toolbox != "" {
		return append([]string{ToolboxPath, "tar"}, args...)
	}
	return append([]string{"tar"}, args...)
}

// CopyIn implements runtime.Driver, by tar in the container: with
// --overlay2=root:self, the container's root as the workload sees it
// is in the sandbox, not on the host. It runs as the container's root.
func (d *Driver) CopyIn(ctx context.Context, id, dir string, r io.Reader) error {
	if !path.IsAbs(dir) {
		return fmt.Errorf("gvisor: copy into %q, not an absolute path", dir)
	}
	var stderr strings.Builder
	code, err := d.Exec(ctx, id, runtime.Process{Args: d.tar("-x", "-f", "-", "-C", dir), User: "0", Stdin: true},
		runtime.ExecIO{Stdin: r, Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("gvisor: extracting into %s: %s", dir, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CopyOut implements runtime.Driver, by tar in the container.
func (d *Driver) CopyOut(ctx context.Context, id, p string) (io.ReadCloser, error) {
	if !path.IsAbs(p) {
		return nil, fmt.Errorf("gvisor: copy from %q, not an absolute path", p)
	}
	p = path.Clean(p)
	if _, _, err := d.load(d.ns(ctx), id); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		var stderr strings.Builder
		code, err := d.Exec(ctx, id, runtime.Process{Args: d.tar("-c", "-f", "-", "-C", path.Dir(p), path.Base(p)), User: "0"},
			runtime.ExecIO{Stdout: pw, Stderr: &stderr})
		if err == nil && code != 0 {
			err = fmt.Errorf("gvisor: archiving %s: %w: %s", p, os.ErrNotExist, strings.TrimSpace(stderr.String()))
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

var _ runtime.Committer = (*Driver)(nil)

// RootfsUpper implements runtime.Committer: `runsc tar rootfs-upper`,
// which with --overlay2=root:self is the only reader of the container's
// writes, in the sandbox's memory. `silod runsc` runs it inside the
// holder, in the container's user namespace as its root, as the shim
// runs runsc, given the task's bundle since runsc tar names none. The
// tar is streamed through a pipe runsc's --file names. runsc
// release-20261005 writes deletions as overlayfs whiteouts
// (local-registry.md §7, "commit"). A container whose sandbox has
// exited has no upper layer left, and is ErrState.
func (d *Driver) RootfsUpper(ctx context.Context, id string) (io.ReadCloser, error) {
	if d.cfg.HolderCmd == nil || len(d.cfg.Runsc) == 0 || d.cfg.RunscRoot == "" {
		return nil, errors.New("gvisor: reading a writable layer needs `silod runsc` and runsc's root, and this driver was given neither")
	}
	nctx := d.ns(ctx)
	t, err := d.task(nctx, id)
	if err != nil {
		return nil, err
	}
	if s, err := t.Status(nctx); err != nil {
		return nil, err
	} else if s.Status != containerd.Running && s.Status != containerd.Paused {
		return nil, fmt.Errorf("%w: %s is %s, and its writable layer went with its sandbox", runtime.ErrState, id, s.Status)
	}
	// runsc prints a line on stdout before the tar, so the tar has a
	// pipe of its own, which runsc reopens by its path.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := append(append([]string(nil), d.cfg.Runsc...), "--bundle", rootless.Paths{}.TaskBundle(d.cfg.Namespace, id), "--",
		"--root", filepath.Join(d.cfg.RunscRoot, d.cfg.Namespace), "tar", "rootfs-upper", "--file", "/proc/self/fd/3", id)
	cmd := d.cfg.HolderCmd(args...)
	cmd.ExtraFiles = []*os.File{pw}
	r := &cmdReader{cmd: cmd, r: pr}
	cmd.Stdout, cmd.Stderr = &r.msgs, &r.msgs
	err = cmd.Start()
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, fmt.Errorf("gvisor: runsc tar rootfs-upper: %w", err)
	}
	r.stop = context.AfterFunc(ctx, func() { cmd.Process.Kill() })
	return r, nil
}

// cmdReader is a command's output pipe, which fails with the command's
// messages if the command does; Close kills it if it is still running.
type cmdReader struct {
	cmd  *exec.Cmd
	r    io.ReadCloser
	msgs bytes.Buffer
	stop func() bool
	once sync.Once
	err  error
}

func (c *cmdReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if err == io.EOF {
		if werr := c.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *cmdReader) wait() error {
	c.once.Do(func() {
		c.stop()
		if err := c.cmd.Wait(); err != nil {
			c.err = fmt.Errorf("gvisor: runsc tar rootfs-upper: %v: %s", err, strings.TrimSpace(c.msgs.String()))
		}
	})
	return c.err
}

func (c *cmdReader) Close() error {
	c.cmd.Process.Kill()
	c.wait()
	return c.r.Close()
}

// List implements runtime.Driver: every sandbox-class container in
// silo-containerd, whether this silod or an earlier one made it.
func (d *Driver) List(ctx context.Context) ([]runtime.Container, error) {
	ctx = d.ns(ctx)
	cs, err := d.client.Containers(ctx, fmt.Sprintf("labels.%q==%s", LabelClass, runtime.Sandbox))
	if err != nil {
		return nil, err
	}
	var out []runtime.Container
	for _, c := range cs {
		info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			continue
		}
		ct := runtime.Container{ID: c.ID(), Class: runtime.Sandbox, State: runtime.Stopped, Labels: map[string]string{}}
		for k, v := range info.Labels {
			if k != LabelClass && k != LabelStopSignal {
				ct.Labels[k] = v
			}
		}
		if t, err := c.Task(ctx, nil); err == nil {
			if st, err := t.Status(ctx); err == nil {
				switch st.Status {
				case containerd.Created:
					ct.State = runtime.Created
				case containerd.Running:
					ct.State = runtime.Running
				case containerd.Paused, containerd.Pausing:
					ct.State = runtime.Paused
				default:
					e := runtime.ExitStatus{Code: int(st.ExitStatus), Finished: st.ExitTime}
					if e.Code > 128 && e.Code <= 128+64 {
						e.Signal = syscall.Signal(e.Code - 128)
					}
					ct.Exit = &e
				}
			}
		}
		out = append(out, ct)
	}
	return out, nil
}

// Adopt takes over the containers an earlier silod left: List reports
// them as it would its own, and Adopt restarts the log copier of any
// with a live sandbox whose copier died, so the workload's output isn't
// held in a full pipe. It returns the containers it found.
func (d *Driver) Adopt(ctx context.Context) ([]runtime.Container, error) {
	cs, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.State == runtime.Stopped || d.copierAlive(c.ID) {
			continue
		}
		ioc := cio.Config{Stdout: d.fifo(c.ID, "stdout")}
		if fileExists(d.fifo(c.ID, "stderr")) {
			ioc.Stderr = d.fifo(c.ID, "stderr")
		}
		if fileExists(d.fifo(c.ID, "stdin")) {
			ioc.Stdin = d.fifo(c.ID, "stdin")
		}
		if !fileExists(ioc.Stdout) {
			continue
		}
		if err := d.startCopier(c.ID, ioc); err != nil {
			return cs, err
		}
	}
	return cs, nil
}
