// Command silo-tools is the MCP tool server on a Silo agent's tool VM
// (docs/silo/agents.md, "Tools", SV-105, and "The tool channel",
// SV-106). The tool VM's startup script fetches it from the gateway
// and runs it as the systemd unit silo-tools.service, as root.
//
// It listens on nothing. It dials the gateway's port 80 at
// http://silo.internal/silo/v1/agents/-/tools:attach, follows a
// redirect to the gateway of the control VM hosting the agent's
// session, and serves its tools -- exec, read_file, write_file,
// edit_file and list_dir -- down that channel, attaching again with
// backoff whenever it drops. Commands run as the user agent.
//
// It is built static, for amd64 and arm64:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o silo-tools-linux-amd64 ./cmd/silo-tools
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o silo-tools-linux-arm64 ./cmd/silo-tools
//
// and the gateway serves those files from its --tools-dir.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwsalmon/silo/pkg/silo/toolchan"
	"github.com/bwsalmon/silo/pkg/silo/tools"
)

func main() {
	attach := flag.String("attach", toolchan.DefaultURL, "the tool channel's attach URL, on the gateway's port 80")
	user := flag.String("user", "agent", "the user commands run as, when silo-tools runs as root")
	proxy := flag.String("proxy", "http://silo.internal:3128", "the egress proxy, set as HTTPS_PROXY and HTTP_PROXY for commands")
	noProxy := flag.String("no-proxy", "localhost,127.0.0.1,::1,silo.internal,metadata.google.internal,169.254.169.254", "NO_PROXY for commands")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(tools.Version)
		return
	}
	srv, err := tools.New(tools.Options{User: *user, Proxy: *proxy, NoProxy: *noProxy})
	if err != nil {
		log.Fatalf("silo-tools: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &toolchan.Client{URL: *attach, Handler: srv, Attached: func(u string) {
		log.Printf("silo-tools %s: the tool channel is up, at %s", tools.Version, u)
	}}
	c.Run(ctx)
}
