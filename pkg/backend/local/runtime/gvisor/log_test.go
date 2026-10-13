//go:build linux

package gvisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.json")
	if o, e, err := ReadLog(path); err != nil || o != nil || e != nil {
		t.Fatalf("a log not written yet: %q %q %v", o, e, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &logWriter{f: f}
	copyLines(w, "stdout", strings.NewReader("one\ntwo\nno newline"))
	copyLines(w, "stderr", strings.NewReader("err\n"))
	copyLines(w, "stdout", strings.NewReader(strings.Repeat("x", maxLine+10)+"\n"))
	// A copier killed mid-line leaves a torn entry, which is skipped.
	f.WriteString(`{"log":"torn`)
	f.Close()
	o, e, err := ReadLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "one\ntwo\nno newline" + strings.Repeat("x", maxLine+10) + "\n"; string(o) != want {
		t.Errorf("stdout: %q", o[:min(len(o), 40)])
	}
	if string(e) != "err\n" {
		t.Errorf("stderr: %q", e)
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 6 {
		t.Errorf("%d entries, want 6 (a long line split in two):\n%s", n, b)
	}
}

func TestNumericUser(t *testing.T) {
	for _, c := range []struct {
		in       string
		uid, gid uint32
		ok       bool
	}{
		{"", 0, 0, true}, {"root", 0, 0, true}, {"1000", 1000, 1000, true}, {"1000:50", 1000, 50, true},
		{"root:5", 0, 5, true}, {"nobody", 0, 0, false}, {"1:staff", 0, 0, false},
	} {
		uid, gid, err := numericUser(c.in)
		if (err == nil) != c.ok || (c.ok && (uid != c.uid || gid != c.gid)) {
			t.Errorf("%q: %d %d %v", c.in, uid, gid, err)
		}
	}
}
