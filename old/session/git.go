package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/old/credentials"
	"github.com/bwsalmon/silo/old/gateway/audit"
	"github.com/bwsalmon/silo/old/gitproxy"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/sandbox/container"
	"github.com/bwsalmon/silo/old/sandbox/granule"
	k8ssb "github.com/bwsalmon/silo/old/sandbox/k8s"
	"github.com/bwsalmon/silo/old/silofile"
)

// The session's git proxy (design §9): it starts before the sandboxes,
// on the invoking machine, listening only on the session's private
// network (sandbox.Networker), with a token for each sandbox that has
// repos. Each of those sandboxes joins the network and has its repos
// cloned through the proxy, so the credential stays in silo.

func init() {
	RegisterStep(Step{Name: "git proxy", Order: 35, Run: stepGitProxy})
	RegisterCheck("git", checkGit)
}

// gitSession is a running git proxy.
type gitSession struct {
	// base is the proxy's URL as a sandbox reaches it,
	// http://<network host IP>:<port>.
	base    string
	network string
	// addr is the proxy's address on this machine, host:port, which a
	// backend that carries the proxy itself (k8s) dials.
	addr string
	// tokens is each sandbox's token, by sandbox name.
	tokens map[string]string
}

// remoteURL is repo's URL through the proxy, carrying sandbox's token:
// the origin a checkout is made with. A later session rewrites it with
// its own (git remote set-url), which is how a persistent workspace
// would be re-attached (grain.md §2.3).
func (g *gitSession) remoteURL(sb sandbox.Sandbox, sandboxName, repo string) string {
	base := g.base
	if p, ok := sb.(sandbox.GitProxyURLer); ok {
		base = p.GitProxyURL()
	}
	u, _ := url.Parse(base + "/" + repo + ".git")
	u.User = url.UserPassword("silo", g.tokens[sandboxName])
	return u.String()
}

// usesGit reports whether any sandbox checks out a repo.
func usesGit(f *silofile.Silofile) bool {
	for _, sc := range f.Sandboxes {
		if len(sc.Repos) > 0 {
			return true
		}
	}
	return false
}

// gitCredential is the credential a git entry's requests go out with:
// its own credential:, else the Silofile's one credential delivered to
// {proxy: git}, else none (validation refuses an entry that leaves
// several to choose from).
func gitCredential(f *silofile.Silofile, g silofile.GitRepo) *silofile.Credential {
	for i, c := range f.Credentials {
		if g.Credential != "" {
			if c.Name == g.Credential {
				return &f.Credentials[i]
			}
			continue
		}
		for _, d := range c.To {
			if d.Proxy == "git" {
				return &f.Credentials[i]
			}
		}
	}
	return nil
}

// proxyOnlyAmbient reports whether c is an ambient credential delivered
// only to {proxy: git}: one resolved for the git hosts that use it,
// rather than for credentials.DefaultHost.
func proxyOnlyAmbient(c silofile.Credential) bool {
	if c.From.Ambient == "" || len(c.To) == 0 {
		return false
	}
	for _, d := range c.To {
		if d.Proxy != "git" {
			return false
		}
	}
	return true
}

// gitHosts is every git host each credential is used for.
func gitHosts(f *silofile.Silofile) map[string][]string {
	out := map[string][]string{}
	for _, g := range f.Git {
		c := gitCredential(f, g)
		if c == nil {
			continue
		}
		repo, err := gitproxy.ParseRepo(g.Repo)
		if err != nil {
			continue
		}
		hosts := out[c.Name]
		dup := false
		for _, h := range hosts {
			dup = dup || h == repo.Host
		}
		if !dup {
			out[c.Name] = append(hosts, repo.Host)
		}
	}
	return out
}

