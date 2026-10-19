package grainfiles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

func eachStore(t *testing.T, f func(t *testing.T, s *Store, dbURL string)) {
	t.Run("sqlite", func(t *testing.T) {
		dbURL := "sqlite:" + filepath.Join(t.TempDir(), "grainfiles.db")
		if _, err := OpenExisting(context.Background(), dbURL); err == nil {
			t.Fatal("OpenExisting on unmigrated SQLite DB succeeded, want error")
		}
		s, err := Open(context.Background(), dbURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		s2, err := OpenExisting(context.Background(), dbURL)
		if err != nil {
			t.Fatalf("OpenExisting after Open: %v", err)
		}
		s2.Close()
		f(t, s, dbURL)
	})
	t.Run("postgres", func(t *testing.T) {
		pg := pgtest.Start(t)
		if _, err := OpenExistingStore(context.Background(), pg.URL); err == nil {
			t.Fatal("OpenExistingStore on unmigrated Postgres DB succeeded, want error")
		}
		s, err := OpenStore(context.Background(), pg.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		s2, err := OpenExistingStore(context.Background(), pg.URL)
		if err != nil {
			t.Fatalf("OpenExistingStore after OpenStore: %v", err)
		}
		s2.Close()
		f(t, s, pg.URL)
	})
}

func dumpAllTables(t *testing.T, dbURL string) string {
	t.Helper()
	var (
		db  *sql.DB
		err error
	)
	if path, ok := strings.CutPrefix(dbURL, "sqlite:"); ok {
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		db.SetMaxOpenConns(1)
	} else {
		db, err = sql.Open("pgx", dbURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var sb strings.Builder
	for _, tbl := range Tables {
		rows, err := db.Query(`SELECT * FROM "` + tbl + `"`)
		if err != nil {
			t.Fatalf("dumping table %s: %v", tbl, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				fmt.Fprintf(&sb, "%s.%s=%v ", tbl, cols[i], v)
			}
			sb.WriteByte('\n')
		}
		rows.Close()
	}
	return sb.String()
}

func TestStoreIndexAndNoCodeAtRest(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, dbURL string) {
		ctx := context.Background()
		fixedNow := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return fixedNow }

		secretPrompt := "SILO_SECRET_PROMPT_BODY_9918273645"
		rawTask := `---
grainfile: v1
title: Migrate widget binary header
recipient: https://github.com/acme/grain-eu
requestor: Bruce Salmon <bsalmon@google.com>
created_at: "2026-10-16T09:00:00Z"
references:
  - /docs/widget-schema.md#schema-version-header
  - ./helper.md
  - https://github.com/acme/other-repo/blob/main/secret.md
depends_on:
  - ./prep.task.md
---
` + secretPrompt + "\n"

		g, err := Parse(".grain/tasks/2026-10-16-migrate.task.md", []byte(rawTask))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := g.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "a1b2c3d4",
			Path:    ".grain/tasks/2026-10-16-migrate.task.md",
		}); err != nil {
			t.Fatalf("ResolveSource: %v", err)
		}

		rec, err := s.IndexGrainfile(ctx, g)
		if err != nil {
			t.Fatalf("IndexGrainfile: %v", err)
		}

		wantAuthorized := []string{
			"https://github.com/acme/widgets/blob/a1b2c3d4/docs/widget-schema.md#schema-version-header",
			"https://github.com/acme/widgets/blob/a1b2c3d4/.grain/tasks/helper.md",
		}
		if !reflect.DeepEqual(rec.AuthorizedReferences, wantAuthorized) {
			t.Errorf("rec.AuthorizedReferences = %v, want %v", rec.AuthorizedReferences, wantAuthorized)
		}
		if rec.Grainfile != "v1" || rec.SpecSummaryPath != "docs/grainfile-v1.md" || rec.SpecPath != "docs/grainfiles-spec.md" {
			t.Errorf("rec version metadata = (%q, %q, %q)", rec.Grainfile, rec.SpecSummaryPath, rec.SpecPath)
		}
		if len(rec.References) != 3 {
			t.Errorf("rec.References = %v, want 3 entries", rec.References)
		}

		got, err := s.GetIndex(ctx, "https://github.com/acme/widgets", ".grain/tasks/2026-10-16-migrate.task.md", "a1b2c3d4")
		if err != nil {
			t.Fatalf("GetIndex: %v", err)
		}
		if got.URL != "https://github.com/acme/widgets/blob/main/.grain/tasks/2026-10-16-migrate.task.md" ||
			got.Kind != KindTask ||
			got.Title != "Migrate widget binary header" ||
			got.Recipient != "https://github.com/acme/grain-eu" ||
			got.Requestor != "Bruce Salmon <bsalmon@google.com>" ||
			got.Draft ||
			!reflect.DeepEqual(got.AuthorizedReferences, wantAuthorized) {
			t.Errorf("GetIndex mismatch: %+v", got)
		}

		byURL, err := s.GetIndexByURL(ctx, got.URL, "a1b2c3d4")
		if err != nil || byURL.CommitSHA != "a1b2c3d4" {
			t.Errorf("GetIndexByURL: %+v, %v", byURL, err)
		}

		// Verify no prompt body or source code is stored anywhere at rest.
		dumped := dumpAllTables(t, dbURL)
		if strings.Contains(dumped, secretPrompt) {
			t.Fatalf("database holds Grainfile prompt body at rest:\n%s", dumped)
		}
		if !strings.Contains(dumped, "Migrate widget binary header") {
			t.Fatalf("dump did not read indexed title from database")
		}
	})
}

