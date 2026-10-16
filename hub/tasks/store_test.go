package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

// eachStore runs f against SQLite and, where its binaries are
// installed, PostgreSQL.
func eachStore(t *testing.T, f func(t *testing.T, s *Store)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(context.Background(), "sqlite:"+t.TempDir()+"/hub.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		f(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		pg := pgtest.Start(t)
		s, err := Open(context.Background(), pg.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		// Opening again finds the table there already.
		s2, err := Open(context.Background(), pg.URL)
		if err != nil {
			t.Fatal(err)
		}
		s2.Close()
		f(t, s)
	})
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ticking is a clock that moves on a millisecond each time it is read,
// so that tasks created one after another are ordered. The dispatcher
// reads it from each task's stream at once.
func ticking() func() time.Time {
	var mu sync.Mutex
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Millisecond)
		return now
	}
}

func spec(team, repo, prompt string) Spec {
	return Spec{Team: team, Repo: repo, Prompt: prompt, Submitter: "alice@example.com"}
}

func TestCreate(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		s.Now = ticking()
		task, err := s.Create(ctx, Spec{
			Team: "infra", Repo: "Bwsalmon/Silo", Prompt: "  Fix the parser\nIt drops a byte.", Submitter: "alice@example.com",
			Constraints: Constraints{Labels: map[string]string{"zone": "eu"}, Node: "ws-1"},
		})
		check(t, err)
		if task.State != Queued || task.Repo != "bwsalmon/silo" || task.Title != "Fix the parser" || !strings.HasPrefix(task.ID, "t-") {
			t.Errorf("created %+v", task)
		}
		got, err := s.Get(ctx, task.ID)
		check(t, err)
		if got.Prompt != task.Prompt || got.Constraints.Labels["zone"] != "eu" || got.Constraints.Node != "ws-1" ||
			!got.Created.Equal(task.Created) || got.State != Queued || len(got.Excluded) != 0 || got.ApprovedBy != "" {
			t.Errorf("read back %+v, want %+v", got, task)
		}
		approved, err := s.Create(ctx, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "x", Submitter: "alice@example.com", ApprovedBy: " Bob@Example.com "})
		check(t, err)
		if got, err := s.Get(ctx, approved.ID); err != nil || got.ApprovedBy != "bob@example.com" {
			t.Errorf("read back an approved task: %+v, %v", got, err)
		}
		if _, err := s.Get(ctx, "t-nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) = %v, want ErrNotFound", err)
		}
		for _, bad := range []Spec{
			spec("infra", "silo", "x"),
			spec("", "bwsalmon/silo", "x"),
			spec("infra", "bwsalmon/silo", " "),
			{Team: "infra", Prompt: "x"},
			{Team: "infra", Prompt: "x", Submitter: "a@b", Constraints: Constraints{Node: "has space"}},
			{Team: "infra", Prompt: "x", Submitter: "a@b", Title: strings.Repeat("x", maxTitle+1)},
			{Team: "infra", Prompt: "x", Submitter: "a@b", Constraints: Constraints{Labels: manyLabels(maxLabels + 1)}},
			{Team: "infra", Prompt: "x", Submitter: "a@b", Constraints: Constraints{Labels: map[string]string{strings.Repeat("k", maxLabelKey+1): "v"}}},
			{Team: "infra", Prompt: "x", Submitter: "a@b", Constraints: Constraints{Labels: map[string]string{"k": strings.Repeat("v", maxLabelValue+1)}}},
			{Team: "infra", Prompt: "x", Submitter: "a@b", ApprovedBy: "A@B"},
		} {
			if _, err := s.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("Create(%+v) = %v, want ErrInvalid", bad, err)
			}
		}
	})
}

