package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// KindRegistryCacheEvict is silo registry cache evict, audited under
// the root.
const KindRegistryCacheEvict = "registry.cache.evict"

// ImageRegistry is what the image repo deletes and the cache's evict
// need of the registry service (local-registry.md §8 "Eviction and the
// cache budget" and §10); *registry.Service is it.
type ImageRegistry interface {
	// Reckon brings repo's disk_gb charge, in tx, to what it holds
	// once its rows have changed: what its manifests reference, and
	// the uploads the registry holds for it. It returns the change.
	Reckon(ctx context.Context, tx store.Tx, chain []*tree.Silo, repo *imagerepos.Repo) (int64, error)
	// Removed is told, once the transaction that removed them has
	// committed, of repo's manifests that are gone: it removes their
	// silo-registry records and has the content store collect, so
	// that what no other root references is gone from disk.
	Removed(ctx context.Context, repo string, digests []string)
	// EvictCache drops the cache entries of upstream's repo, at digest,
	// now; an empty repo or digest is every one (silo registry cache
	// evict).
	EvictCache(ctx context.Context, upstream, repo, digest string) (*CacheEviction, error)
}

// CacheEviction is what an eviction did: the entries it dropped and the
// cache-only bytes they freed, and the cache-only bytes left against
// the budget.
type CacheEviction struct {
	Entries     int   `json:"entries"`
	FreedBytes  int64 `json:"freed_bytes"`
	CacheBytes  int64 `json:"cache_bytes"`
	BudgetBytes int64 `json:"budget_bytes"`
}

// imageDiskClass is the class a repo's bytes are charged to.
var imageDiskClass = ledger.Key{Kind: "local", Class: "disk_gb", Region: ledger.Global}

// reckonImageRepo brings r's charge down after its manifests changed,
// to ms. Without a registry, it refunds to what ms charge alone.
func (s *Server) reckonImageRepo(ctx context.Context, tx store.Tx, chain []*tree.Silo, r *imagerepos.Repo, ms []*imagerepos.Manifest) error {
	if s.opts.Registry != nil {
		_, err := s.opts.Registry.Reckon(ctx, tx, chain, r)
		return err
	}
	n := imagerepos.Charged(ms)
	want := n/(1<<30)*ledger.Micro + (n%(1<<30)*ledger.Micro+(1<<30)-1)/(1<<30)
	object := imageRepoChargeObject(r.ID)
	cs, err := ledger.Charges(ctx, tx, object)
	if err != nil {
		return err
	}
	var have int64
	for _, c := range cs {
		if c.Key == imageDiskClass {
			have += c.Amount
		}
	}
	if want >= have {
		return nil
	}
	_, err = ledger.Refund(ctx, tx, object, []ledger.Amount{{Key: imageDiskClass, Amount: have - want}})
	return err
}

// removedImages tells the registry of repo's manifests that are gone.
func (s *Server) removedImages(ctx context.Context, repo string, digests []string) {
	if s.opts.Registry != nil && repo != "" {
		s.opts.Registry.Removed(context.WithoutCancel(ctx), repo, digests)
	}
}

// cacheEvictPath is POST registry/cache:evict, silo registry cache
// evict.
func cacheEvictPath(rest string) bool { return rest == "registry/cache:evict" }

// CacheEvictRequest names what to evict: <upstream>/<repo>[@digest],
// or <upstream>, or nothing for every entry.
type CacheEvictRequest struct {
	Ref string `json:"ref,omitempty"`
}

// registryCacheEvict serves POST registry/cache:evict, for root members
// with full control, as setting the cache is.
func (s *Server) registryCacheEvict(q *request) {
	ctx := q.r.Context()
	if q.r.Method != http.MethodPost {
		writeError(q.w, errorf(NotFound, "registry/cache:evict is POST"))
		return
	}
	var body CacheEvictRequest
	if err := json.NewDecoder(http.MaxBytesReader(q.w, q.r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(q.w, errorf(InvalidArgument, "the body: %v", err))
		return
	}
	upstream, repo, digest, err := ParseCacheRef(body.Ref)
	if err != nil {
		writeError(q.w, err)
		return
	}
	var root *tree.Silo
	err = s.tx(ctx, func(tx store.Tx) error {
		var err error
		if root, err = tree.Root(ctx, tx); err != nil {
			return err
		}
		if q.caller.Silo != root.ID || q.caller.Manage != "" {
			return errorf(PermissionDenied, "only the root silo's callers may evict from the registry cache")
		}
		eff, err := effectiveOf(ctx, tx, []*tree.Silo{root}, nil)
		if err != nil {
			return err
		}
		if eff.Control != ControlFull {
			return errorf(PermissionDenied, "evicting from the registry cache needs full control; the root's control policy is %s", eff.Control)
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	if s.opts.Registry == nil {
		writeError(q.w, errorf(FailedPrecondition, "this silod serves no registry cache"))
		return
	}
	out, err := s.opts.Registry.EvictCache(ctx, upstream, repo, digest)
	if err != nil {
		writeError(q.w, err)
		return
	}
	if err := s.tx(ctx, func(tx store.Tx) error {
		return Audit(ctx, tx, root, q.caller, KindRegistryCacheEvict, "", "OK", map[string]any{"ref": body.Ref,
			"entries": out.Entries, "freed_bytes": out.FreedBytes})
	}); err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// ParseCacheRef splits silo registry cache evict's argument,
// <upstream>[/<repo>[@digest]], into its parts; "" is every entry.
func ParseCacheRef(ref string) (upstream, repo, digest string, err error) {
	if ref == "" {
		return "", "", "", nil
	}
	rest, digest, _ := strings.Cut(ref, "@")
	upstream, repo, _ = strings.Cut(rest, "/")
	if !strings.Contains(upstream, ".") || digest != "" && (repo == "" || !imagerepos.ValidDigest(digest)) {
		return "", "", "", errorf(InvalidArgument, "%q: want <upstream>[/<repo>[@<digest>]], such as docker.io/library/alpine", ref).with("field", "ref")
	}
	return upstream, repo, digest, nil
}
