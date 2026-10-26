// Command silo is the Silo v3 single binary:
//
//	silo gateway [-config /etc/silo/gateway.toml]
//	silo bridge [-gateway-socket PATH] [-socket PATH] [-http ADDR] [-sessions-dir DIR]
//	silo ui [serve] [-http 127.0.0.1:7070] [-token-file PATH]
//	silo ui token [-file /var/lib/silo/ui-token.sha256]
//	silo connect [--socket PATH | --url ws://...] [--token TOKEN] -- <acp-cli-cmd...>
//	silo mcp [--socket PATH] [--token TOKEN]
//	silo page <add|rm|list> [flags]
//	silo secret <request|provide|reject|list> [flags]
//	silo call [-socket PATH] METHOD [PARAMS-JSON]
//	silo root propose BATCH.json                    the root agent proposes a batch
//	silo approvals list|show|approve|reject         the human's approval queue
//	silo instructions                               print the shipped default root.md
//
// Invoked as silo-gatewayd, silo-bridge, or silo-mcp (for example via a
// symlink), it dispatches to gateway, bridge, or mcp respectively.
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

	"github.com/bwsalmon/silo/v3/acp"
	"github.com/bwsalmon/silo/v3/bridge"
	"github.com/bwsalmon/silo/v3/gateway"
	"github.com/bwsalmon/silo/v3/instructions"
)

const usage = `usage: silo <command> [flags]

commands:
  gateway   run the root gateway daemon
  bridge    run the session bridge and HTTP/WebSocket hub
  ui        serve the web UI and bridge HTTP/WebSocket server (or 'silo ui token')
  connect   attach a coding-agent CLI (ACP stdio) to the session bridge
  mcp       run the stdio MCP server (silo-mcp) for a coding agent
  page      manage clickable UI page links (add, rm, list)
  secret    request or resolve agent secrets (request, provide, reject, list)
  call      make one JSON-RPC call to a Silo socket, e.g.
            silo call agent.list
            silo call agent.register '{"name":"web","runtime":"claude","location":{"kind":"container"}}'
  root      the root agent's run_as_root: silo root propose BATCH.json
  approvals the approval queue: silo approvals list|show|approve|reject
  instructions
            print the shipped default root agent instructions (root.md)
`

