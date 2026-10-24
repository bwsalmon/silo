package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SocketPath != DefaultSocketPath || c.UIDMin != 61000 || c.DefaultLimits.MemoryMax != "2G" || !c.RootEnabled() || c.RootAgent.CLI != "idle" {
		t.Fatalf("defaults %+v", c)
	}
	path := filepath.Join(dir, "gateway.toml")
	os.WriteFile(path, []byte(`
admin_uids = [1000, 1001]
uid_min = 62000
uid_max = 62099
inaccessible_paths = ["/root/.ssh"]
[default_limits]
memory_max = "1G"
[root_agent]
cli = "claude"
enabled = false
[root_agent.limits]
cpu_quota = "50%"
`), 0o644)
	c, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.AdminUIDs, []int{1000, 1001}) || c.UIDMin != 62000 || c.DefaultLimits.MemoryMax != "1G" ||
		c.DefaultLimits.CPUQuota != "100%" || c.RootAgent.CLI != "claude" || c.RootEnabled() ||
		c.RootAgent.Limits.CPUQuota != "50%" || c.RootAgent.Limits.MemoryMax != "4G" {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range []string{
		`uid_min = 10`,
		`state_dir = "relative"`,
		`[default_limits]` + "\n" + `memory_max = "lots"`,
		`admin_uid = 1`,
	} {
		os.WriteFile(path, []byte(bad), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLoadCLIs(t *testing.T) {
	dir := t.TempDir()
	clis, err := LoadCLIs(filepath.Join(dir, "none"))
	if err != nil || len(clis) != 1 || clis["idle"].Command[0] != "/bin/sleep" {
		t.Fatalf("builtins %v %v", clis, err)
	}
	os.WriteFile(filepath.Join(dir, "claude.toml"), []byte("command = [\"/opt/silo/cli/claude\", \"{brief}\"]\n[env]\nA = \"1\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "g.json"), []byte(`{"name": "gemini", "command": ["/usr/bin/gemini"]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "README"), []byte("ignored"), 0o644)
	clis, err = LoadCLIs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if clis["claude"].Env["A"] != "1" || clis["claude"].Name != "claude" || clis["gemini"].Command[0] != "/usr/bin/gemini" || len(clis) != 3 {
		t.Fatalf("clis %+v", clis)
	}
	for name, content := range map[string]string{
		"rel.toml":   `command = ["claude"]`,
		"empty.toml": `name = "x"`,
		"env.toml":   "command = [\"/x\"]\n[env]\n\"A B\" = \"1\"",
		"name.json":  `{"name": "Bad Name", "command": ["/x"]}`,
		"dup.json":   `{"name": "claude", "command": ["/x"]}`,
		"typo.json":  `{"comand": ["/x"]}`,
	} {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "claude.toml"), []byte(`command = ["/x"]`), 0o644)
		os.WriteFile(filepath.Join(d, name), []byte(content), 0o644)
		if _, err := LoadCLIs(d); err == nil {
			t.Errorf("%s: accepted %q", name, content)
		}
	}
}

func TestCLILauncher(t *testing.T) {
	l := &CLILauncher{CLIs: map[string]CLIDef{"c": {Name: "c", Command: []string{"/bin/c", "--id={agent}", "--home", "{home}", "{brief}"}, Env: map[string]string{"K": "v"}}}}
	p, err := l.Prepare(context.Background(), LaunchRequest{Agent: "a", CLI: "c", Brief: "do {agent}", User: UserInfo{Home: "/h"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Argv, []string{"/bin/c", "--id=a", "--home", "/h", "do {agent}"}) || p.Env["K"] != "v" {
		t.Fatalf("process %+v", p)
	}
	if _, err := l.Prepare(context.Background(), LaunchRequest{CLI: "zz"}); err == nil || !strings.Contains(err.Error(), "configured: c") {
		t.Fatalf("unknown cli: %v", err)
	}
}
