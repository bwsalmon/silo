package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/frontdoor"
	"github.com/bwsalmon/silo/pkg/silo/install"
	"google.golang.org/api/idtoken"
)

// This file is the front door's client half (docs/silo/gateway.md,
// "v1: IAP TCP forwarding plus an ID token", SV-88): an IAP TCP tunnel
// to the control VM's port 8443, with a Google ID token on every
// request. silo proxy serves it on localhost; every other command run
// outside the cloud uses it directly.

// DefaultProxyListen is silo proxy's default --listen.
const DefaultProxyListen = "localhost:8642"

// tunnelTarget is where a front-door tunnel goes.
type tunnelTarget struct {
	Project, Zone, Instance string
}

// tunneler opens a TCP tunnel to the target's port frontdoor.Port and
// returns the local address it listens on, and a function that closes
// it. gcloudTunnel is the real one; tests stub it.
type tunneler func(ctx context.Context, t tunnelTarget) (addr string, stop func(), err error)

// minter mints a Google ID token for audience from the user's
// credentials. mintIDToken is the real one; tests stub it.
type minter func(ctx context.Context, audience string) (string, error)

// frontDoorFlags are the flags that say how to reach the front door.
type frontDoorFlags struct {
	use                     bool
	project, zone, instance string
}

func (c *cli) frontDoorFlags(fs *flag.FlagSet, f *frontDoorFlags, withUse bool) {
	project := c.getenv("SILO_PROJECT")
	if project == "" {
		project = c.getenv("CLOUDSDK_CORE_PROJECT")
	}
	if withUse {
		fs.BoolVar(&f.use, "front-door", false, "go through the front door (an IAP tunnel to the control VM) even if silo.internal resolves; the default outside the cloud when the project is known")
	}
	fs.StringVar(&f.project, "project", project, "the host project, for the front door (default $SILO_PROJECT, or $CLOUDSDK_CORE_PROJECT)")
	fs.StringVar(&f.zone, "zone", c.getenv("SILO_ZONE"), "the control VM's zone, for the front door (default $SILO_ZONE, or looked up)")
	fs.StringVar(&f.instance, "instance", install.ControlVM, "the control VM, for the front door")
}

// newFrontDoor is the front door f names.
func (c *cli) newFrontDoor(f *frontDoorFlags) (*frontDoor, error) {
	if f.project == "" {
		return nil, usagef("the front door needs --project (or $SILO_PROJECT): the host project Silo is installed in")
	}
	tunnel, mint := c.tunnel, c.mint
	if tunnel == nil {
		tunnel = gcloudTunnel
	}
	if mint == nil {
		mint = mintIDToken
	}
	return &frontDoor{
		target:   tunnelTarget{Project: f.project, Zone: f.zone, Instance: f.instance},
		audience: frontdoor.Audience(f.project),
		tunnel:   tunnel,
		mint:     mint,
	}, nil
}

// frontDoor is an http.RoundTripper that sends every request, whatever
// its URL's host, down a tunnel to the front door, with a fresh ID
// token in place of whatever Authorization it carried. The tunnel opens
// on the first request, or on open, and stays up until Close.
type frontDoor struct {
	target   tunnelTarget
	audience string
	tunnel   tunneler
	mint     minter

	once sync.Once
	addr string
	stop func()
	err  error

	mu  sync.Mutex
	tok string
	exp time.Time
}

// open opens the tunnel, once.
func (f *frontDoor) open(ctx context.Context) error {
	f.once.Do(func() {
		// The tunnel outlives the call that opened it.
		f.addr, f.stop, f.err = f.tunnel(context.WithoutCancel(ctx), f.target)
		if f.err != nil {
			f.err = fmt.Errorf("opening the IAP tunnel to %s port %d in %s: %w", f.target.Instance, frontdoor.Port, f.target.Project, f.err)
		}
	})
	return f.err
}

// Close closes the tunnel.
func (f *frontDoor) Close() {
	f.once.Do(func() { f.err = errors.New("the front door is closed") })
	if f.stop != nil {
		f.stop()
	}
}

// token is an ID token good for at least another minute.
func (f *frontDoor) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tok != "" && time.Until(f.exp) > time.Minute {
		return f.tok, nil
	}
	tok, err := f.mint(ctx, f.audience)
	if err != nil {
		return "", fmt.Errorf("minting an ID token for %s: %w", f.audience, err)
	}
	f.tok, f.exp = tok, expiry(tok)
	return tok, nil
}

func (f *frontDoor) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := f.open(r.Context()); err != nil {
		return nil, err
	}
	tok, err := f.token(r.Context())
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = "http", f.addr, ""
	r.Header.Set("Authorization", "Bearer "+tok)
	return http.DefaultTransport.RoundTrip(r)
}

// expiry is an ID token's exp, read without verifying it (the server
// does that); the zero time if it can't be read, so that the next call
// mints another.
func expiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

// useFrontDoor reports whether a command should go through the front
// door: when asked to, or, when nobody chose an endpoint, the project
// is known and silo.internal doesn't resolve (we are outside the
// cloud).
func (c *cli) useFrontDoor(ctx context.Context, f *frontDoorFlags, endpointChosen bool) bool {
	if f.use {
		return true
	}
	if endpointChosen || f.project == "" {
		return false
	}
	resolve := c.resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) error {
			_, err := net.DefaultResolver.LookupHost(ctx, host)
			return err
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return resolve(rctx, "silo.internal") != nil
}

