// Package gitserve is silod's git service (docs/silo/local-git.md, §3
// "The git service"): smart HTTP for silo repos, which the gateway
// forwards from http://silo.internal/git/ on port 80, and git's own
// protocol over a hijacked connection for git-remote-silo at the front
// door.
//
// silod runs the git binary itself, upload-pack and receive-pack with
// --stateless-rpc on port 80, in the bare repo and with gitstore's
// scrubbed environment and silod's config, rather than git
// http-backend, so that it controls headers, limits and access. Who
// may do what is the fence API's GitAccess. Pushes to a repo are
// serialized by its lock in gitstore, and fetches never take it. A
// push runs silod's pre-receive hook (hook.go), and one that changed
// refs is recorded: the repo is marked dirty for the mirror, its size
// measured, and repo.push audited with each ref's old and new commit.
package gitserve

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/repos"
)

// Prefix is where the git service is, on port 80 and as silod serves
// it to the gateway (local-git.md, "Where it listens", call L42).
const Prefix = "/git/"

// DefaultMaxBlob is the blob size limit when Options names none: the
// one GitHub, the likeliest uber repo, refuses beyond (local-git.md,
// "Size and limits", call L50).
const DefaultMaxBlob = 100 << 20

// API is what the service needs of the fence API; *fenceapi.Server is
// it.
type API interface {
	GitAccess(ctx context.Context, c fenceapi.Caller, ref, name string, write bool) (*fenceapi.GitTarget, error)
	GitPushed(ctx context.Context, t *fenceapi.GitTarget, requestID string, refs []fenceapi.RefUpdate, size int64) error
}

// Options configure a Service.
type Options struct {
	// API decides access and records pushes.
	API API
	// Host keeps the repos, and runs git in them.
	Host *gitstore.Host
	// Hook is the executable the pre-receive hook runs, as
	// "<Hook> git-hook pre-receive": silod's own binary, whose main
	// hands those arguments to HookMain. Required for pushes: with
	// none, the hook refuses every push.
	Hook string
	// MaxBlob is the largest blob a push may add, in bytes;
	// DefaultMaxBlob if zero.
	MaxBlob int64
	// Disk is a silo's disk use and its disk_gb grant, in bytes, by
	// silo id: a limit below zero, or a nil Disk, is no limit. A push
	// may take the silo at most its free headroom
	// (receive.maxInputSize), and one that starts with the silo over
	// its grant is refused by the hook (local-git.md, "Quota").
	Disk func(ctx context.Context, silo string) (used, limit int64, err error)
}

// waitDelay is how long git's input may stay open after git exits.
const waitDelay = 2 * time.Second

// Service is silod's git service.
type Service struct {
	opts Options
}

// New returns a Service.
func New(opts Options) *Service {
	if opts.MaxBlob == 0 {
		opts.MaxBlob = DefaultMaxBlob
	}
	return &Service{opts: opts}
}

// SetAPI sets Options.API, for a service made before the fence API
// that it is the GitConnector of.
func (s *Service) SetAPI(api API) { s.opts.API = api }

// The hook's environment, which receive-pack passes on to it.
const (
	envHook    = "SILO_GIT_HOOK"
	envMaxBlob = "SILO_GIT_MAX_BLOB"
	envOver    = "SILO_GIT_OVER"
)

