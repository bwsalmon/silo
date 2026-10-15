package silofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/old/gitproxy"
)

// Options changes what Validate accepts.
type Options struct {
	// Builtin allows what only silo's built-in silos may use in v2.0:
	// builtin_tools and the folder sandbox kind (design §2.3, call 10).
	Builtin bool

	// CheckScripts, when set, runs the static checks of design §12.3 and
	// §12.4 on the scripts the local executor would serve, and returns
	// errors and warnings. Stage 4a supplies it; until then it is nil and
	// the scripts are only resolved.
	CheckScripts func(f *Silofile, scripts []LocalScript) (errs, warnings Errors)

	// CheckGranulefile, when set, checks a granule sandbox's Granulefile
	// (design §14, journey 10) -- file is its absolute path, target the
	// stage to build -- and reports whether that stage builds a granule
	// VM image (true) or a container image (false). An error wrapping
	// ErrUnchecked is a warning: the check could not run here. Stage 8b
	// supplies granule.CheckGranulefile; when nil, Granulefiles are only
	// resolved.
	CheckGranulefile func(file, target string) (vm bool, err error)
}

// ErrUnchecked is what a check returns when it could not run on this
// machine (granule is not installed), which validation reports as a
// warning rather than an error.
var ErrUnchecked = errors.New("not checked")

// Result is what Validate found.
type Result struct {
	Errors   Errors
	Warnings Errors
}

// Err returns Errors as an error, or nil when there are none.
func (r Result) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	return r.Errors
}

// LocalScript is one script the local executor would serve (design
// §12.2), from local.scripts.list or local.scripts.dir.
type LocalScript struct {
	// Name is the script's file name, which credentials' {local: <name>}
	// destinations use.
	Name string
	// Tool is the gateway tool that runs it: local_<name>, lowercased,
	// with anything outside [a-z0-9_] turned into _.
	Tool string
	// Path is the resolved absolute path.
	Path string
	// Entry is its local.scripts.list entry, or nil when it came from
	// the directory only.
	Entry *Script
}

// LocalScripts lists the scripts the local executor would serve. It
// needs a loaded Silofile (Load), and returns nil when there is no local
// section.
func (f *Silofile) LocalScripts() ([]LocalScript, Errors) {
	v := &validator{f: f}
	return v.scripts(), v.res.Errors
}

// Validate checks a decoded Silofile against everything design §2.4 asks
// of silo validate, without touching anything outside the silo folder.
// Paths are checked only when the Silofile was read by Load, since a
// Silofile from Parse has no folder.
func (f *Silofile) Validate(opts Options) Result {
	v := &validator{f: f, opts: opts}
	v.top()
	v.agent()
	v.location()
	v.permissions()
	v.sandboxes()
	v.mcp()
	v.git()
	v.projects()
	scripts := v.scripts()
	v.credentials(scripts)
	if opts.CheckScripts != nil && f.Local != nil {
		errs, warns := opts.CheckScripts(f, scripts)
		v.res.Errors = append(v.res.Errors, errs...)
		v.res.Warnings = append(v.res.Warnings, warns...)
	}
	v.res.Errors.sort()
	v.res.Warnings.sort()
	return v.res
}

type validator struct {
	f    *Silofile
	opts Options
	res  Result
}

func (v *validator) add(list *Errors, path, later, format string, args ...any) {
	*list = append(*list, &Error{File: v.f.displayFile(), Path: path, Pos: v.pos(path), Msg: fmt.Sprintf(format, args...), Later: later})
}

// pos is where path was written. remote_permissions left out means none,
// so an error about it points at the CLI that cannot express it.
func (v *validator) pos(path string) Pos {
	if strings.HasPrefix(path, "remote_permissions") {
		if _, ok := v.f.pos["remote_permissions"]; !ok {
			return v.f.Pos("agent.cli")
		}
	}
	return v.f.Pos(path)
}

func (v *validator) errorf(path, format string, args ...any) {
	v.add(&v.res.Errors, path, "", format, args...)
}

func (v *validator) warnf(path, format string, args ...any) {
	v.add(&v.res.Warnings, path, "", format, args...)
}

