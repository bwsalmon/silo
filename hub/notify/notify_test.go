package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

// listening returns a bus over a throwaway server, listening, and the
// notices it receives.
func listening(t *testing.T) (*Bus, func() []Message) {
	t.Helper()
	srv := pgtest.Start(t)
	b, err := Open(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []Message
	b.Handle(func(m Message) {
		if m.Reconnected {
			return
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		b.Close()
	})
	for deadline := time.Now().Add(10 * time.Second); !b.Listening(); {
		if time.Now().After(deadline) {
			t.Fatal("the bus never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return b, func() []Message {
		mu.Lock()
		defer mu.Unlock()
		return append([]Message(nil), got...)
	}
}

// A burst of posts arrives coalesced: one wake-up, one notice per node,
// and one per task, with the highest seq posted for it.
func TestPostCoalesces(t *testing.T) {
	b, got := listening(t)
	// Hold the sender on the database, so the burst queues behind it.
	conn, err := b.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b.db.SetMaxOpenConns(1)
	b.Post(Message{Wake: true})
	time.Sleep(50 * time.Millisecond)
	for i := range 100 {
		b.Post(Message{Wake: true})
		b.Post(Message{Node: "n1"})
		b.Post(Message{Event: "t1", Seq: int64(i + 1)})
		b.Post(Message{Event: "t2", Seq: int64(100 - i)})
	}
	conn.Close()

	want := map[Message]bool{{Wake: true}: true, {Node: "n1"}: true, {Event: "t1", Seq: 100}: true, {Event: "t2", Seq: 100}: true}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ms := got()
		seen := map[Message]int{}
		for _, m := range ms {
			seen[m]++
		}
		ok := true
		for m := range want {
			ok = ok && seen[m] > 0
		}
		if ok {
			// The first wake-up, sent alone, and the burst's.
			if len(ms) > 5 {
				t.Errorf("got %d notices for the burst, want at most 5: %v", len(ms), ms)
			}
			for m := range seen {
				if !want[m] {
					t.Errorf("got %v, which was coalesced into a later notice", m)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %v, want each of %v", ms, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close sends what was posted before it.
func TestCloseSendsPosted(t *testing.T) {
	srv := pgtest.Start(t)
	rx, err := Open(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Message, 10)
	rx.Handle(func(m Message) {
		if !m.Reconnected {
			got <- m
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rx.Run(ctx)
	defer rx.Close()
	for !rx.Listening() {
		time.Sleep(10 * time.Millisecond)
	}
	tx, err := Open(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tx.Post(Message{Event: "t", Seq: 7})
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != (Message{Event: "t", Seq: 7}) {
			t.Errorf("got %v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the notice posted before Close never arrived")
	}
	tx.Post(Message{Wake: true}) // dropped, and does not block
}
