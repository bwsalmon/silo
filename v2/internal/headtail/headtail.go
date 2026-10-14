// Adapted from github.com/bwsalmon/grain pkg/mcp/exec_output.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package headtail is a bounded output buffer: it keeps the first and
// last halves of its limit of whatever is written to it and counts what
// it dropped between them, so a command that prints without end costs a
// bounded amount of memory and still shows how it began and how it
// ended.
package headtail

import (
	"fmt"
	"sync"
)

// Buffer is an io.Writer keeping at most Limit bytes. It is safe for
// concurrent use.
type Buffer struct {
	mu    sync.Mutex
	limit int
	head  []byte
	tail  []byte // a ring once full
	start int    // index of the oldest tail byte
	total int64
}

// New returns a Buffer keeping at most limit bytes.
func New(limit int) *Buffer { return &Buffer{limit: limit} }

func (b *Buffer) headCap() int { return b.limit / 2 }
func (b *Buffer) tailCap() int { return b.limit - b.headCap() }

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if room := b.headCap() - len(b.head); room > 0 {
		take := min(room, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	tailCap := b.tailCap()
	if len(p) >= tailCap {
		b.tail = append(b.tail[:0], p[len(p)-tailCap:]...)
		b.start = 0
		return n, nil
	}
	if room := tailCap - len(b.tail); room > 0 {
		take := min(room, len(p))
		b.tail = append(b.tail, p[:take]...)
		p = p[take:]
	}
	for len(p) > 0 {
		c := copy(b.tail[b.start:], p)
		p = p[c:]
		b.start = (b.start + c) % tailCap
	}
	return n, nil
}

// Total is how many bytes were written.
func (b *Buffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Dropped is how many bytes were written but not kept.
func (b *Buffer) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total - int64(len(b.head)+len(b.tail))
}

// Bytes returns what was kept, with a line marking the cut when
// anything was dropped.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := len(b.head) + len(b.tail)
	dropped := b.total - int64(kept)
	out := make([]byte, 0, kept+128)
	out = append(out, b.head...)
	if dropped > 0 {
		out = fmt.Appendf(out, "\n... [%d bytes of output dropped: silo keeps the first and last %d bytes] ...\n", dropped, b.limit/2)
	}
	out = append(out, b.tail[b.start:]...)
	out = append(out, b.tail[:b.start]...)
	return out
}