// later refuses a value v2.0 reserves but does not ship, naming what
// adds it: "stage 8", or a design section where no stage is planned yet.
func (v *validator) later(path, value, by string) {
	v.add(&v.res.Errors, path, by, "%s is not in v2.0; %s adds it", value, laterBy(by))
}

func laterBy(by string) string {
	if strings.HasPrefix(by, "stage ") {
		return by
	}
	return "a later stage (" + by + ")"
}

func (f *Silofile) displayFile() string {
	if f.File == "" {
		return ""
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, f.File); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return f.File
}

// path checks a path in the silo folder.
func (v *validator) path(yamlPath, p string, kind fileKind) string {
	if v.f.Folder == "" {
		return ""
	}
	real, msg := resolve(v.f.Folder, p, kind)
	if msg != "" {
		v.errorf(yamlPath, "%s", msg)
	}
	return real
}

// pathOrOutside checks a path where the schema allows an absolute or
// ~/ path, which is outside the folder and not looked at here.
func (v *validator) pathOrOutside(yamlPath, p string, kind fileKind) {
	if !isOutside(p) {
		v.path(yamlPath, p, kind)
	}
}

func (v *validator) oneOf(path, value string, words ...string) bool {
	for _, w := range words {
		if value == w {
			return true
		}
	}
	v.errorf(path, "%q is not one of %s", value, strings.Join(words, ", "))
	return false
}

var (
	siloName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	sectionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// names checks that a section's names are well-formed and unique and
// returns them as a set.
func (v *validator) names(section string, n int, name func(int) string) map[string]bool {
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s[%d].name", section, i)
		nm := name(i)
		switch {
		case !sectionName.MatchString(nm):
			v.errorf(p, "%q is not a name: use [a-z0-9_-], starting with a letter or digit", nm)
		case seen[nm]:
			v.errorf(p, "%q is already the name of another %s entry", nm, section)
		}
		seen[nm] = true
	}
	return seen
}

func (v *validator) sandboxNames() map[string]bool {
	m := map[string]bool{}
	for _, s := range v.f.Sandboxes {
		m[s.Name] = true
	}
	return m
}

func (v *validator) top() {
	if !siloName.MatchString(v.f.Name) {
		v.errorf("name", "%q is not a silo name: use [a-z0-9-], starting with a letter or digit", v.f.Name)
	}
	if len(v.f.BuiltinTools) > 0 && !v.opts.Builtin {
		v.errorf("builtin_tools", "only silo's built-in silos may use builtin_tools (design §2.3)")
	}
}

// isFileRef reports whether a prompt is a file in the folder rather than
// inline text: a single line with no whitespace.
func isFileRef(s string) bool {
	return s != "" && !strings.ContainsAny(s, " \t\r\n")
}

func (v *validator) agent() {
	a := v.f.Agent
	if !v.oneOf("agent.cli", a.CLI, "claude", "codex", "agy", "custom") {
		return
	}
	switch a.Mode {
	case "", "interactive", "headless":
	case "attached":
		v.later("agent.mode", "mode attached", "design §6.2")
	default:
		v.oneOf("agent.mode", a.Mode, "interactive", "headless", "attached")
	}
	if a.Mode == "headless" && a.Prompt == "" {
		v.errorf("agent", "mode headless needs a prompt")
	}
	for _, p := range []struct{ key, val string }{{"agent.prompt", a.Prompt}, {"agent.system_prompt", a.SystemPrompt}} {
		if isFileRef(p.val) {
			v.path(p.key, p.val, regularFile)
		}
	}
	if !a.Auth.Ambient() && !v.credentialNamed(a.Auth.Credential) {
		v.errorf("agent.auth.credential", "no credential is named %q", a.Auth.Credential)
	}
	if a.CLI != "custom" {
		if a.Custom != nil {
			v.errorf("agent.custom", "custom is only for cli: custom")
		}
		return
	}
	if a.Binary == "" {
		v.errorf("agent", "cli: custom needs a binary")
	}
	if a.Custom == nil {
		v.errorf("agent", "cli: custom needs a custom: mapping (cli-mapping.md §2.4)")
		return
	}
	c := a.Custom
	if len(c.Argv.Attached) > 0 {
		v.later("agent.custom.argv.attached", "mode attached", "design §6.2")
	}
	v.oneOf("agent.custom.mcp_config.format", c.MCPConfig.Format, "claude-json", "codex-toml", "stdio-bridge")
	if (len(c.MCPConfig.Arg) > 0) == (c.MCPConfig.Env != "") {
		v.errorf("agent.custom.mcp_config", "give exactly one of arg and env")
	}
	if c.Events != "" {
		v.oneOf("agent.custom.events", c.Events, "claude-stream-json", "none")
	}
}

