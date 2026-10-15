package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/install"
)

// fakeHost is a machine: files, accounts, a systemd manager, and the
// commands run against it.
type fakeHost struct {
	files    map[string]string
	users    map[string]*user.User
	kvm      map[string]bool // members of kvm
	enabled  map[string]bool
	active   map[string]bool
	ran      []string
	failing  map[string]error // by command name
	checks   []nsd.Check
	runtimes rootless.Report
	env      map[string]string
	tarball  []byte // what Fetch serves
	fetched  int
	// silod is the fake silod: it serves once silod.service is active,
	// and boot is the bootstrap it was sent.
	rootID string
	boot   *install.BootstrapRequest
}

// testGvisor is testdata/gvisor.tar.bz2 as a pinned release.
var testGvisor = GvisorRelease{Version: "1", Arch: map[string]GvisorArch{"x86_64": {
	Tarball:   "048d0835b8d6533ddfbd9477ffad5482698f3b12142fa4638653e045cf94f0762efd92d1eda4dcd6c32219497dece9cb12da2a5b5483e6afba7ab393e7700ac4",
	Runsc:     "e84869b5dab0347954b146b89eadf7f428505c705a3c8993b973cd7091260f07f11e4dd6d86b9b672f6b029211ca2a317b18c73cc90e85ad1356c223b0842573",
	RunscShim: "b3ea938882893dc411a07b167dfd50378025e88a94bcd091ac07c8b6e7af347f70259b3b2890e072467becc3c199d3b322c3b1c48809e1274328bbd099104f73",
}}}

func TestMain(m *testing.M) {
	Gvisor = testGvisor
	SilodTimeout = time.Second
	os.Exit(m.Run())
}

