// Package grainfiles parses, validates, resolves, and evaluates triggers and
// same-repository reference access grants for .task.md and .process.md
// Grainfiles (docs/grainfiles-spec.md, docs/grainfiles.md, docs/hub.md §6,
// docs/hub-interface.md).
package grainfiles

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind is a registered Grainfile kind (docs/grainfiles-spec.md §12.1).
type Kind string

const (
	// KindTask is a discrete task (<name>.task.md) executed once.
	KindTask Kind = "task"
	// KindProcess is a standing repeatable process (<name>.process.md).
	KindProcess Kind = "process"
)

// Valid reports whether k is a registered Grainfile kind.
func (k Kind) Valid() bool {
	return k == KindTask || k == KindProcess
}

// Registered trigger sources for .process.md files (docs/grainfiles-spec.md §12.2).
const (
	TriggerSchedule    = "schedule"
	TriggerPush        = "push"
	TriggerMerge       = "merge"
	TriggerPullRequest = "pull_request"
	TriggerManual      = "manual"
	TriggerScenario    = "scenario"
	TriggerWith        = "trigger_with"
)

// Resource limits (docs/grainfiles-spec.md §4.1.4, §4.3, §11.4).
const (
	MaxFileBytes        = 1024 * 1024
	MaxFrontMatterBytes = 64 * 1024
	MaxPathBytes        = 1024
	MaxSegmentBytes     = 255
)

// Lint and validation codes (docs/grainfiles-spec.md §10).
const (
	CodeBadName         = "GF001"
	CodeMalformedFile   = "GF003"
	CodeInvalidYAML     = "GF004"
	CodeUnknownSuffix   = "GF005"
	CodeLimitExceeded   = "GF006"
	CodeSchemaInvalid   = "GF020"
	CodeUnknownField    = "GF022"
	CodeUnresolvedRef   = "GF030"
	CodeWrongTargetKind = "GF032"
	CodeCycle           = "GF034"
	CodeMalformedRef    = "GF035"
	CodeEscapesRepo     = "GF036"
)

// Severity is the severity of a lint or validation finding (docs/grainfiles-spec.md §10.2).
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// SeverityOf returns the default severity for a GFxxx lint code.
func SeverityOf(code string) Severity {
	switch code {
	case CodeUnknownSuffix, CodeUnknownField:
		return SeverityWarning
	default:
		return SeverityError
	}
}

// Finding is a single validation or lint diagnostic (docs/grainfiles-spec.md §10.1).
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
}

func newFinding(code, path, msg string) Finding {
	return Finding{
		Code:     code,
		Severity: SeverityOf(code),
		Path:     path,
		Message:  msg,
	}
}

func (f Finding) Error() string {
	if f.Path != "" {
		return fmt.Sprintf("%s %s %s: %s", f.Code, f.Severity, f.Path, f.Message)
	}
	return fmt.Sprintf("%s %s: %s", f.Code, f.Severity, f.Message)
}

// Errors is a non-empty list of error-severity findings.
type Errors []Finding

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, f := range e {
		parts[i] = f.Error()
	}
	return strings.Join(parts, "\n")
}

// Grainfile is a parsed and validated .task.md or .process.md file.
type Grainfile struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`

	// Authoritative source metadata populated by Resolve.
	URL    string `json:"url,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`

	// Front-matter fields (docs/grainfiles-spec.md §6, §7).
	Title        string         `json:"title"`
	Recipient    string         `json:"recipient"`
	Requestor    string         `json:"requestor,omitempty"`
	Draft        bool           `json:"draft"`
	NotBefore    time.Time      `json:"not_before,omitempty"`
	NotBeforeRaw string         `json:"-"`
	CreatedAt    time.Time      `json:"created_at,omitempty"`
	CreatedAtRaw string         `json:"-"`
	References   []string       `json:"references,omitempty"`
	DependsOn    []string       `json:"depends_on,omitempty"`
	TriggerWith  []string       `json:"trigger_with,omitempty"`
	On           Triggers       `json:"on,omitempty"`
	Gate         bool           `json:"gate,omitempty"`
	Extensions   map[string]any `json:"extensions,omitempty"`

	// Body is the UTF-8 Markdown prompt body after the closing --- line.
	Body string `json:"-"`

	// Warnings holds non-fatal findings such as GF022 (unknown non-x_ fields).
	Warnings []Finding `json:"-"`
}

