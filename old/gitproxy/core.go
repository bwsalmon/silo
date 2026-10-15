// Adapted from github.com/bwsalmon/grain pkg/gitproxy/core.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// The proxy's decision logic -- independent of the HTTP transport.
// Order matters:
//
//	match the four legal smart-HTTP paths,
//	authenticate the caller by its token (Authenticator),
//	ask the Policy what the caller may do with the repository, default-deny,
//	read the body, bounded per action (spooled to disk past a threshold
//	  for a push),
//	for a push, read its command list and ask the Policy about every ref
//	  update before anything is forwarded,
//	ask the Policy for the credential and set Authorization,
//	stream the response back, and audit the request -- once.
//
// Authentication is checked before authorization so an unauthenticated
// caller learns nothing about what any caller is allowed -- a 401 with
// nothing else leaked, same as a real git server's own behavior toward an
// unauthenticated client.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// The default request-body limits, per action. A push carries a packfile
// and can be large; anything else is a fetch negotiation (wants, haves,
// and the odd filter line), which stays small even for a deep history,
// or a GET with no body at all. Both are generous on purpose: the point
// is to bound what one request can make the proxy hold, not to
// second-guess git.
const (
	DefaultMaxPushBodyBytes  int64 = 2 << 30  // 2 GiB, git-receive-pack
	DefaultMaxFetchBodyBytes int64 = 64 << 20 // 64 MiB, git-upload-pack and info/refs
)

// ProxyResponse is what Handle decided to answer with. Body and Stream
// are as on UpstreamResponse: a response carrying a Stream is passing an
// upstream body through unread, and whoever receives it must Close it
// (WriteResponse does).
type ProxyResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
	Stream  io.ReadCloser
}

// GitProxy is the proxy's whole decision logic, wired to its
// dependencies. Now is injected rather than read from time.Now() so
// tests can assert on audit timestamps deterministically.
type GitProxy struct {
	// Policy decides every request (policy.go). Required.
	Policy Policy
	// Tokens authenticates callers. Required; SessionToken is silo's.
	Tokens Authenticator
	// Audit records one entry per request; nil discards them.
	Audit AuditLog
	Now   func() time.Time
	// DefaultHost is the host of a request whose path names none
	// (/owner/repo.git/...). Empty means DefaultHost, github.com.
	DefaultHost string
	// Hosts is where each git host's requests are sent, by normalized
	// host, when that is not https://<host>.
	Hosts map[string]Host
	// ForwarderFor, when set, is the Forwarder for a host, in place of a
	// RealForwarder built from Hosts: the seam a test points at a
	// FakeForwarder or a LocalForwarder.
	ForwarderFor func(host string) (Forwarder, error)
	// PushSpoolThresholdBytes is the most of a push body held in memory
	// before spooling to disk. Zero means DefaultSpoolThreshold (16 MiB).
	PushSpoolThresholdBytes int64
	// MaxPushBodyBytes and MaxFetchBodyBytes cap the request body the
	// proxy will read for git-receive-pack and for everything else. Zero
	// means DefaultMaxPushBodyBytes / DefaultMaxFetchBodyBytes.
	MaxPushBodyBytes  int64
	MaxFetchBodyBytes int64
}

// StreamForwarder is a Forwarder that can also send a body from a
// reader, so a push spooled to disk is forwarded from its file rather
// than loaded into memory. RealForwarder and LocalForwarder are.
type StreamForwarder interface {
	Forwarder
	ForwardStream(method, path, query string, headers map[string]string, body io.Reader, contentLength int64, credential *Credential) (UpstreamResponse, error)
}

// pushSpoolThreshold returns the threshold above which push bodies spool to disk.
func (p *GitProxy) pushSpoolThreshold() int64 {
	if p.PushSpoolThresholdBytes > 0 {
		return p.PushSpoolThresholdBytes
	}
	return DefaultSpoolThreshold
}

// maxBodyBytes is the most of a request body the proxy will read for
// action.
func (p *GitProxy) maxBodyBytes(action string) int64 {
	if action == "git-receive-pack" {
		if p.MaxPushBodyBytes > 0 {
			return p.MaxPushBodyBytes
		}
		return DefaultMaxPushBodyBytes
	}
	if p.MaxFetchBodyBytes > 0 {
		return p.MaxFetchBodyBytes
	}
	return DefaultMaxFetchBodyBytes
}

