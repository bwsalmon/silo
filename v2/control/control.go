// Package control is a session's control socket (grain.md §2.1):
// control.sock in the session directory, mode 0600, speaking JSON-RPC
// 2.0 with one request or response per line.
//
// The server holds a table of methods, so that each stage adds its own:
// stage 4a registers approve (design §12.3); stage 6b adds inject,
// cancel and extend_deadline. Only the invoking user can open the
// socket, so it carries no token of its own.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"
)

// FileName is the control socket's name in the session directory.
const FileName = "control.sock"

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// maxLine bounds one request or response.
const maxLine = 1 << 20

// Error is a JSON-RPC error. A method returning one has it sent as it
// is; any other error is sent as an internal error carrying its message.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an *Error with CodeInvalidParams, the code for a
// request a method understood and will not do.
func Errorf(format string, args ...any) error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// Method answers one method's calls. params is the request's params,
// null when it gave none; the result is marshalled as the response's
// result.
type Method func(ctx context.Context, params json.RawMessage) (any, error)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Server is a listening control socket.
type Server struct {
	path string
	ln   net.Listener
	ctx  context.Context

	mu      sync.Mutex
	methods map[string]Method
	conns   map[net.Conn]struct{}
	closed  bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

// Listen creates the socket at path, mode 0600, and serves it until
// Close. Methods run under a context that Close cancels.
func Listen(path string) (*Server, error) {
	// The session directory is 0700, so nobody else can reach the
	// socket in the moment before the chmod.
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{path: path, ln: ln, ctx: ctx, cancel: cancel, methods: map[string]Method{}, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Path is the socket's path.
func (s *Server) Path() string { return s.path }

// Handle adds a method, or replaces one of the same name.
func (s *Server) Handle(name string, m Method) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[name] = m
}

// Close stops serving, ends open connections and removes the socket.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	var wmu sync.Mutex
	var calls sync.WaitGroup
	defer calls.Wait()
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		// Requests on one connection may be answered out of order, so
		// that a long call (an approval) does not hold up a short one.
		calls.Add(1)
		go func() {
			defer calls.Done()
			resp, ok := s.call(line)
			if !ok {
				return
			}
			b, _ := json.Marshal(resp)
			wmu.Lock()
			c.Write(append(b, '\n'))
			wmu.Unlock()
		}()
	}
}

// call answers one line, or reports false for a notification, which
// gets no answer.
func (s *Server) call(line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{Code: CodeParseError, Message: err.Error()}}, true
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if len(req.ID) == 0 {
		resp.ID = json.RawMessage("null")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		resp.Error = &Error{Code: CodeInvalidRequest, Message: `want {"jsonrpc": "2.0", "method": ...}`}
		return resp, true
	}
	s.mu.Lock()
	m, ok := s.methods[req.Method]
	names := make([]string, 0, len(s.methods))
	for n := range s.methods {
		names = append(names, n)
	}
	s.mu.Unlock()
	if !ok {
		resp.Error = &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("no method %q on this session's control socket (it has %v)", req.Method, sorted(names))}
		return resp, len(req.ID) > 0
	}
	params := req.Params
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	res, err := m(s.ctx, params)
	if len(req.ID) == 0 {
		return response{}, false
	}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		resp.Error = e
		return resp, true
	}
	if res == nil {
		res = struct{}{}
	}
	resp.Result = res
	return resp, true
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

// Call makes one call on the control socket at path and decodes its
// result into result, which may be nil.
func Call(ctx context.Context, path, method string, params, result any) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		return errors.New("control socket closed without answering")
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}