func TestStoreDraftAndNotBeforeFiltering(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }

		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Owner:      "alice@example.com",
			Recipients: []string{"https://github.com/acme/grain-eu"},
			Folders:    []string{"https://github.com/acme/widgets/tree/main/.grain/processes"},
		}); err != nil {
			t.Fatalf("SetSubscriptions: %v", err)
		}

		// 1. Index a draft: true task -> must NOT appear in active events.
		draftYAML := `---
grainfile: v1
title: Draft widget task
recipient: https://github.com/acme/grain-eu
draft: true
references:
  - /docs/widget-schema.md
---
Work in progress.
`
		draftGF, err := Parse(".grain/tasks/wip.task.md", []byte(draftYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = draftGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-draft-1",
			Path:    ".grain/tasks/wip.task.md",
		})
		draftRec, err := s.IndexGrainfile(ctx, draftGF)
		if err != nil {
			t.Fatal(err)
		}
		if !draftRec.Draft || len(draftRec.AuthorizedReferences) != 0 {
			t.Errorf("expected draft record with empty authorized_references, got %+v", draftRec)
		}

		evs, err := s.EventsForNode(ctx, "node-eu", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 0 {
			t.Fatalf("expected 0 events for draft task, got %+v", evs)
		}

		// 2. Index a non-draft task with future not_before -> deferred until timestamp reached.
		deferredYAML := `---
grainfile: v1
title: Deferred migration task
recipient: https://github.com/acme/grain-eu
not_before: "2026-10-25T09:00:00Z"
---
Run on Oct 25.
`
		defGF, err := Parse(".grain/tasks/deferred.task.md", []byte(deferredYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = defGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-def-1",
			Path:    ".grain/tasks/deferred.task.md",
		})
		if _, err := s.IndexGrainfile(ctx, defGF); err != nil {
			t.Fatal(err)
		}

		// Also index a process with future not_before.
		defProcYAML := `---
grainfile: v1
title: Deferred security gate
not_before: "2026-10-25T09:00:00Z"
gate: true
when:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Check security.
`
		defProc, err := Parse(".grain/processes/sec.process.grain.md", []byte(defProcYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = defProc.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-def-1",
			Path:    ".grain/processes/sec.process.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, defProc); err != nil {
			t.Fatal(err)
		}

		// Before not_before: no active events, and trigger does not fire.
		evs, err = s.EventsForNode(ctx, "node-eu", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 0 {
			t.Fatalf("before not_before: expected 0 events, got %+v", evs)
		}
		trigEvs, err := s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "c-push-1", ForgeEvent{
			Source:       TriggerPush,
			Branch:       "main",
			ChangedPaths: []string{"src/widget.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(trigEvs) != 0 {
			t.Fatalf("before not_before: expected 0 trigger events, got %+v", trigEvs)
		}

		// Advance clock past not_before -> both deferred task and process become active!
		now = time.Date(2026, 10, 25, 9, 0, 1, 0, time.UTC)
		caughtUp, cursor, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(caughtUp) != 2 {
			t.Fatalf("after not_before: expected 2 promoted events (task_ready + process_updated), got %+v", caughtUp)
		}
		gotTypes := []string{caughtUp[0].Type, caughtUp[1].Type}
		if !slicesContainsAll(gotTypes, EventTaskReady, EventProcessUpdated) {
			t.Errorf("unexpected promoted event types: %v", gotTypes)
		}

		// Now trigger evaluation fires the process!
		trigEvs, err = s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "c-push-2", ForgeEvent{
			Source:       TriggerPush,
			Branch:       "main",
			ChangedPaths: []string{"src/widget.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(trigEvs) != 1 || trigEvs[0].Type != EventProcessTriggered || !trigEvs[0].Gate {
			t.Fatalf("after not_before: expected 1 process_triggered event with Gate=true, got %+v", trigEvs)
		}

		// Catchup since cursor returns only the new process_triggered event.
		delta, newCursor, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(delta) != 1 || delta[0].Type != EventProcessTriggered || newCursor <= cursor {
			t.Fatalf("expected 1 delta event after cursor %d, got %+v (newCursor=%d)", cursor, delta, newCursor)
		}

		// 3. Updating draft: true -> draft: false at a new commit emits task_ready.
		readyYAML := strings.Replace(draftYAML, "draft: true", "draft: false", 1)
		readyGF, err := Parse(".grain/tasks/wip.task.md", []byte(readyYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = readyGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-draft-2",
			Path:    ".grain/tasks/wip.task.md",
		})
		if _, err := s.IndexGrainfile(ctx, readyGF); err != nil {
			t.Fatal(err)
		}
		delta2, _, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(delta2) != 1 || delta2[0].Type != EventTaskReady || delta2[0].CommitSHA != "c-draft-2" {
			t.Fatalf("expected task_ready when draft flipped to false, got %+v", delta2)
		}

		// 4. Updating active draft: false -> draft: true at a new commit emits task_removed (with action="removed").
		reDraftGF, err := Parse(".grain/tasks/wip.task.md", []byte(draftYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = reDraftGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-draft-3",
			Path:    ".grain/tasks/wip.task.md",
		})
		if _, err := s.IndexGrainfile(ctx, reDraftGF); err != nil {
			t.Fatal(err)
		}
		delta3, _, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(delta3) != 1 || delta3[0].Type != EventTaskRemoved || delta3[0].Action != "removed" {
			t.Fatalf("expected task_removed when draft flipped to true, got %+v", delta3)
		}
	})
}

func TestStoreProcessFanoutAndDependsOn(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

		// Subscribe node-eu to repo acme/widgets, and node-us to specific grainfiles (1-to-many process subscription).
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Recipients: []string{"https://github.com/acme/grain-eu"},
			Repos:      []string{"https://github.com/acme/widgets"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-us",
			Recipients: []string{"https://github.com/acme/grain-us"},
			Folders:    []string{"https://github.com/acme/grain-us/tree/main/tasks"},
			Grainfiles: []string{"https://github.com/acme/widgets/blob/main/.grain/processes/smoke.process.grain.md"},
		}); err != nil {
			t.Fatal(err)
		}

		// 1. Primary process smoke.process.grain.md in acme/widgets (no recipient; subscribed by both node-eu and node-us).
		smokeYAML := `---
grainfile: v1
title: Nightly smoke check on push
when:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Run smoke suite.
`
		smokeGF, err := Parse(".grain/processes/smoke.process.grain.md", []byte(smokeYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = smokeGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-1",
			Path:    ".grain/processes/smoke.process.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, smokeGF); err != nil {
			t.Fatal(err)
		}

		// Verify depends_on on .process.grain.md is rejected with GF020.
		badProcYAML := `---
grainfile: v1
title: Invalid process with depends_on
depends_on:
  - https://github.com/acme/widgets/blob/main/.grain/tasks/prep.task.grain.md
when:
  manual: {}
---
Must be rejected.
`
		if _, err := Parse("processes/report.process.grain.md", []byte(badProcYAML)); err == nil || !strings.Contains(err.Error(), CodeSchemaInvalid) {
			t.Fatalf("expected %s for depends_on on .process.grain.md, got %v", CodeSchemaInvalid, err)
		}

		// 2. Concrete task prep.task.grain.md in acme/widgets and dependent task rollout.task.grain.md in acme/grain-us.
		prepYAML := `---
grainfile: v1
title: Prepare widget release artifacts
recipient: https://github.com/acme/grain-eu
---
Prepare release artifacts.
`
		prepGF, err := Parse(".grain/tasks/prep.task.grain.md", []byte(prepYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = prepGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-1",
			Path:    ".grain/tasks/prep.task.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, prepGF); err != nil {
			t.Fatal(err)
		}

		rolloutYAML := `---
grainfile: v1
title: Roll out widget release to US
recipient: https://github.com/acme/grain-us
depends_on:
  - https://github.com/acme/widgets/blob/main/.grain/tasks/prep.task.grain.md
---
Roll out after prep completes.
`
		rolloutGF, err := Parse("tasks/rollout.task.grain.md", []byte(rolloutYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = rolloutGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/grain-us",
			Ref:     "refs/heads/main",
			Commit:  "sha-us-1",
			Path:    "tasks/rollout.task.grain.md",
		})
		rolloutRec, err := s.IndexGrainfile(ctx, rolloutGF)
		if err != nil {
			t.Fatal(err)
		}

		// Both node-eu (by repo/recipient) and node-us (by grainfile_url) receive process_updated for smoke.process.grain.md!
		euInit, _, _ := s.CatchupNode(ctx, "node-eu", true)
		usInit, _, _ := s.CatchupNode(ctx, "node-us", true)
		if len(euInit) != 2 || euInit[0].Type != EventProcessUpdated || euInit[0].GrainfileURL != smokeGF.URL {
			t.Fatalf("expected node-eu to receive process_updated for smoke and task_ready for prep, got %+v", euInit)
		}
		if len(usInit) != 2 {
			t.Fatalf("expected node-us to receive process_updated for smoke and task_ready for rollout, got %+v", usInit)
		}

		// Verify rollout.task.grain.md dependencies are not yet satisfied.
		sat, err := s.DependenciesSatisfied(ctx, rolloutRec.URL)
		if err != nil {
			t.Fatal(err)
		}
		if sat {
			t.Fatal("expected rollout.task.grain.md dependencies to be unsatisfied before prep completes")
		}

		// 3. Evaluate push trigger on acme/widgets -> triggers smoke.process.grain.md!
		trigEvs, err := s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "sha-2", ForgeEvent{
			Source:       TriggerPush,
			Branch:       "refs/heads/main",
			ChangedPaths: []string{"src/widget.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(trigEvs) != 1 || trigEvs[0].GrainfileURL != smokeGF.URL || trigEvs[0].TriggerSource != TriggerPush {
			t.Fatalf("expected 1 triggered event for smoke.process.grain.md, got %+v", trigEvs)
		}

		// 4. Complete prep.task.grain.md -> records dependency_completed for rollout.task.grain.md and satisfies its depends_on!
		depEvs, err := s.RecordDependencyCompleted(ctx, prepGF.URL, "sha-1", "done", "all checks passed")
		if err != nil {
			t.Fatal(err)
		}
		if len(depEvs) != 1 || depEvs[0].Type != EventDependencyCompleted || depEvs[0].GrainfileURL != rolloutGF.URL {
			t.Fatalf("unexpected dependency_completed events: %+v", depEvs)
		}

		sat, err = s.DependenciesSatisfied(ctx, rolloutRec.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !sat {
			t.Fatal("expected rollout.task.grain.md dependencies to be satisfied after prep completed with done")
		}

		usEvents, _, err := s.CatchupNode(ctx, "node-us", true)
		if err != nil {
			t.Fatal(err)
		}
		var sawSmokeTrigger, sawDepDone bool
		for _, ev := range usEvents {
			if ev.Type == EventProcessTriggered && ev.GrainfileURL == smokeGF.URL {
				sawSmokeTrigger = true
			}
			if ev.Type == EventDependencyCompleted && ev.DependencyURL == prepGF.URL && ev.DependencyState == "done" {
				sawDepDone = true
			}
		}
		if !sawSmokeTrigger || !sawDepDone {
			t.Errorf("node-us catchup missing expected events (sawSmokeTrigger=%v, sawDepDone=%v): %+v", sawSmokeTrigger, sawDepDone, usEvents)
		}
	})
}

func slicesContainsAll(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestSubscriptionValidationAndReconnectCursor(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()

		// Invalid subscription URLs are rejected.
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-1",
			Recipients: []string{"https://github.com/acme/widgets/blob/main/file.md"},
		}); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for blob URL in recipients, got %v", err)
		}

		sub1, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-1",
			Owner:      "alice@example.com",
			Recipients: []string{"https://github.com/acme/grain-eu"},
			Repos:      []string{"acme/widgets"},
			Folders:    []string{"https://github.com/acme/mono/tree/main/services/billing"},
		})
		if err != nil {
			t.Fatalf("SetSubscriptions: %v", err)
		}
		if sub1.Revision != 1 || sub1.Repos[0] != "https://github.com/acme/widgets" {
			t.Errorf("unexpected sub1: %+v", sub1)
		}

		if err := s.AdvanceCursor(ctx, "node-1", 42); err != nil {
			t.Fatalf("AdvanceCursor: %v", err)
		}
		// Updating subscriptions preserves the cursor and increments revision.
		sub2, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-1",
			Recipients: []string{"https://github.com/acme/grain-eu"},
		})
		if err != nil {
			t.Fatalf("SetSubscriptions(2): %v", err)
		}
		if sub2.Revision != 2 || sub2.Cursor != 42 || sub2.Owner != "alice@example.com" {
			t.Errorf("expected revision=2, cursor=42, owner preserved; got %+v", sub2)
		}
	})
}

