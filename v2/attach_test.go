package silo_test

import (
	"context"
	"testing"
	"time"

	silo "github.com/bwsalmon/silo/v2"
	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/internal/fakecli"
)

// TestAttachAndList finds a session by its label with List, follows it
// with Attach from the start and from a cursor, and injects and cancels
// through the attached handle, which goes by the control socket.
func TestAttachAndList(t *testing.T) {
	h := newHost(t)
	h.fake(t, fakecli.Config{Echo: true, Answers: 99})
	s, err := silo.Start(context.Background(), file(time.Minute), silo.Options{Environ: h.environ, Credentials: creds, Labels: map[string]string{"grain.run": "r-7"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range s.Events() {
		}
	}()
	opts := silo.AttachOptions{Environ: h.environ}

	var id string
	for deadline := time.Now().Add(30 * time.Second); id == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("List never found the session")
		}
		got, err := silo.List(map[string]string{"grain.run": "r-7"}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 1 {
			id = got[0].ID
		}
	}
	if other, _ := silo.List(map[string]string{"grain.run": "r-8"}, opts); len(other) != 0 {
		t.Errorf("List with another label: %+v", other)
	}

	a, err := silo.Attach(context.Background(), id, 0, opts)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	for e := range a.Events() {
		if e.Transcript != nil && e.Transcript.Text == "got: do the thing" {
			cursor = e.Seq
			if d, err := a.Inject("more"); err != nil || d != silo.Delivered {
				t.Fatalf("Inject: %q, %v", d, err)
			}
		}
		if e.Transcript != nil && e.Transcript.Text == "got: more" {
			break
		}
	}
	a.Detach()
	if _, err := a.Wait(); err != context.Canceled {
		t.Errorf("Wait after Detach: %v", err)
	}

	b, err := silo.Attach(context.Background(), id, cursor, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := cursor + 1
	for e := range b.Events() {
		if e.Seq != want {
			t.Fatalf("event seq %d, want %d", e.Seq, want)
		}
		want++
		if e.Transcript != nil && e.Transcript.Text == "got: more" {
			if err := b.Cancel("enough"); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}
	}
	res, err := b.Wait()
	if err != nil || res.Exit.Reason != events.ReasonCancelled {
		t.Fatalf("result %+v, %v", res.Exit, err)
	}
	if lres, _ := s.Wait(); lres.Exit.Reason != events.ReasonCancelled {
		t.Errorf("the session's own result %+v", lres.Exit)
	}
	if _, err := silo.Attach(context.Background(), "s-aaaaaaaaaaaa", 0, opts); err == nil {
		t.Error("Attach to no session")
	}
}
