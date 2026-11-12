package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/router"
)

// Host is what the built-ins need from the tool-set host: hot reloading
// after a commit or a pull, and loading one-off tools.
type Host interface {
	ReloadRepoSets(ctx context.Context, names []string) (string, bool)
	LoadScratch(ctx context.Context, name, dir string) (string, error)
	UnloadScratch(name string) bool
}

// Edit is one file change in a proposal.
type Edit struct {
	Path string `json:"path"`
	// Content replaces the whole file (or creates it).
	Content *string `json:"content,omitempty"`
	// OldString, which must occur exactly once, is replaced by NewString.
	OldString string `json:"old_string,omitempty"`
	NewString string `json:"new_string,omitempty"`
	// Delete removes the file.
	Delete bool `json:"delete,omitempty"`
}

// Writer commits approved changes to the checkout and pushes them.
type Writer struct {
	Repo     *gitrepo.Repo
	Branch   string
	Remote   string // "" commits without pushing
	Identity config.Identity
	Approver approval.Approver
	Host     Host
	Log      audit.Logger

	mu sync.Mutex
}

// NewRepoWrite returns the repo-write tool set.
func NewRepoWrite(w *Writer) *set {
	s := &set{}
	editSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       str("File path relative to the repository root."),
			"content":    str("New full contents (creates the file if needed)."),
			"old_string": str("Text to replace; must occur exactly once in the file."),
			"new_string": str("Replacement for old_string."),
			"delete":     flag("Delete the file."),
		},
		"required": []string{"path"},
	}
	s.add("propose_change",
		"Propose a commit: a set of file edits plus a commit message. The person sees the full diff in their terminal and approves or rejects it. "+
			"Only on approval is it written, committed and pushed (to branch "+w.Branch+"). A rejection returns the person's reason. "+
			"Changes under .silo/tools/ are hot-loaded as tool sets once committed.",
		mcp.Schema(map[string]any{
			"message": str("Commit message: a summary line, a blank line, then the why."),
			"edits":   map[string]any{"type": "array", "items": editSchema, "description": "File edits; each gives content, old_string/new_string, or delete."},
		}, "message", "edits"),
		func(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
			var a struct {
				Message string `json:"message"`
				Edits   []Edit `json:"edits"`
			}
			if err := decode(args, &a); err != nil {
				return mcp.Errorf("%v", err)
			}
			return w.Propose(ctx, a.Message, a.Edits, nil)
		})
	return s
}

// Propose shows the change for approval and, once approved, commits,
// pushes and hot-reloads. after, if set, runs after a successful commit
// and its output is appended to the result.
func (w *Writer) Propose(ctx context.Context, message string, edits []Edit, after func(context.Context) string) *mcp.ToolResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.TrimSpace(message) == "" {
		return mcp.Errorf("a commit message is required")
	}
	if len(edits) == 0 {
		return mcp.Errorf("no edits proposed")
	}
	head, err := w.Repo.Head(ctx)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	changes, err := w.plan(edits)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	diff, err := renderDiff(changes)
	if err != nil {
		return mcp.Errorf("diff: %v", err)
	}
	dest := w.Branch
	if w.Remote != "" {
		dest = w.Remote + "/" + w.Branch
	}
	summary, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	d := w.Approver.Approve(ctx, approval.Request{
		Kind:  "commit",
		Title: fmt.Sprintf("The agent proposes a commit to %s: %s", dest, summary),
		Body:  "Commit message:\n\n" + indent(message) + "\n\n" + diff,
	})
	if !d.Approved {
		return mcp.Errorf("The person rejected this change. Nothing was written. Their reason: %s", d.Reason)
	}
	if now, err := w.Repo.Head(ctx); err != nil || now != head {
		return mcp.Errorf("the checkout moved while the change was waiting for approval; propose it again")
	}

	var paths []string
	for _, c := range changes {
		paths = append(paths, c.rel)
		if c.delete {
			if err := os.Remove(c.abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				return w.rollback(ctx, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.abs), 0o755); err != nil {
			return w.rollback(ctx, err)
		}
		mode := os.FileMode(0o644)
		if fi, err := os.Stat(c.abs); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(c.abs, []byte(c.after), mode); err != nil {
			return w.rollback(ctx, err)
		}
	}
	if _, err := w.Repo.Git(ctx, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return w.rollback(ctx, err)
	}
	// Only the proposed paths are committed, whatever else is staged.
	commit := []string{"-c", "user.name=" + w.Identity.Name, "-c", "user.email=" + w.Identity.Email, "commit", "-q", "-m", message, "--"}
	if _, err := w.Repo.Git(ctx, append(commit, paths...)...); err != nil {
		return w.rollback(ctx, err)
	}
	sha, _ := w.Repo.Head(ctx)
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	w.Log.Log("commit", map[string]any{"commit": sha, "branch": w.Branch, "paths": paths})

	var out []string
	isErr := false
	out = append(out, fmt.Sprintf("Approved and committed %s on %s.", short, w.Branch))
	if w.Remote == "" {
		out = append(out, "Not pushed: pushing is off for this session.")
	} else if _, err := w.Repo.Git(ctx, "push", "-q", w.Remote, "HEAD:refs/heads/"+w.Branch); err != nil {
		isErr = true
		out = append(out, fmt.Sprintf("The push to %s failed, so the commit is only local: %v", dest, err))
		w.Log.Log("push_failed", map[string]any{"commit": sha, "error": err.Error()})
	} else {
		out = append(out, "Pushed to "+dest+".")
		w.Log.Log("pushed", map[string]any{"commit": sha, "dest": dest})
	}
	if names := router.ChangedToolsets(paths); len(names) > 0 && w.Host != nil {
		report, ok := w.Host.ReloadRepoSets(ctx, names)
		out = append(out, "Tool sets:\n"+report)
		if !ok {
			isErr = true
			out = append(out, "The commit landed, but a tool set failed to load; fix it with another proposal.")
		}
	}
	if after != nil {
		out = append(out, after(ctx))
	}
	res := mcp.Text(strings.Join(out, "\n"))
	res.IsError = isErr
	return res
}

