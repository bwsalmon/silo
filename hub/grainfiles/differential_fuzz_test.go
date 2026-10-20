package grainfiles

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type diffTemplateCase struct {
	Name        string            `json:"name"`
	Path        string            `json:"path"`
	Raw         string            `json:"raw"`
	Vars        map[string]string `json:"vars"`
	SkipBodyCmp bool              `json:"skip_body_cmp,omitempty"`
}

type diffRefCase struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

type diffTreeCase struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
	Vars  map[string]string `json:"vars,omitempty"`
}

type pyTemplateResult struct {
	OK           bool     `json:"ok"`
	Code         string   `json:"code,omitempty"`
	Rendered     string   `json:"rendered,omitempty"`
	TemplateVars []string `json:"template_vars,omitempty"`
}

type pyRefResult struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Form   string `json:"form,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path,omitempty"`
	GitRef string `json:"gitref,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type pyFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
}

type pyBatchRequest struct {
	Templates []diffTemplateCase `json:"templates"`
	Refs      []diffRefCase      `json:"refs"`
	TreeDirs  []TreeDirInput     `json:"tree_dirs"`
}

type TreeDirInput struct {
	Name string            `json:"name"`
	Dir  string            `json:"dir"`
	Vars map[string]string `json:"vars,omitempty"`
}

type pyBatchResponse struct {
	Templates map[string]pyTemplateResult `json:"templates"`
	Refs      map[string]pyRefResult      `json:"refs"`
	Trees     map[string][]pyFinding      `json:"trees"`
}

const pyBatchRunnerScript = `
import json, pathlib, sys
import grainlint

req = json.load(sys.stdin)
resp = {"templates": {}, "refs": {}, "trees": {}}

for tc in (req.get("templates") or []):
    try:
        rendered, tvars = grainlint.render_template(tc["raw"], tc.get("path") or "<template>", tc.get("vars") or {})
        if tc.get("skip_body_cmp"):
            rendered = f"<len={len(rendered)}>"
        resp["templates"][tc["name"]] = {
            "ok": True,
            "rendered": rendered,
            "template_vars": list(tvars),
        }
    except grainlint.Finding as f:
        resp["templates"][tc["name"]] = {
            "ok": False,
            "code": f.code,
        }

for rc in (req.get("refs") or []):
    try:
        r = grainlint.parse_ref(rc["ref"])
        resp["refs"][rc["name"]] = {
            "ok": True,
            "form": r.form or "",
            "repo": r.repo or "",
            "path": r.path or "",
            "gitref": r.gitref or "",
            "anchor": r.anchor or "",
            "mode": r.mode or "",
        }
    except grainlint.Finding as f:
        resp["refs"][rc["name"]] = {
            "ok": False,
            "code": f.code,
        }

for tr in (req.get("tree_dirs") or []):
    fs = grainlint.lint(pathlib.Path(tr["dir"]), cli_vars=tr.get("vars") or {})
    resp["trees"][tr["name"]] = [
        {"code": f.code, "severity": f.severity, "path": f.path}
        for f in fs
    ]

