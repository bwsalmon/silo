// Package vendor copies tool sets from another context repository into
// this one's .silo/tools/vendor/<alias>/, recording where they came from
// in a VENDOR.lock. A vendored set is then an ordinary committed tool
// set: sessions never fetch tool sets themselves. `silo vendor` and the
// silo.vendor built-in both use it.
package vendor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/router"
	"gopkg.in/yaml.v3"
)

// MaxBytes caps the size of what one vendoring copies.
const MaxBytes = 8 << 20

// Lock is .silo/tools/vendor/<alias>/VENDOR.lock.
type Lock struct {
	// Repo is the upstream repository, relative to the repos root.
	Repo string `yaml:"repo"`
	// Ref is the ref asked for, or the default branch's name when none
	// was; --update follows it.
	Ref string `yaml:"ref"`
	// Commit is what Ref resolved to; the copied files are its.
	Commit string `yaml:"commit"`
	// Toolsets are the vendored sets' names.
	Toolsets []string `yaml:"toolsets"`
}

// Request says what to vendor.
type Request struct {
	Repo  string
	Ref   string
	Alias string
	// Toolsets to copy; empty means all of the upstream's own.
	Toolsets []string
}

// File is one file of a plan, by checkout-relative slash path.
type File struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Plan is the result of reading a request against its upstream.
type Plan struct {
	Alias    string
	Lock     Lock
	Files    []File // the sets' files and VENDOR.lock, sorted by path
	Warnings []string
}

// Dir is the checkout-relative directory of an alias.
func Dir(alias string) string { return config.VendorDir + "/" + alias }

// CheckAlias validates an alias for a checkout: a valid name, not a
// built-in's, and not one of the repository's own tool sets (whose
// names would then clash with <alias>.<set>).
func CheckAlias(checkout, alias string) error {
	if err := config.ValidToolsetName(alias); err != nil {
		return fmt.Errorf("alias: %w", err)
	}
	if router.Reserved[alias] {
		return fmt.Errorf("alias %q is reserved", alias)
	}
	if fi, err := os.Stat(filepath.Join(checkout, config.ToolsDir, alias)); err == nil && fi.IsDir() {
		return fmt.Errorf("alias %q is also a tool set in %s/; tool-set names must stay unique, so pick another alias", alias, config.ToolsDir)
	}
	return nil
}

// ReadLock reads an alias's VENDOR.lock from the checkout.
func ReadLock(checkout, alias string) (*Lock, error) {
	data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(Dir(alias)), config.VendorLock))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("nothing is vendored as %q (no %s/%s)", alias, Dir(alias), config.VendorLock)
	} else if err != nil {
		return nil, err
	}
	var l Lock
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", Dir(alias), config.VendorLock, err)
	}
	if l.Repo == "" {
		return nil, fmt.Errorf("%s/%s names no repo", Dir(alias), config.VendorLock)
	}
	return &l, nil
}

// Exists reports whether something is vendored as alias.
func Exists(checkout, alias string) bool {
	_, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(Dir(alias))))
	return err == nil
}

// Build reads the requested tool sets from the upstream's committed
// tree at the requested ref. up must be the opened upstream (see
// gitrepo.Open).
func Build(ctx context.Context, up *gitrepo.Checkout, req Request) (*Plan, error) {
	if err := config.ValidToolsetName(req.Alias); err != nil {
		return nil, fmt.Errorf("alias: %w", err)
	}
	ref := req.Ref
	if ref == "" {
		var err error
		if ref, err = up.DefaultBranch(ctx); err != nil {
			return nil, err
		}
	}
	commit, err := up.ResolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	p := &Plan{Alias: req.Alias, Lock: Lock{Repo: req.Repo, Ref: ref, Commit: commit}}

	out, err := up.Git(ctx, "ls-tree", "-r", "-z", "--full-tree", commit, "--", config.ToolsDir+"/")
	if err != nil {
		return nil, err
	}
	type entry struct{ mode, typ, obj, rest string }
	bySet := map[string][]entry{}
	sawVendor := false
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("unexpected git ls-tree output %q", rec)
		}
		rel := strings.TrimPrefix(p, config.ToolsDir+"/")
		set, rest, ok := strings.Cut(rel, "/")
		if !ok {
			continue // a file directly in .silo/tools
		}
		if set == config.VendorName {
			sawVendor = true
			continue
		}
		bySet[set] = append(bySet[set], entry{f[0], f[1], f[2], rest})
	}
	if sawVendor {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s has tool sets vendored from elsewhere in %s/; they are not included (vendoring is not transitive)", req.Repo, config.VendorDir))
	}
	sets := req.Toolsets
	if len(sets) == 0 {
		for s := range bySet {
			sets = append(sets, s)
		}
		if len(sets) == 0 {
			return nil, fmt.Errorf("%s has no tool sets in %s/ at %s", req.Repo, config.ToolsDir, short(commit))
		}
	}
	sort.Strings(sets)
	total := 0
	seen := map[string]bool{}
	for _, set := range sets {
		if seen[set] {
			continue
		}
		seen[set] = true
		if set == config.VendorName {
			return nil, fmt.Errorf("%s/ holds the upstream's own vendored tool sets, which are not included (vendoring is not transitive); vendor them from their own repository", config.VendorDir)
		}
		if err := config.ValidToolsetName(set); err != nil {
			return nil, err
		}
		ents := bySet[set]
		if len(ents) == 0 {
			return nil, fmt.Errorf("%s has no tool set %q at %s (it has: %s)", req.Repo, set, short(commit), strings.Join(keys(bySet), ", "))
		}
		top := map[string]bool{}
		var manifest *string
		for _, e := range ents {
			if err := checkPath(e.rest); err != nil {
				return nil, fmt.Errorf("%s/%s/%s: %v", config.ToolsDir, set, e.rest, err)
			}
			if e.typ != "blob" {
				return nil, fmt.Errorf("%s/%s/%s is a %s; only plain files can be vendored", config.ToolsDir, set, e.rest, e.typ)
			}
			mode := os.FileMode(0o644)
			switch e.mode {
			case "100644":
			case "100755":
				mode = 0o755
			default:
				return nil, fmt.Errorf("%s/%s/%s has mode %s (a symlink?); only plain files can be vendored", config.ToolsDir, set, e.rest, e.mode)
			}
			content, err := up.Git(ctx, "cat-file", "blob", e.obj)
			if err != nil {
				return nil, err
			}
			if total += len(content); total > MaxBytes {
				return nil, fmt.Errorf("the tool sets come to more than %d bytes; vendor fewer of them", MaxBytes)
			}
			if !strings.Contains(e.rest, "/") {
				top[e.rest] = true
				if e.rest == config.Manifest {
					manifest = &content
				}
			}
			p.Files = append(p.Files, File{Path: path.Join(Dir(req.Alias), set, e.rest), Content: content, Mode: mode})
		}
		if _, err := config.Entrypoint(func(n string) bool { return top[n] }); err != nil {
			return nil, fmt.Errorf("upstream tool set %s %v", set, err)
		}
		if manifest != nil {
			if _, err := config.ParseManifest([]byte(*manifest), set); err != nil {
				return nil, fmt.Errorf("upstream tool set %s: %s: %v", set, config.Manifest, err)
			}
		}
		p.Lock.Toolsets = append(p.Lock.Toolsets, set)
	}
	p.Files = append(p.Files, File{Path: Dir(req.Alias) + "/" + config.VendorLock, Content: p.Lock.Render(req.Alias), Mode: 0o644})
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return p, nil
}

