// Command mcpserver is stage 5a's test MCP server (internal/mcptest) as
// a binary of its own, installed in the sshd test image as
// /usr/local/bin/mcpserver for the remote MCP server tests: with no
// arguments it serves over stdio; with -http <file> it serves Streamable
// HTTP on a loopback port and writes {"url", "token"} to <file>.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bwsalmon/silo/v2/internal/mcptest"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-http" {
		srv, err := mcptest.New(nil).Listen(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		b, _ := json.Marshal(map[string]string{"url": srv.URL, "token": srv.Token})
		if err := os.WriteFile(os.Args[2], b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		select {}
	}
	os.Setenv(mcptest.ModeVar, "stdio")
	mcptest.MaybeServe()
}
