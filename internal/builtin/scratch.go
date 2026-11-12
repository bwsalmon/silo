package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/router"
)

// maxScratchBytes caps a one-off tool set's total size.
const maxScratchBytes = 1 << 20

// Scratch manages one-off tool sets: session-only tool sets the agent
// writes, in the same format as .silo/tools/<set>/, kept under the
// session's directory and never committed. Each creation or change runs
// code on the person's machine, so each needs their approval.
type Scratch struct {
	// Dir holds one directory per one-off tool set.
	Dir      string
	Approver approval.Approver
	Host     Host
	Writer   *Writer
	Log      audit.Logger

	mu sync.Mutex
}

// NewScratch returns the scratch tool set.
func NewScratch(s *Scratch) *set {
	t := &set{}
	t.add("define",
		"Create or change a one-off tool set for this session only: a toolset.yaml manifest plus the MCP server's code, "+
			"in the same format as .silo/tools/<set>/ (see the repository layout docs). The person sees the full code and must approve it, "+
			"because it runs on their machine. Once approved it is loaded and its tools appear as scratch.<name>.<tool>. "+
			"It is deleted when the session ends; use scratch.promote to commit it to the repository.",
		mcp.Schema(map[string]any{
			"name":     str("Tool-set name: lowercase letters, digits, '-' and '_'."),
			"manifest": str("The toolset.yaml contents; its name must equal name."),
			"files":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "The server's files: relative path -> contents."},
		}, "name", "manifest", "files"), s.define)
	t.add("remove", "Stop and delete a one-off tool set.",
		mcp.Schema(map[string]any{"name": str("Tool-set name.")}, "name"), s.remove)
	t.add("list", "List this session's one-off tool sets.", mcp.Schema(map[string]any{}), s.list)
	t.add("promote",
		"Turn a one-off tool set into a repository tool set: proposes a commit adding it under .silo/tools/<name>/, "+
			"which the person approves like any repo-write change. Once committed it loads as <name>.<tool> and the one-off is removed.",
		mcp.Schema(map[string]any{
			"name":    str("The one-off tool set to promote."),
			"message": str("Commit message."),
		}, "name", "message"), s.promote)
	return t
}

func (s *Scratch) setDir(name string) string { return filepath.Join(s.Dir, name) }

