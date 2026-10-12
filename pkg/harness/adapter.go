// Package harness runs agents' harness sessions on the control VMs
// (docs/silo/agents.md, §3 "v1 placement", SV-102; "Adapters", SV-103;
// §9 "silod restarts", SV-115; §10 "Capacity of the control VM",
// SV-116 and SV-117; §11 "Isolation on the control VM", SV-118).
//
// It has three parts:
//
//   - the adapters, claude-code and codex (call 39): how to start each
//     CLI for one turn with a prompt, a model and an MCP configuration
//     naming the session socket, with its built-in tools off; how to
//     resume it from saved state; how to read its event stream; and
//     which credential kinds it takes;
//   - the container spec (Spec) each turn runs in, and the session
//     launcher, sessiond, which alone holds the container runtime and
//     refuses any spec that loosens the isolation;
//   - the Runner, the agents.SessionRunner silod runs: placement on the
//     control VM with the most free slots, one turn loop a session,
//     the harness's state saved to the database after every turn, and
//     on start the leftover containers removed and this VM's sessions
//     resumed.
//
// A harness runs one turn at a time: the CLI is started in print mode
// on the turn's prompt, and exits when the turn ends. Between turns
// nothing of the session runs; its state is the CLI's own session file
// in the session's private directory, and its copy in the database.
package harness

import (
	"sort"
	"strings"
)

// Credential kinds (agents.md, §6).
const (
	KindAPIKey       = "api_key"
	KindSubscription = "subscription"
)

// Paths inside a session's container.
const (
	// Home is the session's private writable directory, the CLI's HOME.
	Home = "/state"
	// WorkDir is the CLI's working directory, under Home. Nothing
	// runs in it: the tools are the tool VM's.
	WorkDir = Home + "/work"
	// SocketPath is where the session socket is mounted.
	SocketPath = "/run/silo/silo.sock"
	// Endpoint is the session's forwarder (silod harness-init): HTTP
	// on the container's loopback, carried to SocketPath unchanged,
	// for CLIs that speak HTTP to MCP servers and model APIs but not
	// over a Unix socket. It is the socket, by another door: it adds
	// nothing a connection to the socket can't do.
	Endpoint = "http://127.0.0.1:4180"
	// Init is the container's entrypoint: the forwarder, which then
	// runs the CLI as its child.
	Init = "/usr/local/bin/silod"
)

// ProxyKey is the API key a CLI is given: none. The model proxy on the
// session socket adds the credential's key on the way out (agents.md,
// §6); the CLIs refuse to start without one.
const ProxyKey = "silo-model-proxy"

// Launch is one turn of a session, as an adapter builds its command.
type Launch struct {
	Model  string
	Prompt string
	// SessionID is the CLI's session: chosen by silod for a new
	// claude-code session, reported by the CLI for codex.
	SessionID string
	// Resume is set to continue SessionID from its saved state.
	Resume bool
	// Subscription is set for a subscription session: the CLI uses
	// its own login, through the egress proxy, not the model proxy
	// (agents.md, §6, SV-108).
	Subscription bool
}

// Command is what a container runs for one turn.
type Command struct {
	Argv []string
	Env  []string
}

// Adapter is the code that knows how to run one harness (agents.md,
// "Adapters").
type Adapter interface {
	Name() string
	// Command is the CLI invocation for one turn.
	Command(l Launch) Command
	// Parser reads one turn's event stream, a JSON object a line.
	Parser() Parser
	// StatePaths are the paths, relative to Home, that hold the CLI's
	// session state: saved to the database after every turn and put
	// back before a session resumes on any control VM.
	StatePaths() []string
	// NewSessionID is the session id to start a new session with, or
	// "" if the CLI picks its own and reports it.
	NewSessionID() string
	// CredentialKinds are the kinds of credential it takes.
	CredentialKinds() []string
}

// Parser turns a CLI's output lines into events.
type Parser interface {
	Line(line []byte) []Event
}

// The kinds of Event.
const (
	// EvSession reports the CLI's session id.
	EvSession = "session"
	// EvReply is text the agent said.
	EvReply = "reply"
	// EvToolCall is a tool call through the session socket's MCP
	// server, which the socket records itself.
	EvToolCall = "tool_call"
	// EvLocalTool is a call of a built-in tool, which the adapter
	// turned off: an adapter bug, recorded as an error.
	EvLocalTool = "local_tool"
	// EvUsage is model usage as the CLI counts it.
	EvUsage = "usage"
	// EvError is an error the CLI reported.
	EvError = "error"
	// EvTurnEnd is the end of the turn.
	EvTurnEnd = "turn_end"
)

// Event is one thing a CLI's stream said.
type Event struct {
	Kind      string
	SessionID string
	Text      string
	Tool      string
	ToolID    string
	Usage     map[string]any
	// Tools are the tools the CLI says it has, on EvSession, when it
	// lists them: anything but the session's MCP tools is an error.
	Tools []string
}

// Adapters are the adapters v1 ships, by name.
var Adapters = map[string]Adapter{
	"claude-code": ClaudeCode{},
	"codex":       Codex{},
}

// Names lists the adapters, sorted.
func Names() []string {
	var out []string
	for n := range Adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// mcpServer is the session's one MCP server's name in a CLI's
// configuration; its tools are mcp__silo__<tool> to Claude Code.
const mcpServer = "silo"

func modelBase(provider string) string { return Endpoint + "/model/" + provider }

func isSiloTool(name string) bool {
	return strings.HasPrefix(name, "mcp__"+mcpServer+"__")
}
