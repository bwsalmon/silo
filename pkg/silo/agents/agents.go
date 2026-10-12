// Package agents is what silod knows about agents apart from the fence
// API that serves them (docs/silo/agents.md): the adapters this release
// ships, the deployment's credentials as the API sees them, the model
// price table (SV-111), the tool VM's startup script (SV-104), and the
// SessionRunner that runs harness sessions on the control VM.
//
// The harnesses themselves are package harness's: its Runner is the
// SessionRunner silod runs on a control VM with --sessiond-socket.
// Without one an agent stays PROVISIONING; tests use Fake.
package agents

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/silo/ids"
)

// The agent states (agents.md, "The agent object", SV-101).
const (
	StateProvisioning    = "PROVISIONING"
	StateRunning         = "RUNNING"
	StateIdle            = "IDLE"
	StateWaiting         = "WAITING"
	StateStopped         = "STOPPED"
	StateBudgetExhausted = "BUDGET_EXHAUSTED"
	StateDeleting        = "DELETING"
	StateFailed          = "FAILED"
)

// HoldsSession reports whether an agent in state holds one of its
// silo's agent_sessions (agents.md, "Sizing"): running, idle and
// waiting agents do; stopped, exhausted, deleting and failed ones
// don't. A PROVISIONING agent holds the slot it will run in, so that a
// create that passes the check can't then find none free.
func HoldsSession(state string) bool {
	switch state {
	case StateProvisioning, StateRunning, StateIdle, StateWaiting:
		return true
	}
	return false
}

// SessionsPerVM is how many harness sessions a control VM of cpus and
// memoryMB carries (agents.md, "Sizing", SV-117; install.md
// "Prerequisite checks", agent capacity): 0.2 vCPU and 0.5 GB per
// session, after 1 vCPU and 2 GB for silod, the gateway and the
// container runtime. Install's agent-capacity check and silod's own
// slot count both use it, so the slots the control VMs report add up
// to what install checked --agent-sessions against.
func SessionsPerVM(cpus, memoryMB int64) int {
	byCPU := float64(cpus-1) / 0.2
	byMem := float64(memoryMB-2048) / 512
	n := math.Floor(math.Min(byCPU+1e-9, byMem))
	if n < 0 {
		return 0
	}
	return int(n)
}

// SessionSlots is how many sessions one control VM runs: what it can
// carry, but no more than the fleet's --agent-sessions, the root's
// agent_sessions, which no number of sessions can exceed.
func SessionSlots(agentSessions int, cpus, memoryMB int64) int {
	return min(agentSessions, SessionsPerVM(cpus, memoryMB))
}

// The tool server's states.
const (
	ToolServerPending      = "PENDING"
	ToolServerConnected    = "CONNECTED"
	ToolServerDisconnected = "DISCONNECTED"
)

// Adapter is a harness this release can run (agents.md, "Adapters").
type Adapter struct {
	Name    string
	Version string
	// Providers are the model providers whose credentials it takes.
	Providers []string
}

// Adapters are the adapters v1 ships (call 39), by name.
var Adapters = map[string]Adapter{
	"claude-code": {Name: "claude-code", Version: "2.1.4", Providers: []string{"anthropic"}},
	"codex":       {Name: "codex", Version: "0.46.0", Providers: []string{"openai"}},
}

