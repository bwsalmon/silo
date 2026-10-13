package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// The content types of an output stream: Docker's multiplexed framing,
// or a TTY's raw bytes.
const (
	mediaMultiplexed = "application/vnd.docker.multiplexed-stream"
	mediaRaw         = "application/vnd.docker.raw-stream"
)

// outputs are the writers a process's stdout and stderr go to on w: in
// Docker's multiplexed framing (an 8-byte header naming the stream, 1
// or 2, and the frame's length), or raw for a TTY, whose output is one
// stream. The two may be written at once.
func outputs(w io.Writer, tty bool) (stdout, stderr io.Writer) {
	mu := &sync.Mutex{}
	if tty {
		raw := &muxWriter{mu: mu, w: w}
		return raw, raw
	}
	return &muxWriter{mu: mu, w: w, stream: 1}, &muxWriter{mu: mu, w: w, stream: 2}
}

// muxWriter writes one stream of an output: framed, unless stream is 0.
type muxWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	stream byte
}

func (m *muxWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stream != 0 {
		var h [8]byte
		h[0] = m.stream
		binary.BigEndian.PutUint32(h[4:], uint32(len(b)))
		if _, err := m.w.Write(h[:]); err != nil {
			return 0, err
		}
	}
	return m.w.Write(b)
}

// hijack takes the call's connection, as Docker's attach and exec start
// do, and answers on it: 101 UPGRADED when the client asked to upgrade
// to tcp, and 200 otherwise, with the output's content type. in is what
// the client sends after its request, which is the process's stdin.
// Once it returns, the handler writes nothing to q.w and returns nil.
func hijack(q *request, tty bool) (conn net.Conn, in io.Reader, err error) {
	hj, ok := q.w.(http.Hijacker)
	if !ok {
		return nil, nil, errorf(http.StatusInternalServerError, "silo: this connection cannot be hijacked for a stream")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	ct := mediaMultiplexed
	if tty {
		ct = mediaRaw
	}
	status, upgrade := "200 OK", ""
	if strings.EqualFold(q.r.Header.Get("Upgrade"), "tcp") {
		status, upgrade = "101 UPGRADED", "Connection: Upgrade\r\nUpgrade: tcp\r\n"
	}
	fmt.Fprintf(rw, "HTTP/1.1 %s\r\nContent-Type: %s\r\nApi-Version: %s\r\nServer: Silo (local backend)\r\n%s\r\n",
		status, ct, APIVersion, upgrade)
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw.Reader, nil
}

// errDetached is stdin's end at the detach keys.
var errDetached = errors.New("detached")

// detachKeys parses a detach key sequence as Docker writes one:
// "ctrl-p,ctrl-q", each key ctrl-<a-z, @, [, \, ], ^ or _> or one
// character. Empty is ctrl-p,ctrl-q.
func detachKeys(s string) ([]byte, error) {
	if s == "" {
		return []byte{0x10, 0x11}, nil
	}
	var out []byte
	for _, k := range strings.Split(s, ",") {
		switch c, ok := strings.CutPrefix(strings.ToLower(k), "ctrl-"); {
		case len(k) == 1:
			out = append(out, k[0])
		case ok && len(c) == 1 && c[0] >= 'a' && c[0] <= 'z':
			out = append(out, c[0]-'a'+1)
		case ok && len(c) == 1 && strings.Contains("@[\\]^_", c):
			out = append(out, c[0]-'@')
		default:
			return nil, invalid("Invalid detach keys (%s) provided", s)
		}
	}
	return out, nil
}

// detachReader is a TTY's stdin until the detach keys: the keys
// themselves are not passed on, and a partial sequence is, once the
// next byte breaks it.
type detachReader struct {
	r       io.Reader
	keys    []byte
	matched int
	pending []byte
	err     error
}

func (d *detachReader) Read(p []byte) (int, error) {
	for len(d.pending) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		buf := make([]byte, max(len(p), 1))
		n, err := d.r.Read(buf)
		for _, b := range buf[:n] {
			if b != d.keys[d.matched] {
				d.pending = append(d.pending, d.keys[:d.matched]...)
				d.matched = 0
			}
			if b != d.keys[d.matched] {
				d.pending = append(d.pending, b)
				continue
			}
			if d.matched++; d.matched == len(d.keys) {
				d.err = errDetached
				break
			}
		}
		if err != nil && d.err == nil {
			d.pending = append(d.pending, d.keys[:d.matched]...)
			d.err = err
		}
	}
	n := copy(p, d.pending)
	d.pending = d.pending[n:]
	return n, nil
}

// pollOutput is how often a streaming attach reads the workload's
// output again.
const pollOutput = 100 * time.Millisecond