func newFake() *fakeHost {
	ok := func(names ...string) []nsd.Check {
		var out []nsd.Check
		for _, n := range names {
			out = append(out, nsd.Check{Name: n, OK: true, Detail: "fine"})
		}
		return out
	}
	tarball, err := os.ReadFile("testdata/gvisor.tar.bz2")
	if err != nil {
		panic(err)
	}
	return &fakeHost{
		files: map[string]string{"/etc/subuid": "op:100000:65536\n", "/etc/subgid": "op:100000:65536\n",
			"/proc/sys/kernel/osrelease": "6.1.0-40-amd64\n", "/sys/fs/cgroup/cgroup.controllers": "cpu memory io\n",
			"/run/systemd/system": "", "/proc/meminfo": "MemTotal:       16777216 kB\nMemFree: 1 kB\n", "/": ""},
		users: map[string]*user.User{"op": {Username: "op", Uid: "1000", Gid: "1000", HomeDir: "/home/op"},
			"root": {Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"}},
		env:     map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
		tarball: tarball,
		kvm:     map[string]bool{},
		enabled: map[string]bool{}, active: map[string]bool{},
		failing: map[string]error{},
		checks:  ok("unprivileged user namespaces", "newuidmap", "newgidmap", "range in /etc/subuid", "range in /etc/subgid"),
		runtimes: rootless.Report{Checks: ok("containerd", "runsc", "containerd-shim-runsc-v1", "crun", "buildkitd",
			"/dev/kvm through the kvm group", "cgroup delegation")},
	}
}

func (f *fakeHost) host(euid int, progress *bytes.Buffer) Host {
	return Host{
		EUID: euid,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			cmd := strings.Join(append([]string{name}, args...), " ")
			if err := f.failing[name]; err != nil {
				return nil, err
			}
			args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--user" })
			switch {
			case name == "systemctl" && args[0] == "is-enabled":
				if f.enabled[args[1]] {
					return []byte("enabled\n"), nil
				}
				return []byte("disabled\n"), errors.New("exit 1")
			case name == "systemctl" && args[0] == "is-active":
				if f.active[args[1]] {
					return []byte("active\n"), nil
				}
				return []byte("inactive\n"), errors.New("exit 3")
			case name == "systemctl" && args[0] == "enable":
				for _, u := range args[1:] {
					f.enabled[u] = true
				}
			case name == "systemctl" && (args[0] == "start" || args[0] == "restart"):
				f.active[args[1]] = true
			case name == "systemctl" && args[0] == "stop":
				f.active[args[1]] = false
			case name == "systemctl" && args[0] == "disable":
				for _, u := range args[1:] {
					f.enabled[u] = false
				}
			case name == "useradd":
				n := args[len(args)-1]
				f.users[n] = &user.User{Username: n, Uid: "990", Gid: "990"}
			case name == "loginctl":
				f.files["/var/lib/systemd/linger/"+args[1]] = ""
			}
			f.ran = append(f.ran, cmd)
			return nil, nil
		},
		Probe: func(Config) ([]nsd.Check, rootless.Report) { return f.checks, f.runtimes },
		Lookup: func(name string) (*user.User, error) {
			if u, ok := f.users[name]; ok {
				return u, nil
			}
			return nil, user.UnknownUserError(name)
		},
		GroupExists: func(name string) bool { return name == "kvm" },
		InGroup:     func(u *user.User, g string) bool { return g == "kvm" && f.kvm[u.Username] },
		ReadFile: func(name string) ([]byte, error) {
			if s, ok := f.files[name]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		WriteFile: func(name string, b []byte) error { f.files[name] = string(b); return nil },
		Stat: func(name string) (os.FileInfo, error) {
			for n := range f.files {
				if n == name || strings.HasPrefix(n, name+"/") {
					return fakeInfo{}, nil
				}
			}
			return nil, fs.ErrNotExist
		},
		Open: func(name string) (io.ReadCloser, error) {
			if s, ok := f.files[name]; ok {
				return io.NopCloser(strings.NewReader(s)), nil
			}
			return nil, fs.ErrNotExist
		},
		Install: func(name string, r io.Reader) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			f.files[name] = string(b)
			return nil
		},
		RemoveAll: func(name string) error {
			for n := range f.files {
				if n == name || strings.HasPrefix(n, name+"/") {
					delete(f.files, n)
				}
			}
			f.ran = append(f.ran, "rm -r "+name)
			return nil
		},
		OpenDataDir: func(path string) error {
			f.files[filepath.Join(path, "layout.json")] = `{"layout":2}`
			return nil
		},
		Fetch: func(context.Context, string) (io.ReadCloser, error) {
			f.fetched++
			return io.NopCloser(bytes.NewReader(f.tarball)), nil
		},
		Silod:    func(path string) (Silod, error) { return fakeSilod{f: f, path: path}, nil },
		Env:      func(k string) string { return f.env[k] },
		Statfs:   func(string) (FS, error) { return FS{Type: 0xef53, Size: 100 << 30}, nil },
		CPUs:     8,
		Arch:     "amd64",
		Self:     "/usr/bin/silo",
		UnitDir:  "/units",
		Progress: progress,
	}
}

type fakeSilod struct {
	f    *fakeHost
	path string
}

func (s fakeSilod) Health(context.Context) (install.SilodHealth, error) {
	if !s.f.active[units.Silod] {
		return install.SilodHealth{}, errors.New("dial unix " + s.path + ": connect: no such file or directory")
	}
	return install.SilodHealth{Status: "ok", Root: s.f.rootID != "", RootID: s.f.rootID}, nil
}

func (s fakeSilod) Bootstrap(_ context.Context, req *install.BootstrapRequest) error {
	if s.f.rootID != "" {
		return errors.New("409: the root silo exists")
	}
	s.f.boot, s.f.rootID = req, "r00t"
	return nil
}

type fakeInfo struct{}

func (fakeInfo) Name() string       { return "x" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return true }
func (fakeInfo) Sys() any           { return nil }

func rootlessConfig() Config {
	return Config{Mode: units.Rootless, User: "op", DataDir: "/home/op/.local/share/silo", Silod: "/usr/bin/silod"}
}

// changes is a plan's lines as "step action object".
func changes(plans []install.StepPlan) []string {
	var out []string
	for _, p := range plans {
		if p.Err != nil {
			out = append(out, p.Step+" error "+p.Err.Error())
		}
		for _, c := range p.Changes {
			out = append(out, p.Step+" "+string(c.Action)+" "+c.Object)
		}
	}
	return out
}

func hasAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q in:\n  %s", w, strings.Join(got, "\n  "))
		}
	}
}

