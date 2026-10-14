package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/agentcli/claude"
	"github.com/bwsalmon/silo/v2/agentcli/codex"
	"github.com/bwsalmon/silo/v2/credentials"
	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/audit"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/mcp"
	"github.com/bwsalmon/silo/v2/silofile"
)

// Undo reverses one setup step. It must work against a step that only
// half happened.
type Undo func(ctx context.Context) error

// Step is one setup step of design §3. Steps run in Order; each returns
// its own Undo, and teardown runs the undos of the steps that ran in
// reverse, each independent of the others.
type Step struct {
	Name  string
	Order int
	Run   func(ctx context.Context, s *Session) (Undo, error)
}

// ToolProvider starts something the gateway fronts (design §3, step 4):
// sandboxes now; MCP servers, the local executor, the git proxy and
// cloud identities in later stages. Providers run in parallel.
type ToolProvider func(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error)

var (
	regMu     sync.Mutex
	steps     []Step
	providers = map[string]ToolProvider{}
)

// RegisterStep adds a setup step. The built-in steps are numbered as in
// design §3, times ten: credentials 20, location 30, tools 40, gateway
// 50, stage 60.
func RegisterStep(st Step) {
	regMu.Lock()
	defer regMu.Unlock()
	steps = append(steps, st)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Order < steps[j].Order })
}

// RegisterToolProvider adds a provider of gateway tools.
func RegisterToolProvider(name string, p ToolProvider) {
	regMu.Lock()
	defer regMu.Unlock()
	providers[name] = p
}

// Steps lists the registered setup steps in order.
func Steps() []Step {
	regMu.Lock()
	defer regMu.Unlock()
	return append([]Step(nil), steps...)
}

func init() {
	RegisterStep(Step{Name: "credentials", Order: 20, Run: stepCredentials})
	RegisterStep(Step{Name: "location", Order: 30, Run: stepLocation})
	RegisterStep(Step{Name: "tools", Order: 40, Run: stepTools})
	RegisterStep(Step{Name: "gateway", Order: 50, Run: stepGateway})
	RegisterStep(Step{Name: "stage", Order: 60, Run: stepStage})
}

// apiKeyVars are the variables agent.auth: {credential: x} sets
// (design §8.2).
var apiKeyVars = map[string]string{"claude": claude.APIKeyVar, "codex": codex.APIKeyVar, "agy": "GEMINI_API_KEY"}

// stepCredentials resolves every credential (step 2), and collects
// those delivered to the agent's own environment.
func stepCredentials(ctx context.Context, s *Session) (Undo, error) {
	r := s.opts.Resolver
	if r == nil {
		r = &credentials.Resolver{Folder: s.File.Folder, Getenv: s.getenv}
	}
	var resolve []silofile.Credential
	for _, c := range s.File.Credentials {
		if _, ok := s.opts.Credentials[c.Name]; !ok {
			resolve = append(resolve, c)
		}
	}
	vals, err := r.ResolveAll(ctx, resolve, nil)
	if err != nil {
		return nil, &location.PrerequisiteError{What: err.Error()}
	}
	if vals == nil {
		vals = map[string]credentials.Value{}
	}
	for name, v := range s.opts.Credentials {
		vals[name] = credentials.Value{Name: name, Secret: v, Source: "the caller"}
	}
	s.Credentials = vals
	s.em.setSecrets(s.secrets())
	for _, c := range s.File.Credentials {
		v := vals[c.Name]
		for _, d := range c.To {
			if d.Agent == "" {
				continue
			}
			if v.SilosOwn {
				return nil, &location.PrerequisiteError{What: fmt.Sprintf("credential %q is silo's own (%s) and is never delivered", c.Name, v.Source)}
			}
			s.AgentEnv[d.Name] = v.Secret
			s.warnf("credential %q is delivered to the agent's environment as %s: the agent can read it", c.Name, d.Name)
		}
	}
	if a := s.File.Agent.Auth; !a.Ambient() {
		key := apiKeyVars[s.File.Agent.CLI]
		if key == "" {
			return nil, agentcli.Refusef("agent.auth: {credential: %s} is not supported for cli %s, which has no API-key variable silo knows", a.Credential, s.File.Agent.CLI)
		}
		s.AgentEnv[key] = vals[a.Credential].Secret
	}
	return nil, nil
}

