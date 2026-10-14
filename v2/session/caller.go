package session

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/gateway/proxy"
	"github.com/bwsalmon/silo/v2/mcp"
)

func init() { RegisterToolProvider("caller", provideCaller) }

// CallerMCP is an MCP server the caller serves and passes to the
// session (grain.md §2.2): grain's task tools. The gateway connects to
// it as a client and fronts its tools as <Name>__<tool>, allowlisted
// and audited like a Silofile's mcp[] server. Silo neither starts nor
// stops it. When it goes away, as while the caller restarts, calls fail
// with a retryable *proxy.UnavailableError, and the gateway dials it
// again on the next call, so a caller back on the same address is used
// again (grain.md §2.7).
type CallerMCP struct {
	// Name prefixes the server's tools. It follows mcp[]'s naming rule
	// and must not be the name of a Silofile mcp[] entry.
	Name string
	// URL is the server's Streamable HTTP endpoint: an http:// or
	// https:// URL, or a Unix socket, as unix:<path> or an absolute
	// path, served at /mcp on the socket.
	URL string
	// Token is the session's own bearer token for the server, sent as
	// Authorization: Bearer <token> and redacted like a credential.
	Token string
	// Tools is the allowlist of the server's own tool names; nil allows
	// every tool it lists.
	Tools []string
	// Blocking names tools that may wait as long as they need (grain's
	// wait_for_checks, ask_question): the gateway gives them no timeout,
	// and the CLI's own MCP timeout is raised (design §6.2).
	Blocking []string
}

var callerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// socket returns the Unix socket path c names, or "" for an HTTP URL.
func (c CallerMCP) socket() (string, error) {
	switch {
	case strings.HasPrefix(c.URL, "unix:"):
		p := strings.TrimPrefix(strings.TrimPrefix(c.URL, "unix:"), "//")
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("caller MCP server %s: unix: needs an absolute socket path, not %q", c.Name, p)
		}
		return p, nil
	case filepath.IsAbs(c.URL):
		return c.URL, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("caller MCP server %s: %q is neither an http(s) URL nor a Unix socket (unix:<path> or an absolute path)", c.Name, c.URL)
	}
	return "", nil
}

// spec checks c and returns the proxy spec that fronts it.
func (c CallerMCP) spec() (proxy.Spec, error) {
	if !callerName.MatchString(c.Name) {
		return proxy.Spec{}, fmt.Errorf("caller MCP server name %q: use lower-case letters, digits, - and _", c.Name)
	}
	if strings.Contains(c.Name, proxy.Separator) {
		return proxy.Spec{}, fmt.Errorf("caller MCP server name %q holds %q, which separates a server's name from its tools'", c.Name, proxy.Separator)
	}
	sock, err := c.socket()
	if err != nil {
		return proxy.Spec{}, err
	}
	for _, b := range c.Blocking {
		if c.Tools != nil && !contains(c.Tools, b) {
			return proxy.Spec{}, fmt.Errorf("caller MCP server %s: blocking tool %s is not in its allowlist", c.Name, b)
		}
	}
	sp := proxy.Spec{Name: c.Name, URL: c.URL, Allow: c.Tools, Required: true, Blocking: c.Blocking, Reconnect: true}
	if c.Tools != nil && len(c.Tools) == 0 {
		sp.Allow = []string{}
	}
	if c.Token != "" {
		sp.Headers = map[string]string{"Authorization": "Bearer " + c.Token}
	}
	if sock != "" {
		sp.URL, sp.HTTP = mcp.UnixURL, mcp.UnixClient(sock)
	}
	return sp, nil
}

// callerSecret is the name a caller server's token is redacted as.
func callerSecret(name string) string { return "caller-mcp:" + name }

// provideCaller connects to the caller's MCP servers and serves their
// allowed tools through the gateway. Each is required: a caller server
// that does not answer, or refuses the token, fails the session.
func provideCaller(ctx context.Context, s *Session) ([]gateway.Tool, Undo, error) {
	if len(s.opts.CallerMCP) == 0 {
		return nil, nil, nil
	}
	declared := map[string]bool{}
	for _, m := range s.File.MCP {
		declared[m.Name] = true
	}
	var specs []proxy.Spec
	for _, c := range s.opts.CallerMCP {
		sp, err := c.spec()
		if err != nil {
			return nil, nil, &Error{Code: ExitUsage, Err: err}
		}
		if declared[c.Name] {
			return nil, nil, &Error{Code: ExitUsage, Err: fmt.Errorf("caller MCP server %s has the name of the Silofile's mcp[] entry %s", c.Name, c.Name)}
		}
		declared[c.Name] = true
		where := c.URL
		if sp.HTTP != nil {
			where = "the Unix socket " + strings.TrimPrefix(c.URL, "unix:")
		}
		note := ""
		if len(c.Blocking) > 0 {
			note = fmt.Sprintf(" (blocking, with no gateway timeout: %s)", strings.Join(c.Blocking, ", "))
		}
		s.status("caller mcp %s: connecting to %s%s", c.Name, where, note)
		specs = append(specs, sp)
	}
	opts := mcpOptions
	opts.Warn = s.warnf
	p := proxy.New(opts)
	undo := func(ctx context.Context) error { return p.Close(ctx) }
	tools, err := p.Start(ctx, specs)
	if err != nil {
		return nil, undo, err
	}
	s.onGateway(func(g *gateway.Gateway) { p.Attach(g) })
	return tools, undo, nil
}
