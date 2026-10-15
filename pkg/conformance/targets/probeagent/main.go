// Command probeagent is what a local target's probe container runs
// (targets.Local's Fence): busybox has no UDP netcat and no curl, so
// this, built static and put in the probe's image beside busybox,
// serves and runs the cases' checks.
//
//	probeagent serve        TCP and UDP (echo) on every port in Ports,
//	                        and each line of stdin is a round: base64
//	                        of JSON []conformance.Check, run and
//	                        reported on stdout as ProbeScript's lines
//	probeagent run <b64>    runs one round
//
// The target sends a round by attaching to the container's stdin, and
// reads its report from the container's log: an exec into a sandbox
// container needs runsc exec, which a rootless install's shim can't
// run yet.
//
// Beyond ProbeScript's kinds, a check of kind "call" makes the HTTP
// request its Method, URL, Headers and Body say, with no proxy, and
// its detail is "<status> <base64 body>": a fence API or Docker call
// made from the probe, as its silo.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Ports are the ports a probe answers on, TCP and UDP: those the cases
// check a probe on. gVisor's netstack has no NAT to redirect every
// port to one, as the GCP probe's nftables does.
var Ports = []int{7000, 7001, 7002, 7003}

// check is conformance.Check's JSON, which this binary doesn't import
// so that it stays small.
type check struct {
	ID, Kind, Host string
	Port, Count    int

	Method, URL, Body, Proxy string
	Headers                  []string
	Script                   string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probeagent serve | run <base64 checks>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "run":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		if err := round(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("SILO-PROBE-DONE")
	default:
		os.Exit(2)
	}
}

func serve() {
	for _, p := range Ports {
		l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); io.Copy(c, c) }()
			}
		}()
		u, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", p))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			buf := make([]byte, 2048)
			for {
				n, from, err := u.ReadFrom(buf)
				if err != nil {
					return
				}
				u.WriteTo(buf[:n], from)
			}
		}()
	}
	fmt.Println("SILO-PROBE ready ok")
	// Each attach that ends closes stdin's writer; the next opens it
	// again, so an end of input is a pause, not the end.
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			if err := round(line); err != nil {
				fmt.Println("SILO-PROBE bad-round fail " + err.Error())
			}
		}
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			in.Reset(os.Stdin)
		}
	}
}

// round runs one round of checks, base64 JSON, and reports each.
func round(b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	var cs []check
	if err == nil {
		err = json.Unmarshal(raw, &cs)
	}
	if err != nil {
		return err
	}
	for _, c := range cs {
		ok, detail := run(c)
		word := "fail"
		if ok {
			word = "ok"
		}
		fmt.Printf("SILO-PROBE %s %s %s\n", c.ID, word, strings.ReplaceAll(detail, "\n", " "))
	}
	return nil
}

func run(c check) (bool, string) {
	hp := net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
	switch c.Kind {
	case "tcp":
		conn, err := net.DialTimeout("tcp4", hp, 5*time.Second)
		if err != nil {
			return false, err.Error()
		}
		conn.Close()
		return true, "connected"
	case "udp":
		// A UDP "connect" proves nothing on its own: ask for a reply.
		conn, err := net.DialTimeout("udp4", hp, 5*time.Second)
		if err != nil {
			return false, err.Error()
		}
		defer conn.Close()
		buf := make([]byte, 512)
		msg := []byte("probe\n")
		if c.Port == 53 {
			// A DNS server answers only a query: one for silo.internal.
			msg = []byte("\x51\x10\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x04silo\x08internal\x00\x00\x01\x00\x01")
		}
		for range 3 {
			conn.Write(msg)
			conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err = conn.Read(buf); err == nil {
				return true, "answered"
			}
		}
		return false, err.Error()
	case "icmp":
		return ping(c)
	case "dns":
		as, err := net.LookupHost(c.Host)
		if err != nil {
			return false, err.Error()
		}
		return true, strings.Join(as, " ")
	case "http":
		return httpCheck(c)
	case "call":
		return call(c)
	case "sh":
		out, err := exec.Command("sh", "-c", c.Script).CombinedOutput()
		if len(out) > 200 {
			out = out[:200]
		}
		return err == nil, string(out)
	}
	return false, "unsupported: kind " + c.Kind
}

// ping sends ICMP echoes from a ping socket, or a raw one where the
// sandbox has no ping sockets, and is ok on any reply.
func ping(c check) (bool, string) {
	n := c.Count
	if n == 0 {
		n = 2
	}
	dst, err := net.ResolveIPAddr("ip4", c.Host)
	if err != nil {
		return false, err.Error()
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	var to net.Addr = &net.UDPAddr{IP: dst.IP}
	if err != nil {
		var rerr error
		if conn, rerr = icmp.ListenPacket("ip4:icmp", "0.0.0.0"); rerr != nil {
			return false, fmt.Sprintf("unsupported: no ping socket (%v) and no raw socket (%v)", err, rerr)
		}
		to = dst
	}
	defer conn.Close()
	buf := make([]byte, 1500)
	for i := range n {
		msg, _ := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: i + 1, Data: []byte("silo-probe")}}).Marshal(nil)
		if _, err := conn.WriteTo(msg, to); err != nil {
			return false, err.Error()
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			k, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			if m, err := icmp.ParseMessage(1, buf[:k]); err == nil && m.Type == ipv4.ICMPTypeEchoReply {
				return true, "echo reply"
			}
		}
	}
	return false, fmt.Sprintf("no echo reply in %d tries", n)
}

// httpCheck is ProbeScript's curl: ok when the request got an answer at
// all, its detail the status and the head of the body. An https URL
// through a proxy is a CONNECT, whose answer is the proxy's verdict:
// the TLS that follows is the destination's, which the cases don't ask
// about.
func httpCheck(c check) (bool, string) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return false, err.Error()
	}
	if c.Proxy != "" && u.Scheme == "https" {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		conn, err := net.DialTimeout("tcp4", c.Proxy, 10*time.Second)
		if err != nil {
			return false, err.Error()
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(20 * time.Second))
		target := net.JoinHostPort(u.Hostname(), port)
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false, err.Error()
		}
		return true, fmt.Sprintf("%d CONNECT %s", res.StatusCode, res.Status)
	}
	tr := &http.Transport{}
	if c.Proxy != "" {
		tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: c.Proxy})
	}
	res, err := do(c, tr)
	if err != nil {
		return false, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 400))
	return true, fmt.Sprintf("%d %s", res.StatusCode, b)
}

func call(c check) (bool, string) {
	res, err := do(c, &http.Transport{})
	if err != nil {
		return false, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return true, fmt.Sprintf("%d %s", res.StatusCode, base64.StdEncoding.EncodeToString(b))
}

func do(c check, tr *http.Transport) (*http.Response, error) {
	m := c.Method
	if m == "" {
		m = "GET"
	}
	var body io.Reader
	if c.Body != "" {
		body = bytes.NewReader([]byte(c.Body))
	}
	req, err := http.NewRequest(m, c.URL, body)
	if err != nil {
		return nil, err
	}
	if c.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range c.Headers {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
}
