package vendor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/testutil"
	"gopkg.in/yaml.v3"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	cfg      *config.Config
	bare     string
	checkout string
}

func newFixture(t *testing.T) *fixture {
	base := t.TempDir()
	bare := testutil.Remote(t, base, "team/shared", map[string]string{
		".silo/tools/tickets/start.sh":         "exec ./tickets \"$1\"\n",
		".silo/tools/tickets/lib/util.sh":      "util v1\n",
		".silo/tools/clock/start.py":           "print('clock v1')\n",
		".silo/tools/clock/toolset.yaml":       "description: clock\n",
		".silo/tools/vendor/far/x/start.sh":    "exit 0\n",
		".silo/tools/vendor/far/VENDOR.lock":   "repo: far\n",
		".silo/tools/README.md":                "not a tool set\n",
		".silo/prompt.md":                      "upstream prompt\n",
		".silo/tools/broken/start.sh":          "exit 0\n",
		".silo/tools/broken/start.py":          "pass\n",
		".silo/tools/badmeta/start.sh":         "exit 0\n",
		".silo/tools/badmeta/toolset.yaml":     "run: [x]\n",
		".silo/tools/tickets/docs/README.md":   "tickets docs\n",
		".silo/tools/clock/subdir/start.sh.md": "not the entry point\n",
	})
	checkout := t.TempDir()
	testutil.Git(t, checkout, "init", "-q", "-b", "main")
	cfg := &config.Config{Repos: config.Repos{Root: filepath.Join(base, "co"), GitBase: base + "/"}, Push: config.Push{Remote: "origin"}}
	return &fixture{t: t, ctx: context.Background(), cfg: cfg, bare: bare, checkout: checkout}
}

func (f *fixture) build(repo, ref, alias string, sets []string, update bool) (*Plan, error) {
	req, err := NewRequest(f.checkout, repo, ref, alias, sets, update)
	if err != nil {
		return nil, err
	}
	up, err := gitrepo.Open(f.ctx, f.cfg, req.Repo)
	if err != nil {
		return nil, err
	}
	return Build(f.ctx, up, req)
}

