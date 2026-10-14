// Package audit writes a session's audit log, audit.jsonl (design §4.2):
// one JSON line when a gateway call starts, written before the call
// runs so that a crash mid-call still leaves a record, and one when it
// ends. Every string written passes through the redactor first, which
// replaces each registered credential value with [credential:<name>]
// (design §8.4).
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileName is the audit log's name in the session directory.
const FileName = "audit.jsonl"

// HeadBytes is how much of a result an end line keeps.
const HeadBytes = 256

// MinSecretLength is the shortest value Register redacts. Shorter values
// would rewrite ordinary text all over the log while hiding nothing a
// guess would not find.
const MinSecretLength = 4

// Outcomes of a call.
const (
	OutcomeOK      = "ok"      // the tool ran and succeeded
	OutcomeError   = "error"   // the tool ran and failed
	OutcomeRefused = "refused" // the gateway refused the call
	OutcomeTimeout = "timeout" // the call ran out of time
)

// Caller is who made the calls: the CLI type and the agent host.
type Caller struct {
	CLI  string `json:"cli,omitempty"`
	Host string `json:"host,omitempty"`
}

// Record is one line of audit.jsonl.
type Record struct {
	Time    time.Time `json:"time"`
	Event   string    `json:"event"` // "start" or "end"
	Session string    `json:"session"`
	Caller  Caller    `json:"caller"`
	Call    int64     `json:"call"` // pairs a call's start and end
	Tool    string    `json:"tool"`
	Args    any       `json:"args,omitempty"` // start only

	Outcome    string `json:"outcome,omitempty"` // end only
	Reason     string `json:"reason,omitempty"`  // why it was refused or failed
	Size       *int   `json:"size,omitempty"`    // the result's length in bytes
	Head       string `json:"head,omitempty"`    // the result's first HeadBytes
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// Log is an open audit log. Its methods are safe for concurrent use.
type Log struct {
	session string
	caller  Caller

	mu      sync.Mutex
	f       *os.File
	secrets []secret // longest first
	nextID  atomic.Int64
}

type secret struct{ name, value string }

// Open opens (creating, mode 0600) the audit log at path for appending.
func Open(path, session string, caller Caller) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return &Log{session: session, caller: caller, f: f}, nil
}

// Close closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Register records a credential value, so that every later line has
// each occurrence of it replaced by [credential:<name>]. Values shorter
// than MinSecretLength are not redacted.
func (l *Log) Register(name, value string) {
	if len(value) < MinSecretLength {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets = append(l.secrets, secret{name, value})
	// Longest first, so that a secret containing another is replaced
	// whole rather than around the shorter one.
	sort.SliceStable(l.secrets, func(i, j int) bool { return len(l.secrets[i].value) > len(l.secrets[j].value) })
}

// Redact replaces every registered credential value in s.
func (l *Log) Redact(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.redactLocked(s)
}

func (l *Log) redactLocked(s string) string {
	for _, sec := range l.secrets {
		if strings.Contains(s, sec.value) {
			s = strings.ReplaceAll(s, sec.value, "[credential:"+sec.name+"]")
		}
	}
	return s
}

// redactValue redacts every string in a decoded JSON value: keys and
// values of objects, and elements of arrays.
func (l *Log) redactValue(v any) any {
	switch v := v.(type) {
	case string:
		return l.redactLocked(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[l.redactLocked(k)] = l.redactValue(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = l.redactValue(e)
		}
		return out
	default:
		return v
	}
}

// write appends one line in a single write(2), so that lines from
// concurrent calls never interleave and a line is either there or not.
func (l *Log) write(r Record) error {
	r.Session, r.Caller = l.session, l.caller
	r.Args = l.redactValue(r.Args)
	r.Reason = l.redactLocked(r.Reason)
	r.Head = l.redactLocked(r.Head)
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// Call is one call in progress.
type Call struct {
	log   *Log
	id    int64
	tool  string
	start time.Time
}

// Start writes a call's start line and returns the call, whose End
// writes its end line. The start line is on disk (in the kernel's page
// cache, which survives the process) before Start returns.
func (l *Log) Start(tool string, args map[string]any) (*Call, error) {
	c := &Call{log: l, id: l.nextID.Add(1), tool: tool, start: time.Now()}
	var a any
	if args != nil {
		// Round-trip through JSON so redactValue sees plain JSON values
		// whatever the caller's map holds.
		if b, err := json.Marshal(args); err == nil {
			_ = json.Unmarshal(b, &a)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return c, l.write(Record{Time: c.start.UTC(), Event: "start", Call: c.id, Tool: tool, Args: a})
}

// End writes the call's end line: its outcome, the reason when it did
// not succeed, and the result's size and first bytes.
func (c *Call) End(outcome, reason, result string) error {
	d := time.Since(c.start).Milliseconds()
	size := len(result)
	l := c.log
	l.mu.Lock()
	defer l.mu.Unlock()
	// Redacted before it is cut, so that a secret straddling the cut
	// does not leave its first half behind.
	head := l.redactLocked(result)
	if len(head) > HeadBytes {
		head = head[:HeadBytes]
	}
	return l.write(Record{
		Time: time.Now().UTC(), Event: "end", Call: c.id, Tool: c.tool,
		Outcome: outcome, Reason: reason, Size: &size, Head: head, DurationMS: &d,
	})
}