func TestRootlessInstall(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	var progress bytes.Buffer
	c := rootlessConfig()
	steps := Steps(c, f.host(1000, &progress))
	plans := install.PlanAll(ctx, steps)
	got := changes(plans)
	hasAll(t, got,
		"prerequisites ok classes",
		"prerequisites warn /run/silo",
		"lingering enable op",
		"units create silo-nsd.service", "units create silo-containerd.service",
		"units create silo-buildkitd.service", "units create silod.service",
		"prerequisites ok Linux", "prerequisites ok cgroup v2", "prerequisites ok systemd", "prerequisites ok nftables",
		"prerequisites warn reflinks",
		"data create /home/op/.local/share/silo",
		"runtimes create runsc", "runtimes create containerd-shim-runsc-v1",
		"root create /", "docker context create silo-root")
	for _, p := range plans {
		if p.Failed() {
			t.Fatalf("step %s failed: %v", p.Step, got)
		}
	}
	if err := install.Apply(ctx, steps, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.files["/units/silo-nsd.service"], "ExecStart=/usr/bin/silod nsd") {
		t.Errorf("silo-nsd.service:\n%s", f.files["/units/silo-nsd.service"])
	}
	want := []string{
		"loginctl enable-linger op",
		"systemctl --user daemon-reload",
		"systemctl --user enable silo-nsd.service silo-containerd.service silo-buildkitd.service silod.service",
		"systemctl --user start silo-nsd.service",
		"systemctl --user start silo-containerd.service",
		"systemctl --user start silo-buildkitd.service",
		"systemctl --user start silod.service",
	}
	var acted []string
	for _, r := range f.ran {
		if !strings.Contains(r, "is-enabled") && !strings.Contains(r, "is-active") && !strings.HasPrefix(r, "unshare ") {
			acted = append(acted, r)
		}
	}
	if !slices.Equal(acted, want) {
		t.Errorf("ran:\n  %s\nwant:\n  %s", strings.Join(acted, "\n  "), strings.Join(want, "\n  "))
	}

	// The data directory was opened before the runtimes were written
	// into it, and the pinned runtimes are first on the runtimes' PATH.
	data := "/home/op/.local/share/silo"
	rt := data + "/cache/runtimes/runsc-1"
	if _, ok := f.files[data+"/layout.json"]; !ok {
		t.Error("no layout.json")
	}
	if f.files[rt+"/runsc"] != "#!/bin/sh\necho runsc\n" || f.files[rt+"/containerd-shim-runsc-v1"] != "#!/bin/sh\necho shim\n" {
		t.Errorf("runtimes not installed: %q", f.files[rt+"/runsc"])
	}
	if !strings.Contains(f.files["/units/silo-containerd.service"], "Environment=PATH="+rt+":") {
		t.Errorf("silo-containerd.service:\n%s", f.files["/units/silo-containerd.service"])
	}

	// The root: backing local, both classes, ingress host, the installer
	// as admin, and the capacity call L20 gives 8 CPUs, 16 GiB and 100 GiB.
	b := f.boot
	if b == nil {
		t.Fatal("no bootstrap")
	}
	if b.Backing.Kind != "local" || b.Ingress != "host" || !slices.Equal(b.Admins, []string{"uid:1000"}) ||
		!slices.Equal(b.Isolation.Allowed, []string{"sandbox", "vm"}) || b.Isolation.Default != "vm" {
		t.Errorf("bootstrap: %+v, isolation %+v", b, b.Isolation)
	}
	for class, want := range map[string]string{"vcpu_m": "14000", "memory_mb": "14336", "disk_gb": "80", "containers": "1000"} {
		if got := b.Capacity["global"][class]; got != want {
			t.Errorf("capacity %s = %s, want %s", class, got, want)
		}
	}

	// The docker context names the root's socket in the installer's
	// runtime directory, there being no /run/silo.
	ctxFile := ""
	for n := range f.files {
		if strings.HasPrefix(n, "/home/op/.docker/contexts/meta/") && strings.HasSuffix(n, "/meta.json") {
			ctxFile = n
		}
	}
	if !strings.Contains(f.files[ctxFile], `"Host":"unix:///run/user/1000/silo/docker/r00t.sock"`) {
		t.Errorf("docker context %s: %s", ctxFile, f.files[ctxFile])
	}
	if !strings.Contains(progress.String(), "docker context use silo-root") {
		t.Errorf("no docker-env hint:\n%s", progress.String())
	}

	// A second install changes nothing.
	again := install.PlanAll(ctx, steps)
	for _, p := range again {
		if p.Changing() || p.Failed() {
			t.Errorf("second install: %s: %v", p.Step, changes([]install.StepPlan{p}))
		}
	}

	// A changed unit (a new silod) is rewritten and restarted, the
	// others are left running.
	c.Silod = "/opt/silo/silod"
	f.ran = nil
	steps = Steps(c, f.host(1000, &progress))
	hasAll(t, changes(install.PlanAll(ctx, steps)), "units update silo-nsd.service", "units update silod.service")
	if err := install.Apply(ctx, steps, &progress); err != nil {
		t.Fatal(err)
	}
	hasAll(t, f.ran, "systemctl --user restart silo-nsd.service", "systemctl --user restart silod.service")
}

func TestRootlessRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("as root", func(t *testing.T) {
		f := newFake()
		hasAll(t, changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(0, &bytes.Buffer{})))), "prerequisites fail installer")
	})
	t.Run("no newuidmap", func(t *testing.T) {
		f := newFake()
		f.checks[1] = nsd.Check{Name: "newuidmap", Detail: "not found", Fix: "apt-get install uidmap"}
		hasAll(t, changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{})))), "prerequisites fail newuidmap")
	})
	t.Run("no class", func(t *testing.T) {
		f := newFake()
		for i, ch := range f.runtimes.Checks {
			if ch.Name == "runsc" || ch.Name == "crun" {
				f.runtimes.Checks[i].OK = false
			}
		}
		// Where gVisor is pinned, install fetches runsc.
		got := changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{}))))
		hasAll(t, got, "prerequisites ok runsc", "prerequisites warn crun", "prerequisites ok classes")
		h := f.host(1000, &bytes.Buffer{})
		h.Arch = "riscv64"
		got = changes(install.PlanAll(ctx, Steps(rootlessConfig(), h)))
		hasAll(t, got, "prerequisites warn runsc", "prerequisites warn crun", "prerequisites fail classes", "runtimes warn gVisor")
	})
	t.Run("old kernel", func(t *testing.T) {
		f := newFake()
		f.files["/proc/sys/kernel/osrelease"] = "5.10.0-30-amd64\n"
		delete(f.files, "/sys/fs/cgroup/cgroup.controllers")
		hasAll(t, changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{})))),
			"prerequisites fail Linux", "prerequisites fail cgroup v2")
	})
	t.Run("no nftables", func(t *testing.T) {
		f := newFake()
		f.failing["unshare"] = errors.New("nft: not found")
		hasAll(t, changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{})))), "prerequisites fail nftables")
	})
	t.Run("no buildkitd", func(t *testing.T) {
		f := newFake()
		for i, ch := range f.runtimes.Checks {
			if ch.Name == "buildkitd" {
				f.runtimes.Checks[i].OK = false
			}
		}
		got := changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{}))))
		hasAll(t, got, "prerequisites warn buildkitd", "prerequisites ok classes")
		if slices.Contains(got, "units create silo-buildkitd.service") {
			t.Error("silo-buildkitd.service planned without buildkitd")
		}
	})
}

func TestLingerRefusedWarns(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.failing["loginctl"] = errors.New("access denied")
	var progress bytes.Buffer
	if err := install.Apply(ctx, Steps(rootlessConfig(), f.host(1000, &progress)), &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "sudo loginctl enable-linger op") {
		t.Errorf("no warning naming the fix:\n%s", progress.String())
	}
}

