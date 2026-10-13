package registry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The service is the fence API's registry, for its deletes and evict.
var _ fenceapi.ImageRegistry = (*Service)(nil)

// The audit kinds of the registry's reconcile (local-registry.md, §10
// "Garbage collection", and §4 "Restore").
const (
	// KindRegistryDrift is a difference between the store's manifest
	// rows and the silo-registry records, and what was done about it.
	KindRegistryDrift = "registry_drift"
	// KindManifestMissing is a manifest row whose content is gone; it
	// is answered with 404 MANIFEST_UNKNOWN and never re-created.
	KindManifestMissing = "image_manifest_missing"
	// KindRegistryOrphan is a silo-registry record newer than the
	// store: made by a push after the point a restored store was copied
	// at. It is reported, never served and never deleted.
	KindRegistryOrphan = "registry.orphan"
)

// OrphanLabel marks a silo-registry record the reconcile found newer
// than the store, so that it is never taken for a leftover and removed,
// however far the object sequence has gone on since.
const OrphanLabel = "dev.silo.orphan"

// cacheEntry is a cache entry's record name in silo-cache: the whole
// host's <upstream>/<repo>@<digest>, and an isolated domain's the same
// under domain-<id>/, so that each domain's entry keeps its blob alive
// for as long as that entry is there (call L73).
func cacheEntry(domain, upstream, repo, digest string) string {
	name := upstream + "/" + repo + "@" + digest
	if domain != "" {
		name = "domain-" + domain + "/" + name
	}
	return name
}

// Reckon brings repo's disk_gb charge to what it holds now, in tx: the
// fence API's manifest delete calls it, so that a delete refunds at
// once, as the registry's own does.
func (s *Service) Reckon(ctx context.Context, tx store.Tx, chain []*tree.Silo, repo *imagerepos.Repo) (int64, error) {
	t := &fenceapi.RegistryTarget{Chain: chain, Name: repo.Name, Repo: repo,
		Caller: fenceapi.Caller{Silo: repo.Silo, Via: fenceapi.ViaInternal}}
	return s.charge(ctx, tx, t)
}

// Removed removes the silo-registry records of repo's manifests that
// are gone from the store, and collects: containerd's GC then removes
// what no other repo, image record, cache entry or lease reaches
// (local-registry.md, §10). A record it fails to remove is the
// reconcile's to find.
func (s *Service) Removed(ctx context.Context, repo string, digests []string) {
	for _, d := range digests {
		if err := s.opts.Content.DeleteImage(ctx, Namespace, repo+"@"+d); err != nil {
			log.Printf("registry: removing %s@%s from %s: %v", repo, d, Namespace, err)
		}
	}
	if len(digests) > 0 {
		s.collect(ctx)
	}
}

func (s *Service) collect(ctx context.Context) {
	if err := s.opts.Content.Collect(ctx); err != nil {
		log.Printf("registry: collecting the content store: %v", err)
	}
}

// cacheRow is one cache entry, as eviction sees it.
type cacheRow struct {
	domain                 string // "" for the whole host's index
	upstream, repo, digest string
	size                   int64
	accessed               int64
}

// cacheUse is the cache's entries, least recently used first, and
// which of their digests are cache-only: referenced by no record
// outside silo-cache, so by no silo's image, no repo and no container.
type cacheUse struct {
	rows []cacheRow
	// only is each cache-only digest's size; entries counts the rows of
	// each digest.
	only    map[string]int64
	entries map[string]int
	bytes   int64
}