func TestLifecycle(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		s.Now = ticking()
		task, err := s.Create(ctx, spec("infra", "bwsalmon/silo", "one"))
		check(t, err)

		if _, err := s.Accept(ctx, task.ID, "x"); !errors.Is(err, ErrState) {
			t.Errorf("accepting a queued task: %v, want ErrState", err)
		}
		got, err := s.Offer(ctx, task.ID, "ws-1", "alice@example.com")
		check(t, err)
		if got.State != Offered || got.Node != "ws-1" || got.NodeOwner != "alice@example.com" || got.Attempts != 1 {
			t.Errorf("offered: %+v", got)
		}
		if _, err := s.Offer(ctx, task.ID, "ws-2", "alice@example.com"); !errors.Is(err, ErrState) {
			t.Errorf("offering twice: %v, want ErrState", err)
		}
		// Refused: back to the queue without that node.
		got, err = s.Requeue(ctx, task.ID, "busy", "ws-1")
		check(t, err)
		if got.State != Queued || got.Node != "" || got.NodeOwner != "" || got.Reason != "busy" || strings.Join(got.Excluded, ",") != "ws-1" {
			t.Errorf("requeued: %+v", got)
		}
		_, err = s.Offer(ctx, task.ID, "ws-2", "alice@example.com")
		check(t, err)
		got, err = s.Accept(ctx, task.ID, "local-7")
		check(t, err)
		if got.State != Running || got.NodeTask != "local-7" || got.Attempts != 2 {
			t.Errorf("accepted: %+v", got)
		}
		got, err = s.Lose(ctx, task.ID, "gone")
		check(t, err)
		if got.State != Lost || got.Node != "ws-2" {
			t.Errorf("lost: %+v", got)
		}
		// Its node comes back and resumes it.
		got, err = s.Accept(ctx, task.ID, "")
		check(t, err)
		if got.State != Running || got.NodeTask != "local-7" || got.Reason != "" {
			t.Errorf("resumed: %+v", got)
		}
		if _, err := s.Finish(ctx, task.ID, Cancelled, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("finishing as cancelled: %v, want ErrInvalid", err)
		}
		got, err = s.Finish(ctx, task.ID, Done, "")
		check(t, err)
		if got.State != Done || !got.State.Terminal() {
			t.Errorf("finished: %+v", got)
		}
		if _, err := s.Cancel(ctx, task.ID, ""); !errors.Is(err, ErrState) {
			t.Errorf("cancelling a finished task: %v, want ErrState", err)
		}
		if _, err := s.Get(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCancel(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		s.Now = ticking()
		queued, err := s.Create(ctx, spec("infra", "bwsalmon/silo", "queued"))
		check(t, err)
		lost, err := s.Create(ctx, spec("infra", "bwsalmon/silo", "lost"))
		check(t, err)
		_, err = s.Offer(ctx, lost.ID, "ws-1", "alice@example.com")
		check(t, err)
		_, err = s.Accept(ctx, lost.ID, "")
		check(t, err)
		_, err = s.Lose(ctx, lost.ID, "gone")
		check(t, err)

		got, err := s.Cancel(ctx, queued.ID, "by alice")
		check(t, err)
		if got.State != Cancelled || got.CancelPending {
			t.Errorf("cancelled while queued: %+v; no node to tell", got)
		}
		got, err = s.Cancel(ctx, lost.ID, "by alice")
		check(t, err)
		if got.State != Cancelled || !got.CancelPending {
			t.Errorf("cancelled while lost: %+v; its node is to be told", got)
		}
		ts, err := s.Dispatchable(ctx)
		check(t, err)
		if len(ts) != 1 || ts[0].ID != lost.ID {
			t.Errorf("dispatchable: %v, want the cancel to deliver", ts)
		}
		check(t, s.CancelDelivered(ctx, lost.ID))
		if ts, _ := s.Dispatchable(ctx); len(ts) != 0 {
			t.Errorf("dispatchable after delivery: %v", ts)
		}
	})
}

func TestDispatchableAndRecover(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		s.Now = ticking()
		var ids []string
		for _, p := range []string{"a", "b", "c", "d"} {
			task, err := s.Create(ctx, spec("infra", "bwsalmon/silo", p))
			check(t, err)
			ids = append(ids, task.ID)
		}
		// a offered, b running, c queued, d done.
		_, err := s.Offer(ctx, ids[0], "ws-1", "alice@example.com")
		check(t, err)
		_, err = s.Offer(ctx, ids[1], "ws-1", "alice@example.com")
		check(t, err)
		_, err = s.Accept(ctx, ids[1], "")
		check(t, err)
		_, err = s.Offer(ctx, ids[3], "ws-1", "alice@example.com")
		check(t, err)
		_, err = s.Finish(ctx, ids[3], Done, "")
		check(t, err)

		check(t, s.Recover(ctx, "restarted"))
		ts, err := s.Dispatchable(ctx)
		check(t, err)
		var got []string
		for _, task := range ts {
			got = append(got, task.Prompt+":"+string(task.State))
		}
		if strings.Join(got, " ") != "a:queued b:lost c:queued" {
			t.Errorf("after Recover, dispatchable %v, want a requeued, b lost, c queued, oldest first", got)
		}

		all, err := s.List(ctx, Filter{})
		check(t, err)
		if len(all) != 4 || all[0].Prompt != "d" {
			t.Errorf("List = %d tasks, first %q; want 4, newest first", len(all), all[0].Prompt)
		}
		if ts, _ := s.List(ctx, Filter{States: []State{Done}}); len(ts) != 1 || ts[0].ID != ids[3] {
			t.Errorf("List(done) = %v", ts)
		}
		if ts, _ := s.List(ctx, Filter{Teams: []string{}}); len(ts) != 0 {
			t.Errorf("List(no teams) = %v, want none", ts)
		}
		if ts, _ := s.List(ctx, Filter{Teams: []string{"infra"}, Limit: 2}); len(ts) != 2 {
			t.Errorf("List(infra, limit 2) = %d tasks", len(ts))
		}

		// A human requeues the lost task, clearing nothing but its node.
		got2, err := s.Retry(ctx, ids[1])
		check(t, err)
		if got2.State != Queued || got2.Node != "" {
			t.Errorf("retried: %+v", got2)
		}
		if _, err := s.Retry(ctx, ids[3]); !errors.Is(err, ErrState) {
			t.Errorf("retrying a finished task: %v, want ErrState", err)
		}
	})
}

