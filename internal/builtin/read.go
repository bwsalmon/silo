package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
)

const maxOutput = 256 * 1024

// NewRepoRead returns the repo-read tool set over a checkout.
func NewRepoRead(repo *gitrepo.Repo) *set {
	r := &reader{repo: repo}
	s := &set{}
	s.add("list_files", "List files in the repository (relative paths; .git is skipped).",
		mcp.Schema(map[string]any{
			"path":      str("Directory to list, relative to the repository root. Default: the root."),
			"recursive": flag("List the whole tree below path. Default true."),
		}), r.listFiles)
	s.add("read_file", "Read a text file from the repository.",
		mcp.Schema(map[string]any{
			"path":   str("File path relative to the repository root."),
			"offset": num("First line to return, 1-based. Default 1."),
			"limit":  num("Number of lines to return. Default: all (up to 256 KiB)."),
		}, "path"), r.readFile)
	s.add("grep", "Search file contents with a regular expression (RE2 syntax). Returns path:line:text.",
		mcp.Schema(map[string]any{
			"pattern":     str("Regular expression."),
			"path":        str("File or directory to search. Default: the whole repository."),
			"ignore_case": flag("Match case-insensitively."),
			"max_results": num("Stop after this many matches. Default 200."),
		}, "pattern"), r.grep)
	s.add("git_log", "Show commit history (git log).",
		mcp.Schema(map[string]any{
			"ref":       str("Revision to start from. Default HEAD."),
			"path":      str("Only commits touching this path."),
			"max_count": num("Number of commits. Default 20."),
			"patch":     flag("Include each commit's diff."),
		}), r.gitLog)
	s.add("git_diff", "Show changes between commits (git diff from..to), or between from and the working tree.",
		mcp.Schema(map[string]any{
			"from": str("Base revision. Default HEAD."),
			"to":   str("Target revision. Default: the working tree."),
			"path": str("Limit to this path."),
			"stat": flag("Only a diffstat."),
		}), r.gitDiff)
	s.add("git_show", "Show a commit (message and diff), or a file's contents at a revision when path is given.",
		mcp.Schema(map[string]any{
			"ref":  str("Revision, e.g. HEAD~2 or a commit id."),
			"path": str("A file to show at that revision instead of the commit."),
		}, "ref"), r.gitShow)
	return s
}

type reader struct{ repo *gitrepo.Repo }

func (r *reader) listFiles(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Path      string `json:"path"`
		Recursive *bool  `json:"recursive"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	dir, err := resolvePath(r.repo.Dir, a.Path)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	recursive := a.Recursive == nil || *a.Recursive
	var out []string
	const max = 5000
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(r.repo.Dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if !recursive {
				out = append(out, rel+"/")
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, rel)
		if len(out) >= max {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if len(out) >= max {
		out = append(out, fmt.Sprintf("... stopped at %d entries; list a subdirectory", max))
	}
	if len(out) == 0 {
		return mcp.Text("(no files)")
	}
	return mcp.Text(strings.Join(out, "\n"))
}

func (r *reader) readFile(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	p, err := resolvePath(r.repo.Dir, a.Path)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		return mcp.Errorf("%s looks binary (%d bytes); not shown", a.Path, len(data))
	}
	if a.Offset > 1 || a.Limit > 0 {
		lines := strings.SplitAfter(string(data), "\n")
		start := max(a.Offset, 1) - 1
		if start >= len(lines) {
			return mcp.Errorf("%s has %d lines", a.Path, len(lines))
		}
		end := len(lines)
		if a.Limit > 0 && start+a.Limit < end {
			end = start + a.Limit
		}
		data = []byte(strings.Join(lines[start:end], ""))
	}
	return mcp.Text(clip(string(data), maxOutput))
}

func (r *reader) grep(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		IgnoreCase bool   `json:"ignore_case"`
		MaxResults int    `json:"max_results"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return mcp.Errorf("bad pattern: %v", err)
	}
	root, err := resolvePath(r.repo.Dir, a.Path)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 200
	}
	var out []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(r.repo.Dir, p)
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			if re.Match(sc.Bytes()) {
				out = append(out, fmt.Sprintf("%s:%d:%s", filepath.ToSlash(rel), n, clip(sc.Text(), 500)))
				if len(out) >= a.MaxResults {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if len(out) == 0 {
		return mcp.Text("(no matches)")
	}
	return mcp.Text(clip(strings.Join(out, "\n"), maxOutput))
}

func (r *reader) gitLog(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Ref      string `json:"ref"`
		Path     string `json:"path"`
		MaxCount int    `json:"max_count"`
		Patch    bool   `json:"patch"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.MaxCount <= 0 {
		a.MaxCount = 20
	}
	if a.Ref == "" {
		a.Ref = "HEAD"
	}
	if err := checkRef(a.Ref); err != nil {
		return mcp.Errorf("%v", err)
	}
	argv := []string{"log", "--no-color", fmt.Sprintf("--max-count=%d", a.MaxCount), "--format=commit %H%nAuthor: %an <%ae>%nDate:   %aI%n%n%w(0,4,4)%B"}
	if a.Patch {
		argv = append(argv, "--patch")
	}
	argv = append(argv, a.Ref, "--")
	if a.Path != "" {
		if _, err := resolvePath(r.repo.Dir, a.Path); err != nil {
			return mcp.Errorf("%v", err)
		}
		argv = append(argv, a.Path)
	}
	return r.git(ctx, argv...)
}

func (r *reader) gitDiff(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
		Path string `json:"path"`
		Stat bool   `json:"stat"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.From == "" {
		a.From = "HEAD"
	}
	argv := []string{"diff", "--no-color"}
	if a.Stat {
		argv = append(argv, "--stat")
	}
	for _, ref := range []string{a.From, a.To} {
		if ref == "" {
			continue
		}
		if err := checkRef(ref); err != nil {
			return mcp.Errorf("%v", err)
		}
		argv = append(argv, ref)
	}
	argv = append(argv, "--")
	if a.Path != "" {
		if _, err := resolvePath(r.repo.Dir, a.Path); err != nil {
			return mcp.Errorf("%v", err)
		}
		argv = append(argv, a.Path)
	}
	return r.git(ctx, argv...)
}

func (r *reader) gitShow(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Ref  string `json:"ref"`
		Path string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if err := checkRef(a.Ref); err != nil {
		return mcp.Errorf("%v", err)
	}
	if a.Path != "" {
		if _, err := resolvePath(r.repo.Dir, a.Path); err != nil {
			return mcp.Errorf("%v", err)
		}
		return r.git(ctx, "show", "--no-color", a.Ref+":"+filepath.ToSlash(filepath.Clean(a.Path)))
	}
	return r.git(ctx, "show", "--no-color", "--stat", "--patch", a.Ref, "--")
}

func (r *reader) git(ctx context.Context, argv ...string) *mcp.ToolResult {
	out, err := r.repo.Git(ctx, argv...)
	if err != nil {
		return mcp.Errorf("%v", err)
	}
	if strings.TrimSpace(out) == "" {
		return mcp.Text("(no output)")
	}
	return mcp.Text(clip(out, maxOutput))
}
