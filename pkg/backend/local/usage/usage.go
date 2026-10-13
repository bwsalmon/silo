// Package usage meters the local backend's host resources
// (local-backend.md §11, "Metering"): every minute silod reads each
// silo's slice in its user's systemd manager (rootless.Slice), cpu.stat's
// usage_usec, memory.current and memory.peak, and io.stat's bytes, and
// keeps the last reading of each silo, and of each agent's own cgroups
// (its tool container, and its harness's scope while a turn runs).
// describe shows a silo's as its usage.
//
// This is metering, not a budget: v1 charges no money for host
// resources, and the quota is the limit. A slice holds its child
// silos' slices, so a silo's reading includes theirs, as cgroups count.
package usage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Sample is one reading of a cgroup.
type Sample struct {
	SampledAt time.Time `json:"sample_time"`
	// CPUUsec is cpu.stat's usage_usec: CPU time used since the
	// cgroup was made.
	CPUUsec uint64 `json:"cpu_usec"`
	// MemoryBytes is memory.current, and MemoryPeakBytes memory.peak
	// (0 on kernels before 5.19, which have none).
	MemoryBytes     uint64 `json:"memory_bytes"`
	MemoryPeakBytes uint64 `json:"memory_peak_bytes,omitempty"`
	// ReadBytes and WriteBytes are io.stat's rbytes and wbytes, over
	// every device (0 where io isn't delegated).
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// Add is s with o's counts added: the reading of two cgroups as one.
// Its time is the later.
func (s Sample) Add(o Sample) Sample {
	if o.SampledAt.After(s.SampledAt) {
		s.SampledAt = o.SampledAt
	}
	s.CPUUsec += o.CPUUsec
	s.MemoryBytes += o.MemoryBytes
	s.MemoryPeakBytes += o.MemoryPeakBytes
	s.ReadBytes += o.ReadBytes
	s.WriteBytes += o.WriteBytes
	return s
}

// ErrNoCgroup is a cgroup that isn't there: a silo with nothing running
// has no slice yet.
var ErrNoCgroup = errors.New("usage: no such cgroup")

// Read reads the cgroup at dir. Only cpu.stat must be there; a file a
// controller that isn't delegated would have is read as zero.
func Read(dir string, now time.Time) (Sample, error) {
	s := Sample{SampledAt: now}
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return s, ErrNoCgroup
	}
	if err != nil {
		return s, err
	}
	s.CPUUsec = keyed(b)["usage_usec"]
	s.MemoryBytes = single(filepath.Join(dir, "memory.current"))
	s.MemoryPeakBytes = single(filepath.Join(dir, "memory.peak"))
	if b, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil {
		s.ReadBytes, s.WriteBytes = ioStat(b)
	}
	return s, nil
}

// keyed reads "key value" lines.
func keyed(b []byte) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}

func single(name string) uint64 {
	b, err := os.ReadFile(name)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// ioStat sums io.stat's rbytes and wbytes over its devices: lines of
// "<maj:min> rbytes=N wbytes=N rios=N ...".
func ioStat(b []byte) (r, w uint64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		for _, kv := range strings.Fields(sc.Text()) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				continue
			}
			switch k {
			case "rbytes":
				r += n
			case "wbytes":
				w += n
			}
		}
	}
	return r, w
}

// SliceDir is the cgroup directory of the silo whose ids, from the root
// down, are path, under manager, the user manager's cgroup directory
// (/sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service):
// systemd nests silo-<root>-<child>.slice in silo-<root>.slice in
// silo.slice.
func SliceDir(manager string, path ...string) string {
	parts := []string{manager, rootless.RootSlice}
	for i := range path {
		parts = append(parts, rootless.Slice(path[:i+1]...))
	}
	return filepath.Join(parts...)
}

// ManagerDir is the cgroup directory of uid's systemd user manager.
func ManagerDir(uid int) string {
	u := strconv.Itoa(uid)
	return filepath.Join("/sys/fs/cgroup/user.slice", "user-"+u+".slice", "user@"+u+".service")
}

// Meter samples silos' slices and agents' cgroups every Every. Its
// fields are set before Run.
type Meter struct {
	// Manager is the user manager's cgroup directory (ManagerDir).
	Manager string
	// Silos lists every silo, by its id path from the root down.
	Silos func(ctx context.Context) (map[string][]string, error)
	// Agents lists, for each agent, the cgroup directories that are its
	// own; nil meters no agent.
	Agents func(ctx context.Context) (map[string][]string, error)
	// Every is how often it samples; a minute if 0.
	Every time.Duration
	// Now is the clock; time.Now if nil.
	Now func() time.Time

	mu     sync.Mutex
	silos  map[string]Sample
	agents map[string]Sample
}

