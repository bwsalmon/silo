package grainfiles

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Clean(filepath.Join("..", ".."))
}

func TestExamplesGrainfiles(t *testing.T) {
	root := repoRoot(t)
	examplesDir := filepath.Join(root, "examples", "grainfiles")

	// 1. Full tree lint on examples/grainfiles must pass with zero findings.
	findings, err := LintTree(examplesDir)
	if err != nil {
		t.Fatalf("LintTree(examples/grainfiles): %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("expected 0 findings on examples/grainfiles, got %v", findings)
	}

	// 2. Test widgets/2026-10-16-widget-schema-v2.task.md
	v2Path := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-10-16-widget-schema-v2.task.md")
	v2, err := ParseFile(v2Path)
	if err != nil {
		t.Fatalf("ParseFile(v2): %v", err)
	}
	if v2.Kind != KindTask {
		t.Errorf("v2.Kind = %q, want %q", v2.Kind, KindTask)
	}
	if v2.Name != "2026-10-16-widget-schema-v2" {
		t.Errorf("v2.Name = %q, want 2026-10-16-widget-schema-v2", v2.Name)
	}
	if v2.Title != "Add version header to the widget binary schema" {
		t.Errorf("v2.Title = %q", v2.Title)
	}
	if v2.Requestor != "Bruce Salmon <bsalmon@google.com>" {
		t.Errorf("v2.Requestor = %q", v2.Requestor)
	}
	if v2.Recipient != "https://github.com/acme/grain-eu" {
		t.Errorf("v2.Recipient = %q", v2.Recipient)
	}
	if v2.Draft {
		t.Errorf("v2.Draft = true, want false")
	}
	wantCreated := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if !v2.CreatedAt.Equal(wantCreated) {
		t.Errorf("v2.CreatedAt = %v, want %v", v2.CreatedAt, wantCreated)
	}
	if !v2.NotBefore.IsZero() {
		t.Errorf("v2.NotBefore = %v, want zero", v2.NotBefore)
	}
	if !strings.Contains(v2.Body, "Update `src/widget.go`") {
		t.Errorf("unexpected v2.Body: %q", v2.Body)
	}

	if err := v2.Resolve("https://github.com/acme/widgets", "refs/heads/main", ".grain/tasks/2026-10-16-widget-schema-v2.task.md"); err != nil {
		t.Fatalf("v2.Resolve: %v", err)
	}
	if v2.URL != "https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.md" {
		t.Errorf("v2.URL = %q", v2.URL)
	}
	wantV2Refs := []string{
		"https://github.com/acme/widgets/blob/main/docs/widget-schema.md#schema-version-header",
		"https://github.com/acme/widgets/blob/main/src/widget.go",
	}
	if !reflect.DeepEqual(v2.References, wantV2Refs) {
		t.Errorf("v2.References = %v, want %v", v2.References, wantV2Refs)
	}
	sameRefs, crossRefs := v2.AuthorizedReferences("https://github.com/acme/widgets")
	if !reflect.DeepEqual(sameRefs, wantV2Refs) || len(crossRefs) != 0 {
		t.Errorf("v2.AuthorizedReferences = (%v, %v), want (%v, nil)", sameRefs, crossRefs, wantV2Refs)
	}
	if !v2.AuthorizesReference("https://github.com/acme/widgets", "/docs/widget-schema.md") {
		t.Errorf("expected v2 to authorize /docs/widget-schema.md")
	}
	if !v2.AuthorizesReference("https://github.com/acme/widgets", "/src/widget.go") {
		t.Errorf("expected v2 to authorize /src/widget.go")
	}
	if v2.AuthorizesReference("https://github.com/acme/widgets", "/src/other.go") {
		t.Errorf("expected v2 not to authorize /src/other.go")
	}

	// 3. Test widgets/2026-10-16-widget-schema-v2-migration.task.md (no requestor, depends_on, RFC 3339 not_before)
	migPath := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-10-16-widget-schema-v2-migration.task.md")
	mig, err := ParseFile(migPath)
	if err != nil {
		t.Fatalf("ParseFile(mig): %v", err)
	}
	if mig.Requestor != "" {
		t.Errorf("mig.Requestor = %q, want empty", mig.Requestor)
	}
	wantMigNB := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	if !mig.NotBefore.Equal(wantMigNB) {
		t.Errorf("mig.NotBefore = %v, want %v", mig.NotBefore, wantMigNB)
	}
	if mig.ActiveAt(time.Date(2026, 10, 25, 8, 59, 59, 0, time.UTC)) {
		t.Errorf("expected mig to be inactive before not_before")
	}
	if !mig.ActiveAt(wantMigNB) {
		t.Errorf("expected mig to be active at not_before")
	}
	if err := mig.Resolve("https://github.com/acme/widgets", "main", ".grain/tasks/2026-10-16-widget-schema-v2-migration.task.md"); err != nil {
		t.Fatalf("mig.Resolve: %v", err)
	}
	wantMigDeps := []string{
		"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.md",
	}
	if !reflect.DeepEqual(mig.DependsOn, wantMigDeps) {
		t.Errorf("mig.DependsOn = %v, want %v", mig.DependsOn, wantMigDeps)
	}

	// 4. Test widgets/2026-11-01-drop-widget-v1-support.task.md (draft: true, YYYY-MM-DD not_before)
	dropPath := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-11-01-drop-widget-v1-support.task.md")
	drop, err := ParseFile(dropPath)
	if err != nil {
		t.Fatalf("ParseFile(drop): %v", err)
	}
	if !drop.Draft {
		t.Errorf("drop.Draft = false, want true")
	}
	wantDropNB := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !drop.NotBefore.Equal(wantDropNB) {
		t.Errorf("drop.NotBefore = %v, want %v", drop.NotBefore, wantDropNB)
	}
	if drop.ActiveAt(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("draft task must never be active even after not_before")
	}
	if drop.AuthorizesReference("https://github.com/acme/widgets", "/src/widget.go") {
		t.Errorf("draft task must not authorize reference fetches")
	}

	// 5. Test widgets/schema-security-review.process.md (gate: true, pull_request & push triggers)
	secPath := filepath.Join(examplesDir, "widgets", ".grain", "processes", "schema-security-review.process.md")
	sec, err := ParseFile(secPath)
	if err != nil {
		t.Fatalf("ParseFile(sec): %v", err)
	}
	if sec.Kind != KindProcess {
		t.Errorf("sec.Kind = %q, want %q", sec.Kind, KindProcess)
	}
	if !sec.Gate {
		t.Errorf("sec.Gate = false, want true")
	}
	if !sec.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"src/widget.go", "docs/widget-schema.md"},
	}) {
		t.Errorf("expected pull_request on main touching src/widget.go to match")
	}
	if sec.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"src/README.md"},
	}) {
		t.Errorf("expected pull_request touching only src/README.md to be ignored by **/*.md")
	}
	if !sec.MatchesEvent(ForgeEvent{
		Source:       TriggerPush,
		Branch:       "refs/heads/main",
		ChangedPaths: []string{"src/README.md"},
	}) {
		t.Errorf("expected push on main touching src/README.md to match (push has no ignore)")
	}
	if sec.MatchesEvent(ForgeEvent{
		Source:       TriggerMerge,
		Branch:       "main",
		ChangedPaths: []string{"src/widget.go"},
	}) {
		t.Errorf("expected merge not to match when on.merge is not set")
	}

	// 6. Test widgets/release-cut-check.process.md (scenario + manual)
	relPath := filepath.Join(examplesDir, "widgets", ".grain", "processes", "release-cut-check.process.md")
	rel, err := ParseFile(relPath)
	if err != nil {
		t.Fatalf("ParseFile(rel): %v", err)
	}
	if rel.On.Manual == nil {
		t.Errorf("expected rel.On.Manual != nil")
	}
	if rel.On.Scenario == nil || !strings.Contains(rel.On.Scenario.Description, "release/*") {
		t.Errorf("unexpected rel.On.Scenario: %+v", rel.On.Scenario)
	}

	// 7. Test grain-eu/processes/qualification/smoke.process.md & report.process.md
	smokePath := filepath.Join(examplesDir, "grain-eu", "processes", "qualification", "smoke.process.md")
	smoke, err := ParseFile(smokePath)
	if err != nil {
		t.Fatalf("ParseFile(smoke): %v", err)
	}
	if smoke.On.Schedule == nil || smoke.On.Schedule.Cron != "0 2 * * *" || smoke.On.Schedule.Timezone != "Europe/Berlin" {
		t.Errorf("unexpected smoke.On.Schedule: %+v", smoke.On.Schedule)
	}
	if err := smoke.Resolve("https://github.com/acme/grain-eu", "main", "processes/qualification/smoke.process.md"); err != nil {
		t.Fatalf("smoke.Resolve: %v", err)
	}
	// smoke.process.md lives in grain-eu, but its references point to acme/widgets (cross-repo!)
	smokeSame, smokeCross := smoke.AuthorizedReferences("https://github.com/acme/grain-eu")
	if len(smokeSame) != 0 || len(smokeCross) != 2 {
		t.Errorf("smoke.AuthorizedReferences(grain-eu) = (same=%v, cross=%v), want 0 same and 2 cross", smokeSame, smokeCross)
	}
	if smoke.AuthorizesReference("https://github.com/acme/grain-eu", "https://github.com/acme/widgets/tree/main/src") {
		t.Errorf("grainfile in grain-eu must not authorize cross-origin fetch from acme/widgets")
	}

	repPath := filepath.Join(examplesDir, "grain-eu", "processes", "qualification", "report.process.md")
	rep, err := ParseFile(repPath)
	if err != nil {
		t.Fatalf("ParseFile(rep): %v", err)
	}
	if err := rep.Resolve("https://github.com/acme/grain-eu", "main", "processes/qualification/report.process.md"); err != nil {
		t.Fatalf("rep.Resolve: %v", err)
	}
	wantSmokeURL := []string{"https://github.com/acme/grain-eu/blob/main/processes/qualification/smoke.process.md"}
	if !reflect.DeepEqual(rep.TriggerWith, wantSmokeURL) {
		t.Errorf("rep.TriggerWith = %v, want %v", rep.TriggerWith, wantSmokeURL)
	}
	if !reflect.DeepEqual(rep.DependsOn, wantSmokeURL) {
		t.Errorf("rep.DependsOn = %v, want %v", rep.DependsOn, wantSmokeURL)
	}
}

