// Adapted from github.com/bwsalmon/grain pkg/mcp/managed_http.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxRequestBytes bounds one request body. A sandbox_write_file of a
// large file is the biggest thing a client sends.
const maxRequestBytes = 64 << 20

// Server is one authenticated Streamable HTTP endpoint on loopback
// (design §4.3, grain's docs/codex-isolation.md): it listens on
// 127.0.0.1 on a random port, requires `Authorization: Bearer <token>`
// with a 256-bit random token, and refuses any Origin header and any
// Host other than the address it bound, so a browser on the same
// machine cannot reach it through DNS rebinding.
//
// POST /mcp answers one JSON-RPC request with a JSON response. GET /mcp
// with Accept: text/event-stream opens the server-to-client stream that
// carries notifications/tools/list_changed.
type Server struct {
	// URL is http://127.0.0.1:<port>/mcp.
	URL string
	// Token is the bearer token, without "Bearer ".
	Token string

	handler   Handler
	authority string
	http      *http.Server
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	streams map[chan []byte]struct{}
}

// Listen starts serving h on a fresh loopback port. Calls run under a
// context derived from ctx, and cancelled by Close.
func Listen(ctx context.Context, h Handler) (*Server, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listening on loopback: %w", err)
	}
	s := &Server{
		Token:     hex.EncodeToString(secret[:]),
		handler:   h,
		authority: l.Addr().String(),
		streams:   map[chan []byte]struct{}{},
	}
	s.URL = "http://" + s.authority + "/mcp"
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
	}
	go func() { _ = s.http.Serve(l) }()
	return s, nil
}

// Close stops the server, cancelling calls in flight and closing every
// notification stream.
func (s *Server) Close() error {
	s.cancel()
	return s.http.Close()
}

// NotifyToolsChanged sends notifications/tools/list_changed on every
// open notification stream.
func (s *Server) NotifyToolsChanged() {
	msg := marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		select {
		case ch <- msg:
		default:
			// A client that is not reading loses a notification that only
			// says "list again", rather than holding up the gateway.
		}
	}
}

// Handler is the HTTP handler: what Listen serves, exported so that a
// test can drive it with httptest.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != s.authority || r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.post(w, r)
		case http.MethodGet:
			s.stream(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	resp := Dispatch(r.Context(), s.handler, body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "expected Accept: text/event-stream", http.StatusNotAcceptable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 8)
	s.mu.Lock()
	s.streams[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
