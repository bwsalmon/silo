// Package gitstore keeps silo repos on the local backend's disk
// (docs/silo/local-git.md, §5 "On disk"): each a bare git repo in its
// silo's subtree of the data directory, made through the storage
// driver with git init --bare (or, for a seeded repo, a server-side
// clone) and silod's own config. Package repos has the rows; this is
// the fence API's RepoHost.
package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/repos"
)

// Storage is what Host needs of the storage driver: a repo's directory
// made, found, measured and removed. storage.Dir is one.
type Storage interface {
	Create(ctx context.Context, ref storage.Ref, size int64) (string, error)
	Delete(ctx context.Context, ref storage.Ref) error
	Usage(ctx context.Context, ref storage.Ref) (storage.Usage, error)
	Path(ref storage.Ref) (string, error)
}

// SeqFile is the file in a repo's directory that holds the change-log
// sequence number it was created at (local-git.md, "On disk";
// local-rootless-storage.md, "Restoring", call L40). git ignores it.
const SeqFile = ".silo-seq"

// HideRefs is the namespace of local archive refs, which neither
// fetches nor pushes see (local-git.md, "Local archive refs", call L48).
const HideRefs = "refs/silo-archive/"

// Host keeps repos on disk: each a bare repo in its silo's subtree,
// made with git init --bare (or, for a seeded one, a server-side clone)
// and silod's own config. It runs the git binary with a scrubbed
// environment, so that the operator's own git config and credential
// helpers never reach it (local-git.md, "How a request is served").
type Host struct {
	// Storage makes and removes the repos' directories.
	Storage Storage
	// Git is the git binary; "" is git on $PATH.
	Git string
	// HooksDir is silod's own read-only hooks directory, which every
	// repo's core.hooksPath names, never the repo's own. "" names
	// /dev/null, which holds no hooks.
	HooksDir string

	mu    sync.Mutex
	locks map[string]*repoLock // by repo id, while held or waited on
}

func (h *Host) ref(silo, id string) storage.Ref { return storage.Ref{Silo: silo, Repo: id} }

// Path is where silo's repo id is kept.
func (h *Host) Path(silo, id string) (string, error) { return h.Storage.Path(h.ref(silo, id)) }

// Create makes silo's repo id: empty, with HEAD at branch, or a copy of
// seed's heads and tags, with HEAD at branch if it is given and at
// seed's otherwise. It writes silod's config and seq to the repo's
// SeqFile, and returns the repo's default branch and size. On an error
// it leaves nothing behind.
func (h *Host) Create(ctx context.Context, silo, id, branch string, seq uint64, seed *repos.Source) (head string, size int64, err error) {
	if branch != "" && !repos.ValidBranch(branch) {
		return "", 0, fmt.Errorf("gitstore: invalid branch %q", branch)
	}
	ref := h.ref(silo, id)
	if _, err := h.Storage.Create(ctx, storage.Ref{Silo: silo}, 0); err != nil {
		return "", 0, err
	}
	p, err := h.Storage.Create(ctx, ref, 0)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			_ = h.Storage.Delete(context.WithoutCancel(ctx), ref)
		}
	}()
	if entries, err := os.ReadDir(p); err != nil {
		return "", 0, fmt.Errorf("gitstore: %w", err)
	} else if len(entries) > 0 {
		return "", 0, fmt.Errorf("gitstore: %s is not empty", p)
	}
	if seed == nil {
		if branch == "" {
			branch = repos.DefaultBranch
		}
		if _, err := h.git(ctx, "", "init", "--quiet", "--bare", "--initial-branch="+branch, p); err != nil {
			return "", 0, err
		}
	} else {
		src, err := h.Path(seed.Silo, seed.ID)
		if err != nil {
			return "", 0, err
		}
		// --no-local goes through upload-pack, as a fetch does, so the
		// seed's hidden refs stay behind and only its heads and tags come.
		if _, err := h.git(ctx, "", "clone", "--quiet", "--bare", "--no-local", src, p); err != nil {
			return "", 0, err
		}
		if _, err := h.git(ctx, p, "config", "--remove-section", "remote.origin"); err != nil {
			return "", 0, err
		}
		if branch == "" {
			if branch, err = h.Head(ctx, silo, id); err != nil {
				return "", 0, err
			}
		}
	}
	if err := h.Configure(ctx, p); err != nil {
		return "", 0, err
	}
	if _, err := h.git(ctx, p, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(p, SeqFile), []byte(strconv.FormatUint(seq, 10)+"\n"), 0o644); err != nil {
		return "", 0, fmt.Errorf("gitstore: %w", err)
	}
	u, err := h.Storage.Usage(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	return branch, u.Bytes, nil
}

// Config is silod's config of every repo, which Configure writes. The
// git service sets receive.maxInputSize per push.
func (h *Host) Config() [][2]string {
	hooks := h.HooksDir
	if hooks == "" {
		hooks = os.DevNull
	}
	return [][2]string{
		{"core.hooksPath", hooks},
		{"receive.fsckObjects", "true"},
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.hideRefs", HideRefs},
		{"receive.hideRefs", HideRefs},
	}
}

