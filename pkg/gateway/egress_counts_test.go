package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests hold the egress audit's bytes_out and bytes_in to what
// really crossed the upstream connection, for plain HTTP with bodies of
// several sizes, under concurrency, and for upstreams that answer
// without reading the body or hang up after the headers. They are the
// regression tests for 11dbb21e1 (bytes_out: 0 while the Transport's
// writer goroutine was still writing) and for the Wait-before-Add race
// that commit left: see countedConn.

// egressLoadIterations is how many requests each size and upstream
// gets. CI keeps it small; SILO_EGRESS_LOAD sets it for a load run
// (docs/test-runs/2026-10-07-silo-gateway-egress-counts.md used 200).
func egressLoadIterations(t *testing.T) int {
	if s := os.Getenv("SILO_EGRESS_LOAD"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("SILO_EGRESS_LOAD=%q", s)
		}
		return n
	}
	return 8
}

// countingUpstream is a raw HTTP/1.1 server that does what each
// request's Host asks of it, "<behaviour>-<size>-<n>.example.com", and
// records, by Host, every byte it read from the connection and wrote to
// it, until the connection ends.
//
// Behaviours:
//   - read: read the whole body, and answer 200 with a body of size bytes.
//   - reject: answer 413 at once without reading the body.
//   - hangup: answer 200 headers, promising a body, then close.
//
// For reject, it keeps reading after answering until the gateway hangs
// up, so that what it read is everything the gateway's writes got onto
// the wire.
type countingUpstream struct {
	l    net.Listener
	mu   sync.Mutex
	got  map[string]wireCount
	wait sync.WaitGroup
}

type wireCount struct{ read, wrote int64 }

func newCountingUpstream(t *testing.T) *countingUpstream {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &countingUpstream{l: l, got: map[string]wireCount{}}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u.wait.Add(1)
			go func() {
				defer u.wait.Done()
				u.serve(c)
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return u
}

type wireReader struct {
	r io.Reader
	n int64
}

func (c *wireReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

func (u *countingUpstream) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Minute))
	cr := &wireReader{r: c}
	br := bufio.NewReader(cr)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	behaviour, rest, _ := strings.Cut(req.Host, "-")
	size, _ := strconv.Atoi(strings.SplitN(rest, "-", 2)[0])
	var wrote int64
	write := func(s string) {
		n, _ := io.WriteString(c, s)
		wrote += int64(n)
	}
	switch behaviour {
	case "read":
		io.Copy(io.Discard, req.Body)
		write(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", size))
		n, _ := io.Copy(c, io.LimitReader(zeros{}, int64(size)))
		wrote += n
	case "reject":
		write("HTTP/1.1 413 Request Entity Too Large\r\nContent-Length: 9\r\nConnection: close\r\n\r\ntoo large")
	case "hangup":
		write("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nConnection: close\r\n\r\n")
		u.record(req.Host, cr.n, wrote)
		return
	}
	// Everything the gateway wrote, until it hangs up.
	io.Copy(io.Discard, cr)
	u.record(req.Host, cr.n, wrote)
}

func (u *countingUpstream) record(host string, read, wrote int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.got[host] = wireCount{read, wrote}
}

func (u *countingUpstream) count(host string) (wireCount, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	w, ok := u.got[host]
	return w, ok
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// proxyClient is a client sending through the egress proxy from silo
// a's VM.
func (e *env) proxyClient(tlsConfig *tls.Config) *http.Client {
	proxy, _ := url.Parse("http://" + e.proxy)
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		Proxy:               http.ProxyURL(proxy),
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dialFrom(ctx, e.proxy, e.ip[e.a]) },
		TLSClientConfig:     tlsConfig,
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: -1,
	}}
}