func TestTaskIdempotencyEditsDeletionAndBoundedTriggerWith(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Owner:      "alice@example.com",
			Recipients: []string{"https://github.com/acme/grain-eu"},
		}); err != nil {
			t.Fatal(err)
		}

		// 1. A .task.grain.md pushed on a feature branch (non-default branch) must NOT emit task_ready.
		featTask, err := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ngrainfile: v1\ntitle: One-off task\nrecipient: https://github.com/acme/grain-eu\n---\nRun once.\n"))
		if err != nil {
			t.Fatal(err)
		}
		_ = featTask.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/feat/branch",
			Commit:  "sha-feat-1",
			Path:    ".grain/tasks/one-off.task.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, featTask); err != nil {
			t.Fatal(err)
		}
		if evs, _, err := s.CatchupNode(ctx, "node-eu", true); err != nil || len(evs) != 0 {
			t.Fatalf("expected 0 events for .task.grain.md on non-default branch, got %+v (err=%v)", evs, err)
		}

		// 2. Pushing the same .task.grain.md on main emits task_ready; editing before completion emits updated task_ready.
		mainTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ngrainfile: v1\ntitle: One-off task v1\nrecipient: https://github.com/acme/grain-eu\n---\nRun once.\n"))
		_ = mainTask.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-main-1",
			Path:    ".grain/tasks/one-off.task.grain.md",
		})
		rec1, err := s.IndexGrainfile(ctx, mainTask)
		if err != nil {
			t.Fatal(err)
		}
		evs1, _, _ := s.CatchupNode(ctx, "node-eu", true)
		if len(evs1) != 1 || evs1[0].Type != EventTaskReady || evs1[0].Title != "One-off task v1" {
			t.Fatalf("expected initial task_ready, got %+v", evs1)
		}

		// Edit before completion -> emits updated task_ready.
		editTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ngrainfile: v1\ntitle: One-off task v2\nrecipient: https://github.com/acme/grain-eu\n---\nRun once v2.\n"))
		_ = editTask.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-main-2",
			Path:    ".grain/tasks/one-off.task.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, editTask); err != nil {
			t.Fatal(err)
		}
		evs2, _, _ := s.CatchupNode(ctx, "node-eu", true)
		if len(evs2) != 1 || evs2[0].Type != EventTaskReady || evs2[0].Title != "One-off task v2" {
			t.Fatalf("expected updated task_ready on edit before completion, got %+v", evs2)
		}

		// 3. Complete the task -> subsequent pushes must NOT re-emit task_ready (task idempotency),
		// and deleting a completed task must be a no-op!
		if _, err := s.RecordDependencyCompleted(ctx, rec1.URL, "sha-main-2", "done", "completed"); err != nil {
			t.Fatal(err)
		}
		_, _, _ = s.CatchupNode(ctx, "node-eu", true)

		repushTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ngrainfile: v1\ntitle: One-off task v3\nrecipient: https://github.com/acme/grain-eu\n---\nRun once v3.\n"))
		_ = repushTask.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-main-3",
			Path:    ".grain/tasks/one-off.task.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, repushTask); err != nil {
			t.Fatal(err)
		}
		if evs3, _, err := s.CatchupNode(ctx, "node-eu", true); err != nil || len(evs3) != 0 {
			t.Fatalf("completed task must not re-run on subsequent push, got %+v (err=%v)", evs3, err)
		}
		if err := s.RemoveGrainfile(ctx, "https://github.com/acme/widgets", ".grain/tasks/one-off.task.grain.md"); err != nil {
			t.Fatalf("RemoveGrainfile(completed): %v", err)
		}
		if evs4, _, err := s.CatchupNode(ctx, "node-eu", true); err != nil || len(evs4) != 0 {
			t.Fatalf("deleting completed task must be a no-op, got %+v (err=%v)", evs4, err)
		}

		// 4. Deleting a queued (uncompleted) task emits a task_removed event (with action=removed and state=cancelled).
		qTask, _ := Parse(".grain/tasks/cancel-me.task.grain.md", []byte("---\ngrainfile: v1\ntitle: Cancel me\nrecipient: https://github.com/acme/grain-eu\n---\nBody.\n"))
		_ = qTask.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-q-1",
			Path:    ".grain/tasks/cancel-me.task.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, qTask); err != nil {
			t.Fatal(err)
		}
		_, _, _ = s.CatchupNode(ctx, "node-eu", true)
		if err := s.RemoveGrainfile(ctx, "https://github.com/acme/widgets", ".grain/tasks/cancel-me.task.grain.md"); err != nil {
			t.Fatalf("RemoveGrainfile(queued): %v", err)
		}
		delEvs, _, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil || len(delEvs) != 1 || delEvs[0].Type != EventTaskRemoved || delEvs[0].Action != "removed" || delEvs[0].DependencyState != "cancelled" {
			t.Fatalf("deleting queued task: expected task_removed event, got %+v (err=%v)", delEvs, err)
		}

		// 5. Bounded depends_on chain depth (MaxDependsOnDepth = 8) & cycle safety.
		for i := 0; i <= MaxDependsOnDepth+3; i++ {
			path := fmt.Sprintf(".grain/processes/p%02d.process.md", i)
			var yamlBody string
			if i == 0 {
				yamlBody = "---\ngrainfile: v1\ntitle: Root process\nwhen:\n  push:\n    branches: [\"main\"]\n---\nRoot.\n"
			} else {
				prevURL := fmt.Sprintf("https://github.com/acme/widgets/blob/main/.grain/processes/p%02d.process.md", i-1)
				yamlBody = fmt.Sprintf("---\ngrainfile: v1\ntitle: Step %d\ndepends_on:\n  - %s\nwhen:\n  manual: {}\n---\nStep.\n", i, prevURL)
			}
			pg, err := Parse(path, []byte(yamlBody))
			if err != nil {
				t.Fatalf("Parse(%s): %v", path, err)
			}
			_ = pg.ResolveSource(Source{
				RepoURL: "https://github.com/acme/widgets",
				Ref:     "refs/heads/main",
				Commit:  "sha-chain",
				Path:    path,
			})
			if _, err := s.IndexGrainfile(ctx, pg); err != nil {
				t.Fatalf("IndexGrainfile(%s): %v", path, err)
			}
		}
		chainEvs, err := s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "sha-chain-run", ForgeEvent{
			Source:       TriggerPush,
			Branch:       "main",
			ChangedPaths: []string{"README.md"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Root (depth 0) + MaxDependsOnDepth (8) transitive hops = 9 total events (p00..p08).
		if len(chainEvs) != MaxDependsOnDepth+1 {
			t.Fatalf("expected bounded depends_on chain of %d events (1 root + %d hops), got %d", MaxDependsOnDepth+1, MaxDependsOnDepth, len(chainEvs))
		}
	})
}

