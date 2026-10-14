// Adapted from github.com/bwsalmon/grain pkg/agent/codex/codex.go, pkg/agent/codex/runtime.go and pkg/agent/codex/auth.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package codex maps a Silofile onto the Codex CLI (cli-mapping.md §2.2,
// §3). Every setting gets a private CODEX_HOME in the staging directory,
// so the user's own config.toml, MCP servers and profiles do not apply.
// For remote_permissions none that home also holds grain's reviewed
// no-environment configuration (grain docs/codex-isolation.md), which is
// trusted only on a reviewed Codex version.
package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() { agentcli.Register("codex", Mapping{}) }

// ReviewedVersions are the Codex versions whose tool registry has been
// reviewed against the no-environment configuration that implements
// remote_permissions: none. It is grain's ReviewedCLIVersion table
// (grain pkg/agent/codex/runtime.go), deliberately exact, not a minimum:
// an older CLI may ignore the configuration, and a newer one may add
// tools that are not environment-backed. Adding a version is a reviewed
// change (design open question 3), never automatic.
var ReviewedVersions = []string{"0.153.0"}

// Staged names, relative to the staging directory.
const (
	Home             = "codex-home"
	configFile       = "config.toml"
	environmentsFile = "environments.toml"
	authFile         = "auth.json"
)

// APIKeyVar is set from agent.auth: {credential: x} (design §8.2).
const APIKeyVar = "OPENAI_API_KEY"

// Mapping is Codex.
type Mapping struct{}

func (Mapping) VersionArgs() []string { return []string{"--version"} }

// ParseVersion reads "codex-cli 0.153.0".
func (Mapping) ParseVersion(out string) (string, error) { return agentcli.SemVer(out) }

func reviewed(version string) bool {
	for _, v := range ReviewedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// tomlString renders a Go string as a TOML basic string, which is
// spelled exactly as a JSON string.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Config is the private CODEX_HOME's config.toml.
func Config(req agentcli.Request) []byte {
	none := req.Permissions.Effective() == silofile.PermNone
	var b strings.Builder
	b.WriteString("# Written by silo for one session; see cli-mapping.md §2.2.\n")
	if none {
		b.WriteString("approval_policy = \"never\"\nsandbox_mode = \"read-only\"\n")
		b.WriteString("web_search = \"disabled\"\ninclude_environment_context = false\n")
	}
	if req.Agent.Effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", tomlString(req.Agent.Effort))
	}
	if req.Agent.Auth.Ambient() {
		b.WriteString("cli_auth_credentials_store = \"file\"\n")
	}
	if req.SystemPrompt != "" {
		fmt.Fprintf(&b, "developer_instructions = %s\n", tomlString(req.SystemPrompt))
	}
	if none {
		b.WriteString("[features]\napps = false\nplugins = false\nmulti_agent = false\nmulti_agent_v2 = false\n")
		b.WriteString("[orchestrator.skills]\nenabled = false\n")
		b.WriteString("[skills.bundled]\nenabled = false\n")
	}
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", agentcli.ServerName)
	fmt.Fprintf(&b, "url = %s\nhttp_headers = { Authorization = %s }\nrequired = true\n",
		tomlString(req.GatewayURL), tomlString("Bearer "+req.GatewayToken))
	fmt.Fprintf(&b, "tool_timeout_sec = %d\n", int(agentcli.ToolTimeout.Seconds()))
	// Only the gateway's own tools are enabled and preapproved.
	tools, _ := json.Marshal(append([]string{}, req.GatewayTools...))
	fmt.Fprintf(&b, "enabled_tools = %s\n", tools)
	for _, name := range req.GatewayTools {
		fmt.Fprintf(&b, "[mcp_servers.%s.tools.%s]\napproval_mode = \"approve\"\n", agentcli.ServerName, tomlString(name))
	}
	return []byte(b.String())
}

// Environments is the manifest that withholds Codex's execution
// environments: no [[environments]] and include_local = false, so not
// even a read-only local environment is registered.
const Environments = "default = \"none\"\ninclude_local = false\n"

// AmbientAuthFile is the user's Codex login file: $CODEX_HOME/auth.json,
// or ~/.codex/auth.json.
func AmbientAuthFile(env []string, home string) string {
	if h := agentcli.Getenv(env, "CODEX_HOME"); h != "" {
		return filepath.Join(h, authFile)
	}
	return filepath.Join(home, ".codex", authFile)
}

func (Mapping) Build(req agentcli.Request) (agentcli.Plan, error) {
	if err := agentcli.CheckMode(req.Mode); err != nil {
		return agentcli.Plan{}, err
	}
	perm := req.Permissions.Effective()
	switch {
	case perm == silofile.PermSubset:
		return agentcli.Plan{}, agentcli.Refusef("codex cannot express a remote_permissions subset: it has no per-tool allowlist for its built-in tools; use none or all (cli-mapping.md §2.2)")
	case perm == silofile.PermNone && !reviewed(req.Host.Version):
		return agentcli.Plan{}, agentcli.Refusef("remote_permissions none on codex %s is refused: silo's no-environment configuration has been reviewed only for codex-cli %s (cli-mapping.md §2.2); install a reviewed version, or use remote_permissions: all",
			req.Host.Version, strings.Join(ReviewedVersions, ", "))
	}

	plan := agentcli.Plan{Files: []location.StagedFile{
		{Name: filepath.Join(Home, configFile), Data: Config(req)},
	}}
	if perm == silofile.PermNone {
		plan.Files = append(plan.Files, location.StagedFile{Name: filepath.Join(Home, environmentsFile), Data: []byte(Environments)})
	}
	if req.Agent.Auth.Ambient() {
		auth := AmbientAuthFile(req.Host.Env, req.Host.Home)
		if fi, err := os.Stat(auth); err != nil || !fi.Mode().IsRegular() {
			return agentcli.Plan{}, &location.PrerequisiteError{
				What: fmt.Sprintf("codex has no file login at %s", auth),
				Fix:  "run `codex login` (silo links that file into its private CODEX_HOME), or set agent.auth to a credential",
			}
		}
		plan.Files = append(plan.Files, location.StagedFile{Name: filepath.Join(Home, authFile), Link: auth})
	}

	plan.Command = func(dir string) location.Command {
		env := map[string]string{"CODEX_HOME": filepath.Join(dir, Home)}
		if perm == silofile.PermNone {
			env["CODEX_EXEC_SERVER_URL"] = "none"
		}
		for k, v := range req.Env {
			env[k] = v
		}
		argv := []string{req.Host.Binary}
		cmd := location.Command{Env: agentcli.MergeEnv(req.Host.Env, env)}
		if req.Mode == agentcli.Headless {
			argv = append(argv, "exec", "--json", "--skip-git-repo-check")
			if perm == silofile.PermAll {
				// A run must never block on an approval nobody can give.
				argv = append(argv, "-c", `approval_policy="never"`)
			}
		}
		if req.Agent.Model != "" {
			argv = append(argv, "-m", req.Agent.Model)
		}
		switch {
		case req.Mode == agentcli.Headless:
			// "-" reads the prompt from stdin, never argv, where ps
			// would show it.
			argv = append(argv, "-")
			cmd.Stdin = []byte(req.Prompt)
		case req.Prompt != "":
			argv = append(argv, "--", req.Prompt)
		}
		cmd.Argv = argv
		cmd.TTY = req.Mode == agentcli.Interactive
		return cmd
	}
	return plan, nil
}