// gitPolicy builds the proxy's policy from the Silofile's git entries,
// with each entry's credential asked for through source.
func gitPolicy(f *silofile.Silofile, source func(c silofile.Credential, host string) *gitproxy.CredentialSource) (*gitproxy.StaticPolicy, error) {
	cfg := gitproxy.StaticConfig{Source: "the Silofile's git: entries"}
	if f.File != "" {
		cfg.Source = "the git: entries of " + f.File
	}
	for _, g := range f.Git {
		r := gitproxy.StaticRepo{Repo: g.Repo, Access: gitproxy.AccessRead, Force: g.Force}
		if g.Access == "push" {
			r.Access = gitproxy.AccessPush
		}
		for _, p := range g.Push {
			rule, err := gitproxy.ParseRefRule(p)
			if err != nil {
				return nil, fmt.Errorf("git entry %s: %w", g.Name, err)
			}
			r.Push = append(r.Push, rule)
		}
		if c := gitCredential(f, g); c != nil {
			repo, err := gitproxy.ParseRepo(g.Repo)
			if err != nil {
				return nil, fmt.Errorf("git entry %s: %w", g.Name, err)
			}
			r.Credential = source(*c, repo.Host)
		}
		cfg.Repos = append(cfg.Repos, r)
	}
	return gitproxy.NewStaticPolicy(cfg)
}

// gitCredentialSource asks for c's secret on every request: a value the
// caller passed, or the resolver's (which caches an ambient login's
// token for credentials.AmbientTTL). A value not seen before is
// registered with the audit log, for redaction.
func (s *Session) gitCredentialSource(c silofile.Credential, host string) *gitproxy.CredentialSource {
	var mu sync.Mutex
	last := ""
	return &gitproxy.CredentialSource{
		Name: c.Name,
		Token: func(ctx context.Context) (string, error) {
			if v, ok := s.opts.Credentials[c.Name]; ok {
				return v, nil
			}
			v, err := s.resolver.ResolveHost(ctx, c, host)
			if err != nil {
				return "", err
			}
			mu.Lock()
			if v.Secret != last && s.Audit != nil {
				s.Audit.Register(c.Name, v.Secret)
				last = v.Secret
			}
			mu.Unlock()
			return v.Secret, nil
		},
		Refused: func() { s.resolver.Forget(c, host) },
	}
}

// gitAudit writes the proxy's entries to the session's audit log.
type gitAudit struct{ log *audit.Log }

func (a gitAudit) Record(e gitproxy.AuditEntry) { _ = a.log.Git(e) }

// stepGitProxy starts the git proxy (design §3, step 4), before the
// sandboxes that clone through it.
func stepGitProxy(ctx context.Context, s *Session) (Undo, error) {
	if !usesGit(s.File) {
		for _, g := range s.File.Git {
			s.warnf("git entry %s is in no sandbox's repos, so this session does not reach it", g.Name)
		}
		return nil, nil
	}
	b, err := s.backend()
	if err != nil {
		return nil, err
	}
	policy, err := gitPolicy(s.File, s.gitCredentialSource)
	if err != nil {
		return nil, err
	}
	// A sandbox on a cluster reaches the proxy through its pod's tunnel
	// (design §9.3), which silo dials from this machine, so a session
	// whose only sandboxes with repos are there needs no network, and
	// the proxy listens on loopback.
	local := false
	for _, sc := range s.File.Sandboxes {
		local = local || (len(sc.Repos) > 0 && sc.Backend.K8s == nil)
	}
	var srv *gitproxy.Server
	var nw sandbox.Networker
	network := sandbox.Network{HostIP: "127.0.0.1"}
	undo := func(ctx context.Context) error {
		var errs []error
		if srv != nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			errs = append(errs, srv.Close(cctx))
			cancel()
		}
		if nw != nil {
			errs = append(errs, nw.RemoveNetwork(ctx, network.Name))
		}
		return errors.Join(errs...)
	}
	if local {
		var ok bool
		if nw, ok = b.(sandbox.Networker); !ok {
			return nil, fmt.Errorf("git proxy: the sandbox backend cannot give sandboxes a private network to reach the proxy on")
		}
		network, err = nw.CreateNetwork(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("git proxy: %w", err)
		}
		if err := s.AddResource(Resource{Kind: KindNetwork, Runtime: s.runtime(), Network: network.Name}); err != nil {
			return undo, err
		}
	}
	tokens := &gitproxy.TokenSet{}
	g := &gitSession{network: network.Name, tokens: map[string]string{}}
	for _, sc := range s.File.Sandboxes {
		if len(sc.Repos) == 0 {
			continue
		}
		tok, err := tokens.Mint(gitproxy.Caller{ID: sc.Name, Audit: map[string]string{"session": s.ID, "sandbox": sc.Name}})
		if err != nil {
			return undo, err
		}
		g.tokens[sc.Name] = tok
	}
	proxy := &gitproxy.GitProxy{
		Policy: policy, Tokens: tokens, Audit: gitAudit{s.Audit},
		Hosts: s.opts.GitHosts, ForwarderFor: s.opts.GitForwarderFor,
	}
	srv, err = gitproxy.Serve(proxy, net.JoinHostPort(network.HostIP, "0"))
	if err != nil {
		return undo, fmt.Errorf("git proxy: cannot listen on %s, the session network's address on this machine (%v); "+
			"a rootless Podman network's gateway is not on the host: use Docker or rootful Podman for sandboxes with repos", network.HostIP, err)
	}
	g.base = srv.URL
	g.addr = strings.TrimPrefix(srv.URL, "http://")
	s.git = g
	if network.Name != "" {
		s.status("git proxy: %d repositories for %d sandboxes, on network %s at %s", len(s.File.Git), len(g.tokens), network.Name, srv.URL)
	} else {
		s.status("git proxy: %d repositories for %d sandboxes, at %s", len(s.File.Git), len(g.tokens), srv.URL)
	}
	return undo, nil
}

