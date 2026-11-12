// Package audit writes a session's audit log: one JSON object per line,
// for every tool call, approval, rejection and tool-set reload.
//
// Adapted from silo v4's pkg/audit.
package audit

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger records events.
type Logger interface {
	Log(event string, fields map[string]any)
}

// File appends events to a JSONL file.
type File struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// Open opens (creating if needed) the log at path.
func Open(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &File{w: f}, nil
}

// Log appends one event. A write failure is dropped: the session goes
// on, and the gap shows in the log.
func (l *File) Log(event string, fields map[string]any) {
	b, err := json.Marshal(entry(event, fields))
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(append(b, '\n'))
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Close()
}

// Memory keeps events in memory, for tests.
type Memory struct {
	mu     sync.Mutex
	events []map[string]any
}

// Log records one event.
func (m *Memory) Log(event string, fields map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, entry(event, fields))
}

// Events returns a copy of what has been logged.
func (m *Memory) Events() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.events...)
}

// Count returns how many events of the given kind were logged.
func (m *Memory) Count(event string) int {
	n := 0
	for _, e := range m.Events() {
		if e["event"] == event {
			n++
		}
	}
	return n
}

// Discard drops everything.
type Discard struct{}

// Log does nothing.
func (Discard) Log(string, map[string]any) {}

func entry(event string, fields map[string]any) map[string]any {
	e := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		e[k] = v
	}
	e["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	e["event"] = event
	return e
}
