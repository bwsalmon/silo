package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/v3/internal/tomlite"
)

// DefaultCLIsDir is where CLI preset files (*.toml, *.json) are read from.
const DefaultCLIsDir = "/etc/silo/clis.d"

// CLIDef defines how `silo connect` launches a coding-agent CLI in ACP stdio mode.
type CLIDef struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// BuiltinCLIs returns the stock ACP presets (design §5.1). A file in
// /etc/silo/clis.d of the same name overrides the built-in preset.
func BuiltinCLIs() map[string]CLIDef {
	return map[string]CLIDef{
		"gemini": {Name: "gemini", Command: []string{"gemini", "--experimental-acp"}},
		"claude": {Name: "claude", Command: []string{"claude-code-acp"}},
		"codex":  {Name: "codex", Command: []string{"codex-acp"}},
	}
}

var (
	cliNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,31}$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadCLIs reads every *.toml and *.json file in dir on top of BuiltinCLIs().
// A missing directory returns BuiltinCLIs() without error.
func LoadCLIs(dir string) (map[string]CLIDef, error) {
	out := BuiltinCLIs()
	if dir == "" {
		dir = DefaultCLIsDir
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	seen := map[string]string{}
	for _, n := range names {
		ext := filepath.Ext(n)
		if ext != ".toml" && ext != ".json" {
			continue
		}
		path := filepath.Join(dir, n)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var d CLIDef
		if ext == ".toml" {
			err = tomlite.Unmarshal(data, &d)
		} else {
			dec := json.NewDecoder(strings.NewReader(string(data)))
			dec.DisallowUnknownFields()
			err = dec.Decode(&d)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if d.Name == "" {
			d.Name = strings.TrimSuffix(n, ext)
		}
		if err := d.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, dup := seen[d.Name]; dup {
			return nil, fmt.Errorf("%s: cli %q is already defined in %s", path, d.Name, prev)
		}
		seen[d.Name] = path
		out[d.Name] = d
	}
	return out, nil
}

func (d CLIDef) validate() error {
	if !cliNameRe.MatchString(d.Name) {
		return fmt.Errorf("cli name %q: want lower-case letters, digits, ., - and _", d.Name)
	}
	if len(d.Command) == 0 || d.Command[0] == "" {
		return errors.New("command: must be non-empty")
	}
	for k := range d.Env {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("env: bad variable name %q", k)
		}
	}
	return nil
}

// ResolveCommand resolves the command argv and extra env for `silo connect`.
// If explicit args were passed after `--`:
//   - a single token that matches a preset name in clis (and is not a path)
//     expands that preset;
//   - otherwise args are used verbatim as the command.
//
// If no args were passed after `--`, runtime is looked up in clis.
func ResolveCommand(dir, runtime string, args []string) ([]string, map[string]string, error) {
	clis, err := LoadCLIs(dir)
	if err != nil {
		return nil, nil, err
	}
	if len(args) > 0 {
		if len(args) == 1 && !strings.Contains(args[0], "/") {
			if def, ok := clis[args[0]]; ok {
				return append([]string(nil), def.Command...), cloneEnv(def.Env), nil
			}
		}
		return append([]string(nil), args...), nil, nil
	}
	if runtime == "" {
		runtime = "claude"
	}
	def, ok := clis[runtime]
	if !ok {
		return nil, nil, fmt.Errorf("unknown cli preset %q", runtime)
	}
	return append([]string(nil), def.Command...), cloneEnv(def.Env), nil
}

func cloneEnv(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
