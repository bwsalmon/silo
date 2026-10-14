package ssh_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
)

// The tests run against a real sshd in a Docker container: Debian with
// openssh-server, a user silo (GatewayPorts clientspecified, so the
// server would honour a wildcard bind if silo asked for one), a user
// wide (GatewayPorts yes, which overrides silo's loopback bind), and testdata/fakecli installed as
// /usr/local/bin/claude. Without Docker they skip, unless
// SILO_V2_REQUIRE_DOCKER is set, as it is in CI.

const image = "silo-v2-sshd-test"

type sshd struct {
	dir       string // keys, ssh_config, known_hosts
	container string
	port      string
	config    string
	empty     string // an ssh_config whose known_hosts is empty
}

var (
	fixOnce sync.Once
	fix     *sshd
	fixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix != nil {
		exec.Command("docker", "rm", "-f", fix.container).Run()
		os.RemoveAll(fix.dir)
	}
	os.Exit(code)
}

// server returns the shared sshd, starting it on first use.
func server(t *testing.T) *sshd {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but docker is not working: %v", err)
		}
		t.Skipf("no docker: %v", err)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatalf("no OpenSSH client: %v", err)
	}
	fixOnce.Do(func() { fix, fixErr = startSSHD() })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	// Each test starts logged in, with no fake.json or report.
	fix.exec(t, "rm -f ~/fake.json ~/report.json && touch ~/.fake-login")
	return fix
}

func sh(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func startSSHD() (*sshd, error) {
	dir, err := os.MkdirTemp("/tmp", "silo-sshd-")
	if err != nil {
		return nil, err
	}
	s := &sshd{dir: dir}
	ctx := filepath.Join(dir, "image")
	os.MkdirAll(ctx, 0o755)
	_, file, _, _ := runtime.Caller(0)
	build := exec.Command("go", "build", "-o", filepath.Join(ctx, "claude"), "./testdata/fakecli")
	build.Dir = filepath.Dir(file)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the fake CLI: %v: %s", err, out)
	}
	dockerfile := `FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server procps net-tools \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd \
 && useradd -m -s /bin/sh silo && usermod -p '*' silo \
 && useradd -m -s /bin/sh wide && usermod -p '*' wide \
 && printf 'AllowTcpForwarding yes\nGatewayPorts clientspecified\nMatch User wide\n  GatewayPorts yes\n' >> /etc/ssh/sshd_config
COPY claude /usr/local/bin/claude
COPY entry.sh /entry.sh
CMD ["/bin/sh", "/entry.sh"]
`
	entry := `set -e
for u in silo wide; do
  mkdir -p /home/$u/.ssh
  printf '%s\n' "$KEY" > /home/$u/.ssh/authorized_keys
  chown -R $u /home/$u/.ssh
  chmod 700 /home/$u/.ssh
  chmod 600 /home/$u/.ssh/authorized_keys
done
exec /usr/sbin/sshd -D -e
`
	os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(dockerfile), 0o644)
	os.WriteFile(filepath.Join(ctx, "entry.sh"), []byte(entry), 0o644)
	if _, err := sh(ctx, "docker", "build", "-q", "-t", image, "."); err != nil {
		return nil, err
	}
	key := filepath.Join(dir, "id")
	if _, err := sh(dir, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key); err != nil {
		return nil, err
	}
	pub, _ := os.ReadFile(key + ".pub")
	id, err := sh(dir, "docker", "run", "-d", "--rm", "-e", "KEY="+strings.TrimSpace(string(pub)), "-p", "127.0.0.1::22", image)
	if err != nil {
		return nil, err
	}
	s.container = strings.TrimSpace(id)
	out, err := sh(dir, "docker", "port", s.container, "22/tcp")
	if err != nil {
		return s, err
	}
	_, s.port, _ = strings.Cut(strings.TrimSpace(strings.Split(out, "\n")[0]), "127.0.0.1:")
	var keys string
	for i := 0; i < 100; i++ {
		if keys, err = sh(dir, "ssh-keyscan", "-p", s.port, "127.0.0.1"); err == nil && strings.Contains(keys, "ssh-ed25519") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(keys, "ssh-ed25519") {
		return s, fmt.Errorf("sshd did not come up: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(keys), 0o600)
	os.WriteFile(filepath.Join(dir, "empty_hosts"), nil, 0o600)
	conf := func(kh string) string {
		return fmt.Sprintf(`Host silotest
  HostName 127.0.0.1
  Port %s
  User silo
  IdentityFile %s
  IdentitiesOnly yes
  UserKnownHostsFile %s
  GlobalKnownHostsFile /dev/null
  LogLevel ERROR
`, s.port, key, kh)
	}
	s.config = filepath.Join(dir, "ssh_config")
	s.empty = filepath.Join(dir, "ssh_config_empty")
	os.WriteFile(s.config, []byte(conf(filepath.Join(dir, "known_hosts"))), 0o600)
	os.WriteFile(s.empty, []byte(conf(filepath.Join(dir, "empty_hosts"))), 0o600)
	return s, nil
}

// transport reaches the sshd as the Host alias silotest.
func (s *sshd) transport() *sshloc.OpenSSH {
	return &sshloc.OpenSSH{Target: "silotest", Options: []string{"-F", s.config}}
}

// exec runs a shell command in the container as the user silo.
func (s *sshd) exec(t *testing.T, script string) string {
	t.Helper()
	out, err := sh("", "docker", "exec", "-u", "silo", "-w", "/home/silo", s.container, "sh", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeConfig is the fake CLI's ~/fake.json.
type fakeConfig struct {
	Call  string         `json:"call,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	Exit  int            `json:"exit"`
	Wait  bool           `json:"wait,omitempty"`
	Sleep int            `json:"sleep,omitempty"`
}

type fakeReport struct {
	PID    int      `json:"pid"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	TTY    bool     `json:"tty"`
	URL    string   `json:"url"`
	Tools  []string `json:"tools"`
	Result string   `json:"result"`
	Error  string   `json:"error"`
	Signal string   `json:"signal"`
}

func (s *sshd) fake(t *testing.T, c fakeConfig) {
	t.Helper()
	b, _ := json.Marshal(c)
	s.exec(t, "printf '%s' "+sshloc.Quote(string(b))+" > ~/fake.json")
}

// report waits for the fake CLI's report.
func (s *sshd) report(t *testing.T) fakeReport {
	t.Helper()
	var r fakeReport
	for i := 0; i < 300; i++ {
		out, _ := sh("", "docker", "exec", "-u", "silo", s.container, "cat", "/home/silo/report.json")
		if out != "" && json.Unmarshal([]byte(out), &r) == nil {
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the fake CLI wrote no report")
	return r
}

// remoteDirs lists the remote session directories.
func (s *sshd) remoteDirs(t *testing.T) []string {
	t.Helper()
	return strings.Fields(s.exec(t, "ls -d /tmp/silo.* 2>/dev/null || true"))
}
