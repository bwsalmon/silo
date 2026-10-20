package grainfiles

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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

	// 2. Test widgets/2026-10-16-widget-schema-v2.task.grain.md
	v2Path := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-10-16-widget-schema-v2.task.grain.md")
	v2, err := ParseFile(v2Path)
	if err != nil {
		t.Fatalf("ParseFile(v2): %v", err)
	}
	if v2.Kind != KindTask {
		t.Errorf("v2.Kind = %q, want %q", v2.Kind, KindTask)
	}
	if v2.Grainfile != "v1" || v2.SpecSummaryPath != "docs/grainfile-v1.md" || v2.SpecPath != "docs/grainfiles-spec.md" {
		t.Errorf("v2 version metadata = (%q, %q, %q)", v2.Grainfile, v2.SpecSummaryPath, v2.SpecPath)
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
	if !v2.NotBefore.IsZero() {
		t.Errorf("v2.NotBefore = %v, want zero", v2.NotBefore)
	}
	if !strings.Contains(v2.Body, "Update `src/widget.go`") {
		t.Errorf("unexpected v2.Body: %q", v2.Body)
	}

	if err := v2.Resolve("https://github.com/acme/widgets", "refs/heads/main", ".grain/tasks/2026-10-16-widget-schema-v2.task.grain.md"); err != nil {
		t.Fatalf("v2.Resolve: %v", err)
	}
	if v2.URL != "https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md" {
		t.Errorf("v2.URL = %q", v2.URL)
	}
	wantV2Refs := []string{
		"https://github.com/acme/widgets/blob/main/docs/widget-schema.md#schema-version-header",
		"https://github.com/acme/widgets/blob/main/src/widget.go",
	}
	if !reflect.DeepEqual(v2.References, wantV2Refs) {
		t.Errorf("v2.References = %v, want %v", v2.References, wantV2Refs)
	}
	wantV2Caps := []CapabilityRef{
		{Capability: "https://github.com/bwsalmon/grain/capabilities/self-debug"},
		{
			Capability: "https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key",
			With: map[string]any{
				"account": "infra-staging",
			},
		},
	}
	if !reflect.DeepEqual(v2.Capabilities, wantV2Caps) {
		t.Errorf("v2.Capabilities = %#v, want %#v", v2.Capabilities, wantV2Caps)
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

	// 3. Test widgets/2026-10-16-widget-schema-v2-migration.task.grain.md (no requestor, depends_on, RFC 3339 not_before)
	migPath := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-10-16-widget-schema-v2-migration.task.grain.md")
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
	if err := mig.Resolve("https://github.com/acme/widgets", "main", ".grain/tasks/2026-10-16-widget-schema-v2-migration.task.grain.md"); err != nil {
		t.Fatalf("mig.Resolve: %v", err)
	}
	wantMigDeps := []string{
		"https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-widget-schema-v2.task.grain.md",
	}
	if !reflect.DeepEqual(mig.DependsOn, wantMigDeps) {
		t.Errorf("mig.DependsOn = %v, want %v", mig.DependsOn, wantMigDeps)
	}

	// 4. Test widgets/2026-11-01-drop-widget-v1-support.task.grain.md (draft: true, YYYY-MM-DD not_before)
	dropPath := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "2026-11-01-drop-widget-v1-support.task.grain.md")
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

	// 5. Test widgets/schema-security-review.process.grain.md (gate: true, pull_request & push triggers)
	secPath := filepath.Join(examplesDir, "widgets", ".grain", "processes", "schema-security-review.process.grain.md")
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
		t.Errorf("expected merge not to match when when.merge is not set")
	}

	// 6. Test widgets/release-cut-check.process.grain.md (scenario + manual)
	relPath := filepath.Join(examplesDir, "widgets", ".grain", "processes", "release-cut-check.process.grain.md")
	rel, err := ParseFile(relPath)
	if err != nil {
		t.Fatalf("ParseFile(rel): %v", err)
	}
	if rel.When.Manual == nil {
		t.Errorf("expected rel.When.Manual != nil")
	}
	if rel.When.Scenario == nil || !strings.Contains(rel.When.Scenario.Description, "release/*") {
		t.Errorf("unexpected rel.When.Scenario: %+v", rel.When.Scenario)
	}

	// 7. Test grain-eu/processes/qualification/smoke.process.grain.md & report.process.grain.md
	smokePath := filepath.Join(examplesDir, "grain-eu", "processes", "qualification", "smoke.process.grain.md")
	smoke, err := ParseFile(smokePath)
	if err != nil {
		t.Fatalf("ParseFile(smoke): %v", err)
	}
	if smoke.When.Schedule == nil || smoke.When.Schedule.Cron != "0 2 * * *" || smoke.When.Schedule.Timezone != "Europe/Berlin" {
		t.Errorf("unexpected smoke.When.Schedule: %+v", smoke.When.Schedule)
	}
	if err := smoke.Resolve("https://github.com/acme/grain-eu", "main", "processes/qualification/smoke.process.grain.md"); err != nil {
		t.Fatalf("smoke.Resolve: %v", err)
	}
	// smoke.process.grain.md lives in grain-eu, but its references point to acme/widgets (cross-repo!)
	smokeSame, smokeCross := smoke.AuthorizedReferences("https://github.com/acme/grain-eu")
	if len(smokeSame) != 0 || len(smokeCross) != 2 {
		t.Errorf("smoke.AuthorizedReferences(grain-eu) = (same=%v, cross=%v), want 0 same and 2 cross", smokeSame, smokeCross)
	}
	if smoke.AuthorizesReference("https://github.com/acme/grain-eu", "https://github.com/acme/widgets/tree/main/src") {
		t.Errorf("grainfile in grain-eu must not authorize cross-origin fetch from acme/widgets")
	}

	repPath := filepath.Join(examplesDir, "grain-eu", "processes", "qualification", "report.process.grain.md")
	rep, err := ParseFile(repPath)
	if err != nil {
		t.Fatalf("ParseFile(rep): %v", err)
	}
	if rep.Recipient != "" {
		t.Errorf("rep.Recipient before Resolve = %q, want empty", rep.Recipient)
	}
	if err := rep.Resolve("https://github.com/acme/grain-eu", "main", "processes/qualification/report.process.grain.md"); err != nil {
		t.Fatalf("rep.Resolve: %v", err)
	}
	if rep.Recipient != "" {
		t.Errorf("rep.Recipient after Resolve = %q, want empty", rep.Recipient)
	}
	if len(rep.DependsOn) != 0 {
		t.Errorf("rep.DependsOn = %v, want empty (depends_on is .task.grain.md-only)", rep.DependsOn)
	}
	wantSmokeRefs := []string{
		"https://github.com/acme/grain-eu/blob/main/processes/qualification/smoke.process.grain.md",
		"https://github.com/acme/widgets/blob/main/docs/widget-schema.md",
	}
	if !reflect.DeepEqual(rep.References, wantSmokeRefs) {
		t.Errorf("rep.References = %v, want %v", rep.References, wantSmokeRefs)
	}

	// Verify the worked .task.grain.md.jinja2 and .process.grain.md.jinja2 examples.
	rolloutTplPath := filepath.Join(examplesDir, "widgets", ".grain", "tasks", "rollout-region.task.grain.md.jinja2")
	rolloutTpl, err := ParseFile(rolloutTplPath)
	if err != nil {
		t.Fatalf("ParseFile(rollout-region.task.grain.md.jinja2): %v", err)
	}
	if !rolloutTpl.IsTemplate {
		t.Errorf("rolloutTpl.IsTemplate = false, want true")
	}
	wantTaskVars := []string{"dry_run", "environment", "recipient_url", "region", "requestor"}
	if !reflect.DeepEqual(rolloutTpl.TemplateVars, wantTaskVars) {
		t.Errorf("rolloutTpl.TemplateVars = %v, want %v", rolloutTpl.TemplateVars, wantTaskVars)
	}
	if rolloutTpl.Kind != KindTask || rolloutTpl.Recipient != "https://github.com/acme/grain-eu" {
		t.Errorf("unexpected default-rendered rolloutTpl: kind=%q recipient=%q", rolloutTpl.Kind, rolloutTpl.Recipient)
	}
	rolloutBytes, err := os.ReadFile(rolloutTplPath)
	if err != nil {
		t.Fatal(err)
	}
	renderedRollout, err := ParseWithVars(".grain/tasks/rollout-region.task.grain.md.jinja2", rolloutBytes, map[string]string{
		"region":        "us-east-1",
		"environment":   "staging",
		"recipient_url": "https://github.com/acme/grain-us-east-1",
		"requestor":     "https://github.com/acme/sre",
	})
	if err != nil {
		t.Fatalf("ParseWithVars(rollout-region): %v", err)
	}
	if renderedRollout.Title != "Roll out widget schema v2 to US-EAST-1 (staging)" ||
		renderedRollout.Recipient != "https://github.com/acme/grain-us-east-1" ||
		renderedRollout.Requestor != "https://github.com/acme/sre" {
		t.Errorf("unexpected renderedRollout: %+v", renderedRollout)
	}

	auditTplPath := filepath.Join(examplesDir, "widgets", ".grain", "processes", "branch-audit.process.grain.md.jinja2")
	auditTpl, err := ParseFile(auditTplPath)
	if err != nil {
		t.Fatalf("ParseFile(branch-audit.process.grain.md.jinja2): %v", err)
	}
	if !auditTpl.IsTemplate || auditTpl.Kind != KindProcess {
		t.Errorf("unexpected auditTpl: IsTemplate=%v Kind=%q", auditTpl.IsTemplate, auditTpl.Kind)
	}
	wantProcVars := []string{"enforce_gate", "target_branch"}
	if !reflect.DeepEqual(auditTpl.TemplateVars, wantProcVars) {
		t.Errorf("auditTpl.TemplateVars = %v, want %v", auditTpl.TemplateVars, wantProcVars)
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

func TestPythonGrainlintCrossCheckParity(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("SILO_REQUIRE_GRAINLINT") != "" {
			t.Fatalf("python3 required by SILO_REQUIRE_GRAINLINT: %v", err)
		}
		t.Skip("python3 not installed")
	}
	if err := exec.Command("python3", "-c", "import yaml, jsonschema").Run(); err != nil {
		if os.Getenv("SILO_REQUIRE_GRAINLINT") != "" {
			t.Fatalf("python3 yaml/jsonschema required by SILO_REQUIRE_GRAINLINT: %v", err)
		}
		t.Skipf("python3 yaml/jsonschema not installed: %v", err)
	}

	root := repoRoot(t)
	script := filepath.Join(root, "tools", "grainfiles", "grainlint.py")
	examplesDir := filepath.Join(root, "examples", "grainfiles")
	badDir := filepath.Join(root, "tools", "grainfiles", "testdata", "bad")

	// 1. Both Python grainlint.py and Go LintTree must accept examples/grainfiles with 0 errors and 0 warnings.
	pyOut, err := exec.Command("python3", script, examplesDir).CombinedOutput()
	if err != nil {
		t.Fatalf("grainlint.py(examples/grainfiles) failed: %v\n%s", err, pyOut)
	}
	if !strings.Contains(string(pyOut), "0 error(s), 0 warning(s)") {
		t.Fatalf("grainlint.py(examples/grainfiles) unexpected output:\n%s", pyOut)
	}
	goFindings, err := LintTree(examplesDir)
	if err != nil {
		t.Fatalf("LintTree(examples/grainfiles): %v", err)
	}
	if len(goFindings) != 0 {
		t.Fatalf("LintTree(examples/grainfiles) findings = %v, want none", goFindings)
	}

	// Verify every individual valid .task.grain.md / .process.grain.md file in examples/grainfiles parses cleanly in Go.
	var validCount int
	err = filepath.WalkDir(examplesDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		if strings.HasSuffix(path, ".task.grain.md") || strings.HasSuffix(path, ".process.grain.md") || strings.HasSuffix(path, ".task.md") || strings.HasSuffix(path, ".process.md") {
			validCount++
			gf, pErr := ParseFile(path)
			if pErr != nil {
				t.Errorf("ParseFile(%s) failed on valid example: %v", path, pErr)
			} else if len(gf.Warnings) > 0 {
				t.Errorf("ParseFile(%s) produced unexpected warnings: %v", path, gf.Warnings)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(examples/grainfiles): %v", err)
	}
	if validCount == 0 {
		t.Fatal("expected at least one valid grainfile in examples/grainfiles")
	}

	// 2. Every broken fixture in tools/grainfiles/testdata/bad must produce the exact same
	// (Code, Severity) pairs in both Python grainlint.py and Go LintTree.
	lineRe := regexp.MustCompile(`^(GF\d{3})\s+(error|warning)\s+`)
	entries, err := os.ReadDir(badDir)
	if err != nil {
		t.Fatalf("ReadDir(testdata/bad): %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseName := entry.Name()
		caseDir := filepath.Join(badDir, caseName)

		pyOut, _ := exec.Command("python3", script, caseDir).CombinedOutput()
		var pyCodeSev []string
		for _, line := range strings.Split(string(pyOut), "\n") {
			if m := lineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				pyCodeSev = append(pyCodeSev, m[1]+":"+m[2])
			}
		}
		sort.Strings(pyCodeSev)
		pyCodeSev = slices.Compact(pyCodeSev)

		findings, err := LintTree(caseDir)
		if err != nil {
			t.Fatalf("LintTree(%s): %v", caseName, err)
		}
		var goCodeSev []string
		for _, f := range findings {
			sev := string(f.Severity)
			if sev == "" {
				sev = string(SeverityError)
			}
			goCodeSev = append(goCodeSev, f.Code+":"+sev)
		}
		sort.Strings(goCodeSev)
		goCodeSev = slices.Compact(goCodeSev)

		if len(pyCodeSev) == 0 {
			t.Errorf("%s: Python grainlint.py emitted no findings (output: %s)", caseName, pyOut)
		}
		if !reflect.DeepEqual(goCodeSev, pyCodeSev) {
			t.Errorf("%s parity mismatch: Go LintTree = %v, Python grainlint.py = %v\nPython output:\n%s",
				caseName, goCodeSev, pyCodeSev, pyOut)
		}
	}
}

func TestParseAndValidateFields(t *testing.T) {
	t.Run("task with default recipient, grainfile v1, x_ extensions, and unknown field warning", func(t *testing.T) {
		raw := []byte(`---
grainfile: v1
title: Compact task
requestor: https://github.com/acme/team-a
not_before: 2026-10-20
draft: false
depends_on:
  - /tasks/lead.task.grain.md
references:
  - ./design.md#overview
x_custom_priority: 1
x_tags: ["security", "v2"]
unknown_field: ignored
---
Execute the task.
`)
		g, err := Parse("tasks/20261016-compact.task.grain.md", raw)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if g.Kind != KindTask || g.Name != "20261016-compact" {
			t.Errorf("Kind=%q Name=%q", g.Kind, g.Name)
		}
		if g.Grainfile != "v1" || g.SpecSummaryPath != "docs/grainfile-v1.md" || g.SpecPath != "docs/grainfiles-spec.md" {
			t.Errorf("version metadata = (%q, %q, %q)", g.Grainfile, g.SpecSummaryPath, g.SpecPath)
		}
		if g.Recipient != "/" {
			t.Errorf("Recipient = %q, want \"/\"", g.Recipient)
		}
		if len(g.Warnings) != 1 || g.Warnings[0].Code != CodeUnknownField {
			t.Errorf("Warnings = %v, want 1 GF022 warning", g.Warnings)
		}
		if g.Extensions["x_custom_priority"] != int64(1) {
			t.Errorf("x_custom_priority = %v (%T)", g.Extensions["x_custom_priority"], g.Extensions["x_custom_priority"])
		}
		wantNB := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		if !g.NotBefore.Equal(wantNB) {
			t.Errorf("NotBefore = %v, want %v", g.NotBefore, wantNB)
		}
		prompt := FormatExecutionPrompt(g, []MaterializedReference{
			{URL: "https://github.com/acme/widgets/blob/a1b2c3d4/tasks/design.md#overview", CommitSHA: "a1b2c3d4", Content: "# Design\n"},
		}, &MaterializedPRDiff{
			RepoURL:       "https://github.com/acme/widgets",
			BaseCommitSHA: "a1b2c3d4",
			HeadCommitSHA: "e5f6a7b8",
			Diff:          "diff --git a/widget.go b/widget.go",
		})
		if !strings.Contains(prompt, "<grainfile_prompt") || !strings.Contains(prompt, "<grainfile_reference") || !strings.Contains(prompt, "<pull_request_diff") {
			t.Errorf("unexpected FormatExecutionPrompt output:\n%s", prompt)
		}
	})

	t.Run("process with string scenario and schedule default UTC timezone", func(t *testing.T) {
		raw := []byte(`---
grainfile: v1
title: Nightly check
when:
  schedule:
    cron: "30 4 * * 1-5"
  scenario: "When cutting a release branch"
  merge:
    branches: ["main", "release/*"]
---
Check the release branch.
`)
		g, err := Parse("processes/nightly.process.grain.md", raw)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if g.When.Schedule == nil || g.When.Schedule.Timezone != "UTC" {
			t.Errorf("Schedule = %+v, want Timezone=UTC", g.When.Schedule)
		}
		if g.When.Scenario == nil || g.When.Scenario.Description != "When cutting a release branch" {
			t.Errorf("Scenario = %+v", g.When.Scenario)
		}
		if g.When.Merge == nil || !reflect.DeepEqual(g.When.Merge.Branches, []string{"main", "release/*"}) {
			t.Errorf("Merge = %+v", g.When.Merge)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		// Verify mixed-case stems (including UTC timestamps with T and Z) are valid.
		for _, validPath := range []string{
			"tasks/GoodName.task.grain.md",
			"tasks/2026-10-18T143000Z-fix-API.task.grain.md",
		} {
			if _, err := Parse(validPath, []byte("---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\nBody.\n")); err != nil {
				t.Fatalf("Parse(%q) failed unexpectedly: %v", validPath, err)
			}
		}
		// Verify ordinary .task.md / .process.md files without .grain.md do not emit GF005 in ClassifyStrictPath.
		for _, nonGrain := range []string{"notes/foo.task.md", "notes/foo.process.md", "notes/foo.widget.md"} {
			if _, _, isGrain, warn := ClassifyStrictPath(nonGrain); isGrain || warn != "" {
				t.Fatalf("ClassifyStrictPath(%q) = isGrain=%v warn=%q, want false/empty", nonGrain, isGrain, warn)
			}
		}

		cases := []struct {
			name string
			path string
			body string
			code string
		}{
			{
				name: "leading underscore stem",
				path: "tasks/_bad.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeBadName,
			},
			{
				name: "invalid punctuation in stem",
				path: "tasks/bad$name.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeBadName,
			},
			{
				name: "non-string grainfile version",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: 1\ntitle: T\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "unsupported grainfile version",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v2\ntitle: T\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "trigger_with on task",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\ntrigger_with: ./p.process.grain.md\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "task depends_on process",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\ndepends_on:\n  - ./p.process.grain.md\n---\n",
				code: CodeWrongTargetKind,
			},
			{
				name: "unknown suffix",
				path: "notes/foo.widget.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\n---\n",
				code: CodeUnknownSuffix,
			},
			{
				name: "from_template wrong suffix on task",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\nfrom_template: /templates/p.process.grain.md.jinja2\n---\n",
				code: CodeMalformedRef,
			},
			{
				name: "from_template wrong suffix on process",
				path: "processes/p.process.grain.md",
				body: "---\ngrainfile: v1\ntitle: P\nfrom_template: /templates/t.task.grain.md.jinja2\nwhen:\n  manual: {}\n---\n",
				code: CodeMalformedRef,
			},
			{
				name: "unregistered capability suffix on grainfile",
				path: "capabilities/c.capability.grain.md",
				body: "---\ngrainfile: v1\ntitle: C\n---\n",
				code: CodeUnknownSuffix,
			},
			{
				name: "task capabilities points to .capability.grain.md file path",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\ncapabilities:\n  - /.grain/capabilities/gcp-key.capability.grain.md\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "process capabilities with non-mapping with field",
				path: "processes/p.process.grain.md",
				body: "---\ngrainfile: v1\ntitle: P\ncapabilities:\n  - capability: https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key\n    with: invalid\nwhen:\n  manual: {}\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "task capabilities with unknown property on entry object",
				path: "tasks/a.task.grain.md",
				body: "---\ngrainfile: v1\ntitle: T\ncapabilities:\n  - capability: https://github.com/bwsalmon/grain/capabilities/gcp#gcp-key\n    extra: 1\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "when.push empty filter object",
				path: "processes/p.process.grain.md",
				body: "---\ngrainfile: v1\ntitle: P\nwhen:\n  push: {}\n---\n",
				code: CodeSchemaInvalid,
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
				body: "\xef\xbb\xbf---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeMalformedFile,
			},
			{
				name: "NUL byte in file",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\x00\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeMalformedFile,
			},
			{
				name: "duplicate key in nested mapping",
				path: "processes/p.process.md",
				body: "---\ngrainfile: v1\ntitle: P\nrecipient: https://github.com/acme/grain-eu\non:\n  manual: {}\n  manual: {}\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "YAML merge key",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\n<<: *base\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "YAML explicit tag",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: !!str T\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeInvalidYAML,
			},
			{
				name: "multiline title",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: |\n  Line 1\n  Line 2\nrecipient: https://github.com/acme/grain-eu\n---\n",
				code: CodeSchemaInvalid,
			},
			{
				name: "recipient with anchor",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu#sec\n---\n",
				code: CodeMalformedRef,
			},
			{
				name: "recipient is blob URL",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu/blob/main/README.md\n---\n",
				code: CodeWrongTargetKind,
			},
			{
				name: "depends_on points to tree URL",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\ndepends_on:\n  - https://github.com/acme/widgets/tree/main/src\n---\n",
				code: CodeWrongTargetKind,
			},
			{
				name: "relative reference climbs above repo root",
				path: "tasks/a.task.md",
				body: "---\ngrainfile: v1\ntitle: T\nrecipient: https://github.com/acme/grain-eu\nreferences:\n  - ../../secret.md\n---\n",
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
grainfile: v1
title: Multi-ref task
recipient: ../deployments/grain-eu
requestor: /teams/platform
depends_on:
  - ./prep.task.grain.md
  - /tasks/shared/init.task.grain.md
  - https://GitHub.com:443/acme/widgets.git/blob/main/.grain/tasks/ext.task.grain.md
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

	g, err := Parse(".grain/tasks/work.task.grain.md", raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	err = g.Resolve("https://GitHub.com:443/acme/widgets.git/", "refs/heads/main", ".grain/tasks/work.task.grain.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if g.Repo != "https://github.com/acme/widgets" {
		t.Errorf("g.Repo = %q", g.Repo)
	}
	if g.URL != "https://github.com/acme/widgets/blob/main/.grain/tasks/work.task.grain.md" {
		t.Errorf("g.URL = %q", g.URL)
	}
	if g.Recipient != "https://github.com/acme/widgets/tree/main/.grain/deployments/grain-eu" {
		t.Errorf("g.Recipient = %q", g.Recipient)
	}
	if g.Requestor != "https://github.com/acme/widgets/tree/main/teams/platform" {
		t.Errorf("g.Requestor = %q", g.Requestor)
	}
	wantDeps := []string{
		"https://github.com/acme/widgets/blob/main/.grain/tasks/prep.task.grain.md",
		"https://github.com/acme/widgets/blob/main/tasks/shared/init.task.grain.md",
		"https://github.com/acme/widgets/blob/main/.grain/tasks/ext.task.grain.md",
	}
	if !reflect.DeepEqual(g.DependsOn, wantDeps) {
		t.Errorf("g.DependsOn = %v, want %v", g.DependsOn, wantDeps)
	}

	procRaw := []byte(`---
grainfile: v1
title: Co-triggered process
depends_on:
  - ../processes/nightly.process.grain.md
when:
  manual: {}
---
Run co-triggered check.
`)
	if _, err := Parse(".grain/processes/follow.process.grain.md", procRaw); err == nil || !strings.Contains(err.Error(), CodeSchemaInvalid) {
		t.Fatalf("Parse(proc with depends_on): got %v, want %s", err, CodeSchemaInvalid)
	}

	taskDepOnProcRaw := []byte(`---
grainfile: v1
title: Task depending on process
depends_on:
  - ../processes/nightly.process.grain.md
---
Rejected because depends_on must target a concrete .task.grain.md.
`)
	if _, err := Parse(".grain/tasks/bad-dep.task.grain.md", taskDepOnProcRaw); err == nil || !strings.Contains(err.Error(), CodeWrongTargetKind) {
		t.Fatalf("Parse(task with depends_on pointing to .process.grain.md): got %v, want %s", err, CodeWrongTargetKind)
	}

	taskDepOnTplRaw := []byte(`---
grainfile: v1
title: Task depending on unrendered jinja2 template
depends_on:
  - ./rollout-region.task.grain.md.jinja2
---
Rejected because depends_on cannot target an unrendered .jinja2 template.
`)
	if _, err := Parse(".grain/tasks/bad-dep-tpl.task.grain.md", taskDepOnTplRaw); err == nil || !strings.Contains(err.Error(), CodeWrongTargetKind) {
		t.Fatalf("Parse(task with depends_on pointing to .jinja2): got %v, want %s", err, CodeWrongTargetKind)
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
grainfile: v1
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

func TestFromTemplateAndMultiRepoWhenTriggers(t *testing.T) {
	// 1. Same-repo from_template grants AuthorizedReferences and CheckReferenceAccess;
	// cross-repo from_template is recorded as provenance without granting read access.
	sameRepoTask, err := Parse(".grain/tasks/2026-10-18T143000Z-fix-API.task.grain.md", []byte(`---
grainfile: v1
title: Fix API from template
recipient: https://github.com/acme/grain-eu
from_template: /.grain/templates/rollout.task.grain.md.jinja2
references:
  - /docs/spec.md
---
Fix API.
`))
	if err != nil {
		t.Fatalf("Parse sameRepoTask: %v", err)
	}
	if err := sameRepoTask.ResolveSource(Source{
		RepoURL: "https://github.com/acme/widgets",
		Ref:     "refs/heads/main",
		Commit:  "c111222",
		Path:    ".grain/tasks/2026-10-18T143000Z-fix-API.task.grain.md",
	}); err != nil {
		t.Fatalf("ResolveSource sameRepoTask: %v", err)
	}
	if sameRepoTask.FromTemplate != "https://github.com/acme/widgets/blob/main/.grain/templates/rollout.task.grain.md.jinja2" {
		t.Errorf("FromTemplate = %q", sameRepoTask.FromTemplate)
	}
	authRefs, _ := sameRepoTask.AuthorizedReferences("https://github.com/acme/widgets")
	wantTplRef := "https://github.com/acme/widgets/blob/c111222/.grain/templates/rollout.task.grain.md.jinja2"
	if !slices.Contains(authRefs, wantTplRef) {
		t.Errorf("AuthorizedReferences(%v) missing same-repo from_template %q", authRefs, wantTplRef)
	}
	if !sameRepoTask.AuthorizesReference("https://github.com/acme/widgets", "/.grain/templates/rollout.task.grain.md.jinja2") {
		t.Errorf("expected same-repo from_template to be authorized by AuthorizesReference")
	}

	crossRepoTask, err := Parse(".grain/tasks/cross-tpl.task.grain.md", []byte(`---
grainfile: v1
title: Task from cross-repo template
recipient: https://github.com/acme/grain-eu
from_template: https://github.com/acme/central-templates/blob/main/templates/rollout.task.grain.md.jinja2
---
Cross-repo template task.
`))
	if err != nil {
		t.Fatalf("Parse crossRepoTask: %v", err)
	}
	if err := crossRepoTask.ResolveSource(Source{
		RepoURL: "https://github.com/acme/widgets",
		Ref:     "refs/heads/main",
		Commit:  "c111222",
		Path:    ".grain/tasks/cross-tpl.task.grain.md",
	}); err != nil {
		t.Fatalf("ResolveSource crossRepoTask: %v", err)
	}
	crossAuth, _ := crossRepoTask.AuthorizedReferences("https://github.com/acme/widgets")
	if len(crossAuth) != 0 {
		t.Errorf("expected empty AuthorizedReferences for cross-repo from_template, got %v", crossAuth)
	}
	if crossRepoTask.AuthorizesReference("https://github.com/acme/widgets", crossRepoTask.FromTemplate) {
		t.Errorf("cross-repo from_template must not grant file read access")
	}

	// 2. RenderTemplate and ParseWithVars auto-populate from_template when unset.
	tplBytes := []byte(`---
grainfile: v1
title: "Rollout {{ region }}"
template_vars:
  region:
    required: true
---
Deploy to {{ region }}.
`)
	rendered, _, err := RenderTemplate(string(tplBytes), map[string]string{
		"region":        "eu",
		"from_template": "https://github.com/acme/widgets/blob/main/.grain/templates/rollout.task.grain.md.jinja2",
	})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if !strings.Contains(rendered, "from_template: https://github.com/acme/widgets/blob/main/.grain/templates/rollout.task.grain.md.jinja2") {
		t.Errorf("expected RenderTemplate to auto-populate from_template, got:\n%s", rendered)
	}
	parsedTpl, err := ParseWithVars(".grain/templates/rollout.task.grain.md.jinja2", tplBytes, map[string]string{"region": "eu"})
	if err != nil {
		t.Fatalf("ParseWithVars: %v", err)
	}
	if parsedTpl.FromTemplate != "/.grain/templates/rollout.task.grain.md.jinja2" {
		t.Errorf("parsedTpl.FromTemplate = %q, want /.grain/templates/rollout.task.grain.md.jinja2", parsedTpl.FromTemplate)
	}

	// 3. Multi-repo and file/folder/URL triggers in when.push, when.pull_request, when.merge.
	procRaw := []byte(`---
grainfile: v1
title: Multi-repo auth and schema gate
gate: true
when:
  pull_request:
    - repo: acme/payments
      branches: ["main", "release/*"]
      paths:
        - src/auth/
        - /pkg/crypto
        - https://github.com/acme/payments/blob/main/schemas/v2.sql
      ignore:
        - src/auth/testdata/
    - repos:
        - https://github.com/acme/billing
        - acme/ledger
      paths:
        - https://github.com/acme/billing/tree/main/migrations
        - /contracts/
  merge:
    repos: ["acme/payments", "acme/billing"]
    branches: ["main"]
---
Verify auth and schema changes across repositories.
`)
	proc, err := Parse("processes/multi-gate.process.grain.md", procRaw)
	if err != nil {
		t.Fatalf("Parse multi-gate process: %v", err)
	}
	if err := proc.ResolveSource(Source{
		RepoURL: "https://github.com/acme/grain-eu",
		Ref:     "refs/heads/main",
		Commit:  "c-proc-1",
		Path:    "processes/multi-gate.process.grain.md",
	}); err != nil {
		t.Fatalf("ResolveSource multi-gate process: %v", err)
	}

	// Matches folder path src/auth/ in acme/payments
	if ok, paths := proc.MatchedEventPaths(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"src/auth/jwt/verify.go", "README.md"},
	}); !ok || !reflect.DeepEqual(paths, []string{"src/auth/jwt/verify.go"}) {
		t.Errorf("expected src/auth/ folder match on acme/payments, got ok=%v paths=%v", ok, paths)
	}

	// Ignored folder src/auth/testdata/ does not fire
	if proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"src/auth/testdata/fixture.json"},
	}) {
		t.Errorf("expected ignored folder src/auth/testdata/ not to match")
	}

	// Matches /pkg/crypto (folder path without trailing slash) on release/v1
	if !proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "release/v1",
		ChangedPaths: []string{"pkg/crypto/ed25519.go"},
	}) {
		t.Errorf("expected /pkg/crypto folder match on release/v1")
	}

	// Full blob URL in paths matches exact file on matching branch, and rejects non-matching branch
	if !proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"schemas/v2.sql"},
	}) {
		t.Errorf("expected full blob URL https://github.com/acme/payments/blob/main/schemas/v2.sql to match on main")
	}
	if proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "release/v1",
		ChangedPaths: []string{"schemas/v2.sql"},
	}) {
		t.Errorf("expected full blob URL pinned to main not to match on release/v1")
	}

	// Second filter rule matches full tree URL on acme/billing (only on main)
	if proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/billing",
		Branch:       "feat/any",
		ChangedPaths: []string{"migrations/001_init.sql"},
	}) {
		t.Errorf("expected full tree URL pinned to main not to match on feat/any")
	}
	if !proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/billing",
		Branch:       "main",
		ChangedPaths: []string{"migrations/001_init.sql"},
	}) {
		t.Errorf("expected full tree URL on acme/billing to match on main")
	}

	// Unlisted repo acme/widgets does not match
	if proc.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/widgets",
		Branch:       "main",
		ChangedPaths: []string{"src/auth/jwt.go"},
	}) {
		t.Errorf("expected unlisted repo acme/widgets not to match")
	}
}

