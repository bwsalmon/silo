// Adapted from github.com/bwsalmon/grain pkg/gitproxy/forward.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// Forwards a validated request to the real git smart-HTTP endpoint.
//
// Behind a Forwarder interface for the same reason command execution sits
// behind an interface elsewhere: the proxy's decision logic (core.go) is
// worth testing without a real network call to github.com on every test
// run.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UpstreamResponse is what the forward target answered.
//
// The body is in one of two places. Stream, when set, is the body still
// being read from the forward target -- which is how the real forwarders
// answer, so a clone's packfile goes on to the sandbox as it arrives
// rather than being held whole in the proxy's memory -- and whoever
// receives the response owns it and must Close it. Body is the whole body
// already in memory, for a response that was built rather than forwarded
// (FakeForwarder, a test); it is ignored when Stream is set.
type UpstreamResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
	Stream  io.ReadCloser
}

// Forwarder sends one validated request on to the real git host.
type Forwarder interface {
	Forward(method, path, query string, headers map[string]string, body []byte, credential *Credential) (UpstreamResponse, error)
}

// RealForwarder talks to a git smart-HTTP endpoint over HTTPS by default.
// UseTLS false is the git-transport half of the mock-GitHub test seam:
// it lets a live test point a real GitProxy's forward target at a local
// stand-in server instead of github.com.
//
// Client, when set, is used for the request, but its CheckRedirect is
// always replaced by one that refuses to follow anything; when nil,
// defaultForwardClient is used.
type RealForwarder struct {
	Host   string
	UseTLS bool
	Client *http.Client
}

// Forward sends the request to Host, with credential (when non-nil) as
// Basic auth (Credential.basicAuth), and returns the response with its body as Stream, unread.
func (f *RealForwarder) Forward(method, path, query string, headers map[string]string, body []byte, credential *Credential) (UpstreamResponse, error) {
	var reader io.Reader
	var contentLength int64 = -1
	if body != nil {
		reader = bytes.NewReader(body)
		contentLength = int64(len(body))
	}
	return f.ForwardStream(method, path, query, headers, reader, contentLength, credential)
}

// ForwardStream sends the request to Host reading body from reader with the given contentLength (-1 if unknown).
func (f *RealForwarder) ForwardStream(method, path, query string, headers map[string]string, body io.Reader, contentLength int64, credential *Credential) (UpstreamResponse, error) {
	// Checked on every request rather than trusted from construction: a
	// RealForwarder is a plain struct anybody can fill in, and what is at
	// stake is the credential Forward is about to attach.
	if err := ValidateForwardTarget(f.Host, f.UseTLS); err != nil {
		return UpstreamResponse{}, err
	}
	scheme := "https"
	if !f.UseTLS {
		scheme = "http"
	}
	target := (&url.URL{Scheme: scheme, Host: f.Host, Path: path, RawQuery: query}).String()

	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return UpstreamResponse{}, err
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	setHeader(req, "Content-Type", headers["Content-Type"])
	accept := headers["Accept"]
	if accept == "" {
		accept = "*/*"
	}
	setHeader(req, "Accept", accept)
	userAgent := headers["User-Agent"]
	if userAgent == "" {
		userAgent = "git/silo-gitproxy"
	}
	setHeader(req, "User-Agent", userAgent)
	// git gzip-compresses a git-upload-pack request body once its
	// "have"/"want" negotiation gets large enough (http.postBuffer) --
	// forwarding a compressed body without this header means the upstream
	// tries to parse gzip bytes as plain pkt-line data.
	setHeader(req, "Content-Encoding", headers["Content-Encoding"])
	if credential != nil {
		req.Header.Set("Authorization", credential.basicAuth())
	}

	resp, err := forwardClient(f.Client).Do(req)
	if err != nil {
		return UpstreamResponse{}, err
	}
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	return UpstreamResponse{Status: resp.StatusCode, Headers: respHeaders, Stream: resp.Body}, nil
}

