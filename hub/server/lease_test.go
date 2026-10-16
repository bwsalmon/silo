package server

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

// TestOneDispatcher runs two hubs on one PostgreSQL database, as two
// overlapping pods would: only one of them runs the dispatcher, and
// when it shuts down the other takes the lease and runs it
// (docs/hub.md §2.2.1).
func TestOneDispatcher(t *testing.T) {
	pg := pgtest.Start(t)
	type pod struct {
		h    *Hub
		stop context.CancelFunc
		done chan error
	}
	run := func(holder string) *pod {
		cfg := Config{
			Database:      pg.URL,
			DevUserHeader: userHeader,
			Holder:        holder,
			LeaseRetry:    100 * time.Millisecond,
			ShutdownGrace: 10 * time.Second,
			Logf:          logf(t),
		}
		h, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		p := &pod{h: h, stop: cancel, done: make(chan error, 1)}
		go func() { p.done <- h.Serve(ctx, l) }()
		t.Cleanup(func() {
			cancel()
			<-p.done
			h.Close()
		})
		return p
	}
	leading := func(p *pod) bool { return strings.Contains(scrape(t, p.h), "\nsilo_hub_dispatcher_leader 1\n") }

	a := run("pod-a")
	eventually(t, "pod-a to dispatch", func() bool { return leading(a) })
	b := run("pod-b")
	time.Sleep(500 * time.Millisecond)
	if leading(b) {
		t.Fatal("pod-b dispatches while pod-a does")
	}
	epoch, holder, err := b.h.Leases.Holder(context.Background(), "dispatcher")
	if err != nil || epoch != 1 || holder != "pod-a" {
		t.Fatalf("the lease: epoch %d held by %q (%v); want 1, pod-a", epoch, holder, err)
	}

	a.stop()
	if err := <-a.done; err != nil {
		t.Fatalf("pod-a: %v", err)
	}
	a.done <- nil
	eventually(t, "pod-b to dispatch", func() bool { return leading(b) })
	if leading(a) {
		t.Error("pod-a still says it dispatches after shutting down")
	}
	if m := scrape(t, b.h); !strings.Contains(m, "\nsilo_hub_dispatcher_terms_total 1\n") {
		t.Errorf("pod-b did not take exactly one term:\n%s", m)
	}
	epoch, holder, err = b.h.Leases.Holder(context.Background(), "dispatcher")
	if err != nil || epoch != 2 || holder != "pod-b" {
		t.Fatalf("the lease: epoch %d held by %q (%v); want 2, pod-b", epoch, holder, err)
	}
}