func TestEgressPlainHTTPByteCounts(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.example.com"}}})
	e.sync()
	up := newCountingUpstream(t)
	e.upstream.route(80, up.l.Addr().String())
	c := e.proxyClient(nil)
	iterations := egressLoadIterations(t)
	sizes := []int{0, 1 << 10, 64 << 10, 8 << 20}

	for _, behaviour := range []string{"read", "reject", "hangup"} {
		t.Run(behaviour, func(t *testing.T) {
			var hosts []string
			bodies := map[string]int{}
			for _, size := range sizes {
				for i := range iterations {
					h := fmt.Sprintf("%s-%d-%d.example.com", behaviour, size, i)
					hosts = append(hosts, h)
					bodies[h] = size
				}
			}
			var next atomic.Int64
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						i := int(next.Add(1)) - 1
						if i >= len(hosts) {
							return
						}
						h := hosts[i]
						req, _ := http.NewRequest(http.MethodPost, "http://"+h+"/upload", io.LimitReader(zeros{}, int64(bodies[h])))
						req.ContentLength = int64(bodies[h])
						resp, err := c.Do(req)
						if err != nil {
							if behaviour == "read" {
								t.Errorf("POST %s: %v", h, err)
							}
							continue
						}
						n, err := io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if behaviour == "read" && (err != nil || resp.StatusCode != 200 || n != int64(bodies[h])) {
							t.Errorf("POST %s: %s, %d bytes back, %v", h, resp.Status, n, err)
						}
					}
				}()
			}
			wg.Wait()

			// Every request is audited exactly once, and its counts are
			// no less than what crossed the wire.
			want := map[string]bool{}
			for _, h := range hosts {
				want[h] = true
			}
			deadline := time.Now().Add(30 * time.Second)
			var recs map[string][]obj
			for {
				recs = map[string][]obj{}
				for _, r := range e.egressAudit() {
					if h, _ := r["host"].(string); want[h] {
						recs[h] = append(recs[h], r)
					}
				}
				if len(recs) == len(hosts) || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			for _, h := range hosts {
				rs := recs[h]
				if len(rs) != 1 {
					t.Errorf("%s: audited %d times", h, len(rs))
					continue
				}
				r := rs[0]
				out, in := int64(r["bytes_out"].(float64)), int64(r["bytes_in"].(float64))
				var w wireCount
				for {
					var ok bool
					if w, ok = up.count(h); ok || time.Now().After(deadline) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				requestLine := int64(len("POST /upload HTTP/1.1\r\n"))
				if out < w.read || out < requestLine {
					t.Errorf("%s: bytes_out %d, but the upstream read %d", h, out, w.read)
				}
				if behaviour == "read" && out < requestLine+int64(bodies[h]) {
					t.Errorf("%s: bytes_out %d, under the %d-byte body and its request line", h, out, bodies[h])
				}
				// The gateway reads the whole response but for hangup's,
				// whose body never comes.
				if behaviour != "hangup" && in != w.wrote {
					t.Errorf("%s: bytes_in %d, but the upstream wrote %d", h, in, w.wrote)
				}
				if behaviour == "read" && in < int64(bodies[h]) {
					t.Errorf("%s: bytes_in %d, under the %d-byte response body", h, in, bodies[h])
				}
				// An upstream that closes with the body unread resets the
				// connection, and the reset can discard its headers
				// before the gateway reads them, so 0 is right too.
				if behaviour == "hangup" && (in < 0 || in > w.wrote) {
					t.Errorf("%s: bytes_in %d, but the upstream wrote %d", h, in, w.wrote)
				}
			}
		})
	}
}

// TestEgressConnectByteCounts sends 8 MiB each way through a CONNECT
// tunnel to a TLS server, and holds the audit's counts to at least that.
func TestEgressConnectByteCounts(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.example.com"}}})
	e.sync()
	const size = 8 << 20
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != size {
			http.Error(w, fmt.Sprintf("read %d: %v", n, err), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		io.Copy(w, io.LimitReader(zeros{}, size))
	}))
	t.Cleanup(ts.Close)
	e.upstream.route(443, ts.Listener.Addr().String())
	cfg := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.ServerName = "example.com"
	c := e.proxyClient(cfg)

	req, _ := http.NewRequest(http.MethodPost, "https://tls.example.com/", bytes.NewReader(make([]byte, size)))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || n != size {
		t.Fatalf("POST through CONNECT: %s, %d bytes back, %v", resp.Status, n, err)
	}
	r := e.waitAudit(func(r obj) bool { return r["host"] == "tls.example.com" && r["method"] == "CONNECT" })
	if out, in := r["bytes_out"].(float64), r["bytes_in"].(float64); r["allowed"] != true || out < size || in < size {
		t.Errorf("an 8 MiB each way tunnel audited as %v", r)
	}
}

// gatedConn is a net.Conn whose Write says when it has begun, and
// blocks until released.
type gatedConn struct {
	net.Conn
	entered, release chan struct{}
	wrote            atomic.Int64
}

func (g *gatedConn) Write(b []byte) (int, error) {
	g.entered <- struct{}{}
	<-g.release
	g.wrote.Add(int64(len(b)))
	return len(b), nil
}

// TestCountedConnFinish pins countedConn.finish deterministically: it
// waits for a Write in flight and counts it, and a Write that had not
// begun when it ran is kept off the wire rather than written uncounted.
// (With the WaitGroup it replaces, the second Write was an Add from
// zero after Wait: under -race, a report, and otherwise bytes on the
// wire that the audit had already been written without.)
func TestCountedConnFinish(t *testing.T) {
	g := &gatedConn{entered: make(chan struct{}), release: make(chan struct{})}
	ec := &egressConn{}
	cc := &countedConn{Conn: g, ec: ec}

	wrote := make(chan struct{})
	go func() {
		cc.Write([]byte("hello"))
		close(wrote)
	}()
	<-g.entered
	finished := make(chan struct{})
	go func() {
		cc.finish()
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("finish returned with a Write in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	<-wrote
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("finish didn't return once the Write did")
	}
	if got := ec.out.Load(); got != 5 {
		t.Errorf("counted %d bytes out, want 5", got)
	}

	// The writer goroutine's next Write, begun only now.
	if n, err := cc.Write([]byte("world")); n != 0 || err == nil {
		t.Errorf("a Write after finish: %d, %v; want 0 and an error", n, err)
	}
	if got, wire := ec.out.Load(), g.wrote.Load(); got != 5 || wire != 5 {
		t.Errorf("after finish, counted %d bytes and wrote %d; want 5 and 5", got, wire)
	}
}