json.dump(resp, sys.stdout)
`

func ensurePythonGrainlint(t testing.TB) string {
	t.Helper()
	scriptDir, err := filepath.Abs("../../tools/grainfiles")
	if err != nil {
		t.Fatalf("resolving tools/grainfiles path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scriptDir, "grainlint.py")); err != nil {
		t.Fatalf("missing tools/grainfiles/grainlint.py: %v", err)
	}
	checkCmd := exec.Command("python3", "-c", "import yaml, jsonschema, jinja2")
	if err := checkCmd.Run(); err != nil {
		if os.Getenv("SILO_REQUIRE_GRAINLINT") == "1" {
			t.Fatalf("python3 dependencies (yaml, jsonschema, jinja2) required when SILO_REQUIRE_GRAINLINT=1: %v", err)
		}
		t.Skipf("skipping differential test: python3 dependencies unavailable: %v", err)
	}
	return scriptDir
}

func runPythonDifferentialBatch(t testing.TB, scriptDir string, req pyBatchRequest) pyBatchResponse {
	t.Helper()
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling batch request: %v", err)
	}
	cmd := exec.Command("python3", "-c", pyBatchRunnerScript)
	cmd.Dir = scriptDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+scriptDir)
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python batch runner failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var resp pyBatchResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decoding python batch response: %v\nraw:\n%s", err, stdout.String())
	}
	return resp
}

// TestDifferentialPythonGoCorpus cross-checks tools/grainfiles/grainlint.py and
// Go hub/grainfiles across a wide corpus of:
//  1. Sandboxed Jinja2 templates (filters, conditionals, parenthesized expressions,
//     nested loops, loop.* variables, whitespace trimming, comments, undefined
//     variables, template_vars AST extraction order, GF034 forbidden tags, GF033
//     syntax errors, and GF006 MaxFileBytes expansion caps).
//  2. Reference parsing (GitHub/GitLab/Gitiles /blob/, /tree/, /+/ URLs, encoded
//     path traversal %2e/%2f/%5c/%00, double-slash separators, dot/dot-dot climbing).
//  3. YAML 1.2 Core Schema corner cases and multi-file tree linting (booleans vs
//     plain yes/no/on/off strings, timestamps, duplicate keys, empty keys,
//     anchors/aliases/tags GF004 vs quoted strings/comments, CRLF line endings,
//     empty bodies GF005, .task.grain.md-only depends_on GF032, and cycle detection GF034).
func TestDifferentialPythonGoCorpus(t *testing.T) {
	scriptDir := ensurePythonGrainlint(t)

	templateCases := []diffTemplateCase{
		{
			Name: "all-filters-chained",
			Path: "deploy.task.grain.md.jinja2",
			Raw: `---
grainfile: v1
title: "{{ raw_title | default('  Deploy_Widget_Service  ', true) | trim | lower | replace('_', '-') | upper }}"
recipient: "https://github.com/acme/{{ target_env | d('staging') | lower }}"
---
Deploy service to {{ region | default(fallback_region | lower, boolean = true) }}.
`,
			Vars: map[string]string{
				"raw_title":       "",
				"target_env":      "PROD",
				"region":          "",
				"fallback_region": "EU-WEST-1",
			},
		},
		{
			Name: "if-elif-else-and-parenthesized-conditions",
			Path: "cond.task.grain.md.jinja2",
			Raw: `---
grainfile: v1
title: "Conditional rollout"
recipient: https://github.com/acme/grain-eu
{% if (env == 'prod' or env == 'staging') and not (dry_run == 'true') %}
not_before: "2026-11-01T00:00:00Z"
{% elif (env in ['dev', 'qa']) andnot_var != 'skip' %}
draft: true
{% else %}
draft: false
{% endif %}
---
{% if not(disabled) and (tier not in ['legacy', 'deprecated']) -%}
Active tier: {{ tier | default('standard') }}
{%- else -%}
Disabled
{%- endif %}
`,
			Vars: map[string]string{
				"env":        "prod",
				"dry_run":    "false",
				"andnot_var": "go",
				"tier":       "premium",
			},
		},
		{
			Name: "nested-loops-and-loop-vars",
			Path: "matrix.process.grain.md.jinja2",
			Raw: `---
grainfile: v1
title: "Matrix verification {{ suite | default('core') }}"
on:
  push:
    branches:
{%- for br in ['main', 'release/v1'] %}
      - "{{ br }}"
{%- endfor %}
---
Matrix combinations:
{% for dc in ['eu', 'us'] -%}
DC {{ loop.index }}/{{ loop.length }} (first={{ loop.first }}, last={{ loop.last }}): {{ dc | upper }}
{% for shard in ['s1', 's2'] -%}
  - [{{ loop.index0 }}] {{ dc }}-{{ shard }}{% if not loop.last %},{% endif %}
{% endfor -%}
After inner loop:idx={{ loop.index }},last={{ loop.last }}
{% endfor -%}
Undefined var renders empty: [{{ missing_var }}]
`,
			Vars: map[string]string{
				"suite": "nightly",
			},
		},
		{
			Name: "comments-and-whitespace-trimming",
			Path: "comments.task.grain.md.jinja2",
			Raw:  "---\r\ntitle: \"Comment test\"\r\nrecipient: https://github.com/acme/grain-eu\r\n---\r\nHello {# untrimmed comment #}{%- if flag %}World{% endif %}\r\nTrimmed {#- trimmed comment -#}   Tight\r\n",
			Vars: map[string]string{
				"flag": "1",
			},
		},
		{
			Name: "template-vars-extraction-order",
			Path: "vars.task.grain.md.jinja2",
			Raw: `---
