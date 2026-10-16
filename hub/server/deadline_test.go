package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBodyDeadline(t *testing.T) {
	const d = 200 * time.Millisecond
	ts := httptest.NewServer(bodyDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		// A response that outlasts the deadline, as a subscription does.
		time.Sleep(2 * d)
		io.WriteString(w, "ok")
	}), d))
	defer ts.Close()

	// A whole body in time: the response may take longer.
	resp, err := http.Post(ts.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "ok" {
		t.Errorf("a prompt body: %d %q", resp.StatusCode, b)
	}

	// A body that never finishes is cut off.
	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	io.WriteString(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nslow")
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, _ := io.ReadAll(c)
	if took := time.Since(start); took > 5*time.Second || !strings.Contains(string(got), "408") {
		t.Errorf("a stalled body got %q after %s", got, took)
	}
}