// checkPath refuses a path from the upstream's tree that could land
// outside the tool set's directory, or in a .git, when written out. git
// itself never makes such a tree, but a crafted one could.
func checkPath(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") || strings.ContainsAny(seg, "\\\x00") {
			return errors.New("unsafe path in the upstream tree")
		}
	}
	return nil
}

// Render is the lock file's contents.
func (l Lock) Render(alias string) string {
	b, _ := yaml.Marshal(l)
	return "# Written by silo vendor. Do not edit: update with\n" +
		"#   silo vendor --update " + alias + " [@<ref>]\n" + string(b)
}

// Apply replaces the alias's directory in the checkout with the plan's
// files.
func Apply(checkout string, p *Plan) error {
	dir := filepath.Join(checkout, filepath.FromSlash(Dir(p.Alias)))
	tmp := dir + ".new"
	os.RemoveAll(tmp)
	for _, f := range p.Files {
		rel := strings.TrimPrefix(f.Path, Dir(p.Alias)+"/")
		full := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(f.Content), f.Mode); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// Stale lists the checkout-relative paths now under the alias's
// directory that the plan does not have, so would delete.
func Stale(checkout string, p *Plan) ([]string, error) {
	want := map[string]bool{}
	for _, f := range p.Files {
		want[f.Path] = true
	}
	var out []string
	root := filepath.Join(checkout, filepath.FromSlash(Dir(p.Alias)))
	err := filepath.WalkDir(root, func(full string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(checkout, full)
		if rel = filepath.ToSlash(rel); !want[rel] {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Summary describes a plan in a line.
func (p *Plan) Summary() string {
	return fmt.Sprintf("%s from %s@%s (commit %s) as %s", strings.Join(p.Lock.Toolsets, ", "), p.Lock.Repo, p.Lock.Ref, short(p.Lock.Commit), p.Alias)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// NewRequest builds a vendor request for checkout: a new alias from repo,
// or (update) an existing one from its lock, with ref and toolsets
// overriding the lock's when given.
func NewRequest(checkout, repo, ref, alias string, toolsets []string, update bool) (Request, error) {
	if err := CheckAlias(checkout, alias); err != nil {
		return Request{}, err
	}
	req := Request{Repo: repo, Ref: ref, Alias: alias, Toolsets: toolsets}
	if update {
		lock, err := ReadLock(checkout, alias)
		if err != nil {
			return req, err
		}
		if repo != "" && repo != lock.Repo {
			return req, fmt.Errorf("%s is vendored from %s, not %s; vendor %s under a new alias instead", alias, lock.Repo, repo, repo)
		}
		req.Repo = lock.Repo
		if req.Ref == "" {
			req.Ref = lock.Ref
		}
		if len(req.Toolsets) == 0 {
			req.Toolsets = lock.Toolsets
		}
		return req, nil
	}
	if repo == "" {
		return req, fmt.Errorf("repo is required (or update, to re-vendor %s)", alias)
	}
	if Exists(checkout, alias) {
		return req, fmt.Errorf("something is already vendored as %s; update it, or pick another alias", alias)
	}
	return req, nil
}

// ParseSpec splits "<repo>[@<ref>]".
func ParseSpec(spec string) (repo, ref string) {
	repo, ref, _ = strings.Cut(spec, "@")
	return repo, ref
}