func (s *Service) cacheUse(ctx context.Context) (*cacheUse, error) {
	u := &cacheUse{only: map[string]int64{}, entries: map[string]int{}}
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		u.rows = nil
		rows, err := tx.Query(ctx, `SELECT '' AS domain, upstream, repo, digest, size, accessed_at FROM registry_cache_objects
			UNION ALL SELECT domain, upstream, repo, digest, size, accessed_at FROM registry_cache_domain_objects
			ORDER BY accessed_at, upstream, repo, digest, domain`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r cacheRow
			if err := rows.Scan(&r.domain, &r.upstream, &r.repo, &r.digest, &r.size, &r.accessed); err != nil {
				return err
			}
			u.rows = append(u.rows, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	nss, err := s.opts.Content.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	elsewhere := map[string]bool{}
	for _, ns := range nss {
		if ns == CacheNamespace {
			continue
		}
		imgs, err := s.opts.Content.ListImages(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, img := range imgs {
			elsewhere[img.Digest] = true
			for _, r := range img.Refs {
				elsewhere[r] = true
			}
		}
	}
	for _, r := range u.rows {
		u.entries[r.digest]++
		if !elsewhere[r.digest] {
			if _, ok := u.only[r.digest]; !ok {
				u.bytes += r.size
			}
			u.only[r.digest] = r.size
		}
	}
	return u, nil
}

// drop1 removes one cache entry, its row and its silo-cache record, and
// returns the cache-only bytes that removing it freed: its size, if it
// was the last entry of a cache-only digest.
func (s *Service) drop1(ctx context.Context, u *cacheUse, r cacheRow) (int64, error) {
	x := indexOf(r.domain)
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM `+x.objects+` WHERE `+x.where+`upstream = ? AND repo = ? AND digest = ?`,
			x.args(r.upstream, r.repo, r.digest)...)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := s.opts.Content.DeleteImage(ctx, CacheNamespace, cacheEntry(r.domain, r.upstream, r.repo, r.digest)); err != nil {
		return 0, err
	}
	u.entries[r.digest]--
	size, only := u.only[r.digest]
	if !only || u.entries[r.digest] > 0 {
		return 0, nil
	}
	delete(u.only, r.digest)
	u.bytes -= size
	return size, nil
}

// budget is the cache budget, in bytes: the configuration's
// size_limit_gb, which install sets from --image-cache-size.
func (s *Service) budget(ctx context.Context) (int64, error) {
	var cfg fenceapi.CacheSettings
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		cfg, err = fenceapi.ReadCacheSettings(ctx, tx)
		return err
	})
	return int64(cfg.SizeLimitGB * (1 << 30)), err
}

// Evict keeps the cache within its budget (local-registry.md, "Eviction
// and the cache budget", call L74): when the cache-only bytes pass the
// budget, it drops cache-only entries, least recently used first, until
// they are down to 90% of it. An entry whose blob something else
// references isn't counted and isn't dropped, since dropping it would
// free nothing; and since only the content store's GC removes a blob,
// dropping an entry never removes one something else references.
func (s *Service) Evict(ctx context.Context) (*fenceapi.CacheEviction, error) {
	budget, err := s.budget(ctx)
	if err != nil {
		return nil, err
	}
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	u, err := s.cacheUse(ctx)
	if err != nil {
		return nil, err
	}
	out := &fenceapi.CacheEviction{BudgetBytes: budget}
	if u.bytes > budget {
		target := budget / 10 * 9
		for _, r := range u.rows {
			if u.bytes <= target {
				break
			}
			if _, ok := u.only[r.digest]; !ok {
				continue
			}
			n, err := s.drop1(ctx, u, r)
			if err != nil {
				return nil, err
			}
			out.Entries++
			out.FreedBytes += n
		}
		s.collect(ctx)
	}
	out.CacheBytes = u.bytes
	return out, nil
}

// EvictCache drops the entries of upstream's repo at digest now, an
// empty part matching every one (silo registry cache evict). Entries
// whose blobs something else references go too, but their blobs stay,
// as containerd's GC keeps what anything references.
func (s *Service) EvictCache(ctx context.Context, upstream, repo, digest string) (*fenceapi.CacheEviction, error) {
	budget, err := s.budget(ctx)
	if err != nil {
		return nil, err
	}
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	u, err := s.cacheUse(ctx)
	if err != nil {
		return nil, err
	}
	out := &fenceapi.CacheEviction{BudgetBytes: budget}
	for _, r := range u.rows {
		if upstream != "" && r.upstream != upstream || repo != "" && r.repo != repo || digest != "" && r.digest != digest {
			continue
		}
		n, err := s.drop1(ctx, u, r)
		if err != nil {
			return nil, err
		}
		out.Entries++
		out.FreedBytes += n
	}
	if out.Entries > 0 {
		s.collect(ctx)
	}
	out.CacheBytes = u.bytes
	return out, nil
}

// touch records that a cache entry was used now, for eviction's order,
// and, after a miss brought new bytes in, keeps the cache in budget.
func (s *Service) touch(ctx context.Context, cr *cacheReq, digest string) {
	ctx = context.WithoutCancel(ctx)
	x := indexOf(cr.domain)
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE `+x.objects+` SET accessed_at = ? WHERE `+x.where+`upstream = ? AND repo = ? AND digest = ?`,
			append([]any{store.Millis(s.opts.Now())}, x.args(cr.name.Upstream, cr.name.UpstreamRepo, digest)...)...)
		return err
	})
	if err != nil {
		log.Printf("registry: cache: recording a use of %s@%s: %v", cr.name, digest, err)
	}
	if cr.result != CacheMiss {
		return
	}
	if _, err := s.Evict(ctx); err != nil {
		log.Printf("registry: cache: evicting: %v", err)
	}
}

// DriftReport is what one Reconcile found and did.
type DriftReport struct {
	// Recreated are the records re-created for rows whose content
	// exists; Missing the rows whose manifest content is gone.
	Recreated, Missing []string
	// Removed are the records of manifests the store no longer has,
	// left by a delete that stopped before removing them.
	Removed []string
	// Orphans are the records newer than the store, by name.
	Orphans []string
}

