package rootless

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// runcSpec is the part of `runc spec`'s default that the patches touch.
const runcSpec = `{
	"ociVersion": "1.0.2",
	"process": {"user": {"uid": 0, "gid": 0}, "args": ["sh"]},
	"mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}, {"destination": "/dev/kvm", "type": "tmpfs", "source": "tmpfs"}],
	"linux": {
		"uidMappings": [{"containerID": 0, "hostID": 1, "size": 65536}],
		"gidMappings": [{"containerID": 0, "hostID": 1, "size": 65536}],
		"namespaces": [{"type": "pid"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}, {"type": "user"}]
	}
}`

type spec struct {
	Annotations map[string]string `json:"annotations"`
	Process     struct {
		User struct {
			AdditionalGids []uint32 `json:"additionalGids"`
		} `json:"user"`
	} `json:"process"`
	Mounts []Mount `json:"mounts"`
	Linux  struct {
		CgroupsPath string            `json:"cgroupsPath"`
		Namespaces  []Namespace       `json:"namespaces"`
		UIDMappings []json.RawMessage `json:"uidMappings"`
	} `json:"linux"`
}

func apply(t *testing.T, p Patch) spec {
	t.Helper()
	b, err := p.Apply([]byte(runcSpec))
	if err != nil {
		t.Fatal(err)
	}
	var s spec
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func nsPaths(s spec) map[string]string {
	m := map[string]string{}
	for _, n := range s.Linux.Namespaces {
		m[n.Type] = n.Path
	}
	return m
}

var testNS = nsd.ContainerNS{User: "/run/silo/netns/c1.user", Net: "/run/silo/netns/c1.net", Range: nsd.Range{Inside: 0, Outside: 65537, Size: 65536}}

func TestSandboxPatch(t *testing.T) {
	s := apply(t, SandboxPatch(testNS, CgroupsPath("c1", "aaaaaa")))
	ns := nsPaths(s)
	if _, ok := ns["user"]; ok {
		t.Errorf("the sandbox spec names a user namespace; runsc is started inside it instead: %v", ns)
	}
	if ns["network"] != testNS.Net || len(ns) != 5 {
		t.Errorf("namespaces = %v, want the pinned netns and the other four", ns)
	}
	if len(s.Linux.UIDMappings) != 0 {
		t.Errorf("uid mappings kept: %s", s.Linux.UIDMappings)
	}
	if s.Annotations[AnnotationUserNS] != testNS.User || s.Annotations[AnnotationRoot] != "65537" {
		t.Errorf("annotations = %v", s.Annotations)
	}
	if s.Linux.CgroupsPath != "silo-aaaaaa.slice:silo:c1" {
		t.Errorf("cgroupsPath = %q", s.Linux.CgroupsPath)
	}
}

func TestVMPatch(t *testing.T) {
	s := apply(t, VMPatch(testNS, CgroupsPath("c1", "aaaaaa", "bbbbbb"), nil))
	ns := nsPaths(s)
	if ns["user"] != testNS.User || ns["network"] != testNS.Net || len(ns) != 6 {
		t.Errorf("namespaces = %v, want both pins joined by path", ns)
	}
	if s.Annotations[AnnotationKeepGroups] != "1" {
		t.Errorf("annotations = %v, want %s", s.Annotations, AnnotationKeepGroups)
	}
	if len(s.Linux.UIDMappings) != 0 {
		t.Errorf("uid mappings kept for a joined user namespace: %s", s.Linux.UIDMappings)
	}
	var devs []string
	for _, m := range s.Mounts {
		if strings.HasPrefix(m.Destination, "/dev/") {
			devs = append(devs, m.Destination)
			if m.Type != "bind" || m.Source != m.Destination {
				t.Errorf("device %s is not a bind mount of itself: %+v", m.Destination, m)
			}
		}
	}
	if !slices.Equal(devs, VMDevices) {
		t.Errorf("device mounts = %v, want %v (replacing the spec's own /dev/kvm)", devs, VMDevices)
	}
	if s.Linux.CgroupsPath != "silo-aaaaaa-bbbbbb.slice:silo:c1" {
		t.Errorf("cgroupsPath = %q", s.Linux.CgroupsPath)
	}

	gid := uint32(70000)
	s = apply(t, VMPatch(testNS, "", &gid))
	if _, ok := s.Annotations[AnnotationKeepGroups]; ok {
		t.Error("the mapped-gid fallback still asks crun to keep groups")
	}
	if !slices.Equal(s.Process.User.AdditionalGids, []uint32{gid}) {
		t.Errorf("additionalGids = %v", s.Process.User.AdditionalGids)
	}
}

func TestSlices(t *testing.T) {
	for _, c := range []struct {
		path []string
		want string
	}{{nil, "silo.slice"}, {[]string{"abc234"}, "silo-abc234.slice"}, {[]string{"abc234", "zz7777"}, "silo-abc234-zz7777.slice"}} {
		if got := Slice(c.path...); got != c.want {
			t.Errorf("Slice(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestConfigs(t *testing.T) {
	p := Paths{DataDir: "/d", RuntimeDir: "/r/silo"}
	cfg := ContainerdConfigTOML(p)
	for _, want := range []string{`version = 3`, `root = "/d/cache/containerd"`, `state = "/run/containerd"`, `address = "/r/silo/containerd.sock"`, `"io.containerd.grpc.v1.cri"`, `tcp_address = ""`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("containerd config lacks %s:\n%s", want, cfg)
		}
	}
	args := strings.Join(BuildkitdArgs(p, "buildkitd"), " ")
	for _, want := range []string{"--oci-worker=false", "--containerd-worker-addr /r/silo/containerd.sock", "--containerd-worker-namespace silo-build", "--root /d/cache/buildkit", "unix:///r/silo/buildkitd.sock"} {
		if !strings.Contains(args, want) {
			t.Errorf("buildkitd args lack %s: %s", want, args)
		}
	}
	opts := RunscOptionsTOML(p, "/r/silo/runsc.toml.sh")
	for _, want := range []string{`binary_name = "/r/silo/runsc.toml.sh"`, `platform = "systrap"`, `network = "sandbox"`, `overlay2 = "root:self"`, `root = "/run/silo/runsc"`} {
		if !strings.Contains(opts, want) {
			t.Errorf("runsc options lack %s:\n%s", want, opts)
		}
	}
	if got := RunscWrapperScript("/o'k/silod", "/usr/bin/runsc"); got != "#!/bin/sh\nexec '/o'\\''k/silod' runsc --runsc '/usr/bin/runsc' -- \"$@\"\n" {
		t.Errorf("wrapper script = %q", got)
	}
}

func TestRunscBundle(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--root=/r", "create", "--bundle", "/b", "--pid-file", "/b/p", "c1"}, "/b"},
		{[]string{"run", "-bundle", "/b2", "c1"}, "/b2"},
		{[]string{"--root", "/r", "kill", "c1", "9"}, "/cwd"},
		{[]string{"state", "--bundle=/b3", "c1"}, "/b3"},
	} {
		if got := RunscBundle(c.args, "/cwd"); got != c.want {
			t.Errorf("RunscBundle(%v) = %q, want %q", c.args, got, c.want)
		}
	}
	if got := runscRoot([]string{"--log=x", "--root=/run/silo/runsc", "state", "c1"}); got != "/run/silo/runsc" {
		t.Errorf("runscRoot = %q", got)
	}
}

func TestRunscUserNSRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(ann map[string]string) {
		b, _ := json.Marshal(map[string]any{"annotations": ann})
		if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := RunscUserNS(filepath.Join(dir, "missing")); !errors.Is(err, ErrNoUserNS) {
		t.Errorf("no bundle: %v", err)
	}
	write(nil)
	if _, _, err := RunscUserNS(dir); !errors.Is(err, ErrNoUserNS) {
		t.Errorf("no annotation: %v", err)
	}
	write(map[string]string{AnnotationUserNS: "/run/silo/netns/c1.user", AnnotationRoot: "0"})
	if _, _, err := RunscUserNS(dir); !errors.Is(err, ErrNoUserNS) {
		t.Errorf("the holder's own root: %v", err)
	}
	write(map[string]string{AnnotationUserNS: "/run/silo/netns/c1.user", AnnotationRoot: "65537"})
	if ns, root, err := RunscUserNS(dir); err != nil || ns != "/run/silo/netns/c1.user" || root != 65537 {
		t.Errorf("RunscUserNS = %q, %d, %v", ns, root, err)
	}
}

func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestProbeCgroups(t *testing.T) {
	const mgr = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers"
	root := "/sys/fs/cgroup/cgroup.controllers"
	for _, c := range []struct {
		name     string
		files    map[string]string
		enforced string
		missing  []string
		detail   string
	}{
		{"systemd's default delegation", map[string]string{root: "cpuset cpu io memory pids", mgr: "cpu memory pids"}, "partial", []string{"cpuset", "io"}, "I/O is neither limited nor metered"},
		{"the drop-in", map[string]string{root: "cpuset cpu io memory pids", mgr: "cpuset cpu io memory pids"}, "all", nil, "limits hold"},
		{"no memory", map[string]string{root: "cpuset cpu io memory pids", mgr: "cpu pids"}, "none", []string{"cpuset", "io", "memory"}, "memory is not delegated"},
		{"no user manager", map[string]string{root: "cpuset cpu io memory pids"}, "none", Wanted, "is not running"},
		{"cgroup v1", map[string]string{"/proc/self/cgroup": "12:memory:/"}, "none", Wanted, "cgroup v1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ProbeCgroups(CgroupHost{ReadFile: fakeFiles(c.files), UID: 1000, SystemdVersion: func() int { return 252 }})
			if got.Enforced != c.enforced || !slices.Equal(got.Missing, c.missing) || !strings.Contains(got.Detail, c.detail) {
				t.Errorf("got %+v, want enforced %s, missing %v, detail containing %q", got, c.enforced, c.missing, c.detail)
			}
			if (got.Enforced == "all") != (got.Fix == "") {
				t.Errorf("fix %q for enforced %s", got.Fix, got.Enforced)
			}
		})
	}
	got := ProbeCgroups(CgroupHost{ReadFile: fakeFiles(map[string]string{root: "cpu", mgr: "cpu memory pids io cpuset"}), UID: 1000, SystemdVersion: func() int { return 239 }})
	if !strings.Contains(got.Detail, "older than 244") {
		t.Errorf("an old systemd isn't reported: %s", got.Detail)
	}
	if v := parseSystemdVersion("systemd 252 (252.39-1~deb12u2)\n+PAM +AUDIT"); v != 252 {
		t.Errorf("parseSystemdVersion = %d", v)
	}
}