// errBodyTooLarge is readBody's answer to a body over the action's limit.
var errBodyTooLarge = errors.New("request body too large")

// readBody reads body into memory, refusing one longer than limit. A nil
// body is no body at all, and stays nil.
func readBody(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func (p *GitProxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *GitProxy) audit() AuditLog {
	if p.Audit != nil {
		return p.Audit
	}
	return NullAuditLog{}
}

// defaultHost is the host a request naming none goes to.
func (p *GitProxy) defaultHost() string {
	if h, err := NormalizeHost(p.DefaultHost); err == nil && h != "" {
		return h
	}
	return DefaultHost
}

// Handle is HandleRequest for a body already in memory; nil is no body.
func (p *GitProxy) Handle(ctx context.Context, method, path, query string, headers map[string]string, body []byte) ProxyResponse {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return p.HandleRequest(ctx, method, path, query, headers, reader)
}

// HandleRequest decides how to answer one request and, if it is allowed,
// forwards it -- the entire proxy, as one call -- and records exactly one
// audit entry for it, whatever the answer.
//
// body is not touched until the request has been decided from its path
// and headers alone: a caller that is not running git, has no valid
// token, or may not reach the repository is answered without a byte of
// it being read, and an allowed one is read only up to maxBodyBytes for
// its action (413 past that). nil is no body.
func (p *GitProxy) HandleRequest(ctx context.Context, method, path, query string, headers map[string]string, body io.Reader) ProxyResponse {
	entry := &AuditEntry{Time: p.now()}
	resp := p.handle(ctx, entry, method, path, query, headers, body)
	entry.Status = resp.Status
	p.audit().Record(*entry)
	return resp
}

// deny fills in entry for a refusal, and returns it.
func deny(entry *AuditEntry, status int, outcome, message string) ProxyResponse {
	entry.Outcome, entry.Reason = outcome, message
	return Denial(status, message)
}

func (p *GitProxy) handle(ctx context.Context, entry *AuditEntry, method, path, query string, headers map[string]string, body io.Reader) ProxyResponse {
	req, ok := ParsePath(path)
	if !ok {
		entry.Outcome = "denied: not a git smart-HTTP path"
		return ProxyResponse{Status: 404, Body: []byte("not found")}
	}
	entry.Action = req.Action
	if req.Action == "info/refs" {
		if v, err := url.ParseQuery(query); err == nil {
			entry.Service = v.Get("service")
		}
	}
	if !IsValidGitRequest(headers["User-Agent"], headers["Accept"], req.Action) {
		entry.Outcome = "denied: not a git client"
		return ProxyResponse{Status: 404, Body: []byte("not found")}
	}
	// One spelling of the host from here on, normalized, so
	// /owner/repo and /<default host>/owner/repo are one repository and
	// every check below compares like with like.
	host, err := NormalizeHost(req.Host)
	if err != nil {
		entry.Outcome = "denied: not a git host"
		return ProxyResponse{Status: 404, Body: []byte("not found")}
	}
	if host == "" {
		host = p.defaultHost()
	}
	repo := Repo{Host: host, Owner: req.Owner, Name: req.Repo}
	entry.Repo = repo.String()

	token, hasToken := ExtractBasicAuthToken(headers["Authorization"])
	var caller Caller
	if hasToken && p.Tokens != nil {
		caller, ok = p.Tokens.Authenticate(token)
	}
	if !hasToken || !ok {
		unauthenticated := deny(entry, 401, "denied: authentication required", "authentication required")
		unauthenticated.Headers["WWW-Authenticate"] = `Basic realm="git-proxy"`
		return unauthenticated
	}
	entry.Caller = caller.Audit

	// A push's info/refs advertisement is authorized as the push it opens,
	// not as the fetch its action looks like (isPush). git prints the body
	// of a refused advertisement but not of a refused git-receive-pack
	// POST, so this is the one place a read-only caller's push can be
	// refused in a sentence the person running git actually sees.
	push := isPush(req.Action, query)
	access, refusal, err := p.Policy.Access(ctx, caller, repo)
	if err != nil {
		entry.Outcome = errPolicy("authorization check", err)
		return Denial(500, "authorization check failed")
	}
	switch {
	case access < AccessRead:
		if refusal == "" {
			refusal = fmt.Sprintf("%s is not in scope for this caller", repo)
		}
		return deny(entry, 403, "denied: not in scope", refusal)
	case push && access < AccessPush:
		if refusal == "" {
			refusal = fmt.Sprintf("%s is read-only for this caller", repo)
		}
		return deny(entry, 403, "denied: push to read-only repository", refusal)
	}

	limit := p.maxBodyBytes(req.Action)
	var data []byte
	var spooled *spooledBody
	if req.Action == "git-receive-pack" {
		spooled, err = spoolBody(body, limit, p.pushSpoolThreshold())
		if spooled != nil {
			defer spooled.Close()
		}
	} else {
		data, err = readBody(body, limit)
	}
	if errors.Is(err, errBodyTooLarge) {
		return deny(entry, 413, fmt.Sprintf("denied: request body over %d bytes", limit),
			fmt.Sprintf("request body exceeds the git proxy's %d-byte limit for %s", limit, req.Action))
	}
	if err != nil {
		return deny(entry, 400, fmt.Sprintf("error: reading request body: %v", err), "reading request body")
	}

	// A push held in memory is forwarded from there. One spooled to disk
	// is read from its file -- by the ref check below as a stream, and on
	// to a StreamForwarder as one -- and is loaded into memory whole only
	// for a forwarder that needs the bytes.
	if spooled != nil && spooled.file == nil {
		data = spooled.mem
	}

	// A push may update only the refs its Policy allows (refupdates.go),
	// read off the command list before anything is forwarded.
	if req.Action == "git-receive-pack" {
		if refusal, refused := p.refuseRefUpdates(ctx, entry, caller, repo, headers["Content-Encoding"], spooled.reader); refused {
			return refusal
		}
	}

	credential, refused := p.credential(ctx, entry, caller, repo)
	if refused != nil {
		return *refused
	}
	if credential != nil {
		entry.Credential = credential.Name
	}
	forwarder, err := p.forwarderFor(repo.Host)
	if err != nil {
		entry.Outcome = fmt.Sprintf("error: %v", err)
		return Denial(502, "upstream request failed")
	}
	upstreamPath := fmt.Sprintf("/%s/%s.git/%s", repo.Owner, repo.Name, req.Action)
	var upstream UpstreamResponse
	if sf, ok := forwarder.(StreamForwarder); ok && spooled != nil && spooled.file != nil {
		r, rErr := spooled.NewReader()
		if rErr != nil {
			entry.Outcome = fmt.Sprintf("error: reading spooled push body: %v", rErr)
			return Denial(500, "reading spooled push body")
		}
		if c, ok := r.(io.Closer); ok {
			defer c.Close()
		}
		upstream, err = sf.ForwardStream(method, upstreamPath, query, headers, r, spooled.Len(), credential)
	} else {
		if spooled != nil && spooled.file != nil {
			if data, err = spooled.Bytes(); err != nil {
				entry.Outcome = fmt.Sprintf("error: reading spooled push body: %v", err)
				return Denial(500, "reading spooled push body")
			}
		}
		upstream, err = forwarder.Forward(method, upstreamPath, query, headers, data, credential)
	}
	if err != nil {
		entry.Outcome = fmt.Sprintf("error: forwarding failed: %v", err)
		return Denial(502, "upstream request failed")
	}
	entry.Outcome = fmt.Sprintf("forwarded: %d", upstream.Status)
	if upstream.Status == 401 {
		// The credential was refused upstream. Passed through as it came,
		// its WWW-Authenticate would have git ask for a new password for
		// this proxy, which is not what was wrong; an ambient credential's
		// token is asked for again next time instead.
		if credential != nil {
			if r, ok := p.Policy.(CredentialRefuser); ok {
				r.CredentialRefused(credential)
			}
		}
		if upstream.Stream != nil {
			upstream.Stream.Close()
		}
		name := "no credential (anonymous)"
		if credential != nil {
			name = "credential " + credential.Name
		}
		entry.Reason = fmt.Sprintf("%s answered 401 to %s", repo.Host, name)
		return Denial(403, fmt.Sprintf("%s refused %s for %s (HTTP 401)", repo.Host, name, repo))
	}
	return ProxyResponse(upstream)
}

// credential is the Policy's credential for repo, or the refusal to
// answer with when there is none to be had: the Policy failed, or
// answered with a credential for another host, which is refused rather
// than sent.
func (p *GitProxy) credential(ctx context.Context, entry *AuditEntry, caller Caller, repo Repo) (*Credential, *ProxyResponse) {
	credential, err := p.Policy.Credential(ctx, caller, repo)
	if err != nil {
		entry.Outcome = errPolicy("resolving credential", err)
		r := Denial(500, fmt.Sprintf("resolving the credential for %s failed: %v", repo, err))
		return nil, &r
	}
	if credential != nil && credential.Host != "" {
		if h, err := NormalizeHost(credential.Host); err != nil || h != repo.Host {
			msg := fmt.Sprintf("credential %s is for git host %s, not %s, so it is not sent", credential.Name, credential.Host, repo.Host)
			r := deny(entry, 500, "error: "+msg, msg)
			return nil, &r
		}
	}
	return credential, nil
}

// refuseRefUpdates is the push's ref check: the refusal to answer with,
// and true, when the command list in body names a ref the Policy refuses
// -- or cannot be read at all, which is refused rather than forwarded on
// trust. A refused ref is answered as receive-pack would answer it
// (rejectPush), so the client prints why; an unreadable push gets a plain
// 403. An allowed push's grants go on entry.
//
// body opens the request body, as the client encoded it, from its start;
// it is read only as far as the command list, and past it to the
// packfile only when the Policy asks for a fast-forward proof.
func (p *GitProxy) refuseRefUpdates(ctx context.Context, entry *AuditEntry, caller Caller, repo Repo, contentEncoding string, body func() io.Reader) (ProxyResponse, bool) {
	commands, err := commandList(body(), contentEncoding)
	if err == nil {
		var updates []RefUpdate
		var capabilities []string
		if updates, capabilities, err = parseReceivePack(commands); err == nil {
			check := &pushCheck{
				body: func() (io.Reader, error) { return decodedPush(body(), contentEncoding) },
				upstream: func(wants []string, have string, hashLen int) (packCommits, error) {
					return p.upstreamCommits(ctx, entry, caller, repo, wants, have, hashLen)
				},
			}
			decision, err := p.Policy.RefUpdates(ctx, caller, repo, updates, check.fastForward)
			if err != nil {
				entry.Outcome = errPolicy("ref check", err)
				return Denial(500, "authorization check failed"), true
			}
			if decision.Refusal == "" {
				entry.Refs = decision.Grants
				return ProxyResponse{}, false
			}
			refusal := deny(entry, 403, "denied: ref not allowed", decision.Refusal)
			if rejected, ok := rejectPush(updates, capabilities, decision.Refusal); ok {
				refusal = rejected
			}
			return refusal, true
		}
	}
	return deny(entry, 403, fmt.Sprintf("denied: unreadable push: %v", err),
		fmt.Sprintf("refusing a push to %s whose ref updates could not be read: %v", repo, err)), true
}

// forwarderFor is where a request for host is sent: ForwarderFor's
// answer, or a RealForwarder to the host (or its Hosts base URL).
func (p *GitProxy) forwarderFor(host string) (Forwarder, error) {
	if p.ForwarderFor != nil {
		return p.ForwarderFor(host)
	}
	h, err := normalizeHostConfig(host, p.Hosts[host])
	if err != nil {
		return nil, err
	}
	return &RealForwarder{Host: h.hostPort(host), UseTLS: h.useTLS()}, nil
}

// isPush reports whether a request is part of a push: git-receive-pack,
// or the info/refs advertisement that opens one.
func isPush(action, query string) bool {
	if action == "git-receive-pack" {
		return true
	}
	if action != "info/refs" {
		return false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return false
	}
	return values.Get("service") == "git-receive-pack"
}
