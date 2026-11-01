package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/quota"
)

func TestSupervisor_OneShotAgent(t *testing.T) {
	ledger := quota.NewLedger("root", 5, 2, 2, true)
	mockAuth := &auth.MockTokenProvider{}
	sup := NewSupervisor(ledger, mockAuth, t.TempDir(), "")
	defer sup.Shutdown()

	ctx := context.Background()
	spec := AgentSpec{
		ID:        "worker-1",
		ParentID:  "root",
		Name:      "test-worker",
		Lifecycle: LifecycleOneShot,
		Command:   []string{"true"},
	}

	_, err := sup.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("failed to spawn: %v", err)
	}

	// Wait for process completion
	time.Sleep(50 * time.Millisecond)

	proc, err := sup.GetAgent("worker-1")
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	if proc.Status != StatusCompleted {
		t.Errorf("expected StatusCompleted, got %s", proc.Status)
	}
	if proc.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", proc.ExitCode)
	}
}

func TestSupervisor_PersistentAgentRestart(t *testing.T) {
	ledger := quota.NewLedger("root", 5, 2, 2, true)
	mockAuth := &auth.MockTokenProvider{}
	sup := NewSupervisor(ledger, mockAuth, t.TempDir(), "")
	defer sup.Shutdown()

	ctx := context.Background()
	spec := AgentSpec{
		ID:        "daemon-1",
		ParentID:  "root",
		Name:      "test-daemon",
		Lifecycle: LifecyclePersistent,
		Command:   []string{"true"}, // exits immediately, triggering restart
	}

	_, err := sup.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("failed to spawn daemon: %v", err)
	}

	// Allow several quick crash-restarts
	time.Sleep(350 * time.Millisecond)

	proc, err := sup.GetAgent("daemon-1")
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	if proc.Restarts < 1 {
		t.Errorf("expected restarts >= 1, got %d", proc.Restarts)
	}
}

func TestSupervisor_CascadingTeardown(t *testing.T) {
	ledger := quota.NewLedger("root", 10, 5, 3, true)
	mockAuth := &auth.MockTokenProvider{}
	sup := NewSupervisor(ledger, mockAuth, t.TempDir(), "")
	defer sup.Shutdown()

	ctx := context.Background()

	// Allocate in quota ledger
	_, _ = ledger.AllocateChild("root", "mgr-1", 5, 2, 2, true)
	_, _ = ledger.AllocateChild("mgr-1", "sub-worker-1", 0, 0, 0, false)

	// Spawn manager
	_, _ = sup.Spawn(ctx, AgentSpec{
		ID:        "mgr-1",
		ParentID:  "root",
		Lifecycle: LifecyclePersistent,
		Command:   []string{"sleep", "10"},
	})

	// Spawn child worker
	_, _ = sup.Spawn(ctx, AgentSpec{
		ID:        "sub-worker-1",
		ParentID:  "mgr-1",
		Lifecycle: LifecyclePersistent,
		Command:   []string{"sleep", "10"},
	})

	time.Sleep(50 * time.Millisecond)

	// Stop manager
	reapedCount, err := sup.Stop("mgr-1")
	if err != nil {
		t.Fatalf("failed to stop manager: %v", err)
	}
	if reapedCount != 1 {
		t.Errorf("expected 1 reaped child, got %d", reapedCount)
	}

	// Verify both are stopped
	mgr, _ := sup.GetAgent("mgr-1")
	if mgr.Status != StatusStopped {
		t.Errorf("expected mgr-1 StatusStopped, got %s", mgr.Status)
	}

	sub, _ := sup.GetAgent("sub-worker-1")
	if sub.Status != StatusStopped {
		t.Errorf("expected sub-worker-1 StatusStopped, got %s", sub.Status)
	}
}
