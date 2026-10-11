package harness_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/harness"
)

var claudeLogin = map[string][]byte{
	".claude/.credentials.json": []byte(`{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1"}}`),
	".claude.json":              []byte(`{"oauthAccount":{"emailAddress":"bob@example.com"}}`),
}

func subscriptionSpec(lay harness.Layout, agent string, slot int) harness.Spec {
	a := harness.ClaudeCode{}
	s := harness.NewSpec(lay, agent, slot, a, a.Command(harness.Launch{Model: "m", Prompt: "p", SessionID: "sid", Subscription: true}), harness.Limits{})
	s.Subscribe(lay, "anthropic", claudeLogin)
	return s
}

func hasEnv(env []string, prefix string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// TestSubscriptionSpec: a subscription session's container mounts the
// socket, its private directory and its login files, read-only, and
// nothing else, and is on the egress network; an API-key session's
// gets no files and no network.
func TestSubscriptionSpec(t *testing.T) {
	lay := layout
	lay.LoginDir = "/run/silo/logins"
	s := subscriptionSpec(lay, "m3x8re", 2)
	if err := s.Validate(lay); err != nil {
		t.Fatal(err)
	}
	want := []harness.Mount{
		{Source: "/run/silo/sessions/m3x8re/silo.sock", Target: harness.SocketPath},
		{Source: "/var/lib/silo/sessions/m3x8re", Target: harness.Home},
		{Source: "/run/silo/logins/m3x8re", Target: harness.LoginPath, ReadOnly: true},
	}
	if !slices.Equal(s.Mounts, want) {
		t.Errorf("mounts %+v, want %+v", s.Mounts, want)
	}
	if s.Network != harness.NetworkEgress || s.Provider != "anthropic" || !s.DropMetadata || len(s.Login) != 2 {
		t.Errorf("network %q, provider %q, drop metadata %v, %d login files", s.Network, s.Provider, s.DropMetadata, len(s.Login))
	}
	// harness-init copies the login in; the CLI uses it, not the
	// model proxy.
	if i := slices.Index(s.Argv, "--login"); i < 0 || s.Argv[i+1] != harness.LoginPath || i > slices.Index(s.Argv, "--") {
		t.Errorf("argv %q: want harness-init's --login before --", s.Argv)
	}
	if hasEnv(s.Env, "ANTHROPIC_API_KEY=") || hasEnv(s.Env, "ANTHROPIC_BASE_URL=") {
		t.Errorf("a subscription session's env %q points it at the model proxy", s.Env)
	}
	c := harness.HarnessContainer(&s, "img@sha256:1")
	if len(c.HostConfig.Mounts) != 3 || !c.HostConfig.Mounts[2].ReadOnly || c.HostConfig.Mounts[2].Target != harness.LoginPath {
		t.Errorf("harness container mounts %+v", c.HostConfig.Mounts)
	}
	n := harness.NetContainer(&s, "img@sha256:1")
	if n.HostConfig.NetworkMode != harness.EgressNetwork || n.NetworkDisabled || len(n.HostConfig.CapAdd) != 0 {
		t.Errorf("namespace container %+v", n)
	}
	proxy := netip.MustParseAddrPort("172.30.0.1:3128")
	script := strings.Join(harness.FirewallContainer(&s, "img@sha256:1", proxy).Cmd, " ")
	for _, rule := range []string{
		"-A OUTPUT -d 169.254.169.254 -j DROP",
		"-A OUTPUT -p tcp -d 172.30.0.1 --dport 3128 -j ACCEPT",
		"-A OUTPUT -j DROP",
	} {
		if !strings.Contains(script, rule) {
			t.Errorf("the firewall lacks %q: %s", rule, script)
		}
	}

	// An API-key session: no files, no network, the model proxy.
	k := spec(t, 2)
	if k.Network != harness.NetworkNone || k.Provider != "" || len(k.Login) != 0 || len(k.Mounts) != 2 {
		t.Errorf("an API-key session's spec: network %q, provider %q, %d files, mounts %+v", k.Network, k.Provider, len(k.Login), k.Mounts)
	}
	if !hasEnv(k.Env, "ANTHROPIC_API_KEY="+harness.ProxyKey) || slices.Contains(k.Argv, "--login") {
		t.Errorf("an API-key session's env %q, argv %q", k.Env, k.Argv)
	}

	for name, edit := range map[string]func(*harness.Spec){
		"writable login":    func(s *harness.Spec) { s.Mounts[2].ReadOnly = false },
		"another's login":   func(s *harness.Spec) { s.Mounts[2].Source = "/run/silo/logins/other" },
		"no files":          func(s *harness.Spec) { s.Login = nil },
		"unknown provider":  func(s *harness.Spec) { s.Provider = "acme" },
		"escaping file":     func(s *harness.Spec) { s.Login = map[string][]byte{"../etc/passwd": nil} },
		"absolute file":     func(s *harness.Spec) { s.Login = map[string][]byte{"/etc/passwd": nil} },
		"no login mount":    func(s *harness.Spec) { s.Mounts = s.Mounts[:2] },
		"files, no network": func(s *harness.Spec) { s.Network = harness.NetworkNone },
		"host network":      func(s *harness.Spec) { s.Network = "host" },
	} {
		s := subscriptionSpec(lay, "m3x8re", 2)
		edit(&s)
		if err := s.Validate(lay); err == nil {
			t.Errorf("%s: a spec with it passes", name)
		}
	}
	// An API-key session may not be given a provider or files.
	k.Provider = "anthropic"
	if err := k.Validate(lay); err == nil {
		t.Error("an API-key session with a provider passes")
	}
}

func TestCodexSubscriptionCommand(t *testing.T) {
	c := harness.Codex{}.Command(harness.Launch{Model: "gpt-5", Prompt: "p", Subscription: true})
	argv := strings.Join(c.Argv, " ")
	if strings.Contains(argv, `model_provider="silo"`) || hasEnv(c.Env, "OPENAI_API_KEY=") {
		t.Errorf("a codex subscription session goes to the model proxy: %s %q", argv, c.Env)
	}
	k := harness.Codex{}.Command(harness.Launch{Model: "gpt-5", Prompt: "p"})
	if !strings.Contains(strings.Join(k.Argv, " "), `model_provider="silo"`) || !hasEnv(k.Env, "OPENAI_API_KEY=") {
		t.Errorf("a codex API-key session doesn't: %q %q", k.Argv, k.Env)
	}
}

// TestSessiondSubscriptionTurn: sessiond puts the login files in the
// session's login directory, runs its namespace on the egress network
// with the proxy's rule and admits it to the proxy, gives the harness
// the proxy, and returns the login as the harness left it.
func TestSessiondSubscriptionTurn(t *testing.T) {
	d, e, lay := newSessiond(t)
	lay.LoginDir = filepath.Join(filepath.Dir(lay.StateDir), "logins")
	d.Layout = lay
	var listened netip.AddrPort
	d.ListenEgress = func(a netip.AddrPort) (net.Listener, error) {
		listened = a
		return net.Listen("tcp", "127.0.0.1:0")
	}
	listen(t, lay.Socket("a1"))
	s := subscriptionSpec(lay, "a1", 4)
	refreshed := []byte(`{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`)
	e.during = func() {
		// What harness-init and the CLI do in the container: copy the
		// login in, and refresh it.
		if err := harness.CopyLogin(lay.Login("a1"), lay.Private("a1")); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(lay.Private("a1"), ".claude/.credentials.json"), refreshed, 0o600); err != nil {
			t.Error(err)
		}
	}
	res, err := d.Turn(context.Background(), s, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	for p, b := range claudeLogin {
		if got, err := os.ReadFile(filepath.Join(lay.Login("a1"), p)); err != nil || string(got) != string(b) {
			t.Errorf("login file %s: %q %v", p, got, err)
		}
	}
	if string(res.Login[".claude/.credentials.json"]) != string(refreshed) || string(res.Login[".claude.json"]) != string(claudeLogin[".claude.json"]) {
		t.Errorf("the login read back: %q", res.Login)
	}
	if !slices.Contains(e.calls, "network "+harness.EgressNetwork) || !slices.Contains(e.calls, "address silo-session-a1-net "+harness.EgressNetwork) {
		t.Errorf("calls %q", e.calls)
	}
	if listened != netip.MustParseAddrPort("127.0.0.1:3128") {
		t.Errorf("the proxy listens at %s, want the network's gateway", listened)
	}
	c := e.created["silo-session-a1"]
	if !slices.Contains(c.Env, "HTTPS_PROXY=http://127.0.0.1:3128") || !hasEnv(c.Env, "NO_PROXY=127.0.0.1") {
		t.Errorf("the harness's env %q", c.Env)
	}
	if m := c.HostConfig.Mounts[2]; m.Source != lay.Login("a1") || m.Target != harness.LoginPath || !m.ReadOnly {
		t.Errorf("login mount %+v", m)
	}
	if fw := e.created["silo-session-a1-fw"]; !strings.Contains(strings.Join(fw.Cmd, " "), "--dport 3128 -j ACCEPT") {
		t.Errorf("firewall %q", fw.Cmd)
	}
	if err := d.Proxy.Allowed(netip.MustParseAddr("172.30.0.5"), "api.anthropic.com:443"); err != nil {
		t.Errorf("the session isn't admitted to the proxy: %v", err)
	}
	// What the session stopping reads back.
	if got, err := d.Login(context.Background(), "a1", []string{".claude/.credentials.json", "missing"}); err != nil || len(got) != 1 {
		t.Errorf("Login: %q %v", got, err)
	}
	// A symlink the harness leaves isn't followed.
	os.Remove(filepath.Join(lay.Private("a1"), ".claude.json"))
	os.Symlink("/etc/passwd", filepath.Join(lay.Private("a1"), ".claude.json"))
	if got, _ := d.Login(context.Background(), "a1", []string{".claude.json"}); len(got) != 0 {
		t.Errorf("a symlinked login file was read: %q", got)
	}
	if err := d.Remove(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lay.Login("a1")); !os.IsNotExist(err) {
		t.Errorf("the login files outlive the session: %v", err)
	}
	if err := d.Proxy.Allowed(netip.MustParseAddr("172.30.0.5"), "api.anthropic.com:443"); err == nil {
		t.Error("a removed session is still admitted to the proxy")
	}
}

func TestCopyLogin(t *testing.T) {
	src, home := t.TempDir(), t.TempDir()
	for p, b := range claudeLogin {
		os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o700)
		os.WriteFile(filepath.Join(src, p), b, 0o400)
	}
	// A symlink in the home where a login file goes is replaced, not
	// followed.
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("untouched"), 0o600)
	os.Symlink(outside, filepath.Join(home, ".claude.json"))
	if err := harness.CopyLogin(src, home); err != nil {
		t.Fatal(err)
	}
	for p, b := range claudeLogin {
		if got, err := os.ReadFile(filepath.Join(home, p)); err != nil || string(got) != string(b) {
			t.Errorf("%s: %q %v", p, got, err)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "untouched" {
		t.Errorf("the copy followed a symlink: %q", b)
	}
}

func TestEgressProxyAllowlist(t *testing.T) {
	p := &harness.EgressProxy{}
	claude, codex, stranger := netip.MustParseAddr("172.30.0.5"), netip.MustParseAddr("172.30.0.6"), netip.MustParseAddr("172.30.0.7")
	p.Admit(claude, "anthropic")
	p.Admit(codex, "openai")
	for _, c := range []struct {
		from netip.Addr
		host string
		ok   bool
	}{
		{claude, "api.anthropic.com:443", true},
		{claude, "console.anthropic.com:443", true},
		{claude, "API.Anthropic.com.:443", true},
		{claude, "api.anthropic.com:80", false},
		{claude, "api.openai.com:443", false},
		{claude, "169.254.169.254:443", false},
		{claude, "metadata.google.internal:443", false},
		{claude, "evil.api.anthropic.com:443", false},
		{codex, "api.openai.com:443", true},
		{codex, "auth.openai.com:443", true},
		{codex, "api.anthropic.com:443", false},
		{stranger, "api.anthropic.com:443", false},
	} {
		if err := p.Allowed(c.from, c.host); (err == nil) != c.ok {
			t.Errorf("%s to %s: %v, want allowed %v", c.from, c.host, err, c.ok)
		}
	}
	p.Forget(claude)
	if p.Allowed(claude, "api.anthropic.com:443") == nil {
		t.Error("a forgotten session is still allowed")
	}
}

// TestEgressProxyConnects: an allowed CONNECT is carried to the host,
// on an address it resolves to that isn't internal; a host resolving
// to an internal address is refused.
func TestEgressProxyConnects(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	p := &harness.EgressProxy{
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "console.anthropic.com" {
				return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("203.0.113.9")}, nil
		},
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			a, b := net.Pipe()
			go func() { // an upstream that says hello
				io.WriteString(b, "hello from "+addr)
				b.Close()
			}()
			return a, nil
		},
	}
	p.Admit(netip.MustParseAddr("127.0.0.1"), "anthropic")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go p.Serve(l)
	connect := func(host string) (int, string) {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, ""
		}
		b, _ := io.ReadAll(br)
		return resp.StatusCode, string(b)
	}
	if code, body := connect("api.anthropic.com:443"); code != 200 || body != "hello from 203.0.113.9:443" {
		t.Errorf("an allowed CONNECT: %d %q", code, body)
	}
	if code, _ := connect("api.openai.com:443"); code != http.StatusForbidden {
		t.Errorf("another provider's host: %d", code)
	}
	if code, _ := connect("console.anthropic.com:443"); code != http.StatusBadGateway {
		t.Errorf("a host resolving to the metadata server: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(dialed, "10.0.0.1:443") || slices.Contains(dialed, "169.254.169.254:443") {
		t.Errorf("dialed an internal address: %q", dialed)
	}
}