func (f *fixture) lock(alias string) *Lock {
	f.t.Helper()
	l, err := ReadLock(f.checkout, alias)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func paths(p *Plan) string {
	var out []string
	for _, f := range p.Files {
		out = append(out, f.Path)
	}
	return strings.Join(out, " ")
}

func TestVendorAndUpdate(t *testing.T) {
	f := newFixture(t)
	head := strings.TrimSpace(testutil.Git(t, f.bare, "rev-parse", "main"))
	p, err := f.build("team/shared.git", "", "acme", []string{"tickets", "clock"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := ".silo/tools/vendor/acme/VENDOR.lock .silo/tools/vendor/acme/clock/start.py .silo/tools/vendor/acme/clock/subdir/start.sh.md " +
		".silo/tools/vendor/acme/clock/toolset.yaml .silo/tools/vendor/acme/tickets/docs/README.md .silo/tools/vendor/acme/tickets/lib/util.sh " +
		".silo/tools/vendor/acme/tickets/start.sh"
	if got := paths(p); got != want {
		t.Errorf("files =\n%s\nwant\n%s", got, want)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "not transitive") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if err := Apply(f.checkout, p); err != nil {
		t.Fatal(err)
	}
	l := f.lock("acme")
	if l.Repo != "team/shared.git" || l.Ref != "main" || l.Commit != head || strings.Join(l.Toolsets, ",") != "clock,tickets" {
		t.Errorf("lock = %+v", l)
	}
	data, _ := os.ReadFile(filepath.Join(f.checkout, ".silo/tools/vendor/acme/VENDOR.lock"))
	if !strings.HasPrefix(string(data), "# Written by silo vendor") || !strings.Contains(string(data), "silo vendor --update acme") {
		t.Errorf("lock file = %s", data)
	}
	var raw map[string]any
	yaml.Unmarshal(data, &raw)
	if len(raw) != 4 {
		t.Errorf("lock keys = %v", raw)
	}
	if got, _ := os.ReadFile(filepath.Join(f.checkout, ".silo/tools/vendor/acme/tickets/lib/util.sh")); string(got) != "util v1\n" {
		t.Errorf("util.sh = %q", got)
	}
	// The vendored sets are what the host loads, by key.
	keys, _ := config.RepoToolsets(f.checkout)
	if strings.Join(keys, " ") != "vendor/acme/clock vendor/acme/tickets" {
		t.Errorf("keys = %v", keys)
	}

	if _, err := f.build("team/shared.git", "", "acme", nil, false); err == nil || !strings.Contains(err.Error(), "already vendored as acme") {
		t.Errorf("re-vendor without update: %v", err)
	}

	// Upstream moves on; an update follows the locked ref and keeps the
	// locked sets.
	testutil.PushFiles(t, f.bare, "main", map[string]string{".silo/tools/tickets/lib/util.sh": "util v2\n"})
	newHead := strings.TrimSpace(testutil.Git(t, f.bare, "rev-parse", "main"))
	p, err = f.build("", "", "acme", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := Stale(f.checkout, p)
	if len(stale) != 0 {
		t.Errorf("stale = %v", stale)
	}
	Apply(f.checkout, p)
	if l := f.lock("acme"); l.Commit != newHead || l.Ref != "main" || strings.Join(l.Toolsets, ",") != "clock,tickets" {
		t.Errorf("updated lock = %+v", l)
	}
	if got, _ := os.ReadFile(filepath.Join(f.checkout, ".silo/tools/vendor/acme/tickets/lib/util.sh")); string(got) != "util v2\n" {
		t.Errorf("util.sh after update = %q", got)
	}

	// Pinning to the first commit, with fewer sets: the dropped set's
	// files are stale.
	p, err = f.build("", head, "acme", []string{"tickets"}, true)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ = Stale(f.checkout, p)
	if strings.Join(stale, " ") != ".silo/tools/vendor/acme/clock/start.py .silo/tools/vendor/acme/clock/subdir/start.sh.md .silo/tools/vendor/acme/clock/toolset.yaml" {
		t.Errorf("stale = %v", stale)
	}
	Apply(f.checkout, p)
	if l := f.lock("acme"); l.Commit != head || l.Ref != head || strings.Join(l.Toolsets, ",") != "tickets" {
		t.Errorf("pinned lock = %+v", l)
	}
	if _, err := os.Stat(filepath.Join(f.checkout, ".silo/tools/vendor/acme/clock")); !os.IsNotExist(err) {
		t.Error("dropped set left behind")
	}
	// A later update stays on the pinned commit.
	p, _ = f.build("", "", "acme", nil, true)
	if p.Lock.Commit != head {
		t.Errorf("update left the pin: %s", p.Lock.Commit)
	}
}

func TestVendorErrors(t *testing.T) {
	f := newFixture(t)
	testutil.WriteFiles(t, f.checkout, map[string]string{".silo/tools/local/start.sh": "exit 0\n"})
	for name, tc := range map[string]struct {
		repo, ref, alias string
		sets             []string
		update           bool
		want             string
	}{
		"no such set":     {"team/shared.git", "", "a", []string{"nope"}, false, `no tool set "nope"`},
		"transitive":      {"team/shared.git", "", "a", []string{"vendor"}, false, "not transitive"},
		"both entrypoint": {"team/shared.git", "", "a", []string{"broken"}, false, "upstream tool set broken has both start.sh and start.py"},
		"old manifest":    {"team/shared.git", "", "a", []string{"badmeta"}, false, `"run" is no longer a toolset.yaml setting`},
		"alias clash":     {"team/shared.git", "", "local", nil, false, "also a tool set in .silo/tools/"},
		"reserved alias":  {"team/shared.git", "", "silo", nil, false, "reserved"},
		"bad alias":       {"team/shared.git", "", "A.B", nil, false, "alias"},
		"no repo":         {"", "", "a", nil, false, "repo is required"},
		"bad ref":         {"team/shared.git", "nope", "a", []string{"tickets"}, false, `no branch, tag or commit "nope"`},
		"option ref":      {"team/shared.git", "--upload-pack=x", "a", []string{"tickets"}, false, "may not start with '-'"},
		"update missing":  {"", "", "a", nil, true, "nothing is vendored as \"a\""},
		"escaping repo":   {"../x", "", "a", nil, false, "may not contain"},
	} {
		_, err := f.build(tc.repo, tc.ref, tc.alias, tc.sets, tc.update)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestParseSpec(t *testing.T) {
	if r, ref := ParseSpec("team/tools@v1.2"); r != "team/tools" || ref != "v1.2" {
		t.Errorf("got %q %q", r, ref)
	}
	if r, ref := ParseSpec("team/tools"); r != "team/tools" || ref != "" {
		t.Errorf("got %q %q", r, ref)
	}
}

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"start.sh", "lib/a.py", ".hidden/x"} {
		if err := checkPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"../x", "a/../../x", "a//b", "./x", ".git/hooks/pre-commit", "a/.GIT/x", `a\..\x`, "a\x00b"} {
		if checkPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