func TestOptionalEmptyMarkdownBodyAndCanonicalDeploymentRecipient(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		raw  string
		kind Kind
	}{
		{
			name: "task with completely empty body and canonical Hub deployment recipient",
			path: ".grain/tasks/2026-10-20-rotate-keys.task.grain.md",
			raw:  "---\ngrainfile: v1\ntitle: Rotate staging API signing keys and verify health checks\nrecipient: https://hub.example.com/deployments/payments-prod\n---\n",
			kind: KindTask,
		},
		{
			name: "task with whitespace-only body",
			path: ".grain/tasks/2026-10-20-whitespace.task.grain.md",
			raw:  "---\ngrainfile: v1\ntitle: Bump Go toolchain to 1.24.2 across all module files\n---\n   \n\t\n",
			kind: KindTask,
		},
		{
			name: "process with completely empty body",
			path: ".grain/processes/smoke-check.process.grain.md",
			raw:  "---\ngrainfile: v1\ntitle: Run go test ./... and verify zero linter warnings on every pull request\nwhen:\n  pull_request:\n    branches: [\"main\"]\n---\n",
			kind: KindProcess,
		},
		{
			name: "process with whitespace-only body",
			path: ".grain/processes/nightly-audit.process.grain.md",
			raw:  "---\ngrainfile: v1\ntitle: Run govulncheck ./... nightly and file a task for any reported vulnerability\nwhen:\n  schedule:\n    cron: \"0 4 * * *\"\n---\n  \n",
			kind: KindProcess,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Parse(tc.path, []byte(tc.raw))
			if err != nil {
				t.Fatalf("Parse(%s): unexpected error: %v", tc.path, err)
			}
			if g.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", g.Kind, tc.kind)
			}
			if strings.TrimSpace(g.Body) != "" {
				t.Errorf("TrimSpace(Body) = %q, want empty string", g.Body)
			}
			if err := g.ResolveSource(Source{
				RepoURL: "https://github.com/acme/widgets",
				Ref:     "refs/heads/main",
				Commit:  "abc1234",
				Path:    tc.path,
			}); err != nil {
				t.Fatalf("ResolveSource(%s): %v", tc.path, err)
			}
		})
	}

	if got := DeploymentIDFromRecipient("https://hub.example.com/deployments/payments-prod"); got != "payments-prod" {
		t.Errorf("DeploymentIDFromRecipient(https) = %q, want payments-prod", got)
	}
	if got := DeploymentIDFromRecipient("/deployments/payments-prod"); got != "payments-prod" {
		t.Errorf("DeploymentIDFromRecipient(rooted) = %q, want payments-prod", got)
	}
	if got := CanonicalDeploymentURL("https://hub.example.com/", "payments-prod"); got != "https://hub.example.com/deployments/payments-prod" {
		t.Errorf("CanonicalDeploymentURL = %q", got)
	}
}

