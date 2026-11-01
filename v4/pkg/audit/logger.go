package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger is the interface for recording structured security and operational events.
type Logger interface {
	Log(event string, fields map[string]interface{}) error
}

// FileLogger writes newline-delimited JSON audit logs to a file or writer.
type FileLogger struct {
	mu     sync.Mutex
	writer io.Writer
	closer io.Closer
}

// NewFileLogger creates an audit logger appending to the specified file path.
func NewFileLogger(filePath string) (*FileLogger, error) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return nil, fmt.Errorf("create audit log dir: %w", err)
	}

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, fmt.Errorf("open audit log file: %w", err)
	}

	return &FileLogger{
		writer: file,
		closer: file,
	}, nil
}

// NewWriterLogger creates an audit logger that writes to an arbitrary io.Writer.
func NewWriterLogger(w io.Writer) *FileLogger {
	return &FileLogger{
		writer: w,
	}
}

// Log records a single structured audit event.
func (l *FileLogger) Log(event string, fields map[string]interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := make(map[string]interface{}, len(fields)+2)
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["event"] = event

	for k, v := range fields {
		entry[k] = v
	}

	bytes, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}

	bytes = append(bytes, '\n')
	if _, err := l.writer.Write(bytes); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}

	return nil
}

// Close closes the underlying log file if applicable.
func (l *FileLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

// MemoryLogger records events in memory for testing and assertions.
type MemoryLogger struct {
	mu     sync.RWMutex
	Events []map[string]interface{}
}

// NewMemoryLogger creates an in-memory audit logger.
func NewMemoryLogger() *MemoryLogger {
	return &MemoryLogger{
		Events: make([]map[string]interface{}, 0),
	}
}

// Log appends an event to the in-memory log list.
func (m *MemoryLogger) Log(event string, fields map[string]interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry := make(map[string]interface{}, len(fields)+2)
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["event"] = event
	for k, v := range fields {
		entry[k] = v
	}

	m.Events = append(m.Events, entry)
	return nil
}

// GetEvents returns a snapshot of all logged events.
func (m *MemoryLogger) GetEvents() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	copied := make([]map[string]interface{}, len(m.Events))
	copy(copied, m.Events)
	return copied
}

// CountEvent returns how many times a given event type occurred.
func (m *MemoryLogger) CountEvent(event string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, e := range m.Events {
		if e["event"] == event {
			count++
		}
	}
	return count
}