func TestStoreSchemaMigrationColumnUpgrade(t *testing.T) {
	runUpgrade := func(t *testing.T, dbURL string) {
		ctx := context.Background()
		var (
			db  *sql.DB
			err error
		)
		if path, ok := strings.CutPrefix(dbURL, "sqlite:"); ok {
			db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		} else {
			db, err = sql.Open("pgx", dbURL)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Create legacy minimal tables missing the newer columns added by dbaccess.AddColumns.
		legacyDDL := []string{
			`CREATE TABLE grainfile_index (
				repo            TEXT    NOT NULL,
				path            TEXT    NOT NULL,
				commit_sha      TEXT    NOT NULL,
				ref             TEXT    NOT NULL DEFAULT '',
				url             TEXT    NOT NULL DEFAULT '',
				kind            TEXT    NOT NULL,
				title           TEXT    NOT NULL,
				recipient       TEXT    NOT NULL,
				requestor       TEXT    NOT NULL DEFAULT '',
				draft           INTEGER NOT NULL DEFAULT 0,
				"references"    TEXT    NOT NULL DEFAULT '[]',
				updated_at      BIGINT  NOT NULL,
				PRIMARY KEY (repo, path, commit_sha)
			)`,
			`CREATE TABLE grainfile_subscriptions (
				node_id         TEXT    PRIMARY KEY,
				owner           TEXT    NOT NULL DEFAULT '',
				recipients      TEXT    NOT NULL DEFAULT '[]',
				repos           TEXT    NOT NULL DEFAULT '[]',
				folders         TEXT    NOT NULL DEFAULT '[]',
				grainfiles      TEXT    NOT NULL DEFAULT '[]',
				cursor          BIGINT  NOT NULL DEFAULT 0,
				revision        BIGINT  NOT NULL DEFAULT 1,
				updated_at      BIGINT  NOT NULL
			)`,
			`CREATE TABLE grainfile_events (
				seq             BIGINT  PRIMARY KEY,
				type            TEXT    NOT NULL,
				repo            TEXT    NOT NULL,
				path            TEXT    NOT NULL DEFAULT '',
				commit_sha      TEXT    NOT NULL DEFAULT '',
				ref             TEXT    NOT NULL DEFAULT '',
				grainfile_url   TEXT    NOT NULL DEFAULT '',
				kind            TEXT    NOT NULL DEFAULT '',
				title           TEXT    NOT NULL DEFAULT '',
				recipient       TEXT    NOT NULL DEFAULT '',
				payload         TEXT    NOT NULL DEFAULT '{}',
				created_at      BIGINT  NOT NULL
			)`,
		}
		for _, stmt := range legacyDDL {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("legacy DDL: %v", err)
			}
		}

		// OpenExisting on legacy schema missing added columns must fail.
		if _, err := OpenExisting(ctx, dbURL); err == nil {
			t.Fatal("OpenExisting on legacy schema missing added columns succeeded, want error")
		}

		// Opening the store must migrate all missing columns cleanly and idempotently.
		s, err := Open(ctx, dbURL)
		if err != nil {
			t.Fatalf("Open on legacy schema: %v", err)
		}
		defer s.Close()

		s2, err := Open(ctx, dbURL)
		if err != nil {
			t.Fatalf("second Open on migrated schema: %v", err)
		}
		s2.Close()

		// Verify queries and writes involving migrated columns (not_before, ready_emitted,
		// completed_*, validation_json, removed, dependency_url, triggered_by) succeed.
		gf, err := Parse("tasks/01-mig.task.md", []byte("---\ngrainfile: v1\ntitle: Migrated task test\nrecipient: https://github.com/acme/agent\nnot_before: \"2026-01-01T00:00:00Z\"\n---\nBody.\n"))
		if err != nil {
			t.Fatal(err)
		}
		_ = gf.ResolveSource(Source{
			RepoURL: "https://github.com/acme/migrated",
			Ref:     "refs/heads/main",
			Commit:  "aaaa1111bbbb2222cccc3333dddd4444eeee5555",
			Path:    "tasks/01-mig.task.md",
		})
		rec, err := s.IndexGrainfile(ctx, gf)
		if err != nil {
			t.Fatalf("IndexGrainfile after migration: %v", err)
		}
		if rec.URL == "" {
			t.Fatal("expected non-empty indexed record URL after schema migration")
		}
		promoted, err := s.PromoteDeferred(ctx, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("PromoteDeferred after migration: %v", err)
		}
		_ = promoted
		if _, err := s.RecordDependencyCompleted(ctx, rec.URL, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", "done", "migrated completion"); err != nil {
			t.Fatalf("RecordDependencyCompleted after migration: %v", err)
		}
	}

	t.Run("sqlite", func(t *testing.T) {
		runUpgrade(t, "sqlite:"+filepath.Join(t.TempDir(), "legacy.db"))
	})
	t.Run("postgres", func(t *testing.T) {
		pg := pgtest.Start(t)
		runUpgrade(t, pg.URL)
	})
}

