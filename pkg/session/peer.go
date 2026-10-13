package session

import (
	"errors"
	"log"
	"net"
	"strings"
)

// PeerCheck is a session socket's check of who connects, beside the
// socket's file mode: it returns an error, and the connection is closed
// unserved, unless c's peer may be agent's session.
type PeerCheck func(agent string, c net.Conn) error

// ErrPeer is a connection refused by the peer check.
var ErrPeer = errors.New("session: the peer is not the session's")

// InCgroup reports whether a process whose cgroup (v2) is path is in
// the cgroup named leaf, and, if manager isn't empty, under the systemd
// user manager manager (user@<uid>.service), silod's own: what
// ScopePeer checks a connecting process against.
func InCgroup(path, leaf, manager string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[len(parts)-1] != leaf {
		return false
	}
	if manager == "" {
		return true
	}
	for _, p := range parts[:len(parts)-1] {
		if p == manager {
			return true
		}
	}
	return false
}

// UserManager is the systemd user manager unit, user@<uid>.service,
// that the cgroup path is under, or "".
func UserManager(path string) string {
	for _, p := range strings.Split(strings.Trim(path, "/"), "/") {
		if strings.HasPrefix(p, "user@") && strings.HasSuffix(p, ".service") {
			return p
		}
	}
	return ""
}

// peerListener closes, unserved, each connection its check refuses.
type peerListener struct {
	net.Listener
	agent string
	check PeerCheck
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := l.check(l.agent, c); err != nil {
			log.Printf("session socket of %s: refused a connection: %v", l.agent, err)
			c.Close()
			continue
		}
		return c, nil
	}
}
