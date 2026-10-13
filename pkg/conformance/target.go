package conformance

import (
	"errors"
	"net/http"
	"testing"
)

// A Target is one backend the backend-neutral cases run against
// (local-backend.md §13, call L24): "gcp", "local/sandbox", "local/vm"
// and "local/fake". pkg/conformance/targets has them.
//
// Today a Target offers only what the registry cases need
// (local-registry.md §12): the fence cases of this package's GCP
// harness, behind the silo_conformance tag, are moved behind it by
// local-backend.md's item 12.
type Target interface {
	// Name is the target's name, as cases report it.
	Name() string
	// Registry starts a fresh install's registry for t, with the test
	// upstream of NewUpstream behind its pull-through cache, and stops
	// it when t ends. It returns ErrNotOffered when the backend has no
	// registry, and a *NotRunnable when it has one that can't be run
	// here.
	Registry(t testing.TB) (Registry, error)
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