// AdapterNames lists the shipped adapters, sorted.
func AdapterNames() []string {
	var out []string
	for n := range Adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Takes reports whether a takes a credential of provider.
func (a Adapter) Takes(provider string) bool {
	for _, p := range a.Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// The kinds of credential (agents.md, §6, SV-108).
const (
	CredentialAPIKey       = "api_key"
	CredentialSubscription = "subscription"
)

// Credential is one of the deployment's agent credentials, as the
// fence API needs it: its name, the provider it logs in to, its kind,
// and how many sessions may use it at once (0: no cap). Its secret is
// the fence API's, sealed in its database (agents.md, §6).
type Credential struct {
	Name        string
	Provider    string
	Kind        string
	MaxSessions int
}

// Credentials finds the deployment's credentials by name.
type Credentials interface {
	Credential(ctx context.Context, name string) (Credential, bool, error)
	// Names lists every credential, for the root's effective policy,
	// which allows them all.
	Names(ctx context.Context) ([]string, error)
}

// StaticCredentials is a fixed set of credentials, name to provider.
type StaticCredentials map[string]string

// Credential implements Credentials.
func (s StaticCredentials) Credential(_ context.Context, name string) (Credential, bool, error) {
	p, ok := s[name]
	return Credential{Name: name, Provider: p, Kind: CredentialAPIKey}, ok, nil
}

// Names implements Credentials.
func (s StaticCredentials) Names(context.Context) ([]string, error) {
	var out []string
	for n := range s {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

var dnsLabel = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidName reports whether name is a DNS label, as an agent's name
// must be. The tool VM is <id>-agent-<name>, so the name leaves room
// for the silo's prefix within GCE's 63 characters.
func ValidName(name string) error {
	if !dnsLabel.MatchString(name) {
		return fmt.Errorf("agent name %q: want a DNS label (lower-case letters, digits and -, starting with a letter)", name)
	}
	if vm := InstanceName(strings.Repeat("x", ids.Len), name); len(vm) > 63 {
		return fmt.Errorf("agent name %q: too long; its tool VM, %s, must fit a GCE name with the silo's prefix", name, InstanceName("<id>", name))
	}
	return nil
}

// InstanceName is the name of the tool VM of silo's agent agent:
// <id>-agent-<name>, carrying the silo's prefix as every name in its
// facade must (gcp-backend.md, "Naming", SV-76).
func InstanceName(silo, agent string) string { return ids.NamePrefix(silo) + "agent-" + agent }

// LabelAgent is the hidden label silod puts on a tool VM, with the
// agent's id as its value. The facade hides labels starting with silo
// and refuses them in requests, so only silod can set it.
const LabelAgent = "silo-agent"

// ToolsVersion is the silo-tools release a tool VM runs.
const ToolsVersion = "v1.1.0"

// ToolsBinaryURL is where a tool VM fetches silo-tools from: the
// gateway's port 80, which the fence lets every silo reach.
const ToolsBinaryURL = "http://silo.internal/silo/v1/agents/-/tools/binary"

// StartupScriptKey is the metadata key of the startup script silod
// sets on a tool VM, and refuses in an agent's own request.
const StartupScriptKey = "startup-script"

// StartupScript is the tool VM's startup script (agents.md, "Creation",
// SV-104): fetch silo-tools for the VM's architecture unless the image
// carries the right version, and run it as a systemd unit. It needs
// systemd and a POSIX shell, and nothing else of the image.
func StartupScript() string {
	return strings.TrimLeft(`
#!/bin/sh
# silo-tools for a Silo agent's tool VM. Set by silod; not editable.
set -eu
bin=/usr/local/bin/silo-tools
want=`+ToolsVersion+`
if [ "$("$bin" --version 2>/dev/null || true)" != "$want" ]; then
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "silo-tools: unsupported architecture $(uname -m)" >&2; exit 1 ;;
  esac
  tmp="$bin.new"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --retry 10 --retry-delay 3 -o "$tmp" "`+ToolsBinaryURL+`?arch=$arch&version=$want"
  else
    wget -q -O "$tmp" "`+ToolsBinaryURL+`?arch=$arch&version=$want"
  fi
  chmod 0755 "$tmp"
  mv "$tmp" "$bin"
fi
id agent >/dev/null 2>&1 || useradd --create-home --shell /bin/bash agent
echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/silo-agent
chmod 0440 /etc/sudoers.d/silo-agent
cat > /etc/systemd/system/silo-tools.service <<'UNIT'
[Unit]
Description=Silo agent tool server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/silo-tools
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now silo-tools.service
`, "\n")
}

// Session is what a SessionRunner needs to start or resume an agent's
// harness.
type Session struct {
	Agent      string // the agent's id
	Silo       string // its silo's id
	Adapter    string
	Credential string
	Model      string
	Prompt     string
	// Resume is set to start from the saved session rather than the
	// prompt.
	Resume bool
	// Socket is the session socket the harness is given, the only one
	// mounted into its container (agents.md, §5, SV-107): set by the
	// session package's Runner, which opens it before the session
	// starts.
	Socket string
}

// TurnStarter is a SessionRunner that can be told an IDLE agent has a
// message to start a turn with (agents.md, §8, SV-112). The agent is
// RUNNING by then; the harness takes its messages from the fence API
// (fenceapi's Server.TakeMessages) and says when the turn is over
// (Server.EndTurn). A RUNNING agent's messages wait for its next turn,
// and tell the runner nothing.
type TurnStarter interface {
	StartTurn(ctx context.Context, agent string) error
}

// SessionRunner runs agents' harness sessions on the control VMs
// (package harness's Runner; the session socket is package session's).
// The fence API only starts and stops sessions through this.
//
// An agent's create finishes when its tool server connects; the runner
// reports that with the callback it is given (Server.ToolServerConnected
// in package fenceapi).
type SessionRunner interface {
	// HasSlot reports whether some control VM has a session slot free
	// (agents.md, "Placement on the control VMs").
	HasSlot(ctx context.Context) (bool, error)
	// Start starts, or with s.Resume resumes, a session.
	Start(ctx context.Context, s Session) error
	// Stop stops an agent's session, saving it. Stopping one that isn't
	// running does nothing.
	Stop(ctx context.Context, agent string) error
}

// Fake is a SessionRunner for tests. It records what it was asked,
// runs at most Slots sessions (unlimited if 0), and, with AutoConnect,
// reports each started agent's tool server as connected.
type Fake struct {
	Slots       int
	AutoConnect bool
	// Connected is called by Connect, and by Start with AutoConnect;
	// a test sets it to the fence API's ToolServerConnected.
	Connected func(ctx context.Context, agent string) error

	mu      sync.Mutex
	running map[string]Session
	log     []string
}

// SetSlots changes how many sessions f runs at most.
func (f *Fake) SetSlots(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Slots = n
}

// HasSlot implements SessionRunner.
func (f *Fake) HasSlot(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Slots == 0 || len(f.running) < f.Slots, nil
}

// Start implements SessionRunner.
func (f *Fake) Start(ctx context.Context, s Session) error {
	f.mu.Lock()
	if f.running == nil {
		f.running = map[string]Session{}
	}
	f.running[s.Agent] = s
	verb := "start"
	if s.Resume {
		verb = "resume"
	}
	f.log = append(f.log, verb+" "+s.Agent)
	auto := f.AutoConnect
	f.mu.Unlock()
	if auto {
		go f.Connect(context.WithoutCancel(ctx), s.Agent)
	}
	return nil
}

// Stop implements SessionRunner.
func (f *Fake) Stop(_ context.Context, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.running[agent]; ok {
		delete(f.running, agent)
		f.log = append(f.log, "stop "+agent)
	}
	return nil
}

// StartTurn implements TurnStarter: it records the turn.
func (f *Fake) StartTurn(_ context.Context, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "turn "+agent)
	return nil
}

// Connect says agent's tool server has connected.
func (f *Fake) Connect(ctx context.Context, agent string) error {
	if f.Connected == nil {
		return nil
	}
	return f.Connected(ctx, agent)
}

// Running lists the agents with a session, sorted.
func (f *Fake) Running() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for a := range f.running {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Log is every start, resume, stop and turn, in order.
func (f *Fake) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}
