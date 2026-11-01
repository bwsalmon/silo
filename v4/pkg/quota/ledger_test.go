package quota

import (
	"errors"
	"testing"
)

func TestQuotaLedger_RootSpawn(t *testing.T) {
	// Root has 5 total, 2 concurrent, depth 2
	ledger := NewLedger("root", 5, 2, 2, true)

	// Spawn child 1
	c1, err := ledger.AllocateChild("root", "child-1", 2, 1, 1, true)
	if err != nil {
		t.Fatalf("expected child 1 spawn to succeed, got %v", err)
	}
	if c1.MaxDepth != 1 {
		t.Errorf("expected child 1 depth to be 1, got %d", c1.MaxDepth)
	}

	// Spawn child 2
	_, err = ledger.AllocateChild("root", "child-2", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("expected child 2 spawn to succeed, got %v", err)
	}

	// Spawn child 3 should fail due to concurrency limit (2 max concurrent)
	_, err = ledger.AllocateChild("root", "child-3", 0, 0, 0, false)
	if !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("expected ErrConcurrencyLimit, got %v", err)
	}

	// Child 1 terminates
	if err := ledger.ChildTerminated("child-1"); err != nil {
		t.Fatalf("expected ChildTerminated to succeed, got %v", err)
	}

	// Now child 3 can be spawned
	_, err = ledger.AllocateChild("root", "child-3", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("expected child 3 spawn to succeed after termination, got %v", err)
	}
}

func TestQuotaLedger_DelegationAndDepthCeiling(t *testing.T) {
	ledger := NewLedger("root", 10, 5, 2, true)

	// Manager spawned with can_spawn = true, budget 3, depth 1
	manager, err := ledger.AllocateChild("root", "mgr-1", 3, 2, 1, true)
	if err != nil {
		t.Fatalf("failed to spawn manager: %v", err)
	}
	if !manager.CanSpawn {
		t.Fatal("expected manager.CanSpawn to be true")
	}

	// Manager spawns worker-1 (leaf)
	worker1, err := ledger.AllocateChild("mgr-1", "w-1", 0, 0, 0, false)
	if err != nil {
		t.Fatalf("failed to spawn worker: %v", err)
	}
	if worker1.MaxDepth != 0 {
		t.Errorf("expected worker depth 0, got %d", worker1.MaxDepth)
	}
	if worker1.CanSpawn {
		t.Fatal("expected worker1.CanSpawn to be false at depth 0")
	}

	// Worker attempting to spawn should fail
	_, err = ledger.AllocateChild("w-1", "w-illegal", 0, 0, 0, false)
	if !errors.Is(err, ErrDelegationNotAllowed) {
		t.Fatalf("expected ErrDelegationNotAllowed, got %v", err)
	}
}

func TestQuotaLedger_SubtreeRelease(t *testing.T) {
	ledger := NewLedger("root", -1, -1, 3, true)

	_, _ = ledger.AllocateChild("root", "mgr-1", 10, 5, 2, true)
	_, _ = ledger.AllocateChild("mgr-1", "w-1", 5, 2, 1, true)
	_, _ = ledger.AllocateChild("w-1", "leaf-1", 0, 0, 0, false)
	_, _ = ledger.AllocateChild("root", "other-leaf", 0, 0, 0, false)

	// Reaping mgr-1 should return w-1 and leaf-1, but not other-leaf
	reaped := ledger.ReleaseSubtree("mgr-1")
	expected := map[string]bool{"w-1": true, "leaf-1": true}

	if len(reaped) != 2 {
		t.Fatalf("expected 2 reaped agents, got %d: %v", len(reaped), reaped)
	}
	for _, id := range reaped {
		if !expected[id] {
			t.Errorf("unexpected reaped id %s", id)
		}
	}

	// Checking leaf-1 quota should now return not found
	_, err := ledger.GetQuota("leaf-1")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound for reaped agent, got %v", err)
	}

	// other-leaf should still exist
	if _, err := ledger.GetQuota("other-leaf"); err != nil {
		t.Errorf("expected other-leaf to exist, got %v", err)
	}
}