type fakeRepoTeamResolver struct {
	teams map[string]string
}

func (f *fakeRepoTeamResolver) RepoOwner(_ context.Context, repo string) (string, error) {
	return f.teams[strings.ToLower(strings.TrimSpace(repo))], nil
}

func TestCrossRepoWhenTriggersAndFromTemplateStore(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
		s.TeamResolver = &fakeRepoTeamResolver{
			teams: map[string]string{
				"acme/grain-eu":  "team-eu",
				"acme/widgets":   "team-eu",
				"acme/payments":  "team-eu",
				"other/outsider": "team-other",
			},
		}

		// Subscribe node-eu to acme/grain-eu (central process repo).
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Recipients: []string{"https://github.com/acme/grain-eu"},
			Repos:      []string{"https://github.com/acme/grain-eu"},
		}); err != nil {
			t.Fatal(err)
		}

		// 1. Index a central process in acme/grain-eu with from_template and gate: true
		// watching acme/widgets (folder src/auth/) and acme/payments, plus an unauthorized other/outsider repo.
		centralProcYAML := `---
grainfile: v1
title: Central EU Auth Gate
from_template: /.grain/templates/auth-gate.process.grain.md.jinja2
gate: true
when:
  pull_request:
    - repos:
        - acme/widgets
        - https://github.com/acme/payments
        - other/outsider
      branches: ["main"]
      paths:
        - src/auth/
        - https://github.com/acme/payments/blob/main/schemas/v2.sql
---
Verify auth changes.
`
		procGF, err := Parse("processes/CentralAuthGate.process.grain.md", []byte(centralProcYAML))
		if err != nil {
			t.Fatalf("Parse centralProc: %v", err)
		}
		if err := procGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/grain-eu",
			Ref:     "refs/heads/main",
			Commit:  "sha-eu-1",
			Path:    "processes/CentralAuthGate.process.grain.md",
		}); err != nil {
			t.Fatalf("ResolveSource centralProc: %v", err)
		}
		rec, err := s.IndexGrainfile(ctx, procGF)
		if err != nil {
			t.Fatalf("IndexGrainfile centralProc: %v", err)
		}
		if rec.FromTemplate != "https://github.com/acme/grain-eu/blob/main/.grain/templates/auth-gate.process.grain.md.jinja2" {
			t.Errorf("rec.FromTemplate = %q", rec.FromTemplate)
		}
		_, _, _ = s.CatchupNode(ctx, "node-eu", true)

		// 2. PR webhook on acme/widgets (same team-eu) touching src/auth/login.go triggers the central gate!
		evs, err := s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "pr-head-99", ForgeEvent{
			Source:       TriggerPullRequest,
			Repo:         "https://github.com/acme/widgets",
			Branch:       "refs/heads/main",
			CommitSHA:    "pr-head-99",
			PRNumber:     77,
			ChangedPaths: []string{"src/auth/login.go", "docs/readme.md"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers(acme/widgets): %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("expected 1 process_triggered event on acme/widgets, got %+v", evs)
		}
		got := evs[0]
		if got.Type != EventProcessTriggered ||
			!got.Gate ||
			got.Repo != "https://github.com/acme/widgets" ||
			got.Branch != "main" ||
			got.CommitSHA != "pr-head-99" ||
			got.PRNumber != 77 ||
			!reflect.DeepEqual(got.Paths, []string{"src/auth/login.go"}) ||
			got.FromTemplate != rec.FromTemplate {
			t.Errorf("unexpected process_triggered event: %+v", got)
		}

		// Verify node-eu receives the cross-repo process_triggered event via CatchupNode.
		caught, _, err := s.CatchupNode(ctx, "node-eu", true)
		if err != nil || len(caught) != 1 || caught[0].Seq != got.Seq {
			t.Errorf("expected node-eu catchup to receive cross-repo process_triggered event, got %+v (err=%v)", caught, err)
		}

		// 3. PR webhook on other/outsider (different team) must NOT trigger the central gate in team-eu!
		outsiderEvs, err := s.EvaluateTriggers(ctx, "https://github.com/other/outsider", "pr-outsider-1", ForgeEvent{
			Source:       TriggerPullRequest,
			Repo:         "https://github.com/other/outsider",
			Branch:       "main",
			CommitSHA:    "pr-outsider-1",
			PRNumber:     12,
			ChangedPaths: []string{"src/auth/login.go"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers(other/outsider): %v", err)
		}
		if len(outsiderEvs) != 0 {
			t.Errorf("expected 0 events for cross-team repository other/outsider, got %+v", outsiderEvs)
		}
	})
}

