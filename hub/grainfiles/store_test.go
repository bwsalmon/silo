package grainfiles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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
		}); err != nil {
			t.Fatalf("SetSubscriptions: %v", err)
		}

		// 1. Index a draft: true task -> must NOT appear in active events.
		draftYAML := `---
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
title: Deferred security gate
recipient: https://github.com/acme/grain-eu
not_before: "2026-10-25T09:00:00Z"
gate: true
on:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Check security.
`
		defProc, err := Parse(".grain/processes/sec.process.md", []byte(defProcYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = defProc.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "c-def-1",
			Path:    ".grain/processes/sec.process.md",
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
	})
}

func TestStoreTriggerWithAndDependsOn(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		s.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

		// Subscribe node-eu to recipient grain-eu, and node-us to recipient grain-us.
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-eu",
			Recipients: []string{"https://github.com/acme/grain-eu"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetSubscriptions(ctx, Subscriptions{
			NodeID:     "node-us",
			Recipients: []string{"https://github.com/acme/grain-us"},
			Grainfiles: []string{"https://github.com/acme/widgets/blob/main/.grain/processes/smoke.process.md"},
		}); err != nil {
			t.Fatal(err)
		}

		// 1. Primary process smoke.process.md in acme/widgets targeting grain-eu.
		smokeYAML := `---
title: Nightly smoke check on push
recipient: https://github.com/acme/grain-eu
on:
  push:
    branches: ["main"]
    paths: ["src/**"]
---
Run smoke suite.
`
		smokeGF, err := Parse(".grain/processes/smoke.process.md", []byte(smokeYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = smokeGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/widgets",
			Ref:     "refs/heads/main",
			Commit:  "sha-1",
			Path:    ".grain/processes/smoke.process.md",
		})
		if _, err := s.IndexGrainfile(ctx, smokeGF); err != nil {
			t.Fatal(err)
		}

		// 2. Co-triggered process report.process.md in acme/grain-us with trigger_with and depends_on pointing to smoke.process.md.
		reportYAML := `---
title: Publish qualification report
recipient: https://github.com/acme/grain-us
trigger_with:
  - https://github.com/acme/widgets/blob/main/.grain/processes/smoke.process.md
depends_on:
  - https://github.com/acme/widgets/blob/main/.grain/processes/smoke.process.md
---
Summarize smoke results.
`
		reportGF, err := Parse("processes/report.process.md", []byte(reportYAML))
		if err != nil {
			t.Fatal(err)
		}
		_ = reportGF.ResolveSource(Source{
			RepoURL: "https://github.com/acme/grain-us",
			Ref:     "refs/heads/main",
			Commit:  "sha-us-1",
			Path:    "processes/report.process.md",
		})
		reportRec, err := s.IndexGrainfile(ctx, reportGF)
		if err != nil {
			t.Fatal(err)
		}

		// Advance cursors past initial process_updated events.
		_, _, _ = s.CatchupNode(ctx, "node-eu", true)
		_, _, _ = s.CatchupNode(ctx, "node-us", true)

		// Verify report.process.md dependencies are not yet satisfied.
		sat, err := s.DependenciesSatisfied(ctx, reportRec.URL)
		if err != nil {
			t.Fatal(err)
		}
		if sat {
			t.Fatal("expected report.process.md dependencies to be unsatisfied before smoke completes")
		}

		// 3. Evaluate push trigger on acme/widgets -> triggers both smoke.process.md AND report.process.md (via trigger_with)!
		trigEvs, err := s.EvaluateTriggers(ctx, "https://github.com/acme/widgets", "sha-2", ForgeEvent{
			Source:       TriggerPush,
			Branch:       "refs/heads/main",
			ChangedPaths: []string{"src/widget.go"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(trigEvs) != 2 {
			t.Fatalf("expected 2 triggered events (primary + trigger_with co-trigger), got %+v", trigEvs)
		}
		if trigEvs[0].GrainfileURL != smokeGF.URL || trigEvs[0].TriggerSource != TriggerPush {
			t.Errorf("unexpected primary trigger event: %+v", trigEvs[0])
		}
		if trigEvs[1].GrainfileURL != reportGF.URL || trigEvs[1].TriggerSource != TriggerWith || trigEvs[1].TriggeredBy != smokeGF.URL {
			t.Errorf("unexpected co-trigger event: %+v", trigEvs[1])
		}

		// 4. Complete smoke.process.md -> records dependency_completed for node-us and satisfies report.process.md's depends_on!
		depEvs, err := s.RecordDependencyCompleted(ctx, smokeGF.URL, "sha-2", "done", "all checks passed")
		if err != nil {
			t.Fatal(err)
		}
		if len(depEvs) != 1 || depEvs[0].Type != EventDependencyCompleted || depEvs[0].Recipient != "https://github.com/acme/grain-us" {
			t.Fatalf("unexpected dependency_completed events: %+v", depEvs)
		}

		sat, err = s.DependenciesSatisfied(ctx, reportRec.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !sat {
			t.Fatal("expected report.process.md dependencies to be satisfied after smoke completed with done")
		}

		usEvents, _, err := s.CatchupNode(ctx, "node-us", true)
		if err != nil {
			t.Fatal(err)
		}
		var sawCoTrigger, sawDepDone bool
		for _, ev := range usEvents {
			if ev.Type == EventProcessTriggered && ev.TriggerSource == TriggerWith && ev.GrainfileURL == reportGF.URL {
				sawCoTrigger = true
			}
			if ev.Type == EventDependencyCompleted && ev.DependencyURL == smokeGF.URL && ev.DependencyState == "done" {
				sawDepDone = true
			}
		}
		if !sawCoTrigger || !sawDepDone {
			t.Errorf("node-us catchup missing expected events (sawCoTrigger=%v, sawDepDone=%v): %+v", sawCoTrigger, sawDepDone, usEvents)
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
		featTask, err := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ntitle: One-off task\nrecipient: https://github.com/acme/grain-eu\n---\nRun once.\n"))
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
		mainTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ntitle: One-off task v1\nrecipient: https://github.com/acme/grain-eu\n---\nRun once.\n"))
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
		editTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ntitle: One-off task v2\nrecipient: https://github.com/acme/grain-eu\n---\nRun once v2.\n"))
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

		repushTask, _ := Parse(".grain/tasks/one-off.task.grain.md", []byte("---\ntitle: One-off task v3\nrecipient: https://github.com/acme/grain-eu\n---\nRun once v3.\n"))
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

		// 4. Deleting a queued (uncompleted) task emits a cancellation event (dependency_completed with state=cancelled).
		qTask, _ := Parse(".grain/tasks/cancel-me.task.grain.md", []byte("---\ntitle: Cancel me\nrecipient: https://github.com/acme/grain-eu\n---\nBody.\n"))
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
		if err != nil || len(delEvs) != 1 || delEvs[0].Type != EventDependencyCompleted || delEvs[0].DependencyState != "cancelled" {
			t.Fatalf("deleting queued task: expected cancelled event, got %+v (err=%v)", delEvs, err)
		}

		// 5. Bounded trigger_with chain depth (MaxTriggerWithDepth = 8) & cycle safety.
		for i := 0; i <= MaxTriggerWithDepth+3; i++ {
			path := fmt.Sprintf(".grain/processes/p%02d.process.grain.md", i)
			var yamlBody string
			if i == 0 {
				yamlBody = "---\ntitle: Root process\nrecipient: https://github.com/acme/grain-eu\nwhen:\n  push:\n    branches: [\"main\"]\n---\nRoot.\n"
			} else {
				prevURL := fmt.Sprintf("https://github.com/acme/widgets/blob/main/.grain/processes/p%02d.process.grain.md", i-1)
				yamlBody = fmt.Sprintf("---\ntitle: Step %d\nrecipient: https://github.com/acme/grain-eu\ntrigger_with:\n  - %s\n---\nStep.\n", i, prevURL)
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
		// Root (depth 0) + MaxTriggerWithDepth (8) transitive hops = 9 total events (p00..p08).
		if len(chainEvs) != MaxTriggerWithDepth+1 {
			t.Fatalf("expected bounded trigger_with chain of %d events (1 root + %d hops), got %d", MaxTriggerWithDepth+1, MaxTriggerWithDepth, len(chainEvs))
		}
	})
}