func (s *Scratch) define(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Name     string            `json:"name"`
		Manifest string            `json:"manifest"`
		Files    map[string]string `json:"files"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if err := config.ValidToolsetName(a.Name); err != nil {
		return mcp.Errorf("%v", err)
	}
	if _, err := config.ParseManifest([]byte(a.Manifest), a.Name); err != nil {
		return mcp.Errorf("manifest: %v", err)
	}
	files := map[string]string{config.Manifest: a.Manifest}
	total := len(a.Manifest)
	for p, c := range a.Files {
		clean := filepath.ToSlash(filepath.Clean(p))
		if filepath.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
			return mcp.Errorf("file path %q must be relative and inside the tool set", p)
		}
		if clean == config.Manifest {
			return mcp.Errorf("pass toolset.yaml as manifest, not in files")
		}
		files[clean] = c
		total += len(c)
	}
	if total > maxScratchBytes {
		return mcp.Errorf("one-off tool set is %d bytes; the limit is %d", total, maxScratchBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.setDir(a.Name)
	old, existed := readTree(dir)
	body := renderFiles(files)
	verb := "create"
	if existed {
		verb = "change"
		var changes []*change
		for _, p := range unionKeys(old, files) {
			o, had := old[p]
			n, has := files[p]
			if had == has && o == n {
				continue
			}
			changes = append(changes, &change{rel: p, before: o, after: n, existed: had, delete: !has})
		}
		if len(changes) == 0 {
			return mcp.Text("scratch." + a.Name + " is unchanged.")
		}
		diff, err := renderDiff(changes)
		if err != nil {
			return mcp.Errorf("diff: %v", err)
		}
		body = "Changes from the running version:\n\n" + diff + "\nFull code of the new version:\n\n" + body
	}
	d := s.Approver.Approve(ctx, approval.Request{
		Kind:  "scratch",
		Title: fmt.Sprintf("The agent wants to %s one-off tool set scratch.%s, which runs this code on your machine", verb, a.Name),
		Body:  body,
	})
	if !d.Approved {
		return mcp.Errorf("The person rejected one-off tool set %s. Nothing was written. Their reason: %s", a.Name, d.Reason)
	}
	tmp := dir + ".new"
	os.RemoveAll(tmp)
	for p, c := range files {
		if err := writeTemp(tmp, p, c); err != nil {
			os.RemoveAll(tmp)
			return mcp.Errorf("%v", err)
		}
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return mcp.Errorf("%v", err)
	}
	s.Log.Log("scratch_defined", map[string]any{"toolset": a.Name, "change": existed})
	report, err := s.Host.LoadScratch(ctx, a.Name, dir)
	if err != nil {
		return mcp.Errorf("Approved and written, but loading scratch.%s failed:\n%v", a.Name, err)
	}
	return mcp.Text("Approved. " + report)
}

func (s *Scratch) remove(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if err := config.ValidToolsetName(a.Name); err != nil {
		return mcp.Errorf("%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unloaded := s.Host.UnloadScratch(a.Name)
	_, statErr := os.Stat(s.setDir(a.Name))
	os.RemoveAll(s.setDir(a.Name))
	if !unloaded && statErr != nil {
		return mcp.Errorf("no one-off tool set %q", a.Name)
	}
	return mcp.Text("Removed scratch." + a.Name + ".")
}

func (s *Scratch) list(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	ents, _ := os.ReadDir(s.Dir)
	var names []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".new") {
			names = append(names, "scratch."+e.Name())
		}
	}
	if len(names) == 0 {
		return mcp.Text("(no one-off tool sets)")
	}
	return mcp.Text(strings.Join(names, "\n"))
}

func (s *Scratch) promote(ctx context.Context, args json.RawMessage) *mcp.ToolResult {
	var a struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if err := decode(args, &a); err != nil {
		return mcp.Errorf("%v", err)
	}
	if err := config.ValidToolsetName(a.Name); err != nil {
		return mcp.Errorf("%v", err)
	}
	if router.Reserved[a.Name] {
		return mcp.Errorf("%q is a built-in tool set's name; define the one-off under another name first", a.Name)
	}
	files, ok := readTree(s.setDir(a.Name))
	if !ok {
		return mcp.Errorf("no one-off tool set %q", a.Name)
	}
	var edits []Edit
	for _, p := range sortedKeys(files) {
		c := files[p]
		edits = append(edits, Edit{Path: config.ToolsDir + "/" + a.Name + "/" + p, Content: &c})
	}
	return s.Writer.Propose(ctx, a.Message, edits, func(ctx context.Context) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.Host.UnloadScratch(a.Name)
		os.RemoveAll(s.setDir(a.Name))
		return "Removed the one-off scratch." + a.Name + "; use " + a.Name + ".<tool> from now on."
	})
}

// Cleanup deletes every one-off tool set; the session calls it on exit.
func (s *Scratch) Cleanup() error {
	return os.RemoveAll(s.Dir)
}

// readTree reads every file under dir, by slash-separated relative path.
func readTree(dir string) (map[string]string, bool) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			files[filepath.ToSlash(rel)] = string(data)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false
		}
		return files, true
	}
	return files, true
}

// renderFiles shows every file in full, manifest first.
func renderFiles(files map[string]string) string {
	var b strings.Builder
	for _, p := range sortedKeys(files) {
		fmt.Fprintf(&b, "==== %s ====\n%s", p, files[p])
		if !strings.HasSuffix(files[p], "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if (keys[i] == config.Manifest) != (keys[j] == config.Manifest) {
			return keys[i] == config.Manifest
		}
		return keys[i] < keys[j]
	})
	return keys
}

func unionKeys(a, b map[string]string) []string {
	m := map[string]string{}
	for k := range a {
		m[k] = ""
	}
	for k := range b {
		m[k] = ""
	}
	return sortedKeys(m)
}
