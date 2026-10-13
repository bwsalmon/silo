package units

import (
	"slices"
	"strings"
	"testing"
)

func unitMap(t *testing.T, c Config) map[string]string {
	t.Helper()
	us, err := Units(c)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	var names []string
	for _, u := range us {
		m[u.Name] = u.Content
		names = append(names, u.Name)
	}
	if !slices.Equal(names, c.Names()) {
		t.Fatalf("units %v, want Names() %v", names, c.Names())
	}
	return m
}

func lines(s string) []string { return strings.Split(s, "\n") }

func TestRootless(t *testing.T) {
	c := Config{Mode: Rootless, Silod: "/usr/local/bin/silod", DataDir: "/home/op/.local/share/silo", Buildkitd: true}
	m := unitMap(t, c)
	if got := c.Names(); !slices.Equal(got, []string{NSD, Containerd, Buildkitd, Silod}) {
		t.Fatalf("Names = %v", got)
	}
	for name, want := range map[string][]string{
		NSD: {"ExecStart=/usr/local/bin/silod nsd --silos /home/op/.local/share/silo/silos --addr " + Gateway},
		Containerd: {
			"Wants=silo-nsd.service", "After=silo-nsd.service",
			"ExecStart=/usr/local/bin/silod containerd --data-dir /home/op/.local/share/silo --runtime-dir %t/silo",
			"Delegate=yes", "KillMode=process",
		},
		Buildkitd: {"Wants=silo-nsd.service silo-containerd.service", "After=silo-nsd.service silo-containerd.service"},
		Silod: {
			"Wants=silo-nsd.service silo-containerd.service silo-buildkitd.service",
			"ExecStart=/usr/local/bin/silod serve --backend=local --data-dir /home/op/.local/share/silo --migrate --socket %t/silo/silo.sock",
		},
	} {
		for _, w := range want {
			if !slices.Contains(lines(m[name]), w) {
				t.Errorf("%s has no line %q:\n%s", name, w, m[name])
			}
		}
	}
	for name, content := range m {
		for _, bad := range []string{"User=", "BindsTo=", "StateDirectory=", "RuntimeDirectory="} {
			if strings.Contains(content, bad) {
				t.Errorf("rootless %s has %s:\n%s", name, bad, content)
			}
		}
		if !slices.Contains(lines(content), "WantedBy=default.target") || !slices.Contains(lines(content), "Restart=on-failure") {
			t.Errorf("%s:\n%s", name, content)
		}
	}
}

// The registry's host port is a flag on silod's ExecStart, and only
// when it is on.
func TestRegistryHostPort(t *testing.T) {
	base := "ExecStart=/s serve --backend=local --data-dir /d --migrate --socket %t/silo/silo.sock"
	for port, want := range map[int]string{0: base, 5115: base + " --registry-host-listen 127.0.0.1:5115"} {
		m := unitMap(t, Config{Mode: Rootless, Silod: "/s", DataDir: "/d", RegistryHostPort: port})
		if !slices.Contains(lines(m[Silod]), want) {
			t.Errorf("port %d: silod.service has no line %q:\n%s", port, want, m[Silod])
		}
	}
}

func TestSystem(t *testing.T) {
	c := Config{Mode: System, Silod: "/usr/bin/silod", DataDir: "/var/lib/silo"}
	m := unitMap(t, c)
	if _, ok := m[Buildkitd]; ok {
		t.Error("silo-buildkitd.service written without Buildkitd")
	}
	for name, content := range m {
		for _, w := range []string{"User=silo", "Group=silo", "StateDirectory=silo", "StateDirectoryMode=0700", "WantedBy=multi-user.target"} {
			if !slices.Contains(lines(content), w) {
				t.Errorf("%s has no line %q:\n%s", name, w, content)
			}
		}
		if strings.Contains(content, "%t") {
			t.Errorf("%s names the user manager's runtime directory:\n%s", name, content)
		}
		if strings.Contains(content, "AmbientCapabilities") {
			t.Errorf("%s grants capabilities (call L34 needs none):\n%s", name, content)
		}
	}
	for name, want := range map[string][]string{
		NSD: {
			"RuntimeDirectory=silo-nsd", "RuntimeDirectoryPreserve=yes",
			"ExecStart=/usr/bin/silod nsd --silos /var/lib/silo/silos --addr " + Gateway + " --socket /run/silo-nsd/nsd.sock --state /run/silo-nsd/nsd.state",
		},
		Containerd: {"ExecStart=/usr/bin/silod containerd --data-dir /var/lib/silo --runtime-dir /run/silo-nsd"},
		Silod: {
			"RuntimeDirectory=silo", "RuntimeDirectoryMode=0755", "Delegate=yes",
			"Wants=silo-nsd.service silo-containerd.service",
			"ExecStart=/usr/bin/silod serve --backend=local --data-dir /var/lib/silo --migrate --socket /run/silo/silo.sock --nsd-state /run/silo-nsd/nsd.state",
		},
	} {
		for _, w := range want {
			if !slices.Contains(lines(m[name]), w) {
				t.Errorf("%s has no line %q:\n%s", name, w, m[name])
			}
		}
	}
	if d, _ := c.Dir(); d != "/etc/systemd/system" {
		t.Errorf("Dir = %q", d)
	}
}

func TestUserDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if d, err := (Config{Mode: Rootless}).Dir(); err != nil || d != "/cfg/systemd/user" {
		t.Errorf("Dir = %q, %v", d, err)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []Config{
		{Mode: "rootful", Silod: "/s", DataDir: "/d"},
		{Mode: Rootless, Silod: "silod", DataDir: "/d"},
		{Mode: Rootless, Silod: "/s", DataDir: "rel"},
		{Mode: Rootless, Silod: "/s", DataDir: "/my data"},
		{Mode: System, Silod: "/s", DataDir: "/d", User: "a%b"},
		{Mode: Rootless, Silod: "/s", DataDir: "/d", RegistryHostPort: 65536},
	} {
		if _, err := Units(c); err == nil {
			t.Errorf("Units(%+v) succeeded", c)
		}
	}
}
