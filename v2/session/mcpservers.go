package session

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/proxy"
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
	Name     string   `json:"name"`
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

func (r *mcpResolver) getenv(k string) (string, bool) {
	for i := len(r.environ) - 1; i >= 0; i-- {
		if n, v, ok := strings.Cut(r.environ[i], "="); ok && n == k {
			return v, true
		}
	}
	return "", false
}

// resolve resolves one entry. where: remote is stage 5b's.
func (r *mcpResolver) resolve(m silofile.MCPServer) (proxy.Spec, MCPResolved, error) {
	res := MCPResolved{Name: m.Name, Tools: m.Tools, Required: m.Required == nil || *m.Required, Restart: m.Restart}
	if res.Restart == "" {
		res.Restart = "never"
	}
	spec := proxy.Spec{Name: m.Name, Allow: m.Tools, Required: res.Required, Restart: m.Restart == "on-failure"}
	if m.Where == "remote" {
		return spec, res, fmt.Errorf("mcp %s: where: remote is not built yet; stage 5b adds it", m.Name)
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
		e, err := sources.Resolve(ref, sources.Options{Folder: r.f.Folder, Getenv: func(k string) string { v, _ := r.getenv(k); return v }})
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
			val, err := r.configValue(m, "header", k, v, false)
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
		for _, kv := range r.environ {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	for _, k := range append(append([]string(nil), minimalEnv...), m.InheritEnv.Names...) {
		if v, ok := r.getenv(k); ok {
			env[k] = v
		}
	}
	var named []string
	for k, v := range cfgEnv {
		val, err := r.configValue(m, "env", k, v, true)
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
	spec.Command, spec.Dir = command, r.f.Folder
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
// silo's environment when VAR is inherited.
func (r *mcpResolver) configValue(m silofile.MCPServer, kind, key, v string, launched bool) (string, error) {
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
			return r.getenv(ref)
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

// provideMCP starts the Silofile's local MCP servers (design §13) and
// serves their allowed tools through the gateway.
func provideMCP(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error) {
	if len(s.File.MCP) == 0 {
		return nil, nil, nil
	}
	r := newMCPResolver(s.File, s.environ(), s.Credentials)
	var specs []proxy.Spec
	lock := map[string]MCPResolved{}
	for _, m := range s.File.MCP {
		spec, res, err := r.resolve(m)
		if err != nil {
			return nil, nil, err
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
	for _, sp := range specs {
		switch {
		case sp.URL != "":
			s.status("mcp %s: connecting to %s", sp.Name, sp.URL)
		default:
			s.status("mcp %s: starting %s", sp.Name, sp.Command[0])
		}
	}
	tools, err := p.Start(ctx, specs)
	if err != nil {
		return nil, undo, err
	}
	s.onGateway(func(g *gateway.Gateway) { p.Attach(g) })
	return tools, undo, nil
}

// checkMCP is --dry-run's check of mcp[] (design §2.4): every server
// resolvable, its from: name found, a launched server's binary present.
func checkMCP(_ context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	r := newMCPResolver(f, environ, nil)
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
		var b strings.Builder
		if res.From != "" {
			fmt.Fprintf(&b, "%s in %s: ", res.From, res.File)
		}
		if spec.URL != "" {
			fmt.Fprintf(&b, "connects to %s, which silo neither starts nor stops", spec.URL)
			if len(res.Headers) > 0 {
				fmt.Fprintf(&b, "; headers %s", strings.Join(res.Headers, ", "))
			}
			c.OK = true
		} else {
			fmt.Fprintf(&b, "launches %s", quoteArgv(spec.Command))
			if len(res.Env) > 0 {
				fmt.Fprintf(&b, "; env %s", strings.Join(res.Env, ", "))
			}
			fmt.Fprintf(&b, "; runs as %s with access to their files", who)
			path := ""
			for _, kv := range spec.Env {
				if strings.HasPrefix(kv, "PATH=") {
					path = kv[5:]
				}
			}
			if bin, err := lookPath(spec.Command[0], path, f.Folder); err != nil {
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