grainfile: v1
title: "{{ zeta }} and {{ alpha | default(beta) }}"
recipient: https://github.com/acme/grain-eu
---
{% if gamma == delta %}
{% for item in epsilon %}
{{ item }} - {{ omega }}
{% endfor %}
{% endif %}
`,
			Vars: map[string]string{},
		},
		{
			Name: "forbidden-include-gf034",
			Path: "bad-include.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: Bad\nrecipient: https://github.com/acme/grain-eu\n---\n{% include 'other.md' %}\n",
		},
		{
			Name: "forbidden-extends-gf034",
			Path: "bad-extends.task.grain.md.jinja2",
			Raw:  "{% extends 'base.md' %}\n---\ngrainfile: v1\ntitle: Bad\n---\nBody\n",
		},
		{
			Name: "forbidden-import-gf034",
			Path: "bad-import.task.grain.md.jinja2",
			Raw:  "{% import 'macros.md' as m %}\n---\ngrainfile: v1\ntitle: Bad\n---\nBody\n",
		},
		{
			Name: "forbidden-from-import-gf034",
			Path: "bad-from.task.grain.md.jinja2",
			Raw:  "{% from 'macros.md' import helper %}\n---\ngrainfile: v1\ntitle: Bad\n---\nBody\n",
		},
		{
			Name: "syntax-unclosed-expr-gf033",
			Path: "unclosed-expr.task.grain.md.jinja2",
			Raw:  "---\ntitle: \"{{ unclosed\"\n---\nBody\n",
		},
		{
			Name: "syntax-unclosed-if-gf033",
			Path: "unclosed-if.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: T\n---\n{% if x %}Body\n",
		},
		{
			Name: "syntax-unclosed-for-gf033",
			Path: "unclosed-for.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: T\n---\n{% for x in ['a'] %}Body\n",
		},
		{
			Name: "syntax-elif-after-else-gf033",
			Path: "elif-after-else.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: T\n---\n{% if x %}A{% else %}B{% elif y %}C{% endif %}\n",
		},
		{
			Name: "syntax-string-concat-operator-gf033",
			Path: "concat.task.grain.md.jinja2",
			Raw:  "---\ntitle: \"{{ 'a' + 'b' }}\"\n---\nBody\n",
		},
		{
			Name: "syntax-unknown-filter-gf033",
			Path: "unknown-filter.task.grain.md.jinja2",
			Raw:  "---\ntitle: \"{{ x | nonexistent_filter }}\"\n---\nBody\n",
		},
		{
			Name: "forbidden-call-range-gf033",
			Path: "call-range.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: \"{{ range(100) }}\"\n---\nBody\n",
		},
		{
			Name: "forbidden-getattr-gf033",
			Path: "getattr.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: \"{{ user.email }}\"\n---\nBody\n",
		},
		{
			Name: "forbidden-getitem-gf033",
			Path: "getitem.task.grain.md.jinja2",
			Raw:  "---\ngrainfile: v1\ntitle: \"{{ user['email'] }}\"\n---\nBody\n",
		},
		{
			Name: "forbidden-set-assign-gf033",
			Path: "assign.task.grain.md.jinja2",
			Raw:  "{% set x = 'val' %}---\ngrainfile: v1\ntitle: \"{{ x }}\"\n---\nBody\n",
		},
		{
			Name: "expansion-cap-exceeds-1mib-gf006",
			Path: "bomb.task.grain.md.jinja2",
			Raw: "---\ngrainfile: v1\ntitle: Bomb\nrecipient: https://github.com/acme/grain-eu\n---\n" +
				"{% for a in ['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'] %}" +
				"{% for b in ['0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'] %}" +
				strings.Repeat("X", 4200) +
				"{% endfor %}{% endfor %}\n",
		},
		{
			Name:        "raw-input-exceeds-1mib-gf006",
			Path:        "huge-input.task.grain.md.jinja2",
			Raw:         "---\ngrainfile: v1\ntitle: Huge\nrecipient: https://github.com/acme/grain-eu\n---\n" + strings.Repeat("A", MaxFileBytes+64),
			SkipBodyCmp: true,
		},
	}

	refCases := []diffRefCase{
		{"github-repo-root", "https://github.com/acme/widgets"},
		{"github-repo-dotgit", "https://github.com/acme/widgets.git"},
		{"github-blob-with-anchor", "https://github.com/acme/widgets/blob/main/docs/spec.md#section-1"},
		{"github-tree-dir", "https://github.com/acme/widgets/tree/v1.2.3/src/pkg"},
		{"gitlab-nested-blob", "https://gitlab.com/acme/platform/sub/widgets/-/blob/main/docs/spec.md#overview"},
		{"gitlab-nested-tree", "https://gitlab.com/acme/platform/widgets/-/tree/release/src"},
		{"gitiles-ref", "https://gerrit.googlesource.com/widgets/+/main/docs/spec.md"},
		{"rooted-with-anchor", "/docs/widget-schema.md#schema-version-header"},
		{"rooted-folder", "/src/pkg/"},
		{"relative-dot-slash", "./helper.task.grain.md"},
		{"relative-parent-climbing", "../shared/spec.md#intro"},
		{"bad-encoded-dot-relative", "./%2e%2e/secret.md"},
		{"bad-encoded-dot-upper", "/docs/%2E%2E/secret.md"},
		{"bad-encoded-slash-lower", "/docs/a%2fb.md"},
		{"bad-encoded-slash-remote", "https://github.com/acme/widgets/blob/main/docs%2fsecret.md"},
		{"bad-encoded-backslash", "https://github.com/acme/widgets/blob/main/docs%5csecret.md"},
		{"bad-encoded-nul", "/docs/file%00.md"},
		{"bad-double-slash-relative", "./a//b.md"},
		{"bad-double-slash-rooted", "/docs//spec.md"},
		{"bad-double-slash-remote", "https://github.com/acme/widgets//blob/main/docs/spec.md"},
		{"bad-remote-dotdot-escapes-gf036", "https://github.com/acme/widgets/blob/main/docs/../../secret.md"},
		{"bad-remote-dot-segment-gf035", "https://github.com/acme/widgets/blob/main/./docs/spec.md"},
		{"bad-http-scheme", "http://github.com/acme/widgets/blob/main/docs/spec.md"},
		{"bad-git-getter", "git::https://github.com/acme/widgets.git//docs/spec.md"},
		{"bad-ssh-scheme", "git@github.com:acme/widgets.git"},
		{"bad-query-param", "https://github.com/acme/widgets/blob/main/docs/spec.md?raw=1"},
		{"bad-credentials", "https://user:pass@github.com/acme/widgets"},
		{"bad-dotless-host", "https://localhost/acme/widgets"},
		{"bad-single-segment-repo", "https://github.com/onlyowner"},
		{"bad-gitlab-single-seg", "https://gitlab.com/onlyowner/-/blob/main/a.md"},
		{"bad-blob-trailing-slash", "https://github.com/acme/widgets/blob/main/docs/"},
		{"bad-anchor-uppercase", "/docs/spec.md#Invalid_Anchor"},
		{"bad-empty-anchor", "/docs/spec.md#"},
		{"bad-backslash", `docs\spec.md`},
	}

	treeCases := []diffTreeCase{
		{
			Name: "valid-yaml12-scalars-quoted-specials-and-templates",
			Vars: map[string]string{"env": "prod", "team": "grain-eu"},
			Files: map[string]string{
				"docs/spec.md": "# Spec\n\n## Schema Version Header\nDetails.\n",
				".grain/tasks/2026-10-16-prep.task.grain.md": `---
