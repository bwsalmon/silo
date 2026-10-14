package session

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/proxy"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/mcp"
	"github.com/bwsalmon/silo/v2/mcp/sources"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() {
	RegisterToolProvider("mcp", provideMCP)
	RegisterCheck("mcp", checkMCP)
}

// minimalEnv is what a launched MCP server inherits from silo's own
// environment before anything is added (design §13.3).
var minimalEnv = []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"}

// MCPResolved is one mcp[] entry as silo runs it: what --dry-run prints
// and .silo/lock.json records. It names variables and headers, never
// their values.
type MCPResolved struct {
	Name string `json:"name"`
	// Where is remote, with Host the agent host, for a server on the
	// agent host.
	Where    string   `json:"where,omitempty"`
	Host     string   `json:"host,omitempty"`
	From     string   `json:"from,omitempty"`
	File     string   `json:"file,omitempty"`
	Command  []string `json:"command,omitempty"`
	URL      string   `json:"url,omitempty"`
	Env      []string `json:"env,omitempty"`
	Headers  []string `json:"headers,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	Required bool     `json:"required"`
	Restart  string   `json:"restart"`
	Notes    []string `json:"notes,omitempty"`
}

// mcpResolver turns mcp[] entries into proxy specs.
type mcpResolver struct {
	f       *silofile.Silofile
	environ []string
	// delivered[server][env] is a credential delivered to {mcp: server,
	// env}. In a dry run the values are placeholders.
	delivered map[string]map[string]string
	// remote is the agent host, for where: remote; nil when the location
	// has none.
	remote *remoteSide
}

// remoteSide is what resolving a where: remote entry reads from the
// agent host (design §13.1, §13.3).
type remoteSide struct {
	host string
	// environ is the remote login environment, unless err says why it
	// could not be read.
	environ []string
	err     error
	read    func(path string) ([]byte, error)
}

func newMCPResolver(f *silofile.Silofile, environ []string, vals map[string]credentials.Value) *mcpResolver {
	r := &mcpResolver{f: f, environ: environ, delivered: map[string]map[string]string{}}
	for _, c := range f.Credentials {
		for _, d := range c.To {
			if d.MCP == "" {
				continue
			}
			if r.delivered[d.MCP] == nil {
				r.delivered[d.MCP] = map[string]string{}
			}
			v := "<credential " + c.Name + ">"
			if vals != nil {
				v = vals[c.Name].Secret
			}
			r.delivered[d.MCP][d.Env] = v
		}
	}
	return r
}

func lookupEnv(environ []string, k string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if n, v, ok := strings.Cut(environ[i], "="); ok && n == k {
			return v, true
		}
	}
	return "", false
}

// resolve resolves one entry. A where: remote one is resolved against
// the agent host: its config file is read there, and its environment
// filtered from the remote login environment.
func (r *mcpResolver) resolve(m silofile.MCPServer) (proxy.Spec, MCPResolved, error) {
	res := MCPResolved{Name: m.Name, Tools: m.Tools, Required: m.Required == nil || *m.Required, Restart: m.Restart}
	if res.Restart == "" {
		res.Restart = "never"
	}
	spec := proxy.Spec{Name: m.Name, Allow: m.Tools, Required: res.Required, Restart: m.Restart == "on-failure"}
	environ := r.environ
	getenv := func(k string) string { v, _ := lookupEnv(environ, k); return v }
	opts := sources.Options{Folder: r.f.Folder, Getenv: getenv}
	if m.Where == "remote" {
		if r.remote == nil {
			return spec, res, fmt.Errorf("mcp %s: where: remote runs a server on the agent host, and location.kind %s has no host but this one: use where: local", m.Name, orLocal(r.f.Location.Kind))
		}
		if r.remote.err != nil {
			return spec, res, fmt.Errorf("mcp %s: %w", m.Name, r.remote.err)
		}
		res.Where, res.Host = "remote", r.remote.host
		environ = r.remote.environ
		// Relative paths are the remote home directory's, where remote
		// servers run: the silo folder is not on the agent host.
		opts = sources.Options{Folder: getenv("HOME"), Getenv: getenv, ReadFile: r.remote.read}
		if strings.HasPrefix(m.From, "mcp-json:") && !strings.Contains(m.From, "@") {
			return spec, res, fmt.Errorf("mcp %s: %s is where: remote, so its file is read on %s, which has no silo folder: name it with %s@<path>", m.Name, m.From, r.remote.host, m.From)
		}
	}
	if m.Tools != nil && len(m.Tools) == 0 {
		spec.Allow = []string{}
	}
	delivered := r.delivered[m.Name]
	command, url := m.Command, m.URL
	var cfgEnv, cfgHeaders map[string]string
	if m.From != "" {
		ref, err := sources.Parse(m.From)
		if err != nil {
			return spec, res, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		e, err := sources.Resolve(ref, opts)
		if err != nil {
			return spec, res, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		res.From, res.File, res.Notes = e.From, e.File, e.Notes
		command, url, cfgEnv, cfgHeaders = e.Command, e.URL, e.Env, e.Headers
		if url != "" && (m.Env != nil || m.InheritEnv.All || m.InheritEnv.Names != nil) {
			return spec, res, fmt.Errorf("mcp %s: %s is a url server, which silo does not start, so env and inherit_env do not apply", m.Name, m.From)
		}
		if url != "" && m.Restart == "on-failure" {
			return spec, res, fmt.Errorf("mcp %s: %s is a url server; silo never restarts a server it did not start", m.Name, m.From)
		}
	}
	res.Command, res.URL = command, url
	if url != "" {
		spec.URL = url
		headers := map[string]string{}
		for k, v := range m.Headers {
			headers[k] = v
		}
		for k, v := range cfgHeaders {
			val, err := r.configValue(m, environ, "header", k, v, false)
			if err != nil {
				return spec, res, err
			}
			headers[k] = val
		}
		spec.Headers = headers
		res.Headers = sortedKeys(headers)
		return spec, res, nil
	}

	inherited := map[string]bool{}
	for _, n := range m.InheritEnv.Names {
		inherited[n] = true
	}
	env := map[string]string{}
	if m.InheritEnv.All {
		for _, kv := range environ {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	for _, k := range append(append([]string(nil), minimalEnv...), m.InheritEnv.Names...) {
		if v, ok := lookupEnv(environ, k); ok {
			env[k] = v
		}
	}
	var named []string
	for k, v := range cfgEnv {
		val, err := r.configValue(m, environ, "env", k, v, true)
		if err != nil {
			return spec, res, err
		}
		env[k] = val
		named = append(named, k)
	}
	for k, v := range m.Env {
		env[k] = v
		named = append(named, k)
	}
	for k, v := range delivered {
		env[k] = v
		named = append(named, k)
	}
	sort.Strings(named)
	res.Env = dedupe(named)
	spec.Command = command
	if m.Where != "remote" {
		spec.Dir = r.f.Folder
	}
	for _, k := range sortedKeys(env) {
		spec.Env = append(spec.Env, k+"="+env[k])
	}
	return spec, res, nil
}

// configValue is the value silo uses for an env variable or header a
// config file gives (call 25). A credential delivered to {mcp: name,
// env: key} replaces it. Otherwise an inline value that looks like a
// secret is refused, and each ${VAR} it references is filled from a
// credential delivered with env: VAR or, for a launched server, from
// silo's environment (the remote login environment, for where: remote)
// when VAR is inherited.
func (r *mcpResolver) configValue(m silofile.MCPServer, environ []string, kind, key, v string, launched bool) (string, error) {
	delivered := r.delivered[m.Name]
	if c, ok := delivered[key]; ok {
		return c, nil
	}
	if sources.LooksSecret(key, v) {
		return "", fmt.Errorf("mcp %s: %s holds %s %s inline, and it looks like a secret; silo does not pass on a secret the Silofile does not name: declare it as a credential delivered to {mcp: %s, env: %s} (design §13.1, call 25)",
			m.Name, m.From, kind, key, m.Name, key)
	}
	var missing []string
	val := sources.Expand(v, func(ref string) (string, bool) {
		if c, ok := delivered[ref]; ok {
			return c, true
		}
		if launched && (m.InheritEnv.All || contains(m.InheritEnv.Names, ref)) {
			return lookupEnv(environ, ref)
		}
		missing = append(missing, ref)
		return "", false
	})
	if len(missing) > 0 {
		how := "list it in inherit_env or deliver"
		if !launched {
			how = "deliver"
		}
		return "", fmt.Errorf("mcp %s: %s's %s %s references ${%s}, which silo does not pass on: %s a credential to {mcp: %s, env: %s}",
			m.Name, m.From, kind, key, missing[0], how, m.Name, missing[0])
	}
	return val, nil
}

func orLocal(kind string) string {
	if kind == "" {
		return "local"
	}
	return kind
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mcpOptions lets tests shorten the proxy's timeouts.
var mcpOptions proxy.Options

// remoteMCPLocation is a location with an agent host of its own that
// where: remote servers run on or are reached at, over its connection
// (design §13.2): the ssh location.
type remoteMCPLocation interface {
	Host() *sshloc.Host
	LaunchMCP(name string, argv, env []string) (sshloc.RemoteServer, error)
	ForwardMCP(ctx context.Context, name, rawURL string) (*http.Client, error)
	Gone() error
}

func hasRemote(f *silofile.Silofile) bool {
	for _, m := range f.MCP {
		if m.Where == "remote" {
			return true
		}
	}
	return false
}

// remoteFrom reads what resolving where: remote entries needs of h.
func remoteFrom(ctx context.Context, h *sshloc.Host) *remoteSide {
	env, err := h.Environ(ctx)
	return &remoteSide{host: h.Name(), environ: env, err: err, read: func(p string) ([]byte, error) { return h.ReadFile(ctx, p) }}
}

// provideMCP starts the Silofile's MCP servers (design §13), local and
// on the agent host, and serves their allowed tools through the gateway.
func provideMCP(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error) {
	if len(s.File.MCP) == 0 {
		return nil, nil, nil
	}
	r := newMCPResolver(s.File, s.environ(), s.Credentials)
	rl, _ := s.Location.(remoteMCPLocation)
	if rl != nil && hasRemote(s.File) {
		r.remote = remoteFrom(ctx, rl.Host())
	}
	var specs []proxy.Spec
	lock := map[string]MCPResolved{}
	for _, m := range s.File.MCP {
		spec, res, err := r.resolve(m)
		if err != nil {
			return nil, nil, err
		}
		if m.Where == "remote" {
			if spec, err = remoteSpec(ctx, rl, spec); err != nil {
				return nil, nil, fmt.Errorf("mcp %s: %w", m.Name, err)
			}
		}
		if spec.Command != nil {
			f, err := os.OpenFile(filepath.Join(s.StateDir, "mcp-"+m.Name+".stderr"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err == nil {
				spec.Stderr = f
				defer f.Close() // the child holds its own copy
			}
		}
		specs = append(specs, spec)
		lock[m.Name] = res
	}
	if err := writeLock(s.File.Folder, "mcp", lock); err != nil {
		s.warnf("recording the resolved MCP servers in .silo/lock.json: %v", err)
	}
	opts := mcpOptions
	opts.Warn = s.warnf
	opts.OnLaunch = func(name string, pgid int) {
		_ = s.AddResource(Resource{Kind: KindProcessGroup, PGID: pgid})
	}
	p := proxy.New(opts)
	undo := func(ctx context.Context) error { return p.Close(ctx) }
	for _, m := range s.File.MCP {
		res := lock[m.Name]
		on := ""
		if res.Where == "remote" {
			on = " on " + res.Host
		}
		switch {
		case res.URL != "":
			s.status("mcp %s: connecting to %s%s", m.Name, res.URL, on)
		default:
			s.status("mcp %s: starting %s%s", m.Name, res.Command[0], on)
		}
	}
	tools, err := p.Start(ctx, specs)
	if err != nil {
		return nil, undo, err
	}
	s.onGateway(func(g *gateway.Gateway) { p.Attach(g) })
	return tools, undo, nil
}

// remoteSpec makes a resolved where: remote entry one the proxy reaches
// over the location's connection: a launched server runs as a channel
// of the master, with the local ssh as the proxy's process, and a url:
// server is dialled through a local forward. A forward that fails is the
// server not answering, which required: decides on like any other.
func remoteSpec(ctx context.Context, rl remoteMCPLocation, spec proxy.Spec) (proxy.Spec, error) {
	spec.Gone = rl.Gone
	if spec.URL != "" {
		hc, err := rl.ForwardMCP(ctx, spec.Name, spec.URL)
		if err != nil {
			url := spec.URL
			spec.URL = ""
			spec.Dial = func(context.Context) (mcp.Conn, error) {
				return nil, fmt.Errorf("forwarding to %s: %w", url, err)
			}
			return spec, nil
		}
		spec.HTTP = hc
		return spec, nil
	}
	rs, err := rl.LaunchMCP(spec.Name, spec.Command, spec.Env)
	if err != nil {
		return spec, err
	}
	// The local ssh runs with silo's own environment, as every other
	// ssh silo runs does; the server's is in the env file.
	spec.Command, spec.Env, spec.Dir = rs.Argv, os.Environ(), ""
	spec.Prelaunch, spec.Reap = rs.Prelaunch, rs.Reap
	return spec, nil
}

// checkMCP is --dry-run's check of mcp[] (design §2.4): every server
// resolvable, its from: name found, a launched server's binary present.
// For where: remote it reads the agent host on a connection of its own.
func checkMCP(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	var h *sshloc.Host
	if hasRemote(f) && f.Location.Kind == "ssh" && f.Location.SSH != nil {
		h = sshloc.DirectHost(SSHTransport(f.Location.SSH, func(k string) string { return agentcli.Getenv(environ, k) }))
	}
	return CheckMCPOn(ctx, f, environ, h)
}

// CheckMCPOn is checkMCP with the agent host h, nil for none, for
// where: remote entries. It changes nothing, here or there.
func CheckMCPOn(ctx context.Context, f *silofile.Silofile, environ []string, h *sshloc.Host) []CheckResult {
	r := newMCPResolver(f, environ, nil)
	if h != nil {
		r.remote = remoteFrom(ctx, h)
	}
	who := "the invoking user"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	var out []CheckResult
	for _, m := range f.MCP {
		c := CheckResult{Name: "mcp " + m.Name}
		spec, res, err := r.resolve(m)
		if err != nil {
			c.Detail = err.Error()
			out = append(out, c)
			continue
		}
		remote := res.Where == "remote"
		on, runner := "", who
		if remote {
			on = " on " + res.Host
			runner = "the remote user"
			if u, ok := lookupEnv(r.remote.environ, "USER"); ok && u != "" {
				runner = u
			}
			runner += " on " + res.Host
		}
		var b strings.Builder
		if res.From != "" {
			fmt.Fprintf(&b, "%s in %s%s: ", res.From, res.File, on)
		}
		if spec.URL != "" {
			fmt.Fprintf(&b, "connects to %s%s, which silo neither starts nor stops", spec.URL, on)
			if remote {
				b.WriteString(", through a local forward on the ssh connection")
			}
			if len(res.Headers) > 0 {
				fmt.Fprintf(&b, "; headers %s", strings.Join(res.Headers, ", "))
			}
			c.OK = true
		} else {
			fmt.Fprintf(&b, "launches %s%s", quoteArgv(spec.Command), on)
			if remote {
				b.WriteString(" over the ssh connection")
			}
			if len(res.Env) > 0 {
				fmt.Fprintf(&b, "; env %s", strings.Join(res.Env, ", "))
			}
			fmt.Fprintf(&b, "; runs as %s with access to their files", runner)
			path, _ := lookupEnv(spec.Env, "PATH")
			var bin string
			if remote {
				bin, err = h.LookPath(ctx, spec.Command[0], path)
			} else {
				bin, err = lookPath(spec.Command[0], path, f.Folder)
			}
			if err != nil {
				fmt.Fprintf(&b, "; %v", err)
			} else {
				fmt.Fprintf(&b, " (%s)", bin)
				c.OK = true
			}
		}
		if m.Tools != nil {
			fmt.Fprintf(&b, "; tools %s", strings.Join(m.Tools, ", "))
		} else {
			b.WriteString("; every tool it lists")
		}
		if !res.Required {
			b.WriteString("; optional")
		}
		for _, n := range res.Notes {
			fmt.Fprintf(&b, "; note: %s", n)
		}
		c.Detail = b.String()
		out = append(out, c)
	}
	return out
}

// lookPath finds a command as exec would for a server with PATH path
// running in dir.
func lookPath(name, path, dir string) (string, error) {
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", p)
		}
		return p, nil
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not found on the server's PATH (%s)", name, path)
}

// quoteArgv prints an argv for reading, quoting the words that need it.
func quoteArgv(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`;&|<>(){}*?[]#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
