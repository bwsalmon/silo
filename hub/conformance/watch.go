package conformance

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/live"
)

// watcher follows one task's events as a subscriber does, from the
// start, picking up from its last seq when the subscription drops (a
// hub restart), until the task's events end or it is stopped.
type watcher struct {
	mu     sync.Mutex
	got    []live.Event
	ended  bool
	stop   context.CancelFunc
	change chan struct{}
}

func (h *hub) watch(id string) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{stop: cancel, change: make(chan struct{})}
	go func() {
		for ctx.Err() == nil {
			err := h.events(ctx, id, w.last(), w.add)
			// A hub shutting down ends its subscriptions as a task's end
			// does: only a task that is over has had all its events.
			if err == nil {
				if t, terr := h.task(ctx, id); terr != nil || !t.State.Terminal() {
					err = errors.New("ended early")
				}
			}
			if err == nil {
				w.mu.Lock()
				w.ended = true
				w.notify()
				w.mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return w
}

// notify wakes every waiter. Lock w.mu.
func (w *watcher) notify() {
	close(w.change)
	w.change = make(chan struct{})
}

func (w *watcher) add(e live.Event) {
	w.mu.Lock()
	w.got = append(w.got, e)
	w.notify()
	w.mu.Unlock()
}

func (w *watcher) last() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.got) == 0 {
		return 0
	}
	return w.got[len(w.got)-1].Seq
}

// Events is what the subscription has delivered so far, in order.
func (w *watcher) Events() []live.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]live.Event(nil), w.got...)
}

// Await waits up to d for an event that ok accepts, and returns it.
func (w *watcher) Await(d time.Duration, ok func(live.Event) bool) (live.Event, bool) {
	deadline := time.After(d)
	seen := 0
	for {
		w.mu.Lock()
		for ; seen < len(w.got); seen++ {
			if ok(w.got[seen]) {
				e := w.got[seen]
				w.mu.Unlock()
				return e, true
			}
		}
		ch := w.change
		w.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return live.Event{}, false
		}
	}
}

func (w *watcher) Stop() { w.stop() }