func TestCanonicalDeploymentURLRoutingAndReplayForNode(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.HubURL = "https://hub.example.com"

		// Subscribe deployment "payments-prod" without explicitly listing its canonical URL.
		sub, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID: "payments-prod",
			Owner:  "alice@example.com",
			Repos:  []string{"https://github.com/acme/payments"},
		})
		if err != nil {
			t.Fatalf("SetSubscriptions: %v", err)
		}
		wantCanon := "https://hub.example.com/deployments/payments-prod"
		if !slices.Contains(sub.Recipients, wantCanon) {
			t.Fatalf("sub.Recipients = %v, want to include %q", sub.Recipients, wantCanon)
		}

		// Index a task in another repo targeting https://hub.example.com/deployments/payments-prod
		taskGF := &Grainfile{
			Kind:      KindTask,
			Repo:      "https://github.com/acme/caller-repo",
			Ref:       "refs/heads/main",
			Commit:    "c111",
			Path:      ".grain/tasks/2026-10-20-call-payments.task.grain.md",
			Title:     "Audit payment webhook retries",
			Recipient: wantCanon,
		}
		if _, err := s.IndexGrainfile(ctx, taskGF); err != nil {
			t.Fatalf("IndexGrainfile(task): %v", err)
		}

		// Index a standing process in acme/payments
		procGF, err := Parse(".grain/processes/ci.process.grain.md", []byte("---\ngrainfile: v1\ntitle: Payments CI check\nwhen:\n  pull_request:\n    branches: [\"main\"]\n---\n"))
		if err != nil {
			t.Fatalf("Parse(process): %v", err)
		}
		procGF.Repo = "https://github.com/acme/payments"
		procGF.Ref = "refs/heads/main"
		procGF.Commit = "c222"
		if _, err := s.IndexGrainfile(ctx, procGF); err != nil {
			t.Fatalf("IndexGrainfile(process): %v", err)
		}

		// Initial catch-up advances cursor past both events.
		evs, cur, err := s.CatchupNode(ctx, "payments-prod", true)
		if err != nil {
			t.Fatalf("CatchupNode initial: %v", err)
		}
		if len(evs) != 2 || cur == 0 {
			t.Fatalf("initial CatchupNode events = %d (cursor=%d), want 2", len(evs), cur)
		}

		// Subsequent catch-up returns 0 events because cursor is at high-water mark.
		evsAfter, _, err := s.CatchupNode(ctx, "payments-prod", true)
		if err != nil {
			t.Fatalf("CatchupNode second: %v", err)
		}
		if len(evsAfter) != 0 {
			t.Fatalf("second CatchupNode events = %d, want 0", len(evsAfter))
		}

		// Host replacement replays standing processes and active tasks by resetting cursor to 0.
		replayed, err := s.ReplayForNode(ctx, "payments-prod")
		if err != nil {
			t.Fatalf("ReplayForNode: %v", err)
		}
		if len(replayed) != 2 {
			t.Fatalf("ReplayForNode returned %d events, want 2: %+v", len(replayed), replayed)
		}

		// Reconnecting replacement host catches up from cursor=0 and receives both again.
		evsReplayed, newCur, err := s.CatchupNode(ctx, "payments-prod", true)
		if err != nil {
			t.Fatalf("CatchupNode after ReplayForNode: %v", err)
		}
		if len(evsReplayed) != 2 || newCur == 0 {
			t.Fatalf("CatchupNode after ReplayForNode events = %d (cursor=%d), want 2", len(evsReplayed), newCur)
		}
	})
}