// Triggers holds the structured trigger rules under `on` in a .process.md file
// (docs/grainfiles-spec.md §7.2.1).
type Triggers struct {
	Schedule    *ScheduleTrigger `json:"schedule,omitempty"`
	Push        *EventFilter     `json:"push,omitempty"`
	Merge       *EventFilter     `json:"merge,omitempty"`
	PullRequest *EventFilter     `json:"pull_request,omitempty"`
	Manual      *ManualTrigger   `json:"manual,omitempty"`
	Scenario    *ScenarioTrigger `json:"scenario,omitempty"`
}

// IsZero reports whether no trigger source is configured.
func (t Triggers) IsZero() bool {
	return t.Schedule == nil &&
		t.Push == nil &&
		t.Merge == nil &&
		t.PullRequest == nil &&
		t.Manual == nil &&
		t.Scenario == nil
}

// ScheduleTrigger is `on.schedule` (docs/grainfiles-spec.md §7.2.1).
type ScheduleTrigger struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
}

// EventFilter is `on.push`, `on.merge`, or `on.pull_request`
// (docs/grainfiles-spec.md §7.2.1).
type EventFilter struct {
	Branches []string `json:"branches,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Ignore   []string `json:"ignore,omitempty"`
}

// ManualTrigger is `on.manual: {}` (docs/grainfiles-spec.md §7.2.1).
type ManualTrigger struct{}

// ScenarioTrigger is `on.scenario` (docs/grainfiles-spec.md §7.2.1).
type ScenarioTrigger struct {
	Description string `json:"description"`
}

var (
	stemRe         = regexp.MustCompile(`^(\d{8}T\d{6}Z|[a-z0-9])[a-z0-9._-]*$`)
	compactUTCRe   = regexp.MustCompile(`^(\d{8}T\d{6}Z)`)
	unknownKindRe  = regexp.MustCompile(`^(.+)\.([a-z][a-z0-9-]*)\.md$`)
	userIdentityRe = regexp.MustCompile(`^[^<>\s][^<>]*\s+<[^<>\s@]+@[^<>\s@]+\.[^<>\s@]+>$`)
	timeRe         = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.(\d+))?Z$`)
	dateRe         = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// ClassifyPath classifies a repository-relative path according to
// docs/grainfiles-spec.md §4.1 and §4.2.
//
// If relPath ends with .task.md or .process.md, isGrainfile is true and kind
// and stem are returned. If relPath is an asset Markdown file with an
// unregistered .<kind>.md suffix (excluding README*), warnCode is "GF005".
func ClassifyPath(relPath string) (kind Kind, stem string, isGrainfile bool, warnCode string) {
	norm := filepath.ToSlash(relPath)
	leaf := norm
	if idx := strings.LastIndexByte(norm, '/'); idx >= 0 {
		leaf = norm[idx+1:]
	}
	for _, k := range []Kind{KindTask, KindProcess} {
		suf := "." + string(k) + ".md"
		if strings.HasSuffix(leaf, suf) {
			return k, leaf[:len(leaf)-len(suf)], true, ""
		}
	}
	if unknownKindRe.MatchString(leaf) && !strings.HasPrefix(strings.ToLower(leaf), "readme") {
		return "", "", false, CodeUnknownSuffix
	}
	return "", "", false, ""
}

// ValidateStem checks whether stem matches the Grainfile stem grammar
// (docs/grainfiles-spec.md §4.1).
func ValidateStem(stem string) bool {
	if !stemRe.MatchString(stem) {
		return false
	}
	if m := compactUTCRe.FindString(stem); m != "" {
		if _, err := time.Parse("20060102T150405Z", m); err != nil {
			return false
		}
	}
	return true
}

