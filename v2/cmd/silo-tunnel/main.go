// Command silo-tunnel is the k8s location's gateway sidecar (design
// §5.4): it runs beside the agent CLI in the session's pod, serves the
// gateway address on the pod's loopback, and carries each connection to
// it back to silo over the control connection silo opens with kubectl
// port-forward (package internal/tunnel). Its image is built from
// cmd/silo-tunnel/Dockerfile.
//
// A granule sandbox pod with repos (sandbox/k8s, design §9.3) runs it
// too, with -gateway unix:<path>: the Unix socket cloud-hypervisor
// connects a guest's vsock connection to, carried to the session's git
// proxy. In the guest, the same binary -- copied there with -self --
// runs with -guest-forward, listening on the guest's loopback for git
// and carrying each connection to the host over vsock.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/v2/internal/tunnel"
)

func main() {
	gateway := flag.String("gateway", "127.0.0.1:7811", "the address the agent CLI reaches the gateway at")
	control := flag.String("control", "127.0.0.1:7812", "the address silo's port-forward connects to")
	check := flag.Bool("check", false, "exit 0 at once: silo run --dry-run's check that the image runs")
	self := flag.Bool("self", false, "write this binary to stdout, for silo to copy into a granule guest")
	guest := flag.String("guest-forward", "", "in a granule guest: listen on this address and carry each connection to the host over vsock, to -vsock-port")
	vsockPort := flag.Uint("vsock-port", 0, "with -guest-forward, the host's vsock port")
	flag.Parse()
	if *check {
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	var err error
	switch {
	case *self:
		err = writeSelf()
	case *guest != "":
		err = guestForward(*guest, uint32(*vsockPort))
	default:
		err = serve(*gateway, *control)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "silo-tunnel: %v\n", err)
		os.Exit(1)
	}
}

// listen listens on addr, a TCP address or unix:<path>. A Unix socket
// is made writable by everyone, since the pod's other container (the
// VMM, which may run as another user) connects to it, and only this
// pod's containers can see it.
func listen(addr string) (net.Listener, error) {
	path, ok := strings.CutPrefix(addr, "unix:")
	if !ok {
		return net.Listen("tcp", addr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return l, os.Chmod(path, 0o666)
}

func writeSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

func guestForward(addr string, port uint32) error {
	if port == 0 {
		return fmt.Errorf("-guest-forward needs -vsock-port")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("forwarding %s to vsock %d:%d", l.Addr(), tunnel.HostCID, port)
	return tunnel.Forward(l, func() (io.ReadWriteCloser, error) { return tunnel.DialVsock(tunnel.HostCID, port) }, log.Printf)
}

func serve(gateway, control string) error {
	gl, err := listen(gateway)
	if err != nil {
		return err
	}
	cl, err := net.Listen("tcp", control)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Printf("gateway on %s, control on %s", gl.Addr(), cl.Addr())
	s := &tunnel.Sidecar{Logf: log.Printf}
	return s.Serve(ctx, gl, cl)
}
