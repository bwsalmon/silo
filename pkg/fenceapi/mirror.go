package fenceapi

import (
	"context"
	"net/http"

	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of the outbound mirror (local-git.md, §8).
const (
	KindMirrorSet      = "mirror.set"
	KindMirrorAdopt    = "mirror.adopt"
	KindMirrorStalled  = "mirror_stalled"
	KindMirrorDiverged = "mirror_diverged"
)

// The states of a repo's mirror once there is one (local-git.md, "The
// mirror loop", call L47), beside MirrorOff and MirrorUnconfigured.
const (
	MirrorInSync   = "IN_SYNC"
	MirrorPending  = "PENDING"
	MirrorStalled  = "STALLED"
	MirrorDiverged = "DIVERGED"
	MirrorDeleting = "DELETING"
)

// MirrorSettings is what silo mirror set sets (local-git.md,
// "Configuration and the credential", call L52). Credential is the
// credential file's contents: an SSH deploy key with write access for
// an ssh URL, an HTTPS token for an https one. silod keeps it in
// keys/mirror/ and never in the store, and never shows it back.
type MirrorSettings struct {
	URL         string `json:"url"`
	Credential  string `json:"credential,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	IndexBranch string `json:"index_branch,omitempty"`
}

// MirrorStatus is GET /silo/v1/mirror: the mirror's configuration and
// where every repo that isn't in sync stands.
type MirrorStatus struct {
	Configured  bool   `json:"configured"`
	URL         string `json:"url,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	IndexBranch string `json:"index_branch,omitempty"`
	// Credential is the credential's kind, ssh or https, never itself.
	Credential string `json:"credential,omitempty"`
	// Paused says why no passes run, if none do: report-only mode
	// after a restore (local-git.md, "After a restore").
	Paused string `json:"paused,omitempty"`
	// Refs is how many refs silod has in the uber repo, and
	// EstimatedBytes the sum of the mirrored repos' sizes (local-git.md,
	// "Size and limits", call L50).
	Refs           int   `json:"refs"`
	EstimatedBytes int64 `json:"estimated_bytes"`
	InSync         int   `json:"in_sync"`
	// PendingDeletes is the repos deleted whose refs the mirror has yet
	// to archive: repos_pending_delete.
	PendingDeletes int `json:"pending_deletes"`
	// Repos is every repo that is not IN_SYNC, or every repo if asked.
	Repos []MirrorRepo `json:"repos"`
}

// MirrorRepo is one repo's mirror.
type MirrorRepo struct {
	ID         string        `json:"id"`
	Path       string        `json:"path"` // its silo's path and its name, as /team-a/x/foo
	State      string        `json:"state"`
	LagSeconds int64         `json:"lag_seconds,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	Diverged   []DivergedRef `json:"diverged,omitempty"`
}

// DivergedRef is an external ref whose lease failed: the uber repo has
// Actual ("" if the ref is gone) where silod expected Expected.
type DivergedRef struct {
	Ref      string `json:"ref"`
	Local    string `json:"local"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// MirrorRetry is POST /silo/v1/mirror:retry: Repo, a repo's path, to
// run its pass now and report it, or none to retry every repo at once.
type MirrorRetry struct {
	Repo string `json:"repo,omitempty"`
}

// MirrorAdopt is POST /silo/v1/mirror:adopt (local-git.md, "When the
// uber repo disagrees"): Repo's diverged refs, or those of Refs, are
// archived in the uber repo and overwritten with the local tips, or,
// with KeepExternal, recorded as mirrored as they are.
type MirrorAdopt struct {
	Repo         string   `json:"repo"`
	Refs         []string `json:"refs,omitempty"`
	KeepExternal bool     `json:"keep_external,omitempty"`
}

// Mirror is the outbound mirror (package mirror) as the fence API
// uses it. Set, Status, Retry and Adopt answer the root's calls; the
// errors they return may be *Error.
type Mirror interface {
	Set(ctx context.Context, c Caller, s MirrorSettings) (*MirrorStatus, error)
	Status(ctx context.Context, all bool) (*MirrorStatus, error)
	Retry(ctx context.Context, repoID string) (*MirrorStatus, error)
	Adopt(ctx context.Context, c Caller, repoID string, refs []string, keepExternal bool) (*MirrorStatus, error)
	// Deleting is told, in the transaction that deletes repo r of silo
	// t, that it is going. It returns true if the mirror takes the
	// deletion over: it archives the repo's refs in the uber repo,
	// then removes the repo's directory and finishes operation op ("" for
	// none). False leaves both to the caller, as with no mirror set or
	// mirroring off for the silo.
	Deleting(ctx context.Context, tx store.Tx, t *tree.Silo, r *repos.Repo, op string) (bool, error)
}

// GitPath is a repo's path under /git/ and in the uber repo's refs:
// its silo's path, without the leading "/", and its name, with .git
// (call L46).
func GitPath(siloPath, name string) string { return gitPath(siloPath, name) }

// ReposMirrored reports whether silo's repos are mirrored: its
// effective repos.mirror policy.
func ReposMirrored(ctx context.Context, tx store.Tx, silo string) (bool, error) {
	chain, err := tree.Ancestry(ctx, tx, silo)
	if err != nil {
		return false, err
	}
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return false, err
	}
	return eff.Repos.Mirror, nil
}

// FinishOperation marks operation id DONE.
func FinishOperation(ctx context.Context, tx store.Tx, id string) error {
	return setOp(ctx, tx, id, OpDone, true, nil)
}

// mirrorPath reports whether rest is one of the mirror's paths: GET
// mirror is its status, PUT mirror sets it, POST mirror:retry and
// mirror:adopt. Like the reconcile's, they are silod's own, for the
// root silo's callers only.
func mirrorPath(rest string) bool {
	return rest == "mirror" || rest == "mirror:retry" || rest == "mirror:adopt"
}

func (s *Server) mirror(q *request, rest string) {
	ctx := q.r.Context()
	method := http.MethodPost
	if rest == "mirror" {
		method = http.MethodGet
		if q.r.Method == http.MethodPut {
			method = http.MethodPut
		}
	}
	if q.r.Method != method {
		writeError(q.w, errorf(NotFound, "%s is %s", rest, method))
		return
	}
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		if q.caller.Silo != root.ID || q.caller.Manage != "" {
			return errorf(PermissionDenied, "only the root silo's callers may see or change the mirror")
		}
		return nil
	})
	if err == nil && s.opts.Mirror == nil {
		err = errorf(FailedPrecondition, "this silod keeps no repos, so it mirrors none: it runs without a data directory")
	}
	if err != nil {
		writeError(q.w, err)
		return
	}
	if method != http.MethodGet {
		body, err := readBody(q.r)
		if err != nil {
			writeError(q.w, err)
			return
		}
		q.r = q.r.WithContext(context.WithValue(ctx, bodyKey{}, body))
	}
	var out *MirrorStatus
	switch {
	case method == http.MethodGet:
		out, err = s.opts.Mirror.Status(ctx, q.r.URL.Query().Get("all") == "true")
	case method == http.MethodPut:
		var req MirrorSettings
		if err = decode(q, &req); err == nil {
			out, err = s.opts.Mirror.Set(ctx, q.caller, req)
		}
	case rest == "mirror:retry":
		var req MirrorRetry
		if err = decode(q, &req); err != nil {
			break
		}
		id := ""
		if req.Repo != "" {
			if id, err = s.mirrorRepo(ctx, req.Repo); err != nil {
				break
			}
		}
		out, err = s.opts.Mirror.Retry(ctx, id)
	default:
		var req MirrorAdopt
		if err = decode(q, &req); err != nil {
			break
		}
		var id string
		if id, err = s.mirrorRepo(ctx, req.Repo); err == nil {
			out, err = s.opts.Mirror.Adopt(ctx, q.caller, id, req.Refs, req.KeepExternal)
		}
	}
	if err != nil {
		writeError(q.w, asError(ctx, nil, err))
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// mirrorRepo resolves a repo's path, /team-a/x/foo, to its id.
func (s *Server) mirrorRepo(ctx context.Context, p string) (string, error) {
	var id string
	err := s.tx(ctx, func(tx store.Tx) error {
		silo, name := SplitRepoPath(p)
		if name == "" {
			return errorf(InvalidArgument, "repo %q: want <silo path>/<repo>", p)
		}
		t, err := tree.ByPath(ctx, tx, silo)
		if err != nil {
			return errorf(NotFound, "no silo %s", silo)
		}
		r, err := repos.ByName(ctx, tx, t.ID, name)
		if err != nil {
			return repoNotFound(t, name)
		}
		id = r.ID
		return nil
	})
	return id, err
}

// SplitRepoPath splits /team-a/x/foo into /team-a/x and foo, and /foo
// into / and foo.
func SplitRepoPath(p string) (silo, name string) {
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	i := len(p) - 1
	for i > 0 && p[i] != '/' {
		i--
	}
	silo, name = p[:i], p[i+1:]
	if silo == "" {
		silo = "/"
	}
	return silo, name
}