// ParseFilename validates a Grainfile filename or repository-relative path
// and extracts its Kind and stem (docs/grainfiles-spec.md §4.1).
func ParseFilename(relPath string) (Kind, string, error) {
	norm := strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	if len(norm) > MaxPathBytes {
		return "", "", newFinding(CodeLimitExceeded, relPath, "path exceeds 1024 bytes")
	}
	for _, seg := range strings.Split(norm, "/") {
		if len(seg) > MaxSegmentBytes {
			return "", "", newFinding(CodeLimitExceeded, relPath, "path segment exceeds 255 bytes")
		}
	}
	kind, stem, isGrain, warnCode := ClassifyPath(norm)
	if !isGrain {
		if warnCode != "" {
			return "", "", newFinding(warnCode, relPath, "unregistered kind suffix")
		}
		return "", "", newFinding(CodeBadName, relPath, "filename must end with .task.md or .process.md")
	}
	if !ValidateStem(stem) {
		return kind, stem, newFinding(CodeBadName, relPath, fmt.Sprintf("bad grainfile name %q", stem))
	}
	return kind, stem, nil
}

// IsUserIdentity reports whether s matches the RFC 5322 / git author notation
// `Name <email>` (docs/grainfiles-spec.md §6.2.1).
func IsUserIdentity(s string) bool {
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	return userIdentityRe.MatchString(s)
}

// ParseTime parses and validates an RFC 3339 UTC timestamp with a Z suffix
// (`YYYY-MM-DDTHH:MM:SSZ`, with optional fractional seconds;
// docs/grainfiles-spec.md §1.4).
func ParseTime(s string) (time.Time, error) {
	m := timeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid UTC timestamp %q", s)
	}
	if _, err := time.Parse("2006-01-02T15:04:05", m[1]+"T"+m[2]); err != nil {
		return time.Time{}, fmt.Errorf("invalid calendar date or UTC time %q: %w", s, err)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid UTC timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// ParseNotBefore parses and validates a `not_before` string, which may be
// either an RFC 3339 UTC timestamp (`YYYY-MM-DDTHH:MM:SSZ`) or a UTC calendar
// date (`YYYY-MM-DD`, representing 00:00:00Z at the start of that day in UTC;
// docs/grainfiles-spec.md §1.4, §6.4).
func ParseNotBefore(s string) (time.Time, error) {
	if dateRe.MatchString(s) {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid calendar date %q: %w", s, err)
		}
		return t.UTC(), nil
	}
	return ParseTime(s)
}

// ParseFile reads, parses, and validates a .task.md or .process.md file from disk.
func ParseFile(fsPath string) (*Grainfile, error) {
	data, err := os.ReadFile(fsPath)
	if err != nil {
		return nil, err
	}
	return Parse(fsPath, data)
}

// Parse parses and validates a .task.md or .process.md Grainfile from its
// path/filename and raw UTF-8 bytes (docs/grainfiles-spec.md §4–§8).
func Parse(path string, data []byte) (*Grainfile, error) {
	var errs Errors
	normPath := filepath.ToSlash(path)
	if len(normPath) > MaxPathBytes {
		errs = append(errs, newFinding(CodeLimitExceeded, path, "path too long"))
	} else {
		for _, seg := range strings.Split(normPath, "/") {
			if len(seg) > MaxSegmentBytes {
				errs = append(errs, newFinding(CodeLimitExceeded, path, "path too long"))
				break
			}
		}
	}

	kind, stem, isGrain, warnCode := ClassifyPath(normPath)
	if !isGrain {
		if warnCode != "" {
			return nil, Errors{newFinding(warnCode, path, "unregistered kind suffix")}
		}
		return nil, Errors{newFinding(CodeBadName, path, "filename must end with .task.md or .process.md")}
	}
	if !ValidateStem(stem) {
		errs = append(errs, newFinding(CodeBadName, path, fmt.Sprintf("bad grainfile name %q", stem)))
	}

	g, parseErrs := parseWithKind(kind, stem, path, data)
	errs = append(errs, parseErrs...)
	if len(errs) > 0 {
		return g, errs
	}
	return g, nil
}

// ParseKind parses and validates Grainfile bytes for an explicit Kind when no
// filename is available.
func ParseKind(kind Kind, data []byte) (*Grainfile, error) {
	if !kind.Valid() {
		return nil, Errors{newFinding(CodeBadName, "", fmt.Sprintf("unknown grainfile kind %q", kind))}
	}
	g, errs := parseWithKind(kind, "", "", data)
	if len(errs) > 0 {
		return g, errs
	}
	return g, nil
}