// Configure writes silod's config into the bare repo at p.
func (h *Host) Configure(ctx context.Context, p string) error {
	for _, kv := range h.Config() {
		if _, err := h.git(ctx, p, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// Head is the branch silo's repo id's HEAD names.
func (h *Host) Head(ctx context.Context, silo, id string) (string, error) {
	p, err := h.Path(silo, id)
	if err != nil {
		return "", err
	}
	out, err := h.git(ctx, p, "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	b, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/heads/")
	if !ok {
		return "", fmt.Errorf("gitstore: %s's HEAD is %q, not a branch", p, out)
	}
	return b, nil
}

// SetHead points silo's repo id's HEAD at branch, which need not exist
// yet.
func (h *Host) SetHead(ctx context.Context, silo, id, branch string) error {
	if !repos.ValidBranch(branch) {
		return fmt.Errorf("gitstore: invalid branch %q", branch)
	}
	p, err := h.Path(silo, id)
	if err != nil {
		return err
	}
	_, err = h.git(ctx, p, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

// Seq reads the sequence number in silo's repo id's SeqFile.
func (h *Host) Seq(silo, id string) (uint64, error) {
	p, err := h.Path(silo, id)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(filepath.Join(p, SeqFile))
	if err != nil {
		return 0, fmt.Errorf("gitstore: %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gitstore: %s/%s: %w", p, SeqFile, err)
	}
	return n, nil
}

// Delete removes silo's repo id. One that is gone is not an error.
func (h *Host) Delete(ctx context.Context, silo, id string) error {
	return h.Storage.Delete(ctx, h.ref(silo, id))
}

// Env is the environment silod runs git in: no system or global
// config, so nothing of the operator's, no HOME to find any in, and no
// prompts.
func Env() []string {
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + os.DevNull,
		"XDG_CONFIG_HOME=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	return env
}

func (h *Host) git(ctx context.Context, dir string, args ...string) (string, error) {
	bin := h.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = Env()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			return "", fmt.Errorf("gitstore: git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("gitstore: git %s: %w", args[0], err)
	}
	return out.String(), nil
}

// Lock takes silo's repo id's lock, which serializes its pushes and
// anything else that changes its refs (local-git.md, "How a request
// is served"). Fetches never take it. The returned function releases
// it.
func (h *Host) Lock(id string) (unlock func()) {
	h.mu.Lock()
	if h.locks == nil {
		h.locks = map[string]*repoLock{}
	}
	l := h.locks[id]
	if l == nil {
		l = &repoLock{}
		h.locks[id] = l
	}
	l.users++
	h.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		h.mu.Lock()
		if l.users--; l.users == 0 {
			delete(h.locks, id)
		}
		h.mu.Unlock()
	}
}

type repoLock struct {
	mu    sync.Mutex
	users int // holders and waiters, under Host.mu
}

// Refs reads every ref of silo's repo id, hidden ones included, by
// name.
func (h *Host) Refs(ctx context.Context, silo, id string) (map[string]string, error) {
	p, err := h.Path(silo, id)
	if err != nil {
		return nil, err
	}
	out, err := h.git(ctx, p, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs, nil
}

// Size measures silo's repo id on disk.
func (h *Host) Size(ctx context.Context, silo, id string) (int64, error) {
	u, err := h.Storage.Usage(ctx, h.ref(silo, id))
	return u.Bytes, err
}

// Maintain runs git maintenance run --auto in silo's repo id, as the
// git service does after a push, under the repo's lock. Nothing is
// pruned that any ref reaches, archive refs included.
func (h *Host) Maintain(ctx context.Context, silo, id string) error {
	p, err := h.Path(silo, id)
	if err != nil {
		return err
	}
	_, err = h.git(ctx, p, h.configArgs("maintenance", "run", "--auto", "--quiet")...)
	return err
}

// configArgs is args with silod's config before them, as -c options,
// so that it holds whatever the repo's own config says.
func (h *Host) configArgs(args ...string) []string {
	var out []string
	for _, kv := range h.Config() {
		out = append(out, "-c", kv[0]+"="+kv[1])
	}
	return append(out, args...)
}

// Command is git with silod's config and its scrubbed environment,
// in the bare repo at dir, running args; extra is added to the
// environment. The git service runs upload-pack and receive-pack so.
func (h *Host) Command(ctx context.Context, dir string, extra []string, args ...string) *exec.Cmd {
	bin := h.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, h.configArgs(args...)...)
	cmd.Dir = dir
	cmd.Env = append(Env(), extra...)
	return cmd
}

// Archives lists silo's repo id's local archive refs, oldest first.
func (h *Host) Archives(ctx context.Context, silo, id string) ([]repos.Archive, error) {
	refs, err := h.Refs(ctx, silo, id)
	if err != nil {
		return nil, err
	}
	out := []repos.Archive{}
	for name, sha := range refs {
		rest, ok := strings.CutPrefix(name, HideRefs)
		if !ok {
			continue
		}
		t, ref, ok := repos.ParseArchive(rest)
		if !ok {
			continue
		}
		out = append(out, repos.Archive{Name: rest, Ref: ref, Commit: sha, Time: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RestoreRef makes branch, which must not exist, at the commit of
// silo's repo id's archive ref archive (a name Archives lists), under
// the repo's lock. It returns the commit.
func (h *Host) RestoreRef(ctx context.Context, silo, id, archive, branch string) (string, error) {
	if !repos.ValidBranch(branch) {
		return "", fmt.Errorf("gitstore: invalid branch %q", branch)
	}
	unlock := h.Lock(id)
	defer unlock()
	refs, err := h.Refs(ctx, silo, id)
	if err != nil {
		return "", err
	}
	sha, ok := refs[HideRefs+strings.TrimPrefix(archive, HideRefs)]
	if !ok {
		return "", fmt.Errorf("%w: %s", repos.ErrNoArchive, archive)
	}
	if _, ok := refs["refs/heads/"+branch]; ok {
		return "", fmt.Errorf("%w: %s", repos.ErrRefExists, branch)
	}
	p, err := h.Path(silo, id)
	if err != nil {
		return "", err
	}
	// The zero old value makes git refuse a branch made meanwhile.
	if _, err := h.git(ctx, p, "update-ref", "refs/heads/"+branch, sha, strings.Repeat("0", len(sha))); err != nil {
		return "", err
	}
	return sha, nil
}
