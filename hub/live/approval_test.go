package live

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
)

// TestApprovalRequestLookup finds a task's approval_request by its ID,
// and only that task's.
func TestApprovalRequestLookup(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, e := range []Event{
		{Task: "t-1", Event: events.Event{Seq: 1, Type: events.TypeStatus, Status: &events.Status{State: "running"}}},
		{Task: "t-1", Event: events.Event{Seq: 2, Type: events.TypeApprovalRequest, ApprovalRequest: &events.ApprovalRequest{ID: "r-1", Tool: "deploy"}}},
		{Task: "t-2", Event: events.Event{Seq: 1, Type: events.TypeApprovalRequest, ApprovalRequest: &events.ApprovalRequest{ID: "r-2", Tool: "push"}}},
	} {
		if err := s.Append(ctx, e, Conversation); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if a, ok, err := s.ApprovalRequest(ctx, "t-1", "r-1"); err != nil || !ok || a.Tool != "deploy" {
		t.Errorf("t-1's r-1: %+v %v %v", a, ok, err)
	}
	for _, c := range [][2]string{{"t-1", "r-2"}, {"t-2", "r-1"}, {"t-3", "r-1"}} {
		if _, ok, err := s.ApprovalRequest(ctx, c[0], c[1]); err != nil || ok {
			t.Errorf("%s's %s: found %v, %v", c[0], c[1], ok, err)
		}
	}
}

func TestConcurrentTaskInboxStateStress(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const numTasks = 4
	const stepsPerTask = 12
	errCh := make(chan error, 64)
	recordErr := func(err error) {
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}

	var wg sync.WaitGroup
	for ti := 0; ti < numTasks; ti++ {
		taskID := fmt.Sprintf("task-inbox-%d", ti)
		wg.Add(2)

		// Writer goroutine appending awaiting_reply, approval_request, and message events in sequence.
		go func() {
			defer wg.Done()
			var seq int64
			for step := 0; step < stepsPerTask; step++ {
				now := time.Now().UTC()
				seq++
				if err := s.Append(ctx, Event{
					Task: taskID,
					Event: events.Event{
						Seq:           seq,
						Time:          now,
						Type:          events.TypeAwaitingReply,
						AwaitingReply: &events.AwaitingReply{State: events.AwaitingWaiting, Question: fmt.Sprintf("q-%d", step)},
					},
				}, Lifecycle); err != nil {
					recordErr(err)
					return
				}

				seq++
				reqID := fmt.Sprintf("req-%d", step)
				if err := s.Append(ctx, Event{
					Task: taskID,
					Event: events.Event{
						Seq:             seq,
						Time:            now,
						Type:            events.TypeApprovalRequest,
						ApprovalRequest: &events.ApprovalRequest{ID: reqID, Tool: "bash", Message: "run check"},
					},
				}, Conversation); err != nil {
					recordErr(err)
					return
				}

				seq++
				if step%2 == 0 {
					if err := s.Append(ctx, Event{
						Task: taskID,
						Event: events.Event{
							Seq:     seq,
							Time:    now,
							Type:    events.TypeMessage,
							Message: &events.Message{Text: "answered"},
						},
					}, Conversation); err != nil {
						recordErr(err)
						return
					}
				} else {
					if err := s.Append(ctx, Event{
						Task: taskID,
						Event: events.Event{
							Seq:           seq,
							Time:          now,
							Type:          events.TypeAwaitingReply,
							AwaitingReply: &events.AwaitingReply{State: events.AwaitingResumed},
						},
					}, Lifecycle); err != nil {
						recordErr(err)
						return
					}
				}
			}
		}()

		// Reader goroutine querying TaskInboxState and ApprovalRequest concurrently.
		go func() {
			defer wg.Done()
			for step := 0; step < stepsPerTask*2; step++ {
				awaiting, question, _, approvals, times, err := s.TaskInboxState(ctx, taskID)
				if err != nil {
					recordErr(err)
					return
				}
				if awaiting && question == "" {
					recordErr(fmt.Errorf("%s: awaiting=true with empty question", taskID))
					return
				}
				if len(approvals) != len(times) {
					recordErr(fmt.Errorf("%s: approvals=%d != times=%d", taskID, len(approvals), len(times)))
					return
				}
				if len(approvals) > 0 {
					last := approvals[len(approvals)-1]
					got, ok, err := s.ApprovalRequest(ctx, taskID, last.ID)
					if err != nil || !ok || got.ID != last.ID {
						recordErr(fmt.Errorf("%s: ApprovalRequest(%s) = %+v, %v, %v", taskID, last.ID, got, ok, err))
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
