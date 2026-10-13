package rootless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
)

// StartContainerd returns silo-containerd, not yet started: containerd
// with ContainerdConfigTOML, written here, run inside the holder whose
// pid is holderPid, through exe's nsexec constructor (exe is silod).
func StartContainerd(p Paths, holderPid int, exe, containerd string) (*exec.Cmd, error) {
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.ContainerdConfig(), []byte(ContainerdConfigTOML(p)), 0o600); err != nil {
		return nil, err
	}
	return holderExec(holderPid, exe, ContainerdArgs(p, containerd))
}

// StartBuildkitd returns silo-buildkitd, not yet started, run inside
// the holder like StartContainerd.
func StartBuildkitd(p Paths, holderPid int, exe, buildkitd string) (*exec.Cmd, error) {
	return holderExec(holderPid, exe, BuildkitdArgs(p, buildkitd))
}

// WriteRunsc writes the runsc shim's options file and the wrapper
// script it names, which runs `<silod> runsc`.
func WriteRunsc(p Paths, silod, runsc string) error {
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return err
	}
	wrapper := p.RunscOptions() + ".sh"
	if err := os.WriteFile(wrapper, []byte(RunscWrapperScript(silod, runsc)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.RunscOptions(), []byte(RunscOptionsTOML(p, wrapper)), 0o600)
}

func holderExec(holderPid int, exe string, argv []string) (*exec.Cmd, error) {
	if !nsexec.Available {
		return nil, errors.New("rootless: this binary was built without cgo, which joining the holder needs")
	}
	return nsd.JoinExec(holderPid, exe, argv...), nil
}

// RunscMain is `silod runsc --runsc <path> -- <runsc args>`, the runsc
// the sandbox class's shim calls (call L30). It runs inside the holder,
// as the shim does. It finds the container's bundle, reads the user
// namespace the bundle names, and replaces itself with runsc started
// inside that namespace as its root, through exe's nsexec constructor.
// A bundle that names none is refused (ErrNoUserNS).
func RunscMain(exe string, args []string) error {
	fs := flag.NewFlagSet("runsc", flag.ContinueOnError)
	runsc := fs.String("runsc", "runsc", "the runsc binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rargs := fs.Args()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	bundle := RunscBundle(rargs, cwd)
	userns, root, err := RunscUserNS(bundle)
	if err != nil {
		return err
	}
	if err := prepareBundle(bundle, root); err != nil {
		return fmt.Errorf("rootless: bundle: %w", err)
	}
	if err := prepareRunscRoot(runscRoot(rargs)); err != nil {
		return fmt.Errorf("rootless: runsc root: %w", err)
	}
	if err := prepareProcessFile(rargs, root); err != nil {
		return fmt.Errorf("rootless: exec's process file: %w", err)
	}
	path, err := exec.LookPath(*runsc)
	if err != nil {
		return err
	}
	cmd := nsd.UserNSExec(userns, exe, append([]string{path}, rargs...)...)
	switch runscCommand(rargs) {
	case "create":
		if os.Getenv(ReaperEnv) != "" {
			return reap(cmd, bundle, rargs)
		}
		return startReaper()
	case "wait":
		return waitOrRecorded(cmd, bundle, rargs)
	}
	return syscall.Exec(exe, append([]string{exe}, cmd.Args[1:]...), cmd.Env)
}

// ReaperEnv marks the process `silod runsc create` starts to be its
// sandbox's parent: the same command line, run again with it set.
const ReaperEnv = "_SILO_RUNSC_REAPER"

// ExitFile is the file in the bundle where the reaper records how the
// sandbox ended.
const ExitFile = "silo-exit"

// startReaper runs `runsc create` under a reaper (reap) and exits with
// create's status, leaving the reaper behind as the sandbox's parent.
//
// A sandbox exits with its root container's status, and only its
// parent can collect that. runsc create forks the sandbox and exits, so
// the shim, a subreaper, inherits it and drops its status: `runsc wait`
// asks the sandbox instead, and a workload that exits before the wait
// connects comes back as "exit status is unavailable", which the shim
// reports as 128. The reaper is a subreaper between the two, so the
// sandbox is reparented to it, and it writes the status to ExitFile
// for waitOrRecorded.
func startReaper() error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Args[0] = os.Args[0]
	cmd.Env = append(os.Environ(), ReaperEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.Close()
	b, _ := io.ReadAll(r)
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		cmd.Wait()
		return fmt.Errorf("rootless: the sandbox's reaper said %q", b)
	}
	os.Exit(code)
	return nil
}

