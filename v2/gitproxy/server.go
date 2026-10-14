// Adapted from github.com/bwsalmon/grain pkg/gitproxy/server.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// Wires GitProxy's decision logic to a real HTTP server. net/http is
// enough for a proxy whose whole job is decide-then-stream, no routing
// framework needed. grain builds one proxy for its daemon from files under
// its data directory (BuildProxy); a silo session builds its own, from its
// Silofile, and serves it on loopback for as long as the session runs
// (Serve).

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewHandler wraps proxy as an http.Handler: split the path and query,
// hand the body over unread (GitProxy.HandleRequest reads it, bounded,
// only once the request is allowed), run the decision logic, and stream
// the response back with WriteResponse.
func NewHandler(proxy *GitProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader
		if r.ContentLength != 0 {
			body = r.Body
		}
		headers := map[string]string{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}

		WriteResponse(w, proxy.HandleRequest(r.Context(), r.Method, r.URL.Path, r.URL.RawQuery, headers, body))
	})
}

// WriteResponse writes resp to w: its headers, less the hop-by-hop ones
// and Content-Length (which a decision about an upstream response could
// get wrong for this transport's own), then its body. A Stream is copied
// through as it is read and flushed after every read, so a large
// packfile is never held whole and git's progress messages reach the
// sandbox when they are sent; it is closed once copied. A Stream that
// fails part way aborts the response rather than ending it cleanly, so
// the client sees a truncated transfer as one instead of as a complete
// response that happens to be short.
func WriteResponse(w http.ResponseWriter, resp ProxyResponse) {
	if resp.Stream != nil {
		defer resp.Stream.Close()
	}
	for k, v := range resp.Headers {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding":
			continue
		}
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)
	if resp.Stream == nil {
		w.Write(resp.Body)
		return
	}
	if _, err := io.Copy(flushWriter{w, http.NewResponseController(w)}, resp.Stream); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// flushWriter flushes every write straight through to the client.
// io.Copy hands it whatever each read of the upstream returned, so this
// adds no buffering of its own and keeps none back.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := f.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, err
	}
	return n, nil
}

// Server is a GitProxy serving HTTP on a listener of its own.
type Server struct {
	// URL is the proxy's base URL, http://<addr>; a repository is
	// <URL>/<host>/<owner>/<name>.git.
	URL string

	srv  *http.Server
	ln   net.Listener
	done chan struct{}
}

// Serve serves proxy on addr ("127.0.0.1:0" for a free loopback port)
// until Close.
func Serve(proxy *GitProxy, addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		URL:  "http://" + ln.Addr().String(),
		srv:  &http.Server{Handler: NewHandler(proxy), ReadHeaderTimeout: 30 * time.Second},
		ln:   ln,
		done: make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// Close stops the server, waiting up to ctx's deadline for requests in
// flight, and closing them past it.
func (s *Server) Close(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	<-s.done
	return err
}