func parseWithKind(kind Kind, stem, path string, raw []byte) (*Grainfile, Errors) {
	fm, body, fErr := splitAndParseFrontMatter(path, raw)
	if fErr != nil {
		return nil, Errors{*fErr}
	}
	g := &Grainfile{
		Kind: kind,
		Name: stem,
		Path: filepath.ToSlash(path),
		Body: body,
	}
	errs := validateFrontMatter(g, fm, path)
	return g, errs
}

func splitAndParseFrontMatter(path string, raw []byte) (map[string]any, string, *Finding) {
	if len(raw) > MaxFileBytes {
		f := newFinding(CodeLimitExceeded, path, "grainfile exceeds 1 MiB")
		return nil, "", &f
	}
	if bytes.HasPrefix(raw, []byte("version https://git-lfs")) {
		f := newFinding(CodeMalformedFile, path, "a grainfile may not be an LFS pointer")
		return nil, "", &f
	}
	if bytes.IndexByte(raw, 0) >= 0 || bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		f := newFinding(CodeMalformedFile, path, "NUL or byte-order mark")
		return nil, "", &f
	}
	if !utf8.Valid(raw) {
		f := newFinding(CodeMalformedFile, path, "not UTF-8")
		return nil, "", &f
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		f := newFinding(CodeMalformedFile, path, "no front matter")
		return nil, "", &f
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		f := newFinding(CodeMalformedFile, path, "unclosed front matter")
		return nil, "", &f
	}
	src := strings.Join(lines[1:end], "\n")
	if len(src) > MaxFrontMatterBytes {
		f := newFinding(CodeLimitExceeded, path, "front matter exceeds 64 KiB")
		return nil, "", &f
	}
	fm, fErr := parseYAMLFrontMatter(path, src)
	if fErr != nil {
		return nil, "", fErr
	}
	body := strings.Join(lines[end+1:], "\n")
	return fm, body, nil
}

var (
	knownTaskFields = map[string]bool{
		"title":        true,
		"requestor":    true,
		"recipient":    true,
		"created_at":   true,
		"draft":        true,
		"not_before":   true,
		"depends_on":   true,
		"trigger_with": true,
		"references":   true,
	}
	knownProcessFields = map[string]bool{
		"title":        true,
		"requestor":    true,
		"recipient":    true,
		"created_at":   true,
		"draft":        true,
		"not_before":   true,
		"depends_on":   true,
		"trigger_with": true,
		"references":   true,
		"on":           true,
		"gate":         true,
	}
	knownTriggerSources = map[string]bool{
		TriggerSchedule:    true,
		TriggerPush:        true,
		TriggerMerge:       true,
		TriggerPullRequest: true,
		TriggerManual:      true,
		TriggerScenario:    true,
	}
)

