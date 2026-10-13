package fenceapi

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/bwsalmon/silo/pkg/exports"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
)

// Registry exports (local-registry.md, "Registry exports", call L64):
// an export of protocol "registry" names image repos of the exporter's
// own silo, and an accepted, ACTIVE import of it lets the importer pull
// them, and never push or delete. Offering, accepting, withdrawing and
// suspending are every export's (exports.go); what is left is what the
// repos list may hold and the registry's questions.

// repoPrefixRE is a repo name prefix before its "*": the start of a
// name (imagerepos.ValidName), which may end in a separator.
var repoPrefixRE = regexp.MustCompile(`^([a-z0-9]+((_|__|-+)[a-z0-9]+)*(_|__|-+)?)?$`)

// checkRegistryRepos checks a registry export's repos: at least one,
// each an image repo name or a prefix ending in "*" ("*" alone is every
// repo the exporter has, or will have). It returns them sorted, each
// once.
func checkRegistryRepos(repos []string) ([]string, error) {
	if len(repos) == 0 {
		return nil, errorf(InvalidArgument, "a registry export names repos: names, or prefixes ending in *").with("field", "repos")
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if p, ok := strings.CutSuffix(r, "*"); ok {
			if len(p) >= 63 || !repoPrefixRE.MatchString(p) || strings.HasPrefix(p, imagerepos.IDPrefix) {
				return nil, errorf(InvalidArgument, "repos %q: want the start of an image repo name before the *", r).with("field", "repos")
			}
		} else if !imagerepos.ValidName(r) {
			return nil, errorf(InvalidArgument, "repos %q: want an image repo name, or a prefix ending in *", r).with("field", "repos")
		}
		out = append(out, r)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ImportOfImageRepo is silo's import that lets it read image repo r:
// an ACTIVE import of a registry export, from r's own silo, that covers
// r. It is nil when there is none: no offer, an offer not accepted, a
// suspended or withdrawn grant, an export that doesn't cover r, or r
// being deleted. A descendant's repo is never covered by an ancestor's
// export, since r's silo must be the exporter. The registry (step 27 of
// local-registry.md §13) asks this for a caller the access table gives
// nothing else, and pulls only: an import never writes.
func ImportOfImageRepo(ctx context.Context, tx store.Tx, silo string, r *imagerepos.Repo) (*exports.Import, error) {
	if r.State == imagerepos.StateDeleting || silo == r.Silo {
		return nil, nil
	}
	ims, err := exports.ImportsBy(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	for _, im := range ims {
		if im.State != exports.StateActive {
			continue
		}
		e, err := exports.Get(ctx, tx, im.Export)
		if errors.Is(err, exports.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if e.Exporter == r.Silo && e.Covers(r.Name) {
			return im, nil
		}
	}
	return nil, nil
}

// MayReadImported reports whether silo may read image repo r through
// an accepted import (ImportOfImageRepo).
func MayReadImported(ctx context.Context, tx store.Tx, silo string, r *imagerepos.Repo) (bool, error) {
	im, err := ImportOfImageRepo(ctx, tx, silo, r)
	return im != nil, err
}

// ImportedImageRepos is every image repo silo may read through its
// ACTIVE registry imports, by silo and name, each once: what the
// registry's GET /v2/_catalog lists beside the caller's own repos and
// its descendants' (local-registry.md, "Listing").
func ImportedImageRepos(ctx context.Context, tx store.Tx, silo string) ([]*imagerepos.Repo, error) {
	ims, err := exports.ImportsBy(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	var out []*imagerepos.Repo
	seen := map[string]bool{}
	for _, im := range ims {
		if im.State != exports.StateActive {
			continue
		}
		e, err := exports.Get(ctx, tx, im.Export)
		if errors.Is(err, exports.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if e.Protocol != exports.Registry {
			continue
		}
		rs, err := imagerepos.Of(ctx, tx, e.Exporter)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.State != imagerepos.StateDeleting && e.Covers(r.Name) && !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	slices.SortFunc(out, func(a, b *imagerepos.Repo) int {
		return strings.Compare(a.Silo+"/"+a.Name, b.Silo+"/"+b.Name)
	})
	return out, nil
}
