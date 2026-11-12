// Package gitrepo runs git for silo: preparing a context repository's
// checkout, and the commands the built-in tool sets need.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/internal/config"
)

// Repo is a git working tree.
type Repo struct {
	Dir string
}

// Git runs git in the working tree and returns its stdout. A failure's
// error carries stderr.
func (r *Repo) Git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, r.Dir, args...)
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never prompt on the person's terminal for credentials: silo owns
	// that terminal, and a prompt there would hang the session. Paths
	// after "--" come from the agent, so they are file names, never
	// pathspec magic (":(glob)**" or ":/" would make `git add` stage
	// files the person was not shown).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// Checkout is a prepared context repository.
type Checkout struct {
	Repo
	// Remote is the remote name, or "" for a repository without one.
	Remote string
	// Base is the branch the session starts from.
	Base string
}

// StartPoint is the ref the session branch is created from.
func (c *Checkout) StartPoint() string {
	if c.Remote != "" {
		return c.Remote + "/" + c.Base
	}
	return c.Base
}

// Prepare finds or clones the repository name under the configured root
// and fetches it. base is the branch to start from; "" means the
// repository's default branch.
func Prepare(ctx context.Context, cfg *config.Config, name, base string) (*Checkout, error) {
	if err := config.ValidateRepoName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(cfg.Repos.Root, filepath.FromSlash(name))
	remote := cfg.Push.Remote
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if cfg.Repos.GitBase == "" {
			return nil, fmt.Errorf("no repository %s under %s", name, cfg.Repos.Root)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		if _, err := run(ctx, filepath.Dir(dir), "clone", "--origin", remote, cfg.RepoURL(name), dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	c := &Checkout{Repo: Repo{Dir: dir}}
	if _, err := c.Git(ctx, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository", dir)
	}
	if status, err := c.Git(ctx, "status", "--porcelain"); err != nil {
		return nil, err
	} else if strings.TrimSpace(status) != "" {
		return nil, fmt.Errorf("%s has uncommitted changes; commit or stash them first:\n%s", dir, status)
	}
	if _, err := c.Git(ctx, "remote", "get-url", remote); err == nil {
		c.Remote = remote
		if _, err := c.Git(ctx, "fetch", "--prune", remote); err != nil {
			return nil, err
		}
	}
	if base == "" {
		var err error
		if base, err = c.defaultBranch(ctx); err != nil {
			return nil, err
		}
	}
	c.Base = base
	if _, err := c.Git(ctx, "rev-parse", "--verify", "--quiet", c.StartPoint()+"^{commit}"); err != nil {
		return nil, fmt.Errorf("no branch %s in %s", c.StartPoint(), name)
	}
	return c, nil
}

func (c *Checkout) defaultBranch(ctx context.Context) (string, error) {
	if c.Remote != "" {
		ref := "refs/remotes/" + c.Remote + "/HEAD"
		out, err := c.Git(ctx, "symbolic-ref", "--short", ref)
		if err != nil {
			c.Git(ctx, "remote", "set-head", c.Remote, "--auto")
			out, err = c.Git(ctx, "symbolic-ref", "--short", ref)
		}
		if err == nil {
			return strings.TrimPrefix(strings.TrimSpace(out), c.Remote+"/"), nil
		}
	}
	out, err := c.Git(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", errors.New("cannot tell the default branch; pass --branch")
	}
	return strings.TrimSpace(out), nil
}

// StartBranch checks out a local work branch for the session. If the
// remote already has that branch (a configured push branch), the session
// continues from it; otherwise it starts from the base.
func (c *Checkout) StartBranch(ctx context.Context, branch string) error {
	from := c.StartPoint()
	if c.Remote != "" {
		if _, err := c.Git(ctx, "rev-parse", "--verify", "--quiet", c.Remote+"/"+branch+"^{commit}"); err == nil {
			from = c.Remote + "/" + branch
		}
	}
	_, err := c.Git(ctx, "checkout", "-q", "-B", branch, from)
	return err
}

// Head is HEAD's commit id.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.Git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// ChangedPaths lists the paths that differ between two commits.
func (r *Repo) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	out, err := r.Git(ctx, "diff", "--name-only", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
