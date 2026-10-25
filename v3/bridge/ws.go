package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// FrameConn reads and writes single JSON frames over either a newline-delimited
// stream (Unix socket) or a WebSocket connection.
type FrameConn interface {
	ReadFrame() ([]byte, error)
	WriteFrame(data []byte) error
	Close() error
}

// lineConn implements FrameConn over a newline-delimited net.Conn.
type lineConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func newLineConn(conn net.Conn, br *bufio.Reader) *lineConn {
	if br == nil {
		br = bufio.NewReaderSize(conn, 64<<10)
	}
	return &lineConn{conn: conn, br: br}
}

func (c *lineConn) ReadFrame() ([]byte, error) {
	for {
		var buf []byte
		for {
			chunk, isPrefix, err := c.br.ReadLine()
			if err != nil {
				return nil, err
			}
			buf = append(buf, chunk...)
			if len(buf) > 4<<20 {
				return nil, errors.New("frame too large")
			}
			if !isPrefix {
				break
			}
		}
		line := bytes.TrimSpace(buf)
		if len(line) > 0 {
			return line, nil
		}
	}
}

func (c *lineConn) WriteFrame(data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	data = bytes.TrimRight(data, "\n")
	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	_, err := c.conn.Write(buf)
	return err
}

func (c *lineConn) Close() error { return c.conn.Close() }

// ---- Minimal RFC 6455 WebSocket ---------------------------------------------

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

type wsConn struct {
	conn   net.Conn
	br     *bufio.Reader
	client bool
	wmu    sync.Mutex
}

func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// UpgradeWS upgrades an HTTP request to a WebSocket connection.
func UpgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing Upgrade: websocket header")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key header")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijacking")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := wsAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: rw.Reader, client: false}, nil
}

// DialWS dials a ws:// (or http://) URL and performs the WebSocket handshake.
func DialWS(ctx context.Context, rawURL string, headers map[string]string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "ws", "http":
	default:
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	var keyBytes [16]byte
	_, _ = rand.Read(keyBytes[:])
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	reqURI := u.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", reqURI)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, v := range headers {
		fmt.Fprintf(&req, "%s: %s\r\n", k, v)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake status %d", resp.StatusCode)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(key) {
		conn.Close()
		return nil, errors.New("invalid Sec-WebSocket-Accept")
	}
	return &wsConn{conn: conn, br: br, client: true}, nil
}

func (w *wsConn) ReadFrame() ([]byte, error) {
	var msg []byte
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
			return nil, err
		}
		fin := hdr[0]&0x80 != 0
		op := hdr[0] & 0x0F
		masked := hdr[1]&0x80 != 0
		length := uint64(hdr[1] & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > 4<<20 {
			return nil, errors.New("websocket frame too large")
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, maskKey[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch op {
		case wsOpClose:
			_ = w.writeControl(wsOpClose, nil)
			return nil, io.EOF
		case wsOpPing:
			_ = w.writeControl(wsOpPong, payload)
			continue
		case wsOpPong:
			continue
		case wsOpText, wsOpBinary, wsOpContinuation:
			msg = append(msg, payload...)
			if fin {
				line := bytes.TrimSpace(msg)
				if len(line) == 0 {
					msg = nil
					continue
				}
				return line, nil
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", op)
		}
	}
}

func (w *wsConn) WriteFrame(data []byte) error {
	return w.writeRaw(wsOpText, bytes.TrimRight(data, "\n"))
}

func (w *wsConn) writeControl(op byte, data []byte) error {
	return w.writeRaw(op, data)
}

func (w *wsConn) writeRaw(op byte, data []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	var buf bytes.Buffer
	buf.WriteByte(0x80 | op)
	maskBit := byte(0)
	if w.client {
		maskBit = 0x80
	}
	n := len(data)
	switch {
	case n < 126:
		buf.WriteByte(maskBit | byte(n))
	case n <= 65535:
		buf.WriteByte(maskBit | 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		buf.Write(ext[:])
	default:
		buf.WriteByte(maskBit | 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		buf.Write(ext[:])
	}
	if w.client {
		var maskKey [4]byte
		_, _ = rand.Read(maskKey[:])
		buf.Write(maskKey[:])
		masked := make([]byte, n)
		for i := range data {
			masked[i] = data[i] ^ maskKey[i%4]
		}
		buf.Write(masked)
	} else {
		buf.Write(data)
	}
	_, err := w.conn.Write(buf.Bytes())
	return err
}

func (w *wsConn) Close() error {
	_ = w.writeControl(wsOpClose, nil)
	return w.conn.Close()
}
