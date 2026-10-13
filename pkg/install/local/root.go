package local

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/install"
)

// Silod is what the root and docker context steps ask silod, on its
// socket.
type Silod interface {
	Health(ctx context.Context) (install.SilodHealth, error)
	Bootstrap(ctx context.Context, req *install.BootstrapRequest) error
}

// SilodTimeout is how long the root step waits for silod to serve once
// its unit has started.
var SilodTimeout = 2 * time.Minute

// The root's capacity (local-backend.md call L20): (host total −
// reserve) × overcommit. The reserve is 1 vCPU, 2 GiB or 10% of memory
// (whichever is larger), and 10% of the data filesystem. Memory and
// disk aren't overcommitted.
const (
	DefaultCPUOvercommit = 2.0
	MaxCPUOvercommit     = 8.0
)

// RootCounts are the root's count classes, which the host's totals
// don't bound: as many as any one machine is likely to want.
var RootCounts = map[string]int64{"containers": 1000, "networks": 256, "volumes": 1000}

// silodSocket is where silod.service serves: units.Config.Socket, with
// the user manager's %t filled in.
func silodSocket(c Config, h Host) string {
	if c.Mode == units.System {
		return "/run/silo/silo.sock"
	}
	return filepath.Join(runtimeDir(h), "silo", "silo.sock")
}

// runtimeDir is the installer's $XDG_RUNTIME_DIR.
func runtimeDir(h Host) string {
	if d := h.Env("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
		return d
	}
	return fmt.Sprintf("/run/user/%d", h.EUID)
}

// installerUID is the installing user's uid, the root's admin: the
// user who ran sudo, for the system install.
func installerUID(h Host) int {
	if h.EUID == 0 {
		if n, err := strconv.Atoi(h.Env("SUDO_UID")); err == nil {
			return n
		}
	}
	return h.EUID
}

// rootStep creates the root silo once silod serves (local-backend.md
// call L23, step 4): backing local, --address-space, the classes the
// host offers, the capacity call L20 gives, ingress host, and the
// installing user, by uid, as its admin. silod accepts bootstrap only
// while there is no root, so once there is one this step is ok whatever
// the flags now say.
type rootStep struct {
	c Config
	h Host
}

type rootObserved struct {
	Health      *install.SilodHealth `json:"health,omitempty"`
	Unreachable string               `json:"unreachable,omitempty"`
	Classes     []string             `json:"classes"`
	CPUs        int                  `json:"cpus"`
	MemoryMB    int64                `json:"memoryMB"`
	DiskGB      int64                `json:"diskGB"`
}

func (s *rootStep) Name() string                  { return "root" }
func (s *rootStep) NewObserved() install.Observed { return &rootObserved{} }

func (s *rootStep) Observe(ctx context.Context) (install.Observed, error) {
	o := &rootObserved{}
	o.Health, o.Unreachable = health(ctx, s.c, s.h)
	if o.Health != nil && o.Health.Root {
		return o, nil
	}
	_, report := s.h.Probe(s.c)
	o.Classes = classesOn(s.c, s.h, report)
	o.CPUs = s.h.CPUs
	if b, err := s.h.ReadFile("/proc/meminfo"); err == nil {
		o.MemoryMB = memTotalMB(string(b))
	}
	if fs, err := s.h.Statfs(existing(s.h, s.c.DataDir)); err == nil {
		o.DiskGB = int64(fs.Size >> 30)
	}
	return o, nil
}

