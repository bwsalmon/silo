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

// ToolsetManifest is the optional .silo/tools/<set>/toolset.yaml:
// metadata about a tool set. How the set's server starts is not in it;
// that is the directory's start.sh or start.py.
type ToolsetManifest struct {
	// Name, if set, must match the directory name.
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
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

// Entry points: a tool set's directory holds exactly one of these, which
// silo runs as `bash start.sh <port>` or `python3 start.py <port>`.
const (
	StartSh = "start.sh"
	StartPy = "start.py"
)

// Interpreter is the program that runs an entry point.
func Interpreter(entry string) string {
	if entry == StartPy {
		return "python3"
	}
	return "bash"
}

// Entrypoint finds the one entry point among a tool set's top-level file
// names (has reports whether a name is present).
func Entrypoint(has func(name string) bool) (string, error) {
	sh, py := has(StartSh), has(StartPy)
	switch {
	case sh && py:
		return "", fmt.Errorf("has both %s and %s; keep exactly one", StartSh, StartPy)
	case sh:
		return StartSh, nil
	case py:
		return StartPy, nil
	}
	return "", fmt.Errorf("has no %s or %s: one of them starts the tool set's MCP server", StartSh, StartPy)
}

// DirEntrypoint is Entrypoint for a directory on disk.
func DirEntrypoint(dir string) (string, error) {
	return Entrypoint(func(name string) bool {
		fi, err := os.Stat(filepath.Join(dir, name))
		return err == nil && fi.Mode().IsRegular()
	})
}

var setNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// ValidToolsetName reports whether name is usable as a tool-set name (or
// a vendor alias).
func ValidToolsetName(name string) error {
	if !setNameRE.MatchString(name) {
		return fmt.Errorf("tool-set name %q: want lowercase letters, digits, '_' and '-', starting with a letter or digit, at most 40", name)
	}
	return nil
}

// retiredKeys were toolset.yaml's launch settings before start.sh and
// start.py replaced them.
var retiredKeys = []string{"build", "run"}

// ParseManifest decodes and validates a toolset.yaml. dirName is the
// directory it was found in, which a name in it must match; pass "" to
// skip that check.
func ParseManifest(data []byte, dirName string) (*ToolsetManifest, error) {
	var keys map[string]any
	if err := strictDecode(data, &keys); err == nil {
		for _, k := range retiredKeys {
			if _, ok := keys[k]; ok {
				return nil, fmt.Errorf("%q is no longer a toolset.yaml setting: the server starts from %s or %s in the tool set's directory, "+
					"which silo runs with a port to serve MCP over HTTP on (see docs/repo-layout.md)", k, StartSh, StartPy)
			}
		}
	}
	var m ToolsetManifest
	if err := strictDecode(data, &m); err != nil {
		return nil, err
	}
	if m.Name != "" {
		if err := ValidToolsetName(m.Name); err != nil {
			return nil, err
		}
		if dirName != "" && m.Name != dirName {
			return nil, fmt.Errorf("name %q does not match its directory %q", m.Name, dirName)
		}
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

// LoadManifest reads <dir>/toolset.yaml; without one, a set has no
// metadata.
func LoadManifest(dir string) (*ToolsetManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, Manifest))
	if errors.Is(err, os.ErrNotExist) {
		return &ToolsetManifest{}, nil
	} else if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data, filepath.Base(dir))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(filepath.Base(dir), Manifest), err)
	}
	return m, nil
}

// VendorDir holds tool sets vendored from other repositories, one
// directory per alias: .silo/tools/vendor/<alias>/<set>/.
const (
	VendorName = "vendor"
	VendorDir  = ToolsDir + "/" + VendorName
	VendorLock = "VENDOR.lock"
)

// A tool set's key is its directory relative to .silo/tools: "<set>" for
// the repository's own, "vendor/<alias>/<set>" for a vendored one.

// ToolsetName is the name the router knows a tool set by: the key itself
// for the repository's own sets, "<alias>.<set>" for vendored ones.
func ToolsetName(key string) string {
	if alias, set, ok := VendoredKey(key); ok {
		return alias + "." + set
	}
	return key
}

// VendoredKey splits a vendored set's key into alias and set.
func VendoredKey(key string) (alias, set string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) == 3 && parts[0] == VendorName {
		return parts[1], parts[2], true
	}
	return "", "", false
}

// ValidToolsetKey reports whether key names a tool-set directory.
func ValidToolsetKey(key string) error {
	if alias, set, ok := VendoredKey(key); ok {
		if err := ValidToolsetName(alias); err != nil {
			return fmt.Errorf("vendor alias: %w", err)
		}
		return ValidToolsetName(set)
	}
	if key == VendorName {
		return fmt.Errorf("%s/ holds vendored tool sets, not a tool set", VendorDir)
	}
	return ValidToolsetName(key)
}

// RepoToolsets lists the keys of the tool-set directories under
// <checkout>/.silo/tools, the vendored ones included, sorted.
func RepoToolsets(checkout string) ([]string, error) {
	keys, err := subdirs(filepath.Join(checkout, ToolsDir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		if k != VendorName {
			out = append(out, k)
			continue
		}
		aliases, err := subdirs(filepath.Join(checkout, VendorDir))
		if err != nil {
			return nil, err
		}
		for _, a := range aliases {
			sets, err := subdirs(filepath.Join(checkout, VendorDir, a))
			if err != nil {
				return nil, err
			}
			for _, s := range sets {
				out = append(out, VendorName+"/"+a+"/"+s)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func subdirs(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
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
