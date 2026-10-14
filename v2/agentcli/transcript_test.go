package agentcli_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/events"
	"github.com/bwsalmon/silo/v2/silofile"
)

// TestTranscriptGolden feeds each recorded CLI output in
// testdata/transcripts through its CLI's parser and compares the events
// and the outcome with <name>.golden. The fixtures are grain's recorded
// lines (pkg/agent/{claude,codex} tests); run go test ./agentcli
// -update after a deliberate change.
func TestTranscriptGolden(t *testing.T) {
	inputs, _ := filepath.Glob(filepath.Join("testdata", "transcripts", "*.jsonl"))
	if len(inputs) == 0 {
		t.Fatal("no fixtures")
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".jsonl")
		t.Run(name, func(t *testing.T) {
			cli, _, _ := strings.Cut(name, "-")
			m, err := agentcli.Lookup(cli)
			if err != nil {
				t.Fatal(err)
			}
			p := agentcli.NewParser(m, agentcli.Request{Agent: silofile.Agent{CLI: cli, Model: "gpt-5-codex"}})
			if p == nil {
				t.Fatalf("%s has no parser", cli)
			}
			raw, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			sc := bufio.NewScanner(bytes.NewReader(raw))
			for sc.Scan() {
				for _, e := range p.Line(sc.Bytes()) {
					b, _ := json.Marshal(e)
					got.Write(append(b, '\n'))
				}
			}
			b, _ := json.MarshalIndent(p.Outcome(), "", "  ")
			got.WriteString("outcome: " + string(b) + "\n")
			golden := filepath.Join("testdata", "transcripts", name+".golden")
			if *update {
				if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != string(want) {
				t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", golden, got.String(), want)
			}
		})
	}
}

// TestClassification pins each failure fixture's kind, so that a golden
// rewrite cannot quietly change one.
func TestClassification(t *testing.T) {
	want := map[string]string{
		"claude-success": "", "claude-usage-limit": events.CLIErrorUsageLimit, "claude-hit-limit": events.CLIErrorUsageLimit,
		"claude-overloaded": events.CLIErrorOverloaded, "claude-refused": events.CLIErrorRefused,
		"claude-max-turns": events.CLIErrorMaxTurns, "claude-error": events.CLIErrorOther, "claude-truncated": "",
		"codex-success": "", "codex-usage-limit": events.CLIErrorUsageLimit, "codex-quota": events.CLIErrorUsageLimit,
		"codex-overloaded": events.CLIErrorOverloaded, "codex-refused": events.CLIErrorRefused,
		"codex-error": events.CLIErrorOther, "codex-legacy-item-type": "",
	}
	for name, kind := range want {
		cli, _, _ := strings.Cut(name, "-")
		m, _ := agentcli.Lookup(cli)
		p := agentcli.NewParser(m, agentcli.Request{})
		raw, err := os.ReadFile(filepath.Join("testdata", "transcripts", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range bytes.Split(raw, []byte("\n")) {
			p.Line(l)
		}
		o := p.Outcome()
		got := ""
		if o.Failure != nil {
			got = o.Failure.Kind
		}
		if got != kind {
			t.Errorf("%s: failure %q, want %q (%+v)", name, got, kind, o.Failure)
		}
		if kind == "" && o.FinalText == "" && name != "claude-truncated" {
			t.Errorf("%s: no final text", name)
		}
	}
}