// rollback undoes a half-applied change.
func (w *Writer) rollback(ctx context.Context, cause error) *mcp.ToolResult {
	w.Repo.Git(ctx, "reset", "-q", "--hard", "HEAD")
	w.Repo.Git(ctx, "clean", "-q", "-fd")
	return mcp.Errorf("applying the approved change failed, and it was rolled back: %v", cause)
}

type change struct {
	rel, abs      string
	before, after string
	existed       bool
	delete        bool
}

func (w *Writer) plan(edits []Edit) ([]*change, error) {
	byPath := map[string]*change{}
	var order []*change
	for i, e := range edits {
		abs, err := resolvePath(w.Repo.Dir, e.Path)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %v", i+1, err)
		}
		rel, _ := filepath.Rel(w.Repo.Dir, abs)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil, fmt.Errorf("edit %d: a file path is required", i+1)
		}
		// Writing a symlink writes its target, while the diff shows (and
		// git commits) the link's path.
		if fi, err := os.Lstat(abs); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("edit %d: %s is a symlink; edit the file it points to instead", i+1, rel)
		}
		c := byPath[rel]
		if c == nil {
			c = &change{rel: rel, abs: abs}
			data, err := os.ReadFile(abs)
			switch {
			case err == nil:
				c.before, c.existed = string(data), true
			case errors.Is(err, os.ErrNotExist):
			default:
				return nil, fmt.Errorf("edit %d: %v", i+1, err)
			}
			c.after = c.before
			byPath[rel] = c
			order = append(order, c)
		}
		modes := 0
		if e.Content != nil {
			modes++
		}
		if e.OldString != "" {
			modes++
		}
		if e.Delete {
			modes++
		}
		if modes != 1 {
			return nil, fmt.Errorf("edit %d (%s): give exactly one of content, old_string/new_string or delete", i+1, rel)
		}
		switch {
		case e.Delete:
			if !c.existed {
				return nil, fmt.Errorf("edit %d: %s does not exist", i+1, rel)
			}
			c.delete, c.after = true, ""
		case e.Content != nil:
			c.delete, c.after = false, *e.Content
		default:
			if n := strings.Count(c.after, e.OldString); n != 1 {
				return nil, fmt.Errorf("edit %d: old_string occurs %d times in %s; it must occur exactly once", i+1, n, rel)
			}
			c.after = strings.Replace(c.after, e.OldString, e.NewString, 1)
		}
	}
	var out []*change
	for _, c := range order {
		if c.delete || !c.existed || c.after != c.before {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the edits change nothing")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// renderDiff makes a unified diff of the planned changes with git diff
// --no-index, in a scratch directory: nothing touches the checkout
// before approval.
func renderDiff(changes []*change) (string, error) {
	tmp, err := os.MkdirTemp("", "silo-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	var all strings.Builder
	for _, c := range changes {
		a, b := "/dev/null", "/dev/null"
		if c.existed {
			a = filepath.Join("a", c.rel)
			if err := writeTemp(tmp, a, c.before); err != nil {
				return "", err
			}
		}
		if !c.delete {
			b = filepath.Join("b", c.rel)
			if err := writeTemp(tmp, b, c.after); err != nil {
				return "", err
			}
		}
		d, err := diffNoIndex(tmp, a, b)
		if err != nil {
			return "", err
		}
		all.WriteString(d)
	}
	return all.String(), nil
}

func writeTemp(root, rel, content string) error {
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o600)
}

// diffNoIndex runs git diff --no-index, for which exit status 1 means
// "they differ".
func diffNoIndex(dir, a, b string) (string, error) {
	cmd := exec.Command("git", "diff", "--no-index", "--no-color", "--no-prefix", "--", a, b)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		return "", fmt.Errorf("%v: %s", err, errb.String())
	}
	return out.String(), nil
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
