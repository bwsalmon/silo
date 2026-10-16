// Adapted from github.com/bwsalmon/grain pkg/gitproxy/hosts.go and pkg/model/githost.go (NormalizeGitHost) at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// Git hosts: how a host is spelt, and where its requests are sent.
//
// A request names its host in its path (/github.com/acme/widgets.git/...),
// or names none and is for GitProxy.DefaultHost. Either way it is
// forwarded to that host over HTTPS, unless GitProxy.Hosts gives the host
// a base URL of its own: another port, or plain http to a loopback test
// upstream (ValidateForwardTarget).
//
// grain records each credential's host in a <name>.host.json beside its
// token, in the daemon's credential directory; here the Policy says which
// host a credential is for (Credential.Host), so there is no directory.

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultHost is the host a request naming none is for, when
// GitProxy.DefaultHost is empty.
const DefaultHost = "github.com"

// NormalizeHost is a git host as the proxy compares it: lowercase
// host[:port], with a default port (:443) dropped and a scheme, path or
// trailing slash ignored, so "https://GHE.example.com/" and
// "ghe.example.com:443" are one host. Empty stays empty. Userinfo, a
// query or anything that is not a host at all is refused rather than
// folded into one.
func NormalizeHost(s string) (string, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return "", nil
	}
	hostport := text
	if strings.Contains(text, "://") {
		u, err := url.Parse(text)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("git host %q is not a host or URL", s)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("git host %q must be a bare host or base URL", s)
		}
		hostport = u.Host
	} else {
		hostport = strings.TrimSuffix(hostport, "/")
	}
	hostport = strings.ToLower(hostport)
	name, port, err := net.SplitHostPort(hostport)
	if err != nil {
		name, port = hostport, ""
	}
	if name == "" || strings.ContainsAny(name, "/@?#\\ \t") {
		return "", fmt.Errorf("git host %q is not a host name", s)
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(name, port), nil
	}
	return name, nil
}

// Host is where one git host's requests go, when that is not
// https://<host>.
type Host struct {
	// BaseURL is the scheme://host[:port] the host is reached on. Plain
	// http is allowed only to a loopback address.
	BaseURL string
}

// normalizeHostConfig checks h as host's: an http(s) URL with nothing
// past host[:port].
func normalizeHostConfig(host string, h Host) (Host, error) {
	base := strings.TrimSuffix(strings.TrimSpace(h.BaseURL), "/")
	if base == "" {
		return Host{}, nil
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return h, fmt.Errorf("base URL %q for %s must be http(s)://host[:port]", h.BaseURL, host)
	}
	return Host{BaseURL: u.Scheme + "://" + u.Host}, nil
}

// useTLS reports whether h is reached over https.
func (h Host) useTLS() bool {
	return !strings.HasPrefix(h.BaseURL, "http://")
}

// hostPort is where host's traffic goes: BaseURL's host[:port] when set,
// host otherwise.
func (h Host) hostPort(host string) string {
	if h.BaseURL != "" {
		if u, err := url.Parse(h.BaseURL); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return host
}
