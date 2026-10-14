package tunnel

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func get(t *testing.T, url string) string {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func connect(t *testing.T, ctx context.Context, control net.Listener, target string) *Client {
	t.Helper()
	conn, err := net.Dial("tcp", control.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Connect(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve(target)
	return c
}

// TestTunnel carries HTTP requests made to the sidecar's gateway address
// to a server on silo's side, across a reconnect.
func TestTunnel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, "gateway saw "+r.URL.Path+" "+string(b))
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw, control := listen(t), listen(t)
	sc := &Sidecar{Wait: 5 * time.Second, Logf: t.Logf}
	go sc.Serve(ctx, gw, control)

	c := connect(t, ctx, control, target)
	url := "http://" + gw.Addr().String() + "/mcp"
	for i := 0; i < 3; i++ {
		if got := get(t, url); got != "gateway saw /mcp " {
			t.Fatalf("got %q", got)
		}
	}
	big := strings.Repeat("x", 3<<20)
	resp, err := http.Post(url, "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "gateway saw /mcp "+big {
		t.Fatalf("large body came back %d bytes", len(b))
	}

	// silo's port-forward drops; a new control connection takes over.
	c.Close()
	<-c.Done()
	connect(t, ctx, control, target)
	if got := get(t, url); got != "gateway saw /mcp " {
		t.Fatalf("after reconnect got %q", got)
	}
}

// TestTunnelRefusesStranger drops a control connection that does not
// say hello, and a gateway connection when silo is not connected.
func TestTunnelRefusesStranger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw, control := listen(t), listen(t)
	sc := &Sidecar{Wait: 200 * time.Millisecond}
	go sc.Serve(ctx, gw, control)

	conn, err := net.Dial("tcp", control.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET / HTTP/1.0\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := conn.Read(make([]byte, 16)); n != 0 {
		t.Fatalf("a stranger got an answer")
	}
	g, err := net.Dial("tcp", gw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	g.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := g.Read(make([]byte, 1)); err == nil {
		t.Fatalf("a gateway connection with no silo was not dropped")
	}
}
