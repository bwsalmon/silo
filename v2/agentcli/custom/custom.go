// Package custom maps a Silofile onto a custom CLI using the mapping the
// Silofile declares (cli-mapping.md §2.4). Any setting it does not
// declare is refused.
package custom

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/agentcli/claude"
	"github.com/bwsalmon/silo/v2/location"
	"github.com/bwsalmon/silo/v2/silofile"
)

func init() { agentcli.Register("custom", Mapping{}) }

// Staged file names.
const (
	PromptFile = "prompt.md"
	JSONConfig = "mcp.json"
	TOMLConfig = "mcp.toml"
)

// Mapping is a custom CLI.
type Mapping struct{}

// VersionArgs is nil: a custom CLI's version flag is not known.
func (Mapping) VersionArgs() []string { return nil }

func (Mapping) ParseVersion(out string) (string, error) { return strings.TrimSpace(out), nil }

func (Mapping) Build(req agentcli.Request) (agentcli.Plan, error) {
	if err := agentcli.CheckMode(req.Mode); err != nil {
		return agentcli.Plan{}, err
	}
	c := req.Agent.Custom
	if c == nil {
		return agentcli.Plan{}, agentcli.Refusef("cli: custom needs a custom: mapping (cli-mapping.md §2.4)")
	}
	var perm []string
	switch p := req.Permissions.Effective(); p {
	case silofile.PermNone, silofile.PermAll:
		decl := c.Permissions.None
		if p == silofile.PermAll {
			decl = c.Permissions.All
		}
		if decl == nil {
			return agentcli.Plan{}, agentcli.Refusef("remote_permissions %s is refused: agent.custom.permissions declares no %s", p, p)
		}
		perm = *decl
	default:
		return agentcli.Plan{}, agentcli.Refusef("a custom CLI cannot be given a remote_permissions subset: declare none or all in agent.custom.permissions (design §6.1)")
	}
	modeArgs := c.Argv.Interactive
	if req.Mode == agentcli.Headless {
		modeArgs = c.Argv.Headless
	}

	var file string
	plan := agentcli.Plan{}
	switch c.MCPConfig.Format {
	case "claude-json":
		file = JSONConfig
		plan.Files = append(plan.Files, location.StagedFile{Name: file, Data: claude.MCPConfig(req.GatewayURL, req.GatewayToken)})
	case "codex-toml":
		file = TOMLConfig
		data := fmt.Sprintf("[mcp_servers.%s]\nurl = %s\nhttp_headers = { Authorization = %s }\nrequired = true\n",
			agentcli.ServerName, tomlString(req.GatewayURL), tomlString("Bearer "+req.GatewayToken))
		plan.Files = append(plan.Files, location.StagedFile{Name: file, Data: []byte(data)})
	default:
		return agentcli.Plan{}, agentcli.Refusef("agent.custom.mcp_config.format %q is not supported yet: the stdio bridge (silo mcp-bridge, design §4.3) is not built; use claude-json or codex-toml", c.MCPConfig.Format)
	}
	plan.Files = append(plan.Files, location.StagedFile{Name: PromptFile, Data: []byte(req.Prompt)})

	plan.Command = func(dir string) location.Command {
		sub := strings.NewReplacer("{file}", filepath.Join(dir, file), "{prompt}", filepath.Join(dir, PromptFile))
		argv := []string{req.Host.Binary}
		for _, a := range modeArgs {
			argv = append(argv, sub.Replace(a))
		}
		env := map[string]string{}
		if c.MCPConfig.Env != "" {
			env[c.MCPConfig.Env] = filepath.Join(dir, file)
		}
		for _, a := range c.MCPConfig.Arg {
			argv = append(argv, sub.Replace(a))
		}
		argv = append(argv, perm...)
		for k, v := range req.Env {
			env[k] = v
		}
		return location.Command{Argv: argv, Env: agentcli.MergeEnv(req.Host.Env, env), TTY: req.Mode == agentcli.Interactive}
	}
	return plan, nil
}

// tomlString renders a Go string as a TOML basic string, which is
// spelled exactly as a JSON string.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
