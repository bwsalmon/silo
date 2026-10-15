package tunnel

import (
	"io"
	"net"
	"sync"
)

// Forward accepts connections on l and carries each to a connection
// dial makes, both ways, passing each half-close on, until l is closed.
// It is how a granule guest with no network reaches the session's git
// proxy: the guest's git connects to its own loopback, and Forward
// carries that to the host over vsock (design §9.3).
func Forward(l net.Listener, dial func() (io.ReadWriteCloser, error), logf func(string, ...any)) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			d, err := dial()
			if err != nil {
				if logf != nil {
					logf("forward: %v", err)
				}
				c.Close()
				return
			}
			pipe(c, d)
		}()
	}
}

type closeWriter interface{ CloseWrite() error }

// pipe copies a and b both ways and closes both once both directions
// are done.
func pipe(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	one := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go one(a, b)
	go one(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}
