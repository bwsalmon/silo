package approval

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Presentation is what the human is shown for a batch (design §6.3),
// computed by the gateway from the staged content at submit time: facts
// first, the agent's own words last and labelled untrusted.
type Presentation struct {
	Title       string    `json:"title"`
	Hash        string    `json:"hash"`
	ShortHash   string    `json:"short_hash"`
	SubmittedBy string    `json:"submitted_by"`
	SubmittedAt time.Time `json:"submitted_at"`
	RunsAs      string    `json:"runs_as"`
	// Snapshot is "Snapshot: <kind>, taken before run" or NoSnapshotMessage.
	Snapshot          string `json:"snapshot"`
	SnapshotAvailable bool   `json:"snapshot_available"`
	// NeedsHashConfirmation is set when the batch edits the root agent's
	// own instructions: approving it needs the short hash typed back.
	NeedsHashConfirmation bool `json:"needs_hash_confirmation,omitempty"`

	Flags       []Flag                   `json:"flags"`
	Files       []FilePresentation       `json:"files"`
	Steps       []StepPresentation       `json:"steps"`
	Attachments []AttachmentPresentation `json:"attachments"`
	Expects     *Expects                 `json:"expects,omitempty"`

	// Reason is the agent's explanation, verbatim. Render it as plain text.
	ReasonLabel string `json:"reason_label"`
	Reason      string `json:"reason"`
}

// FilePresentation is one file write: what changes and the diff.
type FilePresentation struct {
	Path    string `json:"path"`
	Delete  bool   `json:"delete,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Group   string `json:"group,omitempty"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	Exists  bool   `json:"exists"`
	CurMode string `json:"current_mode,omitempty"`
	CurOwn  string `json:"current_owner,omitempty"`
	// Diff is a unified diff against the file currently on disk.
	Diff string `json:"diff"`
	// DefaultsDiff is set for root-instruction files: the diff against the
	// shipped defaults (design §9.2).
	DefaultsDiff string `json:"defaults_diff,omitempty"`
}

// StepPresentation is one command with argv element by element, escaped.
type StepPresentation struct {
	Index     int      `json:"index"`
	Op        string   `json:"op,omitempty"`
	Argv      []string `json:"argv"`
	Exe       string   `json:"exe"`
	ExeSHA256 string   `json:"exe_sha256"`
	Cwd       string   `json:"cwd"`
	Env       []string `json:"env,omitempty"`
	Stdin     string   `json:"stdin,omitempty"`
	TimeoutS  int      `json:"timeout_s"`
}

