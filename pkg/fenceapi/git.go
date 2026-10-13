package fenceapi

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of the git service (local-git.md, "How a request is
// served", step 7) and of restore-ref ("Local archive refs").
const (
	KindRepoPush       = "repo.push"
	KindRepoRestoreRef = "repo.restoreRef"
)

// GitTarget is a repo a git call may reach, and who reaches it.
type GitTarget struct {
	Silo *tree.Silo
	Repo *repos.Repo
	// Caller is the call's caller, with the manage grant it relies on,
	// if any.
	Caller Caller
	// Write is whether it may push.
	Write bool
}

// GitPath is the repo's path under /git/: its silo's path and its
// name, with .git.
func (t *GitTarget) GitPath() string { return gitPath(t.Silo.Path, t.Repo.Name) }

// RefUpdate is one ref a push changed: Old is "" for a ref it made,
// New "" for one it deleted.
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// GitConnector serves git's own protocol over a hijacked connection,
// for git-remote-silo through the front door (local-git.md, "Where it
// listens"): service is git-upload-pack or git-receive-pack.
type GitConnector interface {
	Connect(w http.ResponseWriter, r *http.Request, t *GitTarget, service string)
}

// GitAccess decides what caller c may do with the repo name of the
// silo ref names ("-<id>" or a path), as local-git.md §4 has it: the
// silo itself reads and pushes; an ancestor reads; a manage grant's
// holder does what the grant's repos access says; anyone else, and
// anyone at all for a repo that doesn't exist, gets NOT_FOUND. A
// caller that may read but not push, asking to push, gets
// PERMISSION_DENIED. Git traffic is a data path, so the control policy
// doesn't apply (call L44).
func (s *Server) GitAccess(ctx context.Context, c Caller, ref, name string, write bool) (*GitTarget, error) {
	var out *GitTarget
	err := s.tx(ctx, func(tx store.Tx) error {
		out = nil
		rc := c
		chain, rel, err := s.resolve(ctx, tx, &rc, ref, opReposRead)
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
		mayWrite := rel == relSelf
		if rel == relManage {
			wc := c
			if _, _, err := s.resolve(ctx, tx, &wc, ref, opReposWrite); err == nil {
				mayWrite, rc = true, wc
			}
		}
		if write && !mayWrite {
			why := "an ancestor may read its descendants' repos but not push to them"
			if rel == relManage {
				why = "the manage grant gives repos read access, not write"
			}
			return errorf(PermissionDenied, "%s/%s.git: %s", t.Path, name, why).with("silo", t.Path).with("repo", name)
		}
		out = &GitTarget{Silo: t, Repo: r, Caller: rc, Write: mayWrite}
		return nil
	})
	return out, err
}

// GitPushed records a push to t that changed refs: the repo is marked
// dirty for the mirror, its size is recorded, and repo.push is audited
// under the repo's silo with each ref's old and new commit and, from a
// tool VM, the agent it came from.
func (s *Server) GitPushed(ctx context.Context, t *GitTarget, requestID string, refs []RefUpdate, size int64) error {
	return s.tx(ctx, func(tx store.Tx) error {
		return s.repoChanged(ctx, tx, t.Silo, t.Repo, t.Caller, KindRepoPush, requestID, size, map[string]any{"refs": refs})
	})
}

// repoChanged marks r dirty, records its size if it is known (not
// negative), and audits kind.
func (s *Server) repoChanged(ctx context.Context, tx store.Tx, t *tree.Silo, r *repos.Repo, c Caller, kind, requestID string, size int64, fields map[string]any) error {
	if _, err := repos.Get(ctx, tx, r.ID); errors.Is(err, repos.ErrNotFound) {
		return nil // deleted meanwhile: nothing to mirror
	} else if err != nil {
		return err
	}
	seq, err := repos.MarkDirty(ctx, tx, r)
	if err != nil {
		return err
	}
	if size >= 0 {
		if err := repos.SetSize(ctx, tx, r.ID, size); err != nil {
			return err
		}
	}
	fields["repo"], fields["id"], fields["seq"] = r.Name, r.ID, seq
	if c.FromToolVM != "" && c.Agent == "" {
		fields["agent"] = c.FromToolVM
	}
	return Audit(ctx, tx, t, c, kind, requestID, "OK", fields)
}

// archiveSilo resolves the silo of an archives or restore-ref call,
// which only the repo's own silo and its ancestors may make
// (local-git.md, "Local archive refs").
func (s *Server) archiveSilo(ctx context.Context, tx store.Tx, c *Caller, ref string) (*tree.Silo, error) {
	cc := *c
	chain, rel, err := s.resolve(ctx, tx, &cc, ref, opReposRead)
	if err != nil {
		return nil, err
	}
	if rel != relSelf && rel != relAncestor {
		return nil, notFound(chain[len(chain)-1].Path)
	}
	return chain[len(chain)-1], nil
}

// listArchives is GET .../repos/{repo}/archives: the repo's local
// archive refs.
func (s *Server) listArchives(q *request, ref, name string) {
	ctx := q.r.Context()
	var t *tree.Silo
	var r *repos.Repo
	err := s.tx(ctx, func(tx store.Tx) error {
		var err error
		if t, err = s.archiveSilo(ctx, tx, &q.caller, ref); err != nil {
			return err
		}
		r, err = repos.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, repos.ErrNotFound) {
			return repoNotFound(t, name)
		}
		return err
	})
	if err == nil && s.opts.Repos == nil {
		err = errorf(FailedPrecondition, "this silod keeps no repos: it runs without a data directory")
	}
	if err != nil {
		writeError(q.w, err)
		return
	}
	as, err := s.opts.Repos.Archives(ctx, t.ID, r.ID)
	if err != nil {
		writeError(q.w, errorf(Internal, "listing %s's repo %s's archive refs: %v", t.Path, r.Name, err))
		return
	}
	writeJSON(q.w, http.StatusOK, map[string]any{"archives": as, "repo": path.Join(t.Path, r.Name)})
}