func TestSystemInstall(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.runtimes.Checks[5].OK = false // silo isn't in kvm yet
	c := Config{Mode: units.System, User: "silo", DataDir: "/var/lib/silo", Silod: "/usr/bin/silod"}

	hasAll(t, changes(install.PlanAll(ctx, Steps(c, f.host(1000, &bytes.Buffer{})))), "prerequisites fail installer")

	var progress bytes.Buffer
	f.env["SUDO_USER"], f.env["SUDO_UID"] = "op", "1000"
	steps := Steps(c, f.host(0, &progress))
	got := changes(install.PlanAll(ctx, steps))
	hasAll(t, got, "account create user silo", "account update group kvm", "account create subordinate ids",
		"account create /var/lib/silo", "prerequisites ok classes", "prerequisites ok /run/silo",
		"units create silod.service")
	for _, bad := range []string{"lingering enable silo", "prerequisites fail range in /etc/subuid"} {
		if slices.Contains(got, bad) {
			t.Errorf("system plan has %q", bad)
		}
	}
	if err := install.Apply(ctx, steps, &progress); err != nil {
		t.Fatal(err)
	}
	hasAll(t, f.ran,
		"useradd --system --user-group --home-dir /var/lib/silo --no-create-home --shell /usr/sbin/nologin --comment Silo (silod) silo",
		"usermod --append --groups kvm silo",
		"usermod --add-subuids 1048576-17825791 --add-subgids 1048576-17825791 silo",
		"install -d -o silo -g silo -m 0700 /var/lib/silo",
		"systemctl daemon-reload",
		"systemctl start silod.service")
	if !strings.Contains(f.files["/units/silod.service"], "User=silo") {
		t.Errorf("silod.service:\n%s", f.files["/units/silod.service"])
	}
	// The data directory root made is silo's; the docker context is the
	// sudo user's, and names /run/silo's socket.
	hasAll(t, f.ran, "chown -R silo:silo /var/lib/silo", "chown -R op: /home/op/.docker/contexts")
	if !slices.Equal(f.boot.Admins, []string{"uid:1000"}) {
		t.Errorf("admins %v, want the sudo user's uid", f.boot.Admins)
	}
	found := false
	for n, v := range f.files {
		if strings.HasPrefix(n, "/home/op/.docker/contexts/meta/") {
			found = strings.Contains(v, `"Host":"unix:///run/silo/docker/r00t.sock"`)
		}
	}
	if !found {
		t.Error("no docker context for /run/silo/docker/r00t.sock in op's configuration")
	}
	for _, r := range f.ran {
		if strings.HasPrefix(r, "systemctl --user") {
			t.Errorf("the system install ran %q", r)
		}
	}
}

func TestSubIDs(t *testing.T) {
	for next, want := range map[uint64]string{
		1 << 20:   "1048576-17825791",
		1<<20 + 1: "1114112-17891327",
	} {
		if got := subIDs(next); got != want {
			t.Errorf("subIDs(%d) = %s, want %s", next, got, want)
		}
	}
}

// --registry-host-port reaches silod's unit as --registry-host-listen
// on 127.0.0.1, and turning it on rewrites silod.service alone.
func TestRegistryHostPort(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	var progress bytes.Buffer
	c := rootlessConfig()
	if err := install.Apply(ctx, Steps(c, f.host(1000, &progress)), &progress); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.files["/units/silod.service"], "--registry-host-listen") {
		t.Errorf("port 0 listens:\n%s", f.files["/units/silod.service"])
	}
	c.RegistryHostPort = 5115
	steps := Steps(c, f.host(1000, &progress))
	got := changes(install.PlanAll(ctx, steps))
	hasAll(t, got, "units update silod.service")
	for _, g := range got {
		if strings.HasPrefix(g, "units update") && g != "units update silod.service" {
			t.Errorf("turning the port on changed %s", g)
		}
	}
	if err := install.Apply(ctx, steps, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.files["/units/silod.service"], " --registry-host-listen 127.0.0.1:5115 --image-cache-size 10\n") {
		t.Errorf("silod.service:\n%s", f.files["/units/silod.service"])
	}
}

// The cache budget is 10% of the data filesystem, at least 10 GiB, and
// comes out of the root's disk_gb (local-registry.md, call L74), as
// TestRootlessInstall's disk_gb shows.
func TestImageCacheBudget(t *testing.T) {
	for fs, want := range map[int64]float64{50 << 30: 10, 1000 << 30: 100} {
		if got := DefaultImageCacheGB(fs); got != want {
			t.Errorf("DefaultImageCacheGB(%d GiB) = %v, want %v", fs>>30, got, want)
		}
	}
	c := rootlessConfig()
	f := newFake()
	if got := imageCacheGB(c, f.host(1000, &bytes.Buffer{})); got != 10 {
		t.Errorf("the default budget on 100 GiB: %v GiB", got)
	}
	c.ImageCacheSizeGB = 25
	if got := imageCacheGB(c, f.host(1000, &bytes.Buffer{})); got != 25 {
		t.Errorf("--image-cache-size 25: %v GiB", got)
	}
}

