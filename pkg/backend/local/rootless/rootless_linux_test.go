package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	_ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
)

// TestMain doubles as the holder (nsd.Start re-runs this binary with
// _SILO_ROOTLESS_TEST_HOLDER) and as `silod runsc`
// (_SILO_ROOTLESS_TEST_RUNSC).
func TestMain(m *testing.M) {
	if cfg := os.Getenv("_SILO_ROOTLESS_TEST_HOLDER"); cfg != "" {
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
	if os.Getenv("_SILO_ROOTLESS_TEST_RUNSC") != "" {
		os.Unsetenv("_SILO_ROOTLESS_TEST_RUNSC")
		err := RunscMain(os.Args[0], os.Args[1:])
		os.Stderr.WriteString("silod runsc: " + err.Error() + "\n")
		os.Exit(125)
	}
	os.Exit(m.Run())
}

// need skips without the holder's prerequisites or tool, and fails
// instead under SILO_REQUIRE_NSD, which CI's nsd job sets.
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

// startHolder starts silo-nsd with its pins in its private /run, as
// `silod nsd` does.
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
	cmd, err := nsd.Start(os.Args[0], []string{"-test.run=^$"}, append(os.Environ(), "_SILO_ROOTLESS_TEST_HOLDER="+string(b)), m, os.Stderr, os.Stderr)
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

// inHolder runs argv inside the holder and returns its combined output.
func inHolder(t *testing.T, pid int, env []string, argv ...string) (string, error) {
	t.Helper()
	cmd := nsd.JoinExec(pid, os.Args[0], argv...)
	cmd.Env = append(cmd.Env, env...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() { <-ctx.Done(); cmd.Process.Kill() }()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// makeBundle writes an OCI bundle at dir, inside the holder's mount
// namespace: a busybox root filesystem owned by container range 0's
// root (holder uid 1), and config, the spec, which runs script.
func makeBundle(t *testing.T, pid int, dir string, config []byte) {
	t.Helper()
	busybox := lookPath(t, "busybox")
	tmp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(tmp, config, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `set -e
mkdir -p "$1/rootfs/bin" "$1/rootfs/proc" "$1/rootfs/dev" "$1/rootfs/sys" "$1/rootfs/tmp" "$1/rootfs/etc"
cp "$2" "$1/rootfs/bin/busybox"
for t in sh id ip cat grep head ls; do ln -sf busybox "$1/rootfs/bin/$t"; done
cp "$3" "$1/config.json"
chown -R 1:1 "$1/rootfs"
chmod 755 "$1" "$(dirname "$1")"
`
	if out, err := inHolder(t, pid, nil, "sh", "-c", script, "sh", dir, busybox, tmp); err != nil {
		t.Fatalf("bundle: %v: %s", err, out)
	}
}

// baseSpec is a runtime's own `spec`, running script.
func baseSpec(t *testing.T, runtime, script string) []byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(runtime, "spec")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s spec: %v: %s", runtime, err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	proc := spec["process"].(map[string]any)
	proc["terminal"] = false
	proc["args"] = []string{"/bin/sh", "-c", script}
	delete(spec["linux"].(map[string]any), "resources")
	b, _ = json.Marshal(spec)
	return b
}

func TestRuntimesInTheHolder(t *testing.T) {
	c, pid := startHolder(t)

	t.Run("containerd", func(t *testing.T) {
		containerd := lookPath(t, "containerd")
		ctr := lookPath(t, "ctr")
		p := Paths{DataDir: t.TempDir(), RuntimeDir: filepath.Join(t.TempDir(), "silo")}
		cmd, err := StartContainerd(p, pid, os.Args[0], containerd)
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() }()
		// silod dials containerd from the host's namespaces.
		var out []byte
		for i := 0; ; i++ {
			out, err = exec.Command(ctr, "--address", p.ContainerdSocket(), "version").CombinedOutput()
			if err == nil {
				break
			}
			if i > 100 {
				t.Fatalf("containerd never answered: %v: %s\ncontainerd's log:\n%s", err, out, log.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.Contains(string(out), "Server:") {
			t.Fatalf("ctr version: %s", out)
		}
		// Its user namespace is the holder's, not the host's.
		theirs, _ := os.Readlink("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/ns/user")
		holder, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/user")
		if theirs != holder {
			t.Errorf("containerd's user namespace is %s, the holder's %s", theirs, holder)
		}
		// Its state, shim sockets included, is in the holder's private
		// /run, which an unprivileged user can't create on the host.
		if out, err := inHolder(t, pid, nil, "ls", "/run/containerd"); err != nil || !strings.Contains(out, "io.containerd.runtime.v2.task") {
			t.Errorf("/run/containerd in the holder: %v: %s", err, out)
		}
	})

	t.Run("vm class keeps the kvm group under crun", func(t *testing.T) {
		crun := lookPath(t, "crun")
		ns, err := c.Container("vm1", 0)
		if err != nil {
			t.Fatal(err)
		}
		script := `grep '^Groups:' /proc/self/status; if [ -e /dev/kvm ]; then head -c0 /dev/kvm && echo kvm-open-ok; fi`
		devs := VMDevices[:0:0]
		for _, d := range VMDevices {
			if _, err := os.Stat(d); err == nil {
				devs = append(devs, d)
			}
		}
		run := func(name string, p Patch) string {
			var ms []Mount
			for _, m := range p.Mounts {
				for _, d := range devs {
					if m.Destination == d {
						ms = append(ms, m)
					}
				}
			}
			p.Mounts = ms
			config, err := p.Apply(baseSpec(t, crun, script))
			if err != nil {
				t.Fatal(err)
			}
			b := "/run/silo/bundles/" + name
			makeBundle(t, pid, b, config)
			out, err := inHolder(t, pid, nil, crun, "--root", "/run/silo/crun", "run", "--bundle", b, name)
			t.Logf("%s: %s", name, out)
			if err != nil && !strings.Contains(out, "Permission denied") {
				t.Fatalf("crun run %s: %v: %s", name, err, out)
			}
			return out
		}
		groups, _ := os.Getgroups()
		need(t, "the user has no supplementary groups to keep", len(groups) > 0)
		kept := run("vm-keep", VMPatch(*ns, "", nil))
		if strings.TrimSpace(strings.TrimPrefix(firstLine(kept), "Groups:")) == "" {
			t.Errorf("keep_original_groups kept no groups: %s", kept)
		}
		dropped := VMPatch(*ns, "", nil)
		delete(dropped.Annotations, AnnotationKeepGroups)
		lost := run("vm-drop", dropped)
		if strings.TrimSpace(strings.TrimPrefix(firstLine(lost), "Groups:")) != "" {
			t.Errorf("without keep_original_groups, the groups survived (the control is wrong): %s", lost)
		}
		if kvm := kvmCheck(LocalHost()); kvm.OK && strings.Contains(kvm.Detail, "group") {
			if !strings.Contains(kept, "kvm-open-ok") {
				t.Errorf("/dev/kvm isn't reachable through the kvm group: %s", kept)
			}
			if strings.Contains(lost, "kvm-open-ok") {
				t.Errorf("/dev/kvm is reachable without the kvm group (the control is wrong): %s", lost)
			}
		} else {
			t.Logf("not checking /dev/kvm: %s", kvm.Detail)
		}
	})

	t.Run("sandbox class runs runsc inside the container's user namespace", func(t *testing.T) {
		runsc := os.Getenv("SILO_NSD_RUNSC")
		if runsc == "" {
			runsc, _ = exec.LookPath("runsc")
		}
		need(t, "no runsc (set SILO_NSD_RUNSC)", runsc != "")
		runc := lookPath(t, "runc")
		ns, err := c.Container("sb1", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Veth(nsd.Veth{Container: "sb1", Name: "vsb1", Bridge: "gw0",
			Addrs: []netip.Prefix{netip.MustParsePrefix("10.99.0.2/16")}, Gateway: netip.MustParseAddr("10.99.0.1")}); err != nil {
			t.Fatal(err)
		}
		script := "id; ip addr show eth0"
		run := func(name string, p Patch) (string, error) {
			config, err := p.Apply(baseSpec(t, runc, script))
			if err != nil {
				t.Fatal(err)
			}
			b := "/run/silo/bundles/" + name
			makeBundle(t, pid, b, config)
			return inHolder(t, pid, []string{"_SILO_ROOTLESS_TEST_RUNSC=1"}, os.Args[0], "--runsc", runsc, "--",
				"--root", "/run/silo/runsc", "--ignore-cgroups", "--network=sandbox", "--platform=systrap", "run", "-bundle", b, name)
		}
		out, err := run("sb1", SandboxPatch(*ns, ""))
		t.Logf("sb1: %s", out)
		if err != nil {
			t.Fatalf("runsc through silod runsc: %v: %s", err, out)
		}
		if !strings.Contains(out, "uid=0") || !strings.Contains(out, "inet 10.99.0.2/16") {
			t.Fatalf("netstack didn't take the veth's address, or the workload isn't root: %s", out)
		}
		// The bundle was handed to the container's root (holder uid 1),
		// which runsc runs as, not the holder's root, silod's uid.
		if out, err := inHolder(t, pid, nil, "stat", "-c", "%u", "/run/silo/bundles/sb1"); err != nil || strings.TrimSpace(out) != "1" {
			t.Errorf("the bundle's owner: %v: %q, want 1", err, out)
		}

		// A bundle that names no user namespace is refused, rather
		// than run as the holder's root.
		p := SandboxPatch(*ns, "")
		p.Annotations = nil
		out, err = run("sb2", p)
		if err == nil || !strings.Contains(out, "names no user namespace") {
			t.Fatalf("a bundle without %s: %v: %s", AnnotationUserNS, err, out)
		}
	})
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
