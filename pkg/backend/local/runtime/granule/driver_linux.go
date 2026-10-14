package granule

import (
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

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
)

// LabelClass is the label the driver adds to silod's own, as the
// sandbox class's driver does: dev.silo.class=vm.
const LabelClass = gvisor.LabelClass

// DefaultSnapshotter is the snapshotter the VM containers' roots, the
// granule image, are made by.
const DefaultSnapshotter = gvisor.DefaultSnapshotter

// GranuleBin is the granule CLI in the granule image, a link to
// granule-shim, whose `granule exec` runs a command in the guest.
const GranuleBin = "/usr/local/bin/granule"

// Config is how the driver reaches silo-containerd, the holder and
// granule's parts.
type Config struct {
	// Address is silo-containerd's socket; Namespace is
	// rootless.ContainerdNamespace when empty.
	Address, Namespace string
	// Crun is crun's path, which the runc shim (rootless.RuntimeCrun)
	// runs as its runc (call L31).
	Crun string
	// Dir holds each container's directory, mounted at IODir: its
	// FIFOs, exit record, virtiofsd sockets and log. The shim and the
	// holder open them, so it must name the same directory there as on
	// the host, as gvisor.Config.Dir must.
	Dir string
	// Wraps is the wraps directory, one guest half per image digest
	// (WrapDir).
	Wraps string
	// Volumes is the storage driver's root (runtime.Spec.Validate).
	Volumes string
	// Image is the pinned granule image, by its name in silo-containerd:
	// every VM container's root, and the wrap's parts and kernel.
	// GuestImage is granule's guest rootfs image, by its name there.
	Image, GuestImage string
	// Granule is the granule binary on the host, which wraps.
	Granule string
	// Virtiofsd is virtiofsd's path; without it, a container with a
	// volume is refused.
	Virtiofsd string
	// DNS is the address containers' guests resolve names at.
	DNS func(runtime.Spec) string
	// Logger is the command, argv, that runs gvisor.LogMain: silod and
	// gvisor.LogArg. LoggerEnv is added to its environment.
	Logger    []string
	LoggerEnv []string
	// InHolder runs silod with args inside the holder and returns its
	// combined output (gvisor.InHolder): the wrap, the snapshot's remap
	// and the virtiofsds run through it.
	InHolder func(ctx context.Context, args ...string) ([]byte, error)
	// CgroupsPath gives a container's linux.cgroupsPath, or "" for none.
	CgroupsPath func(runtime.Spec) string
	// KVMGID is rootless.VMPatch's: nil for crun's keep_original_groups.
	KVMGID *uint32
	// Devices are the host's /dev/kvm and /dev/net/tun, for the probe.
	Devices HostDevices
	// ProbeSpec, if set, gives the probe a spec for its silo-probe VM,
	// with namespaces made and wired for it, and undoes them after.
	// Without it, the probe stops short of booting a VM.
	ProbeSpec func(ctx context.Context, id string) (runtime.Spec, func(), error)
	// Snapshotter is DefaultSnapshotter when empty.
	Snapshotter string
}