grainfile: v1
title: yes
recipient: https://github.com/acme/grain-eu
requestor: "Bruce *ptr &ref !bang <<: #not-a-comment"
draft: false
not_before: "2026-10-16T09:00:00Z"
created_at: "2026-10-16"
references:
  - /docs/spec.md#schema-version-header
# Comment with *alias &anchor !tag and <<: merge
---
Prepare the schema migration.
`,
				".grain/tasks/2026-10-17-follow.task.grain.md": "---\r\ngrainfile: v1\r\ntitle: 'no'\r\nrecipient: https://github.com/acme/grain-eu\r\ndepends_on:\r\n  - ./2026-10-16-prep.task.grain.md\r\n---\r\nFollow up after prep.\r\n",
				".grain/processes/review.process.grain.md.jinja2": `---
grainfile: v1
title: "Review on {{ env | upper }}"
recipient: "https://github.com/acme/{{ team }}"
gate: true
on:
  pull_request:
    branches: ["main", "release/*"]
    paths: ["src/**"]
    ignore: ["**/*.md"]
  schedule:
    - cron: "0 9 * * 1-5"
---
Run automated review for {{ env }}.
`,
			},
		},
		{
			Name: "yaml11-bool-strings-rejected-for-draft-gf020",
			Files: map[string]string{
				"tasks/bad-bool.task.grain.md": `---
