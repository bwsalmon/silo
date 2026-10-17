package live

import (
	"context"
	"testing"

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
