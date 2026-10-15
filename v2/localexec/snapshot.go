// Package localexec is the local executor (design §12): each script the
// Silofile declares on the invoking machine becomes one gateway tool,
// local_<name>, that runs it with the agent's arguments and no shell.
//
// Snapshot records the scripts at session start and refuses any that
// another user could have written; every call re-hashes its script and
// refuses one that changed (call 22). Check is silo validate's part: the
// same refusals as errors, and best-effort warnings on scripts that turn
// an argument into code (§12.4, call 23). Executor serves the tools,
// asking an Approver first for scripts marked approve: true.
package localexec

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/silofile"
)

// Defaults of design §12.3.
const (
	DefaultTimeout   = 5 * time.Minute
	DefaultOutputCap = 1 << 20
	// KillGrace is how long a timed-out script's process group has
	// between SIGTERM and SIGKILL.
	KillGrace = 5 * time.Second
	// describeLines is how far into a script # silo: lines are read.
	describeLines = 20
)

// Script is one snapshotted script.
type Script struct {
	// Name is the file name; Tool the gateway tool, local_<name>.
	Name, Tool string
	// Path is the script's real path, symlinks resolved.
	Path string
	Size int64
	Mode fs.FileMode
	// SHA256 is the hex digest of its content at snapshot.
	SHA256 string

	// Description is the tool's description, with the # silo-args:
	// usage line when there is one.
	Description string
	Approve     bool
	// Timeout bounds one call, approval included.
	Timeout time.Duration
}

// Snapshot checks and records the scripts the Silofile serves, as
// listed by f.LocalScripts. Any refusal fails the whole snapshot: a
// script is never dropped silently (design §12.2).
func Snapshot(f *silofile.Silofile, scripts []silofile.LocalScript) ([]Script, error) {
	var errs silofile.Errors
	out := make([]Script, 0, len(scripts))
	for _, ls := range scripts {
		s, err := snapshotOne(f, ls)
		if err != nil {
			errs = append(errs, &silofile.Error{File: f.File, Path: "local", Msg: err.Error()})
			continue
		}
		out = append(out, s)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func snapshotOne(f *silofile.Silofile, ls silofile.LocalScript) (Script, error) {
	if err := checkFile(f.Folder, ls.Path); err != nil {
		return Script{}, fmt.Errorf("script %s: %w", ls.Name, err)
	}
	fi, sum, head, err := hashFile(ls.Path)
	if err != nil {
		return Script{}, fmt.Errorf("script %s: %w", ls.Name, err)
	}
	s := Script{
		Name: ls.Name, Tool: ls.Tool, Path: ls.Path,
		Size: fi.Size(), Mode: fi.Mode(), SHA256: sum,
		Timeout: DefaultTimeout,
	}
	if f.Local != nil && f.Local.Timeout != nil && *f.Local.Timeout > 0 {
		s.Timeout = time.Duration(*f.Local.Timeout)
	}
	desc := ""
	if e := ls.Entry; e != nil {
		desc = strings.TrimSpace(e.Description)
		s.Approve = e.Approve
		if e.Timeout != nil && *e.Timeout > 0 {
			s.Timeout = time.Duration(*e.Timeout)
		}
	}
	silo, usage := scriptComments(head)
	if desc == "" {
		desc = silo
	}
	if desc == "" {
		desc = fmt.Sprintf("Run %s on the user's machine.", ls.Name)
	}
	if usage != "" {
		desc += "\nUsage: " + ls.Name + " " + usage
	}
	if s.Approve {
		desc += "\nEach call waits for the user to approve it."
	}
	s.Description = desc
	return s, nil
}

// checkFile applies the refusals of design §12.2 to a script's real
// path: it must lie in the silo folder, be a regular executable file
// owned by the invoking user, and neither it nor any directory between
// it and the folder may be writable by group or world.
func checkFile(folder, path string) error {
	root, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("it resolves to %s, outside the silo folder", real)
	}
	fi, err := os.Lstat(real)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", real)
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", real)
	}
	if err := checkOwner(real, fi); err != nil {
		return err
	}
	for p := real; ; p = filepath.Dir(p) {
		pi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if pi.Mode().Perm()&0o022 != 0 {
			what := "it"
			if p != real {
				what = "its directory " + p
			}
			return fmt.Errorf("%s is writable by group or others (mode %04o): only the user may be able to change a script silo runs", what, pi.Mode().Perm())
		}
		if p == root || p == filepath.Dir(p) {
			return nil
		}
	}
}

// getuid is the invoking user's uid; tests replace it.
var getuid = os.Getuid

func checkOwner(path string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := getuid(); int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not the invoking user (uid %d)", path, st.Uid, uid)
	}
	return nil
}

// hashFile returns a file's info, the hex SHA-256 of its content, and
// its first lines (for descriptions and warnings, up to 64 KiB).
func hashFile(path string) (fs.FileInfo, string, []byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, "", nil, err
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return nil, "", nil, err
	}
	h := sha256.New()
	head := &capped{n: 64 << 10}
	if _, err := io.Copy(io.MultiWriter(h, head), fh); err != nil {
		return nil, "", nil, err
	}
	return fi, hex.EncodeToString(h.Sum(nil)), head.b, nil
}

type capped struct {
	b []byte
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Verify re-hashes a script before a call (call 22) and returns an error
// saying what changed since the snapshot.
func (s Script) Verify() error {
	fi, sum, _, err := hashFile(s.Path)
	if err != nil {
		return fmt.Errorf("script %s could not be re-read: %w", s.Name, err)
	}
	var changed []string
	if sum != s.SHA256 {
		changed = append(changed, "content")
	}
	if fi.Size() != s.Size {
		changed = append(changed, "size")
	}
	if fi.Mode() != s.Mode {
		changed = append(changed, "mode")
	}
	if len(changed) > 0 {
		return fmt.Errorf("script %s has changed since this session started (%s): silo runs only what it snapshotted; restart the session to serve the new version",
			s.Name, strings.Join(changed, ", "))
	}
	return nil
}

// scriptComments reads # silo: lines (joined into a description) and the
// first # silo-args: line from a script's first describeLines lines.
func scriptComments(head []byte) (desc, usage string) {
	sc := bufio.NewScanner(strings.NewReader(string(head)))
	var lines []string
	for i := 0; i < describeLines && sc.Scan(); i++ {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "# silo-args:"):
			if usage == "" {
				usage = strings.TrimSpace(strings.TrimPrefix(l, "# silo-args:"))
			}
		case strings.HasPrefix(l, "# silo:"):
			if t := strings.TrimSpace(strings.TrimPrefix(l, "# silo:")); t != "" {
				lines = append(lines, t)
			}
		}
	}
	return strings.Join(lines, " "), usage
}

// Check is silo validate's script check (design §2.4), the hook of
// silofile.Options.CheckScripts: the snapshot's refusals as errors, and
// the warnings of §12.4.
func Check(f *silofile.Silofile, scripts []silofile.LocalScript) (errs, warnings silofile.Errors) {
	for _, ls := range scripts {
		if _, err := snapshotOne(f, ls); err != nil {
			errs = append(errs, &silofile.Error{File: f.File, Path: "local", Msg: err.Error()})
			continue
		}
		for _, w := range Warnings(ls.Path) {
			warnings = append(warnings, &silofile.Error{File: f.File, Path: "local", Msg: fmt.Sprintf("script %s: %s", ls.Name, w)})
		}
	}
	return errs, warnings
}
