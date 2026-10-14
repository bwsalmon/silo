// Package sources reads MCP server definitions from the config files the
// agent CLIs already keep (design §13.1, call 24), for a Silofile's
// `from: <source>:<name>` entries:
//
//	claude-user     ~/.claude.json                 mcpServers.<name>
//	claude-project  ~/.claude.json                 projects["<dir>"].mcpServers.<name>, from claude-project:<name>@<dir>
//	mcp-json        a .mcp.json file               mcpServers.<name>, from mcp-json:<name>[@<path>]
//	codex           $CODEX_HOME/config.toml        [mcp_servers.<name>]
//	agy             ~/.gemini/config/mcp_config.json  mcpServers.<name>
//
// Every source is named: there is no search for a bare name. A resolved
// Entry is a command or a url, with the config's env and headers, and
// says which of their values look like inline secrets, which silo
// refuses unless the Silofile re-declares them (call 25).
//
// The file formats follow grain's writers of the same files
// (github.com/bwsalmon/grain pkg/agent/claude, pkg/agent/codex and
// pkg/agent/antigravity at c950ca085) and the CLIs' own documentation.
package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The source names.
const (
	ClaudeUser    = "claude-user"
	ClaudeProject = "claude-project"
	MCPJSON       = "mcp-json"
	Codex         = "codex"
	Agy           = "agy"
)

// Names lists the sources, in the order design §13.1 gives them.
var Names = []string{ClaudeUser, ClaudeProject, MCPJSON, Codex, Agy}

// DefaultMCPJSON are the files mcp-json reads, in the silo folder, when
// no @<path> is given.
var DefaultMCPJSON = []string{"mcp/servers.json", ".mcp.json"}

// Ref is a parsed from: value.
type Ref struct {
	Source string
	Name   string
	// Arg is what follows @: claude-project's directory, mcp-json's path.
	Arg string
}

func (r Ref) String() string {
	if r.Arg != "" {
		return r.Source + ":" + r.Name + "@" + r.Arg
	}
	return r.Source + ":" + r.Name
}

// Parse parses <source>:<name>[@<arg>]. A bare name, with no source, is
// refused: silo never searches every config for one.
func Parse(from string) (Ref, error) {
	src, rest, ok := strings.Cut(from, ":")
	if !ok {
		return Ref{}, fmt.Errorf("%q names no source: write <source>:%s with source one of %s; silo does not search every config for a bare name (design §13.1)",
			from, from, strings.Join(Names, ", "))
	}
	known := false
	for _, n := range Names {
		known = known || n == src
	}
	if !known {
		return Ref{}, fmt.Errorf("%q: unknown source %q; one of %s", from, src, strings.Join(Names, ", "))
	}
	name, arg, hasArg := strings.Cut(rest, "@")
	if name == "" {
		return Ref{}, fmt.Errorf("%q names no server", from)
	}
	switch {
	case src == ClaudeProject && arg == "":
		return Ref{}, fmt.Errorf("%q: claude-project needs the project's directory: %s:%s@<dir>", from, src, name)
	case src != ClaudeProject && src != MCPJSON && hasArg:
		return Ref{}, fmt.Errorf("%q: %s takes no @", from, src)
	case hasArg && arg == "":
		return Ref{}, fmt.Errorf("%q: an empty @", from)
	}
	return Ref{Source: src, Name: name, Arg: arg}, nil
}

// Options say where the files are.
type Options struct {
	// Folder is the silo folder: where mcp-json's default and relative
	// paths, and claude-project's relative directories, are found.
	Folder string
	// Getenv reads HOME and CODEX_HOME; os.Getenv when nil.
	Getenv func(string) string
}

func (o Options) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o Options) home() (string, error) {
	if h := o.getenv("HOME"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// path resolves p: ~/ against home, a relative path against the folder.
func (o Options) path(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := o.home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, strings.TrimPrefix(p, "~")), nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(o.Folder, p)
	}
	return filepath.Clean(p), nil
}

// Entry is a resolved server definition.
type Entry struct {
	// From is the reference it was resolved from, and File the file read.
	From string `json:"from"`
	File string `json:"file"`
	// Command is the argv of a server to launch; URL that of one to
	// connect to. Exactly one is set.
	Command []string `json:"command,omitempty"`
	URL     string   `json:"url,omitempty"`
	// Env and Headers are as the config gives them. A value may be a
	// ${VAR} reference to the environment the CLI would have expanded.
	Env     map[string]string `json:"-"`
	Headers map[string]string `json:"-"`
	// Notes are config settings silo does not apply, and why.
	Notes []string `json:"notes,omitempty"`
}