func (v *validator) credentialNamed(name string) bool {
	for _, c := range v.f.Credentials {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (v *validator) location() {
	l := v.f.Location
	switch l.Kind {
	case "", "local", "ssh", "k8s":
	case "vm":
		v.later("location.kind", "kind vm", "stage 10")
	case "ax", "capsule":
		v.later("location.kind", "kind "+l.Kind, "stage 12")
	default:
		v.oneOf("location.kind", l.Kind, "local", "ssh", "vm", "k8s", "ax", "capsule")
	}
	if l.Kind != "k8s" && l.K8s != nil {
		v.errorf("location.k8s", "k8s is only for kind: k8s")
	}
	if l.Kind == "k8s" {
		v.k8s()
	}
	if l.Kind != "ssh" {
		if l.SSH != nil {
			v.errorf("location.ssh", "ssh is only for kind: ssh")
		}
		return
	}
	if l.SSH == nil {
		v.errorf("location", "kind ssh needs ssh: {host: ...}")
		return
	}
	s := l.SSH
	if s.Host == "" {
		v.errorf("location.ssh.host", "empty host")
	}
	if s.Port > 65535 {
		v.errorf("location.ssh.port", "%d is not a port", s.Port)
	}
	if s.Identity != "" {
		v.pathOrOutside("location.ssh.identity", s.Identity, regularFile)
	}
	switch s.GatewayTransport {
	case "", "tcp":
	case "unix":
		v.later("location.ssh.gateway_transport", "gateway_transport unix", "design §5.3")
	default:
		v.oneOf("location.ssh.gateway_transport", s.GatewayTransport, "tcp", "unix")
	}
}

// k8sQuantity is a plain Kubernetes quantity: digits with an optional
// fraction and suffix (2, 0.5, 500m, 4Gi).
var k8sQuantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|Ki|Mi|Gi|Ti)?$`)

// k8sName is a DNS-1123 label, which namespaces are.
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (v *validator) k8s() {
	k := v.f.Location.K8s
	if k == nil {
		v.errorf("location", "kind k8s needs k8s: {image: ...}")
		return
	}
	if k.Image == "" {
		v.errorf("location.k8s.image", "empty image")
	}
	if k.Namespace != "" && !k8sName.MatchString(k.Namespace) {
		v.errorf("location.k8s.namespace", "%q is not a namespace name", k.Namespace)
	}
	if r := k.Resources; r != nil {
		if r.CPU != "" && !k8sQuantity.MatchString(r.CPU) {
			v.errorf("location.k8s.resources.cpu", "%q is not a CPU quantity (2, 500m)", r.CPU)
		}
		if r.Memory != nil && *r.Memory <= 0 {
			v.errorf("location.k8s.resources.memory", "memory must be more than 0")
		}
	}
	if k.Identity != nil {
		v.later("location.k8s.identity", "workload identity", "design §5.4")
	}
}

// permissions checks that the CLI can express remote_permissions exactly
// (design §6.1, cli-mapping.md §2).
func (v *validator) permissions() {
	const p = "remote_permissions"
	r := v.f.RemotePermissions
	mode := r.Effective()
	if mode == PermSubset {
		if len(r.Tools) == 0 {
			v.errorf(p, "an empty list: write none")
		}
		seen := map[string]bool{}
		for i, t := range r.Tools {
			if t == "" || seen[t] {
				v.errorf(fmt.Sprintf("%s[%d]", p, i), "%q is empty or repeated", t)
			}
			seen[t] = true
		}
	}
	switch v.f.Agent.CLI {
	case "codex":
		if mode == PermSubset {
			v.errorf(p, "codex cannot express a subset: it has no per-tool allowlist for its built-in tools; use none or all (cli-mapping.md §2.2)")
		}
	case "agy":
		switch mode {
		case PermNone:
			v.errorf(p, "agy cannot express none: it has no switch that withholds its built-in tools, so it runs only with all (cli-mapping.md §2.3)")
		case PermSubset:
			v.errorf(p, "agy cannot express a subset; it runs only with all (cli-mapping.md §2.3)")
		}
	case "custom":
		c := v.f.Agent.Custom
		switch {
		case mode == PermSubset:
			v.errorf(p, "a custom CLI cannot express a subset (cli-mapping.md §2.4)")
		case c == nil:
		case mode == PermNone && c.Permissions.None == nil:
			v.errorf(p, "none needs agent.custom.permissions.none: the arguments that turn the custom CLI's own tools off")
		case mode == PermAll && c.Permissions.All == nil:
			v.errorf(p, "all needs agent.custom.permissions.all")
		}
	}
}

func (v *validator) sandboxes() {
	gitNames := map[string]bool{}
	for _, g := range v.f.Git {
		gitNames[g.Name] = true
	}
	v.names("sandboxes", len(v.f.Sandboxes), func(i int) string { return v.f.Sandboxes[i].Name })
	for i, s := range v.f.Sandboxes {
		p := fmt.Sprintf("sandboxes[%d]", i)
		only := func(kind string, set bool, key string) {
			if set {
				v.errorf(p+"."+key, "%s is only for kind %s", key, kind)
			}
		}
		switch s.Kind {
		case "container", "gvisor":
			if (s.Image == "") == (s.Build == "") {
				v.errorf(p, "give exactly one of image and build")
			}
			if s.Build != "" {
				if real := v.path(p+".build", s.Build, regularFile); real != "" {
					v.dockerfile(p+".build", real, s.Kind)
				}
			}
		case "granule":
			switch {
			case s.Build != "":
				v.errorf(p+".build", "a granule sandbox is booted from a granule VM image: give image, or granulefile for a Granulefile that builds one")
			case s.Image != "" && s.Granulefile != "":
				v.errorf(p, "give exactly one of image and granulefile")
			case s.Granulefile != "":
				if real := v.path(p+".granulefile", s.Granulefile, regularFile); real != "" {
					v.granulefile(p, real, s.Target)
				}
			case s.Image == "":
				v.errorf(p, "kind granule needs image (a granule VM image) or granulefile (a Granulefile that builds one)")
			}
			if s.Granulefile == "" {
				for _, k := range []struct {
					set bool
					key string
				}{{s.Target != "", "target"}, {s.Args != nil, "args"}, {s.PushTo != "", "push_to"}} {
					if k.set {
						v.errorf(p+"."+k.key, "%s is for a sandbox built from a granulefile", k.key)
					}
				}
			}
			if s.PushTo != "" && s.Granulefile != "" {
				switch {
				case !pushRepository.MatchString(s.PushTo):
					v.errorf(p+".push_to", "%q is not a registry repository (host/path, with no tag or digest)", s.PushTo)
				case s.Backend.K8s == nil:
					v.warnf(p+".push_to", "push_to is for a remote backend (backend: {k8s: ...}): the local backend runs the image where it is built and pushes nothing")
				}
			}
			if len(s.Mounts) > 0 {
				v.errorf(p+".mounts", "kind granule takes no mounts: a host directory would have to cross into the guest over virtio-fs, which silo does not set up (design §7.2)")
			}
		case "folder":
			if !v.opts.Builtin {
				v.errorf(p+".kind", "kind folder is for silo's built-in silos only in v2.0 (design §7.1)")
			} else if s.Path == "" {
				v.errorf(p, "kind folder needs a path")
			}
		default:
			v.oneOf(p+".kind", s.Kind, "container", "gvisor", "granule", "folder")
		}
		if s.Kind != "granule" {
			only("granule", s.Granulefile != "", "granulefile")
			only("granule", s.Target != "", "target")
			only("granule", s.Args != nil, "args")
			only("granule", s.PushTo != "", "push_to")
		}
		if s.Kind != "folder" {
			only("folder", s.Path != "", "path")
			only("folder", s.RW, "rw")
		}
		if s.Network.Mode == "allow" {
			v.later(p+".network", "network {allow: [...]}", "grain.md §2.3")
		}
		for j, m := range s.Mounts {
			mp := fmt.Sprintf("%s.mounts[%d]", p, j)
			v.path(mp+".from", m.From, anyFile)
			if !strings.HasPrefix(m.To, "/") {
				v.errorf(mp+".to", "%q is not an absolute path in the sandbox", m.To)
			}
		}
		for j, r := range s.Repos {
			if !gitNames[r] {
				v.errorf(fmt.Sprintf("%s.repos[%d]", p, j), "no git entry is named %q", r)
			}
		}
		if len(s.Repos) > 0 && !strings.HasPrefix(s.Workdir, "/") {
			v.errorf(p+".workdir", "repos are checked out into workdir, which needs to be an absolute path in the sandbox")
		}
		if s.Backend.K8s != nil {
			// Journey 9: a granule VM image as a pod (design §7.3).
			if s.Kind != "granule" {
				v.errorf(p+".backend", "backend k8s runs kind granule sandboxes only, not %s", s.Kind)
			}
			if s.Granulefile != "" && s.PushTo == "" {
				v.errorf(p+".push_to", "a granulefile: sandbox with backend k8s needs push_to, a registry repository the cluster's nodes pull the built image from")
			}
			if s.Backend.K8s.KVMGroup < 0 {
				v.errorf(p+".backend.k8s.kvm_group", "%d is not a group id", s.Backend.K8s.KVMGroup)
			}
		}
		switch s.Lifetime {
		case "", "session":
		case "persistent":
			v.later(p+".lifetime", "lifetime persistent", "grain.md §2.3")
		default:
			v.oneOf(p+".lifetime", s.Lifetime, "session", "persistent")
		}
		if s.PackageCache != nil {
			v.later(p+".package_cache", "package_cache", "grain.md §2.3")
		}
		if s.ModelAccess != nil {
			v.later(p+".model_access", "model_access", "grain.md §2.5")
		}
	}
}

// mcpSources are the config sources of design §13.1, and whether each
// takes an @<arg>: required, optional or refused.
var mcpSources = map[string]string{
	"claude-user":    "refused",
	"claude-project": "required",
	"mcp-json":       "optional",
	"codex":          "refused",
	"agy":            "refused",
}

func (v *validator) mcp() {
	v.names("mcp", len(v.f.MCP), func(i int) string { return v.f.MCP[i].Name })
	for i, m := range v.f.MCP {
		p := fmt.Sprintf("mcp[%d]", i)
		if m.Where != "" {
			v.oneOf(p+".where", m.Where, "local", "remote")
		}
		n := 0
		for _, set := range []bool{m.From != "", m.Command != nil, m.URL != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			v.errorf(p, "give exactly one of from, command and url")
		}
		if m.From != "" {
			v.mcpFrom(p+".from", m)
		}
		if m.Command != nil && (len(m.Command) == 0 || m.Command[0] == "") {
			v.errorf(p+".command", "an empty command")
		}
		if m.URL != "" {
			if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
				v.errorf(p+".url", "%q is not an http(s) URL", m.URL)
			}
			if m.Env != nil || m.InheritEnv.All || m.InheritEnv.Names != nil {
				v.errorf(p, "env and inherit_env are for servers silo starts, not url servers")
			}
			if m.Restart == "on-failure" {
				v.errorf(p+".restart", "silo never restarts a server it did not start")
			}
		} else if m.Headers != nil {
			v.errorf(p+".headers", "headers are for url servers")
		}
		if m.Restart != "" {
			v.oneOf(p+".restart", m.Restart, "never", "on-failure")
		}
		seen := map[string]bool{}
		for j, t := range m.Tools {
			if t == "" || seen[t] {
				v.errorf(fmt.Sprintf("%s.tools[%d]", p, j), "%q is empty or repeated", t)
			}
			seen[t] = true
		}
	}
}

func (v *validator) mcpFrom(p string, m MCPServer) {
	src, rest, ok := strings.Cut(m.From, ":")
	at, known := mcpSources[src]
	if !ok || !known {
		v.errorf(p, "%q is not <source>:<name> with source one of claude-user, claude-project, mcp-json, codex, agy (design §13.1)", m.From)
		return
	}
	name, arg, hasArg := strings.Cut(rest, "@")
	switch {
	case name == "":
		v.errorf(p, "%q names no server", m.From)
	case at == "required" && (!hasArg || arg == ""):
		v.errorf(p, "%s needs @<dir>: %s:%s@<dir>", src, src, name)
	case at == "refused" && hasArg:
		v.errorf(p, "%s takes no @", src)
	}
	if src != "mcp-json" || m.Where == "remote" {
		return // read outside the folder, at run time
	}
	if hasArg {
		v.pathOrOutside(p, arg, regularFile)
		return
	}
	if v.f.Folder == "" {
		return
	}
	for _, def := range []string{"mcp/servers.json", ".mcp.json"} {
		if _, msg := resolve(v.f.Folder, def, regularFile); msg == "" {
			return
		}
	}
	v.errorf(p, "the silo folder has neither mcp/servers.json nor .mcp.json; name the file with %s@<path>", m.From)
}

func (v *validator) git() {
	v.names("git", len(v.f.Git), func(i int) string { return v.f.Git[i].Name })
	// A git entry with no credential: uses the one credential delivered
	// to {proxy: git}, if there is exactly one.
	var proxied []string
	for _, c := range v.f.Credentials {
		for _, d := range c.To {
			if d.Proxy == "git" {
				proxied = append(proxied, c.Name)
				break
			}
		}
	}
	for i, g := range v.f.Git {
		p := fmt.Sprintf("git[%d]", i)
		if strings.Count(g.Repo, "/") < 2 {
			v.errorf(p+".repo", "%q is not host/owner/name", g.Repo)
		}
		if g.Access != "" && !v.oneOf(p+".access", g.Access, "read", "push") {
			continue
		}
		if g.Access == "push" {
			if len(g.Push) == 0 {
				v.errorf(p, "access push needs push: the ref patterns a push may update")
			}
			for j, pat := range g.Push {
				if _, err := gitproxy.ParseRefRule(pat); err != nil {
					v.errorf(fmt.Sprintf("%s.push[%d]", p, j), "%v", err)
				}
			}
		} else {
			if g.Push != nil {
				v.errorf(p+".push", "push is only for access: push")
			}
			if g.Force {
				v.errorf(p+".force", "force is only for access: push")
			}
		}
		if g.Credential == "" {
			if len(proxied) > 1 {
				v.errorf(p+".credential", "credentials %s are all delivered to {proxy: git}: name the one this repository uses", strings.Join(proxied, ", "))
			}
			continue
		}
		var cred *Credential
		for j := range v.f.Credentials {
			if v.f.Credentials[j].Name == g.Credential {
				cred = &v.f.Credentials[j]
			}
		}
		if cred == nil {
			v.errorf(p+".credential", "no credential is named %q", g.Credential)
			continue
		}
		toGit := false
		for _, d := range cred.To {
			toGit = toGit || d.Proxy == "git"
		}
		if !toGit {
			v.errorf(p+".credential", "credential %q is not delivered to {proxy: git}", g.Credential)
		}
	}
}

// basicRoles need allow_basic_roles (design §11.2, call 19).
var basicRoles = map[string]bool{"roles/owner": true, "roles/editor": true, "roles/viewer": true}

func (v *validator) projects() {
	sandboxes := v.sandboxNames()
	seen := map[string]bool{}
	for i, pr := range v.f.Projects {
		p := fmt.Sprintf("projects[%d]", i)
		if pr.Project == "" || seen[pr.Project] {
			v.errorf(p+".project", "%q is empty or already another entry's project", pr.Project)
		}
		seen[pr.Project] = true
		if pr.Account != "" {
			v.later(p+".account", "account", "grain.md §2.6")
		} else if len(pr.Roles) == 0 {
			v.errorf(p, "roles: name the roles the account gets")
		}
		if pr.AttachTo != "" {
			v.later(p+".attach_to", "attach_to", "stage 10")
		}
		for j, r := range pr.Roles {
			if basicRoles[r] && !pr.AllowBasicRoles {
				v.errorf(fmt.Sprintf("%s.roles[%d]", p, j), "%s is a basic role: it needs allow_basic_roles: true", r)
			}
		}
		for j, s := range pr.DeliverTo {
			if !sandboxes[s] {
				v.errorf(fmt.Sprintf("%s.deliver_to[%d]", p, j), "no sandbox is named %q", s)
			}
		}
	}
}

// scripts resolves the local executor's scripts (design §12.2): the
// listed ones, and every regular executable directly in local.scripts.dir
// (default scripts) that is not a dotfile.
func (v *validator) scripts() []LocalScript {
	l := v.f.Local
	if l == nil {
		return nil
	}
	if l.Workdir != "" {
		v.path("local.workdir", l.Workdir, directory)
	}
	var out []LocalScript
	byPath := map[string]int{}
	addScript := func(path, real string, entry *Script) {
		if i, ok := byPath[real]; ok {
			if entry != nil {
				out[i].Entry = entry
			}
			return
		}
		fi, err := os.Stat(real)
		if err == nil && fi.Mode()&0o111 == 0 {
			v.errorf(path, "%q is not executable", filepath.Base(real))
		}
		name := filepath.Base(real)
		byPath[real] = len(out)
		out = append(out, LocalScript{Name: name, Tool: ToolName(name), Path: real, Entry: entry})
	}
	var dir, dirKey string
	if l.Scripts != nil {
		for i := range l.Scripts.List {
			p := fmt.Sprintf("local.scripts.list[%d].path", i)
			if real := v.path(p, l.Scripts.List[i].Path, regularFile); real != "" {
				addScript(p, real, &l.Scripts.List[i])
			}
		}
		dir, dirKey = l.Scripts.Dir, "local.scripts.dir"
	}
	if v.f.Folder == "" {
		return out
	}
	explicit := dir != ""
	if !explicit {
		dir, dirKey = "scripts", "local"
	}
	if explicit || exists(filepath.Join(v.f.Folder, dir)) {
		if real := v.path(dirKey, dir, directory); real != "" {
			entries, err := os.ReadDir(real)
			if err != nil {
				v.errorf(dirKey, "%v", err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") {
					continue
				}
				rel := filepath.Join(dir, e.Name())
				target, msg := resolve(v.f.Folder, rel, anyFile)
				if msg != "" {
					v.errorf(dirKey, "%s", msg)
					continue
				}
				fi, err := os.Stat(target)
				if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
					continue
				}
				addScript(dirKey, target, nil)
			}
		}
	}
	if len(out) == 0 {
		v.errorf("local", "the local executor has no scripts: list them, or put executables in %s", dir)
	}
	tools := map[string]string{}
	for _, s := range out {
		if other, ok := tools[s.Tool]; ok {
			v.errorf("local", "scripts %q and %q both become the tool %s", other, s.Name, s.Tool)
		}
		tools[s.Tool] = s.Name
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// ToolName is the gateway tool name of a local script (design §12.3):
// local_ and the file name, lowercased, with anything outside [a-z0-9_]
// turned into _.
func ToolName(file string) string {
	b := []byte(strings.ToLower(file))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			b[i] = '_'
		}
	}
	return "local_" + string(b)
}

func (v *validator) credentials(scripts []LocalScript) {
	v.names("credentials", len(v.f.Credentials), func(i int) string { return v.f.Credentials[i].Name })
	sandboxes := v.sandboxNames()
	servers := map[string]*MCPServer{}
	for i := range v.f.MCP {
		servers[v.f.MCP[i].Name] = &v.f.MCP[i]
	}
	scriptNames := map[string]bool{}
	for _, s := range scripts {
		scriptNames[s.Name] = true
	}
	for i, c := range v.f.Credentials {
		p := fmt.Sprintf("credentials[%d]", i)
		src := c.From
		n := 0
		for _, s := range []string{src.Env, src.File, src.Store, src.Ambient} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			v.errorf(p+".from", "give exactly one of env, file, store and ambient")
		}
		if src.Ambient != "" && v.oneOf(p+".from.ambient", src.Ambient, "gh", "git-credential", "gcloud") && src.Ambient == "gcloud" {
			v.errorf(p+".from.ambient", "{ambient: gcloud} is silo's own, to set up projects[] (design §8.1): it is never delivered anywhere, so it is not declared as a credential")
			continue
		}
		if src.File != "" {
			v.pathOrOutside(p+".from.file", src.File, regularFile)
		}
		if len(c.To) == 0 && v.f.Agent.Auth.Credential != c.Name {
			v.errorf(p+".to", "credential %q is delivered nowhere: give it at least one destination", c.Name)
		}
		for j, d := range c.To {
			v.destination(fmt.Sprintf("%s.to[%d]", p, j), c.Name, d, sandboxes, servers, scriptNames)
		}
	}
}

func (v *validator) destination(p, cred string, d Destination, sandboxes map[string]bool, servers map[string]*MCPServer, scripts map[string]bool) {
	set := map[string]bool{}
	for k, val := range map[string]string{"sandbox": d.Sandbox, "mcp": d.MCP, "local": d.Local, "agent": d.Agent, "proxy": d.Proxy, "env": d.Env, "file": d.File, "name": d.Name} {
		if val != "" {
			set[k] = true
		}
	}
	allowed := func(keys ...string) bool {
		ok := map[string]bool{}
		for _, k := range keys {
			ok[k] = true
		}
		for k := range set {
			if !ok[k] {
				v.errorf(p, "%s does not go with %s (design §8.2)", k, keys[0])
				return false
			}
		}
		return true
	}
	switch {
	case d.Sandbox != "":
		if !allowed("sandbox", "env", "file") {
			return
		}
		if set["env"] == set["file"] {
			v.errorf(p, "give exactly one of env and file")
		}
		if d.File != "" && !strings.HasPrefix(d.File, "/") {
			v.errorf(p+".file", "%q is not an absolute path in the sandbox", d.File)
		}
		if !sandboxes[d.Sandbox] {
			v.errorf(p+".sandbox", "no sandbox is named %q", d.Sandbox)
		}
	case d.MCP != "":
		if !allowed("mcp", "env") {
			return
		}
		if d.Env == "" {
			v.errorf(p, "{mcp: ...} needs env")
		}
		switch s := servers[d.MCP]; {
		case s == nil:
			v.errorf(p+".mcp", "no MCP server is named %q", d.MCP)
		case s.URL != "":
			v.errorf(p+".mcp", "MCP server %q is a url server, which silo does not start, so it cannot be given an environment", d.MCP)
		}
	case d.Local != "":
		if !allowed("local", "env") {
			return
		}
		if d.Env == "" {
			v.errorf(p, "{local: ...} needs env")
		}
		if v.f.Local == nil {
			v.errorf(p+".local", "there is no local: section")
		} else if v.f.Folder != "" && !scripts[d.Local] {
			v.errorf(p+".local", "no local script is named %q", d.Local)
		}
	case d.Agent != "":
		if !allowed("agent", "name") {
			return
		}
		if d.Agent != "env" {
			v.errorf(p+".agent", "want agent: env")
		}
		if d.Name == "" {
			v.errorf(p, "{agent: env} needs name")
		}
		v.warnf(p, "credential %q is delivered to the agent's own environment, where the agent can read it", cred)
	case d.Proxy != "":
		if !allowed("proxy") {
			return
		}
		switch d.Proxy {
		case "git":
		case "model":
			v.later(p+".proxy", "proxy model", "grain.md §2.5")
		default:
			v.oneOf(p+".proxy", d.Proxy, "git", "model")
		}
	default:
		v.errorf(p, "name a destination: sandbox, mcp, local, agent or proxy (design §8.2)")
	}
}
