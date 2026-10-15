// Adapted from github.com/bwsalmon/grain pkg/gitproxy/audit_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestJSONAuditLogAppendsOneJSONLinePerEntry(t *testing.T) {
	var buf bytes.Buffer
	log := &JSONAuditLog{W: &buf}
	log.Record(AuditEntry{
		Time:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Caller: map[string]string{"session": "s-0"}, Repo: "github.com/acme/widgets",
		Action: "git-receive-pack", Credential: "bot", Outcome: "forwarded: 200", Status: 200,
		Refs: []RefGrant{{Ref: "refs/heads/silo/x", Kind: "create", Caller: map[string]string{"pattern": "refs/heads/silo/*"}}},
	})
	log.Record(AuditEntry{
		Caller: map[string]string{"session": "s-1"}, Repo: "github.com/acme/gadgets",
		Action: "git-receive-pack", Outcome: "denied: not in scope", Status: 403,
	})

	scanner := bufio.NewScanner(&buf)
	var lines []map[string]any
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0]["repo"] != "github.com/acme/widgets" || lines[0]["credential"] != "bot" ||
		lines[0]["time"] != "2026-01-02T03:04:05Z" || lines[0]["caller"].(map[string]any)["session"] != "s-0" {
		t.Errorf("line 0 = %v", lines[0])
	}
	refs, _ := lines[0]["refs"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["caller"].(map[string]any)["pattern"] != "refs/heads/silo/*" {
		t.Errorf("line 0 refs = %v", lines[0]["refs"])
	}
	if lines[1]["outcome"] != "denied: not in scope" {
		t.Errorf("line 1 = %v", lines[1])
	}
	if _, ok := lines[1]["credential"]; ok {
		t.Errorf("expected no credential field when none was selected, got %v", lines[1]["credential"])
	}
}

func TestRecordingAuditLogCollectsEntriesInMemory(t *testing.T) {
	log := &RecordingAuditLog{}
	log.Record(AuditEntry{Caller: map[string]string{"session": "s-0"}, Repo: "github.com/acme/widgets", Action: "info/refs"})
	if e := log.Entries(); len(e) != 1 || e[0].Caller["session"] != "s-0" {
		t.Errorf("Entries = %+v", e)
	}
}

func TestNullAuditLogDiscardsEverything(t *testing.T) {
	// Must not panic; nothing else to assert.
	NullAuditLog{}.Record(AuditEntry{})
}