// Reconcile compares the store's manifest rows with the silo-registry
// records (local-registry.md, §10 "Garbage collection"), every 10
// minutes from silod's reconcile:
//
//   - a row with no record has it re-created when the manifest's
//     content exists, as registry_drift, and is reported as
//     image_manifest_missing when it doesn't (§4, "Restore");
//   - a record with no row, numbered at or below the store's object
//     sequence, is a delete's leftover: it is removed, as
//     registry_drift, unless reportOnly;
//   - a record with no row numbered above the store's object sequence
//     is newer than the store, which was restored to before its push:
//     an orphan. It is reported as registry.orphan, labelled so that it
//     stays one, and never served or deleted, until an operator
//     decides.
//
// A repo being deleted is left to its delete. Then it collects.
func (s *Service) Reconcile(ctx context.Context, reportOnly bool) (*DriftReport, error) {
	type rowKey struct {
		repo *imagerepos.Repo
		m    *imagerepos.Manifest
	}
	rows := map[string]rowKey{}
	deleting := map[string]bool{}
	var hw uint64
	var root *tree.Silo
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		clear(rows)
		clear(deleting)
		var err error
		if hw, err = objseq.HighWater(ctx, tx); err != nil {
			return err
		}
		if root, err = tree.Root(ctx, tx); err != nil {
			return err
		}
		repos, err := imagerepos.All(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range repos {
			if r.State == imagerepos.StateDeleting {
				deleting[r.ID] = true
				continue
			}
			ms, err := imagerepos.Manifests(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			for _, m := range ms {
				rows[r.ID+"@"+m.Digest] = rowKey{r, m}
			}
		}
		return nil
	})
	if errors.Is(err, tree.ErrNotFound) {
		return &DriftReport{}, nil // nothing before bootstrap
	}
	if err != nil {
		return nil, err
	}
	imgs, err := s.opts.Content.ListImages(ctx, Namespace)
	if err != nil {
		return nil, err
	}
	have := map[string]Image{}
	for _, img := range imgs {
		have[img.Name] = img
	}
	rep := &DriftReport{}
	type finding struct {
		silo   string
		kind   string
		fields map[string]any
	}
	var found []finding
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := have[name]; ok {
			continue
		}
		rk := rows[name]
		fields := map[string]any{"image_repo": rk.repo.Name, "id": rk.repo.ID, "digest": rk.m.Digest}
		if _, err := s.opts.Content.Size(ctx, rk.m.Digest); errors.Is(err, ErrUnknown) {
			rep.Missing = append(rep.Missing, name)
			found = append(found, finding{rk.repo.Silo, KindManifestMissing, fields})
			continue
		} else if err != nil {
			return nil, err
		}
		fields["what"], fields["action"] = "record_missing", "recreated"
		if reportOnly {
			fields["action"] = "reported"
		} else {
			img := Image{Name: name, Digest: rk.m.Digest, MediaType: rk.m.MediaType, Size: rk.m.Size,
				Labels: map[string]string{SeqLabel: strconv.FormatUint(hw, 10)}}
			for _, r := range rk.m.Refs {
				img.Refs = append(img.Refs, r.Digest)
			}
			if err := s.opts.Content.PutImage(ctx, Namespace, img); err != nil {
				return nil, fmt.Errorf("re-creating %s: %w", name, err)
			}
			rep.Recreated = append(rep.Recreated, name)
		}
		found = append(found, finding{rk.repo.Silo, KindRegistryDrift, fields})
	}
	removed := false
	for _, img := range imgs {
		if _, ok := rows[img.Name]; ok {
			continue
		}
		repo, digest, _ := cutAt(img.Name)
		if deleting[repo] {
			continue
		}
		seq := objseq.Parse(img.Labels[SeqLabel])
		fields := map[string]any{"record": img.Name, "id": repo, "digest": digest, "seq": seq, "store_seq": hw}
		if img.Labels[OrphanLabel] != "" || seq > hw {
			rep.Orphans = append(rep.Orphans, img.Name)
			if img.Labels[OrphanLabel] == "" {
				// Reported once, when found; the label keeps it an
				// orphan from then on.
				labels := map[string]string{OrphanLabel: s.opts.Now().UTC().Format("2006-01-02T15:04:05Z")}
				for k, v := range img.Labels {
					labels[k] = v
				}
				img.Labels = labels
				if err := s.opts.Content.PutImage(ctx, Namespace, img); err != nil {
					return nil, fmt.Errorf("labelling orphan %s: %w", img.Name, err)
				}
				found = append(found, finding{root.ID, KindRegistryOrphan, fields})
			}
			continue
		}
		fields["what"], fields["action"] = "record_stale", "removed"
		if reportOnly {
			fields["action"] = "reported"
		} else {
			if err := s.opts.Content.DeleteImage(ctx, Namespace, img.Name); err != nil {
				return nil, fmt.Errorf("removing %s: %w", img.Name, err)
			}
			rep.Removed = append(rep.Removed, img.Name)
			removed = true
		}
		found = append(found, finding{root.ID, KindRegistryDrift, fields})
	}
	if len(found) > 0 {
		err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
			for _, f := range found {
				t, err := tree.Get(ctx, tx, f.silo)
				if errors.Is(err, tree.ErrNotFound) {
					t, err = root, nil
				}
				if err != nil {
					return err
				}
				if err := fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal},
					f.kind, "", "OK", f.fields); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if removed {
		s.collect(ctx)
	}
	return rep, nil
}

// cutAt splits a record's name, <repo id>@<digest>.
func cutAt(name string) (string, string, bool) {
	for i := 0; i < len(name); i++ {
		if name[i] == '@' {
			return name[:i], name[i+1:], true
		}
	}
	return name, "", false
}
