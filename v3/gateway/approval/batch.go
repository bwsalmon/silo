// Package approval is silo's privileged path (design §4.3–§4.5, §7.4–§7.5):
// the root agent proposes a batch of file writes and commands with
// run_as_root.submit; the gateway stages everything the batch will write or
// execute in a root-owned directory, binds the batch to a SHA-256 hash of
// all of it, computes what the human is shown (diffs, argv, risk flags,
// snapshot status), and queues it. A human approves or rejects it; an
// approval is good for one execution of exactly that hash. Before running,
// the gateway re-hashes the staged copies and every executable, takes a
// snapshot when the root filesystem supports one, applies the file writes
// atomically (keeping what they replaced), and runs the steps in order as
// root, stopping at the first failure. Every step of that is written to the
// gateway's hash-chained audit log.
//
// The package does not know the gateway's callers or its socket: the gateway
// authenticates a call, describes the caller as a Principal and hands the
// call to Service.Call.
package approval

import (
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Batch is one coherent change with a stated reason, as the root agent
// submits it (design §4.3).
type Batch struct {
	Title       string       `json:"title"`
	Reason      string       `json:"reason"`
	Files       []FileWrite  `json:"files,omitempty"`
	Steps       []Step       `json:"steps,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Expects     *Expects     `json:"expects,omitempty"`
}

// FileWrite is a file the batch writes (or deletes) before its steps run.
type FileWrite struct {
	Path       string `json:"path"`
	Mode       string `json:"mode,omitempty"`  // octal, default 0644
	Owner      string `json:"owner,omitempty"` // default root
	Group      string `json:"group,omitempty"` // default root
	ContentB64 string `json:"content_b64,omitempty"`
	Delete     bool   `json:"delete,omitempty"`
}

// Step is one command. It is either a raw argv or a typed operation (Op),
// which the gateway expands itself and which standing approvals can match.
type Step struct {
	Argv []string `json:"argv,omitempty"`

	// Op is a typed operation: pkg.install and pkg.remove (Names),
	// unit.enable and unit.restart (Unit), user.add_to_group (User, Group).
	Op    string   `json:"op,omitempty"`
	Names []string `json:"names,omitempty"`
	Unit  string   `json:"unit,omitempty"`
	User  string   `json:"user,omitempty"`
	Group string   `json:"group,omitempty"`

	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	StdinAttachment string            `json:"stdin_attachment,omitempty"`
	TimeoutS        int               `json:"timeout_s,omitempty"`
}

// Attachment is a script or input the steps read or execute. A step names
// it in its argv as "attachment:<name>" (or in stdin_attachment), and the
// gateway runs the staged copy.
type Attachment struct {
	Name       string `json:"name"`
	ContentB64 string `json:"content_b64"`
	Mode       string `json:"mode,omitempty"` // octal, default 0644; 0755 to execute it
}

// Expects is declared by the agent and shown, not enforced.
type Expects struct {
	Reboot  bool     `json:"reboot,omitempty"`
	Network []string `json:"network,omitempty"`
}

// AttachmentPrefix marks an argv element that names an attachment.
const AttachmentPrefix = "attachment:"

// Bounds on one batch. The socket's 1 MiB request limit bounds content.
const (
	maxTitle       = 200
	maxReason      = 16 << 10
	maxFiles       = 100
	maxSteps       = 100
	maxAttachments = 50
	maxArgv        = 256
	maxEnv         = 64
	maxTimeoutS    = 6 * 3600
	defaultTimeout = 15 * 60
)

var (
	attachmentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envNameRe        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	unixNameRe       = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)
	pkgNameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,99}(:[a-z0-9-]{1,20})?(=[A-Za-z0-9.+~:-]{1,64})?$`)
	unitNameRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]{0,250}\.(service|socket|timer|path|target|mount)$`)
)

// Groups user.add_to_group refuses (design §4.3).
var privilegedGroups = map[string]bool{
	"sudo": true, "adm": true, "root": true, "shadow": true, "disk": true, "silo-gw": true, "wheel": true,
}

// isSiloUnit reports whether a unit is one of silo's own, which typed
// operations refuse to touch.
func isSiloUnit(u string) bool { return strings.HasPrefix(u, "silo-") || strings.HasPrefix(u, "silo.") }

// parseMode parses an octal mode string, defaulting to def.
func parseMode(s string, def uint32) (uint32, error) {
	if s == "" {
		return def, nil
	}
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil || m > 0o7777 {
		return 0, fmt.Errorf("mode %q: want octal such as 0644", s)
	}
	return uint32(m), nil
}

// cleanAbs reports whether p is absolute and already in clean form.
func cleanAbs(p string) bool {
	return p != "" && path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsRune(p, 0)
}

// normalize fills defaults and validates b, returning the decoded content of
// every file write and attachment.
func (b *Batch) normalize() (files, attachments [][]byte, err error) {
	if strings.TrimSpace(b.Title) == "" || len(b.Title) > maxTitle || strings.ContainsAny(b.Title, "\r\n") {
		return nil, nil, fmt.Errorf("title: want one line of 1-%d bytes", maxTitle)
	}
	if strings.TrimSpace(b.Reason) == "" || len(b.Reason) > maxReason {
		return nil, nil, fmt.Errorf("reason: want 1-%d bytes saying why", maxReason)
	}
	if len(b.Files)+len(b.Steps) == 0 {
		return nil, nil, fmt.Errorf("a batch needs at least one file write or step")
	}
	if len(b.Files) > maxFiles || len(b.Steps) > maxSteps || len(b.Attachments) > maxAttachments {
		return nil, nil, fmt.Errorf("at most %d files, %d steps and %d attachments", maxFiles, maxSteps, maxAttachments)
	}
	seenPath := map[string]bool{}
	for i := range b.Files {
		f := &b.Files[i]
		if !cleanAbs(f.Path) || f.Path == "/" {
			return nil, nil, fmt.Errorf("files[%d].path %q: want a clean absolute path", i, f.Path)
		}
		if seenPath[f.Path] {
			return nil, nil, fmt.Errorf("files[%d].path %q: written twice", i, f.Path)
		}
		seenPath[f.Path] = true
		if f.Delete {
			if f.ContentB64 != "" || f.Mode != "" || f.Owner != "" || f.Group != "" {
				return nil, nil, fmt.Errorf("files[%d]: a delete has only path and delete", i)
			}
			files = append(files, nil)
			continue
		}
		if f.Mode == "" {
			f.Mode = "0644"
		}
		m, err := parseMode(f.Mode, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("files[%d]: %w", i, err)
		}
		f.Mode = fmt.Sprintf("%04o", m)
		if f.Owner == "" {
			f.Owner = "root"
		}
		if f.Group == "" {
			f.Group = "root"
		}
		if !unixNameRe.MatchString(f.Owner) || !unixNameRe.MatchString(f.Group) {
			return nil, nil, fmt.Errorf("files[%d]: owner %q / group %q: want Unix names", i, f.Owner, f.Group)
		}
		data, err := base64.StdEncoding.DecodeString(f.ContentB64)
		if err != nil {
			return nil, nil, fmt.Errorf("files[%d].content_b64: %w", i, err)
		}
		files = append(files, data)
	}
	names := map[string]bool{}
	for i := range b.Attachments {
		a := &b.Attachments[i]
		if !attachmentNameRe.MatchString(a.Name) || names[a.Name] {
			return nil, nil, fmt.Errorf("attachments[%d].name %q: want a unique name of letters, digits, '.', '_' and '-'", i, a.Name)
		}
		names[a.Name] = true
		m, err := parseMode(a.Mode, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("attachments[%d]: %w", i, err)
		}
		a.Mode = fmt.Sprintf("%04o", m&0o755)
		data, err := base64.StdEncoding.DecodeString(a.ContentB64)
		if err != nil {
			return nil, nil, fmt.Errorf("attachments[%d].content_b64: %w", i, err)
		}
		attachments = append(attachments, data)
	}
	for i := range b.Steps {
		if err := b.Steps[i].normalize(names); err != nil {
			return nil, nil, fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	return files, attachments, nil
}

func (s *Step) normalize(attachments map[string]bool) error {
	if (s.Op == "") == (len(s.Argv) == 0) {
		return fmt.Errorf("give argv or op, not both or neither")
	}
	if s.Op != "" {
		if _, err := s.expand(); err != nil {
			return err
		}
	} else if s.Names != nil || s.Unit != "" || s.User != "" || s.Group != "" {
		return fmt.Errorf("names, unit, user and group belong to typed operations")
	}
	if len(s.Argv) > maxArgv {
		return fmt.Errorf("at most %d argv elements", maxArgv)
	}
	for j, a := range s.Argv {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("argv[%d] contains a NUL byte", j)
		}
		if name, ok := strings.CutPrefix(a, AttachmentPrefix); ok && !attachments[name] {
			return fmt.Errorf("argv[%d] names attachment %q, which the batch does not have", j, name)
		}
	}
	if len(s.Argv) > 0 && s.Argv[0] == "" {
		return fmt.Errorf("argv[0] is empty")
	}
	if s.Cwd == "" {
		s.Cwd = "/"
	}
	if !cleanAbs(s.Cwd) {
		return fmt.Errorf("cwd %q: want a clean absolute path", s.Cwd)
	}
	if len(s.Env) > maxEnv {
		return fmt.Errorf("at most %d env entries", maxEnv)
	}
	for k, v := range s.Env {
		if !envNameRe.MatchString(k) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %q: want a variable name and a value without NUL", k)
		}
	}
	if s.StdinAttachment != "" && !attachments[s.StdinAttachment] {
		return fmt.Errorf("stdin_attachment %q: the batch has no such attachment", s.StdinAttachment)
	}
	if s.TimeoutS == 0 {
		s.TimeoutS = defaultTimeout
	}
	if s.TimeoutS < 0 || s.TimeoutS > maxTimeoutS {
		return fmt.Errorf("timeout_s %d: want 1-%d", s.TimeoutS, maxTimeoutS)
	}
	return nil
}

// expand returns the argv a typed operation runs (design §4.3).
func (s *Step) expand() ([]string, error) {
	switch s.Op {
	case "pkg.install", "pkg.remove":
		if len(s.Names) == 0 || len(s.Names) > 100 || s.Unit != "" || s.User != "" || s.Group != "" {
			return nil, fmt.Errorf("%s takes 1-100 names and nothing else", s.Op)
		}
		for _, n := range s.Names {
			if !pkgNameRe.MatchString(n) {
				return nil, fmt.Errorf("%s: %q is not a package name", s.Op, n)
			}
		}
		verb := "install"
		if s.Op == "pkg.remove" {
			verb = "remove"
		}
		return append([]string{"apt-get", verb, "-y", "--"}, s.Names...), nil
	case "unit.enable", "unit.restart":
		if s.Unit == "" || s.Names != nil || s.User != "" || s.Group != "" {
			return nil, fmt.Errorf("%s takes a unit and nothing else", s.Op)
		}
		if !unitNameRe.MatchString(s.Unit) {
			return nil, fmt.Errorf("%s: %q is not a unit name", s.Op, s.Unit)
		}
		if isSiloUnit(s.Unit) {
			return nil, fmt.Errorf("%s: refused for silo's own unit %q", s.Op, s.Unit)
		}
		return []string{"systemctl", strings.TrimPrefix(s.Op, "unit."), "--", s.Unit}, nil
	case "user.add_to_group":
		if s.User == "" || s.Group == "" || s.Names != nil || s.Unit != "" {
			return nil, fmt.Errorf("%s takes a user and a group and nothing else", s.Op)
		}
		if !unixNameRe.MatchString(s.User) || !unixNameRe.MatchString(s.Group) {
			return nil, fmt.Errorf("%s: want Unix user and group names", s.Op)
		}
		if privilegedGroups[s.Group] {
			return nil, fmt.Errorf("%s: refused for the privileged group %q", s.Op, s.Group)
		}
		return []string{"usermod", "-aG", s.Group, "--", s.User}, nil
	}
	return nil, fmt.Errorf("unknown op %q (want pkg.install, pkg.remove, unit.enable, unit.restart or user.add_to_group)", s.Op)
}

// Command is the argv a step runs, typed operations expanded.
func (s *Step) Command() []string {
	if s.Op != "" {
		argv, _ := s.expand()
		return argv
	}
	return s.Argv
}
