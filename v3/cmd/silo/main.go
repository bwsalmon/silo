// Command silo is silo v3's single binary. Each component is a subcommand:
//
//	silo gateway [-config /etc/silo/gateway.toml]   the root daemon
//	silo call [-socket PATH] METHOD [PARAMS-JSON]   one gateway API call
//
// Invoked as silo-gatewayd (for example through a symlink, as the design's
// unit names it), it runs the gateway.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bwsalmon/silo/v3/gateway"
)

const usage = `usage: silo <command> [flags]

commands:
  gateway   run the gateway daemon (as root)
  call      make one call to the gateway API, e.g.
            silo call agent.list
            silo call agent.register '{"name":"web","runtime":"claude","location":{"kind":"container"}}'
            silo call agent.message '{"agent":"main","text":"..."}'
`

func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "silo-gatewayd" {
		args = append([]string{"gateway"}, args...)
	}
	if err := run(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "silo:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "gateway":
		fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
		fs.SetOutput(stderr)
		config := fs.String("config", gateway.DefaultConfigPath, "config file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return gateway.Main(ctx, *config)
	case "call":
		fs := flag.NewFlagSet("call", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", gateway.DefaultSocketPath, "gateway socket")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 || fs.NArg() > 2 {
			return errors.New("usage: silo call [-socket PATH] METHOD [PARAMS-JSON]")
		}
		var params any
		if fs.NArg() == 2 {
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(fs.Arg(1)), &raw); err != nil {
				return fmt.Errorf("params: %w", err)
			}
			params = raw
		}
		c, err := gateway.Dial(context.Background(), *sock)
		if err != nil {
			return err
		}
		defer c.Close()
		var result json.RawMessage
		if err := c.Call(fs.Arg(0), params, &result); err != nil {
			return err
		}
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", out)
		return err
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}