// logins is a fake Logins: one subscription, one API key.
type logins struct {
	mu     sync.Mutex
	files  map[string][]byte
	stored []map[string][]byte
}

func (l *logins) SessionCredential(_ context.Context, name string) (agents.Credential, error) {
	if name == "claude-max-bob" {
		return agents.Credential{Name: name, Provider: "anthropic", Kind: agents.CredentialSubscription}, nil
	}
	return agents.Credential{Name: name, Provider: "anthropic", Kind: agents.CredentialAPIKey}, nil
}

func (l *logins) CredentialFiles(_ context.Context, name string) (map[string][]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return maps.Clone(l.files), nil
}

func (l *logins) StoreCredentialFiles(_ context.Context, name string, files map[string][]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stored = append(l.stored, files)
	l.files = maps.Clone(files)
	return nil
}

func (l *logins) writes() []map[string][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.stored)
}

// TestRunnerWritesBackARefreshedLogin: a subscription session's turn is
// given its login files; a turn that leaves them as they were writes
// nothing, one that refreshed them writes them back, and so does a
// session stopping with a refresh its last turn didn't return.
func TestRunnerWritesBackARefreshedLogin(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{lines: transcript(t, "claude-code.jsonl"), login: maps.Clone(claudeLogin)}
	r := newRunner(c, rt, "ctl-a", 2)
	r.Layout.LoginDir = "/run/silo/logins"
	lg := &logins{files: maps.Clone(claudeLogin)}
	r.Logins = lg
	s := session("a1", "claude-code")
	s.Credential = "claude-max-bob"
	c.add(s, agents.StateProvisioning, "")
	if err := r.Start(ctx, s); err != nil {
		t.Fatal(err)
	}
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	first := rt.turns()[0]
	if err := first.Validate(r.Layout); err != nil {
		t.Error(err)
	}
	if first.Network != harness.NetworkEgress || first.Provider != "anthropic" || !maps.EqualFunc(first.Login, claudeLogin, slices.Equal) {
		t.Errorf("first turn: network %q, provider %q, login %q", first.Network, first.Provider, first.Login)
	}
	if hasEnv(first.Env, "ANTHROPIC_API_KEY=") {
		t.Errorf("env %q", first.Env)
	}
	if w := lg.writes(); len(w) != 0 {
		t.Errorf("an unchanged login was written back: %q", w)
	}

	// The harness refreshes its login.
	refreshed := []byte(`{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`)
	rt.mu.Lock()
	rt.login = map[string][]byte{".claude/.credentials.json": refreshed, ".claude.json": claudeLogin[".claude.json"]}
	rt.mu.Unlock()
	c.mu.Lock()
	c.msgs["a1"] = []string{"Next."}
	c.mu.Unlock()
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	w := lg.writes()
	if len(w) != 1 || string(w[0][".claude/.credentials.json"]) != string(refreshed) || string(w[0][".claude.json"]) != string(claudeLogin[".claude.json"]) {
		t.Fatalf("write-back %q", w)
	}

	// The next turn is given the refreshed login; the session stops
	// with another refresh, which is written back too.
	c.mu.Lock()
	c.msgs["a1"] = []string{"Again."}
	c.mu.Unlock()
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	if third := rt.turns()[2]; string(third.Login[".claude/.credentials.json"]) != string(refreshed) {
		t.Errorf("the third turn's login %q", third.Login)
	}
	rt.mu.Lock()
	rt.stopLogin = map[string][]byte{".claude/.credentials.json": []byte("r3")}
	rt.mu.Unlock()
	if err := r.Stop(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if w := lg.writes(); len(w) != 2 || string(w[1][".claude/.credentials.json"]) != "r3" {
		t.Errorf("write-back on stop %q", w)
	}
}

// TestRunnerAPIKeySessionGetsNoLogin: with Logins set, an API-key
// session still gets no files and no network.
func TestRunnerAPIKeySessionGetsNoLogin(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{lines: transcript(t, "claude-code.jsonl")}
	r := newRunner(c, rt, "ctl-a", 2)
	lg := &logins{files: maps.Clone(claudeLogin)}
	r.Logins = lg
	c.add(session("a1", "claude-code"), agents.StateProvisioning, "")
	if err := r.Start(ctx, session("a1", "claude-code")); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(ctx, "a1")
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	s := rt.turns()[0]
	if s.Network != harness.NetworkNone || len(s.Login) != 0 || s.Provider != "" || len(s.Mounts) != 2 {
		t.Errorf("an API-key session's spec: network %q, %d files, mounts %+v", s.Network, len(s.Login), s.Mounts)
	}
	if !hasEnv(s.Env, "ANTHROPIC_API_KEY="+harness.ProxyKey) {
		t.Errorf("env %q", s.Env)
	}
}
