package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// HashVersion names the canonical encoding a batch hash is computed over.
const HashVersion = "silo-batch-v1"

// DefaultPath is the PATH steps run with and argv[0] is resolved against
// unless a step's env sets its own (design §4.3).
const DefaultPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// Bound is everything a batch's hash covers: the batch as submitted (with
// defaults filled), the sha256 and size of every staged file and attachment,
// and the resolved path and sha256 of every step's executable. It is kept
// with the batch, and execution recomputes it from disk and refuses to run
// if the hash differs.
type Bound struct {
	V           string            `json:"v"`
	Title       string            `json:"title"`
	Reason      string            `json:"reason"`
	Expects     *Expects          `json:"expects,omitempty"`
	Files       []BoundFile       `json:"files"`
	Attachments []BoundAttachment `json:"attachments"`
	Steps       []BoundStep       `json:"steps"`
}

// BoundFile is a file write as bound: content by hash and size.
type BoundFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Group  string `json:"group,omitempty"`
	Delete bool   `json:"delete,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// BoundAttachment is an attachment as bound.
type BoundAttachment struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BoundStep is a step as submitted plus what it will actually execute.
type BoundStep struct {
	Step
	// Command is the argv that runs (typed operations expanded), with
	// attachment references still in "attachment:<name>" form.
	Command []string `json:"command"`
	Exe     Exe      `json:"exe"`
}

// Exe is a step's executable: a root-owned file on the host, or a staged
// attachment (Path "attachment:<name>").
type Exe struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Hash is the batch's identity: hex sha256 of the canonical encoding.
func (b *Bound) Hash() (string, error) {
	c, err := Canonical(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// TrustFunc checks that the file at path (and every directory above it) can
// be changed only by root. It returns nil if so.
type TrustFunc func(path string) error

// RootOwned is the real TrustFunc: path must exist, and it and every
// directory above it must be owned by uid and not writable by group or
// other. Symbolic links are followed and the target's chain checked too.
func RootOwned(uid int) TrustFunc {
	return func(p string) error {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return err
		}
		for _, q := range []string{p, real} {
			for cur := q; ; cur = filepath.Dir(cur) {
				fi, err := os.Lstat(cur)
				if err != nil {
					return err
				}
				st, ok := fi.Sys().(*syscall.Stat_t)
				if !ok {
					return fmt.Errorf("%s: no ownership information", cur)
				}
				if int(st.Uid) != uid {
					return fmt.Errorf("%s is owned by uid %d, not %d", cur, st.Uid, uid)
				}
				if fi.Mode()&fs.ModeSymlink == 0 && fi.Mode().Perm()&0o022 != 0 {
					return fmt.Errorf("%s is writable by group or other (mode %04o)", cur, fi.Mode().Perm())
				}
				if cur == "/" || cur == "." {
					break
				}
			}
		}
		return nil
	}
}

// resolveExe finds argv0 the way the step's exec will: an absolute path as
// is, a bare name in the step's PATH. It returns the symlink-free path.
func resolveExe(argv0, pathEnv string) (string, error) {
	if strings.Contains(argv0, "/") {
		if !filepath.IsAbs(argv0) {
			return "", fmt.Errorf("executable %q: give an absolute path, a bare name, or an attachment", argv0)
		}
		return filepath.EvalSymlinks(argv0)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			continue
		}
		cand := filepath.Join(dir, argv0)
		fi, err := os.Stat(cand)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return filepath.EvalSymlinks(cand)
	}
	return "", fmt.Errorf("executable %q: not found in PATH %s", argv0, pathEnv)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stepPath is the PATH a step runs with.
func stepPath(s *Step) string {
	if p, ok := s.Env["PATH"]; ok {
		return p
	}
	return DefaultPath
}

// bind validates b and computes what its hash covers, checking every
// executable and every file argv names (design §4.3 steps 2-3). files and
// attachments are the decoded contents normalize returned.
func bind(b *Batch, files, attachments [][]byte, trust TrustFunc) (*Bound, error) {
	bd := &Bound{V: HashVersion, Title: b.Title, Reason: b.Reason, Expects: b.Expects,
		Files: []BoundFile{}, Attachments: []BoundAttachment{}, Steps: []BoundStep{}}
	for i, f := range b.Files {
		bf := BoundFile{Path: f.Path, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Delete: f.Delete}
		if !f.Delete {
			bf.Size, bf.SHA256 = int64(len(files[i])), sha(files[i])
		}
		bd.Files = append(bd.Files, bf)
	}
	att := map[string]BoundAttachment{}
	for i, a := range b.Attachments {
		ba := BoundAttachment{Name: a.Name, Mode: a.Mode, Size: int64(len(attachments[i])), SHA256: sha(attachments[i])}
		att[a.Name] = ba
		bd.Attachments = append(bd.Attachments, ba)
	}
	for i := range b.Steps {
		s := b.Steps[i]
		cmd := s.Command()
		bs := BoundStep{Step: s, Command: cmd}
		if name, ok := strings.CutPrefix(cmd[0], AttachmentPrefix); ok {
			a := att[name]
			if m, _ := strconv.ParseUint(a.Mode, 8, 32); m&0o100 == 0 {
				return nil, fmt.Errorf("steps[%d] executes attachment %q, whose mode %s is not executable", i, name, a.Mode)
			}
			bs.Exe = Exe{Path: cmd[0], SHA256: a.SHA256}
		} else {
			p, err := resolveExe(cmd[0], stepPath(&s))
			if err != nil {
				return nil, fmt.Errorf("steps[%d]: %w", i, err)
			}
			if err := trust(p); err != nil {
				return nil, fmt.Errorf("steps[%d]: executable %s is not root-controlled: %v; attach it instead", i, p, err)
			}
			h, err := hashFile(p)
			if err != nil {
				return nil, fmt.Errorf("steps[%d]: %w", i, err)
			}
			bs.Exe = Exe{Path: p, SHA256: h}
		}
		for j, arg := range cmd[1:] {
			for _, p := range argPaths(arg, s.Cwd) {
				if _, err := os.Lstat(p); err != nil {
					continue
				}
				if err := trust(p); err != nil {
					return nil, fmt.Errorf("steps[%d].argv[%d] names %s, which an agent could change (%v): put it in the batch's attachments or files instead", i, j+1, p, err)
				}
			}
		}
		bd.Steps = append(bd.Steps, bs)
	}
	return bd, nil
}

// argPaths returns the filesystem paths an argv element may name: the
// element itself, and the value of a --flag=value, when they contain a
// slash. Relative ones are resolved against cwd.
func argPaths(arg, cwd string) []string {
	if strings.HasPrefix(arg, AttachmentPrefix) || strings.Contains(arg, "://") {
		return nil
	}
	var out []string
	cands := []string{arg}
	if _, v, ok := strings.Cut(arg, "="); ok {
		cands = append(cands, v)
	}
	for _, c := range cands {
		if !strings.Contains(c, "/") || strings.ContainsRune(c, 0) {
			continue
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(cwd, c)
		}
		out = append(out, filepath.Clean(c))
	}
	return out
}

// Staging directory layout, under <state>/batches/<hash>/:
//
//	bound.json         the Bound the hash covers
//	files/<i>          content of file write i
//	attachments/<name> attachment content, with its mode
const (
	boundFile      = "bound.json"
	filesDir       = "files"
	attachmentsDir = "attachments"
)

// stage writes the batch's content into dir/<hash>, root-owned and 0700,
// unless an identical staging directory is already there. It returns the
// staging directory.
func stage(batchesDir, hash string, bd *Bound, files, attachments [][]byte) (string, error) {
	final := filepath.Join(batchesDir, hash)
	if _, err := os.Stat(final); err == nil {
		if verr := verifyStaged(final, bd); verr == nil {
			return final, nil
		}
		// A damaged directory for this hash: replace it.
		if err := os.RemoveAll(final); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(batchesDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(batchesDir, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return "", err
	}
	for _, d := range []string{filesDir, attachmentsDir} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0o700); err != nil {
			return "", err
		}
	}
	for i, f := range bd.Files {
		if f.Delete {
			continue
		}
		if err := writeSynced(filepath.Join(tmp, filesDir, strconv.Itoa(i)), files[i], 0o600); err != nil {
			return "", err
		}
	}
	for i, a := range bd.Attachments {
		m, _ := strconv.ParseUint(a.Mode, 8, 32)
		// Staged attachments are root's: no group or other access at all.
		if err := writeSynced(filepath.Join(tmp, attachmentsDir, a.Name), attachments[i], fs.FileMode(m)&0o700); err != nil {
			return "", err
		}
	}
	raw, err := json.MarshalIndent(bd, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeSynced(filepath.Join(tmp, boundFile), raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}

func writeSynced(p string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ErrMismatch is returned when something a batch is bound to has changed.
var ErrMismatch = errors.New("hash mismatch")

// verifyStaged re-hashes the staged content of dir against bd.
func verifyStaged(dir string, bd *Bound) error {
	for i, f := range bd.Files {
		if f.Delete {
			continue
		}
		if err := checkContent(filepath.Join(dir, filesDir, strconv.Itoa(i)), f.Size, f.SHA256, -1); err != nil {
			return fmt.Errorf("staged file for %s: %w", f.Path, err)
		}
	}
	for _, a := range bd.Attachments {
		m, _ := strconv.ParseUint(a.Mode, 8, 32)
		if err := checkContent(filepath.Join(dir, attachmentsDir, a.Name), a.Size, a.SHA256, int(m&0o700)); err != nil {
			return fmt.Errorf("staged attachment %s: %w", a.Name, err)
		}
	}
	return nil
}

// checkContent checks a staged file's size, sha256 and (if mode >= 0) mode.
// The file must be a regular file, not a link to one.
func checkContent(p string, size int64, want string, mode int) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", ErrMismatch)
	}
	if mode >= 0 && fi.Mode().Perm() != fs.FileMode(mode) {
		return fmt.Errorf("%w: mode %04o, bound %04o", ErrMismatch, fi.Mode().Perm(), mode)
	}
	if fi.Size() != size {
		return fmt.Errorf("%w: %d bytes, bound %d", ErrMismatch, fi.Size(), size)
	}
	h, err := hashFile(p)
	if err != nil {
		return err
	}
	if h != want {
		return fmt.Errorf("%w: sha256 %s, bound %s", ErrMismatch, h[:16], want[:16])
	}
	return nil
}

// verifyBound re-checks everything bd is bound to just before execution:
// that bd still hashes to hash, the staged content, and every host
// executable's trust and content.
func verifyBound(dir, hash string, bd *Bound, trust TrustFunc) error {
	h, err := bd.Hash()
	if err != nil {
		return err
	}
	if h != hash {
		return fmt.Errorf("%w: the batch record hashes to %s, approved %s", ErrMismatch, h[:16], hash[:16])
	}
	if err := verifyStaged(dir, bd); err != nil {
		return err
	}
	for i, s := range bd.Steps {
		if strings.HasPrefix(s.Exe.Path, AttachmentPrefix) {
			continue
		}
		if err := trust(s.Exe.Path); err != nil {
			return fmt.Errorf("%w: steps[%d] executable %s: %v", ErrMismatch, i, s.Exe.Path, err)
		}
		h, err := hashFile(s.Exe.Path)
		if err != nil {
			return fmt.Errorf("%w: steps[%d] executable: %v", ErrMismatch, i, err)
		}
		if h != s.Exe.SHA256 {
			return fmt.Errorf("%w: steps[%d] executable %s changed (sha256 %s, bound %s)", ErrMismatch, i, s.Exe.Path, h[:16], s.Exe.SHA256[:16])
		}
	}
	return nil
}