func validateFrontMatter(g *Grainfile, fm map[string]any, where string) Errors {
	var errs Errors
	known := knownTaskFields
	if g.Kind == KindProcess {
		known = knownProcessFields
	}

	keys := make([]string, 0, len(fm))
	for k := range fm {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if strings.HasPrefix(k, "x_") {
			if g.Extensions == nil {
				g.Extensions = make(map[string]any)
			}
			g.Extensions[k] = fm[k]
			continue
		}
		if !known[k] {
			g.Warnings = append(g.Warnings, newFinding(CodeUnknownField, where, fmt.Sprintf("unknown field %q", k)))
		}
	}

	// title (required non-empty single-line string)
	if rawTitle, ok := fm["title"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: required field missing"))
	} else if title, ok := rawTitle.(string); !ok || title == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: must be a non-empty string"))
	} else if strings.ContainsAny(title, "\r\n") {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title must be a single line"))
	} else {
		g.Title = title
	}

	// recipient (required deployment identity)
	if rawRec, ok := fm["recipient"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: required field missing"))
	} else if rec, ok := rawRec.(string); !ok || rec == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: must be a non-empty string"))
	} else if strings.ContainsAny(rec, "<>@\\ \t\r\n") {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: must be a deployment URL or repo-local folder path, not a user identity"))
	} else {
		g.Recipient = rec
		if f := validateDeploymentIdentitySyntax(rec, "recipient", where); f != nil {
			errs = append(errs, *f)
		}
	}

	// requestor (optional user identity or deployment identity)
	if rawReq, ok := fm["requestor"]; ok {
		if req, ok := rawReq.(string); !ok || req == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "requestor: must be a non-empty string"))
		} else {
			g.Requestor = req
			if !IsUserIdentity(req) {
				if strings.ContainsAny(req, "<>@\\ \t\r\n") {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("requestor: invalid identity %q", req)))
				} else if f := validateDeploymentIdentitySyntax(req, "requestor", where); f != nil {
					errs = append(errs, *f)
				}
			}
		}
	}

	// draft (optional bool, default false)
	if rawDraft, ok := fm["draft"]; ok {
		if b, ok := rawDraft.(bool); !ok {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "draft: must be a boolean"))
		} else {
			g.Draft = b
		}
	}

	// created_at (optional RFC 3339 UTC timestamp)
	if rawCA, ok := fm["created_at"]; ok {
		if ca, ok := rawCA.(string); !ok || ca == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "created_at: must be an RFC 3339 UTC timestamp string"))
		} else {
			t, err := ParseTime(ca)
			if err != nil {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("created_at: %v", err)))
			} else {
				g.CreatedAt = t
				g.CreatedAtRaw = ca
			}
		}
	}

	// not_before (optional RFC 3339 UTC timestamp or YYYY-MM-DD UTC date)
	if rawNB, ok := fm["not_before"]; ok {
		if nb, ok := rawNB.(string); !ok || nb == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "not_before: must be an RFC 3339 UTC timestamp or YYYY-MM-DD date string"))
		} else {
			t, err := ParseNotBefore(nb)
			if err != nil {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("not_before: %v", err)))
			} else {
				g.NotBefore = t
				g.NotBeforeRaw = nb
			}
		}
	}

	// references (optional list of refs)
	if rawRefs, ok := fm["references"]; ok {
		refs, refErrs := validateRefSlice(rawRefs, "references", where, false)
		errs = append(errs, refErrs...)
		g.References = refs
	}

	// depends_on (optional list of refs to .task.md / .process.md)
	if rawDeps, ok := fm["depends_on"]; ok {
		deps, depErrs := validateRefSlice(rawDeps, "depends_on", where, true)
		errs = append(errs, depErrs...)
		g.DependsOn = deps
	}

	// trigger_with (optional string or non-empty list of refs to .task.md / .process.md)
	_, hasTriggerWith := fm["trigger_with"]
	if hasTriggerWith {
		rawTW := fm["trigger_with"]
		switch v := rawTW.(type) {
		case string:
			if v == "" || strings.ContainsAny(v, " \t\r\n\\") {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be a valid reference"))
			} else {
				g.TriggerWith = []string{v}
				if f := validateSingleRef(v, "trigger_with", where, true, g.Path); f != nil {
					errs = append(errs, *f)
				}
			}
		case []any:
			if len(v) == 0 {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be non-empty"))
			} else {
				tw, twErrs := validateRefSliceWithSourcePath(rawTW, "trigger_with", where, true, g.Path)
				errs = append(errs, twErrs...)
				g.TriggerWith = tw
			}
		default:
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be a string or non-empty list of strings"))
		}
	}

	// Validate local climbing above repo root if g.Path is relative to repo root.
	// Note: when g.Path is only a filesystem path, local traversal is checked on Resolve
	// and when climbing above g.Path itself.
	for _, ref := range g.References {
		if f := checkLocalClimb(ref, "references", where, g.Path); f != nil {
			errs = append(errs, *f)
		}
	}
	for _, ref := range g.DependsOn {
		if f := checkLocalClimb(ref, "depends_on", where, g.Path); f != nil {
			errs = append(errs, *f)
		}
	}

	if g.Kind == KindProcess {
		// gate (optional bool, default false)
		if rawGate, ok := fm["gate"]; ok {
			if b, ok := rawGate.(bool); !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "gate: must be a boolean"))
			} else {
				g.Gate = b
			}
		}

		_, hasOn := fm["on"]
		if !hasOn && !hasTriggerWith {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "process must specify at least one of on or trigger_with"))
		}
		if hasOn {
			triggers, onErrs := validateOnTriggers(fm["on"], where)
			errs = append(errs, onErrs...)
			g.On = triggers
		}
	}

	return errs
}