func TestMultiRepoGateCrossRepoAuthorizationAndCoTriggers(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()

		// Configure team resolver:
		// - github.com/acme/control-plane -> team-core
		// - github.com/acme/service-a     -> team-core (same team, authorized)
		// - github.com/acme/service-b     -> team-other (different team, unauthorized)
		// - github.com/evil/attacker      -> team-evil (different team, unauthorized)
		s.TeamResolver = &fakeRepoTeamResolver{
			teams: map[string]string{
				"acme/control-plane":            "team-core",
				"github.com/acme/control-plane": "team-core",
				"acme/service-a":                "team-core",
				"github.com/acme/service-a":     "team-core",
				"acme/service-b":                "team-other",
				"github.com/acme/service-b":     "team-other",
				"evil/attacker":                 "team-evil",
				"github.com/evil/attacker":      "team-evil",
			},
		}

		// 1. Multi-repo gate process in control-plane watching control-plane, service-a (same team), and service-b (other team).
		gateRaw := `---
grainfile: v1
title: Multi-Repo Gate Watcher
gate: true
when:
  pull_request:
    branches: ["main"]
    paths:
      - "internal/"
      - "https://github.com/acme/service-a/tree/main/pkg/"
      - "https://github.com/acme/service-b/tree/main/pkg/"
    ignore:
      - "internal/generated/"
      - "pkg/mocks/"
      - "pkg/doc.go"
---
Gate verification across repos.
`
		gateGF, err := Parse(".grain/multi-gate.process.grain.md", []byte(gateRaw))
		if err != nil {
			t.Fatalf("Parse gateGF: %v", err)
		}
		_ = gateGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/control-plane",
			Ref:     "refs/heads/main",
			Commit:  "1111111111111111111111111111111111111111",
			Path:    ".grain/multi-gate.process.grain.md",
		})
		if _, err := s.IndexGrainfile(ctx, gateGF); err != nil {
			t.Fatalf("IndexGrainfile gateGF: %v", err)
		}

		// 2. Unauthorized cross-team gate process in evil/attacker attempting to co-trigger via depends_on on gateGF!
		evilRaw := `---
grainfile: v1
title: Evil Co-Trigger Gate Process
recipient: https://hub.example.com/deployments/evil-node
gate: true
when:
  manual: {}
depends_on:
  - "https://github.com/acme/control-plane/blob/main/.grain/multi-gate.process.grain.md"
---
Should never co-trigger a gate across teams.
`
		evilGF, err := Parse(".grain/evil.process.md", []byte(evilRaw))
		if err != nil {
			t.Fatalf("Parse evilGF: %v", err)
		}
		_ = evilGF.ResolveSource(Source{
			RepoURL: "https://github.com/evil/attacker",
			Ref:     "refs/heads/main",
			Commit:  "2222222222222222222222222222222222222222",
			Path:    ".grain/evil.process.md",
		})
		if _, err := s.IndexGrainfile(ctx, evilGF); err != nil {
			t.Fatalf("IndexGrainfile evilGF: %v", err)
		}

		// 3. Authorized same-team process in service-a co-triggering via depends_on on gateGF.
		allyRaw := `---
grainfile: v1
title: Authorized Same-Team Co-Trigger
recipient: https://hub.example.com/deployments/core-ally
when:
  manual: {}
depends_on:
  - "https://github.com/acme/control-plane/blob/main/.grain/multi-gate.process.grain.md"
---
Authorized same-team co-trigger.
`
		allyGF, err := Parse(".grain/ally.process.md", []byte(allyRaw))
		if err != nil {
			t.Fatalf("Parse allyGF: %v", err)
		}
		_ = allyGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/service-a",
			Ref:     "refs/heads/main",
			Commit:  "3333333333333333333333333333333333333333",
			Path:    ".grain/ally.process.md",
		})
		if _, err := s.IndexGrainfile(ctx, allyGF); err != nil {
			t.Fatalf("IndexGrainfile allyGF: %v", err)
		}

		// Case A: PR in service-a (same team) touching pkg/api.go -> triggers gateGF AND allyGF, but NOT evilGF!
		evsA, err := s.EvaluateTriggers(ctx, "https://github.com/acme/service-a", "aaaa1111", ForgeEvent{
			Source:       TriggerPullRequest,
			Branch:       "main",
			ChangedPaths: []string{"pkg/api.go"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers service-a: %v", err)
		}
		if len(evsA) != 2 {
			t.Fatalf("EvaluateTriggers service-a got %d events, want 2 (gateGF + allyGF): %+v", len(evsA), evsA)
		}
		for _, ev := range evsA {
			if strings.Contains(ev.Repo, "evil/attacker") {
				t.Fatalf("unauthorized cross-team process co-triggered: %+v", ev)
			}
		}

		// Case B: PR in service-a touching only ignored cross-repo folder pkg/mocks/foo.go and ignored blob pkg/doc.go -> 0 triggers.
		evsB, err := s.EvaluateTriggers(ctx, "https://github.com/acme/service-a", "aaaa2222", ForgeEvent{
			Source:       TriggerPullRequest,
			Branch:       "main",
			ChangedPaths: []string{"pkg/mocks/foo.go", "pkg/doc.go"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers service-a ignored: %v", err)
		}
		if len(evsB) != 0 {
			t.Fatalf("EvaluateTriggers service-a ignored got %d events, want 0: %+v", len(evsB), evsB)
		}

		// Case C: PR in service-b (different team) touching pkg/api.go -> blocked by cross-repo team authorization!
		evsC, err := s.EvaluateTriggers(ctx, "https://github.com/acme/service-b", "bbbb1111", ForgeEvent{
			Source:       TriggerPullRequest,
			Branch:       "main",
			ChangedPaths: []string{"pkg/api.go"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers service-b: %v", err)
		}
		if len(evsC) != 0 {
			t.Fatalf("EvaluateTriggers service-b (unauthorized cross-team) got %d events, want 0: %+v", len(evsC), evsC)
		}

		// Case D: PR in control-plane with path traversal attempt -> rejected, 0 triggers.
		evsD, err := s.EvaluateTriggers(ctx, "https://github.com/acme/control-plane", "cccc1111", ForgeEvent{
			Source:       TriggerPullRequest,
			Branch:       "main",
			ChangedPaths: []string{"internal/generated/../../internal/secret.go", "docs/%2e%2e/internal/secret.go"},
		})
		if err != nil {
			t.Fatalf("EvaluateTriggers traversal: %v", err)
		}
		if len(evsD) != 0 {
			t.Fatalf("EvaluateTriggers traversal got %d events, want 0: %+v", len(evsD), evsD)
		}
	})
}

func TestCapabilityIndexingAndEvents(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.Now = func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

		// 1. Index a .capability.grain.md file -> emits capability_updated.
		capRaw := []byte("---\ngrainfile: v1\ntitle: GCP service-account key\nreferences:\n  - /docs/widget-schema.md\n---\nGrants GOOGLE_APPLICATION_CREDENTIALS.\n")
		capGF, err := Parse(".grain/capabilities/gcp-key.capability.grain.md", capRaw)
		if err != nil {
			t.Fatalf("Parse capability: %v", err)
		}
		capGF.Repo = "https://github.com/acme/widgets"
		capGF.Ref = "refs/heads/main"
		capGF.Commit = "cap-sha-1"
		capRec, err := s.IndexGrainfile(ctx, capGF)
		if err != nil {
			t.Fatalf("IndexGrainfile(capability): %v", err)
		}
		if capRec.Kind != KindCapability {
			t.Fatalf("capRec.Kind = %q, want %q", capRec.Kind, KindCapability)
		}

		// 2. Index a .task.grain.md and .process.grain.md referencing the capability.
		taskRaw := []byte("---\ngrainfile: v1\ntitle: Task with capability\nrecipient: https://github.com/acme/grain-eu\ncapabilities:\n  - /.grain/capabilities/gcp-key.capability.grain.md#agent-usage\n---\nTask body.\n")
		taskGF, err := Parse(".grain/tasks/use-cap.task.grain.md", taskRaw)
		if err != nil {
			t.Fatalf("Parse task: %v", err)
		}
		taskGF.Repo = "https://github.com/acme/widgets"
		taskGF.Ref = "refs/heads/main"
		taskGF.Commit = "task-sha-1"
		taskRec, err := s.IndexGrainfile(ctx, taskGF)
		if err != nil {
			t.Fatalf("IndexGrainfile(task): %v", err)
		}
		wantCapURL := "https://github.com/acme/widgets/blob/main/.grain/capabilities/gcp-key.capability.grain.md#agent-usage"
		wantPinnedCapURL := "https://github.com/acme/widgets/blob/task-sha-1/.grain/capabilities/gcp-key.capability.grain.md#agent-usage"
		if !reflect.DeepEqual(taskRec.Capabilities, []string{wantCapURL}) {
			t.Fatalf("taskRec.Capabilities = %v, want [%s]", taskRec.Capabilities, wantCapURL)
		}
		if !slices.Contains(taskRec.AuthorizedReferences, wantPinnedCapURL) {
			t.Fatalf("taskRec.AuthorizedReferences = %v, want to include %s", taskRec.AuthorizedReferences, wantPinnedCapURL)
		}

		// 3. Mark capability as draft -> emits capability_removed.
		capDraftGF := *capGF
		capDraftGF.Commit = "cap-sha-2"
		capDraftGF.Draft = true
		if _, err := s.IndexGrainfile(ctx, &capDraftGF); err != nil {
			t.Fatalf("IndexGrainfile(capability draft): %v", err)
		}

		// 4. Re-activate capability and then delete via RemoveGrainfile -> emits capability_updated then capability_removed.
		capActiveGF := *capGF
		capActiveGF.Commit = "cap-sha-3"
		capActiveGF.Draft = false
		if _, err := s.IndexGrainfile(ctx, &capActiveGF); err != nil {
			t.Fatalf("IndexGrainfile(capability re-active): %v", err)
		}
		if err := s.RemoveGrainfile(ctx, "https://github.com/acme/widgets", ".grain/capabilities/gcp-key.capability.grain.md"); err != nil {
			t.Fatalf("RemoveGrainfile(capability): %v", err)
		}

		evs, err := s.EventsSince(ctx, 0)
		if err != nil {
			t.Fatalf("EventsSince: %v", err)
		}
		var types []string
		for _, ev := range evs {
			types = append(types, ev.Type)
			if ev.Type == EventTaskReady && !reflect.DeepEqual(ev.Capabilities, []string{wantCapURL}) {
				t.Errorf("task_ready event Capabilities = %v, want [%s]", ev.Capabilities, wantCapURL)
			}
		}
		wantTypes := []string{
			EventCapabilityUpdated,
			EventTaskReady,
			EventCapabilityRemoved,
			EventCapabilityUpdated,
			EventCapabilityRemoved,
		}
		if !reflect.DeepEqual(types, wantTypes) {
			t.Fatalf("event types = %v, want %v", types, wantTypes)
		}
	})
}
