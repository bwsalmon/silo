package gceclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/gceclient"
	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/gcetest"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

const (
	proj   = "host-project"
	region = "us-central1"
	zone   = "us-central1-a"
)

var ctx = context.Background()

// recorder serves the fake behind a handler that records each request
// and can fail the next ones it is told to.
type recorder struct {
	next http.Handler

	mu   sync.Mutex
	reqs []*http.Request
	// fail is how the next requests matching match are answered, first
	// first; a 0 status passes the request on.
	match  string
	fail   []int
	header http.Header
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Clone(context.Background()))
	var code int
	if len(r.fail) > 0 && strings.Contains(req.Method+" "+req.URL.Path, r.match) {
		code, r.fail = r.fail[0], r.fail[1:]
	}
	r.mu.Unlock()
	if code != 0 {
		_, _ = io.Copy(io.Discard, req.Body)
		for k, v := range r.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":`+strconv.Itoa(code)+`,"message":"try again","errors":[{"domain":"global","reason":"rateLimitExceeded","message":"try again"}]}}`)
		return
	}
	r.next.ServeHTTP(w, req)
}

// failNext makes the next len(codes) requests matching match fail with
// those codes.
func (r *recorder) failNext(match string, codes ...int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.match, r.fail = match, codes
}

func (r *recorder) requests(match string) []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*http.Request
	for _, req := range r.reqs {
		if strings.Contains(req.Method+" "+req.URL.Path, match) {
			out = append(out, req)
		}
	}
	return out
}

type env struct {
	*recorder
	fake   *fake.Fake
	client *gceclient.Client
}

