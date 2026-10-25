package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StepSpec is one step as the executor runs it: every path resolved, every
// attachment reference rewritten to its staged copy.
type StepSpec struct {
	// Unit names the transient unit the step runs in.
	Unit string
	// Path is the program executed; Argv[0] is what the agent wrote.
	Path    string
	Argv    []string
	Dir     string
	Env     []string
	Stdin   string // a staged attachment, or "" for /dev/null
	Timeout time.Duration
}

// StepOutput is what a step did.
type StepOutput struct {
	Exit   int
	Stdout []byte
	Stderr []byte
	// Err is set when the step could not be run or was killed.
	Err error
}

// StepRunner runs one step as root.
type StepRunner interface {
	RunStep(ctx context.Context, s StepSpec) StepOutput
}

// maxOutput caps each of a step's stdout and stderr.
const maxOutput = 64 << 10

// truncMarker ends output that hit maxOutput.
const truncMarker = "\n[… output truncated by silo …]\n"

type capped struct {
	buf   bytes.Buffer
	trunc bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.buf.Len(); room < len(p) {
		c.buf.Write(p[:max(room, 0)])
		c.trunc = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) bytes() []byte {
	if c.trunc {
		return append(c.buf.Bytes(), truncMarker...)
	}
	return c.buf.Bytes()
}

// DirectRunner runs each step as a child of the gateway, in its own process
// group, killed when its timeout passes.
type DirectRunner struct{}

func (DirectRunner) RunStep(ctx context.Context, s StepSpec) StepOutput {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := &exec.Cmd{Path: s.Path, Args: s.Argv, Dir: s.Dir, Env: s.Env}
	return runCmd(ctx, cmd, s.Stdin)
}

// SystemdRunner runs each step as a transient service with systemd-run,
// outside the gateway's own cgroup, as the design asks (§4.3).
type SystemdRunner struct{}

func (SystemdRunner) RunStep(ctx context.Context, s StepSpec) StepOutput {
	args := []string{"systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--service-type=exec",
		"--unit=" + s.Unit, "--working-directory=" + s.Dir,
		"--property=RuntimeMaxSec=" + strconv.Itoa(int(s.Timeout/time.Second))}
	for _, e := range s.Env {
		args = append(args, "--setenv="+e)
	}
	// systemd-run cannot set argv[0] apart from the program, so the
	// resolved path is the program and argv[0].
	args = append(args, "--", s.Path)
	args = append(args, s.Argv[1:]...)
	ctx, cancel := context.WithTimeout(ctx, s.Timeout+30*time.Second)
	defer cancel()
	p, err := exec.LookPath("systemd-run")
	if err != nil {
		return StepOutput{Exit: -1, Err: err}
	}
	cmd := &exec.Cmd{Path: p, Args: args, Env: []string{"PATH=" + DefaultPath, "LANG=C.UTF-8"}}
	return runCmd(ctx, cmd, s.Stdin)
}

// DefaultStepRunner uses systemd-run when the gateway runs as root under
// systemd, and runs steps directly otherwise.
func DefaultStepRunner() StepRunner {
	if os.Geteuid() == 0 {
		if _, err := os.Stat("/run/systemd/system"); err == nil {
			if _, err := exec.LookPath("systemd-run"); err == nil {
				return SystemdRunner{}
			}
		}
	}
	return DirectRunner{}
}

func runCmd(ctx context.Context, cmd *exec.Cmd, stdin string) StepOutput {
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if stdin != "" {
		f, err := os.Open(stdin)
		if err != nil {
			return StepOutput{Exit: -1, Err: err}
		}
		defer f.Close()
		cmd.Stdin = f
	}
	if err := cmd.Start(); err != nil {
		return StepOutput{Exit: -1, Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		err = fmt.Errorf("killed: %w", ctx.Err())
	}
	out := StepOutput{Stdout: stdout.bytes(), Stderr: stderr.bytes(), Exit: cmd.ProcessState.ExitCode()}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		out.Err = err
	} else if err != nil && ctx.Err() != nil {
		out.Err = fmt.Errorf("timed out: %w", ctx.Err())
	}
	return out
}

// ChownFunc sets a written file's owner and group by name.
type ChownFunc func(path, owner, group string) error

// RealChown looks the names up and changes the file's ownership.
func RealChown(p, owner, group string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(g.Gid)
	return os.Lchown(p, uid, gid)
}

// FileResult records one applied file write and what it replaced, so it can
// be undone: Backup holds the previous content in the staging directory.
type FileResult struct {
	Path    string `json:"path"`
	Delete  bool   `json:"delete,omitempty"`
	Existed bool   `json:"existed"`
	// Previous state, when it existed.
	PrevMode    string `json:"prev_mode,omitempty"`
	PrevUID     int    `json:"prev_uid,omitempty"`
	PrevGID     int    `json:"prev_gid,omitempty"`
	PrevSymlink string `json:"prev_symlink,omitempty"`
	PrevSHA256  string `json:"prev_sha256,omitempty"`
	Backup      string `json:"backup,omitempty"`
	Error       string `json:"error,omitempty"`
}

// applyFile writes (or deletes) one file atomically: a temp file in the
// same directory, fsynced, chmodded and chowned, renamed over the target,
// and the directory fsynced. The previous content is copied to backupPath
// first.
func applyFile(f BoundFile, staged, backupPath string, chown ChownFunc) (FileResult, error) {
	res := FileResult{Path: f.Path, Delete: f.Delete}
	fi, err := os.Lstat(f.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, err
	case fi.IsDir():
		return res, fmt.Errorf("%s is a directory", f.Path)
	default:
		res.Existed = true
		res.PrevMode = fmt.Sprintf("%04o", fi.Mode().Perm())
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			res.PrevUID, res.PrevGID = int(st.Uid), int(st.Gid)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if res.PrevSymlink, err = os.Readlink(f.Path); err != nil {
				return res, err
			}
		} else if fi.Mode().IsRegular() {
			if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
				return res, err
			}
			if res.PrevSHA256, err = copyFile(f.Path, backupPath); err != nil {
				return res, fmt.Errorf("keeping the previous content: %w", err)
			}
			res.Backup = backupPath
		} else {
			return res, fmt.Errorf("%s is not a regular file or symlink", f.Path)
		}
	}
	dir := filepath.Dir(f.Path)
	if f.Delete {
		if res.Existed {
			if err := os.Remove(f.Path); err != nil {
				return res, err
			}
			return res, syncDir(dir)
		}
		return res, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	src, err := os.Open(staged)
	if err != nil {
		return res, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".silo-"+filepath.Base(f.Path)+"-")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	mode, _ := parseMode(f.Mode, 0o644)
	h := newHasher()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		return res, err
	}
	if got := h.hex(); got != f.SHA256 {
		tmp.Close()
		return res, fmt.Errorf("%w: staged content for %s changed while it was written", ErrMismatch, f.Path)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if err := chown(tmp.Name(), f.Owner, f.Group); err != nil {
		return res, fmt.Errorf("chown %s:%s: %w", f.Owner, f.Group, err)
	}
	// After chown, which clears setuid bits.
	if err := os.Chmod(tmp.Name(), fs.FileMode(mode&0o777)|modeBits(mode)); err != nil {
		return res, err
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return res, err
	}
	return res, syncDir(dir)
}

