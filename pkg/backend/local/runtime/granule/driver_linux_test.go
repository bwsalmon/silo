package granule

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

func TestExecArgs(t *testing.T) {
	got := ExecArgs(runtime.Process{Args: []string{"sh", "-c", "echo hi"}, User: "1000:1000", Workdir: "/srv", Env: []string{"A=1", "B=2"}})
	want := []string{GranuleBin, "exec", "--container", "-u", "1000:1000", "-w", "/srv", "-e", "A=1", "-e", "B=2", "--", "sh", "-c", "echo hi"}
	if !slices.Equal(got, want) {
		t.Errorf("ExecArgs\n got %q\nwant %q", got, want)
	}
	// The container's own user and directory when the process names none.
	got = ExecArgs(runtime.Process{Args: []string{"-v"}})
	if want := []string{GranuleBin, "exec", "--container", "--", "-v"}; !slices.Equal(got, want) {
		t.Errorf("ExecArgs\n got %q\nwant %q", got, want)
	}
}

func TestExitStatus(t *testing.T) {
	d := &Driver{cfg: Config{Dir: t.TempDir()}}
	at := time.Unix(1700000000, 0)
	for _, c := range []struct {
		name   string
		record string // "" for none
		shim   int
		code   int
		sig    syscall.Signal
	}{
		{"reported", `{"exitCode":3,"reported":true}`, 3, 3, 0},
		{"reported signal", `{"exitCode":143,"reported":true}`, 143, 143, syscall.SIGTERM},
		// The VMM's status is not the workload's.
		{"reported over the shim's", `{"exitCode":0,"reported":true}`, 1, 0, 0},
		{"unreported", `{"exitCode":125,"reported":false,"reason":"the guest panicked"}`, 125, 125, 0},
		{"no record, shim killed", "", 137, 137, syscall.SIGKILL},
		{"no record", "", 0, ExitUnreported, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := filepath.Base(t.Name())
			os.MkdirAll(d.dir(id), 0o700)
			if c.record != "" {
				os.WriteFile(filepath.Join(d.dir(id), "exit.json"), []byte(c.record+"\n"), 0o644)
			}
			e := d.exitStatus(id, c.shim, at)
			if e.Code != c.code || e.Signal != c.sig || !e.Finished.Equal(at) {
				t.Errorf("exitStatus = %+v, want code %d signal %d", e, c.code, c.sig)
			}
		})
	}
}

func TestMakeDir(t *testing.T) {
	d := &Driver{cfg: Config{Dir: t.TempDir()}}
	for _, c := range []struct {
		name       string
		tty, stdin bool
		want       []string
	}{
		{"plain", false, false, []string{"stdout", "stderr"}},
		{"stdin", false, true, []string{"stdout", "stderr", "stdin"}},
		{"tty", true, true, []string{"stdout", "resize", "stdin"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := VM{Dir: d.dir(c.name), TTY: c.tty, Stdin: c.stdin}
			if err := d.makeDir(v); err != nil {
				t.Fatal(err)
			}
			es, _ := os.ReadDir(v.Dir)
			var got []string
			for _, e := range es {
				fi, _ := e.Info()
				switch {
				case e.Name() == "fs":
					if !e.IsDir() || fi.Mode().Perm() != 0o777 {
						t.Errorf("fs: %v", fi.Mode())
					}
					continue
				case fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o666:
					t.Errorf("%s: %v, not a FIFO open to the VM container's root", e.Name(), fi.Mode())
				}
				got = append(got, e.Name())
			}
			if fi, _ := os.Stat(v.Dir); fi.Mode().Perm() != 0o777 {
				t.Errorf("directory %v", fi.Mode())
			}
			slices.Sort(got)
			want := slices.Clone(c.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("FIFOs %q, want %q", got, want)
			}
			// Again, as an adoption's retry would.
			if err := d.makeDir(v); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestStopVirtiofsds(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "v0.sock")
	// A process whose command line names the socket, as virtiofsd's
	// does, and one that doesn't, at a pid file of its own.
	served := exec.Command("sh", "-c", "sleep 60; :", "--socket-path="+sock)
	other := exec.Command("sh", "-c", "sleep 60; :")
	for _, c := range []*exec.Cmd{served, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	}
	os.WriteFile(sock+".pid", []byte(strconv.Itoa(served.Process.Pid)), 0o644)
	os.WriteFile(filepath.Join(dir, "v1.sock.pid"), []byte(strconv.Itoa(other.Process.Pid)), 0o644)
	if err := stopVirtiofsds(dir); err != nil {
		t.Fatal(err)
	}
	served.Wait()
	if st := served.ProcessState; st == nil || st.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Errorf("the virtiofsd: %v", st)
	}
	if syscall.Kill(other.Process.Pid, 0) != nil {
		t.Error("a process the pid file no longer names was killed")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.pid")); len(m) != 0 {
		t.Errorf("pid files left: %q", m)
	}
}

func TestSocketArg(t *testing.T) {
	v := Virtiofsd{Binary: "/usr/libexec/virtiofsd"}
	if s := socketArg(v.Args(Share{Socket: "/d/fs/v0.sock", Source: "/v"})); s != "/d/fs/v0.sock" {
		t.Errorf("socketArg %q", s)
	}
	if s := socketArg([]string{"virtiofsd"}); s != "" {
		t.Errorf("socketArg %q", s)
	}
}

func TestNewNeedsItsParts(t *testing.T) {
	if _, err := New(Config{Address: "/nonexistent.sock"}); err == nil {
		t.Error("New with no directories, holder or images succeeded")
	}
}
