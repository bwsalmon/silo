package local

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
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
}

func newFake() *fakeHost {
	ok := func(names ...string) []nsd.Check {
		var out []nsd.Check
		for _, n := range names {
			out = append(out, nsd.Check{Name: n, OK: true, Detail: "fine"})
		}
		return out
	}
	return &fakeHost{
		files:   map[string]string{"/etc/subuid": "op:100000:65536\n", "/etc/subgid": "op:100000:65536\n"},
		users:   map[string]*user.User{"op": {Username: "op", Uid: "1000", Gid: "1000"}},
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
			if _, ok := f.files[name]; ok {
				return fakeInfo{}, nil
			}
			return nil, fs.ErrNotExist
		},
		UnitDir:  "/units",
		Progress: progress,
	}
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
		"units create silo-buildkitd.service", "units create silod.service")
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
		if !strings.Contains(r, "is-enabled") && !strings.Contains(r, "is-active") {
			acted = append(acted, r)
		}
	}
	if !slices.Equal(acted, want) {
		t.Errorf("ran:\n  %s\nwant:\n  %s", strings.Join(acted, "\n  "), strings.Join(want, "\n  "))
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
		got := changes(install.PlanAll(ctx, Steps(rootlessConfig(), f.host(1000, &bytes.Buffer{}))))
		hasAll(t, got, "prerequisites warn runsc", "prerequisites warn crun", "prerequisites fail classes")
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