func TestBrokenFixturesParityWithLinter(t *testing.T) {
	root := repoRoot(t)
	badDir := filepath.Join(root, "tools", "grainfiles", "testdata", "bad")
	entries, err := os.ReadDir(badDir)
	if err != nil {
		t.Fatalf("ReadDir(testdata/bad): %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no fixtures found in %s", badDir)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseDir := filepath.Join(badDir, entry.Name())
		expectBytes, err := os.ReadFile(filepath.Join(caseDir, "expect.txt"))
		if err != nil {
			t.Fatalf("ReadFile(%s/expect.txt): %v", entry.Name(), err)
		}
		wantCodes := strings.Fields(string(expectBytes))
		sort.Strings(wantCodes)
		wantCodes = slices.Compact(wantCodes)

		findings, err := LintTree(caseDir)
		if err != nil {
			t.Fatalf("LintTree(%s): %v", entry.Name(), err)
		}
		var gotCodes []string
		for _, f := range findings {
			gotCodes = append(gotCodes, f.Code)
		}
		sort.Strings(gotCodes)
		gotCodes = slices.Compact(gotCodes)

		if !reflect.DeepEqual(gotCodes, wantCodes) {
			t.Errorf("%s: got codes %v, want %v (findings: %v)", entry.Name(), gotCodes, wantCodes, findings)
		}
	}
}