func validateDeploymentIdentitySyntax(s, field, where string) *Finding {
	r, err := ParseRef(s)
	if err != nil {
		if f, ok := err.(Finding); ok {
			f.Path = where
			f.Message = fmt.Sprintf("%s: %s", field, f.Message)
			return &f
		}
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("%s: %v", field, err))
		return &f
	}
	if r.Anchor != "" {
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("%s: deployment identity %q may not carry an anchor", field, s))
		return &f
	}
	if r.Mode == "blob" {
		f := newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a file reference (/blob/), expected a repository or folder", field, s))
		return &f
	}
	for _, k := range []Kind{KindTask, KindProcess} {
		suf := "." + string(k) + ".md"
		if strings.HasSuffix(r.Path, suf) || strings.HasSuffix(r.Repo, suf) {
			f := newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a %s grainfile, expected a repository or folder", field, s, k))
			return &f
		}
	}
	if f := checkLocalClimb(s, field, where, where); f != nil {
		return f
	}
	return nil
}

func validateRefSlice(raw any, field, where string, mustBeGrainfile bool) ([]string, Errors) {
	return validateRefSliceWithSourcePath(raw, field, where, mustBeGrainfile, "")
}

func validateRefSliceWithSourcePath(raw any, field, where string, mustBeGrainfile bool, sourcePath string) ([]string, Errors) {
	list, ok := raw.([]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: must be a list of strings", field))}
	}
	var out []string
	var errs Errors
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" || strings.ContainsAny(s, " \t\r\n\\") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.%d: must be a non-empty reference string", field, i)))
			continue
		}
		out = append(out, s)
		if f := validateSingleRef(s, field, where, mustBeGrainfile, sourcePath); f != nil {
			errs = append(errs, *f)
		}
	}
	return out, errs
}

func validateSingleRef(s, field, where string, mustBeGrainfile bool, sourcePath string) *Finding {
	r, err := ParseRef(s)
	if err != nil {
		if f, ok := err.(Finding); ok {
			f.Path = where
			f.Message = fmt.Sprintf("%s: %s", field, f.Message)
			return &f
		}
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("%s: %v", field, err))
		return &f
	}
	if sourcePath != "" {
		if f := checkLocalClimb(s, field, where, sourcePath); f != nil {
			return f
		}
	}
	if mustBeGrainfile {
		if r.Mode == "tree" || r.TrailingSlash {
			f := newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a folder, expected task or process", field, s))
			return &f
		}
		targetPath := r.Path
		if !strings.HasSuffix(targetPath, ".task.md") && !strings.HasSuffix(targetPath, ".process.md") {
			f := newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is not a task or process grainfile", field, s))
			return &f
		}
	}
	return nil
}

func checkLocalClimb(s, field, where, sourcePath string) *Finding {
	r, err := ParseRef(s)
	if err != nil {
		return nil
	}
	if r.Form == RefRooted {
		if r.Path != "" {
			cleaned := cleanPosixPath(r.Path)
			if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
				f := newFinding(CodeEscapesRepo, where, fmt.Sprintf("%s: %q climbs above the repository root", field, s))
				return &f
			}
		}
		return nil
	}
	if r.Form == RefRelative && sourcePath != "" && !strings.HasPrefix(sourcePath, "/") {
		cleanSrc := cleanPosixPath(sourcePath)
		if cleanSrc == ".." || strings.HasPrefix(cleanSrc, "../") {
			return nil
		}
		dir := cleanPosixDir(cleanSrc)
		joined := r.Path
		if dir != "" && dir != "." {
			joined = dir + "/" + r.Path
		}
		cleaned := cleanPosixPath(joined)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			f := newFinding(CodeEscapesRepo, where, fmt.Sprintf("%s: %q climbs above the repository root", field, s))
			return &f
		}
	}
	return nil
}

