package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/audit"
	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/config"
	"github.com/bwsalmon/silo/v4/pkg/mcp/memory"
	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
	"github.com/bwsalmon/silo/v4/pkg/mcp/subagents"
	"github.com/bwsalmon/silo/v4/pkg/mcp/vm"
	"github.com/bwsalmon/silo/v4/pkg/quota"
	"github.com/bwsalmon/silo/v4/pkg/supervisor"
)

const Version = "v4.0.0-project"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("Silo Project Edition %s\n", Version)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCPServer(os.Args[2:])
		return
	}

	// Default to 'run' command
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}

	runRoot(args)
}

func runRoot(args []string) {
	fs := flag.NewFlagSet("silo run", flag.ExitOnError)

	configPath := fs.String("config", "", "Path to silo.yaml configuration file")
	mode := fs.String("mode", "project", "Execution mode: 'project' (tool sandbox) or 'vm' (Cloud Hypervisor)")
	projectID := fs.String("project", "", "Target Google Cloud project ID")
	serviceAccount := fs.String("service-account", "", "Target GCP service account email")
	repoURL := fs.String("repo", "", "Git repository URL or local directory to mount read-only")
	maxSubagents := fs.Int("max-subagents", 10, "Maximum lifetime sub-agents for root agent (-1 for unlimited)")
	maxConcurrent := fs.Int("max-concurrent", 3, "Maximum concurrent sub-agents for root agent (-1 for unlimited)")
	maxDepth := fs.Int("max-depth", 2, "Maximum delegation nesting depth (-1 for unlimited)")
	unlimitedSub := fs.Bool("unlimited-subagents", false, "Remove all quota limits for root agent")
	prompt := fs.String("prompt", "", "Initial task prompt for the root agent")
	cliType := fs.String("cli", "", "Agent CLI type: 'jetski', 'claude', 'codex', or 'custom'")
	cliPath := fs.String("cli-path", "", "Explicit path to the agent CLI binary")
	baseDir := fs.String("base-dir", "/var/lib/silo", "Directory for agent workspaces, memory, and state")
	auditLogPath := fs.String("audit-log", "/var/log/silo/audit.log", "Path for structured JSON audit logs")
	mockAuth := fs.Bool("mock-auth", false, "Use mock token provider for local testing")

	_ = fs.Parse(args)

	// 1. Resolve Configuration
	var cfg *config.Config
	var err error
	if *configPath != "" {
		cfg, err = config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
	} else {
		cfg = config.DefaultConfig()
	}

	if *mode != "" {
		cfg.Mode = *mode
	}
	if *projectID != "" {
		cfg.Project.ID = *projectID
	}
	if *serviceAccount != "" {
		cfg.Project.ServiceAccount = *serviceAccount
	}
	if *repoURL != "" {
		cfg.Repo.URL = *repoURL
	}
	if *prompt != "" {
		cfg.Agent.Prompt = *prompt
	}
	if *cliType != "" {
		cfg.Agent.CLI = *cliType
	}
	if *cliPath != "" {
		cfg.Agent.CLIPath = *cliPath
	}

	if *unlimitedSub {
		unlim := -1
		cfg.Agent.Subagents.MaxSubagents = &unlim
		cfg.Agent.Subagents.MaxConcurrent = &unlim
		cfg.Agent.Subagents.MaxDepth = -1
	} else {
		if *maxSubagents != 10 || cfg.Agent.Subagents.MaxSubagents == nil {
			val := *maxSubagents
			cfg.Agent.Subagents.MaxSubagents = &val
		}
		if *maxConcurrent != 3 || cfg.Agent.Subagents.MaxConcurrent == nil {
			val := *maxConcurrent
			cfg.Agent.Subagents.MaxConcurrent = &val
		}
		if *maxDepth != 2 || cfg.Agent.Subagents.MaxDepth == 0 {
			cfg.Agent.Subagents.MaxDepth = *maxDepth
		}
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration validation failed: %v\n", err)
		os.Exit(1)
	}

	// 2. Resolve Base and Log Directories (fallback if /var/lib/silo is not writable)
	effectiveBaseDir := *baseDir
	if err := os.MkdirAll(effectiveBaseDir, 0755); err != nil {
		effectiveBaseDir = filepath.Join(os.TempDir(), "silo")
		_ = os.MkdirAll(effectiveBaseDir, 0755)
	}

	effectiveAuditLog := *auditLogPath
	if err := os.MkdirAll(filepath.Dir(effectiveAuditLog), 0755); err != nil {
		effectiveAuditLog = filepath.Join(effectiveBaseDir, "audit.log")
	}

	logger, err := audit.NewFileLogger(effectiveAuditLog)
	if err != nil {
		logger = audit.NewWriterLogger(os.Stdout)
	}
	defer logger.Close()

	quotaSub := -1
	if cfg.Agent.Subagents.MaxSubagents != nil {
		quotaSub = *cfg.Agent.Subagents.MaxSubagents
	}
	quotaConc := -1
	if cfg.Agent.Subagents.MaxConcurrent != nil {
		quotaConc = *cfg.Agent.Subagents.MaxConcurrent
	}

	_ = logger.Log("ROOT_AGENT_START", map[string]interface{}{
		"mode":            cfg.Mode,
		"project_id":      cfg.Project.ID,
		"service_account": cfg.Project.ServiceAccount,
		"repo":            cfg.Repo.URL,
		"max_subagents":   quotaSub,
		"max_concurrent":  quotaConc,
		"max_depth":       cfg.Agent.Subagents.MaxDepth,
	})

	// 3. Resolve Repository Workspace
	repoDir := ""
	if cfg.Repo.URL != "" {
		repoDir, err = resolveRepository(cfg.Repo.URL, effectiveBaseDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to pull repository %s: %v\n", cfg.Repo.URL, err)
		}
	}

	// 4. Initialize Auth Token Provider
	var tokenProvider auth.TokenProvider
	if *mockAuth || cfg.Project.ID == "mock-project" {
		tokenProvider = &auth.MockTokenProvider{}
	} else {
		tokenProvider = auth.NewSTSTokenProvider(nil)
	}

	// 5. Initialize Quota Ledger and Supervisor
	agentsDir := filepath.Join(effectiveBaseDir, "agents")
	ledger := quota.NewLedger("root", quotaSub, quotaConc, cfg.Agent.Subagents.MaxDepth, true)
	sup := supervisor.NewSupervisor(ledger, tokenProvider, agentsDir, repoDir)

	// 6. Initialize Root Agent Memory
	rootMem, err := memory.NewMemoryStore("root", agentsDir, repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize root memory store: %v\n", err)
		os.Exit(1)
	}
	if cfg.Agent.Prompt != "" {
		_ = rootMem.Set("prompt", cfg.Agent.Prompt)
	}

	// 7. Handle Signal Interrupts for Cascading Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("[silo] Root agent initialized (Project: %s, MaxSubagents: %d, MaxConcurrent: %d, MaxDepth: %d)\n",
		cfg.Project.ID, quotaSub, quotaConc, cfg.Agent.Subagents.MaxDepth)
	if repoDir != "" {
		fmt.Printf("[silo] Repository mounted read-only at: %s\n", repoDir)
	}
	fmt.Printf("[silo] Audit logging active at: %s\n", effectiveAuditLog)

	// 7. Launch Root Agent CLI if prompt is provided
	if cfg.Agent.Prompt != "" {
		go func() {
			if err := launchAgentCLI(cfg, effectiveBaseDir, repoDir, logger); err != nil {
				fmt.Fprintf(os.Stderr, "[silo] Agent execution failed: %v\n", err)
			}
		}()
	}

	// 8. Handle Signal Interrupts for Cascading Shutdown
	// Await termination signal
	sig := <-sigChan
	fmt.Printf("\n[silo] Received signal %v; executing cascading teardown...\n", sig)
	_ = logger.Log("ROOT_AGENT_EXIT", map[string]interface{}{
		"signal":    sig.String(),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})

	sup.Shutdown()
	fmt.Println("[silo] Shutdown complete.")
}