// containerAttach is POST /containers/{id}/attach: the workload's
// output, as the driver's streams give it, on a hijacked connection,
// and its stdin from the connection when the call sets stdin=1 and the
// container set OpenStdin (call 52 makes that call a mutation). The vm
// class's attach is output-only in v1: its create refuses OpenStdin
// (local-backend.md §4, "Exec, logs and attach").
//
// The driver's streams are the output so far, so logs=1 starts at the
// beginning and logs=0 at the end of it, and stream=1 reads them again
// as the workload writes, until it stops, the client goes or its TTY
// stdin sends the detach keys.
func (s *Server) containerAttach(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	keys, err := detachKeys(q.query("detachKeys"))
	if err != nil {
		return err
	}
	d := s.drivers[c.Class]
	if d == nil {
		return conflict("silo: container %s is gone from its runtime; remove it", c.ID)
	}
	st, err := d.Streams(q.ctx(), c.ID)
	if err != nil {
		return runtimeErr(err, c)
	}
	withStdin := q.flag("stdin") && c.Process.Stdin && c.Class != runtime.VM && st.Stdin != nil
	if !withStdin && st.Stdin != nil {
		_ = st.Stdin.Close()
	}
	wantOut, wantErr := q.flag("stdout"), q.flag("stderr")
	var sent [2]int
	if !q.flag("logs") {
		sent = [2]int{readLen(st.Stdout), readLen(st.Stderr)}
	} else {
		discard(st.Stdout)
		discard(st.Stderr)
	}
	if withStdin {
		s.audit(q, "OK", map[string]any{"id": c.ID, "stdin": true}, nil)
	}
	conn, in, err := hijack(q, c.Process.TTY)
	if err != nil {
		if withStdin {
			_ = st.Stdin.Close()
		}
		return err
	}
	defer conn.Close()
	stdout, stderr := outputs(conn, c.Process.TTY)

	ctx, cancel := context.WithCancel(s.bg)
	defer cancel()
	go func() {
		// A client that sends no stdin closes the connection when it
		// goes. One that sends stdin closes its half when that ends,
		// and is still reading; it goes at the detach keys.
		if !withStdin {
			_, _ = io.Copy(io.Discard, in)
			cancel()
			return
		}
		defer st.Stdin.Close()
		r := in
		if c.Process.TTY {
			r = &detachReader{r: in, keys: keys}
		}
		if _, err := io.Copy(st.Stdin, r); errors.Is(err, errDetached) {
			cancel()
		}
	}()

	// write sends the output past what was sent, and reports whether
	// the client is still there.
	write := func(st runtime.Streams) bool {
		for i, r := range []io.ReadCloser{st.Stdout, st.Stderr} {
			b, _ := io.ReadAll(r)
			_ = r.Close()
			if len(b) < sent[i] {
				sent[i] = 0 // a new log, after a restart
			}
			w, want := stdout, wantOut
			if i == 1 {
				w, want = stderr, wantErr
			}
			if want && len(b) > sent[i] {
				if _, err := w.Write(b[sent[i]:]); err != nil {
					return false
				}
			}
			sent[i] = len(b)
		}
		return true
	}
	if q.flag("logs") {
		again, err := d.Streams(ctx, c.ID)
		if err != nil {
			return nil
		}
		if again.Stdin != nil {
			_ = again.Stdin.Close()
		}
		if !write(again) {
			return nil
		}
	}
	if !q.flag("stream") {
		return nil
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			_, err := d.Wait(ctx, c.ID)
			if err == nil || errors.Is(err, runtime.ErrNotFound) {
				return
			}
			// A created container has nothing to wait for until it
			// starts.
			select {
			case <-ctx.Done():
			case <-time.After(pollOutput):
			}
		}
	}()
	for {
		var done bool
		select {
		case <-ctx.Done():
			return nil
		case <-stopped:
			done = true
		case <-time.After(pollOutput):
		}
		again, err := d.Streams(ctx, c.ID)
		if err != nil {
			return nil
		}
		if again.Stdin != nil {
			_ = again.Stdin.Close()
		}
		if !write(again) || done {
			return nil
		}
	}
}

// readLen is how much r holds, read and closed.
func readLen(r io.ReadCloser) int {
	n, _ := io.Copy(io.Discard, r)
	_ = r.Close()
	return int(n)
}

// runtimeErr is a driver's error for c in Docker's terms.
func runtimeErr(err error, c *Container) error {
	switch {
	case errors.Is(err, runtime.ErrState):
		return conflict("Container %s is not running", c.ID)
	case errors.Is(err, runtime.ErrNotFound):
		return conflict("silo: container %s is gone from its runtime; remove it", c.ID)
	}
	return err
}