// EnvNames and HeaderNames list the keys, sorted, for printing and the
// lock file, which never hold values.
func (e *Entry) EnvNames() []string    { return keys(e.Env) }
func (e *Entry) HeaderNames() []string { return keys(e.Headers) }

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrNotFound is wrapped by Resolve's error when the file exists but
// does not define the name.
var ErrNotFound = errors.New("not defined")

// Resolve reads the server ref names.
func Resolve(ref Ref, o Options) (*Entry, error) {
	var files []string
	switch ref.Source {
	case ClaudeUser, ClaudeProject:
		h, err := o.home()
		if err != nil {
			return nil, err
		}
		files = []string{filepath.Join(h, ".claude.json")}
	case MCPJSON:
		if ref.Arg != "" {
			p, err := o.path(ref.Arg)
			if err != nil {
				return nil, err
			}
			files = []string{p}
		} else {
			for _, d := range DefaultMCPJSON {
				files = append(files, filepath.Join(o.Folder, d))
			}
		}
	case Codex:
		dir := o.getenv("CODEX_HOME")
		if dir == "" {
			h, err := o.home()
			if err != nil {
				return nil, err
			}
			dir = filepath.Join(h, ".codex")
		}
		files = []string{filepath.Join(dir, "config.toml")}
	case Agy:
		h, err := o.home()
		if err != nil {
			return nil, err
		}
		files = []string{filepath.Join(h, ".gemini", "config", "mcp_config.json")}
	default:
		return nil, fmt.Errorf("unknown source %q", ref.Source)
	}
	file := ""
	var data []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) && len(files) > 1 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		file, data = f, b
		break
	}
	if file == "" {
		return nil, fmt.Errorf("%s: the silo folder has neither %s; name the file with %s@<path>", ref, strings.Join(DefaultMCPJSON, " nor "), ref)
	}
	var e *Entry
	var err error
	if ref.Source == Codex {
		e, err = readCodex(data, ref.Name)
	} else {
		e, err = readJSON(data, ref, o)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", ref, file, err)
	}
	e.From, e.File = ref.String(), file
	return e, nil
}

// jsonServer is one mcpServers entry: Claude Code's shape (type,
// command, args, env, url, headers), with Gemini CLI's and agy's url
// keys (httpUrl, serverUrl) and cwd.
type jsonServer struct {
	Type      string            `json:"type"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	HTTPURL   string            `json:"httpUrl"`
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers"`
	Cwd       string            `json:"cwd"`
	Disabled  bool              `json:"disabled"`
}