// health is silod's /silod/health on its socket, or why it can't be
// read.
func health(ctx context.Context, c Config, h Host) (*install.SilodHealth, string) {
	api, err := h.Silod(silodSocket(c, h))
	if err != nil {
		return nil, err.Error()
	}
	got, err := api.Health(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return &got, ""
}

// memTotalMB is /proc/meminfo's MemTotal, in MiB.
func memTotalMB(meminfo string) int64 {
	for _, line := range strings.Split(meminfo, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// existing is path, or its nearest ancestor that exists: the data
// directory's filesystem before the directory is made.
func existing(h Host, path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := h.Stat(p); err == nil || p == filepath.Dir(p) {
			return p
		}
	}
}

// overcommit is --cpu-overcommit, DefaultCPUOvercommit when unset.
func (s *rootStep) overcommit() float64 {
	if s.c.CPUOvercommit == 0 {
		return DefaultCPUOvercommit
	}
	return s.c.CPUOvercommit
}

// capacity is the root's capacity from o's host totals (call L20).
func (s *rootStep) capacity(o *rootObserved) map[string]int64 {
	mem := o.MemoryMB - max(2048, o.MemoryMB/10)
	out := map[string]int64{
		"vcpu_m":    int64(float64(max(0, o.CPUs-1)*1000) * s.overcommit()),
		"memory_mb": max(0, mem),
		"disk_gb":   o.DiskGB - (o.DiskGB+9)/10,
	}
	for k, v := range RootCounts {
		out[k] = v
	}
	return out
}

// isolation is the root's isolation policy: every class the host
// offers, default vm when it offers vm (local-backend.md §3).
func isolation(classes []string) *install.RootIsolation {
	p := &install.RootIsolation{Allowed: classes}
	if len(classes) > 0 {
		p.Default = classes[len(classes)-1]
	}
	for _, c := range classes {
		if c == "vm" {
			p.Default = "vm"
		}
	}
	return p
}

func (s *rootStep) request(o *rootObserved) *install.BootstrapRequest {
	capacity := map[string]string{}
	for k, v := range s.capacity(o) {
		capacity[k] = strconv.FormatInt(v, 10)
	}
	return &install.BootstrapRequest{
		AddressSpace: s.c.AddressSpace,
		Capacity:     map[string]map[string]string{"global": capacity},
		Egress:       &install.RootEgress{Allow: []string{}, Deny: []string{}},
		Admins:       []string{"uid:" + strconv.Itoa(installerUID(s.h))},
		Backing:      &install.RootBacking{Kind: "local"},
		Isolation:    isolation(o.Classes),
		Ingress:      "host",
	}
}

func (s *rootStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*rootObserved)
	if o.Health != nil && o.Health.Root {
		return []install.Change{{Action: install.ActionOK, Object: "/", Detail: "exists (changed through the fence API, not by install)"}}
	}
	if s.c.CPUOvercommit < 0 || s.c.CPUOvercommit > MaxCPUOvercommit {
		return []install.Change{{Action: install.ActionFail, Object: "/", Detail: fmt.Sprintf("--cpu-overcommit %g", s.c.CPUOvercommit),
			Why: "CPU overcommit only slows things down, up to a point", Fix: fmt.Sprintf("give 1 to %g", MaxCPUOvercommit)}}
	}
	req := s.request(o)
	space := s.c.AddressSpace
	if space == "" {
		space = "10.0.0.0/8"
	}
	caps := s.capacity(o)
	wait := 10 * time.Second
	if o.Health == nil {
		wait = 30 * time.Second // silod starting for the first time
	}
	return []install.Change{
		{Action: install.ActionCreate, Object: "/", Estimate: wait,
			Detail: fmt.Sprintf("backing local, blocks %s (minus 10.255.254.0/23, the gateway's and control ranges), isolation %s (default %s), ingress host, admin %s",
				space, strings.Join(req.Isolation.Allowed, " and "), req.Isolation.Default, req.Admins[0])},
		{Action: install.ActionCreate, Detail: fmt.Sprintf("capacity vcpu_m %d (%d CPUs less 1, ×%g), memory_mb %d, disk_gb %d, containers %d, networks %d, volumes %d; egress none",
			caps["vcpu_m"], o.CPUs, s.overcommit(),
			caps["memory_mb"], caps["disk_gb"], caps["containers"], caps["networks"], caps["volumes"])},
	}
}

func (s *rootStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*rootObserved)
	if o.Health == nil || !o.Health.Root {
		return []install.Change{{Action: install.ActionOK, Object: "/", Detail: "no root silod serves"}}
	}
	return []install.Change{{Action: install.ActionKeep, Object: "/",
		Detail: "the tree is in the store, which is kept with the data directory unless --delete-data"}}
}

func (s *rootStep) Apply(ctx context.Context, changes []install.Change) error {
	api, err := s.h.Silod(silodSocket(s.c, s.h))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(SilodTimeout)
	var h install.SilodHealth
	for {
		if h, err = api.Health(ctx); err == nil && h.Status == "ok" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("silod didn't serve on %s within %v (%v): see journalctl %s-u silod.service",
				silodSocket(s.c, s.h), SilodTimeout, err, map[bool]string{true: "--user "}[s.c.Mode == units.Rootless])
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if h.Root {
		return nil
	}
	obs, err := s.Observe(ctx)
	if err != nil {
		return err
	}
	if err := api.Bootstrap(ctx, s.request(obs.(*rootObserved))); err != nil {
		return err
	}
	fmt.Fprintf(s.h.Progress, "  created the root silo\n")
	return nil
}
