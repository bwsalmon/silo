package rootless

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

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
	path, err := exec.LookPath(*runsc)
	if err != nil {
		return err
	}
	cmd := nsd.UserNSExec(userns, exe, append([]string{path}, rargs...)...)
	return syscall.Exec(exe, append([]string{exe}, cmd.Args[1:]...), cmd.Env)
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
