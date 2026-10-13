package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Dir is the dir driver: a directory per silo under Root, a directory
// per volume under the silo's volumes/, and one per repo under its
// repos/.
//
//	<Root>/<silo id>/                     the silo's subtree
//	<Root>/<silo id>/volumes/<volume>/    a volume
//	<Root>/<silo id>/repos/<repo id>.git/ a repo, owned by the process
//
// Create is mkdir plus ownership, Delete is a recursive remove and
// Usage is a walk. Sizes are charged, not enforced, and it takes no
// snapshots. In production it runs in the namespace holder, where the
// silos' subuids are mapped; in tests it runs in silod's own process.
type Dir struct {
	// Root is the data directory's silos/.
	Root string
	// Owner, if set, is who a silo's directories belong to: the uid
	// and gid that container root maps to for that silo. ok false, or
	// a nil Owner, leaves them owned by the process.
	Owner func(silo string) (uid, gid int, ok bool)
}

var _ Driver = (*Dir)(nil)

// DirName is the dir driver's Name.
const DirName = "dir"

func (d *Dir) Name() string { return DirName }

func (d *Dir) Guarantees() Guarantees { return Guarantees{} }

// Path is where ref is kept. It doesn't check that it exists.
func (d *Dir) Path(ref Ref) (string, error) {
	if err := ref.Valid(); err != nil {
		return "", err
	}
	switch {
	case ref.Volume != "":
		return filepath.Join(d.Root, ref.Silo, "volumes", ref.Volume), nil
	case ref.Repo != "":
		return filepath.Join(d.Root, ref.Silo, "repos", ref.Repo+".git"), nil
	}
	return filepath.Join(d.Root, ref.Silo), nil
}

// Create makes ref's directory. A volume or a repo needs its silo's
// subtree to exist already: the subtree is made when the silo is, and
// a volume for a silo with none is a bug, not something to paper over.
// size is not enforced. A repo's directories are left to the process,
// whatever Owner says.
func (d *Dir) Create(ctx context.Context, ref Ref, size int64) (string, error) {
	p, err := d.Path(ref)
	if err != nil {
		return "", err
	}
	own := ref.Repo == ""
	if ref.Volume != "" || ref.Repo != "" {
		silo, _ := d.Path(Ref{Silo: ref.Silo})
		if _, err := os.Stat(silo); err != nil {
			return "", fmt.Errorf("storage: %s: silo %s has no subtree: %w", ref, ref.Silo, err)
		}
		if err := d.mkdir(ref.Silo, filepath.Dir(p), own); err != nil {
			return "", err
		}
	}
	if err := d.mkdir(ref.Silo, p, own); err != nil {
		return "", err
	}
	return p, nil
}

func (d *Dir) mkdir(silo, p string, own bool) error {
	if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("storage: %w", err)
	}
	if d.Owner == nil || !own {
		return nil
	}
	uid, gid, ok := d.Owner(silo)
	if !ok {
		return nil
	}
	if err := os.Lchown(p, uid, gid); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	return nil
}

// Delete removes ref's directory and everything in it. One that is
// gone already is not an error.
func (d *Dir) Delete(ctx context.Context, ref Ref) error {
	p, err := d.Path(ref)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if ref.Volume != "" {
		// The label after the volume: a volume that is half gone keeps
		// it.
		l, _ := d.labelPath(ref)
		if err := os.Remove(l); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: %w", err)
		}
	}
	return nil
}

// labelPath is where ref's label is kept.
func (d *Dir) labelPath(ref Ref) (string, error) {
	if err := ref.Valid(); err != nil {
		return "", err
	}
	switch {
	case ref.Repo != "":
		return "", fmt.Errorf("storage: %s: repos are not labelled", ref)
	case ref.Volume != "":
		return filepath.Join(d.Root, ref.Silo, "volumes", "."+ref.Volume+SeqFile), nil
	}
	return filepath.Join(d.Root, ref.Silo, SeqFile), nil
}

// Label writes ref's label, unless it has one. The label belongs to
// the process, not to the silo's ids, so the silo can't change it.
func (d *Dir) Label(ctx context.Context, ref Ref, seq uint64) error {
	p, err := d.labelPath(ref)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	_, err = f.WriteString(strconv.FormatUint(seq, 10) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return fmt.Errorf("storage: %w", err)
	}
	return nil
}

// List reads every silo's subtree under Root, and the volumes in it,
// with their labels. Entries that aren't a silo's or a volume's
// directory are skipped.
func (d *Dir) List(ctx context.Context) ([]Entry, error) {
	silos, err := os.ReadDir(d.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	var out []Entry
	for _, s := range silos {
		ref := Ref{Silo: s.Name()}
		if !s.IsDir() || ref.Valid() != nil {
			continue
		}
		out = append(out, Entry{Ref: ref, Seq: d.readLabel(ref)})
		vols, err := os.ReadDir(filepath.Join(d.Root, s.Name(), "volumes"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("storage: %w", err)
		}
		for _, v := range vols {
			ref := Ref{Silo: s.Name(), Volume: v.Name()}
			if !v.IsDir() || ref.Valid() != nil {
				continue
			}
			out = append(out, Entry{Ref: ref, Seq: d.readLabel(ref)})
		}
	}
	return out, nil
}

// readLabel is ref's label, 0 if it has none or it can't be read.
func (d *Dir) readLabel(ref Ref) uint64 {
	p, err := d.labelPath(ref)
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// Usage walks ref's directory. A file with several links in it is
// counted once. ref missing is an error wrapping fs.ErrNotExist.
func (d *Dir) Usage(ctx context.Context, ref Ref) (Usage, error) {
	p, err := d.Path(ref)
	if err != nil {
		return Usage{}, err
	}
	var u Usage
	seen := map[fileID]bool{}
	err = filepath.WalkDir(p, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			// A file removed while the walk ran is not in use.
			if path != p && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		bytes, id, linked := onDisk(fi)
		if linked {
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		u.Bytes += bytes
		u.Inodes++
		return nil
	})
	if err != nil {
		return Usage{}, fmt.Errorf("storage: usage of %s: %w", ref, err)
	}
	return u, nil
}

// Snapshot is unsupported: plain directories have no point-in-time
// copy.
func (d *Dir) Snapshot(ctx context.Context, ref Ref, name string) (Snapshot, error) {
	return Snapshot{}, ErrSnapshotUnsupported
}