// AttachmentPresentation is an attachment, shown in full if it is text.
type AttachmentPresentation struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Executed bool   `json:"executed,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	Content  string `json:"content,omitempty"`
}

// ReasonLabel heads the agent's explanation.
const ReasonLabel = "Agent's explanation (untrusted)"

// ShippedRoot is where the package installs the default instructions.
const ShippedRoot = "/usr/share/silo"

// maxShown bounds content inlined into a presentation.
const maxShown = 256 << 10

// present computes the presentation of a bound batch. shippedDir is the
// directory holding the shipped defaults (normally /usr/share/silo).
func present(bd *Bound, hash, submitter string, at time.Time, snapKind string, files, attachments [][]byte, shippedDir string) Presentation {
	attByName := map[string][]byte{}
	for i, a := range bd.Attachments {
		attByName[a.Name] = attachments[i]
	}
	p := Presentation{
		Title: bd.Title, Hash: hash, ShortHash: hash[:16], SubmittedBy: submitter, SubmittedAt: at,
		RunsAs:   "root (uid 0), outside every agent's sandbox, with the box's normal network",
		Snapshot: SnapshotStatus(snapKind), SnapshotAvailable: snapKind != "" && snapKind != SnapNone,
		Flags: Flags(bd, attByName), Expects: bd.Expects, ReasonLabel: ReasonLabel, Reason: bd.Reason,
		Files: []FilePresentation{}, Steps: []StepPresentation{}, Attachments: []AttachmentPresentation{},
	}
	if p.Flags == nil {
		p.Flags = []Flag{}
	}
	for _, f := range p.Flags {
		if f.Code == FlagRootInstructions {
			p.NeedsHashConfirmation = true
		}
	}
	for i, f := range bd.Files {
		fp := FilePresentation{Path: f.Path, Delete: f.Delete, Mode: f.Mode, Owner: f.Owner, Group: f.Group, Size: f.Size, SHA256: f.SHA256}
		var newContent []byte
		if !f.Delete {
			newContent = files[i]
			fp.Binary = !isText(newContent)
		}
		cur, curInfo, err := readCurrent(f.Path)
		if err == nil {
			fp.Exists = true
			fp.CurMode = fmt.Sprintf("%04o", curInfo.Mode().Perm())
			fp.CurOwn = ownerOf(curInfo)
		}
		fp.Diff = UnifiedDiff(f.Path+" (current)", f.Path+" (batch)", cur, newContent)
		if isRootInstructionsPath(f.Path) {
			base, label := shippedDefault(f.Path, shippedDir)
			fp.DefaultsDiff = UnifiedDiff(label, f.Path+" (batch)", base, newContent)
			if fp.DefaultsDiff == "" {
				fp.DefaultsDiff = "(identical to the shipped defaults)\n"
			}
		}
		p.Files = append(p.Files, fp)
	}
	executed := map[string]bool{}
	for i, s := range bd.Steps {
		sp := StepPresentation{Index: i, Op: s.Op, Exe: s.Exe.Path, ExeSHA256: s.Exe.SHA256, Cwd: s.Cwd, Stdin: s.StdinAttachment, TimeoutS: s.TimeoutS}
		for _, a := range s.Command {
			sp.Argv = append(sp.Argv, Escape(a))
			if name, ok := strings.CutPrefix(a, AttachmentPrefix); ok {
				executed[name] = true
			}
		}
		for _, kv := range sortedEnv(s.Env) {
			sp.Env = append(sp.Env, Escape(kv))
		}
		if s.StdinAttachment != "" {
			executed[s.StdinAttachment] = true
		}
		p.Steps = append(p.Steps, sp)
	}
	for i, a := range bd.Attachments {
		ap := AttachmentPresentation{Name: a.Name, Mode: a.Mode, Size: a.Size, SHA256: a.SHA256, Executed: executed[a.Name]}
		if c := attachments[i]; isText(c) && len(c) <= maxShown {
			ap.Content = string(c)
		} else {
			ap.Binary = !isText(c)
		}
		p.Attachments = append(p.Attachments, ap)
	}
	return p
}

func readCurrent(p string) ([]byte, fs.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		if fi.Mode()&fs.ModeSymlink != 0 {
			t, _ := os.Readlink(p)
			return []byte("symlink -> " + t + "\n"), fi, nil
		}
		return nil, fi, nil
	}
	if fi.Size() > maxShown*4 {
		return []byte(fmt.Sprintf("(%d bytes, not read)\n", fi.Size())), fi, nil
	}
	data, err := os.ReadFile(p)
	return data, fi, err
}

func ownerOf(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, g := strconv.Itoa(int(st.Uid)), strconv.Itoa(int(st.Gid))
	if x, err := user.LookupId(u); err == nil {
		u = x.Username
	}
	if x, err := user.LookupGroupId(g); err == nil {
		g = x.Name
	}
	return u + ":" + g
}

// shippedDefault is the shipped counterpart of a root-instruction path: for
// /etc/silo/root.md, the shipped root.md it replaces; for a file under
// /usr/share/silo, the shipped file itself; for anything else (root.d
// additions) nothing, since the shipped defaults have no such file.
func shippedDefault(p, shippedDir string) ([]byte, string) {
	var rel string
	switch {
	case p == "/etc/silo/root.md":
		rel = "root.md"
	case strings.HasPrefix(p, ShippedRoot+"/"):
		rel = strings.TrimPrefix(p, ShippedRoot+"/")
	default:
		return nil, "(not in the shipped defaults)"
	}
	src := filepath.Join(shippedDir, rel)
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ShippedRoot + "/" + rel + " (shipped default, not installed)"
	}
	return data, ShippedRoot + "/" + rel + " (shipped default)"
}

func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Text renders the presentation for a terminal (silo approvals show).
func (p *Presentation) Text() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("%s", Escape(p.Title))
	line("  hash:      %s", p.Hash)
	line("  from:      %s at %s", p.SubmittedBy, p.SubmittedAt.Format(time.RFC3339))
	line("  runs as:   %s", p.RunsAs)
	if p.SnapshotAvailable {
		line("  snapshot:  %s", p.Snapshot)
	} else {
		line("  snapshot:  *** %s ***", strings.ToUpper(p.Snapshot[:1])+p.Snapshot[1:])
		line("             File writes can be undone from the staged previous content; command effects cannot.")
	}
	if p.NeedsHashConfirmation {
		line("  *** This batch %s. Approving it needs --confirm %s. ***", RootInstructionsMessage, p.ShortHash)
	}
	line("")
	if len(p.Flags) > 0 {
		line("Risk flags:")
		for _, f := range p.Flags {
			mark := " "
			if f.Blocking {
				mark = "!"
			}
			line("  %s [%s] %s  (%s)", mark, f.Code, f.Message, Escape(f.Where))
		}
		line("")
	}
	if len(p.Files) > 0 {
		line("Files:")
		for i, f := range p.Files {
			switch {
			case f.Delete:
				line("  [%d] delete %s", i, Escape(f.Path))
			case f.Exists:
				line("  [%d] write  %s  %s %s:%s (now %s %s), %d bytes", i, Escape(f.Path), f.Mode, f.Owner, f.Group, f.CurMode, f.CurOwn, f.Size)
			default:
				line("  [%d] create %s  %s %s:%s, %d bytes", i, Escape(f.Path), f.Mode, f.Owner, f.Group, f.Size)
			}
		}
		line("")
	}
	if len(p.Steps) > 0 {
		line("Commands (argv element by element):")
		for _, s := range p.Steps {
			var parts []string
			for _, a := range s.Argv {
				parts = append(parts, "["+a+"]")
			}
			op := ""
			if s.Op != "" {
				op = " (" + s.Op + ")"
			}
			line("  [%d]%s %s", s.Index, op, strings.Join(parts, " "))
			line("      exe %s sha256:%s  cwd %s  timeout %ds", s.Exe, short(s.ExeSHA256), s.Cwd, s.TimeoutS)
			if len(s.Env) > 0 {
				line("      env %s", strings.Join(s.Env, " "))
			}
			if s.Stdin != "" {
				line("      stdin from attachment %s", s.Stdin)
			}
		}
		line("")
	}
	for _, f := range p.Files {
		if f.DefaultsDiff != "" {
			line("Diff of %s against the shipped defaults (%s):", Escape(f.Path), RootInstructionsMessage)
			b.WriteString(f.DefaultsDiff)
			line("")
		}
		if f.Diff != "" {
			line("Diff of %s against the file on disk:", Escape(f.Path))
			b.WriteString(f.Diff)
			line("")
		}
	}
	for _, a := range p.Attachments {
		what := "attachment"
		if a.Executed {
			what = "attachment (executed or read by a step)"
		}
		line("%s %s, mode %s, %d bytes, sha256:%s", what, a.Name, a.Mode, a.Size, short(a.SHA256))
		if a.Content != "" {
			for _, l := range splitLines(a.Content) {
				line("  | %s", Escape(strings.TrimSuffix(l, "\n")))
			}
		}
		line("")
	}
	line("%s:", p.ReasonLabel)
	for _, l := range strings.Split(p.Reason, "\n") {
		line("  > %s", Escape(l))
	}
	return b.String()
}

func short(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