func TestParseAndValidateFields(t *testing.T) {
	t.Run("task with compact UTC stem and x_ extensions and unknown field warning", func(t *testing.T) {
		raw := []byte(`---
title: Compact UTC task
recipient: /deployments/eu
requestor: https://github.com/acme/team-a
created_at: 2026-10-16T13:58:30.123Z
not_before: 2026-10-20
draft: false
trigger_with: ./lead.process.md
depends_on:
  - /processes/lead.process.md
references:
  - ./design.md#overview
x_custom_priority: 1
x_tags: ["security", "v2"]
unknown_field: ignored
---
Execute the task.
`)
		g, err := Parse("tasks/20261016T135830Z-compact.task.md", raw)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if g.Kind != KindTask || g.Name != "20261016T135830Z-compact" {
			t.Errorf("Kind=%q Name=%q", g.Kind, g.Name)
		}
		if len(g.Warnings) != 1 || g.Warnings[0].Code != CodeUnknownField {
			t.Errorf("Warnings = %v, want 1 GF022 warning", g.Warnings)
		}
		if g.Extensions["x_custom_priority"] != int64(1) {
			t.Errorf("x_custom_priority = %v (%T)", g.Extensions["x_custom_priority"], g.Extensions["x_custom_priority"])
		}
		if !reflect.DeepEqual(g.TriggerWith, []string{"./lead.process.md"}) {
			t.Errorf("TriggerWith = %v", g.TriggerWith)
		}
		wantNB := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		if !g.NotBefore.Equal(wantNB) {
			t.Errorf("NotBefore = %v, want %v", g.NotBefore, wantNB)
		}
	})

	t.Run("process with string scenario and schedule default UTC timezone", func(t *testing.T) {
		raw := []byte(`---
title: Nightly check
recipient: https://github.com/acme/grain-eu
on:
  schedule:
    cron: "30 4 * * 1-5"
  scenario: "When cutting a release branch"
  merge:
    branches: ["main", "release/*"]
---
Check the release branch.
`)
		g, err := Parse("processes/nightly.process.md", raw)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if g.On.Schedule == nil || g.On.Schedule.Timezone != "UTC" {
			t.Errorf("Schedule = %+v, want Timezone=UTC", g.On.Schedule)
		}
		if g.On.Scenario == nil || g.On.Scenario.Description != "When cutting a release branch" {
			t.Errorf("Scenario = %+v", g.On.Scenario)
		}
		if g.On.Merge == nil || !reflect.DeepEqual(g.On.Merge.Branches, []string{"main", "release/*"}) {
			t.Errorf("Merge = %+v", g.On.Merge)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		cases := []struct {
			name string
			path string
			body string
			code string
		}{
			{
				name: "uppercase stem",
				path: "tasks/BadName.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeBadName,
			},
			{
				name: "invalid compact UTC in stem",
				path: "tasks/20260230T990000Z-bad.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeBadName,
			},
			{
				name: "unknown suffix",
				path: "notes/foo.widget.md",
				body: "---\ntitle: T\n---\n",
				code: CodeUnknownSuffix,
			},
			{
				name: "LFS pointer",
				path: "tasks/a.task.md",
				body: "version https://git-lfs.github.com/spec/v1\noid sha256:1234\nsize 100\n",
				code: CodeMalformedFile,
			},
			{
				name: "BOM in file",
				path: "tasks/a.task.md",
				body: "\xef\xbb\xbf---\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeMalformedFile,
			},
			{
				name: "NUL byte in file",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\x00\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeMalformedFile,
			},
			{
				name: "duplicate key in nested mapping",
				path: "processes/p.process.md",
				body: "---\ntitle: P\nrecipient: https://github.com/acme/grain-eu\non:\n  manual: {}\n  manual: {}\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "YAML merge key",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\n<<: *base\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "YAML explicit tag",
				path: "tasks/a.task.md",
				body: "---\ntitle: !!str T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "multiline title",
				path: "tasks/a.task.md",
				body: "---\ntitle: |\n  Line 1\n  Line 2\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "recipient with anchor",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu#sec\n---\n",
				code: CodeMalformedRef,
			},
			{
				name: "recipient is blob URL",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu/blob/main/README.md\n---\n",
				code: CodeWrongTargetKind,
			},
			{
				name: "depends_on points to tree URL",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu\ndepends_on:\n  - https://github.com/acme/widgets/tree/main/src\n---\n",
				code: CodeWrongTargetKind,
			},
			{
				name: "relative reference climbs above repo root",
				path: "tasks/a.task.md",
				body: "---\ntitle: T\nrecipient: https://github.com/acme/grain-eu\nreferences:\n  - ../../secret.md\n---\n",
				code: CodeEscapesRepo,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := Parse(tc.path, []byte(tc.body))
				if err == nil {
					t.Fatalf("expected error with code %s, got nil", tc.code)
				}
				errs, ok := err.(Errors)
				if !ok || len(errs) == 0 {
					t.Fatalf("expected Errors, got %T: %v", err, err)
				}
				found := false
				for _, f := range errs {
					if f.Code == tc.code {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("got errors %v, want code %s", errs, tc.code)
				}
			})
		}
	})
}

func TestResolutionAndSameRepoAuthorization(t *testing.T) {
	raw := []byte(`---
title: Multi-ref task
recipient: ../deployments/grain-eu
requestor: /teams/platform
depends_on:
  - ./prep.task.md
  - /tasks/shared/init.task.md
  - https://GitHub.com:443/acme/widgets.git/blob/main/.grain/tasks/ext.task.md
trigger_with: ../processes/nightly.process.md
references:
  - ./design.md# framing
  - /docs/arch.md#overview
  - /src/
  - https://github.com/acme/widgets/blob/v1.0/README.md
  - https://github.com/acme/other-repo/blob/main/secret.md
  - https://gitlab.com/acme/group/other/-/tree/main/pkg
---
Prompt body.
`)
	// Fix anchor without space
	raw = []byte(strings.ReplaceAll(string(raw), "# framing", "#framing"))

	g, err := Parse(".grain/tasks/work.task.md", raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	err = g.Resolve("https://GitHub.com:443/acme/widgets.git/", "refs/heads/main", ".grain/tasks/work.task.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if g.Repo != "https://github.com/acme/widgets" {
		t.Errorf("g.Repo = %q", g.Repo)
	}
	if g.URL != "https://github.com/acme/widgets/blob/main/.grain/tasks/work.task.md" {
		t.Errorf("g.URL = %q", g.URL)
	}
	if g.Recipient != "https://github.com/acme/widgets/tree/main/.grain/deployments/grain-eu" {
		t.Errorf("g.Recipient = %q", g.Recipient)
	}
	if g.Requestor != "https://github.com/acme/widgets/tree/main/teams/platform" {
		t.Errorf("g.Requestor = %q", g.Requestor)
	}
	wantDeps := []string{
		"https://github.com/acme/widgets/blob/main/.grain/tasks/prep.task.md",
		"https://github.com/acme/widgets/blob/main/tasks/shared/init.task.md",
		"https://github.com/acme/widgets/blob/main/.grain/tasks/ext.task.md",
	}
	if !reflect.DeepEqual(g.DependsOn, wantDeps) {
		t.Errorf("g.DependsOn = %v, want %v", g.DependsOn, wantDeps)
	}
	wantTW := []string{
		"https://github.com/acme/widgets/blob/main/.grain/processes/nightly.process.md",
	}
	if !reflect.DeepEqual(g.TriggerWith, wantTW) {
		t.Errorf("g.TriggerWith = %v, want %v", g.TriggerWith, wantTW)
	}

	sameRepo, crossRepo := g.AuthorizedReferences("https://github.com/acme/widgets")
	wantSame := []string{
		"https://github.com/acme/widgets/blob/main/.grain/tasks/design.md#framing",
		"https://github.com/acme/widgets/blob/main/docs/arch.md#overview",
		"https://github.com/acme/widgets/tree/main/src",
		"https://github.com/acme/widgets/blob/v1.0/README.md",
	}
	wantCross := []string{
		"https://github.com/acme/other-repo/blob/main/secret.md",
		"https://gitlab.com/acme/group/other/-/tree/main/pkg",
	}
	if !reflect.DeepEqual(sameRepo, wantSame) {
		t.Errorf("sameRepo = %v, want %v", sameRepo, wantSame)
	}
	if !reflect.DeepEqual(crossRepo, wantCross) {
		t.Errorf("crossRepo = %v, want %v", crossRepo, wantCross)
	}

	// Check individual fetch_reference authorization:
	// 1. Exact file in same repo (with or without anchor)
	if !g.AuthorizesReference("https://github.com/acme/widgets", "/docs/arch.md") {
		t.Errorf("expected /docs/arch.md to be authorized")
	}
	// 2. File inside authorized folder `/src/`
	if !g.AuthorizesReference("https://github.com/acme/widgets", "/src/sub/deep.go") {
		t.Errorf("expected /src/sub/deep.go under /src/ tree reference to be authorized")
	}
	// 3. Unlisted file in same repo
	if g.AuthorizesReference("https://github.com/acme/widgets", "/unrelated/file.go") {
		t.Errorf("expected /unrelated/file.go to be denied")
	}
	// 4. Listed file in cross-origin repo (Repo C) must be denied on Repo A's authority!
	if g.AuthorizesReference("https://github.com/acme/widgets", "https://github.com/acme/other-repo/blob/main/secret.md") {
		t.Errorf("expected cross-repo reference to be denied on source repo's authority")
	}

	// Reject climbing above root during Resolve:
	gEscape := &Grainfile{
		Kind:       KindTask,
		Title:      "Escape",
		Recipient:  "https://github.com/acme/grain-eu",
		References: []string{"../../outside.md"},
	}
	if err := gEscape.Resolve("https://github.com/acme/widgets", "main", "tasks/a.task.md"); err == nil {
		t.Errorf("expected Resolve to reject path climbing above repository root")
	} else if f, ok := err.(Finding); !ok || f.Code != CodeEscapesRepo {
		t.Errorf("expected GF036 Finding, got %T: %v", err, err)
	}
}

func TestTriggerMatchingAndDeferral(t *testing.T) {
	raw := []byte(`---
title: Gated PR and merge process
recipient: https://github.com/acme/grain-eu
not_before: "2026-10-25T09:00:00Z"
gate: true
on:
  pull_request:
    branches: ["main", "release/*"]
    paths: ["hub/**", "cmd/**"]
    ignore: ["**/*.md", "hub/grainfiles/testdata/**"]
  merge:
    branches: ["main"]
---
Run gate check.
`)
	g, err := Parse(".grain/processes/gate.process.md", raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	beforeNB := time.Date(2026, 10, 25, 8, 59, 59, 0, time.UTC)
	atNB := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	afterNB := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

	// 1. Before not_before -> must not match
	if g.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"hub/server/server.go"},
		At:           beforeNB,
	}) {
		t.Errorf("expected process not to fire before not_before")
	}

	// 2. At not_before -> matches
	if !g.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"hub/server/server.go"},
		At:           atNB,
	}) {
		t.Errorf("expected process to fire at not_before")
	}

	// 3. Release branch glob match + mixed ignored and non-ignored paths
	if !g.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "refs/heads/release/v1.2",
		ChangedPaths: []string{"hub/README.md", "cmd/silo/main.go"},
		At:           afterNB,
	}) {
		t.Errorf("expected release/v1.2 touching cmd/silo/main.go to match")
	}

	// 4. All changed paths match ignore globs
	if g.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"hub/README.md", "hub/grainfiles/testdata/f.txt"},
		At:           afterNB,
	}) {
		t.Errorf("expected PR where all matched paths are ignored not to fire")
	}

	// 5. Branch-only filter on merge
	if !g.MatchesEvent(ForgeEvent{
		Source: TriggerMerge,
		Branch: "main",
		At:     afterNB,
	}) {
		t.Errorf("expected merge on main to match branch-only filter")
	}
	if g.MatchesEvent(ForgeEvent{
		Source: TriggerMerge,
		Branch: "develop",
		At:     afterNB,
	}) {
		t.Errorf("expected merge on develop not to match")
	}

	// 6. Draft process never fires
	g.Draft = true
	if g.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Branch:       "main",
		ChangedPaths: []string{"hub/server/server.go"},
		At:           afterNB,
	}) {
		t.Errorf("expected draft: true process never to fire")
	}
}

