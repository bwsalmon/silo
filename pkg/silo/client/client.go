// Package client is a typed Go client of the silo/v1 fence API
// (docs/silo/fence-api.md). The silo CLI is built on it, and so are the
// later commands that reach silod: install, exports, agents.
//
// The client knows the API's paths, bodies and errors. Who the caller
// is belongs to the transport: inside the cloud the gateway knows it
// from the caller's address and no credentials are sent; outside, the
// front door (silo proxy, IAP) supplies a RoundTripper of its own
// through Options.Transport.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultEndpoint is the fence API's base URL inside the cloud, on the
// gateway's API port (fence-api.md, "Transport and conventions", SV-1).
const DefaultEndpoint = "http://silo.internal:8080/silo/v1/"

// Options configure a Client.
type Options struct {
	// Endpoint is the API's base URL, ending in /silo/v1/. Empty is
	// DefaultEndpoint.
	Endpoint string
	// Transport carries each request: nil is http.DefaultTransport.
	// It is the seam for the front door, whose transport tunnels to
	// the gateway and says who is calling.
	Transport http.RoundTripper
	// Timeout bounds each call; 0 is 30 seconds.
	Timeout time.Duration
	// UserAgent is sent with each call.
	UserAgent string
}

// Client calls the fence API.
type Client struct {
	base *url.URL
	hc   *http.Client
	ua   string
}

// New returns a Client for opts.
func New(opts Options) (*Client, error) {
	ep := opts.Endpoint
	if ep == "" {
		ep = DefaultEndpoint
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: %v", ep, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q: want an http or https URL", ep)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "silo-client"
	}
	return &Client{base: u, hc: &http.Client{Transport: opts.Transport, Timeout: timeout}, ua: ua}, nil
}

// Endpoint is the base URL the client calls.
func (c *Client) Endpoint() *url.URL {
	u := *c.base
	return &u
}

// Error is a refusal from the API (fence-api.md, "Errors", SV-2).
type Error struct {
	// Status is the HTTP status it came with; 0 inside an operation.
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is an API refusal with code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Mutation is what every mutating call takes: an idempotency key and
// a dry run (SV-1).
type Mutation struct {
	// RequestID makes a repeat within 24 hours return the first
	// call's result.
	RequestID string
	// ValidateOnly runs every check and changes nothing.
	ValidateOnly bool
}

func (m Mutation) query(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if m.RequestID != "" {
		q.Set("request_id", m.RequestID)
	}
	if m.ValidateOnly {
		q.Set("validate_only", "true")
	}
	return q
}

// Do makes one call: method on path (relative to the endpoint, as
// "silos/team-a/ci"), with query, body encoded as JSON if it isn't nil,
// and the answer decoded into out if it isn't nil. A refusal is an
// *Error.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	// path's segments are escaped already (SiloPath, PathEscape), so it
	// is appended to the base as text rather than resolved.
	base := *c.base
	base.RawQuery, base.Fragment = "", ""
	u, err := url.Parse(base.String() + path)
	if err != nil {
		return fmt.Errorf("path %q: %v", path, err)
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%s %s: reading the answer: %w", method, u.Path, err)
	}
	if resp.StatusCode/100 != 2 {
		var env struct {
			Error *Error `json:"error"`
		}
		if json.Unmarshal(b, &env) == nil && env.Error != nil && env.Error.Code != "" {
			env.Error.Status = resp.StatusCode
			return env.Error
		}
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return &Error{Status: resp.StatusCode, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode),
			Message: fmt.Sprintf("%s %s: %s", method, u.Path, msg)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: the answer is not the JSON expected: %v", method, u.Path, err)
	}
	return nil
}

// SiloPath is the URL path, relative to the endpoint, of the silo ref
// names. ref is "self" (or ""), a path ("/team-a/ci"), or an id
// ("k7f3qa", or "-k7f3qa" as in a URL). The root's path, "/", has no
// URL of its own, so it is found through self: it is visible only to
// the root's own callers.
func (c *Client) SiloPath(ctx context.Context, ref string) (string, error) {
	switch {
	case ref == "" || ref == "self":
		return "self", nil
	case ref == "/":
		self, err := c.Describe(ctx, "self", false)
		if err != nil {
			return "", err
		}
		if self.Parent != nil {
			return "", &Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no silo /",
				Details: map[string]any{"silo": "/"}}
		}
		return "silos/-" + self.ID, nil
	case strings.HasPrefix(ref, "/"):
		segs := strings.Split(strings.Trim(ref, "/"), "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		return "silos/" + strings.Join(segs, "/"), nil
	case strings.HasPrefix(ref, "-"):
		return "silos/" + url.PathEscape(ref), nil
	case strings.Contains(ref, "/"):
		return "", fmt.Errorf("silo %q: a path begins with /", ref)
	}
	return "silos/-" + url.PathEscape(ref), nil
}