// stepLocation prepares the location (step 3) and checks the CLI's
// version.
func stepLocation(ctx context.Context, s *Session) (Undo, error) {
	ls := &location.Session{
		ID: s.ID, Dir: s.Dir, CLI: s.File.Agent.CLI, Binary: s.File.Agent.Binary,
		VersionArgs: s.Mapping.VersionArgs(), AmbientAuth: s.File.Agent.Auth.Ambient(),
		Mode: s.Mode, Environ: s.opts.Environ,
	}
	undo := func(ctx context.Context) error { return s.Location.Teardown(ctx) }
	host, err := s.Location.Prepare(ctx, ls)
	if err != nil {
		return undo, err
	}
	if host.Version != "" {
		raw := host.Version
		if host.Version, err = s.Mapping.ParseVersion(raw); err != nil {
			return undo, &location.PrerequisiteError{What: fmt.Sprintf("%s --version: %v", host.Binary, err)}
		}
	}
	s.Host = host
	if r, ok := s.Location.(interface{ RemoteGC() []string }); ok {
		for _, d := range r.RemoteGC() {
			s.status("gc: removed stale remote session directory %s", d)
		}
	}
	s.status("agent CLI: %s %s (%s)", s.File.Agent.CLI, host.Version, host.Binary)
	return undo, s.rec.update(func(r *Record) { r.CLIBinary, r.CLIVersion = host.Binary, host.Version })
}

// stepTools starts every tool provider in parallel (step 4).
func stepTools(ctx context.Context, s *Session) (Undo, error) {
	regMu.Lock()
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	regMu.Unlock()
	sort.Strings(names)
	type out struct {
		tools []gateway.Tool
		undo  Undo
		err   error
	}
	outs := make([]out, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		regMu.Lock()
		p := providers[n]
		regMu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			t, u, err := p(ctx, s)
			if err != nil {
				err = fmt.Errorf("%s: %w", n, err)
			}
			outs[i] = out{t, u, err}
		}()
	}
	wg.Wait()
	var errs []error
	for _, o := range outs {
		s.Tools = append(s.Tools, o.tools...)
		errs = append(errs, o.err)
	}
	undo := func(ctx context.Context) error {
		var errs []error
		for i := len(outs) - 1; i >= 0; i-- {
			if outs[i].undo != nil {
				errs = append(errs, outs[i].undo(ctx))
			}
		}
		return errors.Join(errs...)
	}
	return undo, errors.Join(errs...)
}

