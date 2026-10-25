package acp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadCLIsAndResolve(t *testing.T) {
	dir := t.TempDir()
	// Built-in presets work when dir has no files.
	argv, _, err := ResolveCommand(filepath.Join(dir, "missing"), "gemini", nil)
	if err != nil || !reflect.DeepEqual(argv, []string{"gemini", "--experimental-acp"}) {
		t.Fatalf("builtin gemini: %v %v", argv, err)
	}
	argv, _, err = ResolveCommand(dir, "", []string{"/usr/bin/custom-acp", "--stdio"})
	if err != nil || !reflect.DeepEqual(argv, []string{"/usr/bin/custom-acp", "--stdio"}) {
		t.Fatalf("explicit command: %v %v", argv, err)
	}

	// Custom preset in clis.d overrides built-in or adds new preset.
	os.WriteFile(filepath.Join(dir, "claude.toml"), []byte(`
command = ["/opt/silo/cli/claude-code-acp", "--custom"]
[env]
CLAUDE_CODE_USE_VERTEX = "1"
`), 0o644)
	os.WriteFile(filepath.Join(dir, "myagent.json"), []byte(`{"command":["/usr/local/bin/myagent","--acp"]}`), 0o644)

	argv, env, err := ResolveCommand(dir, "claude", nil)
	if err != nil || !reflect.DeepEqual(argv, []string{"/opt/silo/cli/claude-code-acp", "--custom"}) || env["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Fatalf("override claude: %v %v %v", argv, env, err)
	}
	argv, _, err = ResolveCommand(dir, "", []string{"myagent"})
	if err != nil || !reflect.DeepEqual(argv, []string{"/usr/local/bin/myagent", "--acp"}) {
		t.Fatalf("json preset: %v %v", argv, err)
	}
}
