package rootless

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCgroup makes dir a cgroup with controllers, as cgroupfs would.
func fakeCgroup(t *testing.T, dir, controllers, procs string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"cgroup.controllers": controllers, "cgroup.procs": procs, "cgroup.subtree_control": ""} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The tree starts the scope when the manager has none, moves the
// scope's process to its keep leaf, and makes each silo's cgroup with
// the controllers it has enabled for its children; the container's own
// is left to `silod runsc create`.
func TestSandboxTree(t *testing.T) {
	fs := t.TempDir()
	const scope = "/user.slice/user@1000.service/silo.slice/" + SandboxScope
	var started []string
	tree := &SandboxTree{FS: fs,
		Systemctl: func(_ context.Context, args ...string) ([]byte, error) {
			if strings.Join(args, " ") != "show --property=ActiveState,ControlGroup "+SandboxScope {
				t.Errorf("systemctl %v", args)
			}
			if started == nil {
				return []byte("ActiveState=inactive\nControlGroup=\n"), nil
			}
			return []byte("ActiveState=active\nControlGroup=" + scope + "\n"), nil
		},
		SystemdRun: func(args ...string) (*exec.Cmd, error) {
			started = args
			fakeCgroup(t, filepath.Join(fs, scope), "cpu memory pids", "42\n")
			return &exec.Cmd{}, nil
		}}
	fakeCgroup(t, filepath.Join(fs, scope, "root"), "cpu memory pids", "")
	fakeCgroup(t, filepath.Join(fs, scope, "root", "child"), "cpu memory pids", "")

	got, err := tree.Path(context.Background(), "c1", "root", "child")
	if err != nil {
		t.Fatal(err)
	}
	if got != scope+"/root/child/c1" {
		t.Errorf("path %s", got)
	}
	if !strings.Contains(strings.Join(started, " "), "--scope --unit="+SandboxScope+" --slice="+RootSlice+" --property=Delegate=yes") {
		t.Errorf("systemd-run %v", started)
	}
	if p := readFile(t, filepath.Join(fs, scope, SandboxKeep, "cgroup.procs")); p != "42" {
		t.Errorf("keep's process %q, want the scope's 42", p)
	}
	for _, d := range []string{"", "root", "root/child"} {
		if s := readFile(t, filepath.Join(fs, scope, d, "cgroup.subtree_control")); s != "+cpu +memory +pids" {
			t.Errorf("%s/cgroup.subtree_control %q", d, s)
		}
	}
	if _, err := os.Stat(filepath.Join(fs, scope, "root", "child", "c1")); err == nil {
		t.Errorf("the container's cgroup was made by the tree")
	}

	// A second container finds the scope made.
	started = []string{"done"}
	if got, err := tree.Path(context.Background(), "c2", "root"); err != nil || got != scope+"/root/c2" {
		t.Errorf("second path %s, %v", got, err)
	}
	for _, bad := range [][]string{{"../x"}, {"c", ".."}, {SandboxKeep}, {"c", "a/b"}} {
		if _, err := tree.Path(context.Background(), bad[0], bad[1:]...); err == nil {
			t.Errorf("%q named a cgroup", bad)
		}
	}
}

// A container's cgroup gets its spec's memory, with no swap, CPU and
// pids limits; what the spec leaves unset stays unlimited.
func TestWriteLimits(t *testing.T) {
	bundle := t.TempDir()
	const spec = `{"annotations": {"` + AnnotationCgroup + `": "/scope/root/c1"},
		"linux": {"resources": {"memory": {"limit": 134217728}, "cpu": {"quota": 50000, "period": 100000}, "pids": {"limit": 256}}}}`
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	cg, l, err := BundleCgroup(bundle)
	if err != nil || cg != "/scope/root/c1" {
		t.Fatalf("cgroup %q, %v", cg, err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("max"), 0o644)
	if err := WriteLimits(dir, l); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"memory.max": "134217728", "memory.swap.max": "0", "cpu.max": "50000 100000", "pids.max": "256"} {
		if got := readFile(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}

	dir = t.TempDir()
	if err := WriteLimits(dir, Limits{}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("limits written for a spec with none: %v", ents)
	}
}
