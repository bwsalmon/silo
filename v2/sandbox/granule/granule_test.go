package granule

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/v2/sandbox"
)

// fakeCLI writes a fake binary that appends its argv, one argument per
// line and a blank line after, to log, and its stdin to log.stdin, then
// runs body.
func fakeCLI(t *testing.T, dir, name, log, body string) string {
	return fakeCLIStdin(t, dir, name, log, body, true)
}

func fakeCLIStdin(t *testing.T, dir, name, log, body string, logStdin bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n{ printf '%s\\n' \"$@\"; echo; } >> " + log + "\n"
	if logStdin {
		script += "if [ ! -t 0 ]; then cat >> " + log + ".stdin; fi\n"
	}
	script += body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// calls reads a fake's log back as one argv per call.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, c := range strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n") {
		out = append(out, strings.Split(c, "\n"))
	}
	return out
}

func fakeBackend(t *testing.T, granuleBody, dockerBody string) (*Backend, string, string) {
	t.Helper()
	dir := t.TempDir()
	glog, dlog := filepath.Join(dir, "granule.log"), filepath.Join(dir, "docker.log")
	return &Backend{
		Granule: fakeCLI(t, dir, "granule", glog, granuleBody),
		Docker:  fakeCLI(t, dir, "docker", dlog, dockerBody),
	}, glog, dlog
}

func TestVMName(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	a, b := VMName("s-abcdefghijkl", "dev"), VMName("s-abcdefghijkl", "build")
	for _, n := range []string{a, b} {
		// granule names a tap device "tap-<name>", at most 15 characters.
		if len("tap-"+n) > 15 || !label.MatchString(n) {
			t.Errorf("VM name %q is not a DNS-1123 label of at most 11 characters", n)
		}
	}
	if a == b || a != VMName("s-abcdefghijkl", "dev") {
		t.Errorf("VM names are not distinct and stable: %q, %q", a, b)
	}
}

func TestCreateArgv(t *testing.T) {
	b, glog, dlog := fakeBackend(t, "", `case "$1" in image) echo "sha256:abc example.com/guest@sha256:def";; esac`)
	b.StateDir = "/state"
	spec := sandbox.SandboxSpec{
		Session: "s-abcdefghijkl", Name: "dev", Kind: "granule", Image: "example.com/guest:1",
		Workdir: "/work", CPUs: 1.5, Memory: 3 << 30, Disk: 20<<30 + 1, Network: "none",
		PrivateNetwork: "silo-s-abcdefghijkl",
		Env:            map[string]string{"TOKEN": "it's secret", "A": "1"},
	}
	sb, err := b.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	vm := VMName(spec.Session, spec.Name)
	uid := strconv.Itoa(os.Getuid())
	got := calls(t, glog)
	want := [][]string{
		{"vm", "create", vm, "-state-dir", "/state", "-kontur-image", "example.com/guest:1",
			"-cpus", "2", "-memory-mb", "3072", "-disk-size-mb", "20481",
			"-docker-run-opt=--label=silo.session=s-abcdefghijkl",
			"-docker-run-opt=--label=silo.sandbox=dev",
			"-docker-run-opt=--label=silo.uid=" + uid,
			"-docker-run-opt=--label=silo.workdir=/work",
			"-docker-run-opt=--network=silo-s-abcdefghijkl"},
		{"vm", "wait", vm, "-state-dir", "/state", "-timeout", "3m0s"},
	}
	if len(got) != 3 {
		t.Fatalf("granule calls = %q, want create, wait and the guest's setup", got)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("granule call %d =\n  %q\nwant\n  %q", i, got[i], want[i])
		}
	}
	setup := got[2]
	if strings.Join(setup[:7], " ") != "vm exec "+vm+" -state-dir /state -w /" {
		t.Errorf("setup = %q", setup)
	}
	for _, a := range setup {
		if strings.Contains(a, "secret") {
			t.Errorf("the environment's value is in granule's argv: %q", setup)
		}
	}
	stdin, _ := os.ReadFile(glog + ".stdin")
	if want := "export A='1'\nexport TOKEN='it'\\''s secret'\n"; string(stdin) != want {
		t.Errorf("the env file written = %q, want %q", stdin, want)
	}
	if d := calls(t, dlog); len(d) != 1 || d[0][0] != "image" {
		t.Errorf("docker calls = %q, want one image inspect", d)
	}
	if img := sb.(*VM).Image; img.Digest != "example.com/guest@sha256:def" {
		t.Errorf("image = %+v", img)
	}
}

