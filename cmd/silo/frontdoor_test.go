package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	facade "github.com/bwsalmon/silo/pkg/silo/facade/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/frontdoor"
	"github.com/bwsalmon/silo/pkg/silo/frontdoor/frontdoortest"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

const fdProject = "host-project"

// door is silod's front door in-process, standing in for the control
// VM's port 8443, with a tunnel stub that "opens" straight to it and a
// minter that signs tokens for email with a key the front door trusts.
type door struct {
	t       *testing.T
	addr    string // the front door's, where the stubbed tunnel goes
	signer  *frontdoortest.Signer
	a       string // the silo alice@example.com is mapped to
	email   string // whose token the minter mints
	opened  atomic.Int32
	stopped atomic.Int32
	minted  atomic.Int32
	target  tunnelTarget
}

func newDoor(t *testing.T) *door {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	gce := fake.New(fake.Options{Project: fdProject})
	if _, err := gce.InsertNetwork(ctx, fdProject, &compute.Network{Name: "silo"}); err != nil {
		t.Fatal(err)
	}
	api := fenceapi.New(db, fenceapi.Options{})
	fc := facade.New(db, facade.Options{GCE: gce, HostProject: fdProject})
	mux := http.NewServeMux()
	mux.Handle("/compute/v1/", fc)
	mux.Handle("/", api)
	d := &door{t: t, signer: frontdoortest.New(t), email: "alice@example.com"}
	srv := httptest.NewServer(frontdoor.Handler(db, frontdoor.Options{Verifier: d.signer.Verifier(frontdoor.Audience(fdProject)), API: mux}))
	t.Cleanup(func() { srv.Close(); fc.Close(); api.Close() })
	d.addr = strings.TrimPrefix(srv.URL, "http://")

	// The scene, set as silod's own caller: a root, and /a with alice
	// mapped to it.
	as := func(caller, method, path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(b))
		if caller != "" {
			r = r.WithContext(fenceapi.ContextWithCaller(r.Context(), fenceapi.Caller{Silo: caller, Via: fenceapi.ViaInternal}))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		out := map[string]any{}
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %v", method, path, w.Code, out)
		}
		return out
	}
	root := as("", "POST", "/silod/bootstrap", map[string]any{
		"regions":  []string{"us-central1"},
		"capacity": map[string]any{"us-central1": map[string]any{"vcpu": 100}, "global": map[string]any{"subnetworks": 20}},
	})["id"].(string)
	d.a = as(root, "POST", "/silo/v1/silos/-"+root+"/children", map[string]any{
		"name": "a", "address": map[string]any{"prefix_length": 16}, "regions": []string{"us-central1"},
	})["id"].(string)
	as(root, "PUT", "/silo/v1/silos/a/members/user:alice@example.com", map[string]any{})
	return d
}

func (d *door) tunnel(_ context.Context, t tunnelTarget) (string, func(), error) {
	d.opened.Add(1)
	d.target = t
	return d.addr, func() { d.stopped.Add(1) }, nil
}

func (d *door) mint(_ context.Context, audience string) (string, error) {
	d.minted.Add(1)
	if audience != "silo:"+fdProject {
		d.t.Errorf("minted for audience %q, want silo:%s", audience, fdProject)
	}
	return d.signer.Token(d.email, audience), nil
}

// noNetwork is a transport for the in-cloud path that must not be
// taken.
type noNetwork struct{ used *atomic.Int32 }

func (n noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.used.Add(1)
	return nil, errors.New("the in-cloud path was taken")
}

// cli is a silo invocation wired to d, with env as its environment and
// resolveErr what looking up silo.internal says.
func (d *door) cli(env map[string]string, resolveErr error, inCloud *atomic.Int32) (*cli, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cli{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] },
		transport: noNetwork{inCloud}, tunnel: d.tunnel, mint: d.mint,
		resolve: func(context.Context, string) error { return resolveErr },
	}, &out, &errb
}

