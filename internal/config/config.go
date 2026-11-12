// Package config reads silo's own config file, a context repository's
// .silo/config.yaml, and tool-set manifests (.silo/tools/<set>/toolset.yaml).
// docs/config.md and docs/repo-layout.md describe the formats.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is silo's own config file (~/.silo/config.yaml by default).
type Config struct {
	CLI         CLI      `yaml:"cli"`
	Repos       Repos    `yaml:"repos"`
	Identity    Identity `yaml:"identity"`
	Push        Push     `yaml:"push"`
	Toolsets    Toolsets `yaml:"toolsets"`
	SessionsDir string   `yaml:"sessions_dir"`
}

// CLI names the agent CLI silo drives.
type CLI struct {
	// Name is the adapter: claude, codex or gemini.
	Name string `yaml:"name"`
	// Binary is the executable; it defaults to Name, looked up on PATH.
	Binary string `yaml:"binary"`
	// Args are appended after the adapter's own flags.
	Args []string `yaml:"args"`
	// Env is added to the CLI's environment.
	Env map[string]string `yaml:"env"`
	// ListChanged says whether the CLI picks up
	// notifications/tools/list_changed: "auto" (the adapter's default),
	// "yes" or "no". With "no" the router also offers silo.list_tools
	// and silo.call, so tools added mid-session stay reachable.
	ListChanged string `yaml:"list_changed"`
}

// Repos says where context repositories come from.
type Repos struct {
	// Root is a directory of repositories, or (with GitBase) the
	// directory silo clones them into.
	Root string `yaml:"root"`
	// GitBase, when set, is prefixed to a repository name to clone it,
	// e.g. "git@github.com:" or "https://github.com/".
	GitBase string `yaml:"git_base"`
}

// Identity is the author and committer of approved commits.
type Identity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Push says where approved commits go.
type Push struct {
	// Branch is pushed to instead of the session branch silo/<session-id>.
	Branch string `yaml:"branch"`
	// Remote defaults to origin.
	Remote string `yaml:"remote"`
	// Disabled commits locally without pushing.
	Disabled bool `yaml:"disabled"`
}

// Toolsets holds tool-set overrides.
type Toolsets struct {
	// Disabled names repo tool sets not to load, as an escape hatch.
	Disabled []string `yaml:"disabled"`
}

// CLINames are the supported agent CLIs.
var CLINames = []string{"claude", "codex", "gemini"}

// DefaultPath is where silo looks for its config when neither --config
// nor $SILO_CONFIG names one.
func DefaultPath() string {
	return filepath.Join(home(), ".silo", "config.yaml")
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a config, filling in defaults.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := strictDecode(data, &c); err != nil {
		return nil, err
	}
	if c.CLI.Name == "" {
		return nil, errors.New("cli.name is required (claude, codex or gemini)")
	}
	if !contains(CLINames, c.CLI.Name) {
		return nil, fmt.Errorf("cli.name %q: want one of %s", c.CLI.Name, strings.Join(CLINames, ", "))
	}
	if c.CLI.Binary == "" {
		c.CLI.Binary = c.CLI.Name
	}
	c.CLI.Binary = ExpandHome(c.CLI.Binary)
	switch c.CLI.ListChanged {
	case "":
		c.CLI.ListChanged = "auto"
	case "auto", "yes", "no":
	default:
		return nil, fmt.Errorf("cli.list_changed %q: want auto, yes or no", c.CLI.ListChanged)
	}
	if c.Repos.Root == "" {
		return nil, errors.New("repos.root is required")
	}
	c.Repos.Root = ExpandHome(c.Repos.Root)
	if c.Identity.Name == "" || c.Identity.Email == "" {
		return nil, errors.New("identity.name and identity.email are required: approved commits are made as them")
	}
	if c.Push.Remote == "" {
		c.Push.Remote = "origin"
	}
	if c.SessionsDir == "" {
		c.SessionsDir = filepath.Join(home(), ".silo", "sessions")
	}
	c.SessionsDir = ExpandHome(c.SessionsDir)
	return &c, nil
}

// RepoURL is the clone URL of a repository name under GitBase.
func (c *Config) RepoURL(name string) string {
	base := c.Repos.GitBase
	if base == "" || strings.HasSuffix(base, "/") || strings.HasSuffix(base, ":") {
		return base + name
	}
	return base + "/" + name
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ValidateRepoName checks that a name relative to the repos root stays
// inside it.
func ValidateRepoName(name string) error {
	if !repoNameRE.MatchString(name) {
		return fmt.Errorf("repository name %q: want path segments of letters, digits, '.', '_' and '-'", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("repository name %q may not contain %q", name, seg)
		}
	}
	return nil
}

// ExpandHome replaces a leading ~/ with the home directory.
func ExpandHome(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func strictDecode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty document is valid: everything takes its default.
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
