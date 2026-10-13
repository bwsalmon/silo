package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of repos (local-git.md, §2 "Operations").
const (
	KindRepoCreate = "repo.create"
	KindRepoPatch  = "repo.patch"
	KindRepoDelete = "repo.delete"
)

// ReposClass is the count class a repo holds while it exists
// (local-git.md, "Quota", call L55). The root's capacity of it is
// unlimited unless bootstrap is given one.
var ReposClass = ledger.Key{Kind: "silo", Class: "repos", Region: ledger.Global}

// The Ops repo calls resolve their silo with, for the ManageChecker:
// a grant's repos resource type at read, or at write.
const (
	opReposRead  Op = "repos.read"
	opReposWrite Op = "repos.write"
)

// GitHost is where in-silo URLs point: the gateway's name, on port 80
// (local-git.md, "Where it listens", call L42).
const GitHost = "http://silo.internal/git/"

// MirrorPrefix is the uber repo's branch namespace for silo repos
// (local-git.md, "Ref layout", call L46).
const MirrorPrefix = "silo"

// The states of a repo's mirror before there is a mirror to run
// (local-git.md, §8): OFF where the effective repos policy keeps the
// repo off the uber repo, UNCONFIGURED where no uber repo is set.
const (
	MirrorOff          = "OFF"
	MirrorUnconfigured = "UNCONFIGURED"
)

// RepoHost keeps repos on disk; *repos.Host is the one silod uses.
type RepoHost interface {
	// Create makes silo's repo id, empty or seeded, and returns its
	// default branch and size.
	Create(ctx context.Context, silo, id, branch string, seq uint64, seed *repos.Source) (head string, size int64, err error)
	// SetHead moves a repo's HEAD.
	SetHead(ctx context.Context, silo, id, branch string) error
	// Delete removes a repo; one that is gone is not an error.
	Delete(ctx context.Context, silo, id string) error
	// Archives lists a repo's local archive refs.
	Archives(ctx context.Context, silo, id string) ([]repos.Archive, error)
	// RestoreRef makes a new branch at an archive ref's commit, and
	// returns the commit: repos.ErrNoArchive if there is no such
	// archive, repos.ErrRefExists if the branch exists.
	RestoreRef(ctx context.Context, silo, id, archive, branch string) (string, error)
}

// RepoObject is a repo, as the API shows it (local-git.md, "The
// object", call L41).
type RepoObject struct {
	Name          string     `json:"name"`
	ID            string     `json:"id"`
	Silo          string     `json:"silo"`
	DefaultBranch string     `json:"default_branch"`
	CloneURL      string     `json:"clone_url"`
	CloneURLByID  string     `json:"clone_url_by_id"`
	SizeBytes     int64      `json:"size_bytes"`
	CreateTime    time.Time  `json:"create_time"`
	Mirror        RepoMirror `json:"mirror"`
}

// RepoMirror is a repo's place in the uber repo, and where its mirror
// stands.
type RepoMirror struct {
	State string `json:"state"`
	// ExternalPrefix is where its branches go, unless mirroring is off.
	ExternalPrefix string `json:"external_prefix,omitempty"`
	// From is the silo whose repos policy turned mirroring off.
	From      string  `json:"from,omitempty"`
	LastError *string `json:"last_error"`
}

// gitPath is a repo's path under /git/ and in the uber repo's refs:
// its silo's path and its name, with .git (call L46).
func gitPath(siloPath, name string) string {
	return strings.TrimPrefix(path.Join(siloPath, name), "/") + ".git"
}

func repoObject(t *tree.Silo, eff *effective, r *repos.Repo) *RepoObject {
	o := &RepoObject{Name: r.Name, ID: r.ID, Silo: t.Path, DefaultBranch: r.DefaultBranch,
		CloneURL: GitHost + gitPath(t.Path, r.Name), CloneURLByID: GitHost + "-" + t.ID + "/" + r.Name + ".git",
		SizeBytes: r.SizeBytes, CreateTime: r.Created, Mirror: RepoMirror{State: MirrorUnconfigured}}
	if !eff.Repos.Mirror {
		o.Mirror = RepoMirror{State: MirrorOff, From: eff.ReposFrom}
	} else {
		o.Mirror.ExternalPrefix = "refs/heads/" + MirrorPrefix + "/" + gitPath(t.Path, r.Name) + "/"
	}
	return o
}

func repoChargeObject(id string) string { return "repo:" + id }

// repoSilo resolves the silo of a repo call: the silo itself and its
// ancestors may make any, and a manage grant's holder those its repos
// resource type's access covers.
func (s *Server) repoSilo(ctx context.Context, tx store.Tx, c *Caller, ref string, write bool) ([]*tree.Silo, error) {
	op := opReposRead
	if write {
		op = opReposWrite
	}
	chain, _, err := s.resolve(ctx, tx, c, ref, op)
	return chain, err
}