grainfile: v1
title: Unquoted yes is a string in YAML 1.2
recipient: https://github.com/acme/grain-eu
draft: yes
---
Body.
`,
			},
		},
		{
			Name: "yaml-duplicate-key-gf004",
			Files: map[string]string{
				"tasks/dup.task.grain.md": `---
grainfile: v1
title: First
recipient: https://github.com/acme/grain-eu
title: Second
---
Body.
`,
			},
		},
		{
			Name: "yaml-empty-and-non-string-keys",
			Files: map[string]string{
				"processes/empty-key.process.grain.md": `---
grainfile: v1
title: Empty key in flow map
on: {: 1}
---
Body.
`,
				"processes/null-key.process.grain.md": `---
grainfile: v1
title: Null key in flow map
on: {null: 1}
---
Body.
`,
				"processes/bool-key.process.grain.md": `---
grainfile: v1
title: Bool key in block map
on:
  true: 1
---
Body.
`,
				"processes/int-key.process.grain.md": `---
grainfile: v1
title: Int key in block map
on:
  123: 1
---
Body.
`,
			},
		},
		{
			Name: "yaml-anchor-and-tag-gf004",
			Files: map[string]string{
				"tasks/anchor.task.grain.md": `---
grainfile: v1
title: &a Anchor
recipient: *a
---
Body.
`,
				"tasks/flow-tag.task.grain.md": `---
grainfile: v1
title: Tag in flow list
recipient: https://github.com/acme/grain-eu
references: [!!str /docs/a.md]
---
Body.
`,
				"tasks/doc-end-marker.task.grain.md": `---
grainfile: v1
title: Document end marker inside front matter
recipient: https://github.com/acme/grain-eu
...
---
Body.
`,
			},
		},
		{
			Name: "empty-body-warning-gf005",
			Files: map[string]string{
				"tasks/empty-body.task.grain.md": "---\ngrainfile: v1\ntitle: Empty body\nrecipient: https://github.com/acme/grain-eu\n---\n   \n",
			},
		},
		{
			Name: "depends-on-wrong-target-kinds-gf032",
			Files: map[string]string{
				"docs/readme.md": "# Readme\n",
				"processes/p.process.grain.md": `---
grainfile: v1
title: Process P
on:
  manual: {}
---
Process body.
`,
				"tasks/tmpl.task.grain.md.jinja2": `---
grainfile: v1
title: "{{ name }}"
recipient: https://github.com/acme/grain-eu
---
Template task body.
`,
				"tasks/bad-deps.task.grain.md": `---
grainfile: v1
title: Task with invalid depends_on targets
recipient: https://github.com/acme/grain-eu
depends_on:
  - ../processes/p.process.grain.md
  - ./tmpl.task.grain.md.jinja2
  - ../docs/readme.md
  - https://github.com/acme/other/blob/main/processes/remote.process.grain.md
---
Task body.
`,
			},
		},
		{
			Name: "depends-on-3-node-cycle-gf034",
			Files: map[string]string{
				"tasks/a.task.grain.md": "---\ngrainfile: v1\ntitle: A\nrecipient: https://github.com/acme/grain-eu\ndepends_on:\n  - ./b.task.grain.md\n---\nBody A.\n",
				"tasks/b.task.grain.md": "---\ngrainfile: v1\ntitle: B\nrecipient: https://github.com/acme/grain-eu\ndepends_on:\n  - ./c.task.grain.md\n---\nBody B.\n",
				"tasks/c.task.grain.md": "---\ngrainfile: v1\ntitle: C\nrecipient: https://github.com/acme/grain-eu\ndepends_on:\n  - ./a.task.grain.md\n---\nBody C.\n",
			},
		},
		{
			Name: "url-capabilities-and-with-args",
			Files: map[string]string{
				"tasks/use-cap.task.grain.md": `---