func resolveCLIPath(cliType, configuredPath string) (string, error) {
	if configuredPath != "" {
		if _, err := os.Stat(configuredPath); err == nil {
			return configuredPath, nil
		}
		if path, err := exec.LookPath(configuredPath); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("configured CLI path %q not found", configuredPath)
	}

	switch cliType {
	case "jetski":
		if path, err := exec.LookPath("agentapi"); err == nil {
			return path, nil
		}
		jetskiPath := filepath.Join(os.Getenv("HOME"), ".gemini/jetski/bin/agentapi")
		if _, err := os.Stat(jetskiPath); err == nil {
			return jetskiPath, nil
		}
		if path, err := exec.LookPath("jetski"); err == nil {
			return path, nil
		}
		return "", errors.New("neither 'agentapi' nor 'jetski' found in PATH or ~/.gemini/jetski/bin")

	case "claude":
		if path, err := exec.LookPath("claude"); err == nil {
			return path, nil
		}
		claudePath := filepath.Join(os.Getenv("HOME"), ".local/bin/claude")
		if _, err := os.Stat(claudePath); err == nil {
			return claudePath, nil
		}
		return "", errors.New("'claude' CLI not found in PATH or ~/.local/bin/claude")

	case "codex":
		if path, err := exec.LookPath("codex"); err == nil {
			return path, nil
		}
		return "", errors.New("'codex' CLI not found in PATH")

	default:
		if path, err := exec.LookPath(cliType); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("unknown CLI type %q", cliType)
	}
}