func repoNotFound(t *tree.Silo, name string) *Error {
	return errorf(NotFound, "%s has no repo %q", t.Path, name).with("silo", t.Path).with("repo", name)
}

// seedSource finds a create's seed, a repo the caller may read: a path
// to it, its silo's relative to the caller's own unless it starts with
// "/" (local-git.md, "Seeding"). A seed the caller may not read is
// NOT_FOUND, as one that doesn't exist.
func (s *Server) seedSource(ctx context.Context, tx store.Tx, c Caller, seed string) (*repos.Repo, error) {
	missing := errorf(NotFound, "no repo %s to seed from", seed).with("field", "seed").with("seed", seed)
	dir, name := path.Split(seed)
	if !repos.ValidName(name) {
		return nil, errorf(InvalidArgument, "seed %q: want a repo's path, such as /team-a/x/foo", seed).with("field", "seed")
	}
	ref := "self"
	switch {
	case strings.HasPrefix(dir, "/"):
		ref = dir
	case dir != "":
		me, err := tree.Get(ctx, tx, c.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil, missing
		} else if err != nil {
			return nil, err
		}
		ref = path.Join(me.Path, dir)
	}
	// The seed's grant, if any, is not the call's: the call is audited
	// as it reached its target.
	cc := c
	cc.Manage = ""
	chain, err := s.repoSilo(ctx, tx, &cc, ref, false)
	var e *Error
	if errors.As(err, &e) && e.Code == NotFound {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	r, err := repos.ByName(ctx, tx, chain[len(chain)-1].ID, name)
	if errors.Is(err, repos.ErrNotFound) {
		return nil, missing
	}
	return r, err
}

// repoRequest is POST .../repos.
type repoRequest struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Seed          *struct {
		Repo string `json:"repo"`
	} `json:"seed"`
}

// createRepo is POST .../repos: a bare repo in the silo's subtree,
// empty or seeded from a repo the caller may read, charged one of the
// silo's repos.
func (s *Server) createRepo(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req repoRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if !repos.ValidName(req.Name) {
		return nil, errorf(InvalidArgument, "name %q: want 1 to 63 of [a-z0-9._-], starting with a letter or digit, with no .. and not ending in .git or .lock", req.Name).
			with("field", "name")
	}
	if req.DefaultBranch != "" && !repos.ValidBranch(req.DefaultBranch) {
		return nil, errorf(InvalidArgument, "default_branch %q is not a branch name git takes", req.DefaultBranch).with("field", "default_branch")
	}
	if req.Seed != nil && req.Seed.Repo == "" {
		return nil, errorf(InvalidArgument, "seed.repo is required: the path of the repo to copy").with("field", "seed")
	}
	// made is the repo a transaction attempt put on disk, removed if
	// the attempt doesn't commit.
	var made *repos.Repo
	unmake := func() {
		if made != nil {
			_ = s.opts.Repos.Delete(context.WithoutCancel(ctx), made.Silo, made.ID)
			made = nil
		}
	}
	var out *RepoObject
	err := s.tx(ctx, func(tx store.Tx) error {
		unmake()
		chain, err := s.repoSilo(ctx, tx, &q.caller, ref, true)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindRepoCreate)
		if s.opts.Repos == nil {
			return errorf(FailedPrecondition, "this silod keeps no repos: it runs without a data directory")
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		if _, err := repos.ByName(ctx, tx, t.ID, req.Name); err == nil {
			return errorf(AlreadyExists, "%s already has a repo %s", t.Path, req.Name).with("silo", t.Path).with("repo", req.Name)
		} else if !errors.Is(err, repos.ErrNotFound) {
			return err
		}
		var seed *repos.Repo
		if req.Seed != nil {
			if seed, err = s.seedSource(ctx, tx, q.caller, req.Seed.Repo); err != nil {
				return err
			}
		}
		eff, err := effectiveOf(ctx, tx, chain, nil)
		if err != nil {
			return err
		}
		r := &repos.Repo{ID: repos.NewID(), Silo: t.ID, Name: req.Name, DefaultBranch: req.DefaultBranch}
		for {
			if _, err := repos.Get(ctx, tx, r.ID); errors.Is(err, repos.ErrNotFound) {
				break
			} else if err != nil {
				return err
			}
			r.ID = repos.NewID()
		}
		if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: repoChargeObject(r.ID), Silo: t.ID, Via: chargeVia(chain, q.caller),
			Amounts: []ledger.Amount{{Key: ReposClass, Amount: ledger.Units(1)}}}); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]string{"silo": t.ID, "name": r.Name, "what": "create"})
		if r.Seq, err = tx.Append(ctx, repos.ChangeKind, r.ID, string(data)); err != nil {
			return err
		}
		if q.validateOnly {
			if r.DefaultBranch == "" {
				r.DefaultBranch = repos.DefaultBranch
				if seed != nil {
					r.DefaultBranch = seed.DefaultBranch
				}
			}
			out = repoObject(t, eff, r)
			return s.finish(ctx, tx, q, out)
		}
		var src *repos.Source
		if seed != nil {
			src = &repos.Source{Silo: seed.Silo, ID: seed.ID}
		}
		head, size, err := s.opts.Repos.Create(ctx, t.ID, r.ID, r.DefaultBranch, r.Seq, src)
		if err != nil {
			return errorf(Internal, "making %s's repo %s: %v", t.Path, r.Name, err)
		}
		made = r
		r.DefaultBranch, r.SizeBytes = head, size
		if err := repos.Create(ctx, tx, r); err != nil {
			return err
		}
		fields := map[string]any{"repo": r.Name, "id": r.ID, "default_branch": r.DefaultBranch}
		if seed != nil {
			fields["seed"] = req.Seed.Repo
			fields["seed_id"] = seed.ID
		}
		if err := Audit(ctx, tx, t, q.caller, KindRepoCreate, q.requestID, "OK", fields); err != nil {
			return err
		}
		out = repoObject(t, eff, r)
		return s.finish(ctx, tx, q, out)
	})
	if err == nil {
		made = nil
	}
	unmake()
	return s.done(q, out, 0, err)
}