// ServeHTTP serves /git/<silo path>/<repo>.git/<endpoint> and
// /git/-<silo id>/<repo>.git/<endpoint>, for the caller in the
// request's context: info/refs, git-upload-pack and
// git-receive-pack, as git's smart HTTP has them. A repo the caller
// may not read is 404 for every endpoint, whether it exists or not.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := fenceapi.CallerFromContext(r.Context())
	if !ok {
		http.Error(w, "silo: the call carries no caller", http.StatusForbidden)
		return
	}
	ref, name, endpoint, ok := ParsePath(r.URL.Path)
	if !ok {
		http.Error(w, "silo: no such repository "+r.URL.Path+"; want /git/<silo path>/<repo>.git/ or /git/-<silo id>/<repo>.git/", http.StatusNotFound)
		return
	}
	// The caller is its origin; git's Authorization, if a client sends
	// one, means nothing here.
	r.Header.Del("Authorization")
	c.Manage = ""
	var service string
	switch {
	case endpoint == "info/refs" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		service = r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			s.access(w, r, c, ref, name, false) // 404 before anything else
			http.Error(w, "silo: only git's smart HTTP is served; the dumb protocol is not", http.StatusForbidden)
			return
		}
	case (endpoint == "git-upload-pack" || endpoint == "git-receive-pack") && r.Method == http.MethodPost:
		service = endpoint
	default:
		if _, ok := s.access(w, r, c, ref, name, false); ok {
			http.Error(w, "silo: no such git endpoint "+r.Method+" "+endpoint, http.StatusNotFound)
		}
		return
	}
	t, ok := s.access(w, r, c, ref, name, service == "git-receive-pack")
	if !ok {
		return
	}
	dir, err := s.opts.Host.Path(t.Silo.ID, t.Repo.ID)
	if err != nil {
		http.Error(w, "silo: "+err.Error(), http.StatusInternalServerError)
		return
	}
	proto := gitProtocol(r.Header.Get("Git-Protocol"))
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	if endpoint == "info/refs" {
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		cmd := s.opts.Host.Command(r.Context(), dir, proto, strings.TrimPrefix(service, "git-"), "--stateless-rpc", "--advertise-refs", ".")
		var out strings.Builder
		var stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			log.Printf("gitserve: %s --advertise-refs in %s: %v: %s", service, t.GitPath(), err, stderr.String())
			http.Error(w, "silo: git failed", http.StatusInternalServerError)
			return
		}
		if !strings.Contains(strings.Join(proto, " "), "version=2") {
			hdr := "# service=" + service + "\n"
			fmt.Fprintf(w, "%04x%s0000", len(hdr)+4, hdr)
		}
		io.WriteString(w, out.String())
		return
	}
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "silo: bad gzip body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	out := &flushWriter{w: w}
	if service == "git-upload-pack" {
		cmd := s.opts.Host.Command(r.Context(), dir, proto, "upload-pack", "--stateless-rpc", ".")
		cmd.Stdin, cmd.Stdout = body, out
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil && r.Context().Err() == nil {
			log.Printf("gitserve: upload-pack in %s: %v: %s", t.GitPath(), err, stderr.String())
		}
		return
	}
	s.push(r.Context(), t, dir, proto, []string{"--stateless-rpc"}, body, out)
}