func launchAgentCLI(cfg *config.Config, baseDir, repoDir string, logger audit.Logger) error {
	cliType := cfg.Agent.CLI
	if cliType == "" {
		cliType = "jetski"
	}

	cliPath, err := resolveCLIPath(cliType, cfg.Agent.CLIPath)
	if err != nil {
		return err
	}

	fmt.Printf("[silo] Spawning %s root agent (CLI: %s)...\n", cliType, cliPath)
	_ = logger.Log("AGENT_CLI_LAUNCHED", map[string]interface{}{
		"cli":      cliType,
		"cli_path": cliPath,
		"prompt":   cfg.Agent.Prompt,
	})

	var cmd *exec.Cmd
	switch cliType {
	case "jetski":
		args := []string{"new-conversation", "--title=Silo Root Agent"}
		if cfg.Agent.Model != "" {
			args = append(args, fmt.Sprintf("--model=%s", cfg.Agent.Model))
		}
		args = append(args, cfg.Agent.Prompt)
		cmd = exec.Command(cliPath, args...)
		cmd.Env = cleanEnv(os.Environ(), "ANTIGRAVITY_SOURCE_METADATA")

	case "claude":
		mcpPath := filepath.Join(baseDir, "mcp.json")
		siloBin, _ := os.Executable()
		if siloBin == "" {
			siloBin = "silo"
		}
		mcpConfig := map[string]interface{}{
			"mcpServers": map[string]interface{}{
				"silo": map[string]interface{}{
					"command": siloBin,
					"args": []string{
						"mcp",
						"--agent-id=root",
						fmt.Sprintf("--project=%s", cfg.Project.ID),
						fmt.Sprintf("--base-dir=%s", baseDir),
						fmt.Sprintf("--repo-dir=%s", repoDir),
						"--allow-vm=true",
						"--allow-subagents=true",
					},
				},
			},
		}
		bytes, _ := json.MarshalIndent(mcpConfig, "", "  ")
		_ = os.WriteFile(mcpPath, bytes, 0644)

		cmd = exec.Command(cliPath, "--mcp-config", mcpPath, "--tools", "", "--dangerously-skip-permissions", "-p", cfg.Agent.Prompt)

	default:
		cmd = exec.Command(cliPath, cfg.Agent.Prompt)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cleanEnv(env []string, removeKey string) []string {
	var cleaned []string
	prefix := removeKey + "="
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			cleaned = append(cleaned, e)
		}
	}
	return cleaned
}

func resolveRepository(urlOrPath, baseDir string) (string, error) {
	// If already a local directory, return it
	if info, err := os.Stat(urlOrPath); err == nil && info.IsDir() {
		return urlOrPath, nil
	}

	// Otherwise clone to <baseDir>/repo
	targetDir := filepath.Join(baseDir, "repo")
	if err := os.RemoveAll(targetDir); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", urlOrPath, targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone error: %s (%w)", string(out), err)
	}

	return targetDir, nil
}

func runMCPServer(args []string) {
	fs := flag.NewFlagSet("silo mcp", flag.ExitOnError)
	agentID := fs.String("agent-id", "root", "Agent identifier")
	projectID := fs.String("project", "", "Target GCP project ID")
	baseDir := fs.String("base-dir", "/tmp/silo", "Base agents directory")
	repoDir := fs.String("repo-dir", "", "Mounted repository directory")
	allowVM := fs.Bool("allow-vm", true, "Enable VM tools")
	allowSub := fs.Bool("allow-subagents", true, "Enable sub-agent tools")

	_ = fs.Parse(args)

	server := protocol.NewServer(fmt.Sprintf("silo-agent-%s", *agentID), Version)

	agentsDir := filepath.Join(*baseDir, "agents")
	_ = os.MkdirAll(agentsDir, 0755)

	// Register memory tools
	memStore, err := memory.NewMemoryStore(*agentID, agentsDir, *repoDir)
	if err == nil {
		memStore.RegisterTools(server)
	}

	// Register VM tools if permitted
	if *allowVM {
		vmRunner := vm.NewGCloudRunner(*projectID)
		vmServer := vm.NewVMServer(vmRunner)
		vmServer.RegisterTools(server)
	}

	// Register subagent management tools if permitted
	if *allowSub {
		ledger := quota.NewLedger(*agentID, 20, 5, 2, true)
		sup := supervisor.NewSupervisor(ledger, &auth.MockTokenProvider{}, agentsDir, *repoDir)
		subMgr := subagents.NewManager(*agentID, ledger, sup, &auth.MockTokenProvider{}, agentsDir, *repoDir, nil)
		subMgr.RegisterTools(server)
	}

	ctx := context.Background()
	_ = server.Serve(ctx, os.Stdin, os.Stdout)
}

func checkErr(err error, w io.Writer) {
	if err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
	}
}
