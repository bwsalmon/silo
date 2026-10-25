package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/v3/bridge"
	"github.com/bwsalmon/silo/v3/gateway"
)

func runUICmd(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "token" {
		fs := flag.NewFlagSet("ui token", flag.ContinueOnError)
		fs.SetOutput(stderr)
		file := fs.String("file", bridge.DefaultUITokenFile, "path to write the SHA-256 token hash (mode 0600)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		tok, err := bridge.GenerateTokenFile(*file)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", tok)
		return err
	}

	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gwSock := fs.String("gateway-socket", gateway.DefaultSocketPath, "gateway Unix socket")
	brSock := fs.String("socket", bridge.DefaultSocketPath(), "bridge Unix socket")
	httpAddr := fs.String("http", bridge.DefaultHTTPAddr, "HTTP/WebSocket listen address (default 127.0.0.1:7070)")
	listenAddr := fs.String("listen", "", "alias for -http")
	sessDir := fs.String("sessions-dir", bridge.DefaultSessionsDir, "session transcripts directory")
	tokenFile := fs.String("token-file", bridge.DefaultUITokenFile, "UI login token SHA-256 hash file")
	rawToken := fs.String("token", os.Getenv("SILO_UI_TOKEN"), "raw UI login token (overrides -token-file)")
	iapMode := fs.Bool("iap", false, "authenticate requests via X-Goog-IAP-JWT-Assertion")
	iapOwners := fs.String("iap-owners", "", "comma-separated allowed IAP email addresses")
	if err := fs.Parse(args); err != nil {
		return err
	}

	addr := *httpAddr
	if *listenAddr != "" {
		addr = *listenAddr
	}

	auth, err := buildUIAuthenticator(*rawToken, *tokenFile, *iapMode, *iapOwners)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return bridge.Main(ctx, bridge.Config{
		GatewaySocket: *gwSock,
		BridgeSocket:  *brSock,
		HTTPAddr:      addr,
		SessionsDir:   *sessDir,
		Auth:          auth,
	})
}

func buildUIAuthenticator(rawToken, tokenFile string, iapMode bool, iapOwners string) (bridge.Authenticator, error) {
	if iapMode {
		var owners []string
		for _, p := range strings.Split(iapOwners, ",") {
			if s := strings.TrimSpace(p); s != "" {
				owners = append(owners, s)
			}
		}
		return &bridge.IAPAuthenticator{AllowedOwners: owners}, nil
	}
	if strings.TrimSpace(rawToken) != "" {
		return bridge.NewTokenAuthenticatorFromRaw(rawToken), nil
	}
	if tokenFile != "" {
		if _, err := os.Stat(tokenFile); err == nil {
			return bridge.LoadTokenAuthenticator(tokenFile)
		}
	}
	return nil, nil
}