// listRepos is GET .../repos: the silo's repos, and with recursive=true
// its descendants' too, each with its silo's path.
func (s *Server) listRepos(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	recursive := q.r.URL.Query().Get("recursive") == "true"
	var items []any
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, err := s.repoSilo(ctx, tx, &q.caller, ref, false)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		silos := []string{t.ID}
		if recursive {
			if silos, err = tree.Subtree(ctx, tx, t.ID); err != nil {
				return err
			}
		}
		rs, err := repos.Of(ctx, tx, silos...)
		if err != nil {
			return err
		}
		type level struct {
			silo *tree.Silo
			eff  *effective
		}
		seen := map[string]level{}
		for _, r := range rs {
			l, ok := seen[r.Silo]
			if !ok {
				c, err := tree.Ancestry(ctx, tx, r.Silo)
				if err != nil {
					return err
				}
				eff, err := effectiveOf(ctx, tx, c, nil)
				if err != nil {
					return err
				}
				l = level{c[len(c)-1], eff}
				seen[r.Silo] = l
			}
			items = append(items, repoObject(l.silo, l.eff, r))
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := map[string]any{"repos": []any{}}
	if offset < len(items) {
		end := min(len(items), offset+size)
		out["repos"] = items[offset:end]
		if end < len(items) {
			out["next_page_token"] = nextToken(offset, size, true)
		}
	}
	writeJSON(q.w, http.StatusOK, out)
}

// getRepo is GET .../repos/{repo}.
func (s *Server) getRepo(q *request, ref, name string) {
	ctx := q.r.Context()
	var out *RepoObject
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
		out = repoObject(t, eff, r)
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// patchRepo is PATCH .../repos/{repo}: its default branch, which is the
// bare repo's HEAD, and which nothing but this changes.
func (s *Server) patchRepo(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var body map[string]json.RawMessage
	if err := decode(q, &body); err != nil {
		return nil, err
	}
	var branch string
	if raw, ok := body["default_branch"]; ok {
		if err := json.Unmarshal(raw, &branch); err != nil || !repos.ValidBranch(branch) {
			return nil, errorf(InvalidArgument, "default_branch %s is not a branch name git takes", raw).with("field", "default_branch")
		}
	}
	var out *RepoObject
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, err := s.repoSilo(ctx, tx, &q.caller, ref, true)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindRepoPatch)
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
		if branch != "" && branch != r.DefaultBranch {
			if s.opts.Repos == nil {
				return errorf(FailedPrecondition, "this silod keeps no repos: it runs without a data directory")
			}
			old := r.DefaultBranch
			if err := repos.SetDefaultBranch(ctx, tx, r.ID, branch); err != nil {
				return err
			}
			r.DefaultBranch = branch
			if err := Audit(ctx, tx, t, q.caller, KindRepoPatch, q.requestID, "OK",
				map[string]any{"repo": r.Name, "id": r.ID, "default_branch": branch, "was": old}); err != nil {
				return err
			}
			if !q.validateOnly {
				if err := s.opts.Repos.SetHead(ctx, t.ID, r.ID, branch); err != nil {
					return errorf(Internal, "moving %s's repo %s's HEAD: %v", t.Path, r.Name, err)
				}
			}
		}
		out = repoObject(t, eff, r)
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// deleteRepo is DELETE .../repos/{repo}: an operation that is done once
// the repo is off the disk. With a mirror that takes it over, that is
// once the mirror has archived its refs in the uber repo; otherwise at
// once (local-git.md, "Deleting a repo or a silo").
func (s *Server) deleteRepo(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var out any
	var gone *repos.Repo
	var opID string
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, err := s.repoSilo(ctx, tx, &q.caller, ref, true)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindRepoDelete)
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
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		if err := dropRepo(ctx, tx, t, r, q.caller, q.requestID, nil); err != nil {
			return err
		}
		if q.validateOnly {
			out = map[string]any{"validate_only": true, "deletes": path.Join(t.Path, r.Name)}
			return errValidateOnly
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		cj, _ := json.Marshal(q.caller)
		opID = ids.New() + ids.New()
		if _, err := tx.Exec(ctx, `INSERT INTO silo_operations (id, kind, silo, path, caller, state, done, error, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
			opID, KindRepoDelete, r.ID, path.Join(t.Path, r.Name), string(cj), OpDeleting, false, store.Millis(now), store.Millis(now)); err != nil {
			return err
		}
		gone = r
		if s.opts.Mirror != nil {
			// The mirror archives the repo's refs in the uber repo
			// first, then removes it and finishes the operation
			// (local-git.md, "Deleting a repo or a silo", call L49).
			handed, err := s.opts.Mirror.Deleting(ctx, tx, t, r, opID)
			if err != nil {
				return err
			}
			if handed {
				gone = nil
			}
		}
		op, err := readOp(ctx, tx, `id = ?`, opID)
		if err != nil {
			return err
		}
		out = op.Operation
		return s.finish(ctx, tx, q, out)
	})
	if err == errValidateOnly {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if gone == nil {
		return out, nil // the mirror's now
	}
	// The row is gone; now the files. A failure leaves the operation
	// DELETE_FAILED, naming the directory that is left.
	state, done := OpDone, true
	var e *Error
	if err := s.opts.Repos.Delete(ctx, gone.Silo, gone.ID); err != nil {
		state, done = OpDeleteFailed, false
		e = errorf(Internal, "removing repo %s's directory: %v", gone.ID, err).with("repo", gone.ID)
	}
	_ = s.st.Tx(ctx, func(tx store.Tx) error { return setOp(ctx, tx, opID, state, done, e) })
	if o, ok := s.readOperation(ctx, opID); ok {
		return o, nil
	}
	return out, nil
}

// readOperation reads an operation in a transaction of its own.
func (s *Server) readOperation(ctx context.Context, id string) (Operation, bool) {
	var out Operation
	ok := false
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		op, err := readOp(ctx, tx, `id = ?`, id)
		if err != nil || op == nil {
			return err
		}
		out, ok = op.Operation, true
		return nil
	})
	return out, ok
}

// dropRepo removes a repo's row and gives its silo its repos count
// back, audited under the silo. Its directory is the caller's to
// remove, once this commits.
func dropRepo(ctx context.Context, tx store.Tx, t *tree.Silo, r *repos.Repo, c Caller, requestID string, extra map[string]any) error {
	if _, err := ledger.Refund(ctx, tx, repoChargeObject(r.ID), nil); err != nil {
		return err
	}
	if err := repos.Delete(ctx, tx, r.ID); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"silo": t.ID, "name": r.Name, "what": "delete"})
	if _, err := tx.Append(ctx, repos.ChangeKind, r.ID, string(data)); err != nil {
		return err
	}
	fields := map[string]any{"repo": r.Name, "id": r.ID}
	for k, v := range extra {
		fields[k] = v
	}
	return Audit(ctx, tx, t, c, KindRepoDelete, requestID, "OK", fields)
}

// deleteReposIn deletes silo t's repos, as t is deleted: their rows and
// charges, and then their directories, but for those the mirror takes
// over to archive first.
func (s *Server) deleteReposIn(ctx context.Context, t *tree.Silo, c Caller) error {
	var gone []*repos.Repo
	err := s.tx(ctx, func(tx store.Tx) error {
		gone = nil
		rs, err := repos.Of(ctx, tx, t.ID)
		if err != nil || len(rs) == 0 {
			return err
		}
		chain, err := tree.Ancestry(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		for _, r := range rs {
			if err := dropRepo(ctx, tx, t, r, c, "", map[string]any{"reason": "silo deleted"}); err != nil {
				return err
			}
			if s.opts.Mirror != nil {
				handed, err := s.opts.Mirror.Deleting(ctx, tx, t, r, "")
				if err != nil {
					return err
				}
				if handed {
					continue
				}
			}
			gone = append(gone, r)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range gone {
		if s.opts.Repos == nil {
			break
		}
		if err := s.opts.Repos.Delete(ctx, r.Silo, r.ID); err != nil {
			return errorf(Internal, "removing repo %s's directory: %v", r.ID, err)
		}
	}
	return nil
}