grainfile: v1
title: Task requesting capability URLs
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/self-debug
  - capability: https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key
    with:
      account: infra-staging
  - capability: https://github.com/bwsalmon/grain/capabilities/github#git-repo
    with:
      repo: bwsalmon/grain
      mode: write
      base: main
---
Run with capabilities.
`,
				"processes/use-cap.process.grain.md": `---
grainfile: v1
title: Process requesting capability URL
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/self-debug
when:
  manual: {}
---
Run process with capability.
`,
			},
		},
		{
			Name: "capabilities-invalid-url-refs-and-unregistered-capability-suffix",
			Files: map[string]string{
				"capabilities/legacy.capability.grain.md": `---
grainfile: v1
title: Unregistered capability suffix
---
Body.
`,
				"tasks/t.task.grain.md": `---
grainfile: v1
title: T
capabilities:
  - /.grain/capabilities/gcp-key.capability.grain.md
  - capability: https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key
    with: not-a-map
  - capability: https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key
    extra: 1
---
Body.
`,
			},
		},
		{
			Name: "encoded-traversal-in-references-gf035-and-climbing-gf036",
			Files: map[string]string{
				"tasks/traversal.task.grain.md": `---
grainfile: v1
title: Traversal check
recipient: https://github.com/acme/grain-eu
references:
  - ./%2e%2e/secret.md
  - ../../outside.md
  - https://github.com/acme/other/blob/main/../escape.md
---
Body.
`,
			},
		},
		{
			Name: "canonical-deployments-and-when-workflow-schedule-tz-parity",
			Files: map[string]string{
				"tasks/deploy-target.task.grain.md": `---
grainfile: v1
title: Canonical deployment recipient and requestor
recipient: /deployments/prod-runner-1
requestor: /deployments/sec-ops
not_before: "2026-10-25T09:00:00+02:00"
---
`,
				"processes/ci-triage.process.grain.md": `---
grainfile: v1
title: Workflow and multi-timezone process
gate: true
when:
  schedule:
    - cron: "0 9 * * 1-5"
      timezone: "America/New_York"
    - cron: "30 14 * * 1-5"
      timezone: "Europe/London"
  push:
    repos: ["acme/widgets", "https://github.com/acme/payments"]
    branches: ["main", "release/*"]
    paths: ["src/auth/", "/db/migrations", "https://github.com/acme/payments/tree/main/api"]
    ignore: ["docs/", "**/*.md"]
  workflow:
    workflows: ["grain CI", "e2e-*", ".github/workflows/release.yml"]
    events: ["triggered", "succeeded", "failed"]
    repos: ["acme/widgets"]
    branches: ["main"]
---
`,
			},
		},
		{
			Name: "invalid-from-template-climbing-and-when-ignore-hazards",
			Files: map[string]string{
				"tasks/bad-ft-climb.task.grain.md": `---
grainfile: v1
title: Climbing from_template remote URL
from_template: https://github.com/acme/widgets/blob/main/../../outside.task.grain.md.jinja2
---
`,
				"processes/bad-when-ignore.process.grain.md": `---
grainfile: v1
title: Invalid ignore URL and timezone abbreviation
when:
  schedule:
    cron: "0 9 * * *"
    timezone: "PST"
  push:
    branches: ["main"]
    ignore:
      - "https://github.com/acme/widgets/blob/main/docs/a.md"
      - "docs/%2e%2e/secret.md"
