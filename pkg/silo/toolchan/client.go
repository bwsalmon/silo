package toolchan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http2"
)

// DefaultURL is where silo-tools attaches: the gateway's port 80.
const DefaultURL = "http://silo.internal/silo/v1/agents/-/tools:attach"

// maxRedirects bounds the redirects one attach follows.
const maxRedirects = 5

// Client is silo-tools' end: it dials the attach URL, follows a
// redirect to the gateway of the control VM that hosts the session,
// serves Handler on the channel once it is up, and attaches again with
// backoff whenever the channel drops or an attach fails.
type Client struct {
	// URL is the attach URL; DefaultURL if empty.
	URL string
	// Handler serves the tool calls that come down the channel: the MCP
	// server, at /mcp.
	Handler http.Handler
	// Dial dials the gateway; a net.Dialer's if nil.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// MinBackoff and MaxBackoff bound the wait before attaching again:
	// a second and a minute if zero. It doubles on each failure and
	// starts again from MinBackoff after a channel that was up.
	MinBackoff, MaxBackoff time.Duration
	// Attached, if set, is called each time a channel comes up, with
	// the URL that took it.
	Attached func(url string)
	// Logf logs; log.Printf if nil.
	Logf func(format string, args ...any)
}

// Run attaches, and attaches again, until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	min, max := c.MinBackoff, c.MaxBackoff
	if min == 0 {
		min = time.Second
	}
	if max == 0 {
		max = time.Minute
	}
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}
	wait := min
	for ctx.Err() == nil {
		up, err := c.Once(ctx)
		if ctx.Err() != nil {
			break
		}
		if up {
			wait = min
			logf("silo-tools: the tool channel dropped: %v; attaching again in %s", err, wait)
		} else {
			logf("silo-tools: attaching: %v; trying again in %s", err, wait)
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if !up {
			wait *= 2
			if wait > max {
				wait = max
			}
		}
	}
	return ctx.Err()
}

// Once attaches once and serves the channel until it drops. up says
// whether the channel came up at all.
func (c *Client) Once(ctx context.Context) (up bool, err error) {
	u := c.URL
	if u == "" {
		u = DefaultURL
	}
	for i := 0; ; i++ {
		conn, next, err := c.attach(ctx, u)
		if err != nil {
			return false, err
		}
		if next == "" {
			if c.Attached != nil {
				c.Attached(u)
			}
			return true, c.serve(ctx, conn)
		}
		if i == maxRedirects {
			return false, fmt.Errorf("more than %d redirects, the last to %s", maxRedirects, next)
		}
		u = next
	}
}

// attach makes one attach to u: the upgraded connection, or the URL a
// redirect names.
func (c *Client) attach(ctx context.Context, u string) (net.Conn, string, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, "", err
	}
	if pu.Scheme != "http" {
		return nil, "", fmt.Errorf("attach URL %s: only http is spoken to the gateway", u)
	}
	addr := pu.Host
	if pu.Port() == "" {
		addr = net.JoinHostPort(pu.Hostname(), "80")
	}
	dial := c.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, "", err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(time.Minute))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	req.Header.Set("User-Agent", "silo-tools")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, "", err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
		conn.SetDeadline(time.Time{})
		return &buffered{Conn: conn, r: br}, "", nil
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		resp.Body.Close()
		conn.Close()
		loc, err := resp.Location()
		if err != nil {
			return nil, "", fmt.Errorf("%s with no Location: %w", resp.Status, err)
		}
		return nil, loc.String(), nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	conn.Close()
	err = fmt.Errorf("%s answered %s: %s", u, resp.Status, body)
	if resp.StatusCode == http.StatusForbidden {
		err = fmt.Errorf("%w: %v", ErrRefused, err)
	}
	return nil, "", err
}

// serve serves Handler as the HTTP/2 server on conn until it ends.
func (c *Client) serve(ctx context.Context, conn net.Conn) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// No idle timeout: the channel waits for calls as long as it is up,
	// and silod's pings keep it known to be.
	(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: c.Handler})
	conn.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("the connection ended")
}

// buffered is a connection whose first bytes were already read into r.
type buffered struct {
	net.Conn
	r *bufio.Reader
}

func (b *buffered) Read(p []byte) (int, error) { return b.r.Read(p) }
