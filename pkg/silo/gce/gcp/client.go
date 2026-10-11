// Package gcp is the real gce.Client: compute/v1 through the generated
// client (google.golang.org/api/compute/v1), and the Cloud Quotas read
// over its REST API, with Application Default Credentials.
//
// On the control VM the credentials are the metadata server's. Tests
// point Options.ComputeEndpoint and Options.CloudQuotasEndpoint at the
// fake's HTTP server (gce/fake.Server) and pass its HTTP client, which
// turns authentication off; the same behaviour tests (package
// gce/gcetest) then run against the fake directly and through this
// client.
//
// What the client adds to the generated one:
//
//   - Retries. A request that gets 429 or a 5xx, or fails in transport,
//     is sent again with exponential backoff and jitter, honouring
//     Retry-After. Every mutation carries a requestId, the same on each
//     attempt, so GCE runs a retried mutation once.
//   - Errors. A refusal is GCE's own *googleapi.Error, code, reason and
//     message intact, so the facade can pass it through.
//   - Paging. Every List follows nextPageToken to the end.
//   - User-Agent silod/<version>.
//
// Operation polling is gce.Await, over WaitOperation.
package gcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

// Options configure a Client.
type Options struct {
	// Version is silod's version, sent as User-Agent silod/<Version>.
	Version string
	// ComputeEndpoint is compute/v1's base URL. Empty means GCE's.
	ComputeEndpoint string
	// CloudQuotasEndpoint is the Cloud Quotas base URL, under which
	// v1/projects/... is served. Empty means
	// https://cloudquotas.googleapis.com/.
	CloudQuotasEndpoint string
	// HTTPClient, when set, is used as it is, without credentials: for
	// tests against the fake. Unset means Application Default
	// Credentials, with ClientOptions.
	HTTPClient *http.Client
	// ClientOptions are passed to the credentials' transport when
	// HTTPClient is unset (option.WithCredentialsFile, say).
	ClientOptions []option.ClientOption

	// Attempts is how many times a request is sent before its error is
	// returned. Zero means 6.
	Attempts int
	// Backoff is the wait before the first retry; it doubles each time,
	// up to MaxBackoff. Zero means 500ms and 30s.
	Backoff, MaxBackoff time.Duration
	// PageSize, when set, is each List's maxResults. Zero leaves it to
	// GCE (500).
	PageSize int64
}

// DefaultCloudQuotasEndpoint is Cloud Quotas' base URL.
const DefaultCloudQuotasEndpoint = "https://cloudquotas.googleapis.com/"

// Client is the real gce.Client.
type Client struct {
	svc      *compute.Service
	http     *http.Client
	quotas   string
	pageSize int64
}

var _ gce.Client = (*Client)(nil)

// New makes a Client.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Attempts <= 0 {
		opts.Attempts = 6
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 500 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	var base http.RoundTripper
	if opts.HTTPClient != nil {
		base = opts.HTTPClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}
	} else {
		copts := append([]option.ClientOption{option.WithScopes(compute.CloudPlatformScope)}, opts.ClientOptions...)
		t, err := htransport.NewTransport(ctx, http.DefaultTransport, copts...)
		if err != nil {
			return nil, fmt.Errorf("gcp: credentials: %w", err)
		}
		base = t
	}
	hc := &http.Client{Transport: &retrying{
		base:       base,
		userAgent:  "silod/" + opts.Version,
		attempts:   opts.Attempts,
		backoff:    opts.Backoff,
		maxBackoff: opts.MaxBackoff,
	}}
	if opts.HTTPClient != nil {
		hc.Timeout = opts.HTTPClient.Timeout
	}
	sopts := []option.ClientOption{option.WithHTTPClient(hc)}
	if opts.ComputeEndpoint != "" {
		sopts = append(sopts, option.WithEndpoint(opts.ComputeEndpoint))
	}
	svc, err := compute.NewService(ctx, sopts...)
	if err != nil {
		return nil, fmt.Errorf("gcp: compute: %w", err)
	}
	quotas := opts.CloudQuotasEndpoint
	if quotas == "" {
		quotas = DefaultCloudQuotasEndpoint
	}
	if !strings.HasSuffix(quotas, "/") {
		quotas += "/"
	}
	return &Client{svc: svc, http: hc, quotas: quotas, pageSize: opts.PageSize}, nil
}

// retrying is the RoundTripper every request goes through: it sets the
// User-Agent, gives each mutation a requestId, and retries.
type retrying struct {
	base       http.RoundTripper
	userAgent  string
	attempts   int
	backoff    time.Duration
	maxBackoff time.Duration
}

// retryable reports whether a response with this status is worth
// sending again: GCE's rate limit, and its server errors.
func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func (t *retrying) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	req = req.Clone(ctx)
	ua := t.userAgent
	if prev := req.Header.Get("User-Agent"); prev != "" {
		ua += " " + prev
	}
	req.Header.Set("User-Agent", ua)
	if mutation(req) {
		q := req.URL.Query()
		if q.Get("requestId") == "" {
			q.Set("requestId", requestID())
			req.URL.RawQuery = q.Encode()
		}
	}
	// The body is sent again on each attempt, so keep it.
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	wait := t.backoff
	for attempt := 1; ; attempt++ {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
		resp, err := t.base.RoundTrip(req)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err == nil && !retryable(resp.StatusCode):
			return resp, nil
		case attempt >= t.attempts:
			return resp, err
		}
		d := jitter(wait)
		if err == nil {
			if ra := retryAfter(resp); ra > 0 {
				d = ra
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		wait = min(2*wait, t.maxBackoff)
	}
}

// mutation reports whether req changes something in compute/v1, and
// so takes a requestId, which GCE keeps for an hour to recognise the
// retry of a request it has already run. operations.wait is a POST that
// changes nothing, and Cloud Quotas is only read.
func mutation(req *http.Request) bool {
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	p := req.URL.Path
	return strings.Contains(p, "/projects/") && !strings.HasSuffix(p, "/wait") &&
		!strings.Contains(p, "/locations/")
}

func requestID() string {
	// GCE wants a UUID.
	var b [16]byte
	if _, err := cryptoRead(b[:]); err != nil {
		panic("gcp: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var cryptoRead = rand.Read

// jitter is d plus or minus a fifth.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d - d/5 + time.Duration(mrand.Int64N(int64(2*d/5)+1))
}

// retryAfter reads a Retry-After of seconds, capped at a minute.
func retryAfter(resp *http.Response) time.Duration {
	s, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || s <= 0 {
		return 0
	}
	return min(time.Duration(s)*time.Second, time.Minute)
}