func TestParseRefAndGlobForms(t *testing.T) {
	goodRefs := []struct {
		url        string
		wantRepo   string
		wantPath   string
		wantRef    string
		wantMode   string
		wantAnchor string
	}{
		{"https://github.com/acme/grain-eu", "https://github.com/acme/grain-eu", "", "", "", ""},
		{"https://github.com/acme/deployments/tree/main/grain-eu", "https://github.com/acme/deployments", "grain-eu", "main", "tree", ""},
		{"https://github.com/acme/widgets/blob/main/docs/widget-schema.md#req-1", "https://github.com/acme/widgets", "docs/widget-schema.md", "main", "blob", "req-1"},
		{"https://gitlab.com/acme/group/widgets/-/blob/main/docs/widget-schema.md", "https://gitlab.com/acme/group/widgets", "docs/widget-schema.md", "main", "blob", ""},
		{"https://gitlab.com/acme/group/widgets/-/tree/main/src", "https://gitlab.com/acme/group/widgets", "src", "main", "tree", ""},
		{"https://gerrit.googlesource.com/widgets/+/main/docs/widget-schema.md", "https://gerrit.googlesource.com/widgets", "docs/widget-schema.md", "main", "", ""},
	}
	for _, tc := range goodRefs {
		r, err := ParseRef(tc.url)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.url, err)
		}
		if r.Form != RefRemote || r.Repo != tc.wantRepo || r.Path != tc.wantPath || r.GitRef != tc.wantRef || r.Mode != tc.wantMode || r.Anchor != tc.wantAnchor {
			t.Errorf("ParseRef(%q) = %+v, want repo=%q path=%q ref=%q mode=%q anchor=%q",
				tc.url, r, tc.wantRepo, tc.wantPath, tc.wantRef, tc.wantMode, tc.wantAnchor)
		}
	}

	badRefs := []string{
		"git::https://github.com/acme/widgets.git//docs/d.md",
		"http://github.com/acme/widgets/blob/main/docs/d.md",
		"ssh://git@github.com/acme/widgets.git",
		"git@github.com:acme/widgets.git",
		"https://github.com/acme/widgets/blob/main/docs/d.md?ref=main",
		"https://github.com/acme/widgets//docs/d.md",
		"https://user:pass@github.com/acme/widgets",
		"https://github.com/acme/widgets/blob/main/",
	}
	for _, bad := range badRefs {
		_, err := ParseRef(bad)
		if err == nil {
			t.Errorf("ParseRef(%q) succeeded, want GF035 error", bad)
			continue
		}
		if f, ok := err.(Finding); !ok || f.Code != CodeMalformedRef {
			t.Errorf("ParseRef(%q) = %v, want GF035", bad, err)
		}
	}

	globs := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"src/**", "src/widget.go", true},
		{"src/**", "src/a/b/c.go", true},
		{"src/**", "docs/widget.go", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/widget-schema.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "src/widget.go", false},
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"release/*", "release/v1.0", true},
		{"release/*", "release/v1.0/rc1", false},
	}
	for _, g := range globs {
		if got := MatchGlob(g.pattern, g.value); got != g.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", g.pattern, g.value, got, g.want)
		}
	}
}
