package server

import (
	"io"
	"net/http"
	"time"
)

// bodyTimeout is how long a client has to send a request's body.
const bodyTimeout = time.Minute

// bodyDeadline gives a request with a body d to send all of it, so a
// client cannot hold a request open by sending its body slowly
// (SA1-L7). A WebSocket upgrade has no body, and an event subscription
// or a view's response may run for an hour, so once the body has been
// read (or when there is none) the connection has no read deadline;
// http.Server's ReadTimeout would instead end those too.
func bodyDeadline(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if rc.SetReadDeadline(time.Now().Add(d)) != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody lifts the read deadline once its body has been read.
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	done bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}
