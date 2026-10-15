// Package local is `silo install --backend local`
// (docs/silo/local-backend.md call L23, as part 2 changes it): a local,
// idempotent install of the local backend's units, in either of its two
// modes (docs/silo/local-rootless-storage.md, calls L33 and L34).
//
//   - Rootless, the default, run as the user silod will run as: the
//     one-time root steps are checked, and each that is missing is named
//     with the command that does it; the units are user units, and
//     lingering is turned on where the user may turn it on.
//   - System (--system), run as root: install creates the silo account,
//     in the kvm group, with subordinate id ranges for per-silo ranges,
//     and writes system units with User=silo. silod never runs as root.
//
// Its steps are install.Steps, so that the plan is printed, confirmed
// and applied as the GCP install's is: the account (system install),
// the prerequisite checks, lingering (rootless), the data directory,
// the pinned gVisor, the units, the root silo and a docker context for
// it. `silo uninstall --backend local` undoes them in reverse order,
// keeping the data directory unless --delete-data.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/datadir"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/hostdoor"
	"github.com/bwsalmon/silo/pkg/install"
)

// SubIDRanges is how many container ranges (nsd.ContainerRange, 65536
// ids each) the system install gives the silo account: one per silo
// that runs containers at once (call L29).
const SubIDRanges = 256

// Config is what install was asked for.
type Config struct {
	Mode    units.Mode
	DataDir string
	// User is the account silod runs as: the current user in rootless
	// mode, units.SystemUser for the system install.
	User string
	// Silod is the silod binary the units run.
	Silod string
	// RegistryHostPort is the registry's port for host clients on
	// 127.0.0.1 (--registry-host-port); 0 turns it off.
	RegistryHostPort int
	// ImageCacheSizeGB is the registry cache's budget, in GiB
	// (--image-cache-size): carved from the root's disk capacity and
	// charged to nobody. 0 is DefaultImageCacheGB of the data
	// directory's filesystem.
	ImageCacheSizeGB float64
	// AddressSpace is the root's block (--address-space); empty is
	// silod's default, 10.0.0.0/8.
	AddressSpace string
	// CPUOvercommit is the root's vcpu_m overcommit (call L20); 0 is
	// DefaultCPUOvercommit.
	CPUOvercommit float64
	// Uninstall is set for silo uninstall, which needs no silod binary,
	// and DeleteData is its --delete-data.
	Uninstall  bool
	DeleteData bool
}

// MinImageCacheGB is the smallest default cache budget, in GiB.
const MinImageCacheGB = 10

// imageCacheGB is c's cache budget: --image-cache-size, or
// DefaultImageCacheGB of the data directory's filesystem.
func imageCacheGB(c Config, h Host) float64 {
	if c.ImageCacheSizeGB > 0 {
		return c.ImageCacheSizeGB
	}
	if fs, err := h.Statfs(existing(h, c.DataDir)); err == nil {
		return DefaultImageCacheGB(int64(fs.Size))
	}
	return MinImageCacheGB
}

// DefaultImageCacheGB is --image-cache-size's default for a data
// filesystem of fsBytes (local-registry.md, "Eviction and the cache
// budget", call L74): 10% of it, and at least MinImageCacheGB.
func DefaultImageCacheGB(fsBytes int64) float64 {
	gb := float64(fsBytes) / 10 / (1 << 30)
	if gb < MinImageCacheGB {
		return MinImageCacheGB
	}
	return gb
}

// DefaultRegistryHostPort is --registry-host-port's default: on, at
// hostdoor.Port (local-registry.md's open question 37, answered on), so
// crane, skopeo and oras reach localhost:5115 after install with no
// flag. A uid mapped to no silo gets nothing from it.
const DefaultRegistryHostPort = hostdoor.Port

// Runner runs a command and returns its combined output. Tests replace
// it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the real Runner.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, err
}

// Host is what the steps read and run. Tests replace every part.
type Host struct {
	Run Runner
	// EUID is the installer's effective uid.
	EUID int
	// Probe runs the holder's and the runtimes' checks for c.User.
	Probe func(c Config) ([]nsd.Check, rootless.Report)
	// Lookup finds an account, and Groups its supplementary groups;
	// GroupExists reports whether a group does.
	Lookup      func(name string) (*user.User, error)
	GroupExists func(name string) bool
	InGroup     func(u *user.User, group string) bool
	// ReadFile and WriteFile are the unit files', and /etc/subuid's.
	ReadFile  func(name string) ([]byte, error)
	WriteFile func(name string, b []byte) error
	Stat      func(name string) (os.FileInfo, error)
	// Open reads a file; Install writes an executable from r, atomically,
	// renaming it into place only if reading r to its end succeeded;
	// RemoveAll removes a tree.
	Open      func(name string) (io.ReadCloser, error)
	Install   func(name string, r io.Reader) error
	RemoveAll func(name string) error
	// OpenDataDir makes or opens the data directory, as silod does.
	OpenDataDir func(path string) error
	// Fetch GETs a URL.
	Fetch func(ctx context.Context, url string) (io.ReadCloser, error)
	// Silod is silod's API on the Unix socket at path.
	Silod func(path string) (Silod, error)
	// Env reads the installer's environment.
	Env func(string) string
	// Statfs is the filesystem a path is on.
	Statfs func(path string) (FS, error)
	// CPUs is the host's online CPUs, and Arch its GOARCH.
	CPUs int
	Arch string
	// Self is this silo binary, which uninstall runs `silo unshare` as.
	Self string
	// UnitDir overrides units.Config.Dir.
	UnitDir string
	// Progress is where Apply says what it did.
	Progress io.Writer
}

