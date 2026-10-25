package approval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Snapshot backends (design §4.5). GCE PD comes with GCP mode (M3).
const (
	SnapBtrfs   = "btrfs"
	SnapZFS     = "zfs"
	SnapLVMThin = "lvm-thin"
	SnapNone    = "none"
)

// NoSnapshotMessage is what a batch's presentation says when no backend is
// available.
const NoSnapshotMessage = "no snapshot available — not undoable"

// Snapshotter takes a snapshot of the root filesystem before a batch runs.
type Snapshotter interface {
	// Kind is one of the Snap* constants.
	Kind() string
	// Take snapshots the root filesystem, naming it after label, and
	// returns an id a human can roll back to by hand.
	Take(ctx context.Context, label string) (id string, err error)
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, argv ...string) ([]byte, error)

// ExecRunner runs commands for real, with a minimal environment.
func ExecRunner(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + DefaultPath, "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// SnapshotHost is what detection reads: the mount table and commands.
type SnapshotHost struct {
	// MountInfo returns /proc/self/mountinfo.
	MountInfo func() ([]byte, error)
	Run       Runner
	// BtrfsDir is where btrfs snapshots go; default /.silo-snapshots.
	BtrfsDir string
}

// RealSnapshotHost reads the real mount table and runs real commands.
func RealSnapshotHost() SnapshotHost {
	return SnapshotHost{
		MountInfo: func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") },
		Run:       ExecRunner,
	}
}

// rootMount is the "/" line of mountinfo.
type rootMount struct {
	fstype, source, root string
}

// parseRootMount finds the last mount of "/" in mountinfo (the one that is
// visible). Format: id parent maj:min root mountpoint opts [tags] - fstype source superopts.
func parseRootMount(info []byte) (rootMount, error) {
	var rm rootMount
	found := false
	for _, line := range strings.Split(string(info), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 5 || len(rf) < 2 || lf[4] != "/" {
			continue
		}
		rm, found = rootMount{fstype: rf[0], source: unescapeMount(rf[1]), root: unescapeMount(lf[3])}, true
	}
	if !found {
		return rm, errors.New("no mount of / in mountinfo")
	}
	return rm, nil
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// DetectSnapshotter works out which backend the root filesystem supports:
// btrfs when / is a btrfs subvolume, ZFS when / is a ZFS dataset, LVM thin
// when / is on a thin logical volume, and none otherwise. Detection never
// fails: anything it cannot work out is "none".
func DetectSnapshotter(ctx context.Context, h SnapshotHost) Snapshotter {
	if h.MountInfo == nil || h.Run == nil {
		return noSnapshot{}
	}
	info, err := h.MountInfo()
	if err != nil {
		return noSnapshot{}
	}
	rm, err := parseRootMount(info)
	if err != nil {
		return noSnapshot{}
	}
	switch rm.fstype {
	case "btrfs":
		if _, err := h.Run(ctx, "btrfs", "subvolume", "show", "/"); err != nil {
			return noSnapshot{}
		}
		dir := h.BtrfsDir
		if dir == "" {
			dir = "/.silo-snapshots"
		}
		return btrfsSnap{run: h.Run, dir: dir}
	case "zfs":
		if _, err := h.Run(ctx, "zfs", "list", "-H", "-o", "name", "--", rm.source); err != nil {
			return noSnapshot{}
		}
		return zfsSnap{run: h.Run, dataset: rm.source}
	}
	if strings.HasPrefix(rm.source, "/dev/") {
		out, err := h.Run(ctx, "lvs", "--noheadings", "--separator", "|", "-o", "lv_attr,vg_name,lv_name", "--", rm.source)
		if err != nil {
			return noSnapshot{}
		}
		parts := strings.Split(strings.TrimSpace(string(out)), "|")
		// lv_attr's first character is the volume type: V is a thin volume.
		if len(parts) == 3 && strings.HasPrefix(strings.TrimSpace(parts[0]), "V") {
			return lvmThinSnap{run: h.Run, vg: strings.TrimSpace(parts[1]), lv: strings.TrimSpace(parts[2])}
		}
	}
	return noSnapshot{}
}

type noSnapshot struct{}

func (noSnapshot) Kind() string { return SnapNone }
func (noSnapshot) Take(context.Context, string) (string, error) {
	return "", errors.New(NoSnapshotMessage)
}

type btrfsSnap struct {
	run Runner
	dir string
}

func (b btrfsSnap) Kind() string { return SnapBtrfs }
func (b btrfsSnap) Take(ctx context.Context, label string) (string, error) {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return "", err
	}
	dst := b.dir + "/silo-" + label
	if _, err := b.run(ctx, "btrfs", "subvolume", "snapshot", "-r", "/", dst); err != nil {
		return "", err
	}
	return dst, nil
}

type zfsSnap struct {
	run     Runner
	dataset string
}

func (z zfsSnap) Kind() string { return SnapZFS }
func (z zfsSnap) Take(ctx context.Context, label string) (string, error) {
	id := z.dataset + "@silo-" + label
	if _, err := z.run(ctx, "zfs", "snapshot", "-r", "--", id); err != nil {
		return "", err
	}
	return id, nil
}

type lvmThinSnap struct {
	run    Runner
	vg, lv string
}

func (l lvmThinSnap) Kind() string { return SnapLVMThin }
func (l lvmThinSnap) Take(ctx context.Context, label string) (string, error) {
	name := "silo-" + label
	if _, err := l.run(ctx, "lvcreate", "--snapshot", "--name", name, "--", l.vg+"/"+l.lv); err != nil {
		return "", err
	}
	return l.vg + "/" + name, nil
}

// SnapshotStatus is the line a batch's presentation shows about snapshots.
func SnapshotStatus(kind string) string {
	if kind == "" || kind == SnapNone {
		return NoSnapshotMessage
	}
	return "Snapshot: " + kind + ", taken before run"
}
