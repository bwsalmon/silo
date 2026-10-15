package rootless

import (
	"bytes"
	"context"
	"crypto/sha256"
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
// A bundle that names none is refused (ErrNoUserNS). --bundle names the
// bundle for a runsc command line that names none and isn't run from
// it, as `runsc tar rootfs-upper` from silod (Paths.TaskBundle).
func RunscMain(exe string, args []string) error {
	fs := flag.NewFlagSet("runsc", flag.ContinueOnError)
	runsc := fs.String("runsc", "runsc", "the runsc binary")
	bundleFlag := fs.String("bundle", "", "the container's bundle, when it isn't the runsc command line's or the working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rargs := fs.Args()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	bundle := *bundleFlag
	if bundle == "" {
		bundle = RunscBundle(rargs, cwd)
	}
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
	if err := prepareTarFile(rargs, root); err != nil {
		return fmt.Errorf("rootless: tar's output: %w", err)
	}
	path, err := exec.LookPath(*runsc)
	if err != nil {
		return err
	}
	if path, err = reachableRunsc(path); err != nil {
		return fmt.Errorf("rootless: runsc: %w", err)
	}
	cmd := nsd.UserNSExec(userns, exe, append([]string{path}, rargs...)...)
	switch runscCommand(rargs) {
	case "create":
		if os.Getenv(ReaperEnv) != "" {
			return reap(cmd, bundle, rargs)
		}
		return startReaper()
	case "wait":
		return waitOrRecorded(cmd, rargs)
	case "delete":
		// The container is gone; so is its record.
		if len(rargs) > 0 {
			if p := exitPath(rargs[len(rargs)-1]); p != "" {
				os.Remove(p)
			}
		}
	}
	return syscall.Exec(exe, append([]string{exe}, cmd.Args[1:]...), cmd.Env)
}

// ReaperEnv marks the process `silod runsc create` starts to be its
// sandbox's parent: the same command line, run again with it set.
const ReaperEnv = "_SILO_RUNSC_REAPER"

// ExitDir is where the reaper records how each sandbox ended, one file
// per container id, in the holder's private /run. Not in the bundle:
// silod runsc gives the bundle to the container's root, and the reaper
// runs as the holder's root, silod's uid on the host, so a file it
// wrote there could be a symlink the container's root planted. ExitDir
// is the holder's root's own, mode 0700, which no container's root can
// enter.
const ExitDir = "/run/silo/exits"

// exitPath is container id's record in ExitDir, or "" for an id that
// isn't one path element.
func exitPath(id string) string {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\x00") {
		return ""
	}
	return filepath.Join(ExitDir, id)
}

// recordExit writes n as container id's record, atomically.
func recordExit(id string, n int) error {
	p := exitPath(id)
	if p == "" {
		return fmt.Errorf("rootless: container id %q", id)
	}
	if err := os.MkdirAll(ExitDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(ExitDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(ExitDir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strconv.Itoa(n)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// startReaper runs `runsc create` under a reaper (reap) and exits with
// create's status, leaving the reaper behind as the sandbox's parent.
//
// A sandbox exits with its root container's status, and only its
// parent can collect that. runsc create forks the sandbox and exits, so
// the shim, a subreaper, inherits it and drops its status: `runsc wait`
// asks the sandbox instead, and a workload that exits before the wait
// connects comes back as "exit status is unavailable", which the shim
// reports as 128. The reaper is a subreaper between the two, so the
// sandbox is reparented to it, and it writes the status to its record
// in ExitDir for waitOrRecorded.
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
// status in ExitDir. The pid file is in the bundle, which the
// container's root can write, so it is only trusted to name one of the
// reaper's own children. It exits when it has no children
// left: the sandbox, and the gofer and sidecars it inherits with it.
func reap(cmd *exec.Cmd, bundle string, args []string) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	status := os.NewFile(3, "status")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	code := 0
	// runsc, and the sandbox and gofer it forks, start in the
	// container's cgroup; the reaper stays where the shim is.
	cg, cgf, err := containerCgroup(bundle)
	if cgf != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgf.Fd())}
		defer cgf.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "silod runsc:", err)
		code = 125
	} else if err := cmd.Run(); err != nil {
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
		if cg != "" {
			os.Remove(cg)
		}
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
			// The sandbox and everything it started are gone, and with
			// them the cgroup's processes.
			if cg != "" {
				os.Remove(cg)
			}
			return nil
		}
		if pid == sandbox && sandbox > 0 {
			n := ws.ExitStatus()
			if ws.Signaled() {
				n = 128 + int(ws.Signal())
			}
			if len(args) > 0 {
				recordExit(args[len(args)-1], n)
			}
		}
	}
}

// waitOrRecorded runs `runsc wait`, and when runsc has lost the
// sandbox's status, answers with the one the reaper recorded, in runsc
// wait's own format.
func waitOrRecorded(cmd *exec.Cmd, args []string) error {
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		os.Stdout.Write(out.Bytes())
		os.Exit(0)
	}
	if strings.Contains(errb.String(), "exit status is unavailable") && len(args) > 0 {
		for i := 0; i < 200; i++ {
			p := exitPath(args[len(args)-1])
			if p == "" {
				break
			}
			if b, err := os.ReadFile(p); err == nil {
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

// RuntimesMount is where reachableRunsc binds a runtime's directory in
// the holder's private /run, one directory per path it binds.
const RuntimesMount = "/run/silo/runtimes"

// reachableRunsc is path as the container's root can exec it. runsc
// runs as that root, a subordinate id, which can't search a directory
// only silod's user may, as an install's data directory under
// ~/.local/share is. Where some ancestor of path isn't searchable by
// others, its directory is bound under RuntimesMount in the holder's
// mount namespace, which a lookup through the bind doesn't need to
// search, and the path through the bind is returned. A bind that is
// already there is reused.
func reachableRunsc(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	searchable := true
	for d := dir; ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return "", err
		}
		if fi.Mode().Perm()&0o001 == 0 {
			searchable = false
			break
		}
		if d == "/" {
			break
		}
	}
	if searchable {
		return path, nil
	}
	target := filepath.Join(RuntimesMount, fmt.Sprintf("%x", sha256.Sum256([]byte(dir)))[:16])
	if !sameFile(dir, target) {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", err
		}
		if err := os.Chmod(RuntimesMount, 0o755); err != nil {
			return "", err
		}
		// Two runsc calls racing here each bind it; either bind serves.
		if err := unix.Mount(dir, target, "", unix.MS_BIND, ""); err != nil && !sameFile(dir, target) {
			return "", fmt.Errorf("binding %s at %s: %w", dir, target, err)
		}
	}
	return filepath.Join(target, filepath.Base(path)), nil
}

// sameFile reports whether a and b are the same file.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}