func TestFair(t *testing.T) {
	var ts []Task
	for _, x := range []string{"a1", "a2", "a3", "b1", "c1", "b2"} {
		ts = append(ts, Task{Spec: Spec{ID: x, Team: x[:1]}})
	}
	var got []string
	for _, task := range fair(ts) {
		got = append(got, task.ID)
	}
	if strings.Join(got, " ") != "a1 b1 c1 a2 b2 a3" {
		t.Errorf("fair = %v, want one from each team in turn, oldest first", got)
	}
}

// manyLabels is n distinct label constraints.
func manyLabels(n int) map[string]string {
	l := make(map[string]string, n)
	for i := range n {
		l[fmt.Sprint("l", i)] = "v"
	}
	return l
}

// TestAddedColumns: a tasks table from before node_owner and
// approved_by gains them, empty on its tasks, and OpenExisting refuses
// one without either.
func TestAddedColumns(t *testing.T) {
	for _, c := range added {
		t.Run(c.Name, func(t *testing.T) { testAddedColumn(t, c.Name) })
	}
}

func testAddedColumn(t *testing.T, column string) {
	run := func(t *testing.T, url string) {
		ctx := context.Background()
		s, err := Open(ctx, url)
		check(t, err)
		task, err := s.Create(ctx, spec("infra", "bwsalmon/silo", "one"))
		check(t, err)
		_, err = s.Offer(ctx, task.ID, "ws-1", "alice@example.com")
		check(t, err)
		_, err = s.db.ExecContext(ctx, `ALTER TABLE tasks DROP COLUMN `+column)
		check(t, err)
		s.Close()
		if _, err := OpenExisting(ctx, url); err == nil || !strings.Contains(err.Error(), column) {
			t.Errorf("OpenExisting on the old table: %v", err)
		}
		for range 2 {
			s, err = Open(ctx, url)
			check(t, err)
			got, err := s.Get(ctx, task.ID)
			check(t, err)
			s.Close()
			if got.Node != "ws-1" || (column == "node_owner" && got.NodeOwner != "") || got.ApprovedBy != "" {
				t.Fatalf("migrated task %+v", got)
			}
		}
		s, err = OpenExisting(ctx, url)
		check(t, err)
		s.Close()
	}
	t.Run("sqlite", func(t *testing.T) { run(t, "sqlite:"+t.TempDir()+"/hub.db") })
	t.Run("postgres", func(t *testing.T) { run(t, pgtest.Start(t).URL) })
}

func TestOfferRead(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		s.Now = ticking()
		var ids []string
		for _, title := range []string{"fresh", "changed", "cancelled"} {
			task, err := s.Create(ctx, spec("infra", "bwsalmon/silo", title))
			check(t, err)
			ids = append(ids, task.ID)
		}
		read, err := s.Dispatchable(ctx)
		check(t, err)
		if len(read) != 3 {
			t.Fatalf("dispatchable: %d tasks, want 3", len(read))
		}
		// Changed since it was read: read again and offered all the same.
		_, err = s.Retry(ctx, ids[1])
		check(t, err)
		_, err = s.Cancel(ctx, ids[2], "no longer wanted")
		check(t, err)

		for _, q := range read[:2] {
			got, err := s.offerRead(ctx, q, "ws-1", "alice@example.com")
			check(t, err)
			if got.State != Offered || got.Node != "ws-1" || got.NodeOwner != "alice@example.com" || got.Attempts != 1 {
				t.Errorf("offered %s: %+v", q.Title, got)
			}
			stored, err := s.Get(ctx, q.ID)
			check(t, err)
			if stored.State != Offered || stored.version != got.version {
				t.Errorf("stored %s: %+v, want it as offered (version %d)", q.Title, stored, got.version)
			}
		}
		if _, err := s.offerRead(ctx, read[2], "ws-1", "alice@example.com"); !errors.Is(err, ErrState) {
			t.Errorf("offering a task cancelled since it was read: %v, want ErrState", err)
		}
	})
}