// A tarball or binary that isn't the pinned one is refused, and
// nothing is installed from it; an installed binary that has changed
// is fetched again.
func TestRuntimesChecked(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.tarball = append(f.tarball, 0)
	var progress bytes.Buffer
	err := install.Apply(ctx, Steps(rootlessConfig(), f.host(1000, &progress)), &progress)
	if err == nil || !strings.Contains(err.Error(), "is not the pinned") {
		t.Fatalf("Apply = %v, want a refused sum", err)
	}
	for n := range f.files {
		if strings.Contains(n, "/cache/runtimes/") {
			t.Errorf("installed %s from a bad tarball", n)
		}
	}

	f = newFake()
	if err := install.Apply(ctx, Steps(rootlessConfig(), f.host(1000, &progress)), &progress); err != nil {
		t.Fatal(err)
	}
	f.files["/home/op/.local/share/silo/cache/runtimes/runsc-1/runsc"] = "tampered"
	steps := Steps(rootlessConfig(), f.host(1000, &progress))
	hasAll(t, changes(install.PlanAll(ctx, steps)), "runtimes update runsc", "runtimes ok containerd-shim-runsc-v1")
	if err := install.Apply(ctx, steps, &progress); err != nil {
		t.Fatal(err)
	}
	if f.fetched != 2 || f.files["/home/op/.local/share/silo/cache/runtimes/runsc-1/runsc"] != "#!/bin/sh\necho runsc\n" {
		t.Errorf("fetched %d times, runsc %q", f.fetched, f.files["/home/op/.local/share/silo/cache/runtimes/runsc-1/runsc"])
	}
}

// Uninstall stops every container by killing the runtimes' cgroups,
// removes the units, the runtimes and the docker context, and keeps the
// data directory, unless --delete-data, which first removes the
// containers' files inside the holder.
func TestUninstall(t *testing.T) {
	ctx := context.Background()
	for _, deleteData := range []bool{false, true} {
		f := newFake()
		var progress bytes.Buffer
		if err := install.Apply(ctx, Steps(rootlessConfig(), f.host(1000, &progress)), &progress); err != nil {
			t.Fatal(err)
		}
		f.ran = nil
		c := rootlessConfig()
		c.Uninstall, c.DeleteData, c.Silod = true, deleteData, ""
		steps := Steps(c, f.host(1000, &progress))
		plans := install.PlanRemoveAll(ctx, steps)
		got := changes(plans)
		hasAll(t, got, "docker context delete silo-root", "root keep /", "units delete containers",
			"units delete silod.service", "units delete silo-nsd.service",
			"runtimes delete /home/op/.local/share/silo/cache/runtimes/runsc-1", "lingering keep op")
		data := "data keep /home/op/.local/share/silo"
		if deleteData {
			data = "data delete /home/op/.local/share/silo"
		}
		hasAll(t, got, data)
		if err := install.Remove(ctx, steps, &progress); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"systemctl --user stop silod.service",
			"systemctl --user kill --signal=SIGKILL silo-buildkitd.service",
			"systemctl --user stop silo-buildkitd.service",
			"systemctl --user kill --signal=SIGKILL silo-containerd.service",
			"systemctl --user stop silo-containerd.service",
			"systemctl --user kill --signal=SIGKILL silo-sandboxes.scope",
			"systemctl --user stop silo-sandboxes.scope",
			"systemctl --user stop silo-nsd.service",
			"systemctl --user disable silo-nsd.service silo-containerd.service silo-buildkitd.service silod.service",
			"systemctl --user daemon-reload",
		}
		if deleteData {
			want = slices.Insert(want, 7, "/usr/bin/silo unshare -- rm -rf /home/op/.local/share/silo/silos /home/op/.local/share/silo/containerd /home/op/.local/share/silo/cache")
		}
		var acted []string
		for _, r := range f.ran {
			if strings.HasPrefix(r, "systemctl") && !strings.Contains(r, "is-") || strings.HasPrefix(r, "/usr/bin/silo") {
				acted = append(acted, r)
			}
		}
		if !slices.Equal(acted, want) {
			t.Errorf("delete-data %v: ran:\n  %s\nwant:\n  %s", deleteData, strings.Join(acted, "\n  "), strings.Join(want, "\n  "))
		}
		for n := range f.files {
			if strings.HasPrefix(n, "/units/") || strings.Contains(n, "/cache/runtimes/") || strings.Contains(n, "/.docker/contexts/") {
				t.Errorf("delete-data %v: %s is left", deleteData, n)
			}
		}
		if _, kept := f.files["/home/op/.local/share/silo/layout.json"]; kept == deleteData {
			t.Errorf("delete-data %v: data directory kept %v", deleteData, kept)
		}
		// A second uninstall has nothing to do.
		for _, p := range install.PlanRemoveAll(ctx, Steps(c, f.host(1000, &progress))) {
			if p.Changing() || p.Failed() {
				t.Errorf("second uninstall: %v", changes([]install.StepPlan{p}))
			}
		}
	}
}
