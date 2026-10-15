package silofile

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The fields below take more than one YAML shape (a word, or a mapping or
// list), or a value with its own syntax (a duration, a size). Each
// decodes itself from the YAML node and describes itself in the schema.

// Auth is agent.auth: ambient (the CLI's own login), or
// {credential: <name>}. The zero value is ambient.
type Auth struct {
	Credential string
}

// Ambient reports whether the CLI uses its own login.
func (a Auth) Ambient() bool { return a.Credential == "" }

func (a *Auth) decodeYAML(d *decoder, n *yaml.Node, path string) {
	if n.Kind == yaml.ScalarNode {
		d.word(n, path, "ambient")
		return
	}
	var c struct {
		Credential string `yaml:"credential" schema:"required"`
	}
	d.value(n, &c, path)
	a.Credential = c.Credential
}

func (Auth) jsonSchema() map[string]any {
	return oneOf(enum("ambient"), object(map[string]any{"credential": str()}, "credential"))
}

// Permissions modes of RemotePermissions.
const (
	PermNone   = "none"
	PermAll    = "all"
	PermSubset = "subset"
)

// RemotePermissions is remote_permissions: none, all, or a list of the
// CLI's own tool names. The zero value is none.
type RemotePermissions struct {
	// Mode is PermNone, PermAll or PermSubset. Empty means PermNone.
	Mode string
	// Tools is the list, for PermSubset.
	Tools []string
}

// Effective returns Mode with the default applied.
func (r RemotePermissions) Effective() string {
	if r.Mode == "" {
		return PermNone
	}
	return r.Mode
}

func (r *RemotePermissions) decodeYAML(d *decoder, n *yaml.Node, path string) {
	if n.Kind == yaml.SequenceNode {
		r.Mode = PermSubset
		d.value(n, &r.Tools, path)
		return
	}
	r.Mode, _ = d.word(n, path, PermNone, PermAll)
}

func (RemotePermissions) jsonSchema() map[string]any {
	return oneOf(enum(PermNone, PermAll), array(str()))
}

// Network is a sandbox's network: none, open, or {allow: [host, ...]}
// (later). The zero value is none.
type Network struct {
	// Mode is "none", "open" or "allow". Empty means "none".
	Mode  string
	Allow []string
}

func (w *Network) decodeYAML(d *decoder, n *yaml.Node, path string) {
	if n.Kind == yaml.ScalarNode {
		w.Mode, _ = d.word(n, path, "none", "open")
		return
	}
	var a struct {
		Allow []string `yaml:"allow" schema:"required"`
	}
	d.value(n, &a, path)
	w.Mode, w.Allow = "allow", a.Allow
}

func (Network) jsonSchema() map[string]any {
	return oneOf(enum("none", "open"), object(map[string]any{"allow": array(str())}, "allow"))
}

// Backend is a sandbox's backend: local, or {k8s: {...}}. The
// zero value is local.
type Backend struct {
	K8s *K8sBackend
}

// K8sBackend is backend.k8s: the cluster a granule sandbox's pod runs
// in (journey 9).
type K8sBackend struct {
	Context     string `yaml:"context" doc:"A context in the kubeconfig; kubectl's current context when empty."`
	Namespace   string `yaml:"namespace" doc:"The namespace the sandbox's pod goes in; the context's namespace when empty."`
	TunnelImage string `yaml:"tunnel_image" doc:"The tunnel sidecar's image, which carries the git proxy to a sandbox with repos (design §9.3); silo's published one when empty."`
	KVMGroup    int    `yaml:"kvm_group" doc:"The group id that owns /dev/kvm on the nodes, given to a guest with no network (which runs as nobody) where /dev/kvm is 0660; GKE's nested-virtualisation nodes make it 0666 and need none."`
}

func (b *Backend) decodeYAML(d *decoder, n *yaml.Node, path string) {
	if n.Kind == yaml.ScalarNode {
		d.word(n, path, "local")
		return
	}
	var k struct {
		K8s *K8sBackend `yaml:"k8s" schema:"required"`
	}
	d.value(n, &k, path)
	b.K8s = k.K8s
}

func (Backend) jsonSchema() map[string]any {
	return oneOf(enum("local"), object(map[string]any{"k8s": schemaOf(typeOf[K8sBackend]())}, "k8s"))
}

// InheritEnv is an MCP server's inherit_env: a list of names, or all.
type InheritEnv struct {
	All   bool
	Names []string
}

func (e *InheritEnv) decodeYAML(d *decoder, n *yaml.Node, path string) {
	if n.Kind == yaml.ScalarNode {
		_, e.All = d.word(n, path, "all")
		return
	}
	d.value(n, &e.Names, path)
}

func (InheritEnv) jsonSchema() map[string]any {
	return oneOf(enum("all"), array(str()))
}

// Duration is a length of time written as one or more number-and-unit
// pairs, units w, d, h, m and s: 7d, 1h30m, 5m. A bare 0 is zero.
type Duration time.Duration

// ParseDuration parses a Silofile duration.
func ParseDuration(s string) (Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	units := map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i == len(rest) {
			return 0, fmt.Errorf("%q is not a duration: want a number and a unit (w, d, h, m, s), such as 7d or 1h30m", s)
		}
		unit, ok := units[rest[i]]
		if !ok {
			return 0, fmt.Errorf("%q is not a duration: unknown unit %q (want w, d, h, m or s)", s, rest[i:i+1])
		}
		n, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil || n > int64(math.MaxInt64/unit) || total > math.MaxInt64-time.Duration(n)*unit {
			return 0, fmt.Errorf("%q is not a duration: too large", s)
		}
		total += time.Duration(n) * unit
		rest = rest[i+1:]
	}
	return Duration(total), nil
}

