package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SocketPath != DefaultSocketPath || c.HubURL != "unix://"+DefaultSocketPath || c.DefaultLimits.MemoryMax != "2G" || !c.RootEnabled() ||
		!c.UserAgentEnabled() || c.RootAgent.User != RootUser || c.UserAgent.Name != UserAgentID || c.UserAgent.User != UserAgentUser ||
		c.HeartbeatTimeoutSec != 90 {
		t.Fatalf("defaults %+v", c)
	}
	path := filepath.Join(dir, "gateway.toml")
	os.WriteFile(path, []byte(`
admin_uids = [1000, 1001]
hub_url = "https://silo.example.com/connect"
heartbeat_timeout_s = 30
[default_limits]
memory_max = "1G"
[root_agent]
runtime = "gemini"
enabled = false
[user_agent]
name = "dev"
user = "silo-dev"
[root_agent.limits]
cpu_quota = "50%"
`), 0o644)
	c, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.AdminUIDs, []int{1000, 1001}) || c.HubURL != "https://silo.example.com/connect" || c.DefaultLimits.MemoryMax != "1G" ||
		c.DefaultLimits.CPUQuota != "100%" || c.RootAgent.Runtime != "gemini" || c.RootEnabled() ||
		c.UserAgent.Name != "dev" || c.UserAgent.User != "silo-dev" || c.HeartbeatTimeoutSec != 30 ||
		c.RootAgent.Limits.CPUQuota != "50%" || c.RootAgent.Limits.MemoryMax != "4G" {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range []string{
		`uid_min = 62000`,
		"[user_agent]\nname = \"root\"",
		"[user_agent]\nuser = \"silo-root\"",
		"[user_agent]\nuser = \"root\"",
		"[root_agent]\nname = \"boss\"",
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