func newEnv(t *testing.T, opts gceclient.Options) env {
	t.Helper()
	f := gcetest.NewFake(proj)
	rec := &recorder{next: f.Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	opts.ComputeEndpoint = srv.URL + "/compute/v1/"
	opts.CloudQuotasEndpoint = srv.URL + "/"
	opts.HTTPClient = srv.Client()
	if opts.Backoff == 0 {
		opts.Backoff = time.Millisecond
	}
	if opts.Version == "" {
		opts.Version = "1.2.3"
	}
	c, err := gceclient.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return env{recorder: rec, fake: f, client: c}
}

// TestBehaviour runs the behaviour tests the fake runs in process
// through the real client and the fake's HTTP server, once with GCE's
// own page size and once a page per item, so every List follows
// nextPageToken.
func TestBehaviour(t *testing.T) {
	for _, pageSize := range []int64{0, 1} {
		name := "DefaultPages"
		if pageSize == 1 {
			name = "OneItemPages"
		}
		t.Run(name, func(t *testing.T) {
			gcetest.Run(t, func(t *testing.T) gcetest.Env {
				e := newEnv(t, gceclient.Options{PageSize: pageSize})
				return gcetest.Env{Client: e.client, Fake: e.fake, Project: proj, Region: region, Zone: zone}
			})
		})
	}
}

func network(t *testing.T, e env, name string) *compute.Operation {
	t.Helper()
	op, err := e.client.InsertNetwork(ctx, proj, &compute.Network{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	op, err = gce.Await(ctx, e.client, proj, op)
	if err != nil || op.Error != nil {
		t.Fatalf("insert network: %+v %v", op, err)
	}
	return op
}

func TestUserAgent(t *testing.T) {
	e := newEnv(t, gceclient.Options{Version: "0.9.1"})
	network(t, e, "n")
	if _, err := e.client.ListComputeQuotaInfos(ctx, proj); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.requests("") {
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "silod/0.9.1") {
			t.Fatalf("%s %s: User-Agent %q", r.Method, r.URL.Path, ua)
		}
	}
}

func TestRetries(t *testing.T) {
	e := newEnv(t, gceclient.Options{})

	// A read that gets 503 and 429 is sent again until it succeeds.
	e.failNext("GET /compute/v1/projects/host-project/regions/us-central1", 503, 429)
	if _, err := e.client.GetRegion(ctx, proj, region); err != nil {
		t.Fatalf("get after retries: %v", err)
	}
	if n := len(e.requests("GET /compute/v1/projects/host-project/regions/us-central1")); n != 3 {
		t.Fatalf("sent %d times, want 3", n)
	}

	// A mutation is retried with its body and the same requestId, so
	// GCE runs it once.
	e.failNext("POST /compute/v1/projects/host-project/global/networks", 500, 502)
	op := network(t, e, "retried")
	reqs := e.requests("POST /compute/v1/projects/host-project/global/networks")
	if len(reqs) != 3 {
		t.Fatalf("insert sent %d times, want 3", len(reqs))
	}
	id := reqs[0].URL.Query().Get("requestId")
	if len(id) != 36 {
		t.Fatalf("requestId %q", id)
	}
	for _, r := range reqs {
		if r.URL.Query().Get("requestId") != id {
			t.Fatalf("requestIds differ: %q, %q", id, r.URL.Query().Get("requestId"))
		}
	}
	if n, err := e.client.GetNetwork(ctx, proj, "retried"); err != nil || n.Name != "retried" || !strings.HasSuffix(op.TargetLink, "/retried") {
		t.Fatalf("network after retried insert: %+v %v", n, err)
	}
	// Each mutation has its own requestId; waits and reads have none.
	network(t, e, "second")
	reqs = e.requests("POST /compute/v1/projects/host-project/global/networks")
	if reqs[len(reqs)-1].URL.Query().Get("requestId") == id {
		t.Fatal("two inserts shared a requestId")
	}
	for _, r := range e.requests("") {
		if (r.Method == http.MethodGet || strings.HasSuffix(r.URL.Path, "/wait")) && r.URL.Query().Has("requestId") {
			t.Fatalf("%s %s carries a requestId", r.Method, r.URL.Path)
		}
	}

	// Cloud Quotas is retried too.
	e.failNext("GET /v1/projects/host-project/locations/global/services/compute.googleapis.com/quotaInfos", 503)
	if _, err := e.client.ListComputeQuotaInfos(ctx, proj); err != nil {
		t.Fatalf("quota infos after a retry: %v", err)
	}
}

func TestRetriesGiveUp(t *testing.T) {
	e := newEnv(t, gceclient.Options{Attempts: 3})
	e.failNext("GET /compute/v1/projects/host-project/zones/us-central1-a", 503, 503, 503)
	_, err := e.client.GetZone(ctx, proj, zone)
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 503 || gce.Reason(err) != "rateLimitExceeded" {
		t.Fatalf("after giving up: %v", err)
	}
	if n := len(e.requests("GET /compute/v1/projects/host-project/zones/us-central1-a")); n != 3 {
		t.Fatalf("sent %d times, want 3", n)
	}

	// A refusal that isn't 429 or 5xx is GCE's answer, not retried.
	_, err = e.client.GetInstance(ctx, proj, zone, "nope")
	if gce.Code(err) != http.StatusNotFound || gce.Reason(err) != gce.ReasonNotFound {
		t.Fatalf("not found: %v", err)
	}
	if n := len(e.requests("GET /compute/v1/projects/host-project/zones/us-central1-a/instances/nope")); n != 1 {
		t.Fatalf("404 sent %d times", n)
	}

	// A cancelled context stops the retries.
	e.failNext("GET /compute/v1/projects/host-project/zones/us-central1-b", 503, 503, 503)
	c, err := gceclient.New(ctx, gceclient.Options{ComputeEndpoint: e.client.ComputeEndpointForTest(), HTTPClient: http.DefaultClient,
		Attempts: 10, Backoff: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.GetZone(cctx, proj, "us-central1-b"); err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cancelled retry: %v after %v", err, time.Since(start))
	}
}

func TestRetryAfter(t *testing.T) {
	e := newEnv(t, gceclient.Options{Backoff: time.Hour})
	e.header = http.Header{"Retry-After": {"1"}}
	e.failNext("GET /compute/v1/projects/host-project/regions", 429)
	start := time.Now()
	if _, err := e.client.ListRegions(ctx, proj); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 30*time.Second {
		t.Fatalf("waited %v, want Retry-After's second", d)
	}
}

// TestErrorShape checks that GCE's refusal reaches the caller whole:
// code, message and every error item's reason.
func TestErrorShape(t *testing.T) {
	e := newEnv(t, gceclient.Options{})
	network(t, e, "n")
	_, err := e.client.InsertNetwork(ctx, proj, &compute.Network{Name: "n"})
	direct := func() error {
		_, err := e.fake.InsertNetwork(ctx, proj, &compute.Network{Name: "n"})
		return err
	}()
	var got, want *googleapi.Error
	if !errors.As(err, &got) || !errors.As(direct, &want) {
		t.Fatalf("errors %T %v / %T %v", err, err, direct, direct)
	}
	if got.Code != want.Code || got.Message != want.Message || len(got.Errors) != 1 ||
		got.Errors[0].Reason != want.Errors[0].Reason || got.Errors[0].Message != want.Errors[0].Message {
		t.Fatalf("through the wire %+v, in process %+v", got, want)
	}
}