func (v *Duration) decodeYAML(d *decoder, n *yaml.Node, path string) {
	s, ok := d.scalar(n, path)
	if !ok {
		return
	}
	p, err := ParseDuration(s)
	if err != nil {
		d.errorf(n, path, "%v", err)
		return
	}
	*v = p
}

func (Duration) jsonSchema() map[string]any {
	return map[string]any{"type": []any{"string", "integer"}, "pattern": `^(0|([0-9]+[wdhms])+)$`,
		"description": "A duration: number-and-unit pairs (w, d, h, m, s), such as 7d or 1h30m; 0 is zero."}
}

// Size is a number of bytes written with an optional unit: 512, 1MiB,
// 4Gi, 10GB. Ki, Mi, Gi and Ti (optionally followed by B) are powers of
// 1024; K, M, G and T (optionally followed by B) are powers of 1000.
type Size int64

// ParseSize parses a Silofile size.
func ParseSize(s string) (Size, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("%q is not a size: want a number and an optional unit, such as 4Gi or 1MiB", s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size: too large", s)
	}
	unit := strings.TrimSuffix(s[i:], "B")
	if s[i:] == "B" {
		unit = ""
	}
	mult := map[string]int64{
		"": 1, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
	}
	m, ok := mult[unit]
	if !ok || (unit == "" && s[i:] != "" && s[i:] != "B") {
		return 0, fmt.Errorf("%q is not a size: unknown unit %q (want B, K, M, G, T, Ki, Mi, Gi or Ti, optionally followed by B)", s, s[i:])
	}
	if n > math.MaxInt64/m {
		return 0, fmt.Errorf("%q is not a size: too large", s)
	}
	return Size(n * m), nil
}

func (v *Size) decodeYAML(d *decoder, n *yaml.Node, path string) {
	s, ok := d.scalar(n, path)
	if !ok {
		return
	}
	p, err := ParseSize(s)
	if err != nil {
		d.errorf(n, path, "%v", err)
		return
	}
	*v = p
}

func (Size) jsonSchema() map[string]any {
	return map[string]any{"type": []any{"string", "integer"}, "pattern": `^[0-9]+(B|[KMGT]i?B?)?$`,
		"description": "A size in bytes, with an optional unit: 1MiB, 4Gi, 10GB."}
}