// restoreRequest is POST .../repos/{repo}:restoreRef.
type restoreRequest struct {
	Archive string `json:"archive"`
	Branch  string `json:"branch"`
}

// restoreRef is POST .../repos/{repo}:restoreRef: a new branch at an
// archive ref's commit, which marks the repo dirty like a push.
func (s *Server) restoreRef(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var req restoreRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	req.Archive = strings.TrimPrefix(req.Archive, "refs/silo-archive/")
	if _, _, ok := repos.ParseArchive(req.Archive); !ok {
		return nil, errorf(InvalidArgument, "archive %q: want an archive ref's name, as silo repo archives lists it", req.Archive).with("field", "archive")
	}
	if !repos.ValidBranch(req.Branch) {
		return nil, errorf(InvalidArgument, "branch %q is not a branch name git takes", req.Branch).with("field", "branch")
	}
	var out map[string]any
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.archiveSilo(ctx, tx, &q.caller, ref)
		if err != nil {
			return err
		}
		q.note(t, KindRepoRestoreRef)
		r, err := repos.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, repos.ErrNotFound) {
			return repoNotFound(t, name)
		}
		if err != nil {
			return err
		}
		if s.opts.Repos == nil {
			return errorf(FailedPrecondition, "this silod keeps no repos: it runs without a data directory")
		}
		out = map[string]any{"repo": path.Join(t.Path, r.Name), "archive": req.Archive, "branch": req.Branch}
		if q.validateOnly {
			out["validate_only"] = true
			return s.finish(ctx, tx, q, out)
		}
		sha, err := s.opts.Repos.RestoreRef(ctx, t.ID, r.ID, req.Archive, req.Branch)
		switch {
		case errors.Is(err, repos.ErrNoArchive):
			return errorf(NotFound, "%s's repo %s has no archive ref %s", t.Path, r.Name, req.Archive).with("archive", req.Archive)
		case errors.Is(err, repos.ErrRefExists):
			return errorf(AlreadyExists, "%s's repo %s already has a branch %s", t.Path, r.Name, req.Branch).with("branch", req.Branch)
		case err != nil:
			return errorf(Internal, "restoring %s: %v", req.Archive, err)
		}
		out["commit"] = sha
		if err := s.repoChanged(ctx, tx, t, r, q.caller, KindRepoRestoreRef, q.requestID, -1,
			map[string]any{"archive": req.Archive, "refs": []RefUpdate{{Ref: "refs/heads/" + req.Branch, New: sha}}}); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// gitConnect is GET .../repos/{repo}:git?service=..., with Upgrade:
// git: git's own protocol over the connection, for git-remote-silo at
// the front door. Access is GitAccess's, as on port 80.
func (s *Server) gitConnect(q *request, ref, name string) {
	service := q.r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeError(q.w, errorf(InvalidArgument, "service %q: want git-upload-pack or git-receive-pack", service).with("field", "service"))
		return
	}
	if !strings.EqualFold(q.r.Header.Get("Upgrade"), "git") {
		writeError(q.w, errorf(InvalidArgument, "a git connection needs Upgrade: git"))
		return
	}
	if s.opts.Git == nil {
		writeError(q.w, errorf(FailedPrecondition, "this silod serves no git: it runs without a data directory"))
		return
	}
	c := q.caller
	c.Manage = ""
	t, err := s.GitAccess(q.r.Context(), c, ref, name, service == "git-receive-pack")
	if err != nil {
		writeError(q.w, err)
		return
	}
	s.opts.Git.Connect(q.w, q.r, t, service)
}