// checkout clones a sandbox's repos into its workdir through the proxy:
// the one repo at workdir itself, several each at workdir/<name>.
func (s *Session) checkout(ctx context.Context, sb sandbox.Sandbox, sc silofile.Sandbox) error {
	for _, name := range sc.Repos {
		var entry silofile.GitRepo
		for _, g := range s.File.Git {
			if g.Name == name {
				entry = g
			}
		}
		repo, err := gitproxy.ParseRepo(entry.Repo)
		if err != nil {
			return err
		}
		dir := sc.Workdir
		if len(sc.Repos) > 1 {
			dir = path.Join(sc.Workdir, name)
		}
		s.status("sandbox %s: cloning %s into %s", sc.Name, repo, dir)
		res, err := sb.Exec(ctx, sandbox.ExecRequest{
			Argv: []string{"git", "clone", "--quiet", "--", s.git.remoteURL(sb, sc.Name, repo.String()), dir},
			Cwd:  "/",
			Env:  map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		})
		if err != nil {
			return fmt.Errorf("cloning %s: %w", repo, err)
		}
		if res.ExitCode != 0 {
			out := strings.TrimSpace(string(res.Stderr) + " " + string(res.Stdout))
			if res.ExitCode == 127 {
				return fmt.Errorf("cloning %s: the image has no git, which repos need (%s)", repo, out)
			}
			return fmt.Errorf("cloning %s into %s: exit %d: %s", repo, dir, res.ExitCode, out)
		}
	}
	return nil
}

// backend is the sandbox backend: Options.Backend, or the container
// backend, made once.
func (s *Session) backend() (sandbox.Backend, error) {
	s.backendMu.Lock()
	defer s.backendMu.Unlock()
	if s.sandboxes != nil {
		return s.sandboxes, nil
	}
	if s.opts.Backend != nil {
		s.sandboxes = s.opts.Backend
		return s.sandboxes, nil
	}
	b, err := newBackend(s.File)
	if err != nil {
		return nil, err
	}
	s.sandboxes = b
	return b, nil
}

// newBackend is the backend for f's sandboxes: the container backend,
// and with granule sandboxes the granule backend beside it, on the
// Docker granule runs its VMs on, so that the two share the session's
// network; and with sandboxes on a cluster (backend: {k8s: ...}), the
// k8s backend, which needs neither.
func newBackend(f *silofile.Silofile) (sandbox.Backend, error) {
	usesGranule, local, builds := false, false, false
	var specs []sandbox.SandboxSpec
	for _, sc := range f.Sandboxes {
		if sc.Backend.K8s != nil {
			builds = builds || sc.Granulefile != ""
			specs = append(specs, sandbox.SandboxSpec{K8s: &sandbox.K8sTarget{Context: sc.Backend.K8s.Context, Namespace: sc.Backend.K8s.Namespace}})
			continue
		}
		local = true
		usesGranule = usesGranule || sc.Kind == "granule"
	}
	var kb sandbox.Backend
	if len(specs) > 0 {
		k := &k8ssb.Backend{Targets: k8ssb.TargetsOf(specs)}
		if builds {
			gb, err := granule.New()
			if err != nil {
				return nil, err
			}
			k.Builder = gb
		}
		kb = k
		if !local {
			return &sandbox.Kinds{K8s: kb}, nil
		}
	}
	if !usesGranule {
		b, err := container.New()
		if err != nil {
			return nil, err
		}
		if kb != nil {
			return &sandbox.Kinds{Default: b, K8s: kb}, nil
		}
		return b, nil
	}
	gb, err := granule.New()
	if err != nil {
		return nil, err
	}
	return &sandbox.Kinds{
		Default: &container.Backend{Runtime: gb.Docker},
		ByKind:  map[string]sandbox.Backend{"granule": gb},
		K8s:     kb,
	}, nil
}

