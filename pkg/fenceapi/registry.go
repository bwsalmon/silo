package fenceapi

import (
	"context"
	"errors"
	"sort"

	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The registry service's audit kinds (local-registry.md, "Audit", call
// L67), beside KindRegistryDelete.
const (
	KindRegistryPull   = "registry.pull"
	KindRegistryPush   = "registry.push"
	KindRegistryDenied = "registry.denied"
)

// RegistryTarget is a local image repo a registry request may act on,
// as RegistryAccess resolves it for its caller.
type RegistryTarget struct {
	// Chain is the repo's silo's ancestry, root first.
	Chain []*tree.Silo
	// Name is the repo's name in its silo.
	Name string
	// Repo is the repo, or nil when it doesn't exist yet and Write lets
	// the caller create it by pushing (call L58).
	Repo *imagerepos.Repo
	// Caller is the caller, with Manage set when it reaches the repo
	// through a manage grant, which audit records as via=manage.
	Caller Caller
	// Write is whether the caller may push to the repo.
	Write bool
	// Import is the import the caller reads the repo through, if that
	// is its only way in.
	Import string
}

// ChargeVia is the ledger's via for a charge the registry makes to the
// target's silo for the target's caller, as every object a holder
// makes is charged.
func (t *RegistryTarget) ChargeVia() string { return chargeVia(t.Chain, t.Caller) }

// Silo is the repo's silo.
func (t *RegistryTarget) Silo() *tree.Silo { return t.Chain[len(t.Chain)-1] }

// RegistryAccess is the registry's access table for a local repo
// (local-registry.md, "Who may do what", call L63), inside tx: the silo
// itself pulls and pushes; a proper ancestor pulls; a manage grant's
// holder with registry in its scope pulls at read and pushes at write;
// a silo holding an accepted import of a registry export that covers
// the repo pulls. Everyone else gets NOT_FOUND, the same whether the
// silo or the repo exists or not, and so does a write by a caller that
// may only read. A repo that doesn't exist is NOT_FOUND too, unless the
// caller may write to its silo and asks to: that is a first push,
// with Repo nil, which CreateImageRepo makes.
//
// The caller's Authorization means nothing here: the caller is its
// origin (call L66).
func (s *Server) RegistryAccess(ctx context.Context, tx store.Tx, c Caller, n imagerepos.Name, write bool) (*RegistryTarget, error) {
	if !n.Local() {
		return nil, errorf(NotFound, "%s is not a local repo", n)
	}
	ref := n.SiloPath
	if n.SiloID != "" {
		ref = "-" + n.SiloID
	}
	c.Manage = ""
	rc := c
	chain, rel, err := s.resolve(ctx, tx, &rc, ref, opRegistryRead)
	var e *Error
	if errors.As(err, &e) && e.Code == NotFound {
		return s.registryImport(ctx, tx, c, ref, n, write)
	}
	if err != nil {
		return nil, err
	}
	t := chain[len(chain)-1]
	mayWrite := rel == relSelf
	if rel == relManage {
		wc := c
		if _, _, err := s.resolve(ctx, tx, &wc, ref, opRegistryWrite); err == nil {
			mayWrite, rc = true, wc
		}
	}
	r, err := imagerepos.ByName(ctx, tx, t.ID, n.Repo)
	switch {
	case errors.Is(err, imagerepos.ErrNotFound):
		r = nil
	case err != nil:
		return nil, err
	case r.State == imagerepos.StateDeleting:
		return nil, registryNotFound(n)
	}
	if write && !mayWrite || r == nil && !(write && mayWrite) {
		return nil, registryNotFound(n)
	}
	return &RegistryTarget{Chain: chain, Name: n.Repo, Repo: r, Caller: rc, Write: mayWrite}, nil
}

// registryImport is RegistryAccess for a caller the tree gives nothing:
// read access through an accepted import, or NOT_FOUND.
func (s *Server) registryImport(ctx context.Context, tx store.Tx, c Caller, ref string, n imagerepos.Name, write bool) (*RegistryTarget, error) {
	if write {
		return nil, registryNotFound(n)
	}
	var t *tree.Silo
	var err error
	if n.SiloID != "" {
		t, err = tree.Get(ctx, tx, n.SiloID)
	} else {
		t, err = tree.ByPath(ctx, tx, ref)
	}
	if errors.Is(err, tree.ErrNotFound) {
		return nil, registryNotFound(n)
	}
	if err != nil {
		return nil, err
	}
	r, err := imagerepos.ByName(ctx, tx, t.ID, n.Repo)
	if errors.Is(err, imagerepos.ErrNotFound) {
		return nil, registryNotFound(n)
	}
	if err != nil {
		return nil, err
	}
	im, err := ImportOfImageRepo(ctx, tx, c.Silo, r)
	if err != nil {
		return nil, err
	}
	if im == nil {
		return nil, registryNotFound(n)
	}
	chain, err := tree.Ancestry(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	return &RegistryTarget{Chain: chain, Name: n.Repo, Repo: r, Caller: c, Import: im.ID}, nil
}

func registryNotFound(n imagerepos.Name) *Error {
	return errorf(NotFound, "repository name not known to registry").with("name", n.String())
}

// RegistryCatalog is what GET /v2/_catalog lists for c
// (local-registry.md, "Listing"): the image repos of its own silo and
// its descendants, and those of its accepted imports, each by its
// path name, sorted. Repos being deleted are left out.
func (s *Server) RegistryCatalog(ctx context.Context, tx store.Tx, c Caller) ([]string, error) {
	silos, err := tree.Subtree(ctx, tx, c.Silo)
	if errors.Is(err, tree.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rs, err := imagerepos.Of(ctx, tx, silos...)
	if err != nil {
		return nil, err
	}
	im, err := ImportedImageRepos(ctx, tx, c.Silo)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	seen := map[string]bool{}
	var out []string
	for _, r := range append(rs, im...) {
		if r.State == imagerepos.StateDeleting || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		p, ok := paths[r.Silo]
		if !ok {
			t, err := tree.Get(ctx, tx, r.Silo)
			if err != nil {
				return nil, err
			}
			p, paths[r.Silo] = t.Path, t.Path
		}
		out = append(out, imagerepos.Name{SiloPath: p, Repo: r.Name}.String())
	}
	sort.Strings(out)
	return out, nil
}
