package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/hub/conformance"
	"github.com/bwsalmon/silo/hub/conformance/refgrain"
)

// errNonconforming is a run of the kit whose Grain failed a scenario.
var errNonconforming = errors.New("the grain does not conform")

// conformanceKit runs the conformance kit (docs/conformance.md): a hub
// of its own, the Grain under test pointed at it, and the scenarios.
func conformanceKit(args []string) error {
	fs := flag.NewFlagSet("silo conformance", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: silo conformance [flags] [-- command [args...]]

Runs a hub, mints a node token, and checks that the Grain which registers
with it speaks the task interface (docs/conformance.md). The command, if
given, is started with the Grain's settings in its environment:
SILO_HUB_URL, SILO_NODE_TOKEN, SILO_NODE_ID, SILO_CONFORMANCE_REPO and
SILO_CONFORMANCE_OUTSIDE_REPO. Without one, they are printed (and the
token written to -token-file) for a Grain started by hand.

Exits 0 when the Grain conforms, 1 when it does not.`)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:0", "where the kit's hub listens")
	node := fs.String("node", conformance.DefaultNode, "the node ID the token is minted for")
	repo := fs.String("repo", conformance.DefaultRepo, "the repository the kit's tasks are in, which the Grain's local policy allows")
	outside := fs.String("outside-repo", conformance.DefaultOutsideRepo, `a repository the Grain registers for but its policy refuses; "-" skips that scenario`)
	tokenFile := fs.String("token-file", "", "write the node token to this file")
	jsonOut := fs.String("json", "", "write the machine-readable verdict to this file (- for stdout)")
	heartbeat := fs.Duration("heartbeat", time.Second, "the heartbeat the hub asks for")
	register := fs.Duration("register-timeout", 2*time.Minute, "how long the Grain has to register")
	timeout := fs.Duration("timeout", 90*time.Second, "how long each wait inside a scenario may take")
	maxSlots := fs.Int("max-slots", conformance.DefaultMaxSlots, "the most slots the capacity scenario fills")
	only := fs.String("only", "", "comma-separated scenarios to run, of "+strings.Join(conformance.Scenarios, ","))
	verbose := fs.Bool("v", false, "log the hub and the kit's progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := fs.Args()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var grain *exec.Cmd
	cfg := conformance.Config{
		Addr: *addr, Node: *node, Repo: *repo, OutsideRepo: *outside,
		Heartbeat: *heartbeat, Register: *register, Timeout: *timeout, MaxSlots: *maxSlots,
		Only: list(*only),
		Start: func(env conformance.Env) error {
			if *tokenFile != "" {
				if err := os.WriteFile(*tokenFile, []byte(env.Token+"\n"), 0o600); err != nil {
					return err
				}
			}
			vars := []string{"SILO_HUB_URL=" + env.Hub, "SILO_NODE_TOKEN=" + env.Token, "SILO_NODE_ID=" + env.Node,
				"SILO_CONFORMANCE_REPO=" + env.Repo, "SILO_CONFORMANCE_OUTSIDE_REPO=" + env.OutsideRepo}
			if len(command) == 0 {
				fmt.Fprintf(os.Stderr, "silo conformance: start the grain under test with:\n  %s\n", strings.Join(vars, "\n  "))
				return nil
			}
			grain = exec.Command(command[0], command[1:]...)
			grain.Env = append(os.Environ(), vars...)
			grain.Stdout, grain.Stderr = os.Stderr, os.Stderr
			return grain.Start()
		},
	}
	if *verbose {
		cfg.Logf = log.Printf
	}
	rep, err := conformance.Run(ctx, cfg)
	if grain != nil && grain.Process != nil {
		grain.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { grain.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			grain.Process.Kill()
			<-done
		}
	}
	if rep != nil {
		rep.WriteText(os.Stdout)
		if *jsonOut != "" {
			out := os.Stdout
			if *jsonOut != "-" {
				f, ferr := os.Create(*jsonOut)
				if ferr != nil {
					return ferr
				}
				defer f.Close()
				out = f
			}
			if werr := rep.WriteJSON(out); werr != nil {
				return werr
			}
		}
	}
	if err != nil {
		return err
	}
	if !rep.Passed {
		return errNonconforming
	}
	return nil
}

// conformanceGrain runs the reference Grain (hub/conformance/refgrain).
func conformanceGrain(args []string) error {
	fs := flag.NewFlagSet("silo conformance grain", flag.ContinueOnError)
	hub := fs.String("hub", env("SILO_HUB_URL", ""), "the hub's URL [SILO_HUB_URL]")
	token := fs.String("token", env("SILO_NODE_TOKEN", ""), "the node token [SILO_NODE_TOKEN]")
	node := fs.String("node", env("SILO_NODE_ID", conformance.DefaultNode), "the node ID [SILO_NODE_ID]")
	repos := fs.String("repos", strings.Join(list(env("SILO_CONFORMANCE_REPO", conformance.DefaultRepo)+","+env("SILO_CONFORMANCE_OUTSIDE_REPO", conformance.DefaultOutsideRepo)), ","),
		"comma-separated repositories to register for [SILO_CONFORMANCE_REPO,SILO_CONFORMANCE_OUTSIDE_REPO]")
	allow := fs.String("allow", env("SILO_CONFORMANCE_REPO", conformance.DefaultRepo), "comma-separated repositories the local policy allows [SILO_CONFORMANCE_REPO]")
	slots := fs.Int("slots", 2, "how many tasks to run at once")
	features := fs.String("features", refgrain.FeatureInteractive+","+refgrain.FeatureApprovals, "comma-separated features to advertise")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hub == "" || *token == "" {
		return errors.New("silo conformance grain: -hub and -token are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	g := &refgrain.Grain{Hub: *hub, Token: *token, ID: *node, Repos: list(*repos), Allow: list(*allow),
		Slots: *slots, Features: list(*features), Logf: log.Printf}
	if err := g.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