// stepGateway starts the gateway (step 5) and confirms it lists every
// tool.
func stepGateway(ctx context.Context, s *Session) (Undo, error) {
	log, err := audit.Open(filepath.Join(s.StateDir, "audit.jsonl"), s.ID, audit.Caller{CLI: s.File.Agent.CLI, Host: s.File.Location.Kind})
	if err != nil {
		return nil, err
	}
	s.Audit = log
	for name, v := range s.secrets() {
		log.Register(name, v)
	}
	log.Observe(func(r audit.Record) { s.em.emit(toolCallEvent(r)) })
	g := gateway.New(log)
	g.Register(s.Tools...)
	if len(s.opts.Rename) > 0 {
		if err := g.SetRename(s.opts.Rename); err != nil {
			log.Close()
			return nil, &Error{Code: ExitUsage, Err: err}
		}
		for from, to := range s.opts.Rename {
			s.status("gateway: %s is listed as %s", from, to)
		}
	}
	srv, err := g.Serve(context.WithoutCancel(ctx))
	undo := func(ctx context.Context) error {
		var errs []error
		if srv != nil {
			errs = append(errs, srv.Close())
		}
		return errors.Join(append(errs, log.Close())...)
	}
	if err != nil {
		return undo, err
	}
	s.Gateway, s.Server = g, srv
	s.runGatewayHooks(g)

	c := mcp.NewClient(srv.URL, srv.Token)
	if _, err := c.Initialize(ctx); err != nil {
		return undo, fmt.Errorf("gateway: %w", err)
	}
	listed, err := c.ListTools(ctx)
	if err != nil {
		return undo, fmt.Errorf("gateway: %w", err)
	}
	have := map[string]bool{}
	for _, t := range listed {
		have[t.Name] = true
		s.ToolNames = append(s.ToolNames, t.Name)
	}
	for _, t := range s.Tools {
		if to, ok := s.opts.Rename[t.Name]; ok && have[to] {
			continue
		}
		if !have[t.Name] {
			return undo, fmt.Errorf("gateway: tool %s is not listed", t.Name)
		}
	}
	for from := range s.opts.Rename {
		if !served(s.Tools, from) {
			s.warnf("gateway: the rename map names %s, which this session does not serve", from)
		}
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		return undo, err
	}
	addr, err := net.ResolveTCPAddr("tcp", u.Host)
	if err != nil {
		return undo, err
	}
	if s.GatewayURL, err = s.Location.Gateway(ctx, addr); err != nil {
		return undo, err
	}
	// A forwarded gateway is named by the forward's address on the agent
	// host (ssh: a remote loopback port).
	if gu, err := url.Parse(s.GatewayURL); err == nil && gu.Host != u.Host {
		srv.AllowHost(gu.Host)
	}
	s.status("gateway: %d tools at %s", len(listed), s.GatewayURL)
	return undo, nil
}

// stepStage writes the CLI's configuration on the agent host (step 6).
func stepStage(ctx context.Context, s *Session) (Undo, error) {
	req := agentcli.Request{
		Agent: s.File.Agent, Mode: s.Mode, Permissions: s.File.RemotePermissions,
		Prompt: s.Prompt, SystemPrompt: s.SystemPrompt,
		GatewayURL: s.GatewayURL, GatewayToken: s.Server.Token, GatewayTools: s.ToolNames,
		Host: s.Host, Env: s.AgentEnv, Deadline: s.Deadline,
	}
	for _, t := range s.Tools {
		req.Blocking = req.Blocking || t.Blocking
	}
	plan, err := s.Mapping.Build(req)
	if err != nil {
		return nil, err
	}
	if s.Mode == agentcli.Headless {
		s.parser = agentcli.NewParser(s.Mapping, req)
	}
	for _, n := range plan.Notes {
		s.warnf("%s", n)
	}
	undo := func(ctx context.Context) error { return s.Location.Teardown(ctx) }
	dir, err := s.Location.Stage(ctx, plan.Files)
	if err != nil {
		return undo, err
	}
	s.Command = plan.Command(dir)
	s.resume, s.stageDir = plan.Resume, dir
	if wd := s.File.Agent.Workdir; wd != "" {
		s.Command.Workdir = expandHome(wd, s.Host.Home)
	}
	return undo, nil
}

func served(tools []gateway.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// secrets are the values the session redacts, by the name they are
// redacted as: the credentials, and the caller MCP servers' tokens.
func (s *Session) secrets() map[string]string {
	named := map[string]string{}
	for _, v := range s.Credentials {
		named[v.Name] = v.Secret
	}
	for _, c := range s.opts.CallerMCP {
		if c.Token != "" {
			named[callerSecret(c.Name)] = c.Token
		}
	}
	return named
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// readText resolves a prompt or system prompt: inline text, or a file in
// the silo folder when it is a single line with no whitespace.
func readText(folder, s string) (string, error) {
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return s, nil
	}
	p := s
	if !filepath.IsAbs(p) {
		p = filepath.Join(folder, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