// LocalHost is this machine.
func LocalHost() Host {
	return Host{
		Run:  Exec,
		EUID: os.Geteuid(),
		Probe: func(c Config) ([]nsd.Check, rootless.Report) {
			rh := rootless.LocalHost()
			if c.Mode == units.Rootless {
				// The installer is silod's user: probe as itself, user
				// namespaces included.
				return nsd.Probe(), rootless.Probe(rh)
			}
			// The system install probes for an account that may not
			// exist yet, and can't create a user namespace as it.
			h := nsd.Host{ReadFile: os.ReadFile, LookPath: exec.LookPath, Stat: os.Stat, User: c.User}
			if u, err := user.Lookup(c.User); err == nil {
				h.UID, _ = strconv.Atoi(u.Uid)
				h.GID, _ = strconv.Atoi(u.Gid)
				rh.Groups = groupIDs(u)
				rh.Cgroup.UID = h.UID
			}
			return nsd.ProbeHost(h), rootless.Probe(rh)
		},
		Lookup: user.Lookup,
		GroupExists: func(name string) bool {
			_, err := user.LookupGroup(name)
			return err == nil
		},
		InGroup: func(u *user.User, group string) bool {
			g, err := user.LookupGroup(group)
			if err != nil {
				return false
			}
			for _, id := range groupIDs(u) {
				if strconv.Itoa(id) == g.Gid {
					return true
				}
			}
			return false
		},
		ReadFile: os.ReadFile,
		WriteFile: func(name string, b []byte) error {
			if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			return os.WriteFile(name, b, 0o644)
		},
		Stat:      os.Stat,
		Open:      func(name string) (io.ReadCloser, error) { return os.Open(name) },
		Install:   installFile,
		RemoveAll: os.RemoveAll,
		OpenDataDir: func(path string) error {
			_, err := datadir.Open(path)
			return err
		},
		Fetch: func(ctx context.Context, url string) (io.ReadCloser, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
			}
			return resp.Body, nil
		},
		Silod: func(path string) (Silod, error) {
			return install.NewSilod(&http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			}})
		},
		Env:      os.Getenv,
		Statfs:   statfs,
		CPUs:     runtime.NumCPU(),
		Arch:     runtime.GOARCH,
		Self:     self(),
		Progress: os.Stdout,
	}
}

func self() string {
	p, _ := os.Executable()
	return p
}

