package fenceapi

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Mapping back (local-git.md, §9): where a repo is in the uber repo,
// for silo repo external and external-clone, and which repo an
// uber-repo ref is, for silo repo which.

// RepoExternal is GET .../repos/{repo}/external: a repo's refs in the
// uber repo. With mirroring off or no mirror set, State says so and
// there are no external refs.
type RepoExternal struct {
	Repo   string `json:"repo"` // its silo's path and its name, as /team-a/x/foo
	ID     string `json:"id"`
	Silo   string `json:"silo"`
	SiloID string `json:"silo_id"`
	State  string `json:"state"`
	// From is the silo whose repos policy turned mirroring off.
	From      string `json:"from,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// MirroredTime is when silod last moved one of the repo's refs in
	// the uber repo.
	MirroredTime *time.Time `json:"mirrored_time,omitempty"`
	// URL is the uber repo's, as silo mirror set was given it.
	URL string `json:"url,omitempty"`
	// Branches, Tags and Archives are the ref prefixes the repo's
	// branches, tags and archives are under; ArchiveRefs counts the
	// archives silod has pushed.
	Branches    string `json:"branches,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Archives    string `json:"archives,omitempty"`
	ArchiveRefs int    `json:"archive_refs"`
	// DefaultBranch is the uber repo's branch for the repo's default
	// branch, as git clone -b takes it.
	DefaultBranch string `json:"default_branch,omitempty"`
	// Index is the repo's gitlink in the submodule index, as
	// <index branch>:<path>, once the index has one.
	Index string `json:"index,omitempty"`
	// Ref is the one ref asked for with branch= or tag=.
	Ref *ExternalRef `json:"ref,omitempty"`
}

// ExternalRef is one of a repo's refs in both views.
type ExternalRef struct {
	Local    string `json:"local"`
	External string `json:"external"`
	// Commit is what silod last pushed to it, if it has.
	Commit string `json:"commit,omitempty"`
}

// RefWhich is GET /silo/v1/mirror:which: the repo an uber-repo ref
// belongs to.
type RefWhich struct {
	Ref string `json:"ref"` // in full: refs/heads/silo/team-a/x/foo.git/feature/a
	// Kind is branch, tag, archive or index.
	Kind string `json:"kind"`
	Repo string `json:"repo"` // /team-a/x/foo
	Silo string `json:"silo"` // /team-a/x
	// Local is the repo's own ref: refs/heads/feature/a, or
	// refs/silo-archive/<ts>-<seq>/heads/main. An index entry's is
	// the repo's default branch, if the repo is there.
	Local string `json:"local,omitempty"`
	// Exists is whether the repo is there now. SiloID and ID are the
	// silo's and the repo's; for a repo that is gone, SiloID is what
	// silod remembers of it, or what the index last said, if anything.
	Exists bool   `json:"exists"`
	ID     string `json:"id,omitempty"`
	SiloID string `json:"silo_id,omitempty"`
	// From says where SiloID came from when the repo is gone: mirror
	// (silod's own record) or index (the index's README).
	From string `json:"from,omitempty"`
	// Commit is what silod last pushed to the ref, if it has.
	Commit string `json:"commit,omitempty"`
}

// The kinds of RefWhich.
const (
	RefBranch  = "branch"
	RefTag     = "tag"
	RefArchive = "archive"
	RefIndex   = "index"
)

// Externals is the part of the mirror that maps between the views;
// *mirror.Mirror is one. A Mirror that isn't one leaves every repo
// UNCONFIGURED here.
type Externals interface {
	// External fills in out, whose Repo, ID, Silo and SiloID are set,
	// for repo id; local, if not "", is the one ref to map.
	External(ctx context.Context, id, local string, out *RepoExternal) error
	// Which parses ref as an uber-repo ref, and looks up its repo.
	Which(ctx context.Context, ref string) (*RefWhich, error)
}

// externalRepo is GET .../repos/{repo}/external, for whoever may read
// the repo.
func (s *Server) externalRepo(q *request, ref, name string) {
	ctx := q.r.Context()
	query := q.r.URL.Query()
	branch, tag := query.Get("branch"), query.Get("tag")
	local := ""
	switch {
	case branch != "" && tag != "":
		writeError(q.w, errorf(InvalidArgument, "branch and tag: name one ref").with("field", "tag"))
		return
	case branch != "":
		if !repos.ValidBranch(branch) {
			writeError(q.w, errorf(InvalidArgument, "branch %q is not a branch name git takes", branch).with("field", "branch"))
			return
		}
		local = "refs/heads/" + branch
	case tag != "":
		if !repos.ValidBranch(tag) {
			writeError(q.w, errorf(InvalidArgument, "tag %q is not a tag name git takes", tag).with("field", "tag"))
			return
		}
		local = "refs/tags/" + tag
	}
	var out *RepoExternal
	on := false
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, err := s.repoSilo(ctx, tx, &q.caller, ref, false)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		r, err := repos.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, repos.ErrNotFound) {
			return repoNotFound(t, name)
		}
		if err != nil {
			return err
		}
		eff, err := effectiveOf(ctx, tx, chain, nil)
		if err != nil {
			return err
		}
		out = &RepoExternal{Repo: path.Join(t.Path, r.Name), ID: r.ID, Silo: t.Path, SiloID: t.ID, State: MirrorUnconfigured}
		if !eff.Repos.Mirror {
			out.State, out.From = MirrorOff, eff.ReposFrom
		}
		on = eff.Repos.Mirror
		return nil
	})
	if err == nil && on {
		if x, ok := s.opts.Mirror.(Externals); ok {
			err = x.External(ctx, out.ID, local, out)
		}
	}
	if err != nil {
		writeError(q.w, asError(ctx, nil, err))
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// which is GET mirror:which?ref=: the repo an uber-repo ref belongs
// to, for a caller who may read that repo. A repo that is gone is
// shown to a caller who may read the repos of the deepest silo on its
// path that is still there, since it could have read the repo too;
// to anyone else, as to anyone for a repo they may not read, the ref
// is NOT_FOUND.
func (s *Server) which(q *request) {
	ctx := q.r.Context()
	if q.r.Method != http.MethodGet {
		writeError(q.w, errorf(NotFound, "mirror:which is GET"))
		return
	}
	ref := q.r.URL.Query().Get("ref")
	if ref == "" {
		writeError(q.w, errorf(InvalidArgument, "ref is required: an uber-repo ref, as refs/heads/silo/team-a/x/foo.git/main").with("field", "ref"))
		return
	}
	x, ok := s.opts.Mirror.(Externals)
	if !ok {
		writeError(q.w, errorf(FailedPrecondition, "no mirror is set, so no uber-repo ref is a silo's"))
		return
	}
	w, err := x.Which(ctx, ref)
	if err == nil {
		err = s.tx(ctx, func(tx store.Tx) error {
			p := w.Silo
			for {
				_, err := tree.ByPath(ctx, tx, p)
				if err == nil {
					break
				}
				if !errors.Is(err, tree.ErrNotFound) {
					return err
				}
				if p == "/" {
					break
				}
				p = path.Dir(p)
			}
			cc := q.caller
			if _, err := s.repoSilo(ctx, tx, &cc, p, false); err != nil {
				var e *Error
				if errors.As(err, &e) && (e.Code == NotFound || e.Code == PermissionDenied) {
					return errorf(NotFound, "%s is no repo's ref that you may read", strings.TrimSpace(ref)).with("ref", ref)
				}
				return err
			}
			return nil
		})
	}
	if err != nil {
		writeError(q.w, asError(ctx, nil, err))
		return
	}
	writeJSON(q.w, http.StatusOK, w)
}
