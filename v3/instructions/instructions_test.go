package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveRoot(t *testing.T) {
	dir := t.TempDir()
	text, srcs, err := EffectiveRoot(filepath.Join(dir, "shipped.md"), filepath.Join(dir, "etc.md"), filepath.Join(dir, "root.d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Path != ShippedRootPath || !strings.Contains(text, "You are the root agent") {
		t.Fatalf("default root.md: %d sources, text=%q", len(srcs), text)
	}

	etcPath := filepath.Join(dir, "etc.md")
	dDir := filepath.Join(dir, "root.d")
	os.MkdirAll(dDir, 0o755)
	os.WriteFile(etcPath, []byte("# Custom Root\n"), 0o644)
	os.WriteFile(filepath.Join(dDir, "20-second.md"), []byte("second rule"), 0o644)
	os.WriteFile(filepath.Join(dDir, "10-first.md"), []byte("first rule"), 0o644)

	text, srcs, err = EffectiveRoot(filepath.Join(dir, "shipped.md"), etcPath, dDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 3 || !strings.HasPrefix(text, "# Custom Root") ||
		strings.Index(text, "first rule") > strings.Index(text, "second rule") {
		t.Fatalf("assembled: %q (%+v)", text, srcs)
	}
}
