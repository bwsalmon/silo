package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func TestStartAndEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, "s-abc", Caller{CLI: "claude", Host: "local"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Start("sandbox_exec", map[string]any{"sandbox": "build", "command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.End(OutcomeOK, "", strings.Repeat("x", 1000)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	recs := readLines(t, path)
	if len(recs) != 2 {
		t.Fatalf("%d lines, want 2", len(recs))
	}
	s, e := recs[0], recs[1]
	if s.Event != "start" || s.Session != "s-abc" || s.Caller.CLI != "claude" || s.Tool != "sandbox_exec" || s.Call != e.Call {
		t.Errorf("start line %+v", s)
	}
	if args, _ := s.Args.(map[string]any); args["command"] != "ls" {
		t.Errorf("start args %v", s.Args)
	}
	if e.Event != "end" || e.Outcome != OutcomeOK || *e.Size != 1000 || len(e.Head) != HeadBytes || e.DurationMS == nil {
		t.Errorf("end line %+v", e)
	}
}

func TestRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, "s-abc", Caller{})
	if err != nil {
		t.Fatal(err)
	}
	l.Register("gh", "ghp_SECRETVALUE")
	l.Register("gh-long", "ghp_SECRETVALUE-and-more")
	l.Register("tiny", "ab") // below MinSecretLength: not redacted
	c, _ := l.Start("sandbox_exec", map[string]any{
		"command": "echo ghp_SECRETVALUE",
		"env":     map[string]any{"ghp_SECRETVALUE": []any{"x ghp_SECRETVALUE-and-more y"}},
	})
	// The secret straddles the head's cut.
	result := strings.Repeat(".", HeadBytes-5) + "ghp_SECRETVALUE"
	c.End(OutcomeError, "failed with ghp_SECRETVALUE", result)
	l.Close()
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Contains(text, "SECRET") || strings.Contains(text, "ghp_") {
		t.Fatalf("secret in the audit log:\n%s", text)
	}
	for _, want := range []string{
		`echo [credential:gh]`,
		`"[credential:gh]":["x [credential:gh-long] y"]`,
		`failed with [credential:gh]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %s:\n%s", want, text)
		}
	}
	if got := l.Redact("ab ghp_SECRETVALUE"); got != "ab [credential:gh]" {
		t.Errorf("Redact = %q", got)
	}
}

// TestCrashMidCallLeavesStartLine kills a process between a call's start
// and its end, as a crash would, and finds the start line in the log.
func TestCrashMidCallLeavesStartLine(t *testing.T) {
	if path := os.Getenv("SILO_AUDIT_CRASH_LOG"); path != "" {
		l, err := Open(path, "s-crash", Caller{})
		if err != nil {
			os.Exit(2)
		}
		if _, err := l.Start("sandbox_exec", map[string]any{"command": "sleep 100"}); err != nil {
			os.Exit(2)
		}
		// Die without ending the call or closing the log.
		p, _ := os.FindProcess(os.Getpid())
		p.Kill()
		select {}
	}
	path := filepath.Join(t.TempDir(), FileName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashMidCallLeavesStartLine$")
	cmd.Env = append(os.Environ(), "SILO_AUDIT_CRASH_LOG="+path)
	if err := cmd.Run(); err == nil {
		t.Fatal("the child was not killed")
	}
	recs := readLines(t, path)
	if len(recs) != 1 || recs[0].Event != "start" || recs[0].Session != "s-crash" {
		t.Fatalf("records %+v, want the one start line", recs)
	}
}