func TestCreateOwnNetworkAndCleanup(t *testing.T) {
	// The guest never answers: Create removes what it made.
	b, glog, dlog := fakeBackend(t, `[ "$2" = wait ] && { echo "guest did not answer" >&2; exit 1; }; exit 0`,
		`case "$1" in image) echo sha256:abc;; esac`)
	spec := sandbox.SandboxSpec{Session: "s-abcdefghijkl", Name: "dev", Kind: "granule", Image: "g", Network: "none"}
	_, err := b.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "guest did not answer") {
		t.Fatalf("Create = %v, want the wait's failure", err)
	}
	g := calls(t, glog)
	if n := len(g); n != 3 || g[2][1] != "delete" {
		t.Errorf("granule calls = %q, want create, wait, delete", g)
	}
	if !strings.Contains(strings.Join(g[0], " "), "-docker-run-opt=--label=silo.network=silo-s-abcdefghijkl-dev -docker-run-opt=--network=silo-s-abcdefghijkl-dev") {
		t.Errorf("create = %q, want the sandbox's own network", g[0])
	}
	var nets []string
	for _, c := range calls(t, dlog) {
		if c[0] == "network" {
			nets = append(nets, c[1])
		}
	}
	if strings.Join(nets, ",") != "create,rm" {
		t.Errorf("docker network calls = %q, want create then rm", nets)
	}
}

func TestCreateOpenUsesDefaultNetwork(t *testing.T) {
	b, glog, _ := fakeBackend(t, "", `case "$1" in image) echo sha256:abc;; esac`)
	spec := sandbox.SandboxSpec{Session: "s-abcdefghijkl", Name: "dev", Kind: "granule", Image: "g", Network: "open", PrivateNetwork: "silo-x"}
	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if c := strings.Join(calls(t, glog)[0], " "); strings.Contains(c, "--network") {
		t.Errorf("create = %q, want Docker's default network", c)
	}
}

func TestCreateRefusals(t *testing.T) {
	b, _, _ := fakeBackend(t, "", "")
	for _, tc := range []struct {
		spec sandbox.SandboxSpec
		want string
	}{
		{sandbox.SandboxSpec{Kind: "granule", Image: "g"}, "needs a session and a name"},
		{sandbox.SandboxSpec{Session: "s", Name: "n", Kind: "container", Image: "g"}, "is not granule"},
		{sandbox.SandboxSpec{Session: "s", Name: "n", Kind: "granule", Build: "/Dockerfile"}, "not build"},
		{sandbox.SandboxSpec{Session: "s", Name: "n", Kind: "granule", Image: "g", Mounts: []sandbox.Mount{{From: "/a", To: "/b"}}}, "no mounts"},
		{sandbox.SandboxSpec{Session: "s", Name: "n", Kind: "granule"}, "needs image"},
		{sandbox.SandboxSpec{Session: "s", Name: "n", Kind: "granule", Image: "g", Env: map[string]string{"A-B": "x"}}, "not a valid environment variable"},
	} {
		if _, err := b.Create(context.Background(), tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Create(%+v) = %v, want %q", tc.spec, err, tc.want)
		}
	}
}

func TestExecArgs(t *testing.T) {
	v := &VM{backend: &Backend{}, VM: "silo1234567", Workdir: "/work"}
	args, err := v.ExecArgs(sandbox.ExecRequest{Argv: []string{"ls", "-l"}, Env: map[string]string{"B": "2", "A": "1"}}, "tag")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vm", "exec", "silo1234567", "-w", "/work", "--", "/bin/sh", "-c", execScript, "tag", "env", "A=1", "B=2", "ls", "-l"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args =\n  %q\nwant\n  %q", args, want)
	}
	args, _ = v.ExecArgs(sandbox.ExecRequest{Argv: []string{"pwd"}, Cwd: "sub"}, "tag")
	if args[3] != "-w" || args[4] != "/work/sub" {
		t.Errorf("a relative cwd: %q", args)
	}
	v.Workdir = ""
	args, _ = v.ExecArgs(sandbox.ExecRequest{Argv: []string{"pwd"}}, "tag")
	if args[3] != "--" {
		t.Errorf("no workdir passes no -w: %q", args)
	}
}

