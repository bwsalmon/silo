// Adapted from github.com/bwsalmon/grain pkg/gitproxy/audit.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// One audit entry per request (design §9.1): every decision the proxy
// makes -- allowed, refused, or failed -- gets exactly one, written when
// the request is decided and before any response body streams back, so
// the caller can read from one place what its agent's git did. grain's
// Sandbox, Task, Template and Declaration fields are a free-form caller
// map here: who asked (Caller.Audit), and for each ref update, what
// allowed it (RefGrant.Caller).

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// AuditEntry is one request and what the proxy did with it.
type AuditEntry struct {
	Time time.Time `json:"time"`
	// Caller is the authenticated caller's Caller.Audit; empty for a
	// request that did not authenticate.
	Caller map[string]string `json:"caller,omitempty"`
	// Repo is host/owner/name; empty for a path that names none.
	Repo string `json:"repo,omitempty"`
	// Action is "info/refs", "git-upload-pack" or "git-receive-pack".
	Action string `json:"action,omitempty"`
	// Service is info/refs's service: which of the other two it opens.
	Service string `json:"service,omitempty"`
	// Refs is every ref update of an allowed push.
	Refs []RefGrant `json:"refs,omitempty"`
	// Credential is the name of the credential the request went out
	// with; empty when it was not forwarded, or went anonymously.
	Credential string `json:"credential,omitempty"`
	// Outcome is "forwarded: <status>", "denied: <why>" or
	// "error: <what>".
	Outcome string `json:"outcome"`
	// Status is the HTTP status the caller was answered with.
	Status int `json:"status"`
	// Reason is what a refused caller was told.
	Reason string `json:"reason,omitempty"`
	// Checks is every request the proxy made of its own to decide this
	// one: the upstream fetches behind a fast-forward proof.
	Checks []string `json:"checks,omitempty"`
}

// AuditLog records one AuditEntry per request. Record is called
// concurrently.
type AuditLog interface {
	Record(entry AuditEntry)
}

// NullAuditLog discards every entry.
type NullAuditLog struct{}

func (NullAuditLog) Record(AuditEntry) {}

// JSONAuditLog writes one JSON line per entry to W, each in one Write so
// lines from concurrent requests never interleave. A failed write is
// dropped: an audit failure must not take the proxy itself down.
type JSONAuditLog struct {
	mu sync.Mutex
	W  io.Writer
}

func (l *JSONAuditLog) Record(entry AuditEntry) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.W.Write(append(data, '\n'))
}

// RecordingAuditLog collects entries in memory. For tests.
type RecordingAuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (l *RecordingAuditLog) Record(entry AuditEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

// Entries is every entry recorded so far.
func (l *RecordingAuditLog) Entries() []AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AuditEntry(nil), l.entries...)
}
