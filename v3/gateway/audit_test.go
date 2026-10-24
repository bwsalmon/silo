package gateway

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log", "audit.jsonl")
	clock := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	a, err := OpenAuditLog(path, clock, false)
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := a.Append("gateway", EvGatewayStart, nil)
	e2, _ := a.Append("admin:uid=0", EvSpawn, map[string]any{"agent": "x"})
	if e1.Seq != 1 || e1.Prev != "" || e2.Seq != 2 || e2.Prev == "" {
		t.Fatalf("entries %+v %+v", e1, e2)
	}
	a.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	// Reopening continues the chain.
	a, err = OpenAuditLog(path, clock, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	e3, _ := a.Append("gateway", EvStop, map[string]any{"agent": "x"})
	if e3.Seq != 3 {
		t.Fatalf("seq after reopen %d", e3.Seq)
	}
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if e3.Prev != lineHash(lines[1]) {
		t.Fatal("prev after reopen does not chain")
	}
	res, err := a.Verify()
	if err != nil || !res.OK || res.Entries != 3 {
		t.Fatalf("verify %+v %v", res, err)
	}
	got, _ := a.Read(1, 1)
	if len(got) != 1 || got[0].Seq != 2 || got[0].Actor != "admin:uid=0" || !got[0].Time.Equal(clock.t) {
		t.Fatalf("read %+v", got)
	}

	// Editing a line breaks the chain at the next one.
	tampered := bytes.Replace(data, []byte(`"agent":"x"}`), []byte(`"agent":"y"}`), 1)
	res, _ = verifyChain(bytes.NewReader(tampered))
	if res.OK || res.FirstBad != 3 {
		t.Fatalf("tampered verify %+v", res)
	}
	// So does dropping a line.
	dropped := bytes.Join([][]byte{lines[0], lines[2]}, []byte("\n"))
	res, _ = verifyChain(bytes.NewReader(dropped))
	if res.OK || res.FirstBad != 2 || !strings.Contains(res.BadReason, "seq") {
		t.Fatalf("dropped verify %+v", res)
	}
}

func TestAuditOpenRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	os.WriteFile(path, []byte("{\"seq\":1}\nnot json\n"), 0o600)
	if _, err := OpenAuditLog(path, SystemClock{}, false); err == nil {
		t.Fatal("opened a log whose last line is not an entry")
	}
}

func TestLastLineLong(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	long := strings.Repeat("x", 200<<10)
	os.WriteFile(path, []byte("first\n"+long+"\n\n"), 0o600)
	got, err := lastLine(path)
	if err != nil || string(got) != long {
		t.Fatalf("lastLine: %d bytes, %v", len(got), err)
	}
	os.WriteFile(path, nil, 0o600)
	if got, _ := lastLine(path); got != nil {
		t.Fatalf("empty file: %q", got)
	}
}
