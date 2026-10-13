package conformance

import (
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// A Target is one backend the backend-neutral cases run against
// (local-backend.md §13, call L24): "gcp", "local/sandbox", "local/vm"
// and "local/fake". pkg/conformance/targets has them.
//
// A Target offers what the registry cases need (local-registry.md
// §12) and, through Fence, what the neutral fence cases of
// pkg/conformance/fence, the Docker facade's of pkg/conformance/docker
// and the local-only structural ones of pkg/conformance/local need: an
// install's fence API, and probes made in its silos.
type Target interface {
	// Name is the target's name, as cases report it.
	Name() string
	// Registry starts a fresh install's registry for t, with the test
	// upstream of NewUpstream behind its pull-through cache, and stops
	// it when t ends. It returns ErrNotOffered when the backend has no
	// registry, and a *NotRunnable when it has one that can't be run
	// here.
	Registry(t testing.TB) (Registry, error)
	// Fence starts a fresh install for t, with an address space, and
	// stops it when t ends. It returns a *NotRunnable when the backend
	// can't be installed here, or its probes can't be made.
	Fence(t testing.TB) (Fence, error)
}

// Fence is an install a Target offers for the fence cases: its fence
// API and its facade, called as a silo, and probes in its silos
// (local-backend.md §13: the GCP target makes them as e2-micro VMs, a
// local one as containers through the Docker facade, in its class).
type Fence interface {
	// Root is the root silo's id.
	Root() string
	// API makes a fence API call as caller (the root's members when
	// caller is the root, nobody when ""), and decodes its JSON answer.
	API(caller, method, path string, body any) (int, map[string]any)
	// Docker sends a Docker Engine API request as caller, to its silo's
	// facade. It returns ErrNotOffered if the target has none.
	Docker(caller string, req *http.Request) (*http.Response, error)
	// Probe makes a probe in silo, on a network of the silo's own, and
	// deletes it when t ends. A probe answers ICMP echo, and TCP and
	// UDP on every port, so another probe's check of it is decided by
	// the fence alone.
	Probe(t testing.TB, silo, name string) Probe
}

// A Probe is one probe a Fence made.
type Probe interface {
	// Addr is the probe's address on its silo's network.
	Addr() netip.Addr
	// Run runs checks from the probe and returns its report by check
	// ID. A target runs the kinds "tcp", "udp" and "icmp" at least, and
	// reports any other it can't run as a failed check whose detail
	// starts "unsupported", which cases treat as a skip.
	Run(checks []Check) map[string]ProbeResult
	// Spoof runs checks from the probe with src as their source
	// address, as a tenant with CAP_NET_RAW in its own netns can send
	// them. It returns ErrNotOffered where the target's probes can't.
	Spoof(src netip.Addr, checks []Check) (map[string]ProbeResult, error)
}

// Unsupported is the detail prefix of a check a target can't run.
const Unsupported = "unsupported"

// Unpinned is what a local target offers beyond Fence for the
// structural cases: a probe's packets sent through a port that has no
// pin entry, as a veth the fence wasn't told of would send them.
type Unpinned interface {
	FromUnpinnedPort(p Probe, checks []Check) map[string]ProbeResult
}

// Git is what a local target's Fence offers beyond it for the repo and
// mirror cases (local-git.md §11 item 24): the git service on port 80,
// as a probe reaches it, an uber repo for the mirror, and a restore of
// silod.
type Git interface {
	// GitURL is http://silo.internal/git/ as p reaches it: a base URL
	// git clients on this host can use, whose connections the gateway
	// sees as coming from p.
	GitURL(t testing.TB, p Probe) string
	// Uber starts an uber repo for the install's mirror to push to, and
	// stops it when t ends.
	Uber(t testing.TB) Uber
	// Backup backs up silod's store and data directory, and returns what
	// restores them: silod started again from the backup, in report-only
	// mode, as it starts after a restore. Calls made after the restore
	// reach the restored silod.
	Backup(t testing.TB) (restore func())
}

// Uber is an uber repo a Git target's mirror pushes to.
type Uber interface {
	// URL and Credential are what PUT /silo/v1/mirror is given.
	URL() string
	Credential() string
	// Refs is every ref in the uber repo, with its commit.
	Refs() map[string]string
	// SetDown makes the uber repo refuse every request, or serve again.
	SetDown(down bool)
}

// FenceOf is target's install for t, or t skipped, as RegistryOf.
func FenceOf(t *testing.T, target Target) Fence {
	t.Helper()
	f, err := target.Fence(t)
	var nr *NotRunnable
	switch {
	case errors.Is(err, ErrNotOffered):
		t.Skipf("not offered: %s has no probes", target.Name())
	case errors.As(err, &nr):
		t.Skipf("%s: %v", target.Name(), err)
	case err != nil:
		t.Fatalf("%s: installing: %v", target.Name(), err)
	}
	return f
}

// ForEachFence runs fn as a subtest per target, with a fresh install
// of it.
func ForEachFence(t *testing.T, targets []Target, fn func(t *testing.T, f Fence)) {
	t.Helper()
	for _, tg := range targets {
		t.Run(tg.Name(), func(t *testing.T) {
			fn(t, FenceOf(t, tg))
		})
	}
}

// ErrNotOffered is a Target's answer for a capability its backend
// doesn't have. Cases report it as "not offered", which is a skip and
// never a pass.
var ErrNotOffered = errors.New("not offered")

// NotRunnable is a Target's answer for a capability its backend has
// but that can't be run in this environment, or not yet.
type NotRunnable struct{ Why string }

func (e *NotRunnable) Error() string { return "not runnable here: " + e.Why }

// Registry is the registry a Target offers, its fence API and its
// facade, each called as a silo, the way a probe in that silo calls
// them (local-registry.md §5, "Who is calling").
type Registry interface {
	// Root is the root silo's id.
	Root() string
	// API makes a fence API call as caller (the root's members when
	// caller is the root, nobody when ""), and decodes its JSON answer.
	API(caller, method, path string, body any) (int, map[string]any)
	// Do sends a registry request, req.URL's path and query being the
	// registry's /v2/..., as caller.
	Do(caller string, req *http.Request) (*http.Response, error)
	// Listen serves the registry as silo on a loopback address, for
	// clients run as processes (the distribution-spec suite), and
	// returns its host:port.
	Listen(silo string) string
	// Docker sends a Docker Engine API request as caller, to its silo's
	// facade. It returns ErrNotOffered if the target has none.
	Docker(caller string, req *http.Request) (*http.Response, error)
	// Upstream is the test upstream the cache's docker.io is.
	Upstream() *Upstream
	// Audit is every audit record of kind, oldest first, its fields
	// with _silo, _actor and _result added.
	Audit(kind string) []map[string]any
}

// Structural is what a local target offers beyond Registry for the
// local-only structural cases (pkg/conformance/local): a look at
// silo-containerd's content store. Targets that aren't local don't
// implement it.
type Structural interface {
	// HasBlob reports whether the content store holds digest.
	HasBlob(digest string) bool
	// CacheBytes is the bytes the cache holds that no record outside
	// it references: what its budget counts.
	CacheBytes() int64
	// Advance moves the registry's clock on by d, past the grace hour an
	// upload's blobs are kept unreferenced for.
	Advance(d time.Duration)
}

// RegistryOf is target's registry for t, or t skipped: "not offered"
// when the backend has none, which is never a pass, and "not runnable
// here" when it can't be run in this environment.
func RegistryOf(t *testing.T, target Target) Registry {
	t.Helper()
	r, err := target.Registry(t)
	var nr *NotRunnable
	switch {
	case errors.Is(err, ErrNotOffered):
		t.Skipf("not offered: %s has no registry", target.Name())
	case errors.As(err, &nr):
		t.Skipf("%s: %v", target.Name(), err)
	case err != nil:
		t.Fatalf("%s: starting the registry: %v", target.Name(), err)
	}
	return r
}

// ForEach runs fn as a subtest per target, with the target's registry.
func ForEach(t *testing.T, targets []Target, fn func(t *testing.T, r Registry)) {
	t.Helper()
	for _, tg := range targets {
		t.Run(tg.Name(), func(t *testing.T) {
			fn(t, RegistryOf(t, tg))
		})
	}
}