func TestExecPassesStatusAndStdin(t *testing.T) {
	// The fake granule runs the guest command itself, after "--".
	dir := t.TempDir()
	b := &Backend{Granule: fakeCLIStdin(t, dir, "granule", filepath.Join(dir, "log"), `while [ "$1" != -- ]; do shift; done; shift; exec "$@"`, false)}
	v := &VM{backend: b, VM: "silo1234567"}
	res, err := v.Exec(context.Background(), sandbox.ExecRequest{
		Argv:  []string{"/bin/sh", "-c", `cat; echo " $X" >&2; exit 3`},
		Stdin: []byte("in"), Env: map[string]string{"X": "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "in" || string(res.Stderr) != " y\n" {
		t.Errorf("result = %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestFindAndSessions(t *testing.T) {
	b, _, dlog := fakeBackend(t, "", `printf 'silo1111111\tdev\t/work\t\ts-aaaaaaaaaaaa\nsilo2222222\tb\t\tsilo-s-bbbbbbbbbbbb-b\ts-bbbbbbbbbbbb\n'`)
	found, err := b.Find(context.Background(), "s-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d", len(found))
	}
	if v := found[1].(*VM); v.VM != "silo2222222" || v.Name != "b" || v.Network != "silo-s-bbbbbbbbbbbb-b" {
		t.Errorf("VM = %+v", v)
	}
	c := calls(t, dlog)[0]
	if strings.Join(c[:6], " ") != "ps -a --filter label=granule.dev/vm --filter label=silo.session=s-aaaaaaaaaaaa" {
		t.Errorf("docker ps = %q", c)
	}
	sessions, err := b.Sessions(context.Background())
	if err != nil || strings.Join(sessions, ",") != "s-aaaaaaaaaaaa,s-bbbbbbbbbbbb" {
		t.Errorf("Sessions = %v, %v", sessions, err)
	}
}

func TestDestroyWithoutGranule(t *testing.T) {
	b, _, dlog := fakeBackend(t, "", "")
	b.Granule = filepath.Join(t.TempDir(), "no-granule")
	v := &VM{backend: b, VM: "silo1234567", Network: "silo-s-x-dev"}
	if err := v.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := calls(t, dlog)
	if len(got) != 2 || strings.Join(got[0], " ") != "rm -f granule-vm-silo1234567" || strings.Join(got[1], " ") != "network rm silo-s-x-dev" {
		t.Errorf("docker calls = %q", got)
	}
}

func TestDoctor(t *testing.T) {
	b, glog, _ := fakeBackend(t, `echo "FAIL  kvm  /dev/kvm: permission denied"; exit 1`, "")
	out, err := b.Doctor(context.Background())
	if err == nil || !strings.Contains(out, "FAIL  kvm") {
		t.Errorf("Doctor = %q, %v; want granule's failure", out, err)
	}
	if got := calls(t, glog); len(got) != 1 || strings.Join(got[0], " ") != "doctor" {
		t.Errorf("granule calls: %q", got)
	}
}

func TestCheckImage(t *testing.T) {
	// Local: one inspect, nothing asked of the registry.
	b, _, dlog := fakeBackend(t, "", `[ "$1" = image ] && echo sha256:abc`)
	if msg, err := b.CheckImage(context.Background(), "ghcr.io/x/guest:1"); err != nil || !strings.Contains(msg, "present locally") {
		t.Errorf("local image: %q, %v", msg, err)
	}
	if got := calls(t, dlog); len(got) != 1 {
		t.Errorf("docker calls for a local image: %q", got)
	}
	// Not local, no buildx: docker manifest inspect decides.
	b, _, dlog = fakeBackend(t, "", `case "$1" in
image) echo "No such image" >&2; exit 1;;
buildx) echo "docker: unknown command: docker buildx" >&2; exit 1;;
manifest) exit 0;;
esac`)
	if msg, err := b.CheckImage(context.Background(), "ghcr.io/x/guest:1"); err != nil || !strings.Contains(msg, "can be pulled") {
		t.Errorf("remote image: %q, %v", msg, err)
	}
	got := calls(t, dlog)
	if len(got) != 3 || got[1][0] != "buildx" || strings.Join(got[2], " ") != "manifest inspect ghcr.io/x/guest:1" {
		t.Errorf("docker calls: %q", got)
	}
	// Nowhere.
	b, _, _ = fakeBackend(t, "", `echo "not found" >&2; exit 1`)
	if _, err := b.CheckImage(context.Background(), "ghcr.io/x/missing:1"); err == nil {
		t.Error("a missing image passed")
	}
}
