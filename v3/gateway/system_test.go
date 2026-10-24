package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordRunner struct {
	calls [][]string
	out   map[string]string // first arg after the command -> output
	err   error
}

func (r *recordRunner) run(_ context.Context, argv ...string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	return []byte(r.out[argv[1]]), r.err
}

func TestSystemdRunCommands(t *testing.T) {
	r := &recordRunner{out: map[string]string{"show": "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nMainPID=0\nExecMainStatus=2\n"}}
	s := &SystemdRun{Run: r.run}
	spec := UnitSpec{
		Name: "silo-agent-a.service", Slice: "silo-agent-a.slice", Description: "silo agent a",
		Argv: []string{"/bin/sleep", "infinity"}, Env: map[string]string{"B": "2", "A": "1"},
		Properties: []Property{{"User", "silo-a-a"}, {"PrivateTmp", "yes"}},
	}
	if err := s.StartService(ctx, spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemd-run", "--quiet", "--no-ask-password", "--service-type=exec", "--unit=silo-agent-a.service",
		"--slice=silo-agent-a.slice", "--description=silo agent a", "--property=User=silo-a-a", "--property=PrivateTmp=yes",
		"--setenv=A=1", "--setenv=B=2", "--", "/bin/sleep", "infinity"}
	if !reflect.DeepEqual(r.calls[0], want) {
		t.Fatalf("systemd-run\n got %q\nwant %q", r.calls[0], want)
	}
	s.SetProperties(ctx, "x.slice", []Property{{"MemoryMax", "1G"}})
	s.Stop(ctx, "x.slice")
	s.Kill(ctx, "x.service")
	s.ResetFailed(ctx, "x.service")
	st, err := s.Status(ctx, "x.service")
	if err != nil {
		t.Fatal(err)
	}
	if st != (UnitStatus{Exists: true, ActiveState: "failed", SubState: "failed", Result: "exit-code", ExecMainStatus: 2}) || st.Active() {
		t.Fatalf("status %+v", st)
	}
	got := []string{}
	for _, c := range r.calls[1:] {
		got = append(got, strings.Join(c, " "))
	}
	wantCalls := []string{
		"systemctl set-property --runtime -- x.slice MemoryMax=1G",
		"systemctl stop -- x.slice",
		"systemctl kill --signal=SIGKILL --kill-whom=all -- x.service",
		"systemctl reset-failed -- x.service",
		"systemctl show --property=LoadState,ActiveState,SubState,Result,MainPID,ExecMainStatus -- x.service",
	}
	if !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls\n%s", strings.Join(got, "\n"))
	}
	r.err = errors.New("exit 1")
	if err := s.Stop(ctx, "x"); err == nil {
		t.Fatal("error swallowed")
	}
}

func TestParseShow(t *testing.T) {
	if st := parseShow([]byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\n")); st.Exists || st.Active() || st.String() != "gone" {
		t.Fatalf("not-found: %+v", st)
	}
	st := parseShow([]byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\n"))
	if !st.Active() || st.MainPID != 42 || st.String() != "active/running" {
		t.Fatalf("active: %+v", st)
	}
}

func TestShadowUsers(t *testing.T) {
	r := &recordRunner{}
	u := &ShadowUsers{Run: r.run}
	if got, err := u.Lookup("root"); err != nil || got == nil || got.UID != 0 {
		t.Fatalf("root: %+v %v", got, err)
	}
	if got, err := u.Lookup("no-such-user-silo-test"); err != nil || got != nil {
		t.Fatalf("missing: %+v %v", got, err)
	}
	if taken, err := u.UIDTaken(0); err != nil || !taken {
		t.Fatalf("uid 0: %v %v", taken, err)
	}
	if taken, err := u.UIDTaken(61999); err != nil || taken {
		t.Fatalf("uid 61999: %v %v", taken, err)
	}
	// Create runs useradd; the fake runner creates nothing, so the
	// follow-up lookup finds no user.
	home := filepath.Join(t.TempDir(), "h")
	os.Mkdir(home, 0o755)
	if got, err := u.Create("silo-a-test", 61500, home); err != nil || got != nil {
		t.Fatalf("create: %+v %v", got, err)
	}
	want := "useradd --system --uid 61500 --user-group --home-dir " + home + " --create-home --shell /usr/sbin/nologin --comment silo agent -- silo-a-test"
	if strings.Join(r.calls[0], " ") != want {
		t.Fatalf("useradd: %q", r.calls[0])
	}
	if fi, _ := os.Stat(home); fi.Mode().Perm() != 0o700 {
		t.Fatalf("home mode %v", fi.Mode())
	}
}

func TestProcFSCgroup(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "7"), 0o755)
	os.WriteFile(filepath.Join(root, "7", "cgroup"), []byte("0::/silo.slice/silo-agent-root.service\n"), 0o644)
	cg, err := ProcFS{Root: root}.Cgroup(7)
	if err != nil || cg != "/silo.slice/silo-agent-root.service" {
		t.Fatalf("%q %v", cg, err)
	}
	if _, err := (ProcFS{Root: root}).Cgroup(8); err == nil {
		t.Fatal("missing pid")
	}
	if cg, err := (ProcFS{}).Cgroup(os.Getpid()); err != nil || !strings.HasPrefix(cg, "/") {
		t.Fatalf("self: %q %v", cg, err)
	}
}

func TestSliceNames(t *testing.T) {
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{"a"}, "silo-agent-a.slice"},
		{[]string{"a", "b"}, "silo-agent-a-b.slice"},
		{[]string{"web-dev", "t"}, `silo-agent-web\x2ddev-t.slice`},
	} {
		if got := sliceName(tc.path); got != tc.want {
			t.Errorf("sliceName(%v) = %s, want %s", tc.path, got, tc.want)
		}
	}
}
