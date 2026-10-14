package granule_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/bwsalmon/silo/pkg/backend/local"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/granule"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
)

// TestMain doubles as the holder and as the `silod runtime-*`
// subcommands the driver runs.
func TestMain(m *testing.M) {
	if cfg := os.Getenv("_SILO_GRANULE_TEST_HOLDER"); cfg != "" {
		var c nsd.Config
		if err := json.Unmarshal([]byte(cfg), &c); err != nil {
			panic(err)
		}
		if err := nsd.Run(context.Background(), c); err != nil {
			os.Stderr.WriteString("holder: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Getenv("_SILO_GRANULE_TEST_SUB") != "" {
		var err error
		switch os.Args[1] {
		case gvisor.LogArg:
			err = gvisor.LogMain(os.Args[2:])
		case gvisor.RemapArg:
			err = gvisor.RemapMain(os.Args[2:])
		case granule.WrapArg:
			err = granule.WrapMain(os.Args[2:])
		case granule.CrunArg:
			err = granule.CrunMain(os.Args[2:])
		case granule.VirtiofsdArg:
			err = granule.VirtiofsdMain(os.Args[0], os.Args[2:])
		default:
			err = fmt.Errorf("unknown subcommand")
		}
		if err != nil {
			os.Stderr.WriteString(os.Args[1] + ": " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// need skips without a prerequisite, and fails instead under
// SILO_REQUIRE_VM.
func need(t *testing.T, what string, ok bool) {
	t.Helper()
	if ok {
		return
	}
	if os.Getenv("SILO_REQUIRE_VM") != "" {
		t.Fatalf("SILO_REQUIRE_VM is set, but %s", what)
	}
	t.Skip(what)
}

func lookPath(t *testing.T, bin string) string {
	t.Helper()
	p, err := exec.LookPath(bin)
	need(t, bin+" is not on PATH", err == nil)
	return p
}

func subID(t *testing.T, file string, id int) nsd.SubID {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	user := os.Getenv("USER")
	if user == "" {
		out, _ := exec.Command("id", "-un").Output()
		user = strings.TrimSpace(string(out))
	}
	s, ok, err := nsd.ParseSubID(bytes.NewReader(b), user, id)
	if err != nil || !ok {
		t.Fatalf("no range for %s in %s: %v", user, file, err)
	}
	return s
}

// env is silo-containerd in a holder with the vm class's images, and a
// driver's configuration for them.
type env struct {
	nsd *nsd.Client
	pid int
	cfg granule.Config
}

// The e2e's inputs: the pinned granule image and granule's guest rootfs
// image as `docker save` archives, the pinned granule-shim (which
// wraps), and virtiofsd. granule's images at granule.Commit are not
// published, so the test runs only where they have been built from it
// (docs/silo/local-backend.md §6, "As built in step 10").
const (
	envImage     = "SILO_GRANULE_IMAGE"
	envGuest     = "SILO_GRANULE_GUEST_IMAGE"
	envShim      = "SILO_GRANULE_SHIM"
	envVirtiofsd = "SILO_VIRTIOFSD"
)

func setup(t *testing.T) *env {
	if p := granule.ProbeHost(granule.DefaultHostDevices); !p.OK {
		need(t, p.Why, false)
	}
	for _, v := range []string{envImage, envGuest, envShim} {
		need(t, "no "+v+": the vm class's e2e needs granule's images and granule-shim at granule.Commit", os.Getenv(v) != "")
	}
	containerd := lookPath(t, "containerd")
	crun := lookPath(t, "crun")
	lookPath(t, "mke2fs")
	var missing []string
	for _, c := range nsd.Failed(nsd.Probe()) {
		missing = append(missing, c.Name+": "+c.Detail)
	}
	need(t, "this host can't run the holder: "+strings.Join(missing, "; "), len(missing) == 0)
	m := nsd.HolderMapping(os.Getuid(), os.Getgid(), subID(t, "/etc/subuid", os.Getuid()), subID(t, "/etc/subgid", os.Getgid()))
	dir := t.TempDir()
	hcfg := nsd.Config{
		Socket: filepath.Join(dir, "nsd.sock"), State: filepath.Join(dir, "nsd.state"), PinDir: nsd.DefaultPinDir,
		Bridge: "gw0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.98.0.1/16")},
	}
	b, _ := json.Marshal(hcfg)
	cmd, err := nsd.Start(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_GRANULE_TEST_HOLDER="+string(b)), m, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	pid := cmd.Process.Pid
	var c *nsd.Client
	for i := 0; ; i++ {
		if c, err = nsd.Dial(hcfg.Socket); err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("holder never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { c.Close() })

	base, err := os.MkdirTemp("", "silo-granule-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o755)
	t.Cleanup(func() {
		cmd := nsd.JoinExec(pid, os.Args[0], "rm", "-rf", base)
		cmd.Run()
		os.RemoveAll(base)
	})
	p := rootless.Paths{DataDir: filepath.Join(base, "data"), RuntimeDir: filepath.Join(base, "rt")}
	ccmd, err := rootless.StartContainerd(p, pid, os.Args[0], containerd)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	ccmd.Stdout, ccmd.Stderr = &log, &log
	if err := ccmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ccmd.Process.Signal(syscall.SIGTERM)
		ccmd.Wait()
		if t.Failed() {
			t.Logf("containerd's log:\n%s", log.String())
		}
	})
	// The holder runs mke2fs, which is in sbin.
	path := os.Getenv("PATH") + ":/usr/sbin:/sbin"
	sub := []string{"_SILO_GRANULE_TEST_SUB=1", "PATH=" + path}
	cfg := granule.Config{
		Address:    p.ContainerdSocket(),
		Crun:       crun,
		Silod:      os.Args[0],
		SilodEnv:   []string{"_SILO_GRANULE_TEST_SUB=1"},
		Dir:        filepath.Join(base, "containers"),
		Wraps:      filepath.Join(base, "wraps"),
		Volumes:    filepath.Join(base, "volumes"),
		Image:      "silo.test/granule:pinned",
		GuestImage: "silo.test/granule-guest-rootfs:pinned",
		Granule:    os.Getenv(envShim),
		Virtiofsd:  os.Getenv(envVirtiofsd),
		DNS:        func(runtime.Spec) string { return "10.98.0.1" },
		Logger:     []string{os.Args[0], gvisor.LogArg},
		LoggerEnv:  []string{"_SILO_GRANULE_TEST_SUB=1"},
		InHolder:   gvisor.InHolder(pid, os.Args[0], sub...),
	}
	for _, d := range []string{cfg.Volumes} {
		os.MkdirAll(d, 0o755)
	}
	var d *granule.Driver
	for i := 0; ; i++ {
		if d, err = granule.New(cfg); err == nil {
			if _, err = d.Client().Version(namespaces.WithNamespace(context.Background(), rootless.ContainerdNamespace)); err == nil {
				break
			}
			d.Close()
		}
		if i > 100 {
			t.Fatalf("containerd never answered: %v\n%s", err, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer d.Close()
	ctx := namespaces.WithNamespace(context.Background(), rootless.ContainerdNamespace)
	for name, file := range map[string]string{cfg.Image: os.Getenv(envImage), cfg.GuestImage: os.Getenv(envGuest)} {
		importAs(t, d, ctx, file, name)
	}
	t.Cleanup(func() {
		d, err := granule.New(cfg)
		if err != nil {
			return
		}
		defer d.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cs, _ := d.List(ctx)
		for _, c := range cs {
			d.Stop(ctx, c.ID, time.Second)
			d.Delete(ctx, c.ID)
		}
	})
	return &env{nsd: c, pid: pid, cfg: cfg}
}

// importAs imports a `docker save` archive's one image as name.
func importAs(t *testing.T, d *granule.Driver, ctx context.Context, file, name string) {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	imgs, err := d.Client().Import(ctx, f)
	if err != nil || len(imgs) == 0 {
		t.Fatalf("importing %s: %v", file, err)
	}
	img := imgs[0]
	img.Name = name
	is := d.Client().ImageService()
	if _, err := is.Create(ctx, img); err != nil {
		if _, err := is.Update(ctx, img); err != nil {
			t.Fatalf("naming %s: %v", name, err)
		}
	}
}

// busyboxImage imports an image of busybox's applets and returns its
// digest.
func busyboxImage(t *testing.T, d *granule.Driver) string {
	t.Helper()
	bb, err := os.ReadFile(lookPath(t, "busybox"))
	if err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, dir := range []string{"bin", "dev", "etc", "proc", "root", "sys", "tmp", "data", "ro"} {
		mode := int64(0o755)
		if dir == "tmp" {
			mode = 0o1777
		}
		tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: mode})
	}
	tw.WriteHeader(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bb))})
	tw.Write(bb)
	for _, a := range []string{"sh", "cat", "echo", "id", "ls", "sleep", "tar", "touch", "hostname", "true"} {
		tw.WriteHeader(&tar.Header{Name: "bin/" + a, Typeflag: tar.TypeSymlink, Linkname: "busybox"})
	}
	passwd := []byte("root:x:0:0:root:/root:/bin/sh\n")
	tw.WriteHeader(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(passwd))})
	tw.Write(passwd)
	tw.Close()

	blobs := map[string][]byte{}
	add := func(b []byte) (string, int) {
		dg := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
		blobs[dg] = b
		return dg, len(b)
	}
	ld, ls := add(layer.Bytes())
	config, _ := json.Marshal(map[string]any{
		"architecture": goruntime.GOARCH, "os": "linux",
		"config": map[string]any{"Env": []string{"PATH=/bin"}, "WorkingDir": "/root"},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{ld}},
	})
	cd, cs := add(config)
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": cs},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": ld, "size": ls}},
	})
	md, ms := add(manifest)
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []any{map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md, "size": ms,
			"annotations": map[string]string{"io.containerd.image.name": "silo.test/busybox:latest"}}},
	})
	var oci bytes.Buffer
	ow := tar.NewWriter(&oci)
	file := func(name string, b []byte) {
		ow.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
		ow.Write(b)
	}
	file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	file("index.json", index)
	for dg, b := range blobs {
		file("blobs/sha256/"+strings.TrimPrefix(dg, "sha256:"), b)
	}
	ow.Close()
	ctx := namespaces.WithNamespace(context.Background(), rootless.ContainerdNamespace)
	if _, err := d.Client().Import(ctx, &oci); err != nil {
		t.Fatalf("import: %v", err)
	}
	return md
}