// runtime is the container runtime's name, for session.json.
func (s *Session) runtime() string {
	b := s.sandboxes
	if k, ok := b.(*sandbox.Kinds); ok {
		b = k.Default
	}
	if cb, ok := b.(*container.Backend); ok {
		return filepath.Base(cb.Runtime)
	}
	return ""
}

// checkGit is silo run --dry-run's check of each git entry (design
// §2.4): its credential resolves, and the repository answers through a
// proxy holding it -- an info/refs for git-upload-pack, and for push
// access one for git-receive-pack too, which asks the host whether the
// credential may push without pushing anything.
func checkGit(ctx context.Context, f *silofile.Silofile, environ []string) []CheckResult {
	if len(f.Git) == 0 {
		return nil
	}
	r := &credentials.Resolver{Folder: f.Folder, Getenv: func(k string) string { return getenvFrom(environ, k) }}
	policy, err := gitPolicy(f, func(c silofile.Credential, host string) *gitproxy.CredentialSource {
		return &gitproxy.CredentialSource{Name: c.Name, Token: func(ctx context.Context) (string, error) {
			v, err := r.ResolveHost(ctx, c, host)
			return v.Secret, err
		}}
	})
	if err != nil {
		return []CheckResult{{Name: "git", Detail: err.Error()}}
	}
	tokens := &gitproxy.TokenSet{}
	tok, err := tokens.Mint(gitproxy.Caller{ID: "dry-run"})
	if err != nil {
		return []CheckResult{{Name: "git", Detail: err.Error()}}
	}
	proxy := &gitproxy.GitProxy{Policy: policy, Tokens: tokens, ForwarderFor: gitCheckForwarder}
	// A git client's request, as the proxy only answers those.
	auth := map[string]string{"Authorization": "Basic " + basic("silo", tok), "User-Agent": "git/silo-dry-run"}
	var out []CheckResult
	for _, g := range f.Git {
		res := CheckResult{Name: "git " + g.Name}
		repo, err := gitproxy.ParseRepo(g.Repo)
		if err != nil {
			res.Detail = err.Error()
			out = append(out, res)
			continue
		}
		cred := "anonymously"
		if c := gitCredential(f, g); c != nil {
			if _, err := r.ResolveHost(ctx, *c, repo.Host); err != nil {
				res.Detail = err.Error()
				out = append(out, res)
				continue
			}
			cred = "with credential " + c.Name
		}
		services := []string{"git-upload-pack"}
		if g.Access == "push" {
			services = append(services, "git-receive-pack")
		}
		res.OK = true
		for _, svc := range services {
			resp := proxy.HandleRequest(ctx, "GET", "/"+repo.String()+".git/info/refs", "service="+svc, auth, nil)
			body := resp.Body
			if resp.Stream != nil {
				if resp.Status != 200 {
					body, _ = io.ReadAll(io.LimitReader(resp.Stream, 4096))
				}
				resp.Stream.Close()
			}
			if resp.Status != 200 {
				res.OK = false
				what := "read"
				if svc == "git-receive-pack" {
					what = "push"
				}
				res.Detail = fmt.Sprintf("%s: %s access %s refused: %d %s", repo, what, cred, resp.Status, strings.TrimSpace(string(body)))
				break
			}
		}
		if res.OK {
			res.Detail = fmt.Sprintf("%s: %s access %s", repo, accessName(g.Access), cred)
		}
		out = append(out, res)
	}
	return out
}

func accessName(a string) string {
	if a == "" {
		return "read"
	}
	return a
}

// gitCheckForwarder replaces the dry-run check's upstream, for tests.
var gitCheckForwarder func(host string) (gitproxy.Forwarder, error)

func getenvFrom(environ []string, k string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], k+"="); ok {
			return v
		}
	}
	return ""
}

func basic(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