func TestMandatoryGrainfileVersionAndUniversalTimezoneSemantics(t *testing.T) {
	t.Run("missing grainfile version emits GF020 on task, process, and rendered template", func(t *testing.T) {
		for _, tc := range []struct {
			path string
			raw  string
		}{
			{
				path: "tasks/missing-ver.task.grain.md",
				raw:  "---\ntitle: Missing grainfile version\nrecipient: https://github.com/acme/grain-eu\n---\nBody.\n",
			},
			{
				path: "processes/missing-ver.process.grain.md",
				raw:  "---\ntitle: Missing grainfile version\nwhen:\n  manual: {}\n---\nBody.\n",
			},
		} {
			_, err := Parse(tc.path, []byte(tc.raw))
			if err == nil {
				t.Fatalf("Parse(%s): expected GF020 error for missing grainfile, got nil", tc.path)
			}
			errs, ok := err.(Errors)
			if !ok || len(errs) == 0 || errs[0].Code != CodeSchemaInvalid || !strings.Contains(errs[0].Message, `grainfile: required field missing (expected "v1")`) {
				t.Fatalf("Parse(%s): unexpected error %v", tc.path, err)
			}
		}

		_, err := Parse("tasks/tpl.task.grain.md.jinja2", []byte("---\ntitle: \"{{ name }}\"\n---\nBody.\n"))
		if err == nil || !strings.Contains(err.Error(), `grainfile: required field missing (expected "v1")`) {
			t.Fatalf("Parse template without grainfile: v1: got %v, want GF020 missing grainfile", err)
		}
	})

	t.Run("not_before and created_at accept Z, explicit offsets, and bare dates, normalizing to UTC", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			notBef  string
			wantUTC time.Time
		}{
			{
				name:    "UTC Z suffix",
				notBef:  "2026-11-01T07:00:00Z",
				wantUTC: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC),
			},
			{
				name:    "positive offset +02:00",
				notBef:  "2026-11-01T09:00:00+02:00",
				wantUTC: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC),
			},
			{
				name:    "negative offset -07:00 with fractional seconds",
				notBef:  "2026-11-01T00:00:00.500-07:00",
				wantUTC: time.Date(2026, 11, 1, 7, 0, 0, 500_000_000, time.UTC),
			},
			{
				name:    "bare calendar date YYYY-MM-DD",
				notBef:  "2026-11-01",
				wantUTC: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				raw := fmt.Sprintf("---\ngrainfile: v1\ntitle: Timezone test\nnot_before: %q\n---\nBody.\n", tc.notBef)
				g, err := Parse("tasks/tz.task.grain.md", []byte(raw))
				if err != nil {
					t.Fatalf("Parse failed: %v", err)
				}
				if !g.NotBefore.Equal(tc.wantUTC) || g.NotBefore.Location() != time.UTC {
					t.Errorf("NotBefore = %v (%v), want %v (UTC)", g.NotBefore, g.NotBefore.Location(), tc.wantUTC)
				}
				parsedTS, err := ParseTime("2026-10-16T11:30:00+05:30")
				if err != nil {
					t.Fatalf("ParseTime(+05:30): %v", err)
				}
				wantCreated := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
				if !parsedTS.Equal(wantCreated) || parsedTS.Location() != time.UTC {
					t.Errorf("ParseTime(+05:30) = %v (%v), want %v (UTC)", parsedTS, parsedTS.Location(), wantCreated)
				}
				if g.ActiveAt(tc.wantUTC.Add(-time.Second)) {
					t.Errorf("expected ActiveAt(before) = false")
				}
				if !g.ActiveAt(tc.wantUTC) {
					t.Errorf("expected ActiveAt(exact) = true")
				}
			})
		}

		for _, badTS := range []string{
			"2026-11-01T09:00:00",       // naive timestamp without Z or offset
			"2026-11-01T09:00:00-00:00", // RFC 3339 unknown local offset
			"2026-11-01T09:00:00+24:00", // out-of-range hour offset
			"2026-11-01T09:00:00+02:60", // out-of-range minute offset
			"2026-02-30T00:00:00Z",      // invalid calendar date
		} {
			raw := fmt.Sprintf("---\ngrainfile: v1\ntitle: Bad timestamp\nnot_before: %q\n---\nBody.\n", badTS)
			if _, err := Parse("tasks/bad-ts.task.grain.md", []byte(raw)); err == nil {
				t.Errorf("Parse with not_before=%q succeeded, want GF020 error", badTS)
			}
		}
	})

	t.Run("when.schedule single and list with IANA timezones", func(t *testing.T) {
		raw := []byte(`---
grainfile: v1
title: Multi-region schedule
when:
  schedule:
    - cron: "0 9 * * 1-5"
      timezone: Europe/London
    - cron: "0 9 * * 1-5"
      timezone: America/Los_Angeles
    - cron: "30 4 * * *"
---
Run weekday checks.
`)
		g, err := Parse("processes/multi-sched.process.grain.md", []byte(raw))
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		items := g.When.Schedule.Items()
		if len(items) != 3 {
			t.Fatalf("Schedule.Items() len = %d, want 3", len(items))
		}
		if items[0].Timezone != "Europe/London" || items[1].Timezone != "America/Los_Angeles" || items[2].Timezone != "UTC" {
			t.Errorf("unexpected schedule items: %+v", items)
		}

		for _, badTZ := range []string{"+02:00", "-05:00", "PST", "EST", "CET", "Mars/Phobos", "UTC+2"} {
			badRaw := fmt.Sprintf("---\ngrainfile: v1\ntitle: Bad schedule tz\nwhen:\n  schedule:\n    cron: \"0 9 * * 1-5\"\n    timezone: %q\n---\nBody.\n", badTZ)
			if _, err := Parse("processes/bad-tz.process.grain.md", []byte(badRaw)); err == nil {
				t.Errorf("Parse with schedule.timezone=%q succeeded, want GF020 error", badTZ)
			}
		}
	})
}

