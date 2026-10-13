package nsd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/store"
)

// Range is one line of a uid_map or gid_map: Size ids from Inside, in
// the namespace, map to Outside in its parent.
type Range struct {
	Inside, Outside, Size uint32
}

// Mapping is the holder's user namespace mapping. Files in volumes are
// owned by subuids on disk, so a change between runs would shift every
// volume's owners; silod records it in the store and refuses a
// different one (call L29).
type Mapping struct {
	UID []Range `json:"uid"`
	GID []Range `json:"gid"`
}

func formatRanges(rs []Range) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%d:%d:%d", r.Inside, r.Outside, r.Size))
	}
	return strings.Join(parts, ",")
}

// String is the form recorded in the store: "uid=0:1000:1,1:100000:65536
// gid=...".
func (m Mapping) String() string {
	return "uid=" + formatRanges(m.UID) + " gid=" + formatRanges(m.GID)
}

// Size is how many ids the holder's namespace maps, the smaller of uids
// and gids.
func (m Mapping) Size() uint32 {
	count := func(rs []Range) (n uint32) {
		for _, r := range rs {
			n += r.Size
		}
		return n
	}
	return min(count(m.UID), count(m.GID))
}

// ParseIDMap reads /proc/<pid>/uid_map or gid_map.
func ParseIDMap(r io.Reader) ([]Range, error) {
	var out []Range
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("id map line %q", sc.Text())
		}
		var v [3]uint32
		for i := range f {
			n, err := strconv.ParseUint(f[i], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("id map line %q: %w", sc.Text(), err)
			}
			v[i] = uint32(n)
		}
		out = append(out, Range{v[0], v[1], v[2]})
	}
	return out, sc.Err()
}

// ReadMapping reads a process's mapping from /proc.
func ReadMapping(pid int) (Mapping, error) {
	var m Mapping
	for _, f := range []struct {
		name string
		dst  *[]Range
	}{{"uid_map", &m.UID}, {"gid_map", &m.GID}} {
		file, err := os.Open(fmt.Sprintf("/proc/%d/%s", pid, f.name))
		if err != nil {
			return m, err
		}
		*f.dst, err = ParseIDMap(file)
		file.Close()
		if err != nil {
			return m, err
		}
	}
	return m, nil
}

// SubID is a user's range in /etc/subuid or /etc/subgid.
type SubID struct {
	Start, Count uint32
}

// ParseSubID finds the first range for user (by name or by uid, as
// newuidmap accepts either) in an /etc/subuid-format file.
func ParseSubID(r io.Reader, user string, uid int) (SubID, bool, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 3 || (f[0] != user && f[0] != strconv.Itoa(uid)) {
			continue
		}
		start, err1 := strconv.ParseUint(f[1], 10, 32)
		count, err2 := strconv.ParseUint(f[2], 10, 32)
		if err := errors.Join(err1, err2); err != nil {
			return SubID{}, false, fmt.Errorf("subordinate id line %q: %w", line, err)
		}
		return SubID{uint32(start), uint32(count)}, true, nil
	}
	return SubID{}, false, sc.Err()
}

// HolderMapping is the mapping silo-nsd asks newuidmap for: root in the
// namespace is the user's own id, and 1 up is the subordinate range.
func HolderMapping(uid, gid int, subuid, subgid SubID) Mapping {
	return Mapping{
		UID: []Range{{0, uint32(uid), 1}, {1, subuid.Start, subuid.Count}},
		GID: []Range{{0, uint32(gid), 1}, {1, subgid.Start, subgid.Count}},
	}
}

// SettingMapping is the store setting the holder's mapping is recorded
// under, at the first start on a store.
const SettingMapping = "nsd_id_mapping"

// ErrMappingChanged is CheckMapping's refusal.
var ErrMappingChanged = errors.New("nsd: the user namespace mapping changed")

// CheckMapping records m in st at the first start and refuses every
// later start whose mapping differs, naming the change: an edit to
// /etc/subuid would otherwise shift the owner of every file in every
// volume (call L29).
func CheckMapping(ctx context.Context, st store.Store, m Mapping) error {
	return st.Tx(ctx, func(tx store.Tx) error {
		got, ok, err := store.ReadSetting(ctx, tx, SettingMapping)
		if err != nil {
			return err
		}
		if !ok {
			return store.WriteSetting(ctx, tx, SettingMapping, m.String())
		}
		if got != m.String() {
			return fmt.Errorf("%w: the store recorded %q, the holder has %q; restore /etc/subuid and /etc/subgid, or move the volumes' owners before starting", ErrMappingChanged, got, m.String())
		}
		return nil
	})
}

// ContainerRange is the slice of the holder's namespace that container
// range n maps to: 65536 ids from 1 + n*65536. Ranges are per silo when
// the subordinate range has room, and shared (range 0) otherwise.
func ContainerRange(m Mapping, n uint32) (Range, error) {
	const size = 65536
	ranges := (m.Size() - 1) / size
	if m.Size() < 1+size || n >= ranges {
		return Range{}, fmt.Errorf("nsd: container range %d is outside the holder's %d ids (%d ranges)", n, m.Size(), ranges)
	}
	return Range{Inside: 0, Outside: 1 + n*size, Size: size}, nil
}
