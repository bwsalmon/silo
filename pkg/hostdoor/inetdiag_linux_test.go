package hostdoor_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/hostdoor"
)

// accepted dials a fresh loopback listener and returns both ends.
func accepted(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

func ends(s net.Conn) (local, peer netip.AddrPort) {
	return s.LocalAddr().(*net.TCPAddr).AddrPort(), s.RemoteAddr().(*net.TCPAddr).AddrPort()
}

// skipNoDiag skips where this kernel has no inet_diag for TCP to an
// unprivileged process (CONFIG_INET_DIAG off, or a sandbox that filters
// netlink): the lookup refuses every connection there, which is what
// the listener should do, but there is nothing to test.
func skipNoDiag(t *testing.T, err error) {
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == syscall.EPROTONOSUPPORT || errno == syscall.EAFNOSUPPORT || errno == syscall.EPERM || errno == syscall.EACCES) {
		t.Skipf("no inet_diag here: %v", err)
	}
}

// inet_diag reports a loopback peer's owner to an unprivileged process:
// this test's own uid, whether it runs as root or not.
func TestInetDiagReportsTheLoopbackPeersUID(t *testing.T) {
	_, s := accepted(t)
	local, peer := ends(s)
	uid, err := hostdoor.InetDiagUID(local, peer)
	skipNoDiag(t, err)
	if err != nil {
		t.Fatalf("InetDiagUID(%s, %s): %v", local, peer, err)
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("uid %d, want %d", uid, os.Getuid())
	}
	// The server's own end is not the peer: asking for the socket at
	// local connected to peer is asking for silod's, whose owner is the
	// same here, but the lookup must find the one asked for.
	if _, err := hostdoor.InetDiagUID(peer, netip.AddrPortFrom(local.Addr(), local.Port()+1)); err == nil {
		t.Errorf("a socket that doesn't exist was found")
	}
}

// A peer that closed before the lookup has no uid: the lookup says
// ErrNoPeer, whether the socket is gone (reset) or only a TIME_WAIT
// remnant, which the kernel reports with uid 0.
func TestInetDiagClosedPeer(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		c, s := accepted(t)
		c.SetLinger(0)
		c.Close()
		time.Sleep(50 * time.Millisecond)
		local, peer := ends(s)
		_, err := hostdoor.InetDiagUID(local, peer)
		skipNoDiag(t, err)
		if !errors.Is(err, hostdoor.ErrNoPeer) {
			t.Errorf("lookup of a reset peer: %v, want ErrNoPeer", err)
		}
	})
	t.Run("time-wait", func(t *testing.T) {
		c, s := accepted(t)
		// The client closes first and the server answers, so the
		// client's end goes to TIME_WAIT.
		c.Close()
		s.Read(make([]byte, 1))
		s.Close()
		time.Sleep(50 * time.Millisecond)
		local, peer := ends(s)
		_, err := hostdoor.InetDiagUID(local, peer)
		skipNoDiag(t, err)
		if !errors.Is(err, hostdoor.ErrNoPeer) {
			t.Errorf("lookup of a TIME_WAIT peer: %v, want ErrNoPeer", err)
		}
	})
}

// The real listener refuses a peer that reset before it was accepted,
// and goes on to accept the next.
func TestListenRefusesAClosedPeer(t *testing.T) {
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 4)
	l := hostdoor.Listen(tl, nil, func(_ net.Addr, err error) { refused <- err })
	defer l.Close()

	gone, err := net.Dial("tcp4", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	gone.(*net.TCPConn).SetLinger(0)
	gone.Close()
	time.Sleep(50 * time.Millisecond)
	live, err := net.Dial("tcp4", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-refused:
		skipNoDiag(t, err)
		if !errors.Is(err, hostdoor.ErrNoPeer) {
			t.Errorf("refused the closed peer with %v, want ErrNoPeer", err)
		}
	default:
		t.Fatalf("the closed peer was accepted (%s; live is %s)", c.RemoteAddr(), live.LocalAddr())
	}
	if c.RemoteAddr().String() != live.LocalAddr().String() {
		t.Errorf("accepted %s, want %s", c.RemoteAddr(), live.LocalAddr())
	}
	if hc, ok := c.(*hostdoor.Conn); !ok || hc.UID != uint32(os.Getuid()) {
		t.Errorf("accepted %#v, want a *Conn of uid %d", c, os.Getuid())
	}
}

// The real thing, where sudo lets the test run a client as nobody:
// connections to localhost:<port> from this test's uid and from
// nobody's, told apart by inet_diag alone, act as their own silos.
func TestHostPortTellsRealUIDsApart(t *testing.T) {
	me := uint32(os.Getuid())
	if me == 65534 {
		t.Skip("running as nobody")
	}
	if err := exec.Command("sudo", "-n", "-u", "nobody", "true").Run(); err != nil {
		t.Skipf("can't run a client as nobody: sudo -n -u nobody: %v", err)
	}
	e := newEnv(t)
	e.child("mine", me)
	e.child("theirs", 65534)
	srv, err := hostdoor.Start("127.0.0.1:0", e.db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := srv.Addr().(*net.TCPAddr).Port

	// Each pushes a repo in its own silo, from its own process.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	e.port = port
	if r := e.push(c, "mine/app", "v1"); r.code != 201 {
		t.Fatalf("uid %d pushing mine/app: %d %s", me, r.code, r.body)
	}
	catalog := func(uid string) string {
		t.Helper()
		// bash's /dev/tcp: a client with no dependency but bash, run as
		// uid.
		script := fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d && printf 'GET /v2/_catalog HTTP/1.0\r\nHost: localhost:%d\r\n\r\n' >&3 && cat <&3`, port, port)
		args := []string{"bash", "-c", script}
		if uid != "" {
			args = append([]string{"sudo", "-n", "-u", uid}, args...)
		}
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("catalog as %q: %v: %s", uid, err, out)
		}
		return string(out)
	}
	if got := catalog(""); !strings.Contains(got, `"mine/app"`) || strings.Contains(got, "theirs/") {
		t.Errorf("uid %d's catalog: %s", me, got)
	}
	if got := catalog("nobody"); !strings.Contains(got, " 200 ") || strings.Contains(got, "mine/app") {
		t.Errorf("nobody's catalog: %s, want 200 without mine/app", got)
	}
}
