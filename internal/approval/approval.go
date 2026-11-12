// Package approval asks the person at the terminal to approve what the
// agent proposes: a commit, a one-off tool's code, or a call to a tool
// whose manifest requires approval.
//
// The answer must come from the person, never from the agent. The
// terminal implementation (Console) runs the agent CLI on a
// pseudo-terminal silo owns, and reads the answer from the person's real
// terminal while it stops forwarding their keystrokes to the CLI: the
// CLI never sees the prompt's input, and nothing the CLI writes can
// reach it.
package approval

import (
	"context"
	"sync"
)

// Request is one thing to approve.
type Request struct {
	// Kind is "commit", "scratch" or "tool-call".
	Kind string
	// Title is a one-line summary.
	Title string
	// Body is what the person must see to decide: the full diff, the
	// full code, or the call's arguments.
	Body string
}

// Decision is the person's answer. A rejection carries their reason,
// which goes back to the agent.
type Decision struct {
	Approved bool
	Reason   string
}

// Approver asks for decisions.
type Approver interface {
	Approve(ctx context.Context, req Request) Decision
}

// Deny rejects everything; it is what a session gets with no terminal.
type Deny struct{ Reason string }

// Approve rejects.
func (d Deny) Approve(context.Context, Request) Decision {
	return Decision{Reason: d.Reason}
}

// Fake answers from a script, for tests, and records what it was asked.
type Fake struct {
	mu        sync.Mutex
	decisions []Decision
	// Default is returned once the script runs out.
	Default  Decision
	Requests []Request
}

// NewFake returns a Fake that answers with decisions, in order.
func NewFake(decisions ...Decision) *Fake {
	return &Fake{decisions: decisions, Default: Decision{Reason: "fake approver: no scripted answer"}}
}

// Approve returns the next scripted decision.
func (f *Fake) Approve(_ context.Context, req Request) Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req)
	if len(f.decisions) == 0 {
		return f.Default
	}
	d := f.decisions[0]
	f.decisions = f.decisions[1:]
	return d
}

// Reset replaces the script and forgets past requests.
func (f *Fake) Reset(decisions ...Decision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = decisions
	f.Requests = nil
}

// Asked returns how many requests the fake has seen.
func (f *Fake) Asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}
