package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLogger_Writer(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWriterLogger(&buf)

	err := logger.Log("SUBAGENT_SPAWN", map[string]interface{}{
		"agent_id":  "subagent-1",
		"parent_id": "root",
		"lifecycle": "one_shot",
	})
	if err != nil {
		t.Fatalf("Log failed: %v", err)
	}

	line := buf.String()
	if !strings.Contains(line, `"event":"SUBAGENT_SPAWN"`) {
		t.Errorf("expected SUBAGENT_SPAWN event, got: %s", line)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("failed to parse json log entry: %v", err)
	}

	if parsed["agent_id"] != "subagent-1" {
		t.Errorf("expected agent_id subagent-1, got: %v", parsed["agent_id"])
	}
	if parsed["timestamp"] == nil {
		t.Errorf("expected timestamp field")
	}
}

func TestFileLogger_FileWriteAndClose(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "logs", "audit.log")

	logger, err := NewFileLogger(logPath)
	if err != nil {
		t.Fatalf("NewFileLogger failed: %v", err)
	}

	_ = logger.Log("ROOT_AGENT_START", map[string]interface{}{"mode": "standalone"})
	_ = logger.Log("ROOT_AGENT_EXIT", map[string]interface{}{"exit_code": 0})
	_ = logger.Close()

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read written log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(lines))
	}
}

func TestMemoryLogger(t *testing.T) {
	mem := NewMemoryLogger()
	_ = mem.Log("SUBAGENT_SPAWN", map[string]interface{}{"agent_id": "a1"})
	_ = mem.Log("SUBAGENT_TERMINATE", map[string]interface{}{"agent_id": "a1"})

	if mem.CountEvent("SUBAGENT_SPAWN") != 1 {
		t.Errorf("expected 1 SPAWN event")
	}
	if mem.CountEvent("SUBAGENT_TERMINATE") != 1 {
		t.Errorf("expected 1 TERMINATE event")
	}
	if mem.CountEvent("UNKNOWN") != 0 {
		t.Errorf("expected 0 UNKNOWN events")
	}

	events := mem.GetEvents()
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}
