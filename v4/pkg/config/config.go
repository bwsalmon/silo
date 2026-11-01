package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var (
	projectIDRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	emailRegex     = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
)

// Config represents the Silo v4 Project Edition configuration.
type Config struct {
	Version string        `yaml:"version"`
	Mode    string        `yaml:"mode"`
	Project ProjectConfig `yaml:"project"`
	Repo    RepoConfig    `yaml:"repo"`
	Agent   AgentConfig   `yaml:"agent"`
	Logging LoggingConfig `yaml:"logging"`
}

// ProjectConfig defines target Google Cloud project settings.
type ProjectConfig struct {
	ID                   string `yaml:"id"`
	Number               string `yaml:"number"`
	ServiceAccount       string `yaml:"service_account"`
	DownscopeCredentials bool   `yaml:"downscope_credentials"`
}

// RepoConfig defines a user-provided read-only repository mounted into agent memory.
type RepoConfig struct {
	URL string `yaml:"url"`
	Ref string `yaml:"ref"`
}

// AgentConfig defines the root agent execution profile.
type AgentConfig struct {
	CLI        string         `yaml:"cli"` // claude | codex | gemini
	Model      string         `yaml:"model"`
	PromptPath string         `yaml:"prompt_path"`
	Prompt     string         `yaml:"prompt"`
	Subagents  SubagentConfig `yaml:"subagents"`
}

// SubagentConfig defines the quota and delegation boundaries for sub-agents.
type SubagentConfig struct {
	Enabled         bool `yaml:"enabled"`
	MaxSubagents    *int `yaml:"max_subagents"` // nil = unlimited, -1 = unlimited, >=0 = bounded
	MaxConcurrent   *int `yaml:"max_concurrent"`
	MaxDepth        int  `yaml:"max_depth"`
	AllowDelegation bool `yaml:"allow_delegation"`
}

// LoggingConfig defines structured audit logging configuration.
type LoggingConfig struct {
	AuditLog string `yaml:"audit_log"`
	Level    string `yaml:"level"`
}

// DefaultConfig returns a sane default configuration for Project Edition.
func DefaultConfig() *Config {
	maxSub := 20
	maxConc := 5
	return &Config{
		Version: "v4",
		Mode:    "project",
		Project: ProjectConfig{
			DownscopeCredentials: true,
		},
		Agent: AgentConfig{
			CLI:        "gemini",
			PromptPath: "SYSTEM.md",
			Subagents: SubagentConfig{
				Enabled:         true,
				MaxSubagents:    &maxSub,
				MaxConcurrent:   &maxConc,
				MaxDepth:        2,
				AllowDelegation: true,
			},
		},
		Logging: LoggingConfig{
			AuditLog: "/var/log/silo/audit.log",
			Level:    "info",
		},
	}
}

// Load loads and validates configuration from a YAML file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// Validate verifies that the configuration satisfies all normative constraints.
func (c *Config) Validate() error {
	if c.Mode != "project" {
		return fmt.Errorf("unsupported mode %q (must be 'project')", c.Mode)
	}

	if c.Project.ID == "" {
		return errors.New("project.id is required")
	}
	if !projectIDRegex.MatchString(c.Project.ID) {
		return fmt.Errorf("invalid project.id %q (must match GCP project ID format)", c.Project.ID)
	}

	if c.Project.ServiceAccount == "" {
		return errors.New("project.service_account is required")
	}
	if !emailRegex.MatchString(c.Project.ServiceAccount) {
		return fmt.Errorf("invalid project.service_account %q", c.Project.ServiceAccount)
	}

	switch c.Agent.CLI {
	case "claude", "codex", "gemini":
	case "":
		c.Agent.CLI = "gemini"
	default:
		return fmt.Errorf("unsupported agent.cli %q (must be claude, codex, or gemini)", c.Agent.CLI)
	}

	sub := c.Agent.Subagents
	if sub.Enabled {
		if sub.MaxSubagents != nil && *sub.MaxSubagents != -1 && *sub.MaxSubagents < 0 {
			return errors.New("subagents.max_subagents must be >= 0 or -1 (unlimited)")
		}
		if sub.MaxConcurrent != nil && *sub.MaxConcurrent != -1 && *sub.MaxConcurrent < 0 {
			return errors.New("subagents.max_concurrent must be >= 0 or -1 (unlimited)")
		}
		if sub.MaxDepth < 0 {
			return errors.New("subagents.max_depth must be >= 0")
		}
		if sub.MaxSubagents != nil && sub.MaxConcurrent != nil &&
			*sub.MaxSubagents > 0 && *sub.MaxConcurrent > 0 &&
			*sub.MaxConcurrent > *sub.MaxSubagents {
			return errors.New("subagents.max_concurrent cannot exceed subagents.max_subagents")
		}
	}

	return nil
}
