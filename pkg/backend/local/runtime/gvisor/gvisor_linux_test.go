package gvisor_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// TestMain doubles as the holder, `silod runsc`, `silod runtime-log`
// and `silod runtime-remap`.
func TestMain(m *testing.M) {
	if cfg := os.Getenv("_SILO_GVISOR_TEST_HOLDER"); cfg != "" {
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
	if os.Getenv("_SILO_GVISOR_TEST_RUNSC") != "" || os.Getenv(rootless.ReaperEnv) != "" {
		os.Unsetenv("_SILO_GVISOR_TEST_RUNSC")
		err := rootless.RunscMain(os.Args[0], os.Args[1:])
		os.Stderr.WriteString("silod runsc: " + err.Error() + "\n")
		os.Exit(125)
	}
	if len(os.Args) > 1 && os.Getenv("_SILO_GVISOR_TEST_SUB") != "" {
		var err error
		switch os.Args[1] {
		case gvisor.LogArg:
			err = gvisor.LogMain(os.Args[2:])
		case gvisor.RemapArg:
			err = gvisor.RemapMain(os.Args[2:])
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
// SILO_REQUIRE_NSD, which CI's nsd job sets.
func need(t *testing.T, what string, ok bool) {
	t.Helper()
	if ok {
		return
	}
	if os.Getenv("SILO_REQUIRE_NSD") != "" {
		t.Fatalf("SILO_REQUIRE_NSD is set, but %s", what)
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

func startHolder(t *testing.T) (*nsd.Client, int) {
	var missing []string
	for _, c := range nsd.Failed(nsd.Probe()) {
		missing = append(missing, c.Name+": "+c.Detail)
	}
	need(t, "this host can't run the holder: "+strings.Join(missing, "; "), len(missing) == 0)
	m := nsd.HolderMapping(os.Getuid(), os.Getgid(), subID(t, "/etc/subuid", os.Getuid()), subID(t, "/etc/subgid", os.Getgid()))
	dir := t.TempDir()
	cfg := nsd.Config{
		Socket: filepath.Join(dir, "nsd.sock"), State: filepath.Join(dir, "nsd.state"), PinDir: nsd.DefaultPinDir,
		Bridge: "gw0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.99.0.1/16")},
	}
	b, _ := json.Marshal(cfg)
	cmd, err := nsd.Start(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_GVISOR_TEST_HOLDER="+string(b)), m, os.Stderr, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var c *nsd.Client
	for i := 0; ; i++ {
		if c, err = nsd.Dial(cfg.Socket); err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("holder never listened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { c.Close() })
	return c, cmd.Process.Pid
}

// env is silo-containerd in a holder, the runsc shim's options, and a
// driver's configuration for them.
type env struct {
	nsd *nsd.Client
	cfg gvisor.Config
}

func setup(t *testing.T) *env {
	runsc := os.Getenv("SILO_NSD_RUNSC")
	if runsc == "" {
		runsc, _ = exec.LookPath("runsc")
	}
	need(t, "no runsc (set SILO_NSD_RUNSC)", runsc != "")
	shim := filepath.Join(filepath.Dir(runsc), "containerd-shim-runsc-v1")
	if _, err := os.Stat(shim); err != nil {
		shim = lookPath(t, "containerd-shim-runsc-v1")
	}
	containerd := lookPath(t, "containerd")
	busybox := lookPath(t, "busybox")
	c, pid := startHolder(t)

	// The test's directories are under /tmp, which the holder sees as
	// the host does; its own /run is private.
	base, err := os.MkdirTemp("", "silo-gvisor-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o755)
	// The containers' files belong to subuids, which only the holder
	// can remove.
	t.Cleanup(func() {
		cmd := nsd.JoinExec(pid, os.Args[0], "rm", "-rf", base)
		cmd.Run()
	})
	p := rootless.Paths{DataDir: filepath.Join(base, "data"), RuntimeDir: filepath.Join(base, "rt")}
	cmd, err := rootless.StartContainerd(p, pid, os.Args[0], containerd)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("_RUNSC_DEBUG") != "" {
		cmd.Args = append(cmd.Args, "--log-level", "debug")
	}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
		if t.Failed() {
			t.Logf("containerd's log:\n%s", log.String())
		}
	})
	// The options are rootless.RunscOptionsTOML's, with this test binary
	// as `silod runsc`, and without cgroups, which a CI runner doesn't
	// delegate.
	wrapper := filepath.Join(p.RuntimeDir, "runsc.sh")
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n_SILO_GVISOR_TEST_RUNSC=1 exec %q --runsc %q -- \"$@\"\n", os.Args[0], runsc)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := strings.Replace(rootless.RunscOptionsTOML(p, wrapper), `systemd-cgroup = "true"`, `ignore-cgroups = "true"`+os.Getenv("_RUNSC_DEBUG"), 1)
	if err := os.WriteFile(p.RunscOptions(), []byte(opts), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := gvisor.Config{
		Address:   p.ContainerdSocket(),
		Shim:      shim,
		Options:   p.RunscOptions(),
		Dir:       filepath.Join(base, "containers"),
		Volumes:   filepath.Join(base, "volumes"),
		Logger:    []string{os.Args[0], gvisor.LogArg},
		LoggerEnv: []string{"_SILO_GVISOR_TEST_SUB=1"},
		InHolder:  gvisor.InHolder(pid, os.Args[0], "_SILO_GVISOR_TEST_SUB=1"),
		Toolbox:   busybox,
	}
	for i := 0; ; i++ {
		d, err := gvisor.New(cfg)
		if err == nil {
			pr := d.Probe(context.Background())
			d.Close()
			if pr.OK {
				break
			}
			err = fmt.Errorf("%s", pr.Why)
		}
		if i > 100 {
			t.Fatalf("containerd never answered: %v\n%s", err, log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Whatever a failed test left running is stopped and deleted, so no
	// shim outlives the test.
	t.Cleanup(func() {
		d, err := gvisor.New(cfg)
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
	return &env{nsd: c, cfg: cfg}
}

// busyboxImage imports an image of busybox's applets into the driver's
// namespace and returns its digest.
func busyboxImage(t *testing.T, d *gvisor.Driver) string {
	t.Helper()
	bb, err := os.ReadFile(lookPath(t, "busybox"))
	if err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	for _, dir := range []string{"bin", "dev", "etc", "proc", "root", "sys", "tmp"} {
		mode := int64(0o755)
		if dir == "tmp" {
			mode = 0o1777
		}
		tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: mode})
	}
	tw.WriteHeader(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bb))})
	tw.Write(bb)
	for _, a := range []string{"sh", "cat", "echo", "id", "ip", "ls", "sleep", "tar", "grep", "head"} {
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

// sandbox is a spec for a sandbox-class container running script, wired
// at addr.
func (e *env) sandbox(t *testing.T, id, image string, n uint32, addr string, script string) runtime.Spec {
	t.Helper()
	spec := runtime.Spec{
		ID: id, Silo: "s1", Image: image,
		Process: runtime.Process{Args: []string{"/bin/sh", "-c", script}},
		Labels:  map[string]string{runtime.LabelSilo: "s1", runtime.LabelName: id, runtime.LabelAddr: addr},
	}
	if err := local.Wire(e.nsd, &spec, n, nsd.Veth{Name: "v" + id, Bridge: "gw0",
		Addrs: []netip.Prefix{netip.MustParsePrefix(addr + "/16")}, Gateway: netip.MustParseAddr("10.99.0.1")}); err != nil {
		t.Fatal(err)
	}
	return spec
}

// eventually polls fn for up to a minute.
func eventually(t *testing.T, what string, fn func() (bool, string)) {
	t.Helper()
	var last string
	for i := 0; i < 600; i++ {
		ok, s := fn()
		if ok {
			return
		}
		last = s
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never happened; last: %s", what, last)
}

func streams(t *testing.T, d *gvisor.Driver, id string) (string, string) {
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

func run(t *testing.T, d *gvisor.Driver, id string, p runtime.Process, stdin io.Reader) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	p.Stdin = stdin != nil
	code, err := d.Exec(context.Background(), id, p, runtime.ExecIO{Stdin: stdin, Stdout: &o, Stderr: &e})
	if err != nil {
		t.Fatalf("exec %v: %v", p.Args, err)
	}
	return code, o.String(), e.String()
}

func TestSandboxDriver(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, err := gvisor.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	if d.Class() != runtime.Sandbox {
		t.Fatalf("class %s", d.Class())
	}
	img := busyboxImage(t, d)
	if err := d.Prepare(ctx, img); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	spec := e.sandbox(t, "c1", img, 0, "10.99.0.2", `echo hello; echo oops >&2; i=0; while :; do i=$((i+1)); echo tick $i; sleep 0.2; done`)
	spec.StopSignal = syscall.SIGTERM
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if cs, err := d.List(ctx); err != nil || len(cs) != 1 || cs[0].State != runtime.Created {
		t.Fatalf("after create, list: %v %+v", err, cs)
	}
	if err := d.Start(ctx, "c1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := d.Start(ctx, "c1"); err == nil {
		t.Errorf("a second start succeeded")
	}

	t.Run("logs", func(t *testing.T) {
		eventually(t, "the workload's output in its log", func() (bool, string) {
			o, er := streams(t, d, "c1")
			b, _ := os.ReadFile(filepath.Join(e.cfg.Dir, "c1", "copier.err"))
			return strings.HasPrefix(o, "hello\ntick 1\n") && er == "oops\n", o + "|" + er + "|" + string(b)
		})
	})

	t.Run("netstack on the wired netns, as the namespace's root", func(t *testing.T) {
		code, out, er := run(t, d, "c1", runtime.Process{Args: []string{"ip", "addr", "show", "eth0"}}, nil)
		if code != 0 || !strings.Contains(out, "inet 10.99.0.2/16") {
			t.Fatalf("ip addr: %d %s %s", code, out, er)
		}
		code, out, _ = run(t, d, "c1", runtime.Process{Args: []string{"id", "-u"}}, nil)
		if code != 0 || strings.TrimSpace(out) != "0" {
			t.Errorf("id -u: %d %q", code, out)
		}
		// The workload's working directory is the image's.
		_, out, _ = run(t, d, "c1", runtime.Process{Args: []string{"sh", "-c", "pwd; echo $PATH"}}, nil)
		if out != "/root\n/bin\n" {
			t.Errorf("pwd and PATH: %q", out)
		}
	})

	t.Run("exec", func(t *testing.T) {
		code, _, _ := run(t, d, "c1", runtime.Process{Args: []string{"sh", "-c", "exit 3"}}, nil)
		if code != 3 {
			t.Errorf("exit code %d, want 3", code)
		}
		code, out, _ := run(t, d, "c1", runtime.Process{Args: []string{"cat"}}, strings.NewReader("from stdin\n"))
		if code != 0 || out != "from stdin\n" {
			t.Errorf("cat: %d %q", code, out)
		}
		code, out, _ = run(t, d, "c1", runtime.Process{Args: []string{"id", "-u"}, User: "1000"}, nil)
		if code != 0 || strings.TrimSpace(out) != "1000" {
			t.Errorf("exec as 1000: %d %q", code, out)
		}
	})

	t.Run("archive", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		tw.WriteHeader(&tar.Header{Name: "x/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: "x/hello.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
		tw.Write([]byte("hello\n"))
		tw.Close()
		if err := d.CopyIn(ctx, "c1", "/tmp", &b); err != nil {
			t.Fatalf("copy in: %v", err)
		}
		if _, out, _ := run(t, d, "c1", runtime.Process{Args: []string{"cat", "/tmp/x/hello.txt"}}, nil); out != "hello\n" {
			t.Errorf("after copy in: %q", out)
		}
		r, err := d.CopyOut(ctx, "c1", "/tmp/x")
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(r)
		var names []string
		for {
			h, err := tr.Next()
			if err != nil {
				if err != io.EOF {
					t.Fatalf("copy out: %v", err)
				}
				break
			}
			names = append(names, h.Name)
		}
		if strings.Join(names, ",") != "x/,x/hello.txt" {
			t.Errorf("copy out: %v", names)
		}
		r, _ = d.CopyOut(ctx, "c1", "/nonexistent")
		if _, err := io.ReadAll(r); err == nil {
			t.Errorf("copying out a path that doesn't exist succeeded")
		}
	})

	t.Run("stats", func(t *testing.T) {
		st, err := d.Stats(ctx, "c1")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		t.Logf("stats: %+v", st)
		if st.Processes == 0 {
			t.Errorf("a running container reports no processes")
		}
	})

	t.Run("pause and resume", func(t *testing.T) {
		if err := d.Pause(ctx, "c1"); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if cs, _ := d.List(ctx); len(cs) != 1 || cs[0].State != runtime.Paused {
			t.Errorf("after pause: %+v", cs)
		}
		if err := d.Pause(ctx, "c1"); err == nil {
			t.Errorf("pausing a paused container succeeded")
		}
		if err := d.Resume(ctx, "c1"); err != nil {
			t.Fatalf("resume: %v", err)
		}
	})

	t.Run("adoption after silod restarts", func(t *testing.T) {
		o, _ := streams(t, d, "c1")
		d.Close()
		// The log copier dies with silod, as a crash could take it.
		pid, _ := os.ReadFile(filepath.Join(e.cfg.Dir, "c1", "copier.pid"))
		var n int
		fmt.Sscan(string(pid), &n)
		if n == 0 {
			t.Fatal("no copier pid file")
		}
		syscall.Kill(n, syscall.SIGKILL)
		if d, err = gvisor.New(e.cfg); err != nil {
			t.Fatal(err)
		}
		cs, err := d.Adopt(ctx)
		if err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if len(cs) != 1 || cs[0].ID != "c1" || cs[0].State != runtime.Running || cs[0].Labels[runtime.LabelName] != "c1" || cs[0].Labels[runtime.LabelSilo] != "s1" {
			t.Fatalf("adopted: %+v", cs)
		}
		// The workload kept running, and its output reaches the log
		// again.
		eventually(t, "output after adoption", func() (bool, string) {
			o2, _ := streams(t, d, "c1")
			return len(o2) > len(o)+20, o2[len(o):]
		})
	})

	t.Run("stop, wait and delete", func(t *testing.T) {
		if err := d.Delete(ctx, "c1"); err == nil {
			t.Errorf("deleting a running container succeeded")
		}
		if err := d.Stop(ctx, "c1", 10*time.Second); err != nil {
			t.Fatalf("stop: %v", err)
		}
		st, err := d.Wait(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if st.Signal != syscall.SIGTERM {
			t.Errorf("exit after stop: %+v", st)
		}
		if err := d.Delete(ctx, "c1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if cs, _ := d.List(ctx); len(cs) != 0 {
			t.Errorf("after delete: %+v", cs)
		}
		if _, err := d.Wait(ctx, "c1"); err == nil {
			t.Errorf("wait on a deleted container succeeded")
		}
		if err := local.Unwire(e.nsd, "c1", "vc1"); err != nil {
			t.Errorf("unwire: %v", err)
		}
	})

	t.Run("exit code", func(t *testing.T) {
		spec := e.sandbox(t, "c2", img, 0, "10.99.0.3", "echo bye; exit 7")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if err := d.Start(ctx, "c2"); err != nil {
			t.Fatal(err)
		}
		st, err := d.Wait(ctx, "c2")
		if err != nil || st.Code != 7 || st.Signal != 0 {
			t.Fatalf("wait: %v %+v", err, st)
		}
		if o, _ := streams(t, d, "c2"); o != "bye\n" {
			t.Errorf("output: %q", o)
		}
		cs, _ := d.List(ctx)
		if len(cs) != 1 || cs[0].State != runtime.Stopped || cs[0].Exit == nil || cs[0].Exit.Code != 7 {
			t.Errorf("list: %+v", cs)
		}
		if err := d.Delete(ctx, "c2"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		spec := runtime.Spec{ID: "c3", Silo: "s1", Image: img, NetNS: "/run/silo/netns/x",
			Process: runtime.Process{Args: []string{"true"}}, Labels: map[string]string{runtime.LabelSilo: "s1"}}
		if err := d.Create(ctx, spec); err == nil || !strings.Contains(err.Error(), "user namespace") {
			t.Errorf("a spec with no user namespace: %v", err)
		}
		if err := d.Start(ctx, "nope"); err == nil {
			t.Errorf("starting a container that doesn't exist succeeded")
		}
	})
}