func TestCommandsThroughTheFrontDoor(t *testing.T) {
	d := newDoor(t)
	notFound := errors.New("no such host")
	for _, tc := range []struct {
		name       string
		env        map[string]string
		args       []string
		resolveErr error
		wantDoor   bool
	}{
		{name: "asked", args: []string{"--front-door", "--project", fdProject, "--zone", "us-central1-a"}, resolveErr: nil, wantDoor: true},
		{name: "auto-detected outside the cloud", env: map[string]string{"SILO_PROJECT": fdProject}, resolveErr: notFound, wantDoor: true},
		{name: "inside the cloud", env: map[string]string{"SILO_PROJECT": fdProject}, resolveErr: nil, wantDoor: false},
		{name: "an endpoint chosen", env: map[string]string{"SILO_PROJECT": fdProject, "SILO_ENDPOINT": "http://localhost:1/silo/v1/"}, resolveErr: notFound, wantDoor: false},
		{name: "no project known", resolveErr: notFound, wantDoor: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.opened.Store(0)
			d.stopped.Store(0)
			var inCloud atomic.Int32
			c, out, errb := d.cli(tc.env, tc.resolveErr, &inCloud)
			code := c.run(context.Background(), append([]string{"describe", "--output", "json"}, tc.args...))
			if !tc.wantDoor {
				if d.opened.Load() != 0 || inCloud.Load() == 0 {
					t.Errorf("went through the front door (%d tunnels, %d in-cloud calls), want the in-cloud path", d.opened.Load(), inCloud.Load())
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			var s map[string]any
			if err := json.Unmarshal(out.Bytes(), &s); err != nil || s["id"] != d.a {
				t.Errorf("describe: %s, want alice's silo /a (%s)", out, d.a)
			}
			if d.opened.Load() != 1 || d.stopped.Load() != 1 || inCloud.Load() != 0 {
				t.Errorf("%d tunnels opened, %d closed, %d in-cloud calls; want one tunnel, closed, and no in-cloud call", d.opened.Load(), d.stopped.Load(), inCloud.Load())
			}
			if d.target.Project != fdProject || d.target.Instance != "silo-control" {
				t.Errorf("tunnel to %+v", d.target)
			}
		})
	}
}

func TestFrontDoorNeedsAProject(t *testing.T) {
	d := newDoor(t)
	var inCloud atomic.Int32
	c, _, errb := d.cli(nil, nil, &inCloud)
	if code := c.run(context.Background(), []string{"describe", "--front-door"}); code != 2 || !strings.Contains(errb.String(), "--project") {
		t.Errorf("exit %d, stderr %s; want 2 naming --project", code, errb)
	}
}

func TestUnmappedCallerThroughTheFrontDoor(t *testing.T) {
	d := newDoor(t)
	d.email = "mallory@example.com"
	var inCloud atomic.Int32
	c, out, errb := d.cli(nil, nil, &inCloud)
	if code := c.run(context.Background(), []string{"describe", "--front-door", "--project", fdProject}); code != 1 ||
		!strings.Contains(errb.String(), "not a member of any silo") {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s\nwant 1, not a member", code, out, errb)
	}
}

// startProxy runs silo proxy against d and returns its address; it
// stops at the end of the test.
func startProxy(t *testing.T, d *door) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var inCloud atomic.Int32
	c, out, errb := d.cli(nil, nil, &inCloud)
	ready := make(chan string, 1)
	c.proxyReady = func(addr string) { ready <- addr }
	done := make(chan int, 1)
	go func() { done <- c.run(ctx, []string{"proxy", "--listen", "127.0.0.1:0", "--project", fdProject}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("silo proxy: exit %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
		case <-time.After(10 * time.Second):
			t.Error("silo proxy didn't stop")
		}
		if d.stopped.Load() != 1 {
			t.Errorf("tunnel closed %d times, want once", d.stopped.Load())
		}
	})
	select {
	case addr := <-ready:
		return addr
	case code := <-done:
		t.Fatalf("silo proxy: exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	case <-time.After(10 * time.Second):
		t.Fatal("silo proxy never served")
	}
	return ""
}

func get(t *testing.T, rawurl string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", rawurl, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestProxy(t *testing.T) {
	d := newDoor(t)
	addr := startProxy(t, d)
	base := "http://" + addr

	// The tunnel opened at start, and a token was minted to check the
	// credentials work.
	if d.opened.Load() != 1 || d.minted.Load() != 1 {
		t.Errorf("at start: %d tunnels, %d tokens; want 1 and 1", d.opened.Load(), d.minted.Load())
	}
	// gcloud and terraform send their own OAuth token; the proxy puts
	// alice's ID token in its place.
	code, s := get(t, base+"/silo/v1/silos/self", "Authorization", "Bearer ya29.gcloud-access-token")
	if code != 200 || s["id"] != d.a {
		t.Errorf("fence API through the proxy: %d %v, want /a", code, s)
	}
	code, out := get(t, base+"/compute/v1/projects/"+fdProject+"/regions/us-central1/subnetworks")
	if code != 200 || out["kind"] != "compute#subnetworkList" {
		t.Errorf("facade through the proxy: %d %v", code, out)
	}
	// The token is minted once and reused while it is good.
	if d.minted.Load() != 1 {
		t.Errorf("%d tokens minted, want 1 reused", d.minted.Load())
	}
	// The silo CLI works through it as an ordinary endpoint.
	var inCloud atomic.Int32
	c, stdout, errb := d.cli(map[string]string{"SILO_ENDPOINT": base + "/silo/v1/"}, nil, &inCloud)
	c.transport = nil
	if code := c.run(context.Background(), []string{"describe", "--output", "json"}); code != 0 || !strings.Contains(stdout.String(), d.a) {
		t.Errorf("silo describe through the proxy: exit %d\nstdout: %s\nstderr: %s", code, stdout, errb)
	}
	// What a web page could send is refused before it is signed.
	for _, h := range [][]string{
		{"Origin", "https://evil.example.com"},
		{"Sec-Fetch-Site", "cross-site"},
		{"Host", "evil.example.com:" + strings.Split(addr, ":")[1]},
		{"Host", "localhost:1"},
	} {
		if code, out := get(t, base+"/silo/v1/silos/self", h...); code != 403 {
			t.Errorf("with %s: %s: %d %v, want 403", h[0], h[1], code, out)
		}
	}
	if code, _ := get(t, base+"/silo/v1/silos/self", "Host", "localhost:"+strings.Split(addr, ":")[1]); code != 200 {
		t.Errorf("Host localhost: %d, want 200", code)
	}
}

func TestProxyListensOnlyOnLoopback(t *testing.T) {
	d := newDoor(t)
	for _, l := range []string{"0.0.0.0:0", ":0", "192.0.2.1:8642"} {
		var inCloud atomic.Int32
		c, _, errb := d.cli(nil, nil, &inCloud)
		if code := c.run(context.Background(), []string{"proxy", "--listen", l, "--project", fdProject}); code != 2 || !strings.Contains(errb.String(), "loopback") {
			t.Errorf("--listen %s: exit %d, stderr %s; want 2 naming loopback", l, code, errb)
		}
	}
	if d.opened.Load() != 0 {
		t.Errorf("a tunnel was opened for a refused --listen")
	}
}

func TestProxyFailsFastWithoutATunnel(t *testing.T) {
	d := newDoor(t)
	var inCloud atomic.Int32
	c, _, errb := d.cli(nil, nil, &inCloud)
	c.tunnel = func(context.Context, tunnelTarget) (string, func(), error) {
		return "", nil, errors.New("Error while connecting [4033: 'not authorized']")
	}
	if code := c.run(context.Background(), []string{"proxy", "--listen", "127.0.0.1:0", "--project", fdProject}); code != 1 ||
		!strings.Contains(errb.String(), "IAP tunnel") || !strings.Contains(errb.String(), "4033") {
		t.Errorf("exit %d, stderr %s; want 1 naming the tunnel's error", code, errb)
	}
}

func TestExpiry(t *testing.T) {
	s := frontdoortest.New(t)
	c := frontdoortest.Valid("a@example.com", "x")
	c["exp"] = int64(2000000000)
	if got := expiry(s.Sign(c)); !got.Equal(time.Unix(2000000000, 0)) {
		t.Errorf("expiry = %v", got)
	}
	if got := expiry("junk"); !got.IsZero() {
		t.Errorf("expiry(junk) = %v, want zero", got)
	}
}