// ValidateForwardTarget is whether host may be forwarded to at all, over
// TLS or not. A custom git host (GitHub Enterprise, a mock) is a supported
// configuration, so this does not insist on github.com; what it insists on
// is that the request -- and the credential riding on it -- goes to the
// host that was configured, and is not readable on the way there:
//
//   - host must be a bare hostname or IP, with an optional port: no
//     scheme, userinfo, path, query or fragment. Forward builds its URL
//     from this string, and "github.com@evil.example" or
//     "evil.example/github.com" would otherwise send the request, Basic
//     auth and all, somewhere other than where it reads as going.
//   - plain HTTP is allowed only to a loopback address (a local test
//     upstream, Host.BaseURL http://127.0.0.1:...). Anywhere else a token sent in
//     the clear is a token handed to anyone on the path.
func ValidateForwardTarget(host string, useTLS bool) error {
	if host == "" {
		return fmt.Errorf("gitproxy: no forward host configured")
	}
	if strings.ContainsAny(host, "/\\?#@ \t\r\n") {
		return fmt.Errorf("gitproxy: forward host %q is not a bare host[:port]", host)
	}
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Host != host || parsed.User != nil || parsed.Path != "" || parsed.Hostname() == "" {
		return fmt.Errorf("gitproxy: forward host %q is not a bare host[:port]", host)
	}
	if !useTLS && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("gitproxy: refusing to forward to %q over plain HTTP: "+
			"that is only allowed to a loopback address (a local mock); use HTTPS for any other host", host)
	}
	return nil
}

// isLoopbackHost reports whether hostname names this machine's loopback
// interface: "localhost" or a literal loopback IP. Any other name is not
// resolved here -- whatever it resolves to today, it is a name somebody
// else's DNS answers for.
func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// forwardResponseHeaderTimeout bounds how long the upstream may take to
// start answering. A large git-upload-pack can spend a while counting and
// compressing objects before the first byte, so this is generous; it is
// there so a host that accepts the connection and never answers does not
// hold a proxy request open forever.
const forwardResponseHeaderTimeout = 5 * time.Minute

// forwardTimeout bounds one whole forwarded request, body included --
// http.Client's Timeout goes on running while the response body streams
// through to the sandbox. The body is not held in memory (Forward returns
// it as a stream), so what this bounds is how long one request may keep an
// upstream connection open, and it has to leave room for cloning a large
// repository over a slow link.
const forwardTimeout = 30 * time.Minute

// defaultForwardClient is what RealForwarder uses when Client is nil,
// rather than http.DefaultClient, which has no timeout and follows
// redirects.
var defaultForwardClient = newDefaultForwardClient()

func newDefaultForwardClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = forwardResponseHeaderTimeout
	return &http.Client{
		Transport:     transport,
		Timeout:       forwardTimeout,
		CheckRedirect: refuseRedirect,
	}
}

// refuseRedirect hands a 3xx back to the caller unfollowed. Following it
// here would replay the Authorization header Forward attached: net/http
// only strips it when the redirect changes hostname, so a redirect to
// another port or path on the same host would carry the credential there.
// A redirect is the git client's to see and act on, not the proxy's to
// absorb a hop earlier.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// forwardClient returns the client Forward sends with: defaultForwardClient
// when none is configured, otherwise a copy of the configured one with
// redirects refused, so no configuration can re-enable following them.
func forwardClient(configured *http.Client) *http.Client {
	if configured == nil {
		return defaultForwardClient
	}
	client := *configured
	client.CheckRedirect = refuseRedirect
	return &client
}

func setHeader(req *http.Request, key, value string) {
	if value != "" {
		req.Header.Set(key, value)
	}
}

// FakeForwarder records calls and replays a scripted response. For tests.
type FakeForwarder struct {
	Response UpstreamResponse
	Err      error

	mu    sync.Mutex
	Calls []ForwardCall
}

// ForwardCall is one recorded call to FakeForwarder.Forward.
type ForwardCall struct {
	Method     string
	Path       string
	Query      string
	Headers    map[string]string
	Body       []byte
	Credential *Credential
}

func (f *FakeForwarder) Forward(method, path, query string, headers map[string]string, body []byte, credential *Credential) (UpstreamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, ForwardCall{Method: method, Path: path, Query: query, Headers: headers, Body: body, Credential: credential})
	if f.Err != nil {
		return UpstreamResponse{}, f.Err
	}
	return f.Response, nil
}