---
`,
			},
		},
	}

	tempRoot := t.TempDir()
	var treeInputs []TreeDirInput
	for _, tc := range treeCases {
		dir := filepath.Join(tempRoot, tc.Name)
		for rel, content := range tc.Files {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		treeInputs = append(treeInputs, TreeDirInput{
			Name: tc.Name,
			Dir:  dir,
			Vars: tc.Vars,
		})
	}

	pyResp := runPythonDifferentialBatch(t, scriptDir, pyBatchRequest{
		Templates: templateCases,
		Refs:      refCases,
		TreeDirs:  treeInputs,
	})

	// 1. Verify template rendering parity.
	for _, tc := range templateCases {
		t.Run("template/"+tc.Name, func(t *testing.T) {
			pyRes, ok := pyResp.Templates[tc.Name]
			if !ok {
				t.Fatalf("missing python template result for %s", tc.Name)
			}
			goRendered, goVars, goErr := RenderTemplate(tc.Raw, tc.Vars)
			if goVars == nil {
				goVars = []string{}
			}
			if pyRes.TemplateVars == nil {
				pyRes.TemplateVars = []string{}
			}
			if !pyRes.OK {
				if goErr == nil {
					t.Fatalf("Python failed with %s, but Go succeeded with rendered=%q", pyRes.Code, goRendered)
				}
				goCode := firstFindingCode(goErr)
				if goCode != pyRes.Code {
					t.Fatalf("template error code mismatch: Go=%s (%v), Python=%s", goCode, goErr, pyRes.Code)
				}
				return
			}
			if goErr != nil {
				t.Fatalf("Python succeeded, but Go failed with %v", goErr)
			}
			if !tc.SkipBodyCmp && goRendered != pyRes.Rendered {
				t.Errorf("rendered output mismatch:\n--- Go ---\n%q\n--- Python ---\n%q", goRendered, pyRes.Rendered)
			}
			if !reflect.DeepEqual(goVars, pyRes.TemplateVars) {
				t.Errorf("template_vars mismatch: Go=%v, Python=%v", goVars, pyRes.TemplateVars)
			}
		})
	}

	// 2. Verify reference parsing parity.
	for _, rc := range refCases {
		t.Run("ref/"+rc.Name, func(t *testing.T) {
			pyRes, ok := pyResp.Refs[rc.Name]
			if !ok {
				t.Fatalf("missing python ref result for %s", rc.Name)
			}
			goRef, goErr := ParseRef(rc.Ref)
			if !pyRes.OK {
				if goErr == nil {
					t.Fatalf("ParseRef(%q): Python failed with %s, but Go succeeded with %+v", rc.Ref, pyRes.Code, goRef)
				}
				goCode := firstFindingCode(goErr)
				if goCode != pyRes.Code {
					t.Fatalf("ParseRef(%q) code mismatch: Go=%s (%v), Python=%s", rc.Ref, goCode, goErr, pyRes.Code)
				}
				return
			}
			if goErr != nil {
				t.Fatalf("ParseRef(%q): Python succeeded (%+v), but Go failed with %v", rc.Ref, pyRes, goErr)
			}
			if string(goRef.Form) != pyRes.Form ||
				goRef.Repo != pyRes.Repo ||
				goRef.Path != pyRes.Path ||
				goRef.GitRef != pyRes.GitRef ||
				goRef.Anchor != pyRes.Anchor ||
				goRef.Mode != pyRes.Mode {
				t.Errorf("ParseRef(%q) field mismatch:\nGo:     form=%q repo=%q path=%q ref=%q anchor=%q mode=%q\nPython: form=%q repo=%q path=%q ref=%q anchor=%q mode=%q",
					rc.Ref,
					goRef.Form, goRef.Repo, goRef.Path, goRef.GitRef, goRef.Anchor, goRef.Mode,
					pyRes.Form, pyRes.Repo, pyRes.Path, pyRes.GitRef, pyRes.Anchor, pyRes.Mode)
			}
		})
	}

	// 3. Verify tree linting & YAML 1.2 / DAG parity.
	for _, ti := range treeInputs {
		t.Run("tree/"+ti.Name, func(t *testing.T) {
			pyFindings := pyResp.Trees[ti.Name]
			goFindings, err := LintTreeWithVars(ti.Dir, ti.Vars)
			if err != nil {
				t.Fatalf("LintTreeWithVars(%s): %v", ti.Name, err)
			}
			var goPairs, pyPairs []string
			for _, f := range goFindings {
				goPairs = append(goPairs, f.Code+":"+string(f.Severity))
			}
			for _, f := range pyFindings {
				pyPairs = append(pyPairs, f.Code+":"+f.Severity)
			}
			sort.Strings(goPairs)
			sort.Strings(pyPairs)
			if !reflect.DeepEqual(goPairs, pyPairs) {
				t.Errorf("LintTree(%s) findings mismatch:\nGo:     %v (full: %+v)\nPython: %v (full: %+v)",
					ti.Name, goPairs, goFindings, pyPairs, pyFindings)
			}
		})
	}
}

func firstFindingCode(err error) string {
	if err == nil {
		return ""
	}
	if f, ok := err.(Finding); ok {
		return f.Code
	}
	if errs, ok := err.(Errors); ok && len(errs) > 0 {
		return errs[0].Code
	}
	return "ERR"
}

// FuzzDifferentialGrainfileAndJinja2 fuzzes Go's RenderTemplate, ParseWithVars,
// and ParseRef against Python's grainlint.py on the seed corpus and fuzzed inputs.
func FuzzDifferentialGrainfileAndJinja2(f *testing.F) {
	scriptDir := ensurePythonGrainlint(f)
	seeds := []struct {
		isTemplate bool
		raw        string
		varVal     string
		ref        string
	}{
		{
			isTemplate: true,
			raw:        "---\ntitle: \"{{ title | default('Widget Task', true) | trim }}\"\nrecipient: https://github.com/acme/grain-eu\n---\nRun {{ title | lower }}.\n",
			varVal:     "  Migrate Schema  ",
			ref:        "https://github.com/acme/widgets/blob/main/docs/spec.md#header",
		},
		{
			isTemplate: false,
			raw:        "---\ngrainfile: v1\ntitle: yes\nrecipient: https://github.com/acme/grain-eu\nreferences:\n  - /docs/spec.md\n---\nBody text.\n",
			varVal:     "",
			ref:        "/docs/%2e%2e/secret.md",
		},
		{
			isTemplate: true,
			raw:        "---\ntitle: \"{% if (x == 'a') %}Alpha{% else %}Beta{% endif %}\"\nrecipient: https://github.com/acme/grain-eu\n---\n{% for item in ['1', '2'] %}{{ loop.index }}:{{ item }}\n{% endfor %}\n",
			varVal:     "a",
			ref:        "https://gitlab.com/acme/group/proj/-/tree/main/src",
		},
	}
	for _, s := range seeds {
		f.Add(s.isTemplate, s.raw, s.varVal, s.ref)
	}

	f.Fuzz(func(t *testing.T, isTemplate bool, raw, varVal, ref string) {
		if len(raw) > 64*1024 || len(varVal) > 4096 || len(ref) > 4096 {
			return
		}
		// Always exercise Go parser/renderer/ref parser to check for panics or hangs.
		vars := map[string]string{"title": varVal, "x": varVal}
		path := "fuzz.task.grain.md"
		if isTemplate {
			path = "fuzz.task.grain.md.jinja2"
		}
		_, _ = ParseWithVars(path, []byte(raw), vars)
		_, _ = ParseRef(ref)

		// When running the deterministic seed corpus (or when input is valid UTF-8 without NUL),
		// cross-check template rendering and reference parsing against Python grainlint.py.
		if strings.IndexByte(raw, 0) >= 0 || strings.IndexByte(ref, 0) >= 0 {
			return
		}
		pyResp := runPythonDifferentialBatch(t, scriptDir, pyBatchRequest{
			Templates: []diffTemplateCase{{Name: "t", Path: path, Raw: raw, Vars: vars}},
			Refs:      []diffRefCase{{Name: "r", Ref: ref}},
		})
		if pyT, ok := pyResp.Templates["t"]; ok {
			goOut, goVars, goErr := RenderTemplate(raw, vars)
			if pyT.OK && goErr == nil {
				if goOut != pyT.Rendered {
					t.Fatalf("Fuzz RenderTemplate output mismatch:\nGo:     %q\nPython: %q", goOut, pyT.Rendered)
				}
				if len(goVars) != len(pyT.TemplateVars) {
					t.Fatalf("Fuzz RenderTemplate vars mismatch: Go=%v Python=%v", goVars, pyT.TemplateVars)
				}
			} else if !pyT.OK && goErr != nil {
				if got := firstFindingCode(goErr); got != pyT.Code {
					t.Fatalf("Fuzz RenderTemplate error code mismatch: Go=%s Python=%s", got, pyT.Code)
				}
			}
		}
		if pyR, ok := pyResp.Refs["r"]; ok {
			_, goRefErr := ParseRef(ref)
			if pyR.OK != (goRefErr == nil) {
				t.Fatalf("Fuzz ParseRef(%q) mismatch: Go err=%v, Python=%+v", ref, goRefErr, pyR)
			}
		}
	})
}