// proxy is silo proxy: the front door, served on localhost without
// credentials, so that gcloud and terraform work from a laptop.
func (c *cli) proxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("silo proxy", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	listen := fs.String("listen", DefaultProxyListen, "the loopback address to serve the facade and fence API on")
	var f frontDoorFlags
	c.frontDoorFlags(fs, &f, false)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	fd, err := c.newFrontDoor(&f)
	if err != nil {
		return err
	}
	defer fd.Close()
	l, err := listenLoopback(*listen)
	if err != nil {
		return usageError{err.Error()}
	}
	defer l.Close()
	// Fail now, not on the first call, if the tunnel or the token can't
	// be had.
	if err := fd.open(ctx); err != nil {
		return err
	}
	if _, err := fd.token(ctx); err != nil {
		return err
	}
	srv := &http.Server{Handler: proxyHandler(l.Addr().String(), fd), ReadHeaderTimeout: 10 * time.Second}
	base := "http://" + l.Addr().String()
	fmt.Fprintf(c.stdout, "silo: serving the front door of %s on %s\n", f.project, base)
	fmt.Fprintf(c.stdout, "  silo:      SILO_ENDPOINT=%s/silo/v1/\n", base)
	fmt.Fprintf(c.stdout, "  gcloud:    gcloud config set api_endpoint_overrides/compute %s/compute/v1/\n", base)
	fmt.Fprintf(c.stdout, "  terraform: provider \"google\" { compute_custom_endpoint = \"%s/compute/v1/\" }\n", base)
	if c.proxyReady != nil {
		c.proxyReady(l.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// listenLoopback listens on addr, which must be a loopback address:
// whoever reaches the proxy acts as the user whose credentials it
// mints tokens from.
func listenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %v", addr, err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("--listen %q: want a loopback address (localhost, 127.0.0.1 or [::1]); anyone who can reach the proxy acts as you", addr)
		}
	}
	return net.Listen("tcp", addr)
}

// proxyHandler forwards to the front door through fd. It refuses what
// a web page could send: a request from a browser (it carries Origin,
// or Sec-Fetch-Site), or naming a host other than the proxy's own (DNS
// rebinding), since a page can reach localhost and the proxy would sign
// its request as the user.
func proxyHandler(self string, fd *frontDoor) http.Handler {
	_, port, _ := net.SplitHostPort(self)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "silo-front-door"
			pr.Out.Header.Del("Authorization") // gcloud's own token: fd puts the ID token in its place
		},
		Transport: fd,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyRefuse(w, http.StatusBadGateway, "silo proxy: "+err.Error())
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			proxyRefuse(w, http.StatusForbidden, "silo proxy: refusing a request from a browser")
			return
		}
		host, p, err := net.SplitHostPort(r.Host)
		if err != nil || p != port || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			proxyRefuse(w, http.StatusForbidden, fmt.Sprintf("silo proxy: refusing a request for host %q", r.Host))
			return
		}
		rp.ServeHTTP(w, r)
	})
}

func proxyRefuse(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// mintIDToken mints an ID token for audience from Application Default
// Credentials (a service account key, the metadata server, or an
// impersonated or external account), and failing that from gcloud's
// active account.
//
// Google mints a token with an audience of the caller's choosing only
// for a service account, so a person's own gcloud login can't: they
// impersonate a service account that is mapped to their silo
// (gcloud config set auth/impersonate_service_account ...).
func mintIDToken(ctx context.Context, audience string) (string, error) {
	ts, adcErr := idtoken.NewTokenSource(ctx, audience)
	if adcErr == nil {
		t, err := ts.Token()
		if err == nil {
			return t.AccessToken, nil
		}
		adcErr = err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-identity-token", "--audiences="+audience, "--include-email")
	cmd.Env = gcloudEnv()
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("Application Default Credentials: %v; gcloud: %v %s\n"+
		"Google mints ID tokens with a custom audience only for service accounts: use one, or impersonate one that is a member of your silo "+
		"(gcloud config set auth/impersonate_service_account <email>)", adcErr, err, strings.TrimSpace(stderr.String()))
}

// gcloudEnv is gcloud's environment: the user's own, deliberately.
// gcloud is the user's tool acting with the user's credentials, and
// finds its configuration and account through HOME, PATH and
// CLOUDSDK_*; silo is a CLI the user runs, not a daemon holding
// secrets of its own.
func gcloudEnv() []string { return os.Environ() }

// gcloudTunnel opens the tunnel with gcloud compute start-iap-tunnel,
// on a free local port, and waits until it accepts connections.
func gcloudTunnel(ctx context.Context, t tunnelTarget) (string, func(), error) {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return "", nil, errors.New("the front door's tunnel needs gcloud on $PATH")
	}
	zone := t.Zone
	if zone == "" {
		list := exec.CommandContext(ctx, "gcloud", "compute", "instances", "list", "--project", t.Project,
			"--filter", "name=("+t.Instance+")", "--format", "value(zone.basename())")
		list.Env = gcloudEnv()
		out, err := list.Output()
		zone, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
		if err != nil || zone == "" {
			return "", nil, fmt.Errorf("finding %s's zone (give --zone or $SILO_ZONE): %v", t.Instance, err)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	addr := l.Addr().String()
	l.Close()
	_, port, _ := net.SplitHostPort(addr)
	var stderr bytes.Buffer
	cmd := exec.Command("gcloud", "compute", "start-iap-tunnel", t.Instance, strconv.Itoa(frontdoor.Port),
		"--local-host-port=127.0.0.1:"+port, "--zone", zone, "--project", t.Project)
	cmd.Env = gcloudEnv()
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	stop := func() {
		cmd.Process.Kill()
		<-done
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			return addr, stop, nil
		}
		select {
		case <-done:
			return "", nil, fmt.Errorf("gcloud compute start-iap-tunnel exited: %s", strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			stop()
			return "", nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			stop()
			return "", nil, fmt.Errorf("the tunnel didn't open within a minute: %s", strings.TrimSpace(stderr.String()))
		}
	}
}