func validateOnTriggers(raw any, where string) (Triggers, Errors) {
	var t Triggers
	var errs Errors

	m, ok := raw.(map[string]any)
	if !ok {
		return t, Errors{newFinding(CodeSchemaInvalid, where, "on: must be a mapping")}
	}
	if len(m) == 0 {
		return t, Errors{newFinding(CodeSchemaInvalid, where, "on: must have at least one trigger source")}
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val := m[k]
		if !knownTriggerSources[k] {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("unknown trigger source %q in on", k)))
			continue
		}
		switch k {
		case TriggerSchedule:
			sm, ok := val.(map[string]any)
			if !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.schedule: must be a mapping"))
				continue
			}
			for sk := range sm {
				if sk != "cron" && sk != "timezone" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("on.schedule: unknown property %q", sk)))
				}
			}
			rawCron, hasCron := sm["cron"]
			if !hasCron {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.schedule: cron is required"))
				continue
			}
			cronStr, ok := rawCron.(string)
			if !ok || cronStr == "" {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.schedule.cron: must be a non-empty string"))
				continue
			}
			if len(strings.Fields(cronStr)) != 5 {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "schedule.cron must be a five-field cron expression"))
				continue
			}
			tz := "UTC"
			if rawTZ, hasTZ := sm["timezone"]; hasTZ {
				tzStr, ok := rawTZ.(string)
				if !ok || tzStr == "" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.schedule.timezone: must be a non-empty string"))
					continue
				}
				tz = tzStr
			}
			t.Schedule = &ScheduleTrigger{
				Cron:     cronStr,
				Timezone: tz,
			}

		case TriggerPush, TriggerMerge, TriggerPullRequest:
			ef, efErrs := validateEventFilter(val, "on."+k, where)
			errs = append(errs, efErrs...)
			if len(efErrs) == 0 {
				switch k {
				case TriggerPush:
					t.Push = ef
				case TriggerMerge:
					t.Merge = ef
				case TriggerPullRequest:
					t.PullRequest = ef
				}
			}

		case TriggerManual:
			mm, ok := val.(map[string]any)
			if !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.manual: must be an empty mapping {}"))
				continue
			}
			if len(mm) > 0 {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.manual: does not accept properties"))
				continue
			}
			t.Manual = &ManualTrigger{}

		case TriggerScenario:
			switch sv := val.(type) {
			case string:
				if sv == "" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.scenario: must be a non-empty string"))
				} else {
					t.Scenario = &ScenarioTrigger{Description: sv}
				}
			case map[string]any:
				for sk := range sv {
					if sk != "description" {
						errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("on.scenario: unknown property %q", sk)))
					}
				}
				rawDesc, hasDesc := sv["description"]
				if !hasDesc {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.scenario: description is required"))
				} else if desc, ok := rawDesc.(string); !ok || desc == "" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.scenario.description: must be a non-empty string"))
				} else {
					t.Scenario = &ScenarioTrigger{Description: desc}
				}
			default:
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "on.scenario: must be a string or {description} mapping"))
			}
		}
	}

	return t, errs
}

func validateEventFilter(raw any, fieldPath, where string) (*EventFilter, Errors) {
	var errs Errors
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a mapping")}
	}
	for k := range m {
		if k != "branches" && k != "paths" && k != "ignore" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: unknown property %q", fieldPath, k)))
		}
	}

	ef := &EventFilter{}
	if rawBranches, ok := m["branches"]; ok {
		bs, bErrs := validateStringSlice(rawBranches, fieldPath+".branches", where, true)
		errs = append(errs, bErrs...)
		ef.Branches = bs
	}
	if rawPaths, ok := m["paths"]; ok {
		ps, pErrs := validateStringSlice(rawPaths, fieldPath+".paths", where, true)
		errs = append(errs, pErrs...)
		ef.Paths = ps
	}
	if rawIgnore, ok := m["ignore"]; ok {
		ig, iErrs := validateStringSlice(rawIgnore, fieldPath+".ignore", where, false)
		errs = append(errs, iErrs...)
		ef.Ignore = ig
	}

	if _, hasBranches := m["branches"]; !hasBranches {
		if _, hasPaths := m["paths"]; !hasPaths {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+": at least one of branches or paths is required"))
		}
	}

	return ef, errs
}

func validateStringSlice(raw any, fieldPath, where string, nonEmptyList bool) ([]string, Errors) {
	list, ok := raw.([]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a list of strings")}
	}
	if nonEmptyList && len(list) == 0 {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a non-empty list")}
	}
	var out []string
	var errs Errors
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.%d: must be a non-empty string", fieldPath, i)))
			continue
		}
		out = append(out, s)
	}
	return out, errs
}
