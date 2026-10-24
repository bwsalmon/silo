package gateway

import (
	"context"
	"fmt"
	"strings"
)

// LaunchRequest is what a Launcher knows about the agent it starts.
type LaunchRequest struct {
	Agent string
	CLI   string
	Brief string
	User  UserInfo
	Root  bool
}

// Process is the command a Launcher wants run in the agent's unit.
type Process struct {
	Argv []string
	Env  map[string]string
}

// Launcher decides what runs inside an agent's unit. M1a's CLILauncher runs
// the configured CLI command; M1b replaces it with the ACP launcher, which
// starts the CLI's ACP adapter and hands its stdio to the bridge.
type Launcher interface {
	Prepare(ctx context.Context, req LaunchRequest) (*Process, error)
}

// CLILauncher runs the command configured for the cli name in
// /etc/silo/clis.d.
type CLILauncher struct {
	CLIs map[string]CLIDef
}

func (l *CLILauncher) Prepare(_ context.Context, req LaunchRequest) (*Process, error) {
	def, ok := l.CLIs[req.CLI]
	if !ok {
		return nil, &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown cli %q (configured: %s)", req.CLI, strings.Join(l.names(), ", "))}
	}
	r := strings.NewReplacer("{brief}", req.Brief, "{agent}", req.Agent, "{home}", req.User.Home)
	p := &Process{Env: map[string]string{}}
	for _, a := range def.Command {
		p.Argv = append(p.Argv, r.Replace(a))
	}
	for k, v := range def.Env {
		p.Env[k] = v
	}
	return p, nil
}

func (l *CLILauncher) names() []string {
	var out []string
	for n := range l.CLIs {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}
