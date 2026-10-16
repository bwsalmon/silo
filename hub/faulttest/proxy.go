// Package faulttest is the hub's fault-injection and load harness
// (docs/test-runs): a real `silo hub serve` process on a real
// PostgreSQL, simulated Grains dialling it over the rendezvous, and a
// controllable proxy in front of each, so that a test can restart or
// kill the hub, partition a Grain, slow or black-hole its connection,
// or take the database away, and then check that no task was lost or
// run twice and that every task's seqs only went up.
//
// The scenarios are in fault_test.go and the load test in
// load_test.go. Both need PostgreSQL (internal/pgtest); the short fault
// scenarios run with every `go test`, the full matrix and the load test
// only when SILO_FAULT_FULL or SILO_LOAD is set (.grain/workflows/hub-nightly.yml).
package faulttest

import (
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// Proxy is a TCP proxy whose link can be faulted: cut, refused,
// black-holed (a half-open connection: nothing arrives and nothing
// says so), slowed or made lossy.
type Proxy struct {
	l      net.Listener
	target string

	mu        sync.Mutex
	cond      *sync.Cond
	conns     map[net.Conn]struct{}
	down      bool
	blackhole bool
	latency   time.Duration
	loss      float64
	closed    bool
}

// NewProxy listens on a free port of 127.0.0.1 and forwards each
// connection to target.
func NewProxy(target string) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{l: l, target: target, conns: map[net.Conn]struct{}{}}
	p.cond = sync.NewCond(&p.mu)
	go p.serve()
	return p, nil
}

// Addr is the address to dial instead of the target.
func (p *Proxy) Addr() string { return p.l.Addr().String() }

// Close stops the proxy and cuts its connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.Cut()
	p.cond.Broadcast()
	return p.l.Close()
}

// Cut resets every connection open through the proxy, as a peer's
// crash or a load balancer dropping its flows does.
func (p *Proxy) Cut() {
	p.mu.Lock()
	cs := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		cs = append(cs, c)
	}
	p.mu.Unlock()
	for _, c := range cs {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		c.Close()
	}
}

// Down cuts every connection and refuses new ones until Up: a
// partition both ends notice.
func (p *Proxy) Down() {
	p.mu.Lock()
	p.down = true
	p.mu.Unlock()
	p.Cut()
}

// Up ends Down and Blackhole.
func (p *Proxy) Up() {
	p.mu.Lock()
	p.down, p.blackhole = false, false
	p.mu.Unlock()
	p.cond.Broadcast()
}

// Blackhole stops forwarding without closing anything, on the
// connections open now and on new ones: a half-open connection, or a
// partition neither end is told about.
func (p *Proxy) Blackhole() {
	p.mu.Lock()
	p.blackhole = true
	p.mu.Unlock()
}

// Degrade delays every chunk forwarded by latency and, with probability
// loss, by a further 200ms: TCP hides a lost packet from both ends but
// for its retransmission's stall, which is what this reproduces.
func (p *Proxy) Degrade(latency time.Duration, loss float64) {
	p.mu.Lock()
	p.latency, p.loss = latency, loss
	p.mu.Unlock()
}

func (p *Proxy) serve() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.down || p.closed {
			p.mu.Unlock()
			c.Close()
			continue
		}
		p.mu.Unlock()
		go p.forward(c)
	}
}

func (p *Proxy) forward(c net.Conn) {
	u, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns[c], p.conns[u] = struct{}{}, struct{}{}
	p.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pipe(u, c) }()
	go func() { defer wg.Done(); p.pipe(c, u) }()
	wg.Wait()
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, u)
	p.mu.Unlock()
}

// pipe copies src to dst, through the faults, and closes both when
// either side ends.
func (p *Proxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			for p.blackhole && !p.closed {
				p.cond.Wait()
			}
			lat, loss := p.latency, p.loss
			p.mu.Unlock()
			if loss > 0 && rand.Float64() < loss {
				lat += 200 * time.Millisecond
			}
			if lat > 0 {
				time.Sleep(lat)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		// Neither the rendezvous nor PostgreSQL half-closes, so
		// either direction ending ends both; a black hole passes on no
		// end either, until it lifts.
		if err != nil {
			p.mu.Lock()
			for p.blackhole && !p.closed {
				p.cond.Wait()
			}
			p.mu.Unlock()
			return
		}
	}
}