func TestWhenWorkflowTriggerParsingAndMatching(t *testing.T) {
	raw := []byte(`---
grainfile: v1
title: Triage CI failures and post-release smoke tests
when:
  workflow:
    - workflow: "CI"
      workflows:
        - "Nightly *"
        - ".github/workflows/e2e-*.yml"
      events: ["failed", "started"]
      branches: ["main", "release/*"]
    - workflow: "Release"
      events: ["success"]
      repo: acme/widgets
      branches: ["main"]
---
Investigate workflow run.
`)
	g, err := Parse("processes/ci-triage.process.grain.md", raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := g.ResolveSource(Source{
		RepoURL: "https://github.com/acme/widgets",
		Ref:     "refs/heads/main",
		Commit:  "abc1234",
		Path:    "processes/ci-triage.process.grain.md",
	}); err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	if len(g.When.Workflow) != 2 {
		t.Fatalf("len(When.Workflow) = %d, want 2", len(g.When.Workflow))
	}
	// Verify alias normalization: "started" -> "triggered", "success" -> "succeeded".
	if !slices.Equal(g.When.Workflow[0].Events, []string{"failed", "triggered"}) {
		t.Errorf("Workflow[0].Events = %v, want [failed triggered]", g.When.Workflow[0].Events)
	}
	if !slices.Equal(g.When.Workflow[1].Events, []string{"succeeded"}) {
		t.Errorf("Workflow[1].Events = %v, want [succeeded]", g.When.Workflow[1].Events)
	}

	// Exact workflow name + failed event on main
	if !g.MatchesWorkflow("https://github.com/acme/widgets", "main", "CI", ".github/workflows/ci.yml", "failed") {
		t.Error("expected CI failed on main to match")
	}
	// Glob workflow name + triggered (via alias "started") on release/v1.2
	if !g.MatchesWorkflow("https://github.com/acme/widgets", "refs/heads/release/v1.2", "Nightly Integration", "", "started") {
		t.Error("expected Nightly Integration started on release/v1.2 to match")
	}
	// Workflow file path glob match
	if !g.MatchesWorkflow("https://github.com/acme/widgets", "main", "Unrelated Display Name", ".github/workflows/e2e-browser.yml", "failed") {
		t.Error("expected .github/workflows/e2e-browser.yml failed on main to match")
	}
	// Non-matching event (CI succeeded should not match filter 0)
	if g.MatchesWorkflow("https://github.com/acme/widgets", "main", "CI", ".github/workflows/ci.yml", "succeeded") {
		t.Error("expected CI succeeded on main NOT to match")
	}
	// Second filter: Release succeeded on main
	if !g.MatchesWorkflow("https://github.com/acme/widgets", "main", "Release", ".github/workflows/release.yml", "succeeded") {
		t.Error("expected Release succeeded on main to match")
	}
	// Non-matching branch
	if g.MatchesWorkflow("https://github.com/acme/widgets", "feature/wip", "CI", ".github/workflows/ci.yml", "failed") {
		t.Error("expected CI failed on feature/wip NOT to match")
	}
	// Non-matching repo
	if g.MatchesWorkflow("https://github.com/acme/other", "main", "CI", ".github/workflows/ci.yml", "failed") {
		t.Error("expected CI failed on acme/other NOT to match")
	}

	// Default events when `events` is omitted: ["succeeded", "failed"] (not "triggered").
	defRaw := []byte("---\ngrainfile: v1\ntitle: Default workflow events\nwhen:\n  workflow:\n    workflow: CI\n---\nBody.\n")
	defGF, err := Parse("processes/def-wf.process.grain.md", defRaw)
	if err != nil {
		t.Fatalf("Parse default workflow events: %v", err)
	}
	if !defGF.MatchesWorkflow("", "main", "CI", "", "succeeded") || !defGF.MatchesWorkflow("", "main", "CI", "", "failed") {
		t.Error("expected default workflow events to match succeeded and failed")
	}
	if defGF.MatchesWorkflow("", "main", "CI", "", "triggered") {
		t.Error("expected default workflow events NOT to match triggered")
	}

	// Invalid workflow triggers must fail with GF020.
	for _, badWF := range []string{
		"---\ngrainfile: v1\ntitle: Missing workflow name\nwhen:\n  workflow:\n    events: [\"failed\"]\n---\nBody.\n",
		"---\ngrainfile: v1\ntitle: Bad workflow event\nwhen:\n  workflow:\n    workflow: CI\n    events: [\"cancelled\"]\n---\nBody.\n",
	} {
		if _, err := Parse("processes/bad-wf.process.grain.md", []byte(badWF)); err == nil {
			t.Errorf("expected GF020 error for invalid when.workflow:\n%s", badWF)
		}
	}
}

func TestMultiRepoWhenGateTriggerEdgeCases(t *testing.T) {
	// 1. Path traversal & encoded path hazards in both Parse and runtime MatchPathPattern / MatchedEventPaths.
	for _, badRaw := range []string{
		"---\ngrainfile: v1\ntitle: P\ngate: true\nwhen:\n  push:\n    paths: [\"../secret.go\"]\n---\n",
		"---\ngrainfile: v1\ntitle: P\ngate: true\nwhen:\n  push:\n    paths: [\"src/%2e%2e/secret.go\"]\n---\n",
		"---\ngrainfile: v1\ntitle: P\ngate: true\nwhen:\n  push:\n    paths: [\"src\\\\auth\\\\login.go\"]\n---\n",
		"---\ngrainfile: v1\ntitle: P\ngate: true\nwhen:\n  push:\n    paths: [\"src/\"]\n    ignore: [\"src/%2e%2e/secret.go\"]\n---\n",
	} {
		if _, err := Parse(".grain/processes/bad.process.grain.md", []byte(badRaw)); err == nil {
			t.Errorf("expected Parse to reject invalid when path in:\n%s", badRaw)
		}
	}

	for _, traversalPath := range []string{
		"../src/auth/login.go",
		"../../etc/passwd",
		"docs/../../src/auth/login.go",
		"docs/../src/auth/login.go",
		"src/auth/../../src/auth/login.go",
		"src/%2e%2e/auth/login.go",
		"src/auth%2flogin.go",
		`src\auth\login.go`,
		"..",
		"../",
	} {
		if MatchPathPattern("src/auth/", traversalPath) {
			t.Errorf("MatchPathPattern(src/auth/, %q) = true, want false for traversal/hazard path", traversalPath)
		}
		if MatchPathPattern("/", traversalPath) {
			t.Errorf("MatchPathPattern(/, %q) = true, want false for traversal/hazard path", traversalPath)
		}
		if MatchPathPattern(traversalPath, "src/auth/login.go") {
			t.Errorf("MatchPathPattern(%q, src/auth/login.go) = true, want false for traversal/hazard pattern", traversalPath)
		}
	}

	// 2. Glob boundaries:
	//    - Folder pattern `dir/` or `/dir` matches `dir/a.go` and `dir/sub/b.go`,
	//      but never `dir_other/a.go`, `dir-2/a.go`, or `other/dir/a.go`.
	//    - File glob `*.go` or `src/*.go` matches `main.go` / `src/main.go`,
	//      but never a file inside a directory named `foo.go/bar.txt`.
	boundaryCases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"dir/", "dir/file.go", true},
		{"dir/", "dir/sub/file.go", true},
		{"dir/", "dir_other/file.go", false},
		{"dir/", "other/dir/file.go", false},
		{"/dir", "dir/file.go", true},
		{"/dir", "dir/sub/file.go", true},
		{"/dir", "dir_other/file.go", false},
		{"/dir", "other/dir/file.go", false},
		{"*.go", "main.go", true},
		{"*.go", "foo.go/bar.txt", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/foo.go/bar.txt", false},
		{"src/*.go", "src/sub/main.go", false},
	}
	for _, tc := range boundaryCases {
		if got := MatchPathPattern(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPathPattern(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}

	// 3. Case-sensitivity:
	//    - File paths, folder names, and branch names are strictly case-sensitive.
	//    - Repository URLs (scheme/host/owner/repo) are case-insensitive.
	// 4. `ignore` precedence over `paths` folders (`dir/` vs `/dir` and full URLs).
	gateRaw := []byte(`---
grainfile: v1
title: Multi-repo folder and ignore precedence gate
gate: true
when:
  pull_request:
    - repos:
        - acme/payments
        - https://GitHub.com/Acme/Billing
      branches: ["main", "release/*"]
      paths:
        - dir/
        - /pkg/service
        - https://github.com/acme/payments/blob/main/schemas/v2.sql
      ignore:
        - /dir/ignored_sub
        - pkg/service/testdata/
        - /pkg/service/generated
    - repo: acme/WholeDirIgnored
      branches: ["main"]
      paths:
        - dir/
      ignore:
        - /dir
    - repo: acme/RootedDirIgnored
      branches: ["main"]
      paths:
        - /dir
      ignore:
        - dir/
---
Run gate check.
`)
	gateGF, err := Parse(".grain/processes/edge-gate.process.grain.md", gateRaw)
	if err != nil {
		t.Fatalf("Parse edge-gate: %v", err)
	}
	if err := gateGF.ResolveSource(Source{
		RepoURL: "https://github.com/acme/grain-eu",
		Ref:     "refs/heads/main",
		Commit:  "c-edge-1",
		Path:    ".grain/processes/edge-gate.process.grain.md",
	}); err != nil {
		t.Fatalf("ResolveSource edge-gate: %v", err)
	}

	// Case-sensitive branch: "Main" does not match "main"
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "Main",
		ChangedPaths: []string{"dir/handler.go"},
	}) {
		t.Errorf("expected branch 'Main' not to match case-sensitive 'main'")
	}

	// Case-sensitive path: "Dir/handler.go" and "PKG/service/api.go" do not match "dir/" or "/pkg/service"
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"Dir/handler.go", "PKG/service/api.go"},
	}) {
		t.Errorf("expected case-mismatched paths 'Dir/handler.go' and 'PKG/service/api.go' not to match")
	}

	// Case-insensitive repo URL matches "https://GITHUB.COM/ACME/BILLING", while case-sensitive path matches
	if !gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://GITHUB.COM/ACME/BILLING",
		Branch:       "main",
		ChangedPaths: []string{"pkg/service/api.go"},
	}) {
		t.Errorf("expected case-insensitive repo URL https://GITHUB.COM/ACME/BILLING with exact-case path to match")
	}

	// Blob URL `https://github.com/acme/payments/blob/main/schemas/v2.sql` matches exact file,
	// and does NOT match a nested path `schemas/v2.sql/extra.sql`
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"schemas/v2.sql/extra.sql"},
	}) {
		t.Errorf("expected blob URL not to match nested file under schemas/v2.sql/extra.sql")
	}

	// `ignore` precedence over `paths` folders:
	// - `/dir/ignored_sub` (without trailing slash) ignores `dir/ignored_sub/a.go` under `paths: ["dir/"]`
	// - `pkg/service/testdata/` (with trailing slash) ignores `pkg/service/testdata/b.go` under `paths: ["/pkg/service"]`
	// - URL ignore `https://github.com/acme/billing/tree/main/pkg/service/generated` ignores `pkg/service/generated/c.go` on acme/billing
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/payments",
		Branch:       "main",
		ChangedPaths: []string{"dir/ignored_sub/a.go", "pkg/service/testdata/b.go"},
	}) {
		t.Errorf("expected ignored subfolders /dir/ignored_sub and pkg/service/testdata/ to take precedence over paths")
	}
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/billing",
		Branch:       "main",
		ChangedPaths: []string{"pkg/service/generated/c.go"},
	}) {
		t.Errorf("expected full tree URL in ignore to suppress match on acme/billing")
	}

	// `ignore: ["/dir"]` suppresses `paths: ["dir/"]`, and `ignore: ["dir/"]` suppresses `paths: ["/dir"]`
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/wholedirignored",
		Branch:       "main",
		ChangedPaths: []string{"dir/sub/file.go"},
	}) {
		t.Errorf("expected ignore: [/dir] to take precedence over paths: [dir/]")
	}
	if gateGF.MatchesEvent(ForgeEvent{
		Source:       TriggerPullRequest,
		Repo:         "https://github.com/acme/rooteddirignored",
		Branch:       "main",
		ChangedPaths: []string{"dir/sub/file.go"},
	}) {
		t.Errorf("expected ignore: [dir/] to take precedence over paths: [/dir]")
	}
}

