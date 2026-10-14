// Command silo-tunnel is the k8s location's gateway sidecar (design
// §5.4): it runs beside the agent CLI in the session's pod, serves the
// gateway address on the pod's loopback, and carries each connection to
// it back to silo over the control connection silo opens with kubectl
// port-forward (package internal/tunnel). Its image is built from
// cmd/silo-tunnel/Dockerfile.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/silo/v2/internal/tunnel"
)

func main() {
	gateway := flag.String("gateway", "127.0.0.1:7811", "the address the agent CLI reaches the gateway at")
	control := flag.String("control", "127.0.0.1:7812", "the address silo's port-forward connects to")
	check := flag.Bool("check", false, "exit 0 at once: silo run --dry-run's check that the image runs")
	flag.Parse()
	if *check {
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := serve(*gateway, *control); err != nil {
		fmt.Fprintf(os.Stderr, "silo-tunnel: %v\n", err)
		os.Exit(1)
	}
}

func serve(gateway, control string) error {
	gl, err := net.Listen("tcp", gateway)
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