// installFile writes r to name, mode 0755, through a temporary file in
// the same directory that is renamed into place only once r has been
// read to its end without error.
func installFile(name string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func groupIDs(u *user.User) []int {
	ids, _ := u.GroupIds()
	var out []int
	for _, s := range ids {
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Defaults fills in what c leaves empty: the account, the data
// directory and silod (the silod beside this binary, else on PATH).
func Defaults(c Config) (Config, error) {
	if c.Mode == "" {
		c.Mode = units.Rootless
	}
	if c.User == "" {
		if c.Mode == units.System {
			c.User = units.SystemUser
		} else if u, err := user.Current(); err == nil {
			c.User = u.Username
		}
	}
	if c.DataDir == "" {
		d, err := datadir.Default(c.Mode == units.System)
		if err != nil {
			return c, err
		}
		c.DataDir = d
	}
	if c.Silod == "" && !c.Uninstall {
		if self, err := os.Executable(); err == nil {
			if p := filepath.Join(filepath.Dir(self), "silod"); fileExists(p) {
				c.Silod = p
			}
		}
		if c.Silod == "" {
			p, err := exec.LookPath("silod")
			if err != nil {
				return c, errors.New("no silod beside silo or on PATH; give --silod")
			}
			c.Silod = p
		}
	}
	if abs, err := filepath.Abs(c.Silod); err == nil && c.Silod != "" {
		c.Silod = abs
	}
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	return c, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Steps are the install's steps, in order: the system account (system
// install only), the prerequisite checks, lingering (rootless only),
// the data directory, the pinned runtimes, the units, the root silo and
// its docker context. Uninstall undoes them in reverse order.
func Steps(c Config, h Host) []install.Step {
	var steps []install.Step
	if c.Mode == units.System {
		steps = append(steps, &accountStep{c: c, h: h})
	}
	steps = append(steps, &prereqStep{c: c, h: h})
	if c.Mode == units.Rootless {
		steps = append(steps, &lingerStep{c: c, h: h})
	}
	return append(steps, &dataStep{c: c, h: h}, &runtimesStep{c: c, h: h}, &unitsStep{c: c, h: h},
		&rootStep{c: c, h: h}, &dockerStep{c: c, h: h})
}

// unitsConfig is c's units.Config: silo-buildkitd only where buildkitd
// is installed, and the pinned gVisor first on the runtimes' PATH.
func unitsConfig(c Config, h Host, report rootless.Report) units.Config {
	uc := units.Config{Mode: c.Mode, Silod: c.Silod, DataDir: c.DataDir, RegistryHostPort: c.RegistryHostPort,
		ImageCacheSizeGB: imageCacheGB(c, h), CPUOvercommit: c.CPUOvercommit}
	if _, ok := pinned(h); ok {
		uc.RuntimePath = []string{RuntimesDir(c)}
	}
	if c.Mode == units.System {
		uc.User = c.User
	}
	for _, ch := range report.Checks {
		if ch.Name == "buildkitd" && ch.OK {
			uc.Buildkitd = true
		}
	}
	return uc
}

// systemctl runs systemctl in c's manager.
func systemctl(ctx context.Context, c Config, h Host, args ...string) ([]byte, error) {
	if c.Mode == units.Rootless {
		args = append([]string{"--user"}, args...)
	}
	return h.Run(ctx, "systemctl", args...)
}

// ---- prerequisites: the one-time root steps (call L33's table) ----

// prereqStep checks the one-time root steps: each that is missing
// either refuses the install or turns something off, and names the
// command that does it.
type prereqStep struct {
	c Config
	h Host
}

type prereqObserved struct {
	Root    bool            `json:"root"`
	Holder  []nsd.Check     `json:"holder"`
	Runtime rootless.Report `json:"runtime"`
	// RunSilo is /run/silo's state for the rootless front door:
	// "missing", "ok", or what is wrong with it.
	RunSilo string `json:"run_silo"`
	// The kernel's: its release, cgroup v2's unified hierarchy, systemd
	// as the init system, and nftables' inet and bridge families in a
	// scratch network namespace ("" when the probe passed).
	Kernel  string `json:"kernel"`
	Cgroup2 bool   `json:"cgroup2"`
	Systemd bool   `json:"systemd"`
	NFT     string `json:"nft"`
	// FSType is the data directory's filesystem's magic number.
	FSType int64 `json:"fs_type"`
}

func (s *prereqStep) Name() string                  { return "prerequisites" }
func (s *prereqStep) NewObserved() install.Observed { return &prereqObserved{} }

func (s *prereqStep) Observe(ctx context.Context) (install.Observed, error) {
	o := &prereqObserved{Root: s.h.EUID == 0}
	o.Holder, o.Runtime = s.h.Probe(s.c)
	o.RunSilo = "missing"
	if fi, err := s.h.Stat("/run/silo"); err == nil {
		o.RunSilo = "ok"
		if !fi.IsDir() || fi.Mode().Perm() != 0o755 {
			o.RunSilo = fmt.Sprintf("is %v, want a directory of mode 0755", fi.Mode())
		}
	}
	if b, err := s.h.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		o.Kernel = strings.TrimSpace(string(b))
	}
	_, err := s.h.Stat("/sys/fs/cgroup/cgroup.controllers")
	o.Cgroup2 = err == nil
	_, err = s.h.Stat("/run/systemd/system")
	o.Systemd = err == nil
	// The tables go with the namespace: the probe changes nothing.
	if out, err := s.h.Run(ctx, "unshare", "--user", "--map-root-user", "--net", "--",
		"env", "PATH="+units.SystemPath, "nft", "add table inet silo_probe; add table bridge silo_probe"); err != nil {
		o.NFT = strings.TrimSpace(err.Error() + " " + string(out))
	}
	if fs, err := s.h.Statfs(existing(s.h, s.c.DataDir)); err == nil {
		o.FSType = fs.Type
	}
	return o, nil
}

// kernelAtLeast reports whether release (as uname -r) is major.minor
// or later.
func kernelAtLeast(release string, major, minor int) bool {
	var ma, mi int
	if _, err := fmt.Sscanf(release, "%d.%d", &ma, &mi); err != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

// Filesystem magic numbers (statfs(2)) that the reflink check names.
const (
	btrfsMagic = 0x9123683e
	xfsMagic   = 0x58465342
)

// kernelChecks are the kernel rows of local-backend.md §12's table.
func (s *prereqStep) kernelChecks(o *prereqObserved) []install.Change {
	refuse := func(object, detail, why, fix string) install.Change {
		return install.Change{Action: install.ActionFail, Object: object, Detail: detail, Why: why, Fix: fix}
	}
	var out []install.Change
	if kernelAtLeast(o.Kernel, 5, 15) {
		out = append(out, install.Change{Action: install.ActionOK, Object: "Linux", Detail: o.Kernel})
	} else {
		out = append(out, refuse("Linux", "kernel "+o.Kernel, "the holder's namespaces and the fence's nftables need Linux 5.15 or later", "upgrade the kernel"))
	}
	if o.Cgroup2 {
		out = append(out, install.Change{Action: install.ActionOK, Object: "cgroup v2", Detail: "unified hierarchy"})
	} else {
		out = append(out, refuse("cgroup v2", "no unified hierarchy at /sys/fs/cgroup", "containers' limits and delegation",
			"boot with systemd.unified_cgroup_hierarchy=1"))
	}
	if o.Systemd {
		out = append(out, install.Change{Action: install.ActionOK, Object: "systemd", Detail: "the init system"})
	} else {
		out = append(out, refuse("systemd", "not the init system", "the units, and cgroup delegation", "run on a host booted with systemd"))
	}
	if o.NFT == "" {
		out = append(out, install.Change{Action: install.ActionOK, Object: "nftables",
			Detail: "inet and bridge tables load in a scratch network namespace"})
	} else {
		out = append(out, refuse("nftables", o.NFT, "the network fence: silo-net's inet and bridge tables",
			"install nftables, and load nf_tables (with its inet and bridge families)"))
	}
	switch o.FSType {
	case btrfsMagic:
		out = append(out, install.Change{Action: install.ActionOK, Object: "reflinks", Detail: "btrfs"})
	case xfsMagic:
		out = append(out, install.Change{Action: install.ActionOK, Object: "reflinks", Detail: "xfs (made with reflink=1, the default since xfsprogs 5.1)"})
	default:
		out = append(out, install.Change{Action: install.ActionWarn, Object: "reflinks",
			Detail: fmt.Sprintf("%s's filesystem (0x%x) has none", s.c.DataDir, o.FSType),
			Why:    "cheap copies of images and volumes; without them, copies are full copies"})
	}
	return out
}

// classOff is what each runtime check's failure turns off; a check
// not named here refuses the install.
var classOff = map[string]string{
	"runsc":                          "sandbox",
	"containerd-shim-runsc-v1":       "sandbox",
	"crun":                           "vm",
	"/dev/kvm through the kvm group": "vm",
}

func (s *prereqStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*prereqObserved)
	var out []install.Change
	switch {
	case s.c.Mode == units.Rootless && o.Root:
		return []install.Change{{Action: install.ActionFail, Object: "installer",
			Detail: "running as root", Why: "rootless units run as the user who installs them, and silod never runs as root",
			Fix: "run silo install as the user silod should run as, or use --system for the system install under a silo account"}}
	case s.c.Mode == units.System && !o.Root:
		return []install.Change{{Action: install.ActionFail, Object: "installer",
			Detail: "not root", Why: "the system install creates the silo account and writes system units",
			Fix: "sudo silo install --backend local --system"}}
	}
	out = append(out, s.kernelChecks(o)...)
	for _, c := range o.Holder {
		// The system install makes the account's ranges itself.
		if s.c.Mode == units.System && strings.HasPrefix(c.Name, "range in ") {
			continue
		}
		out = append(out, check(c, install.ActionFail, "the namespace holder, and every container's user namespace"))
	}
	for _, c := range o.Runtime.Checks {
		why, act := "", install.ActionFail
		switch class, ok := classOff[c.Name]; {
		case ok && class == "sandbox" && fetched(s.h):
			// The runtimes step fetches the pinned gVisor, which the
			// units put first on PATH.
			c = nsd.Check{Name: c.Name, OK: true, Detail: "gVisor " + Gvisor.Version + ", fetched by the runtimes step"}
		case ok:
			why, act = "the "+class+" class; without it, "+class+" is off", install.ActionWarn
			if s.c.Mode == units.System && c.Name == "/dev/kvm through the kvm group" && !strings.HasPrefix(c.Detail, "no /dev/kvm") {
				// The account step puts silo in the kvm group.
				continue
			}
		case c.Name == "buildkitd":
			why, act = "the vm class's image wraps; silo-buildkitd.service is not written without it", install.ActionWarn
		case c.Name == "cgroup delegation" && s.c.Mode == units.System:
			out = append(out, install.Change{Action: install.ActionOK, Object: c.Name,
				Detail: "silod.service has Delegate=yes"})
			continue
		case c.Name == "cgroup delegation":
			why, act = "CPU, memory and I/O limits; without io and cpuset delegated, I/O is unlimited and unmetered", install.ActionWarn
		case c.Name == "containerd":
			why = "both classes"
		}
		out = append(out, check(c, act, why))
	}
	classes := install.Change{Action: install.ActionOK, Object: "classes"}
	on := classesOn(s.c, s.h, o.Runtime)
	if len(on) == 0 {
		classes.Action, classes.Detail = install.ActionFail, "neither the sandbox nor the vm class can run here"
		classes.Why, classes.Fix = "silod needs at least one class", "install runsc and its shim, or crun on a host with /dev/kvm"
	} else {
		classes.Detail = strings.Join(on, " and ")
	}
	out = append(out, classes)

	frontDoor := install.Change{Action: install.ActionOK, Object: "/run/silo", Detail: "front door reachable by every local user"}
	switch {
	case s.c.Mode == units.System:
		frontDoor.Detail = "made by silod.service (RuntimeDirectory=silo)"
	case o.RunSilo == "missing":
		frontDoor.Action = install.ActionWarn
		frontDoor.Detail = "missing: the front door is in $XDG_RUNTIME_DIR/silo, which only " + s.c.User + " can reach"
		frontDoor.Why = "front-door sockets other local users can reach"
		frontDoor.Fix = fmt.Sprintf("echo 'd /run/silo 0755 %s %s -' | sudo tee /etc/tmpfiles.d/silo.conf && sudo systemd-tmpfiles --create", s.c.User, s.c.User)
	case o.RunSilo != "ok":
		frontDoor.Action, frontDoor.Detail = install.ActionWarn, "/run/silo "+o.RunSilo
	}
	out = append(out, frontDoor)
	out = append(out, install.Change{Action: install.ActionOK, Object: "storage driver",
		Detail: "dir: plain directories (no ZFS driver yet, so no `zfs allow` is needed)"})
	return out
}

// fetched reports whether the runtimes step fetches a pinned gVisor
// for this host.
func fetched(h Host) bool {
	_, ok := pinned(h)
	return ok
}

// classesOn are the classes the host offers, weakest first: sandbox
// unless runsc or its shim is missing (and not fetched), vm unless crun
// or /dev/kvm is.
func classesOn(c Config, h Host, report rootless.Report) []string {
	off := map[string]bool{}
	for _, ch := range report.Checks {
		class, ok := classOff[ch.Name]
		switch {
		case !ok || ch.OK:
		case class == "sandbox" && fetched(h):
		case c.Mode == units.System && ch.Name == "/dev/kvm through the kvm group" && !strings.HasPrefix(ch.Detail, "no /dev/kvm"):
			// The account step puts silo in the kvm group.
		default:
			off[class] = true
		}
	}
	var on []string
	for _, cl := range []string{"sandbox", "vm"} {
		if !off[cl] {
			on = append(on, cl)
		}
	}
	return on
}

// check is c as a plan line: ok, or act with why and c's fix.
func check(c nsd.Check, act install.Action, why string) install.Change {
	ch := install.Change{Action: install.ActionOK, Object: c.Name, Detail: c.Detail}
	if !c.OK {
		ch.Action, ch.Why, ch.Fix = act, why, c.Fix
	} else if c.Fix != "" {
		// A check that passed with something missing (a partial
		// cgroup delegation) is a warning.
		ch.Action, ch.Why, ch.Fix = install.ActionWarn, why, c.Fix
	}
	return ch
}

func (s *prereqStep) Apply(context.Context, []install.Change) error { return nil }

// ---- lingering (rootless) ----

// lingerStep turns on lingering for silod's user, so that its manager,
// and silod with it, starts at boot and stays after logout. Some
// polkit policies let a user turn it on for itself; where they don't,
// install says so and carries on, as the table says: silod then runs
// only while the user is logged in.
type lingerStep struct {
	c Config
	h Host
}

type lingerObserved struct {
	Lingering bool `json:"lingering"`
}

func (s *lingerStep) Name() string                  { return "lingering" }
func (s *lingerStep) NewObserved() install.Observed { return &lingerObserved{} }

func (s *lingerStep) Observe(ctx context.Context) (install.Observed, error) {
	_, err := s.h.Stat(filepath.Join("/var/lib/systemd/linger", s.c.User))
	return &lingerObserved{Lingering: err == nil}, nil
}

func (s *lingerStep) Plan(obs install.Observed) []install.Change {
	if obs.(*lingerObserved).Lingering {
		return []install.Change{{Action: install.ActionOK, Object: s.c.User, Detail: "lingering: silod starts at boot"}}
	}
	return []install.Change{{Action: install.ActionEnable, Object: s.c.User,
		Detail: "loginctl enable-linger " + s.c.User + ": start silod at boot, and keep it after logout"}}
}

func (s *lingerStep) PlanRemove(obs install.Observed) []install.Change {
	if !obs.(*lingerObserved).Lingering {
		return []install.Change{{Action: install.ActionOK, Object: s.c.User, Detail: "not lingering"}}
	}
	return []install.Change{{Action: install.ActionKeep, Object: s.c.User,
		Detail: "lingering, which other user units may need; turn it off with: loginctl disable-linger " + s.c.User}}
}

func (s *lingerStep) Apply(ctx context.Context, changes []install.Change) error {
	for _, ch := range changes {
		if ch.Action != install.ActionEnable {
			continue
		}
		if _, err := s.h.Run(ctx, "loginctl", "enable-linger", s.c.User); err != nil {
			fmt.Fprintf(s.h.Progress, "warning: lingering could not be turned on (%v).\n"+
				"  silod will run only while %s is logged in. Fix: sudo loginctl enable-linger %s\n", err, s.c.User, s.c.User)
		}
	}
	return nil
}

// ---- the silo account (system install) ----

// accountStep makes the system install's account: a system user in
// the kvm group, with subordinate uid and gid ranges for SubIDRanges
// container ranges past every range already given out, and the data
// directory, owned by it, mode 0700.
type accountStep struct {
	c Config
	h Host
}

type accountObserved struct {
	Exists   bool   `json:"exists"`
	KVM      bool   `json:"kvm"`       // the kvm group exists
	InKVM    bool   `json:"in_kvm"`    // the account is in it
	SubUID   bool   `json:"subuid"`    // the account has a range in /etc/subuid
	SubGID   bool   `json:"subgid"`    // and /etc/subgid
	NextFree uint64 `json:"next_free"` // the first id past every range
	DataDir  bool   `json:"data_dir"`  // the data directory exists
}

func (s *accountStep) Name() string                  { return "account" }
func (s *accountStep) NewObserved() install.Observed { return &accountObserved{} }

func (s *accountStep) Observe(ctx context.Context) (install.Observed, error) {
	o := &accountObserved{KVM: s.h.GroupExists("kvm"), NextFree: 1 << 20}
	u, err := s.h.Lookup(s.c.User)
	if err == nil {
		o.Exists = true
		o.InKVM = o.KVM && s.h.InGroup(u, "kvm")
	}
	for _, f := range []struct {
		file string
		has  *bool
	}{{"/etc/subuid", &o.SubUID}, {"/etc/subgid", &o.SubGID}} {
		b, err := s.h.ReadFile(f.file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Split(strings.TrimSpace(line), ":")
			if len(p) != 3 {
				continue
			}
			start, err1 := strconv.ParseUint(p[1], 10, 32)
			count, err2 := strconv.ParseUint(p[2], 10, 32)
			if err1 != nil || err2 != nil {
				continue
			}
			if p[0] == s.c.User || (u != nil && p[0] == u.Uid) {
				*f.has = count >= 65536 && (u == nil || !contains(start, count, u.Uid))
			}
			o.NextFree = max(o.NextFree, start+count)
		}
	}
	_, err = s.h.Stat(s.c.DataDir)
	o.DataDir = err == nil
	return o, nil
}

func contains(start, count uint64, id string) bool {
	n, err := strconv.ParseUint(id, 10, 32)
	return err == nil && n >= start && n-start < count
}

// subIDs is the range the account is given: SubIDRanges container
// ranges from the first 65536-aligned id past every other range.
func subIDs(next uint64) string {
	start := (next + 65535) / 65536 * 65536
	return fmt.Sprintf("%d-%d", start, start+SubIDRanges*65536-1)
}

func (s *accountStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*accountObserved)
	var out []install.Change
	if o.Exists {
		out = append(out, install.Change{Action: install.ActionOK, Object: "user " + s.c.User})
	} else {
		out = append(out, install.Change{Action: install.ActionCreate, Object: "user " + s.c.User,
			Detail: "a system user, its own group, no login shell, home " + s.c.DataDir})
	}
	switch {
	case !o.KVM:
		out = append(out, install.Change{Action: install.ActionWarn, Object: "group kvm",
			Detail: "no kvm group on this host", Why: "the vm class's /dev/kvm; without it, vm is off"})
	case o.InKVM:
		out = append(out, install.Change{Action: install.ActionOK, Object: "group kvm", Detail: s.c.User + " is a member"})
	default:
		out = append(out, install.Change{Action: install.ActionUpdate, Object: "group kvm", Detail: "add " + s.c.User})
	}
	if o.SubUID && o.SubGID {
		out = append(out, install.Change{Action: install.ActionOK, Object: "subordinate ids", Detail: s.c.User + " has its ranges"})
	} else {
		out = append(out, install.Change{Action: install.ActionCreate, Object: "subordinate ids",
			Detail: fmt.Sprintf("uids and gids %s (%d container ranges)", subIDs(o.NextFree), SubIDRanges)})
	}
	if o.DataDir {
		out = append(out, install.Change{Action: install.ActionOK, Object: s.c.DataDir})
	} else {
		out = append(out, install.Change{Action: install.ActionCreate, Object: s.c.DataDir, Detail: "owned by " + s.c.User + ", mode 0700"})
	}
	return out
}

func (s *accountStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*accountObserved)
	if !o.Exists {
		return []install.Change{{Action: install.ActionOK, Object: "user " + s.c.User, Detail: "not there"}}
	}
	return []install.Change{{Action: install.ActionKeep, Object: "user " + s.c.User,
		Detail: "with its subordinate ids, which the data directory's files are owned by; remove it with: userdel " + s.c.User}}
}

func (s *accountStep) Apply(ctx context.Context, changes []install.Change) error {
	obs, err := s.Observe(ctx)
	if err != nil {
		return err
	}
	o := obs.(*accountObserved)
	run := func(name string, args ...string) error {
		_, err := s.h.Run(ctx, name, args...)
		if err == nil {
			fmt.Fprintf(s.h.Progress, "  %s %s\n", name, strings.Join(args, " "))
		}
		return err
	}
	for _, ch := range changes {
		var err error
		switch {
		case ch.Object == "user "+s.c.User && ch.Action == install.ActionCreate:
			err = run("useradd", "--system", "--user-group", "--home-dir", s.c.DataDir, "--no-create-home",
				"--shell", "/usr/sbin/nologin", "--comment", "Silo (silod)", s.c.User)
		case ch.Object == "group kvm" && ch.Action == install.ActionUpdate:
			err = run("usermod", "--append", "--groups", "kvm", s.c.User)
		case ch.Object == "subordinate ids" && ch.Action == install.ActionCreate:
			r := subIDs(o.NextFree)
			err = run("usermod", "--add-subuids", r, "--add-subgids", r, s.c.User)
		case ch.Object == s.c.DataDir && ch.Action == install.ActionCreate:
			err = run("install", "-d", "-o", s.c.User, "-g", s.c.User, "-m", "0700", s.c.DataDir)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- the units ----

// unitsStep writes the units, reloads the manager, enables them, and
// starts them in order: restarting those whose file changed, which
// silo-nsd survives by adoption and containerd by its shims.
type unitsStep struct {
	c Config
	h Host
}

type unitsObserved struct {
	Dir     string            `json:"dir"`
	Want    []units.Unit      `json:"want"`
	Have    map[string]string `json:"have"`
	Enabled map[string]bool   `json:"enabled"`
	Active  map[string]bool   `json:"active"`
}

func (s *unitsStep) Name() string                  { return "units" }
func (s *unitsStep) NewObserved() install.Observed { return &unitsObserved{} }

func (s *unitsStep) Observe(ctx context.Context) (install.Observed, error) {
	var uc units.Config
	var want []units.Unit
	if s.c.Uninstall {
		// Uninstall removes whichever of them there are.
		uc = units.Config{Mode: s.c.Mode}
		for _, n := range []string{units.NSD, units.Containerd, units.Buildkitd, units.Silod} {
			want = append(want, units.Unit{Name: n})
		}
	} else {
		_, report := s.h.Probe(s.c)
		uc = unitsConfig(s.c, s.h, report)
		var err error
		if want, err = units.Units(uc); err != nil {
			return nil, err
		}
	}
	var err error
	o := &unitsObserved{Dir: s.h.UnitDir, Want: want, Have: map[string]string{}, Enabled: map[string]bool{}, Active: map[string]bool{}}
	if o.Dir == "" {
		if o.Dir, err = uc.Dir(); err != nil {
			return nil, err
		}
	}
	for _, u := range want {
		if b, err := s.h.ReadFile(filepath.Join(o.Dir, u.Name)); err == nil {
			o.Have[u.Name] = string(b)
		}
		// is-enabled and is-active exit non-zero for "no"; their
		// output is the answer.
		out, _ := systemctl(ctx, s.c, s.h, "is-enabled", u.Name)
		o.Enabled[u.Name] = strings.TrimSpace(string(out)) == "enabled"
		out, _ = systemctl(ctx, s.c, s.h, "is-active", u.Name)
		o.Active[u.Name] = strings.TrimSpace(string(out)) == "active"
	}
	return o, nil
}

func (s *unitsStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*unitsObserved)
	var out []install.Change
	for _, u := range o.Want {
		path := filepath.Join(o.Dir, u.Name)
		have, ok := o.Have[u.Name]
		switch {
		case !ok:
			out = append(out, install.Change{Action: install.ActionCreate, Object: u.Name, Detail: path})
		case have != u.Content:
			out = append(out, install.Change{Action: install.ActionUpdate, Object: u.Name, Detail: path + "; restarted"})
		case !o.Enabled[u.Name] || !o.Active[u.Name]:
			out = append(out, install.Change{Action: install.ActionEnable, Object: u.Name, Detail: "enable and start"})
		default:
			out = append(out, install.Change{Action: install.ActionOK, Object: u.Name, Detail: "enabled and running"})
		}
	}
	return out
}

// containersObject is the plan line for stopping every container.
const containersObject = "containers"

func (s *unitsStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*unitsObserved)
	var out []install.Change
	if o.Active[units.Containerd] || o.Active[units.Buildkitd] {
		out = append(out, install.Change{Action: install.ActionDelete, Object: containersObject,
			Detail: "every container stopped, with the runtimes' shims and sandboxes; silo-net goes with the holder's namespaces"})
	}
	// Silod first: the order they are stopped in.
	for i := len(o.Want) - 1; i >= 0; i-- {
		n := o.Want[i].Name
		_, have := o.Have[n]
		if !have && !o.Enabled[n] && !o.Active[n] {
			continue
		}
		out = append(out, install.Change{Action: install.ActionDelete, Object: n, Detail: "stopped, disabled and removed: " + filepath.Join(o.Dir, n)})
	}
	if len(out) == 0 {
		return []install.Change{{Action: install.ActionOK, Object: "units", Detail: "none installed"}}
	}
	return out
}

// remove stops every unit, silod first, killing the runtimes' whole
// cgroups so that the shims (KillMode=process) and every container go
// too. With --delete-data, rootless, it removes the files container
// ranges own while the holder, whose user namespace maps those ranges,
// still runs. Then it disables and removes the units.
func (s *unitsStep) remove(ctx context.Context, o *unitsObserved, del map[string]bool) error {
	stop := func(n string) {
		if o.Active[n] {
			systemctl(ctx, s.c, s.h, "stop", n)
			fmt.Fprintf(s.h.Progress, "  stopped %s\n", n)
		}
	}
	stop(units.Silod)
	for _, n := range []string{units.Buildkitd, units.Containerd} {
		if o.Active[n] {
			if _, err := systemctl(ctx, s.c, s.h, "kill", "--signal=SIGKILL", n); err != nil {
				return err
			}
		}
		stop(n)
	}
	// Sandbox containers run in silod's scope, not the runtimes'
	// cgroups (rootless.SandboxScope). There is none if no container
	// ran, so its errors are not this step's.
	systemctl(ctx, s.c, s.h, "kill", "--signal=SIGKILL", rootless.SandboxScope)
	systemctl(ctx, s.c, s.h, "stop", rootless.SandboxScope)
	if s.c.DeleteData && s.c.Mode == units.Rootless && o.Active[units.NSD] {
		args := []string{"unshare", "--", "rm", "-rf"}
		for _, d := range []string{"silos", "containerd", "cache"} {
			args = append(args, filepath.Join(s.c.DataDir, d))
		}
		if _, err := s.h.Run(ctx, s.h.Self, args...); err != nil {
			return fmt.Errorf("removing the containers' files inside the holder: %w", err)
		}
		fmt.Fprintf(s.h.Progress, "  removed the containers' files, inside the holder\n")
	}
	stop(units.NSD)
	var names []string
	for _, u := range o.Want {
		if del[u.Name] {
			names = append(names, u.Name)
		}
	}
	if len(names) > 0 {
		if _, err := systemctl(ctx, s.c, s.h, append([]string{"disable"}, names...)...); err != nil {
			return err
		}
	}
	for _, n := range names {
		if _, have := o.Have[n]; have {
			if err := s.h.RemoveAll(filepath.Join(o.Dir, n)); err != nil {
				return err
			}
			fmt.Fprintf(s.h.Progress, "  removed %s\n", filepath.Join(o.Dir, n))
		}
	}
	_, err := systemctl(ctx, s.c, s.h, "daemon-reload")
	return err
}

func (s *unitsStep) Apply(ctx context.Context, changes []install.Change) error {
	obs, err := s.Observe(ctx)
	if err != nil {
		return err
	}
	o := obs.(*unitsObserved)
	act := map[string]install.Action{}
	del := map[string]bool{}
	for _, ch := range changes {
		act[ch.Object] = ch.Action
		del[ch.Object] = ch.Action == install.ActionDelete
	}
	for _, d := range del {
		if d {
			return s.remove(ctx, o, del)
		}
	}
	for _, u := range o.Want {
		if a := act[u.Name]; a == install.ActionCreate || a == install.ActionUpdate {
			if err := s.h.WriteFile(filepath.Join(o.Dir, u.Name), []byte(u.Content)); err != nil {
				return err
			}
			fmt.Fprintf(s.h.Progress, "  wrote %s\n", filepath.Join(o.Dir, u.Name))
		}
	}
	if _, err := systemctl(ctx, s.c, s.h, "daemon-reload"); err != nil {
		return err
	}
	var names []string
	for _, u := range o.Want {
		names = append(names, u.Name)
	}
	if _, err := systemctl(ctx, s.c, s.h, append([]string{"enable"}, names...)...); err != nil {
		return err
	}
	for _, u := range o.Want {
		verb := "start"
		if act[u.Name] == install.ActionUpdate && o.Active[u.Name] {
			verb = "restart"
		}
		if _, err := systemctl(ctx, s.c, s.h, verb, u.Name); err != nil {
			return err
		}
		fmt.Fprintf(s.h.Progress, "  %s %s\n", verb, u.Name)
	}
	return nil
}