// vm is a spec for a vm-class container running script, wired at addr.
func (e *env) vm(t *testing.T, id, image string, addr string, script string) runtime.Spec {
	t.Helper()
	spec := runtime.Spec{
		ID: id, Silo: "s1", Image: image,
		Process:   runtime.Process{Args: []string{"/bin/sh", "-c", script}},
		Resources: runtime.Resources{VCPUm: 1000, MemoryMB: 256},
		Labels:    map[string]string{runtime.LabelSilo: "s1", runtime.LabelName: id, runtime.LabelAddr: addr},
	}
	if err := local.Wire(e.nsd, &spec, 0, nsd.Veth{Name: "v" + id, Bridge: "gw0",
		Addrs: []netip.Prefix{netip.MustParsePrefix(addr + "/16")}, Gateway: netip.MustParseAddr("10.98.0.1")}); err != nil {
		t.Fatal(err)
	}
	return spec
}

// eventually polls fn for up to two minutes: a VM boots in seconds, but
// a CI runner's nested KVM is slow.
func eventually(t *testing.T, what string, fn func() (bool, string)) {
	t.Helper()
	var last string
	for i := 0; i < 1200; i++ {
		ok, s := fn()
		if ok {
			return
		}
		last = s
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never happened; last: %s", what, last)
}

func streams(t *testing.T, d *granule.Driver, id string) (string, string) {
	t.Helper()
	s, err := d.Streams(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := io.ReadAll(s.Stdout)
	e, _ := io.ReadAll(s.Stderr)
	if s.Stdin != nil {
		s.Stdin.Close()
	}
	return string(o), string(e)
}

func run(t *testing.T, d *granule.Driver, id string, p runtime.Process, stdin io.Reader) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	p.Stdin = stdin != nil
	code, err := d.Exec(context.Background(), id, p, runtime.ExecIO{Stdin: stdin, Stdout: &o, Stderr: &e})
	if err != nil {
		t.Fatalf("exec %v: %v", p.Args, err)
	}
	return code, o.String(), e.String()
}

func shimLog(e *env, id string) string {
	b, _ := os.ReadFile(filepath.Join(e.cfg.Dir, id+".shim.log"))
	return string(b)
}

// dirLogs is a container's exit record and its virtiofsds' logs.
func dirLogs(e *env, id string) string {
	var b strings.Builder
	fs, _ := filepath.Glob(filepath.Join(e.cfg.Dir, id, "fs", "*.log"))
	for _, f := range append([]string{filepath.Join(e.cfg.Dir, id, "exit.json")}, fs...) {
		c, _ := os.ReadFile(f)
		fmt.Fprintf(&b, "%s: %s\n", filepath.Base(f), c)
	}
	return b.String()
}

func TestVMDriver(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d, err := granule.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Class() != runtime.VM {
		t.Fatalf("class %s", d.Class())
	}
	if p := d.Probe(ctx); !p.OK {
		t.Fatalf("probe without a boot: %s", p.Why)
	}
	img := busyboxImage(t, d)
	start := time.Now()
	if err := d.Prepare(ctx, img); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Logf("wrapped in %v", time.Since(start))
	if ok, err := granule.Wrapped(e.cfg.Wraps, img); !ok || err != nil {
		t.Fatalf("after prepare, wrapped %v %v", ok, err)
	}

	spec := e.vm(t, "c1", img, "10.98.0.2", `echo hello; echo oops >&2; while :; do sleep 1; done`)
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if cs, err := d.List(ctx); err != nil || len(cs) != 1 || cs[0].State != runtime.Created || cs[0].Labels[runtime.LabelName] != "c1" {
		t.Fatalf("after create, list: %v %+v", err, cs)
	}
	if err := d.Start(ctx, "c1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Run("logs", func(t *testing.T) {
		eventually(t, "the workload's output in its log", func() (bool, string) {
			o, er := streams(t, d, "c1")
			return o == "hello\n" && er == "oops\n", o + "|" + er + "|" + shimLog(e, "c1")
		})
	})
	t.Run("exec", func(t *testing.T) {
		code, o, er := run(t, d, "c1", runtime.Process{Args: []string{"sh", "-c", "echo $FOO in $(pwd); cat; echo err >&2; exit 4"}, Env: []string{"FOO=bar"}, Workdir: "/tmp"},
			strings.NewReader("from stdin\n"))
		if code != 4 || o != "bar in /tmp\nfrom stdin\n" || er != "err\n" {
			t.Errorf("exec: %d %q %q", code, o, er)
		}
	})
	t.Run("copy", func(t *testing.T) {
		var a bytes.Buffer
		tw := tar.NewWriter(&a)
		tw.WriteHeader(&tar.Header{Name: "f.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
		tw.Write([]byte("hi\n"))
		tw.Close()
		if err := d.CopyIn(ctx, "c1", "/tmp", &a); err != nil {
			t.Fatalf("copy in: %v", err)
		}
		if _, o, _ := run(t, d, "c1", runtime.Process{Args: []string{"cat", "/tmp/f.txt"}}, nil); o != "hi\n" {
			t.Errorf("after copy in, %q", o)
		}
		r, err := d.CopyOut(ctx, "c1", "/tmp/f.txt")
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(r)
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("copy out: %v", err)
		}
		b, _ := io.ReadAll(tr)
		r.Close()
		if h.Name != "f.txt" || string(b) != "hi\n" {
			t.Errorf("copy out: %s %q", h.Name, b)
		}
		r, _ = d.CopyOut(ctx, "c1", "/nonexistent")
		if _, err := io.ReadAll(r); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("copy out of nothing: %v", err)
		}
	})
	t.Run("stats", func(t *testing.T) {
		st, err := d.Stats(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("stats %+v", st)
	})
	t.Run("stop", func(t *testing.T) {
		if err := d.Stop(ctx, "c1", 30*time.Second); err != nil {
			t.Fatalf("stop: %v", err)
		}
		ex, err := d.Wait(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		// The power button stops the guest under the workload, whose exit
		// is then granule's unreported, or the signal it took.
		t.Logf("after stop: %+v", ex)
		if cs, _ := d.List(ctx); len(cs) != 1 || cs[0].State != runtime.Stopped || cs[0].Exit == nil {
			t.Errorf("after stop, list: %+v", cs)
		}
		if err := d.Delete(ctx, "c1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.cfg.Dir, "c1")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("c1's directory after delete: %v", err)
		}
	})

	t.Run("exit code", func(t *testing.T) {
		spec := e.vm(t, "c2", img, "10.98.0.3", `echo bye; exit 3`)
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("create: %v", err)
		}
		defer d.Delete(context.WithoutCancel(ctx), "c2")
		if err := d.Start(ctx, "c2"); err != nil {
			t.Fatal(err)
		}
		ex, err := d.Wait(ctx, "c2")
		if err != nil {
			t.Fatal(err)
		}
		if ex.Code != 3 || ex.Signal != 0 {
			t.Errorf("exit %+v, want 3; shim: %s", ex, shimLog(e, "c2"))
		}
		if o, _ := streams(t, d, "c2"); o != "bye\n" {
			t.Errorf("output %q", o)
		}
	})

	// volume runs script with a volume at /data, read-only if ro, and
	// returns its exit and output.
	volume := func(t *testing.T, id, addr string, ro bool, script string) (string, runtime.ExitStatus, string) {
		need(t, "no "+envVirtiofsd, e.cfg.Virtiofsd != "")
		src := filepath.Join(e.cfg.Volumes, id)
		os.MkdirAll(src, 0o755)
		os.WriteFile(filepath.Join(src, "there"), []byte("x\n"), 0o644)
		spec := e.vm(t, id, img, addr, script)
		spec.Mounts = []runtime.Mount{{Kind: runtime.MountVolume, Source: src, Target: "/data", ReadOnly: ro}}
		// The container's root owns its volumes, as the storage driver
		// makes them.
		owner := fmt.Sprintf("%d:%d", spec.Root.Outside, spec.Root.Outside)
		if out, err := nsd.JoinExec(e.pid, os.Args[0], "chown", "-R", owner, src).CombinedOutput(); err != nil {
			t.Fatalf("chown: %v %s", err, out)
		}
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("create: %v", err)
		}
		defer d.Delete(context.WithoutCancel(ctx), id)
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		ex, err := d.Wait(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		o, er := streams(t, d, id)
		diag := fmt.Sprintf("stderr %q; shim: %s; %s", er, shimLog(e, id), dirLogs(e, id))
		if err := d.Delete(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if m, _ := filepath.Glob(filepath.Join(e.cfg.Dir, id, "fs", "*.pid")); len(m) != 0 {
			t.Errorf("virtiofsd pid files after delete: %q", m)
		}
		return src, ex, o + "|" + diag
	}
	t.Run("volume", func(t *testing.T) {
		src, ex, o := volume(t, "c3", "10.98.0.4", false, `cat /data/there; echo w > /data/written && echo ok`)
		if ex.Code != 0 || !strings.HasPrefix(o, "x\nok\n|") {
			t.Errorf("exit %+v, output %s", ex, o)
		}
		if b, err := os.ReadFile(filepath.Join(src, "written")); err != nil || string(b) != "w\n" {
			t.Errorf("the guest's write on the host: %q %v", b, err)
		}
	})
	t.Run("read-only volume", func(t *testing.T) {
		src, ex, o := volume(t, "c4", "10.98.0.5", true, `cat /data/there; touch /data/nope 2>/dev/null && echo WRITABLE || echo refused`)
		if strings.Contains(o, `UnknownOption("readonly")`) {
			// granule at Commit passes readonly=on to cloud-hypervisor's
			// --fs, which its pinned v53.0 doesn't know, so no VM with a
			// read-only share boots (local-backend.md §6, "As built in
			// step 10").
			t.Skip("granule's read-only shares don't boot on cloud-hypervisor v53.0: --fs readonly=on is unknown to it")
		}
		if ex.Code != 0 || !strings.HasPrefix(o, "x\nrefused\n|") {
			t.Errorf("exit %+v, output %s", ex, o)
		}
		if _, err := os.Stat(filepath.Join(src, "nope")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a read-only volume was written: %v", err)
		}
	})

	t.Run("probe boots a VM", func(t *testing.T) {
		cfg := e.cfg
		cfg.ProbeSpec = func(ctx context.Context, id string) (runtime.Spec, func(), error) {
			spec := e.vm(t, id, "", "10.98.0.9", "")
			spec.Process.Args = nil
			return spec, func() { e.nsd.DeleteContainer(id, "v"+id) }, nil
		}
		pd, err := granule.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer pd.Close()
		if p := pd.Probe(ctx); !p.OK {
			t.Fatalf("probe: %s", p.Why)
		}
	})
}