func readJSON(data []byte, ref Ref, o Options) (*Entry, error) {
	type servers struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	var doc struct {
		servers
		Projects map[string]servers `json:"projects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	m := doc.MCPServers
	where := "mcpServers"
	if ref.Source == ClaudeProject {
		dir, err := o.path(ref.Arg)
		if err != nil {
			return nil, err
		}
		p, ok := doc.Projects[dir]
		if !ok {
			return nil, fmt.Errorf("no project %q in projects (%w)", dir, ErrNotFound)
		}
		m, where = p.MCPServers, fmt.Sprintf("projects[%q].mcpServers", dir)
	}
	raw, ok := m[ref.Name]
	if !ok {
		return nil, fmt.Errorf("%s.%s: %w (it defines %s)", where, ref.Name, ErrNotFound, list(m))
	}
	var s jsonServer
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", where, ref.Name, err)
	}
	e := &Entry{Env: s.Env, Headers: s.Headers}
	url := s.URL
	for _, u := range []string{s.HTTPURL, s.ServerURL} {
		if url == "" {
			url = u
		}
	}
	switch {
	case s.Command != "" && url != "":
		return nil, fmt.Errorf("%s.%s has both a command and a url", where, ref.Name)
	case s.Command != "":
		e.Command = append([]string{s.Command}, s.Args...)
	case url != "":
		e.URL = url
	default:
		return nil, fmt.Errorf("%s.%s has neither a command nor a url", where, ref.Name)
	}
	if s.Cwd != "" {
		e.Notes = append(e.Notes, fmt.Sprintf("cwd %q is not used: silo runs a server in the silo folder (design §13.3)", s.Cwd))
	}
	if s.Disabled {
		e.Notes = append(e.Notes, "the config marks it disabled; the Silofile naming it runs it anyway")
	}
	return e, nil
}

// codexServer is one [mcp_servers.<name>] table of codex's config.toml.
type codexServer struct {
	Command           string            `toml:"command"`
	Args              []string          `toml:"args"`
	Env               map[string]string `toml:"env"`
	EnvVars           []string          `toml:"env_vars"`
	Cwd               string            `toml:"cwd"`
	URL               string            `toml:"url"`
	HTTPHeaders       map[string]string `toml:"http_headers"`
	EnvHTTPHeaders    map[string]string `toml:"env_http_headers"`
	BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
	Enabled           *bool             `toml:"enabled"`
}

func readCodex(data []byte, name string) (*Entry, error) {
	var doc struct {
		MCPServers map[string]toml.Primitive `toml:"mcp_servers"`
	}
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, err
	}
	prim, ok := doc.MCPServers[name]
	if !ok {
		names := map[string]json.RawMessage{}
		for n := range doc.MCPServers {
			names[n] = nil
		}
		return nil, fmt.Errorf("[mcp_servers.%s]: %w (it defines %s)", name, ErrNotFound, list(names))
	}
	var s codexServer
	if err := md.PrimitiveDecode(prim, &s); err != nil {
		return nil, fmt.Errorf("[mcp_servers.%s]: %w", name, err)
	}
	e := &Entry{Env: s.Env, Headers: s.HTTPHeaders}
	switch {
	case s.Command != "" && s.URL != "":
		return nil, fmt.Errorf("[mcp_servers.%s] has both a command and a url", name)
	case s.Command != "":
		e.Command = append([]string{s.Command}, s.Args...)
	case s.URL != "":
		e.URL = s.URL
	default:
		return nil, fmt.Errorf("[mcp_servers.%s] has neither a command nor a url", name)
	}
	// Headers codex fills from its environment are references, as a
	// ${VAR} in a JSON config is.
	if len(s.EnvHTTPHeaders) > 0 || s.BearerTokenEnvVar != "" {
		if e.Headers == nil {
			e.Headers = map[string]string{}
		}
		for h, v := range s.EnvHTTPHeaders {
			e.Headers[h] = "${" + v + "}"
		}
		if s.BearerTokenEnvVar != "" {
			e.Headers["Authorization"] = "Bearer ${" + s.BearerTokenEnvVar + "}"
		}
	}
	if len(s.EnvVars) > 0 {
		e.Notes = append(e.Notes, fmt.Sprintf("env_vars %s are not passed through: list them in inherit_env (design §13.3)", strings.Join(s.EnvVars, ", ")))
	}
	if s.Cwd != "" {
		e.Notes = append(e.Notes, fmt.Sprintf("cwd %q is not used: silo runs a server in the silo folder (design §13.3)", s.Cwd))
	}
	if s.Enabled != nil && !*s.Enabled {
		e.Notes = append(e.Notes, "the config marks it disabled; the Silofile naming it runs it anyway")
	}
	return e, nil
}

func list(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return "no servers"
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// refRE matches a ${VAR} or ${VAR:-default} reference.
var refRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// Refs returns the variables v references with ${VAR}.
func Refs(v string) []string {
	var out []string
	for _, m := range refRE.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1])
	}
	return out
}

// Expand replaces each ${VAR} in v with lookup(VAR), or the reference's
// default when lookup has none.
func Expand(v string, lookup func(string) (string, bool)) string {
	return refRE.ReplaceAllStringFunc(v, func(m string) string {
		sub := refRE.FindStringSubmatch(m)
		if val, ok := lookup(sub[1]); ok {
			return val
		}
		return strings.TrimPrefix(sub[2], ":-")
	})
}

// secretName matches variable and header names that hold secrets.
var secretName = regexp.MustCompile(`(?i)(token|secret|passw(or)?d|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credential|auth|cookie|^pat$|_pat$|^pat_)`)

// secretValue matches values that are recognisably secrets, whatever
// they are called.
var secretValue = regexp.MustCompile(`^(Bearer |Basic |gh[opsur]_|github_pat_|glpat-|sk-|xox[abprs]-|AKIA|ASIA|AIza|ya29\.|npm_|-----BEGIN)`)

// LooksSecret reports whether an env variable or header named name
// with inline value v looks like a secret (call 25). A value made of
// ${VAR} references, with at most a "Bearer " before them, holds no
// secret itself: the secret is wherever the reference points.
// (A URL carrying a password counts, as does any value whose name says
// it is a token, key, password or credential.)
func LooksSecret(name, v string) bool {
	lit := strings.TrimSpace(refRE.ReplaceAllString(v, ""))
	if refRE.MatchString(v) {
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(lit, "Bearer"), "Basic"))
		return rest != "" && secretValue.MatchString(rest)
	}
	return lit != "" && (secretName.MatchString(name) || secretValue.MatchString(lit) || userinfo.MatchString(lit))
}

// userinfo matches a URL with a password in it: postgres://u:pw@host.
var userinfo = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@`)
