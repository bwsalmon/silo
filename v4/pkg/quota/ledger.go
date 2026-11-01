package quota

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrQuotaExceeded        = errors.New("sub-agent lifetime quota exceeded")
	ErrConcurrencyLimit     = errors.New("sub-agent concurrency limit reached")
	ErrDepthLimitExceeded   = errors.New("sub-agent depth limit exceeded")
	ErrDelegationNotAllowed = errors.New("creator does not have permission to delegate sub-agent creation")
	ErrAgentNotFound        = errors.New("agent not found in quota ledger")
	ErrAgentExists          = errors.New("agent with this ID already exists")
)

// AgentQuota tracks the quota state and lineage for a specific agent.
type AgentQuota struct {
	AgentID        string   `json:"agent_id"`
	ParentID       string   `json:"parent_id"`
	MaxSubagents   int      `json:"max_subagents"`   // -1 = unlimited
	MaxConcurrent  int      `json:"max_concurrent"`  // -1 = unlimited
	MaxDepth       int      `json:"max_depth"`
	TotalSpawned   int      `json:"total_spawned"`
	ActiveChildren int      `json:"active_children"`
	CanSpawn       bool     `json:"can_spawn"`
	Children       []string `json:"children"`
}

// Ledger is the thread-safe global manager of all agent quotas and lineages.
type Ledger struct {
	mu     sync.RWMutex
	agents map[string]*AgentQuota
	rootID string
}

// NewLedger initializes a quota ledger with the root agent quota.
func NewLedger(rootID string, maxSubagents, maxConcurrent, maxDepth int, allowDelegation bool) *Ledger {
	l := &Ledger{
		agents: make(map[string]*AgentQuota),
		rootID: rootID,
	}

	canSpawn := allowDelegation && maxDepth > 0 && maxSubagents != 0
	l.agents[rootID] = &AgentQuota{
		AgentID:       rootID,
		ParentID:      "",
		MaxSubagents:  maxSubagents,
		MaxConcurrent: maxConcurrent,
		MaxDepth:      maxDepth,
		CanSpawn:      canSpawn,
		Children:      make([]string, 0),
	}

	return l
}

// AllocateChild checks budgets and allocates quota for a newly spawned child.
func (l *Ledger) AllocateChild(parentID, childID string, reqMaxSub, reqMaxConc, reqMaxDepth int, reqCanSpawn bool) (*AgentQuota, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	parent, exists := l.agents[parentID]
	if !exists {
		return nil, ErrAgentNotFound
	}

	if _, exists := l.agents[childID]; exists {
		return nil, ErrAgentExists
	}

	if !parent.CanSpawn {
		return nil, ErrDelegationNotAllowed
	}

	// 1. Check parent lifetime budget
	if parent.MaxSubagents != -1 && parent.TotalSpawned >= parent.MaxSubagents {
		return nil, fmt.Errorf("%w: parent %s reached max_subagents %d", ErrQuotaExceeded, parentID, parent.MaxSubagents)
	}

	// 2. Check parent concurrency limit
	if parent.MaxConcurrent != -1 && parent.ActiveChildren >= parent.MaxConcurrent {
		return nil, fmt.Errorf("%w: parent %s reached max_concurrent %d", ErrConcurrencyLimit, parentID, parent.MaxConcurrent)
	}

	// 3. Check depth limit
	if parent.MaxDepth <= 0 {
		return nil, fmt.Errorf("%w: parent %s depth is 0", ErrDepthLimitExceeded, parentID)
	}

	// Determine child depth
	childDepth := parent.MaxDepth - 1
	if reqMaxDepth > 0 && reqMaxDepth < childDepth {
		childDepth = reqMaxDepth
	}

	// Determine if child can spawn
	childCanSpawn := false
	childMaxSub := 0
	childMaxConc := 0

	if reqCanSpawn && childDepth > 0 {
		childCanSpawn = true

		// Validate child budget against parent remaining
		if parent.MaxSubagents != -1 {
			remainingParentBudget := parent.MaxSubagents - parent.TotalSpawned
			if reqMaxSub == -1 || reqMaxSub > remainingParentBudget {
				return nil, fmt.Errorf("%w: requested %d exceeds remaining parent budget %d", ErrQuotaExceeded, reqMaxSub, remainingParentBudget)
			}
			childMaxSub = reqMaxSub
		} else {
			childMaxSub = reqMaxSub
		}

		if reqMaxConc == -1 || (parent.MaxConcurrent != -1 && reqMaxConc > parent.MaxConcurrent) {
			childMaxConc = parent.MaxConcurrent
		} else {
			childMaxConc = reqMaxConc
		}
	}

	// Record child
	child := &AgentQuota{
		AgentID:       childID,
		ParentID:      parentID,
		MaxSubagents:  childMaxSub,
		MaxConcurrent: childMaxConc,
		MaxDepth:      childDepth,
		CanSpawn:      childCanSpawn,
		Children:      make([]string, 0),
	}

	parent.TotalSpawned++
	parent.ActiveChildren++
	parent.Children = append(parent.Children, childID)
	l.agents[childID] = child

	return child, nil
}

// ChildTerminated updates concurrency counters when a child agent exits.
func (l *Ledger) ChildTerminated(childID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	child, exists := l.agents[childID]
	if !exists {
		return ErrAgentNotFound
	}

	if child.ParentID != "" {
		if parent, exists := l.agents[child.ParentID]; exists {
			if parent.ActiveChildren > 0 {
				parent.ActiveChildren--
			}
		}
	}

	return nil
}

// ReleaseSubtree recursively collects all descendant agent IDs when an agent is terminated.
func (l *Ledger) ReleaseSubtree(agentID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var toReap []string
	var collect func(string)

	collect = func(id string) {
		if ag, exists := l.agents[id]; exists {
			for _, childID := range ag.Children {
				toReap = append(toReap, childID)
				collect(childID)
			}
		}
	}

	collect(agentID)

	// Clean up entries
	for _, id := range toReap {
		delete(l.agents, id)
	}

	return toReap
}

// GetQuota returns the quota snapshot for an agent.
func (l *Ledger) GetQuota(agentID string) (*AgentQuota, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	ag, exists := l.agents[agentID]
	if !exists {
		return nil, ErrAgentNotFound
	}

	// Return a copy
	cp := *ag
	cp.Children = append([]string(nil), ag.Children...)
	return &cp, nil
}

// ListChildren returns snapshots of all direct children of an agent.
func (l *Ledger) ListChildren(parentID string) ([]*AgentQuota, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	parent, exists := l.agents[parentID]
	if !exists {
		return nil, ErrAgentNotFound
	}

	result := make([]*AgentQuota, 0, len(parent.Children))
	for _, childID := range parent.Children {
		if child, exists := l.agents[childID]; exists {
			cp := *child
			result = append(result, &cp)
		}
	}

	return result, nil
}
