package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/internal/fakecli"
	"github.com/bwsalmon/silo/v2/session"
)

func TestMain(m *testing.M) {
	if fakecli.Is() {
		os.Exit(fakecli.Main())
	}
	os.Exit(m.Run())
}

// TestRunJSON runs silo run --json end to end against the fake CLI,
// which plays back a recorded claude stream-json run, and reads the
// event stream back off stdout.
func TestRunJSON(t *testing.T) {
	root := t.TempDir()
	home, bin := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, bin, filepath.Join(root, "run"), filepath.Join(root, "state"), filepath.Join(root, "silo")} {
		os.MkdirAll(d, 0o700)
	}
	self, _ := os.Executable()
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	rec, err := os.ReadFile(filepath.Join("..", "..", "agentcli", "testdata", "transcripts", "claude-success.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(fakecli.Config{Call: "probe", Args: map[string]any{"key": "sk-very-secret"}, Stream: strings.Split(strings.TrimSpace(string(rec)), "\n")})
	os.WriteFile(filepath.Join(home, "fake.json"), cfg, 0o600)
	os.WriteFile(filepath.Join(root, "silo", "Silofile"), []byte(`silo: v2
name: json
agent: {cli: claude, mode: headless, prompt: "run the tests"}
credentials:
  - {name: key, from: {env: SILO_TEST_KEY}, to: [{agent: env, name: SILO_KEY}]}
`), 0o644)
	environ := []string{
		"PATH=" + bin, "HOME=" + home, "SILO_TEST_KEY=sk-very-secret",
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
	}
	var stdout, stderr bytes.Buffer
	code := runSession([]string{"--json", filepath.Join(root, "silo")}, strings.NewReader(""), &stdout, &stderr, session.Options{
		Environ: environ, NoGC: true, Grace: 2 * time.Second, Signals: make(chan os.Signal),
	})
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), "sk-very-secret") {
		t.Error("the credential is in the event stream")
	}
	var evs []events.Event
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		var e events.Event
		if err := events.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("stdout line %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	types := map[string]int{}
	var kinds []string
	for _, e := range evs {
		types[e.Type]++
		if e.Transcript != nil {
			kinds = append(kinds, e.Transcript.Kind)
		}
	}
	if got, want := strings.Join(kinds, ","), "thinking,text,tool_call,tool_result,tool_call,tool_result,text"; got != want {
		t.Errorf("transcript items %s, want %s", got, want)
	}
	for _, typ := range []string{events.TypeStatus, events.TypeToolCall, events.TypeUsage, events.TypeResumable} {
		if types[typ] == 0 {
			t.Errorf("no %s events: %v", typ, types)
		}
	}
	if types[events.TypeToolCall] != 2 {
		t.Errorf("%d tool_call events, want the gateway call's start and end", types[events.TypeToolCall])
	}
	res := evs[len(evs)-1].Result
	if res == nil || types[events.TypeResult] != 1 {
		t.Fatalf("the stream does not end in one result: %+v", evs[len(evs)-1])
	}
	if res.Exit.Reason != events.ReasonCompleted || res.FinalText != "The tests pass." || res.CLISession != "9f2c1d8e-5b0a-4c39-9a51-0d6c2f1e7b44" {
		t.Errorf("result %+v", res)
	}
	if len(res.UsageTotals) != 1 || res.UsageTotals[0].OutputTokens != 77+20+6 {
		t.Errorf("usage totals %+v", res.UsageTotals)
	}
	for _, p := range []string{res.AuditPath, res.TranscriptPath, res.NormalizedTranscriptPath} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
}

func TestRunJSONRefusesInteractive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte("silo: v2\nname: i\nagent: {cli: claude}\n"), 0o644)
	var stdout, stderr bytes.Buffer
	if code := runSession([]string{"--json", dir}, strings.NewReader(""), &stdout, &stderr, session.Options{}); code != exitUsage {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var e events.Event
	if err := events.Unmarshal(stdout.Bytes(), &e); err != nil || e.Result == nil || e.Result.Exit.Reason != events.ReasonSetupFailed {
		t.Errorf("stdout %q: %v", stdout.String(), err)
	}
}
