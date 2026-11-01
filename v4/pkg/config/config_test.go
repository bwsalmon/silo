package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Mode != "project" {
		t.Fatalf("expected mode project, got %s", cfg.Mode)
	}
	if !cfg.Agent.Subagents.Enabled {
		t.Fatal("expected subagents to be enabled by default")
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name: "valid config",
			modify: func(c *Config) {
				c.Project.ID = "my-project-01"
				c.Project.ServiceAccount = "agent@my-project-01.iam.gserviceaccount.com"
			},
			wantErr: false,
		},
		{
			name: "invalid mode",
			modify: func(c *Config) {
				c.Mode = "vm"
				c.Project.ID = "my-project-01"
				c.Project.ServiceAccount = "agent@my-project-01.iam.gserviceaccount.com"
			},
			wantErr: true,
		},
		{
			name: "invalid project ID format",
			modify: func(c *Config) {
				c.Project.ID = "INVALID_PROJECT"
				c.Project.ServiceAccount = "agent@my-project-01.iam.gserviceaccount.com"
			},
			wantErr: true,
		},
		{
			name: "invalid service account email",
			modify: func(c *Config) {
				c.Project.ID = "my-project-01"
				c.Project.ServiceAccount = "not-an-email"
			},
			wantErr: true,
		},
		{
			name: "concurrent exceeds total budget",
			modify: func(c *Config) {
				c.Project.ID = "my-project-01"
				c.Project.ServiceAccount = "agent@my-project-01.iam.gserviceaccount.com"
				maxSub := 5
				maxConc := 10
				c.Agent.Subagents.MaxSubagents = &maxSub
				c.Agent.Subagents.MaxConcurrent = &maxConc
			},
			wantErr: true,
		},
		{
			name: "unlimited quotas are valid",
			modify: func(c *Config) {
				c.Project.ID = "my-project-01"
				c.Project.ServiceAccount = "agent@my-project-01.iam.gserviceaccount.com"
				unlimited := -1
				c.Agent.Subagents.MaxSubagents = &unlimited
				c.Agent.Subagents.MaxConcurrent = &unlimited
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	yamlContent := `
version: "v4"
mode: "project"
project:
  id: "test-proj-123"
  service_account: "silo-root@test-proj-123.iam.gserviceaccount.com"
  downscope_credentials: true
repo:
  url: "https://github.com/example/infra.git"
  ref: "main"
agent:
  cli: "gemini"
  prompt_path: "SYSTEM.md"
  subagents:
    enabled: true
    max_subagents: 15
    max_concurrent: 3
    max_depth: 2
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "silo.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Project.ID != "test-proj-123" {
		t.Errorf("expected project ID test-proj-123, got %s", cfg.Project.ID)
	}
	if cfg.Repo.URL != "https://github.com/example/infra.git" {
		t.Errorf("expected repo url, got %s", cfg.Repo.URL)
	}
	if *cfg.Agent.Subagents.MaxSubagents != 15 {
		t.Errorf("expected max_subagents 15, got %d", *cfg.Agent.Subagents.MaxSubagents)
	}
}