func TestCampaignRegressionSlugCapabilitiesAndClimb(t *testing.T) {
	// 1. DeploymentIDFromRecipient rejects invalid deployment slugs.
	for _, invalid := range []string{
		"/deployments/INVALID_SLUG!",
		"/deployments/-bad",
		"/deployments/bad-",
		"https://hub.example.com/deployments/bad_underscore",
		"https://github.com/acme/widgets/tree/main/deployments/BAD..SLUG",
	} {
		if got := DeploymentIDFromRecipient(invalid); got != "" {
			t.Errorf("DeploymentIDFromRecipient(%q) = %q, want empty string", invalid, got)
		}
	}

	// 2. LintTree reports GF030 on invalid /deployments/<id> slug (matching grainlint.py).
	tmp := t.TempDir()
	taskDir := filepath.Join(tmp, ".grain", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	badRecipFile := filepath.Join(taskDir, "bad-recip.task.grain.md")
	badRecipContent := "---\ngrainfile: v1\ntitle: Bad deployment slug\nrecipient: /deployments/INVALID_SLUG!\n---\nBody.\n"
	if err := os.WriteFile(badRecipFile, []byte(badRecipContent), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := LintTree(tmp)
	if err != nil {
		t.Fatalf("LintTree: %v", err)
	}
	hasGF030 := false
	for _, f := range findings {
		if f.Code == "GF030" {
			hasGF030 = true
		}
	}
	if !hasGF030 {
		t.Errorf("expected LintTree to report GF030 on /deployments/INVALID_SLUG!, got %+v", findings)
	}

	// 3. Climbing above repo root in references or depends_on emits exactly 1 GF036 per offending reference.
	_, climbErr := Parse(".grain/tasks/climb.task.grain.md", []byte("---\ngrainfile: v1\ntitle: Climb\nreferences:\n  - ../../../outside.md\ndepends_on:\n  - ../../../other.task.grain.md\n---\nBody.\n"))
	if climbErr == nil {
		t.Fatal("expected GF036 error on climbing references/depends_on")
	}
	gfErrs, ok := climbErr.(Errors)
	if !ok {
		t.Fatalf("expected Errors, got %T: %v", climbErr, climbErr)
	}
	gf036Count := 0
	for _, f := range gfErrs {
		if f.Code == CodeEscapesRepo {
			gf036Count++
		}
	}
	if gf036Count != 2 {
		t.Errorf("expected exactly 2 GF036 findings (1 for references, 1 for depends_on), got %d: %+v", gf036Count, gfErrs)
	}
}