// reap is the reaper: it runs runsc create as its child, reports its
// exit code on fd 3, lets go of the workload's stdio, and waits for the
// sandbox, whose pid create wrote to its --pid-file, recording its
// status in the bundle's ExitFile. It exits when it has no children
// left: the sandbox, and the gofer and sidecars it inherits with it.
func reap(cmd *exec.Cmd, bundle string, args []string) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	status := os.NewFile(3, "status")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "silod runsc:", err)
			code = 125
		} else {
			code = ee.ExitCode()
		}
	}
	fmt.Fprint(status, code)
	status.Close()
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		for fd := 0; fd < 3; fd++ {
			unix.Dup2(int(null.Fd()), fd)
		}
		null.Close()
	}
	if code != 0 {
		return nil
	}
	pidFile := flagValue(args, "pid-file")
	if pidFile != "" && !filepath.IsAbs(pidFile) {
		pidFile = filepath.Join(bundle, pidFile)
	}
	b, _ := os.ReadFile(pidFile)
	sandbox, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil
		}
		if pid == sandbox && sandbox > 0 {
			n := ws.ExitStatus()
			if ws.Signaled() {
				n = 128 + int(ws.Signal())
			}
			tmp := filepath.Join(bundle, ExitFile+".tmp")
			if os.WriteFile(tmp, []byte(strconv.Itoa(n)), 0o644) == nil {
				os.Rename(tmp, filepath.Join(bundle, ExitFile))
			}
		}
	}
}

// waitOrRecorded runs `runsc wait`, and when runsc has lost the
// sandbox's status, answers with the one the reaper recorded, in runsc
// wait's own format.
func waitOrRecorded(cmd *exec.Cmd, bundle string, args []string) error {
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		os.Stdout.Write(out.Bytes())
		os.Exit(0)
	}
	if strings.Contains(errb.String(), "exit status is unavailable") && len(args) > 0 {
		for i := 0; i < 200; i++ {
			if b, err := os.ReadFile(filepath.Join(bundle, ExitFile)); err == nil {
				n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				json.NewEncoder(os.Stdout).Encode(struct {
					ID         string `json:"id"`
					ExitStatus int    `json:"exitStatus"`
				}{args[len(args)-1], n})
				os.Exit(0)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	os.Stderr.Write(errb.Bytes())
	code := 1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	os.Exit(code)
	return nil
}

// ContainerdMain is `silod containerd`, run by silo-containerd.service:
// it waits for the holder, writes containerd's configuration and the
// runsc shim's options, and replaces itself with containerd inside the
// holder's namespaces, so that containerd is the unit's main process.
// containerd and its shims outlive a restart of silo-nsd, whose next
// holder adopts the namespaces they keep (call L33).
func ContainerdMain(ctx context.Context, exe string, args []string) error {
	p, bin, err := unitFlags("containerd", "containerd", args)
	if err != nil {
		return err
	}
	st, err := nsd.WaitState(ctx, filepath.Join(p.RuntimeDir, "nsd.state"), 60*time.Second)
	if err != nil {
		return err
	}
	if runsc, err := exec.LookPath("runsc"); err == nil {
		if err := WriteRunsc(p, exe, runsc); err != nil {
			return err
		}
	}
	cmd, err := StartContainerd(p, st.Pid, exe, bin)
	if err != nil {
		return err
	}
	return syscall.Exec(cmd.Path, cmd.Args, cmd.Env)
}

// BuildkitdMain is `silod buildkitd`, run by silo-buildkitd.service,
// like ContainerdMain.
func BuildkitdMain(ctx context.Context, exe string, args []string) error {
	p, bin, err := unitFlags("buildkitd", "buildkitd", args)
	if err != nil {
		return err
	}
	st, err := nsd.WaitState(ctx, filepath.Join(p.RuntimeDir, "nsd.state"), 60*time.Second)
	if err != nil {
		return err
	}
	cmd, err := StartBuildkitd(p, st.Pid, exe, bin)
	if err != nil {
		return err
	}
	return syscall.Exec(cmd.Path, cmd.Args, cmd.Env)
}

// unitFlags parses --data-dir, --runtime-dir and the binary's flag.
func unitFlags(name, bin string, args []string) (Paths, string, error) {
	rt, _ := nsd.RuntimeDir()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "the data directory (required)")
	runtimeDir := fs.String("runtime-dir", rt, "the holder's runtime directory, where nsd.state and the sockets are")
	path := fs.String(name, bin, "the "+name+" binary")
	if err := fs.Parse(args); err != nil {
		return Paths{}, "", err
	}
	if *dataDir == "" || *runtimeDir == "" {
		return Paths{}, "", fmt.Errorf("%s: give --data-dir, and --runtime-dir unless XDG_RUNTIME_DIR is set", name)
	}
	p, err := exec.LookPath(*path)
	if err != nil {
		return Paths{}, "", err
	}
	return Paths{DataDir: *dataDir, RuntimeDir: *runtimeDir}, p, nil
}
