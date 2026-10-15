// Adapted from github.com/bwsalmon/grain pkg/gitproxy/protocol.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

import (
	"fmt"
	"regexp"
	"strings"
)

// pathRE matches the four legal smart-HTTP paths, e.g.
// /owner/repo.git/info/refs, or /host/owner/repo.git/info/refs for a
// repository on a named git host (/github.com/acme/widgets.git/...). A
// leading segment is a host only when it contains a dot or a colon,
// which no GitHub owner can.
var pathRE = regexp.MustCompile(
	`^/(?:(?P<host>[^/]*[.:][^/]*)/)?(?P<owner>[^/]+)/(?P<repo>[^/]+?)(?:\.git)?/` +
		`(?P<action>info/refs|git-upload-pack|git-receive-pack)$`)

// GitRequest is one of the four legal smart-HTTP paths, decomposed.
type GitRequest struct {
	Owner  string
	Repo   string
	Action string // "info/refs", "git-upload-pack", or "git-receive-pack"
	// Host is the git host the path named, as written, or empty for the
	// /owner/repo form, which means the proxy's default host
	// (GitProxy.DefaultHost). HandleRequest normalizes it before anything
	// else reads it.
	Host string
}

// ParsePath matches path against the four legal smart-HTTP shapes. The
// second return is false if it isn't one of them.
func ParsePath(path string) (GitRequest, bool) {
	m := pathRE.FindStringSubmatch(path)
	if m == nil {
		return GitRequest{}, false
	}
	req := GitRequest{}
	for i, name := range pathRE.SubexpNames() {
		switch name {
		case "host":
			req.Host = m[i]
		case "owner":
			req.Owner = m[i]
		case "repo":
			req.Repo = m[i]
		case "action":
			req.Action = m[i]
		}
	}
	return req, true
}

// IsValidGitRequest is a tight allowlist of the client shapes a real git
// client produces -- mirrors what FINOS Git Proxy's validGitRequest()
// checks: a git/ user agent,
// and, for the pack endpoints, an Accept header naming the matching
// x-git-* result type. Rejecting anything else means a browser poking at
// these paths gets a plain 404 rather than a confusing partial response.
func IsValidGitRequest(userAgent, accept, action string) bool {
	if !strings.HasPrefix(userAgent, "git/") {
		return false
	}
	if action == "info/refs" {
		return true // git sends Accept: */* here; nothing more to check
	}
	expected := fmt.Sprintf("application/x-%s-result", action)
	return strings.Contains(accept, expected)
}

// Denial is how this proxy refuses one request: an HTTP status, and the
// reason as plain text.
//
// Plain text, not the ERR pkt-line this used to send. A git client only
// parses pkt-lines out of a *200* response carrying a git content type;
// against the 401/403/500 a refusal is, it falls back to printing the
// body verbatim as "remote: " lines, so the pkt-line's own 4-hex length
// prefix ended up in front of the sentence an operator reads:
//
//	remote: 00c6ERR no credential configured for owner/repo -- ...
//
// which is what grain's first clone of a deployment with no GitHub
// credential yet actually printed. The
// Content-Type is explicit for the same reason: git only shows the body
// at all when it is told the body is text.
func Denial(status int, message string) ProxyResponse {
	return ProxyResponse{
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:    []byte(message + "\n"),
	}
}