// modeBits turns the setuid, setgid and sticky bits of an octal mode into
// fs.FileMode bits.
func modeBits(m uint32) fs.FileMode {
	var out fs.FileMode
	if m&0o4000 != 0 {
		out |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		out |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		out |= fs.ModeSticky
	}
	return out
}

func copyFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := newHasher()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", err
	}
	return h.hex(), out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// stepEnv is the minimal environment a step runs with (design §4.3), plus
// the step's own env, which the hash covers.
func stepEnv(s *Step) []string {
	env := map[string]string{"PATH": DefaultPath, "LANG": "C.UTF-8", "DEBIAN_FRONTEND": "noninteractive"}
	for k, v := range s.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// stepSpec turns a bound step into what the runner runs, with attachment
// references rewritten to the staging directory.
func stepSpec(dir, id string, i int, s BoundStep) StepSpec {
	staged := func(a string) string {
		if name, ok := strings.CutPrefix(a, AttachmentPrefix); ok {
			return filepath.Join(dir, attachmentsDir, name)
		}
		return a
	}
	argv := make([]string, len(s.Command))
	for j, a := range s.Command {
		argv[j] = staged(a)
	}
	spec := StepSpec{
		Unit: fmt.Sprintf("silo-batch-%s-%d", id, i+1),
		Path: staged(s.Exe.Path), Argv: argv, Dir: s.Cwd, Env: stepEnv(&s.Step),
		Timeout: time.Duration(s.TimeoutS) * time.Second,
	}
	if s.StdinAttachment != "" {
		spec.Stdin = filepath.Join(dir, attachmentsDir, s.StdinAttachment)
	}
	return spec
}

type hasher struct{ hash.Hash }

func newHasher() *hasher      { return &hasher{sha256.New()} }
func (h *hasher) hex() string { return hex.EncodeToString(h.Sum(nil)) }