// Driver is the vm class.
type Driver struct {
	cfg    Config
	client *containerd.Client
	execs  atomic.Uint64

	// booted is the silo-probe VM's verdict, once it has one: a host
	// that booted a VM once keeps passing until silod restarts.
	probeMu sync.Mutex
	booted  *runtime.Probe
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
	if cfg.Devices == (HostDevices{}) {
		cfg.Devices = DefaultHostDevices
	}
	if cfg.Dir == "" || cfg.Wraps == "" || cfg.Address == "" || len(cfg.Logger) == 0 || cfg.InHolder == nil ||
		cfg.Image == "" || cfg.GuestImage == "" || cfg.Granule == "" || cfg.Crun == "" || cfg.DNS == nil {
		return nil, errors.New("granule: a driver needs containerd's address, its directories, a log copier, the holder, crun, granule, its images and DNS")
	}
	for _, d := range []string{cfg.Dir, cfg.Wraps} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	c, err := containerd.New(cfg.Address, containerd.WithDefaultNamespace(cfg.Namespace), containerd.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("granule: silo-containerd: %w", err)
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
func (d *Driver) Class() runtime.Class { return runtime.VM }

// Probe implements runtime.Driver (local-backend.md §3, "Detection"):
// KVM and the tun device work (ProbeHost), silo-containerd answers, crun,
// granule and the granule image are there, and then, once, a silo-probe
// VM boots from the pinned granule image and runs `true`. The boot is
// what catches a /dev/kvm that creates a VM but can't run one.
func (d *Driver) Probe(ctx context.Context) runtime.Probe {
	if p := ProbeHost(d.cfg.Devices); !p.OK {
		return p
	}
	nctx := d.ns(ctx)
	if _, err := d.client.Version(nctx); err != nil {
		return runtime.Probe{Why: "silo-containerd doesn't answer at " + d.cfg.Address + ": " + err.Error()}
	}
	for _, b := range []struct{ what, path string }{{"crun", d.cfg.Crun}, {"granule", d.cfg.Granule}} {
		if _, err := exec.LookPath(b.path); err != nil {
			return runtime.Probe{Why: b.what + ": " + err.Error()}
		}
	}
	for _, name := range []string{d.cfg.Image, d.cfg.GuestImage} {
		if _, err := d.client.GetImage(nctx, name); err != nil {
			return runtime.Probe{Why: fmt.Sprintf("the vm class's image %s is not in silo-containerd: %v", name, err)}
		}
	}
	if d.cfg.ProbeSpec == nil {
		return runtime.Probe{OK: true}
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.booted != nil && d.booted.OK {
		return *d.booted
	}
	p := d.bootProbe(ctx)
	d.booted = &p
	return p
}

// ProbeID is the silo-probe VM's container id.
const ProbeID = "silo-probe"

// bootProbe boots the silo-probe VM: granule's guest rootfs image,
// wrapped, running `true`, which must report exit 0.
func (d *Driver) bootProbe(ctx context.Context) runtime.Probe {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	fail := func(f string, a ...any) runtime.Probe {
		return runtime.Probe{Why: "a silo-probe VM didn't boot: " + fmt.Sprintf(f, a...)}
	}
	img, err := d.client.GetImage(d.ns(ctx), d.cfg.GuestImage)
	if err != nil {
		return fail("%v", err)
	}
	spec, undo, err := d.cfg.ProbeSpec(ctx, ProbeID)
	if err != nil {
		return fail("its namespaces: %v", err)
	}
	defer undo()
	spec.ID, spec.Image = ProbeID, img.Target().Digest.String()
	if spec.Process.Args == nil {
		spec.Process.Args = []string{"true"}
	}
	// One left by a silod that died mid-probe.
	d.Stop(ctx, ProbeID, time.Second)
	d.Delete(ctx, ProbeID)
	if err := d.Create(ctx, spec); err != nil {
		return fail("%v", err)
	}
	defer d.Delete(context.WithoutCancel(ctx), ProbeID)
	if err := d.Start(ctx, ProbeID); err != nil {
		return fail("%v", err)
	}
	e, err := d.Wait(ctx, ProbeID)
	if err != nil {
		d.Kill(context.WithoutCancel(ctx), ProbeID, syscall.SIGKILL)
		return fail("%v", err)
	}
	if e.Code != 0 {
		out, errb, _ := gvisor.ReadLog(d.LogPath(ProbeID))
		return fail("`true` exited %d: %s", e.Code, strings.TrimSpace(string(out)+string(errb)+d.shimLog(ProbeID)))
	}
	return runtime.Probe{OK: true}
}

// image finds an image by its digest: its manifest's or index's, or its
// config's.
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

// Prepare implements runtime.Driver: the image's wrap, `silod
// runtime-wrap` through the holder, once per digest.
func (d *Driver) Prepare(ctx context.Context, image string) error {
	if ok, err := Wrapped(d.cfg.Wraps, image); err != nil || ok {
		return err
	}
	ctx = d.ns(ctx)
	img, err := d.image(ctx, image)
	if err != nil {
		return err
	}
	out, err := d.cfg.InHolder(ctx, WrapArg, "--address", d.cfg.Address, "--namespace", d.cfg.Namespace,
		"--snapshotter", d.cfg.Snapshotter, "--image", img.Name(), "--digest", image,
		"--guest-image", d.cfg.GuestImage, "--granule-image", d.cfg.Image, "--granule", d.cfg.Granule, "--wraps", d.cfg.Wraps)
	if err != nil {
		return fmt.Errorf("granule: wrapping %s: %v: %s", image, err, strings.TrimSpace(string(out)))
	}
	if ok, err := Wrapped(d.cfg.Wraps, image); err != nil || !ok {
		return fmt.Errorf("granule: wrapping %s left no guest half: %v", image, err)
	}
	return nil
}

// dir is a container's directory, mounted at IODir.
func (d *Driver) dir(id string) string { return filepath.Join(d.cfg.Dir, id) }

// LogPath is a container's log file, in Docker's json-file format. It
// is beside the container's directory, not in it, so the guest's side
// can't write it.
func (d *Driver) LogPath(id string) string { return filepath.Join(d.cfg.Dir, id+".log") }

func (d *Driver) pidFile(id string) string { return filepath.Join(d.cfg.Dir, id+".copier.pid") }

func (d *Driver) fifo(id, name string) string { return filepath.Join(d.dir(id), name) }

// shimLog is what granule-shim said on its own stdout and stderr, which
// the task's IO puts beside the log.
func (d *Driver) shimLog(id string) string {
	b, _ := os.ReadFile(filepath.Join(d.cfg.Dir, id+".shim.log"))
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(b)
}

// Create implements runtime.Driver. The container's netns and user
// namespace exist and are wired already. Create wraps the image if it
// isn't, remaps the granule image's snapshot to the container's range,
// makes its directory, FIFOs and log copier, starts its virtiofsds, and
// makes the task: crun creates the VM container, whose granule-shim
// boots the VM when the task starts.
func (d *Driver) Create(ctx context.Context, spec runtime.Spec) (err error) {
	if err := spec.Validate(d.cfg.Volumes); err != nil {
		return err
	}
	if spec.UserNS == "" || spec.Root.Outside == 0 || spec.Root.Size == 0 {
		return errors.New("granule: the vm class runs its VM container in the container's own user namespace, and the spec names none")
	}
	if spec.SiloTools {
		return errors.New("granule: the vm class's tool containers carry silo-tools in their image (runtime.ToolsPath), and mount none")
	}
	ctx = d.ns(ctx)
	if _, err := d.client.LoadContainer(ctx, spec.ID); err == nil {
		return fmt.Errorf("granule: container %s exists", spec.ID)
	}
	if err := d.Prepare(ctx, spec.Image); err != nil {
		return err
	}
	v, err := NewVM(spec, WrapDir(d.cfg.Wraps, spec.Image), d.dir(spec.ID), d.cfg.DNS(spec))
	if err != nil {
		return err
	}
	if len(v.Shares) > 0 && d.cfg.Virtiofsd == "" {
		return errors.New("granule: volumes need virtiofsd, and the driver was given none")
	}
	cg := ""
	if d.cfg.CgroupsPath != nil {
		cg = d.cfg.CgroupsPath(spec)
	}
	opts, err := SpecOpts(v, spec, cg, d.cfg.KVMGID)
	if err != nil {
		return err
	}
	runtimeImg, err := d.client.GetImage(ctx, d.cfg.Image)
	if err != nil {
		return fmt.Errorf("granule: the granule image: %w", err)
	}

	// The lease keeps the remapped snapshot from containerd's GC until
	// the container holds it.
	ctx, done, err := d.client.WithLease(ctx)
	if err != nil {
		return err
	}
	defer done(context.WithoutCancel(ctx))
	lease, _ := leases.FromContext(ctx)
	if out, err := d.cfg.InHolder(ctx, gvisor.RemapArg, "--address", d.cfg.Address, "--namespace", d.cfg.Namespace,
		"--lease", lease, "--image", runtimeImg.Name(), "--snapshotter", d.cfg.Snapshotter, "--key", spec.ID,
		"--outside", strconv.FormatUint(uint64(spec.Root.Outside), 10), "--size", strconv.FormatUint(uint64(spec.Root.Size), 10)); err != nil {
		return fmt.Errorf("granule: remapping %s's snapshot: %v: %s", spec.ID, err, strings.TrimSpace(string(out)))
	}
	labels := map[string]string{LabelClass: string(runtime.VM)}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	c, err := d.client.NewContainer(ctx, spec.ID,
		containerd.WithSnapshotter(d.cfg.Snapshotter),
		containerd.WithSnapshot(spec.ID),
		containerd.WithImageName(runtimeImg.Name()),
		containerd.WithContainerLabels(labels),
		containerd.WithRuntime(rootless.RuntimeCrun, &runcoptions.Options{BinaryName: d.cfg.Crun}),
		containerd.WithNewSpec(opts...),
	)
	if err != nil {
		d.client.SnapshotService(d.cfg.Snapshotter).Remove(ctx, spec.ID)
		return fmt.Errorf("granule: %w", err)
	}
	defer func() {
		if err != nil {
			d.destroy(context.WithoutCancel(ctx), c)
		}
	}()
	if err := d.makeDir(v); err != nil {
		return err
	}
	ioc := cio.Config{Stdout: d.fifo(spec.ID, "stdout")}
	if !v.TTY {
		ioc.Stderr = d.fifo(spec.ID, "stderr")
	}
	if v.Stdin {
		ioc.Stdin = d.fifo(spec.ID, "stdin")
	}
	if err := d.startCopier(spec.ID, ioc); err != nil {
		return err
	}
	vfs := Virtiofsd{Binary: d.cfg.Virtiofsd, Start: d.startVirtiofsd}
	if err := vfs.StartAll(ctx, v, spec.UserNS); err != nil {
		return err
	}
	// granule-shim's own output, which isn't the workload's, goes to a
	// file of its own; the workload's streams come through
	// GRANULE_WORKLOAD_*.
	shimLog := filepath.Join(d.cfg.Dir, spec.ID+".shim.log")
	if _, err := c.NewTask(ctx, cio.LogFile(shimLog)); err != nil {
		return fmt.Errorf("granule: creating %s's VM container: %w", spec.ID, err)
	}
	return nil
}

// makeDir makes a container's directory and its FIFOs. The VM
// container's root is a subuid silod can't chown to, so the directory
// and what is in it are open to every user: the data directory above
// it, 0700, is what keeps the host's other users out, and no other
// container mounts it.
func (d *Driver) makeDir(v VM) error {
	for _, dir := range []string{v.Dir, filepath.Join(v.Dir, "fs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			return err
		}
	}
	names := []string{"stdout", "stderr"}
	if v.TTY {
		names = []string{"stdout", "resize"}
	}
	if v.Stdin {
		names = append(names, "stdin")
	}
	for _, n := range names {
		f := filepath.Join(v.Dir, n)
		if err := unix.Mkfifo(f, 0o666); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("granule: %s: %w", f, err)
		}
		if err := os.Chmod(f, 0o666); err != nil {
			return err
		}
	}
	return nil
}

// startVirtiofsd is Virtiofsd.Start: `silod runtime-virtiofsd` through
// the holder, which starts virtiofsd inside the container's user
// namespace and leaves it running.
func (d *Driver) startVirtiofsd(ctx context.Context, userns string, argv []string) error {
	args := append([]string{VirtiofsdArg, "--userns", userns, "--"}, argv...)
	if out, err := d.cfg.InHolder(ctx, args...); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopVirtiofsds stops a container's virtiofsds, through the holder.
func (d *Driver) stopVirtiofsds(ctx context.Context, id string) error {
	fs := filepath.Join(d.dir(id), "fs")
	if m, _ := filepath.Glob(filepath.Join(fs, "*.pid")); len(m) == 0 {
		return nil
	}
	if out, err := d.cfg.InHolder(ctx, VirtiofsdArg, "--stop", fs); err != nil {
		return fmt.Errorf("granule: stopping %s's virtiofsds: %v: %s", id, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// copierAlive is whether a container's log copier is running.
func (d *Driver) copierAlive(id string) bool {
	b, err := os.ReadFile(d.pidFile(id))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// startCopier starts a container's log copier, gvisor.LogMain, in a
// session of its own, so that it outlives silod. It reads the FIFOs
// granule-shim writes the workload's streams to.
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
	if f, err := os.OpenFile(filepath.Join(d.cfg.Dir, id+".copier.err"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("granule: log copier: %w", err)
	}
	go cmd.Wait()
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
	if l, err := c.Labels(ctx); err != nil || l[LabelClass] != string(runtime.VM) {
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
		return nil, fmt.Errorf("%w: %s has no VM container", runtime.ErrState, id)
	}
	return t, nil
}

func state(ctx context.Context, t containerd.Task) (containerd.Status, error) {
	if t == nil {
		return containerd.Status{Status: containerd.Stopped}, nil
	}
	return t.Status(ctx)
}

func stateErr(err error) error {
	if errdefs.IsFailedPrecondition(err) || errdefs.IsNotFound(err) || errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("%w: %v", runtime.ErrState, err)
	}
	return err
}

// Start implements runtime.Driver: granule-shim boots the VM.
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

// Stop implements runtime.Driver. SIGTERM to granule-shim is the VM's
// power button: the guest is asked to shut down, and granule-shim gives
// it CHV_SHUTDOWN_TIMEOUT before stopping the VMM. The spec's stop
// signal is the guest's to deliver, and granule's override has no way
// to name one, so it is the power button for every container. SIGKILL
// follows after timeout.
func (d *Driver) Stop(ctx context.Context, id string, timeout time.Duration) error {
	ctx = d.ns(ctx)
	_, t, err := d.load(ctx, id)
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

// Kill implements runtime.Driver: sig to granule-shim, for which
// SIGTERM is the power button and SIGKILL ends the VM at once.
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

// Pause implements runtime.Driver: the VM container's cgroup is frozen,
// VMM and all.
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
// VM container, snapshot, virtiofsds, directory and log.
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
	id := c.ID()
	if err := d.stopVirtiofsds(ctx, id); err != nil {
		return err
	}
	if b, err := os.ReadFile(d.pidFile(id)); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 && syscall.Kill(pid, 0) == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for _, f := range []string{d.LogPath(id), d.pidFile(id), filepath.Join(d.cfg.Dir, id+".copier.err"), filepath.Join(d.cfg.Dir, id+".shim.log")} {
		os.Remove(f)
	}
	// What the VM container wrote here belongs to its subuids, but the
	// directories are silod's, so silod can remove it.
	return os.RemoveAll(d.dir(id))
}

// exitRecord is granule's exit file (workloadio.ExitStatus, granule
// 4a60aae), which granule-shim writes to GRANULE_WORKLOAD_EXIT_FILE
// before it exits.
type exitRecord struct {
	ExitCode int    `json:"exitCode"`
	Reported bool   `json:"reported"`
	Reason   string `json:"reason,omitempty"`
}

// Wait implements runtime.Driver. The task's own status is granule-
// shim's, so the workload's is read from its exit record: the code
// granule-container reported, 128+N for signal N. A VM that stopped
// without a report (the guest died, or was stopped before the workload
// finished) is granule's ExitUnreported, 125, with no signal; so is one
// that left no record at all.
func (d *Driver) Wait(ctx context.Context, id string) (runtime.ExitStatus, error) {
	nctx := d.ns(ctx)
	t, err := d.task(nctx, id)
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	ch, err := t.Wait(nctx)
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	select {
	case s := <-ch:
		code, at, err := s.Result()
		if err != nil {
			return runtime.ExitStatus{}, err
		}
		return d.exitStatus(id, int(code), at), nil
	case <-ctx.Done():
		return runtime.ExitStatus{}, ctx.Err()
	}
}

// ExitUnreported is granule's code for a VM that stopped without its
// workload's exit reported (workloadio.ExitUnreported).
const ExitUnreported = 125

// exitStatus is a stopped container's status from its exit record, or
// from shimCode, granule-shim's own, when there is none.
func (d *Driver) exitStatus(id string, shimCode int, at time.Time) runtime.ExitStatus {
	e := runtime.ExitStatus{Code: ExitUnreported, Finished: at}
	if b, err := os.ReadFile(filepath.Join(d.dir(id), "exit.json")); err == nil {
		var r exitRecord
		if json.Unmarshal(b, &r) == nil {
			e.Code = r.ExitCode
			if !r.Reported {
				return e
			}
		}
	} else if shimCode != 0 {
		// No record: granule-shim died before it could write one (a
		// SIGKILL), and its own code is all there is.
		e.Code = shimCode
	}
	if e.Code > 128 && e.Code <= 128+64 {
		e.Signal = syscall.Signal(e.Code - 128)
	}
	return e
}

// ExecArgs is `granule exec --container` in the VM container: p run in
// the wrapped container, by argv rather than through a shell, as its
// user and in its directory unless p names its own (execwire's
// Container request).
func ExecArgs(p runtime.Process) []string {
	a := []string{GranuleBin, "exec", "--container"}
	if p.User != "" {
		a = append(a, "-u", p.User)
	}
	if p.Workdir != "" {
		a = append(a, "-w", p.Workdir)
	}
	for _, e := range p.Env {
		a = append(a, "-e", e)
	}
	return append(append(a, "--"), p.Args...)
}

// Exec implements runtime.Driver: ExecArgs as a process of the VM
// container, as `docker exec <vm> granule exec` does in granule. granule
// exec takes a terminal when its stdin is one, and forwards the size.
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
		return 0, errors.New("granule: exec of nothing")
	}
	for _, e := range p.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return 0, fmt.Errorf("granule: exec environment %q is not KEY=value", e)
		}
	}
	if p.Workdir != "" && !path.IsAbs(p.Workdir) {
		return 0, fmt.Errorf("granule: exec directory %q is not absolute", p.Workdir)
	}
	s, err := c.Spec(ctx)
	if err != nil {
		return 0, err
	}
	ps := *s.Process
	ps.Args = ExecArgs(p)
	ps.Env = append([]string{}, s.Process.Env...)
	if p.TTY {
		ps.Env = append(ps.Env, "TERM=xterm")
	}
	ps.Terminal = p.TTY
	ps.ConsoleSize = nil
	ps.Cwd = "/"
	var stdin io.Reader
	stdinDone := make(chan struct{})
	if p.Stdin && stdio.Stdin != nil {
		stdin = &eofReader{r: stdio.Stdin, done: stdinDone}
	}
	streams := []cio.Opt{cio.WithStreams(stdin, stdio.Stdout, stdio.Stderr), cio.WithFIFODir(d.cfg.Dir)}
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
	if _, _, err := d.load(d.ns(ctx), id); err != nil {
		return runtime.Streams{}, err
	}
	out, errb, err := gvisor.ReadLog(d.LogPath(id))
	if err != nil {
		return runtime.Streams{}, err
	}
	s := runtime.Streams{Stdout: io.NopCloser(strings.NewReader(string(out))), Stderr: io.NopCloser(strings.NewReader(string(errb)))}
	if in := d.fifo(id, "stdin"); exists(in) {
		// The copier holds the FIFO open, so this doesn't block.
		f, err := os.OpenFile(in, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return runtime.Streams{}, err
		}
		unix.SetNonblock(int(f.Fd()), false)
		s.Stdin = f
	}
	return s, nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Stats implements runtime.Driver, from the shim's cgroup metrics: the
// VM container's, which is the VMM's and so the guest's.
func (d *Driver) Stats(ctx context.Context, id string) (runtime.Stats, error) {
	ctx = d.ns(ctx)
	t, err := d.task(ctx, id)
	if err != nil {
		return runtime.Stats{}, err
	}
	return gvisor.TaskStats(ctx, t)
}

// CopyIn implements runtime.Driver, by tar in the wrapped container
// through granule exec: the container's root is the guest's disk, not
// anything on the host. It needs tar in the image, and runs as root.
func (d *Driver) CopyIn(ctx context.Context, id, dir string, r io.Reader) error {
	if !path.IsAbs(dir) {
		return fmt.Errorf("granule: copy into %q, not an absolute path", dir)
	}
	var stderr strings.Builder
	code, err := d.Exec(ctx, id, runtime.Process{Args: []string{"tar", "-x", "-f", "-", "-C", dir}, User: "0", Stdin: true},
		runtime.ExecIO{Stdin: r, Stdout: io.Discard, Stderr: &stderr})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("granule: extracting into %s: %s", dir, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CopyOut implements runtime.Driver, by tar in the wrapped container.
func (d *Driver) CopyOut(ctx context.Context, id, p string) (io.ReadCloser, error) {
	if !path.IsAbs(p) {
		return nil, fmt.Errorf("granule: copy from %q, not an absolute path", p)
	}
	p = path.Clean(p)
	if _, _, err := d.load(d.ns(ctx), id); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		var stderr strings.Builder
		code, err := d.Exec(ctx, id, runtime.Process{Args: []string{"tar", "-c", "-f", "-", "-C", path.Dir(p), path.Base(p)}, User: "0"},
			runtime.ExecIO{Stdout: pw, Stderr: &stderr})
		if err == nil && code != 0 {
			err = fmt.Errorf("granule: archiving %s: %w: %s", p, os.ErrNotExist, strings.TrimSpace(stderr.String()))
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// List implements runtime.Driver: every vm-class container in
// silo-containerd, whether this silod or an earlier one made it.
func (d *Driver) List(ctx context.Context) ([]runtime.Container, error) {
	ctx = d.ns(ctx)
	cs, err := d.client.Containers(ctx, fmt.Sprintf("labels.%q==%s", LabelClass, runtime.VM))
	if err != nil {
		return nil, err
	}
	var out []runtime.Container
	for _, c := range cs {
		info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			continue
		}
		ct := runtime.Container{ID: c.ID(), Class: runtime.VM, State: runtime.Stopped, Labels: map[string]string{}}
		for k, v := range info.Labels {
			if k != LabelClass {
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
					e := d.exitStatus(c.ID(), int(st.ExitStatus), st.ExitTime)
					ct.Exit = &e
				}
			}
		}
		out = append(out, ct)
	}
	return out, nil
}

// Adopt is List, after restarting the log copier of any container left
// running whose copier died. The VM containers and their virtiofsds
// outlive silod, as the shims do.
func (d *Driver) Adopt(ctx context.Context) ([]runtime.Container, error) {
	cs, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if c.State == runtime.Stopped || d.copierAlive(c.ID) || !exists(d.fifo(c.ID, "stdout")) {
			continue
		}
		ioc := cio.Config{Stdout: d.fifo(c.ID, "stdout")}
		if exists(d.fifo(c.ID, "stderr")) {
			ioc.Stderr = d.fifo(c.ID, "stderr")
		}
		if exists(d.fifo(c.ID, "stdin")) {
			ioc.Stdin = d.fifo(c.ID, "stdin")
		}
		if err := d.startCopier(c.ID, ioc); err != nil {
			return cs, err
		}
	}
	return cs, nil
}

var _ runtime.Driver = (*Driver)(nil)