// access is GitAccess, writing the refusal if there is one.
func (s *Service) access(w http.ResponseWriter, r *http.Request, c fenceapi.Caller, ref, name string, write bool) (*fenceapi.GitTarget, bool) {
	t, err := s.opts.API.GitAccess(r.Context(), c, ref, name, write)
	if err != nil {
		var e *fenceapi.Error
		if errors.As(err, &e) {
			if e.Code == fenceapi.NotFound {
				// The same answer whether the silo, the repo or the
				// caller's view of either is what is missing.
				http.Error(w, "silo: repository not found", http.StatusNotFound)
				return nil, false
			}
			http.Error(w, "silo: "+e.Message, e.Code.Status())
			return nil, false
		}
		http.Error(w, "silo: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return t, true
}

// ParsePath splits a /git/ path into its silo's ref (a path from "/",
// or "-<silo id>"), the repo's name and the git endpoint after it. The
// repo is the segment that ends in .git: silo names are DNS labels,
// with no dots, so the split is unambiguous.
func ParsePath(p string) (ref, name, endpoint string, ok bool) {
	rest, ok := strings.CutPrefix(p, Prefix)
	if !ok {
		return "", "", "", false
	}
	segs := strings.Split(rest, "/")
	for i, seg := range segs {
		n, isRepo := strings.CutSuffix(seg, ".git")
		if !isRepo {
			if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, ".") {
				return "", "", "", false
			}
			continue
		}
		if i == 0 || !repos.ValidName(n) {
			return "", "", "", false
		}
		silo := segs[:i]
		switch {
		case strings.HasPrefix(silo[0], "-") && i == 1:
			ref = silo[0]
		case strings.HasPrefix(silo[0], "-"):
			return "", "", "", false
		default:
			ref = "/" + strings.Join(silo, "/")
		}
		return ref, n, strings.Join(segs[i+1:], "/"), true
	}
	return "", "", "", false
}

var protoRE = regexp.MustCompile(`^[a-zA-Z0-9=:._-]+$`)

// gitProtocol is the environment that passes a client's Git-Protocol
// header (protocol v2) on to git, if it is well formed.
func gitProtocol(h string) []string {
	var keep []string
	for _, p := range strings.Split(h, ":") {
		if protoRE.MatchString(p) {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return []string{"GIT_PROTOCOL=" + strings.Join(keep, ":")}
}

// push runs receive-pack in t's repo at dir, with args, from in to
// out, under the repo's lock; then records what it changed.
func (s *Service) push(ctx context.Context, t *fenceapi.GitTarget, dir string, env, args []string, in io.Reader, out io.Writer) {
	unlock := s.opts.Host.Lock(t.Repo.ID)
	defer unlock()
	before, err := s.opts.Host.Refs(ctx, t.Silo.ID, t.Repo.ID)
	if err != nil {
		log.Printf("gitserve: reading %s's refs: %v", t.GitPath(), err)
		return
	}
	env = append(env, envHook+"="+s.opts.Hook, envMaxBlob+"="+strconv.FormatInt(s.opts.MaxBlob, 10))
	var pre []string
	if s.opts.Disk != nil {
		used, limit, err := s.opts.Disk(ctx, t.Silo.ID)
		if err != nil {
			log.Printf("gitserve: %s's disk use: %v", t.Silo.Path, err)
			env = append(env, envOver+"=silo: cannot measure "+t.Silo.Path+"'s disk use; retry")
		} else if limit >= 0 {
			if used >= limit {
				env = append(env, envOver+"="+OverMessage(t.Silo.Path, used, limit))
			} else {
				pre = []string{"-c", "receive.maxInputSize=" + strconv.FormatInt(limit-used, 10)}
			}
		}
	}
	cmd := s.opts.Host.Command(ctx, dir, env, append(append(pre, "receive-pack"), append(args, ".")...)...)
	var stderr strings.Builder
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	// A client that keeps its connection open after git is done holds
	// nothing up.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		log.Printf("gitserve: receive-pack in %s: %v: %s", t.GitPath(), err, stderr.String())
	}
	// The refs may have moved even if the client has gone: what moved
	// is recorded regardless.
	ctx = context.WithoutCancel(ctx)
	after, err := s.opts.Host.Refs(ctx, t.Silo.ID, t.Repo.ID)
	if err != nil {
		log.Printf("gitserve: reading %s's refs: %v", t.GitPath(), err)
		return
	}
	changed := Diff(before, after)
	if len(changed) == 0 {
		return
	}
	if err := s.opts.Host.Maintain(ctx, t.Silo.ID, t.Repo.ID); err != nil {
		log.Printf("gitserve: maintenance of %s: %v", t.GitPath(), err)
	}
	size, err := s.opts.Host.Size(ctx, t.Silo.ID, t.Repo.ID)
	if err != nil {
		log.Printf("gitserve: measuring %s: %v", t.GitPath(), err)
		size = -1
	}
	if err := s.opts.API.GitPushed(ctx, t, ids.New()+ids.New(), changed, size); err != nil {
		log.Printf("gitserve: recording a push to %s: %v", t.GitPath(), err)
	}
}

// Diff is every ref whose commit differs between before and after, by
// name: the refs a push changed, archive refs its hook wrote included.
func Diff(before, after map[string]string) []fenceapi.RefUpdate {
	var out []fenceapi.RefUpdate
	for ref, n := range after {
		if o := before[ref]; o != n {
			out = append(out, fenceapi.RefUpdate{Ref: ref, Old: o, New: n})
		}
	}
	for ref, o := range before {
		if _, ok := after[ref]; !ok {
			out = append(out, fenceapi.RefUpdate{Ref: ref, Old: o})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// OverMessage is the hook's refusal of a push to a silo over its
// disk_gb grant.
func OverMessage(silo string, used, limit int64) string {
	gib := func(b int64) string {
		return strings.TrimSuffix(strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64), ".0")
	}
	return fmt.Sprintf("silo: %s is over its disk_gb grant (%s of %s GiB)", silo, gib(used), gib(limit))
}

// flushWriter flushes each write through, so that git's progress and
// its sideband reach the client as they are written.
type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// Connect serves git's own protocol for service over r's connection,
// which it hijacks after answering 101: what git-remote-silo's
// connect capability speaks at the front door. t is GitAccess's.
func (s *Service) Connect(w http.ResponseWriter, r *http.Request, t *fenceapi.GitTarget, service string) {
	dir, err := s.opts.Host.Path(t.Silo.ID, t.Repo.ID)
	if err != nil {
		http.Error(w, "silo: "+err.Error(), http.StatusInternalServerError)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "silo: this connection can't carry git", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: git\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	proto := gitProtocol(r.Header.Get("Git-Protocol"))
	in := io.Reader(rw.Reader)
	if service == "git-receive-pack" {
		s.push(ctx, t, dir, proto, nil, in, conn)
		return
	}
	cmd := s.opts.Host.Command(ctx, dir, proto, "upload-pack", ".")
	var stderr strings.Builder
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, conn, &stderr
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			log.Printf("gitserve: upload-pack in %s: %v", t.GitPath(), err)
		}
	}
}

var _ fenceapi.GitConnector = (*Service)(nil)
