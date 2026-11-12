package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Paths inside a context repository.
const (
	SiloDir     = ".silo"
	PromptFile  = ".silo/prompt.md"
	ContextDir  = ".silo/context"
	ToolsDir    = ".silo/tools"
	RepoCfgFile = ".silo/config.yaml"
	Manifest    = "toolset.yaml"
)

// RepoConfig is a context repository's optional .silo/config.yaml.
type RepoConfig struct {
	// PushBranch is pushed to instead of silo/<session-id>. Silo's own
	// push.branch wins over it.
	PushBranch string `yaml:"push_branch"`
	// Disabled names tool sets in .silo/tools not to load.
	Disabled []string `yaml:"disabled"`
	// Context controls how .silo/context reaches the agent.
	Context ContextConfig `yaml:"context"`
}

// ContextConfig controls how .silo/context is handed to the agent.
type ContextConfig struct {
	// Mode is "inline" (file contents go into the system prompt, up to
	// MaxInlineBytes, the rest listed) or "list" (only listed, for the
	// agent to read with repo-read). Default inline.
	Mode string `yaml:"mode"`
	// MaxInlineBytes caps the inlined total. Default 65536.
	MaxInlineBytes int `yaml:"max_inline_bytes"`
}

// LoadRepoConfig reads <checkout>/.silo/config.yaml; a missing file gives
// the defaults.
func LoadRepoConfig(checkout string) (*RepoConfig, error) {
	data, err := os.ReadFile(filepath.Join(checkout, RepoCfgFile))
	if errors.Is(err, os.ErrNotExist) {
		data = nil
	} else if err != nil {
		return nil, err
	}
	rc, err := ParseRepoConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RepoCfgFile, err)
	}
	return rc, nil
}

// ParseRepoConfig decodes a .silo/config.yaml, filling in defaults.
func ParseRepoConfig(data []byte) (*RepoConfig, error) {
	var rc RepoConfig
	if err := strictDecode(data, &rc); err != nil {
		return nil, err
	}
	switch rc.Context.Mode {
	case "":
		rc.Context.Mode = "inline"
	case "inline", "list":
	default:
		return nil, fmt.Errorf("context.mode %q: want inline or list", rc.Context.Mode)
	}
	if rc.Context.MaxInlineBytes <= 0 {
		rc.Context.MaxInlineBytes = 64 * 1024
	}
	return &rc, nil
}

// ToolsetManifest is .silo/tools/<set>/toolset.yaml: a tool set whose
// MCP server is the code in that directory.
type ToolsetManifest struct {
	// Name must match the directory name.
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Build, if set, runs once before Run, in the tool set's directory.
	// ${BUILD_DIR} is a scratch directory outside the checkout for its
	// output.
	Build []string `yaml:"build"`
	// Run starts the MCP server (stdio), in the checkout. ${TOOLSET_DIR},
	// ${BUILD_DIR} and ${CHECKOUT} are substituted in every argument.
	Run []string `yaml:"run"`
	// Env is set in the server's environment.
	Env map[string]string `yaml:"env"`
	// PassEnv names variables copied from silo's own environment.
	PassEnv []string `yaml:"pass_env"`
	// Tools, if set, is the allowlist of the server's tools to expose,
	// with an optional description override.
	Tools []ToolDecl `yaml:"tools"`
	// RequiresApproval names tools the person must approve on every
	// call, seeing its arguments.
	RequiresApproval []string `yaml:"requires_approval"`
}

// ToolDecl is one entry of a manifest's tools list.
type ToolDecl struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

var setNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// ValidToolsetName reports whether name is usable as a tool-set name.
func ValidToolsetName(name string) error {
	if !setNameRE.MatchString(name) {
		return fmt.Errorf("tool-set name %q: want lowercase letters, digits, '_' and '-', starting with a letter or digit, at most 40", name)
	}
	return nil
}

// ParseManifest decodes and validates a toolset.yaml. dirName is the
// directory it was found in, which the name must match; pass "" to skip
// that check.
func ParseManifest(data []byte, dirName string) (*ToolsetManifest, error) {
	var m ToolsetManifest
	if err := strictDecode(data, &m); err != nil {
		return nil, err
	}
	if err := ValidToolsetName(m.Name); err != nil {
		return nil, err
	}
	if dirName != "" && m.Name != dirName {
		return nil, fmt.Errorf("name %q does not match its directory %q", m.Name, dirName)
	}
	if len(m.Run) == 0 {
		return nil, errors.New("run is required: the command that starts the MCP server")
	}
	declared := map[string]bool{}
	for _, t := range m.Tools {
		if t.Name == "" {
			return nil, errors.New("tools: every entry needs a name")
		}
		declared[t.Name] = true
	}
	for _, n := range m.RequiresApproval {
		if len(m.Tools) > 0 && !declared[n] {
			return nil, fmt.Errorf("requires_approval names %q, which tools does not list", n)
		}
	}
	for _, n := range m.PassEnv {
		if n == "" || strings.Contains(n, "=") {
			return nil, fmt.Errorf("pass_env: bad variable name %q", n)
		}
	}
	return &m, nil
}

// LoadManifest reads <dir>/toolset.yaml.
func LoadManifest(dir string) (*ToolsetManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, Manifest))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data, filepath.Base(dir))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(filepath.Base(dir), Manifest), err)
	}
	return m, nil
}

// RepoToolsets lists the tool-set directory names under
// <checkout>/.silo/tools, sorted.
func RepoToolsets(checkout string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(checkout, ToolsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