// Silo is silo's last reading; false if its slice has never been seen.
func (m *Meter) Silo(silo string) (Sample, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.silos[silo]
	return s, ok
}

// Agent is agent's last reading, its cgroups' summed, from the last
// time any of them was there; false if none ever was.
func (m *Meter) Agent(agent string) (Sample, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.agents[agent]
	return s, ok
}

// Sample reads every silo's slice and every agent's cgroups once. A
// silo whose slice isn't there (nothing of it has run) keeps no
// reading; one that was there and is gone keeps its last.
func (m *Meter) Sample(ctx context.Context) error {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	silos, err := m.Silos(ctx)
	if err != nil {
		return err
	}
	read := map[string]Sample{}
	for id, path := range silos {
		s, err := Read(SliceDir(m.Manager, path...), now())
		switch {
		case errors.Is(err, ErrNoCgroup):
		case err != nil:
			log.Printf("usage: silo %s: %v", id, err)
		default:
			read[id] = s
		}
	}
	agents := map[string]Sample{}
	if m.Agents != nil {
		dirs, err := m.Agents(ctx)
		if err != nil {
			return err
		}
		for a, ds := range dirs {
			var sum Sample
			seen := false
			for _, d := range ds {
				s, err := Read(d, now())
				if err != nil {
					continue
				}
				sum, seen = sum.Add(s), true
			}
			if seen {
				agents[a] = sum
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.silos == nil {
		m.silos, m.agents = map[string]Sample{}, map[string]Sample{}
	}
	for id := range m.silos {
		if _, ok := silos[id]; !ok {
			delete(m.silos, id) // deleted
		}
	}
	for id, s := range read {
		m.silos[id] = s
	}
	// An agent whose cgroups are gone (its harness's scope lasts one
	// turn) keeps its last reading.
	for a, s := range agents {
		m.agents[a] = s
	}
	return nil
}

// Run samples every Every until ctx is done.
func (m *Meter) Run(ctx context.Context) {
	every := m.Every
	if every == 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := m.Sample(ctx); err != nil && ctx.Err() == nil {
			log.Printf("usage: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SiloUsage implements fenceapi.UsageMeter.
func (m *Meter) SiloUsage(silo string) (fenceapi.ResourceUsage, bool) {
	s, ok := m.Silo(silo)
	return s.resource(), ok
}

// AgentUsage implements fenceapi.UsageMeter.
func (m *Meter) AgentUsage(agent string) (fenceapi.ResourceUsage, bool) {
	s, ok := m.Agent(agent)
	return s.resource(), ok
}

func (s Sample) resource() fenceapi.ResourceUsage {
	return fenceapi.ResourceUsage{SampleTime: s.SampledAt, CPUUsec: s.CPUUsec, MemoryBytes: s.MemoryBytes,
		MemoryPeakBytes: s.MemoryPeakBytes, ReadBytes: s.ReadBytes, WriteBytes: s.WriteBytes}
}

// StoreSilos lists st's silos by their id paths, for Meter.Silos.
func StoreSilos(st store.Store) func(ctx context.Context) (map[string][]string, error) {
	return func(ctx context.Context) (map[string][]string, error) {
		var all []*tree.Silo
		if err := st.Tx(ctx, func(tx store.Tx) error {
			var err error
			all, err = tree.All(ctx, tx)
			return err
		}); err != nil {
			return nil, err
		}
		parent := map[string]string{}
		for _, t := range all {
			parent[t.ID] = t.Parent
		}
		out := map[string][]string{}
		for _, t := range all {
			var path []string
			for id := t.ID; id != "" && len(path) <= tree.MaxDepth+1; id = parent[id] {
				path = append([]string{id}, path...)
			}
			out[t.ID] = path
		}
		return out, nil
	}
}

// ScopePrefix starts an ambient harness's scope (harness.ScopePrefix).
const ScopePrefix = "silo-agent-"

// HarnessScopes finds each agent's harness scope under the user manager
// at manager, wherever systemd-run put it (app.slice, or the manager's
// top), for Meter.Agents. A scope is there only while a turn runs.
func HarnessScopes(manager string) func(ctx context.Context) (map[string][]string, error) {
	return func(context.Context) (map[string][]string, error) {
		out := map[string][]string{}
		for _, pat := range []string{filepath.Join(manager, ScopePrefix+"*.scope"), filepath.Join(manager, "*", ScopePrefix+"*.scope")} {
			ms, err := filepath.Glob(pat)
			if err != nil {
				return nil, err
			}
			for _, d := range ms {
				a := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(d), ScopePrefix), ".scope")
				out[a] = append(out[a], d)
			}
		}
		return out, nil
	}
}
