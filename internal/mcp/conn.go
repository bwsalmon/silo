package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Handler answers the requests and notifications a peer sends. For a
// notification the result is ignored. Returning an *Error sends it as is;
// any other error is sent as an internal error.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Conn is one JSON-RPC connection: it sends requests and notifications
// and dispatches what the peer sends to a Handler. Incoming requests are
// handled concurrently, so a slow tool call (one waiting on the person's
// approval, say) does not hold up pings.
type Conn struct {
	r       io.Reader
	w       io.Writer
	handler Handler

	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *Message
	closed  bool
	done    chan struct{}
}

// NewConn returns a connection reading from r and writing to w. Call Run
// to start reading.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	return &Conn{r: r, w: w, handler: h, pending: map[string]chan *Message{}, done: make(chan struct{})}
}

// Done is closed once Run returns.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Run reads messages until the stream ends or ctx is cancelled.
func (c *Conn) Run(ctx context.Context) error {
	defer func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.done)
	}()
	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			c.send(&Message{Error: &Error{Code: ParseError, Message: "parse error"}, ID: json.RawMessage("null")})
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			go c.answer(ctx, msg)
		case msg.Method != "":
			if c.handler != nil {
				go c.handler(ctx, msg.Method, msg.Params)
			}
		case len(msg.ID) > 0:
			c.mu.Lock()
			ch := c.pending[string(msg.ID)]
			delete(c.pending, string(msg.ID))
			c.mu.Unlock()
			if ch != nil {
				m := msg
				ch <- &m
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (c *Conn) answer(ctx context.Context, msg Message) {
	if c.handler == nil {
		c.send(&Message{ID: msg.ID, Error: &Error{Code: MethodNotFound, Message: "method not found: " + msg.Method}})
		return
	}
	res, err := c.handler(ctx, msg.Method, msg.Params)
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: InternalError, Message: err.Error()}
		}
		c.send(&Message{ID: msg.ID, Error: rpcErr})
		return
	}
	raw, err := json.Marshal(res)
	if err != nil {
		c.send(&Message{ID: msg.ID, Error: &Error{Code: InternalError, Message: err.Error()}})
		return
	}
	c.send(&Message{ID: msg.ID, Result: raw})
}

// Call sends a request and decodes its result into out (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("connection closed")
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := &Message{ID: json.RawMessage(id), Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = raw
	}
	if err := c.send(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	msg := &Message{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = raw
	}
	return c.send(msg)
}

func (c *Conn) send(msg *Message) error {
	msg.JSONRPC = "2.0"
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}
