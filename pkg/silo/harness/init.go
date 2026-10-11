package harness

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// HarnessInit is a session container's entrypoint (silod harness-init):
// it carries TCP connections on listen, the container's loopback, to
// the session socket, byte for byte, and runs argv as its child with
// its own stdio, exiting as the child does. The forwarder is the
// socket by another door, for CLIs that only speak HTTP over TCP: the
// network namespace has nothing else, so it adds no way out.
func HarnessInit(ctx context.Context, socket, listen string, argv []string) (int, error) {
	if len(argv) == 0 {
		return 2, errors.New("harness-init: no command")
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return 1, err
	}
	defer l.Close()
	go Forward(l, socket)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The CLI gets the container's environment, which is the adapter's
	// (Spec.Env) and holds no credential: the model proxy adds the key.
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return 127, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	go func() {
		for s := range sig {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Forward carries each connection l accepts to the Unix socket.
func Forward(l net.Listener, socket string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("unix", socket)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(u, c); closeWrite(u); done <- struct{}{} }()
			go func() { io.Copy(c, u); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