func main() {
	args := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "silo-gatewayd":
		args = append([]string{"gateway"}, args...)
	case "silo-bridge":
		args = append([]string{"bridge"}, args...)
	case "silo-mcp":
		args = append([]string{"mcp"}, args...)
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

	case "bridge":
		fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
		fs.SetOutput(stderr)
		gwSock := fs.String("gateway-socket", gateway.DefaultSocketPath, "gateway Unix socket")
		brSock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		httpAddr := fs.String("http", bridge.DefaultHTTPAddr, "HTTP/WebSocket listen address")
		sessDir := fs.String("sessions-dir", bridge.DefaultSessionsDir, "session transcripts directory")
		tokenFile := fs.String("token-file", bridge.DefaultUITokenFile, "UI login token SHA-256 hash file")
		rawToken := fs.String("token", os.Getenv("SILO_UI_TOKEN"), "raw UI login token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		auth, err := buildUIAuthenticator(*rawToken, *tokenFile, false, "")
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return bridge.Main(ctx, bridge.Config{
			GatewaySocket: *gwSock,
			BridgeSocket:  *brSock,
			HTTPAddr:      *httpAddr,
			SessionsDir:   *sessDir,
			Auth:          auth,
		})

	case "ui":
		return runUICmd(args[1:], stdout, stderr)

	case "connect":
		return runConnectCmd(args[1:], stderr)

	case "mcp":
		fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "agent token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return bridge.RunMCP(ctx, bridge.MCPOptions{
			SocketPath: *sock,
			Token:      *tok,
			Stdin:      os.Stdin,
			Stdout:     stdout,
		})

	case "page":
		return runPageCmd(args[1:], stdout, stderr)

	case "secret":
		return runSecretCmd(args[1:], stdout, stderr)

	case "call":
		fs := flag.NewFlagSet("call", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", gateway.DefaultSocketPath, "gateway or bridge socket")
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

	case "root":
		return runRoot(args[1:], stdout, stderr)
	case "approvals":
		return runApprovals(args[1:], stdout, stderr)
	case "instructions":
		_, err := io.WriteString(stdout, instructions.DefaultRootMD)
		return err
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func runConnectCmd(args []string, stderr io.Writer) error {
	// Split at `--` if present so CLI flags after `--` are passed straight to the ACP command.
	flagArgs := args
	var cmdArgs []string
	for i, a := range args {
		if a == "--" {
			flagArgs = args[:i]
			cmdArgs = args[i+1:]
			break
		}
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("socket", "", "bridge Unix socket (default /run/silo/bridge.sock)")
	urlFlag := fs.String("url", "", "bridge WebSocket URL (e.g. ws://host:7070/connect)")
	tok := fs.String("token", "", "agent registration token (default $SILO_AGENT_TOKEN)")
	clisDir := fs.String("clis-dir", acp.DefaultCLIsDir, "CLI preset directory")
	schedule := fs.String("schedule", "", "optional schedule name")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(cmdArgs) == 0 {
		cmdArgs = fs.Args()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return bridge.RunConnect(ctx, bridge.ConnectOptions{
		SocketPath: *sock,
		URL:        *urlFlag,
		Token:      *tok,
		CLIsDir:    *clisDir,
		Schedule:   *schedule,
		Command:    cmdArgs,
		Stderr:     stderr,
	})
}

func runPageCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: silo page <add|rm|list> [flags]")
	}
	sub := args[0]
	switch sub {
	case "add", "register":
		fs := flag.NewFlagSet("page "+sub, flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "agent token")
		slug := fs.String("slug", "", "page slug served under /p/<slug>/")
		title := fs.String("title", "", "page title")
		port := fs.Int("port", 0, "internal TCP port to reverse-proxy")
		host := fs.String("host", "", "target host/IP for internal port (default 127.0.0.1)")
		exposePort := fs.Int("expose-port", 0, "optional public listener port")
		extURL := fs.String("url", "", "external http:// or https:// URL")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		params := map[string]any{
			"slug":        *slug,
			"title":       *title,
			"port":        *port,
			"host":        *host,
			"expose_port": *exposePort,
			"url":         *extURL,
		}
		if *tok != "" {
			params["agent_token"] = *tok
		}
		return callAndPrint(stdout, *sock, "page.register", params)

	case "rm", "unregister":
		fs := flag.NewFlagSet("page "+sub, flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "agent token")
		slug := fs.String("slug", "", "page slug to remove")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		target := *slug
		if target == "" && fs.NArg() > 0 {
			target = fs.Arg(0)
		}
		if target == "" {
			return errors.New("usage: silo page rm [--slug SLUG] <slug>")
		}
		params := map[string]any{"slug": target}
		if *tok != "" {
			params["agent_token"] = *tok
		}
		return callAndPrint(stdout, *sock, "page.unregister", params)

	case "list":
		fs := flag.NewFlagSet("page list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "agent token")
		all := fs.Bool("all", false, "include inactive page links")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		params := map[string]any{"all": *all}
		if *tok != "" {
			params["agent_token"] = *tok
		}
		return callAndPrint(stdout, *sock, "page.list", params)

	default:
		return fmt.Errorf("unknown page subcommand %q (want add, rm, or list)", sub)
	}
}

func runSecretCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: silo secret <request|provide|reject|list> [flags]")
	}
	sub := args[0]
	switch sub {
	case "request":
		fs := flag.NewFlagSet("secret request", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		tok := fs.String("token", os.Getenv("SILO_AGENT_TOKEN"), "agent token")
		name := fs.String("name", "", "secret name")
		reason := fs.String("reason", "", "why the secret is needed")
		path := fs.String("path", "", "optional file path (default ~/.config/silo/secrets/<name>)")
		noWait := fs.Bool("no-wait", false, "return immediately instead of waiting for human fulfillment")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		params := map[string]any{
			"name":   *name,
			"reason": *reason,
			"path":   *path,
			"wait":   !*noWait,
		}
		if *tok != "" {
			params["agent_token"] = *tok
		}
		return callAndPrint(stdout, *sock, "secret.request", params)

	case "provide":
		fs := flag.NewFlagSet("secret provide", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		id := fs.String("id", "", "secret request id")
		val := fs.String("value", "", "secret value to write via silo connect")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return callAndPrint(stdout, *sock, "secret.provide", map[string]any{
			"id":    *id,
			"value": *val,
		})

	case "reject":
		fs := flag.NewFlagSet("secret reject", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		id := fs.String("id", "", "secret request id")
		note := fs.String("note", "", "rejection note")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return callAndPrint(stdout, *sock, "secret.reject", map[string]any{
			"id":   *id,
			"note": *note,
		})

	case "list":
		fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
		pending := fs.Bool("pending", false, "only pending secret requests")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return callAndPrint(stdout, *sock, "secret.list", map[string]any{
			"pending": *pending,
		})

	default:
		return fmt.Errorf("unknown secret subcommand %q (want request, provide, reject, or list)", sub)
	}
}

func callAndPrint(stdout io.Writer, sock, method string, params any) error {
	var result json.RawMessage
	if err := bridge.NewClient(sock).Call(context.Background(), method, params, &result); err != nil {
		return err
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", out)
	return err
}
