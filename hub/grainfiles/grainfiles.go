// Package grainfiles parses, validates, resolves, and evaluates triggers and
// same-repository reference access grants for .task.grain.md and
// .process.grain.md Grainfiles (docs/grainfile-v1.md, docs/grainfiles-spec.md,
// docs/grainfiles.md, docs/hub.md §6, docs/hub-interface.md).
package grainfiles

import (
	"bytes"
	"encoding/json"
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
	// KindTask is a discrete task (<name>.task.grain.md) executed once.
	KindTask Kind = "task"
	// KindProcess is a standing repeatable process (<name>.process.grain.md).
	KindProcess Kind = "process"
	// KindCapability is a named capability definition (<name>.capability.grain.md).
	KindCapability Kind = "capability"
)

// Valid reports whether k is a registered Grainfile kind.
func (k Kind) Valid() bool {
	return k == KindTask || k == KindProcess || k == KindCapability
}

// Registered Grainfile format versions and specification paths (docs/grainfiles-spec.md §6.1).
const (
	VersionV1              = "v1"
	SpecSummaryPathV1      = "docs/grainfile-v1.md"
	SpecPathV1             = "docs/grainfiles-spec.md"
	CommonSchemaPathV1     = "schemas/grainfiles/common.schema.json"
	TaskSchemaPathV1       = "schemas/grainfiles/task.schema.json"
	ProcessSchemaPathV1    = "schemas/grainfiles/process.schema.json"
	CapabilitySchemaPathV1 = "schemas/grainfiles/capability.schema.json"
)

// VersionSpec maps a Grainfile format version identifier (such as "v1") to the
// repository-relative paths of its concise agent summary, normative
// specification, and JSON schemas (docs/grainfiles-spec.md §6.1).
type VersionSpec struct {
	Version              string `json:"version"`
	SpecSummaryPath      string `json:"spec_summary_path"`
	SpecPath             string `json:"spec_path"`
	CommonSchemaPath     string `json:"common_schema_path"`
	TaskSchemaPath       string `json:"task_schema_path"`
	ProcessSchemaPath    string `json:"process_schema_path"`
	CapabilitySchemaPath string `json:"capability_schema_path"`
}

var versionRegistry = map[string]VersionSpec{
	VersionV1: {
		Version:              VersionV1,
		SpecSummaryPath:      SpecSummaryPathV1,
		SpecPath:             SpecPathV1,
		CommonSchemaPath:     CommonSchemaPathV1,
		TaskSchemaPath:       TaskSchemaPathV1,
		ProcessSchemaPath:    ProcessSchemaPathV1,
		CapabilitySchemaPath: CapabilitySchemaPathV1,
	},
}

// LookupVersion looks up a registered Grainfile version specification. An
// empty version string defaults to VersionV1 ("v1").
func LookupVersion(v string) (VersionSpec, bool) {
	if v == "" {
		v = VersionV1
	}
	spec, ok := versionRegistry[v]
	return spec, ok
}

// Registered trigger sources for .process.grain.md files (docs/grainfiles-spec.md §12.2).
const (
	TriggerSchedule    = "schedule"
	TriggerPush        = "push"
	TriggerMerge       = "merge"
	TriggerPullRequest = "pull_request"
	TriggerWorkflow    = "workflow"
	TriggerManual      = "manual"
	TriggerScenario    = "scenario"
	TriggerDependsOn   = "depends_on"
	TriggerWith        = TriggerDependsOn
)

// Resource limits (docs/grainfiles-spec.md §4.1.4, §4.3, §8.4.2, §11.4).
const (
	MaxFileBytes        = 1024 * 1024
	MaxFrontMatterBytes = 64 * 1024
	MaxPathBytes        = 1024
	MaxSegmentBytes     = 255
	MaxDependsOnDepth   = 8
	MaxDependsOnFanOut  = 32
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
	CodeTemplateSyntax  = "GF033"
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

// Grainfile is a parsed and validated .task.grain.md[.jinja2] or .process.grain.md[.jinja2] file.
type Grainfile struct {
	Kind         Kind     `json:"kind"`
	Name         string   `json:"name,omitempty"`
	Path         string   `json:"path,omitempty"`
	IsTemplate   bool     `json:"is_template,omitempty"`
	TemplateVars []string `json:"template_vars,omitempty"`

	// Format version and specification paths (docs/grainfiles-spec.md §6.1).
	Grainfile       string `json:"grainfile"`
	SpecSummaryPath string `json:"spec_summary_path"`
	SpecPath        string `json:"spec_path"`

	// Authoritative source metadata populated by Resolve.
	URL    string `json:"url,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`

	// Front-matter fields (docs/grainfiles-spec.md §6, §7).
	Title                string         `json:"title"`
	Recipient            string         `json:"recipient,omitempty"`
	Requestor            string         `json:"requestor,omitempty"`
	Draft                bool           `json:"draft"`
	NotBefore            time.Time      `json:"not_before,omitempty"`
	NotBeforeRaw         string         `json:"-"`
	CreatedAt            time.Time      `json:"created_at,omitempty"`
	CreatedAtRaw         string         `json:"-"`
	References           []string       `json:"references,omitempty"`
	DependsOn            []string       `json:"depends_on,omitempty"`
	Capabilities         []string       `json:"capabilities,omitempty"`
	ResolvedCapabilities []string       `json:"resolved_capabilities,omitempty"`
	FromTemplate         string         `json:"from_template,omitempty"`
	When                 Triggers       `json:"when,omitempty"`
	On                   Triggers       `json:"on,omitempty"`
	Gate                 bool           `json:"gate,omitempty"`
	Extensions           map[string]any `json:"extensions,omitempty"`

	// Body is the UTF-8 Markdown prompt body after the closing --- line.
	Body string `json:"-"`

	// Warnings holds non-fatal findings such as GF022 (unknown non-x_ fields).
	Warnings []Finding `json:"-"`
}

// ResolvedGrainfile is an alias for a resolved Grainfile.
type ResolvedGrainfile = Grainfile

// Triggers holds the structured trigger rules under `when` in a
// .process.grain.md file (docs/grainfiles-spec.md §7.2.1).
type Triggers struct {
	Schedule    *ScheduleTrigger `json:"schedule,omitempty"`
	Push        *EventFilter     `json:"push,omitempty"`
	Merge       *EventFilter     `json:"merge,omitempty"`
	PullRequest *EventFilter     `json:"pull_request,omitempty"`
	Workflow    []WorkflowFilter `json:"workflow,omitempty"`
	Manual      *ManualTrigger   `json:"manual,omitempty"`
	Scenario    *ScenarioTrigger `json:"scenario,omitempty"`
}

// IsZero reports whether no trigger source is configured.
func (t Triggers) IsZero() bool {
	return t.Schedule == nil &&
		t.Push == nil &&
		t.Merge == nil &&
		t.PullRequest == nil &&
		len(t.Workflow) == 0 &&
		t.Manual == nil &&
		t.Scenario == nil
}

// ScheduleTrigger is `when.schedule` (docs/grainfiles-spec.md §7.2.1). It may
// represent either a single schedule object or a list of schedule objects (via
// Schedules).
type ScheduleTrigger struct {
	Cron      string            `json:"cron"`
	Timezone  string            `json:"timezone,omitempty"`
	Schedules []ScheduleTrigger `json:"-"`
}

type singleScheduleJSON struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
}

// Items returns the list of individual schedule entries in s.
func (s *ScheduleTrigger) Items() []ScheduleTrigger {
	if s == nil {
		return nil
	}
	if len(s.Schedules) > 0 {
		return s.Schedules
	}
	return []ScheduleTrigger{*s}
}

// MarshalJSON serializes a single schedule as a JSON object and a multi-schedule
// list as a JSON array.
func (s ScheduleTrigger) MarshalJSON() ([]byte, error) {
	if len(s.Schedules) > 0 {
		out := make([]singleScheduleJSON, len(s.Schedules))
		for i, item := range s.Schedules {
			out[i] = singleScheduleJSON{
				Cron:     item.Cron,
				Timezone: item.Timezone,
			}
		}
		return json.Marshal(out)
	}
	return json.Marshal(singleScheduleJSON{
		Cron:     s.Cron,
		Timezone: s.Timezone,
	})
}

// UnmarshalJSON accepts either a single schedule object or a non-empty array of
// schedule objects.
func (s *ScheduleTrigger) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []singleScheduleJSON
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		s.Schedules = make([]ScheduleTrigger, len(list))
		for i, item := range list {
			tz := item.Timezone
			if tz == "" {
				tz = "UTC"
			}
			s.Schedules[i] = ScheduleTrigger{
				Cron:     item.Cron,
				Timezone: tz,
			}
		}
		if len(s.Schedules) > 0 {
			s.Cron = s.Schedules[0].Cron
			s.Timezone = s.Schedules[0].Timezone
		}
		return nil
	}
	var single singleScheduleJSON
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	s.Cron = single.Cron
	s.Timezone = single.Timezone
	if s.Timezone == "" && s.Cron != "" {
		s.Timezone = "UTC"
	}
	s.Schedules = nil
	return nil
}

// WorkflowFilter is a single filter rule in `when.workflow`
// (docs/grainfiles-spec.md §7.2.1).
type WorkflowFilter struct {
	Workflow  string   `json:"workflow,omitempty"`
	Workflows []string `json:"workflows,omitempty"`
	Events    []string `json:"events,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Repos     []string `json:"repos,omitempty"`
	Branches  []string `json:"branches,omitempty"`
}

// EventFilter is `when.push`, `when.merge`, or `when.pull_request`
// (docs/grainfiles-spec.md §7.2.1). It may represent either a single filter
// object or a list of filter objects (via Filters).
type EventFilter struct {
	Repo     string        `json:"repo,omitempty"`
	Repos    []string      `json:"repos,omitempty"`
	Branches []string      `json:"branches,omitempty"`
	Paths    []string      `json:"paths,omitempty"`
	Ignore   []string      `json:"ignore,omitempty"`
	Filters  []EventFilter `json:"-"`
}

type singleEventFilterJSON struct {
	Repo     string   `json:"repo,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	Branches []string `json:"branches,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Ignore   []string `json:"ignore,omitempty"`
}

// Rules returns the list of individual filter rules in f.
func (f *EventFilter) Rules() []EventFilter {
	if f == nil {
		return nil
	}
	if len(f.Filters) > 0 {
		return f.Filters
	}
	return []EventFilter{*f}
}

// MarshalJSON serializes a single filter as a JSON object and a multi-filter
// list as a JSON array.
func (f EventFilter) MarshalJSON() ([]byte, error) {
	if len(f.Filters) > 0 {
		out := make([]singleEventFilterJSON, len(f.Filters))
		for i, r := range f.Filters {
			out[i] = singleEventFilterJSON{
				Repo:     r.Repo,
				Repos:    r.Repos,
				Branches: r.Branches,
				Paths:    r.Paths,
				Ignore:   r.Ignore,
			}
		}
		return json.Marshal(out)
	}
	return json.Marshal(singleEventFilterJSON{
		Repo:     f.Repo,
		Repos:    f.Repos,
		Branches: f.Branches,
		Paths:    f.Paths,
		Ignore:   f.Ignore,
	})
}

// UnmarshalJSON accepts either a single filter object or a non-empty array of
// filter objects.
func (f *EventFilter) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []singleEventFilterJSON
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		f.Filters = make([]EventFilter, len(list))
		for i, item := range list {
			f.Filters[i] = EventFilter{
				Repo:     item.Repo,
				Repos:    item.Repos,
				Branches: item.Branches,
				Paths:    item.Paths,
				Ignore:   item.Ignore,
			}
		}
		if len(f.Filters) > 0 {
			f.Repo = f.Filters[0].Repo
			f.Repos = f.Filters[0].Repos
			f.Branches = f.Filters[0].Branches
			f.Paths = f.Filters[0].Paths
			f.Ignore = f.Filters[0].Ignore
		}
		return nil
	}
	var single singleEventFilterJSON
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	f.Repo = single.Repo
	f.Repos = single.Repos
	f.Branches = single.Branches
	f.Paths = single.Paths
	f.Ignore = single.Ignore
	f.Filters = nil
	return nil
}

// ManualTrigger is `when.manual: {}` (docs/grainfiles-spec.md §7.2.1).
type ManualTrigger struct{}

// ScenarioTrigger is `when.scenario` (docs/grainfiles-spec.md §7.2.1).
type ScenarioTrigger struct {
	Description string `json:"description"`
}

var (
	stemRe             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	unknownGrainKindRe = regexp.MustCompile(`^(.+)\.([A-Za-z][A-Za-z0-9-]*)\.grain\.md(?:\.jinja2)?$`)
	repoSlugRe         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)
	userIdentityRe     = regexp.MustCompile(`^[^<>\s][^<>]*\s+<[^<>\s@]+@[^<>\s@]+\.[^<>\s@]+>$`)
	timeRe             = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|\+(?:[01]\d|2[0-3]):[0-5]\d|-(?:[01]\d|2[0-3]):[0-5]\d)$`)
	dateRe             = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	ianaTZRe           = regexp.MustCompile(`^(UTC|Etc/(?:UTC|GMT)|[A-Z][A-Za-z_+-]+(?:/[A-Za-z_+-]+)+)$`)
)

// ClassifyStrictPath classifies a repository-relative path strictly against
// the canonical .task.grain.md[.jinja2] and .process.grain.md[.jinja2] suffixes
// (docs/grainfiles-spec.md §4.1 and §4.2).
//
// If relPath ends with .task.grain.md[.jinja2] or .process.grain.md[.jinja2],
// isGrainfile is true and kind and stem are returned. If relPath is an asset
// Markdown file with an unregistered .<kind>.grain.md[.jinja2] suffix,
// warnCode is "GF005". Ordinary .md files without .grain.md are normal
// Markdown assets and never emit GF005.
func ClassifyStrictPath(relPath string) (kind Kind, stem string, isGrainfile bool, warnCode string) {
	norm := filepath.ToSlash(relPath)
	leaf := norm
	if idx := strings.LastIndexByte(norm, '/'); idx >= 0 {
		leaf = norm[idx+1:]
	}
	for _, k := range []Kind{KindTask, KindProcess, KindCapability} {
		sufJ2 := "." + string(k) + ".grain.md.jinja2"
		if strings.HasSuffix(leaf, sufJ2) {
			return k, leaf[:len(leaf)-len(sufJ2)], true, ""
		}
		suf := "." + string(k) + ".grain.md"
		if strings.HasSuffix(leaf, suf) {
			return k, leaf[:len(leaf)-len(suf)], true, ""
		}
	}
	if unknownGrainKindRe.MatchString(leaf) {
		return "", "", false, CodeUnknownSuffix
	}
	return "", "", false, ""
}

// ClassifyPath classifies a repository-relative path according to
// docs/grainfiles-spec.md §4.1 and §4.2, matching canonical .task.grain.md /
// .process.grain.md / .capability.grain.md files first and also accepting
// .task.md / .process.md suffixes when called directly by API callers.
func ClassifyPath(relPath string) (kind Kind, stem string, isGrainfile bool, warnCode string) {
	if k, s, ok, _ := ClassifyStrictPath(relPath); ok {
		return k, s, true, ""
	}
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
	_, _, _, warn := ClassifyStrictPath(relPath)
	return "", "", false, warn
}

// ValidateStem checks whether stem matches the ASCII Grainfile stem
// grammar `[A-Za-z0-9][A-Za-z0-9._-]*` (docs/grainfiles-spec.md §4.1).
func ValidateStem(stem string) bool {
	return stemRe.MatchString(stem)
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
		return "", "", newFinding(CodeBadName, relPath, "filename must end with .task.grain.md, .process.grain.md, or .capability.grain.md")
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

// ParseTime parses and validates an RFC 3339 timestamp with an explicit timezone
// designator (`Z` or `±HH:MM` with `00 <= HH <= 23`, `00 <= MM <= 59`, `-00:00`
// forbidden) and normalizes it to UTC (`Z`; docs/grainfiles-spec.md §1.4).
func ParseTime(s string) (time.Time, error) {
	if strings.HasSuffix(s, "-00:00") {
		return time.Time{}, fmt.Errorf("invalid timestamp %q: -00:00 unknown local offset is not allowed", s)
	}
	m := timeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid RFC 3339 timestamp %q", s)
	}
	if _, err := time.Parse("2006-01-02T15:04:05", m[1]+"T"+m[2]); err != nil {
		return time.Time{}, fmt.Errorf("invalid calendar date or UTC time %q: %w", s, err)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid RFC 3339 timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// ValidIANATimezone reports whether tz is `"UTC"` or a valid IANA Time Zone
// Database identifier (e.g. `"America/Los_Angeles"`, `"Europe/Berlin"`,
// `"Asia/Tokyo"`), rejecting numeric offsets, unknown names, and non-IANA
// abbreviations (e.g. `"PST"`, `"EST"`, `"CET"`).
func ValidIANATimezone(tz string) bool {
	if tz == "" {
		return false
	}
	if tz != "UTC" && tz != "Etc/UTC" && tz != "Etc/GMT" {
		if strings.HasPrefix(tz, "Etc/") || strings.HasPrefix(tz, "SystemV/") {
			return false
		}
		if !ianaTZRe.MatchString(tz) {
			return false
		}
		for _, seg := range strings.Split(tz, "/") {
			if seg == "" ||
				strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "+") || strings.HasPrefix(seg, "-") ||
				strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, "+") || strings.HasSuffix(seg, "-") {
				return false
			}
		}
	}
	_, err := time.LoadLocation(tz)
	return err == nil
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

// Parse parses and validates a .task.grain.md[.jinja2] or .process.grain.md[.jinja2]
// Grainfile from its path/filename and raw UTF-8 bytes (docs/grainfiles-spec.md §4–§8).
func Parse(path string, data []byte) (*Grainfile, error) {
	return ParseWithVars(path, data, nil)
}

// ParseWithVars parses and validates a Grainfile or .grain.md.jinja2 template
// from its path/filename, raw UTF-8 bytes, and optional template variable bindings.
func ParseWithVars(path string, data []byte, vars map[string]string) (*Grainfile, error) {
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
		return nil, Errors{newFinding(CodeBadName, path, "filename must end with .task.grain.md[.jinja2] or .process.grain.md[.jinja2]")}
	}
	if !ValidateStem(stem) {
		errs = append(errs, newFinding(CodeBadName, path, fmt.Sprintf("bad grainfile name %q", stem)))
	}

	g, parseErrs := parseWithKindAndVars(kind, stem, path, data, vars)
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
	g, errs := parseWithKindAndVars(kind, "", "", data, nil)
	if len(errs) > 0 {
		return g, errs
	}
	return g, nil
}

func parseWithKindAndVars(kind Kind, stem, path string, raw []byte, vars map[string]string) (*Grainfile, Errors) {
	isTmpl := IsTemplatePath(path)
	var templateVars []string
	parseRaw := raw
	if isTmpl {
		rendered, tvars, err := renderTemplateAtPath(path, string(raw), vars)
		if err != nil {
			if errs, ok := err.(Errors); ok {
				return nil, errs
			}
			return nil, Errors{newFinding(CodeTemplateSyntax, path, err.Error())}
		}
		templateVars = tvars
		parseRaw = []byte(rendered)
	}
	fm, body, fErr := splitAndParseFrontMatter(path, parseRaw)
	if fErr != nil {
		return nil, Errors{*fErr}
	}
	g := &Grainfile{
		Kind:         kind,
		Name:         stem,
		Path:         filepath.ToSlash(path),
		IsTemplate:   isTmpl,
		TemplateVars: templateVars,
		Body:         body,
	}
	errs := validateFrontMatter(g, fm, path)
	if isTmpl && vars != nil && g.FromTemplate == "" && strings.TrimSpace(path) != "" {
		cleanP := filepath.ToSlash(strings.TrimSpace(path))
		if strings.HasPrefix(cleanP, "https://") || strings.HasPrefix(cleanP, "file://") {
			g.FromTemplate = cleanP
		} else {
			g.FromTemplate = "/" + strings.TrimPrefix(cleanPosixPath(cleanP), "/")
		}
	}
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
		"grainfile":     true,
		"title":         true,
		"requestor":     true,
		"recipient":     true,
		"draft":         true,
		"not_before":    true,
		"depends_on":    true,
		"capabilities":  true,
		"references":    true,
		"from_template": true,
	}
	knownProcessFields = map[string]bool{
		"grainfile":     true,
		"title":         true,
		"draft":         true,
		"not_before":    true,
		"depends_on":    true,
		"capabilities":  true,
		"references":    true,
		"from_template": true,
		"when":          true,
		"on":            true,
		"gate":          true,
	}
	strictProcessFields = map[string]bool{
		"grainfile":     true,
		"title":         true,
		"draft":         true,
		"not_before":    true,
		"capabilities":  true,
		"references":    true,
		"from_template": true,
		"when":          true,
		"gate":          true,
	}
	knownCapabilityFields = map[string]bool{
		"grainfile":     true,
		"title":         true,
		"draft":         true,
		"not_before":    true,
		"references":    true,
		"from_template": true,
	}
	knownTriggerSources = map[string]bool{
		TriggerSchedule:    true,
		TriggerPush:        true,
		TriggerMerge:       true,
		TriggerPullRequest: true,
		TriggerWorkflow:    true,
		TriggerManual:      true,
		TriggerScenario:    true,
	}
)

func validateFrontMatter(g *Grainfile, fm map[string]any, where string) Errors {
	var errs Errors
	compatProcess := g.Kind == KindProcess &&
		strings.HasSuffix(g.Path, ".process.md") &&
		!strings.HasSuffix(g.Path, ".process.grain.md") &&
		!IsTemplatePath(where)

	known := knownTaskFields
	switch g.Kind {
	case KindProcess:
		if compatProcess {
			known = knownProcessFields
		} else {
			known = strictProcessFields
		}
	case KindCapability:
		known = knownCapabilityFields
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
		if k == "trigger_with" && !compatProcess {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with is not allowed in Grainfile v1 (use depends_on and when instead)"))
			continue
		}
		if g.Kind == KindTask && (k == "when" || k == "on" || k == "gate") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s is only valid on .process.grain.md files", k)))
			continue
		}
		if g.Kind == KindProcess && !compatProcess && (k == "recipient" || k == "requestor" || k == "depends_on") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s is only valid on .task.grain.md files", k)))
			continue
		}
		if g.Kind == KindCapability && (k == "recipient" || k == "requestor" || k == "depends_on" || k == "capabilities" || k == "when" || k == "on" || k == "gate") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s is not valid on .capability.grain.md files", k)))
			continue
		}
		if !known[k] && !(compatProcess && (k == "recipient" || k == "requestor" || k == "trigger_with")) {
			g.Warnings = append(g.Warnings, newFinding(CodeUnknownField, where, fmt.Sprintf("unknown field %q", k)))
		}
	}

	// grainfile (required format version string "v1"; §3.1, §6.1)
	g.Grainfile = VersionV1
	g.SpecSummaryPath = SpecSummaryPathV1
	g.SpecPath = SpecPathV1
	if rawVer, ok := fm["grainfile"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "grainfile: required field missing (expected \"v1\")"))
	} else {
		verStr, ok := rawVer.(string)
		if !ok || verStr == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "grainfile: must be a non-empty string"))
		} else if vSpec, valid := LookupVersion(verStr); !valid {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("grainfile: unsupported version %q (expected %q)", verStr, VersionV1)))
		} else {
			g.Grainfile = vSpec.Version
			g.SpecSummaryPath = vSpec.SpecSummaryPath
			g.SpecPath = vSpec.SpecPath
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

	if g.Kind == KindTask || compatProcess {
		// recipient (optional deployment identity on .task.grain.md, defaults to "/" — the enclosing repository; §6.3, §7.1)
		if rawRec, ok := fm["recipient"]; !ok {
			if g.Kind == KindTask {
				g.Recipient = "/"
			}
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

		// requestor (optional user identity or deployment identity on .task.grain.md; §6.3, §7.1)
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
	}

	// draft (optional bool, default false)
	if rawDraft, ok := fm["draft"]; ok {
		if b, ok := rawDraft.(bool); !ok {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "draft: must be a boolean"))
		} else {
			g.Draft = b
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
		refs, refErrs := validateRefSliceWithSourcePath(rawRefs, "references", where, false, g.Path)
		errs = append(errs, refErrs...)
		g.References = refs
	}

	// from_template (optional template provenance ref; §6.5)
	if rawFT, ok := fm["from_template"]; ok {
		ft, ftErr := validateFromTemplate(rawFT, g.Kind, where, g.Path)
		if ftErr != nil {
			errs = append(errs, *ftErr)
		} else {
			g.FromTemplate = ft
		}
	}

	// depends_on (optional list of refs on .task.grain.md: must target concrete .task.grain.md files only; §8.3)
	if rawDeps, ok := fm["depends_on"]; ok && (g.Kind == KindTask || compatProcess) {
		deps, depErrs := validateDependsOnSlice(rawDeps, g.Kind, where, g.Path)
		errs = append(errs, depErrs...)
		g.DependsOn = deps
	}

	// capabilities (optional list of refs on .task.grain.md and .process.grain.md: must target concrete .capability.grain.md files only; §8.3)
	if rawCaps, ok := fm["capabilities"]; ok && (g.Kind == KindTask || g.Kind == KindProcess) {
		caps, capErrs := validateCapabilitiesSlice(rawCaps, where, g.Path)
		errs = append(errs, capErrs...)
		g.Capabilities = caps
	}

	if g.Kind == KindProcess {
		_, hasCompatTW := fm["trigger_with"]
		if compatProcess && hasCompatTW && len(g.DependsOn) == 0 {
			switch v := fm["trigger_with"].(type) {
			case string:
				if v != "" && !strings.ContainsAny(v, " \t\r\n\\") {
					g.DependsOn = []string{v}
				}
			case []any:
				if tw, twErrs := validateRefSliceWithSourcePath(v, "depends_on", where, true, g.Path); len(twErrs) == 0 {
					g.DependsOn = tw
				}
			}
		}

		// gate (optional bool, default false)
		if rawGate, ok := fm["gate"]; ok {
			if b, ok := rawGate.(bool); !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "gate: must be a boolean"))
			} else {
				g.Gate = b
			}
		}

		rawWhen, hasWhen := fm["when"]
		if !hasWhen && compatProcess {
			rawWhen, hasWhen = fm["on"]
		}
		if !hasWhen && !(compatProcess && hasCompatTW) {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "when: required field missing on .process.grain.md"))
		}
		if hasWhen {
			triggers, whenErrs := validateWhenTriggers(rawWhen, where)
			errs = append(errs, whenErrs...)
			g.When = triggers
			g.On = triggers
		}
	}

	return errs
}

func isTaskTargetPath(p string) bool {
	return strings.HasSuffix(p, ".task.grain.md") || strings.HasSuffix(p, ".task.md")
}

func isProcessTargetPath(p string) bool {
	return strings.HasSuffix(p, ".process.grain.md") || strings.HasSuffix(p, ".process.md")
}

func isCapabilityTargetPath(p string) bool {
	return strings.HasSuffix(p, ".capability.grain.md")
}

func isGrainfileTargetPath(p string) bool {
	return isTaskTargetPath(p) || isProcessTargetPath(p) || isCapabilityTargetPath(p)
}

func validateFromTemplate(raw any, sourceKind Kind, where, sourcePath string) (string, *Finding) {
	s, ok := raw.(string)
	if !ok {
		f := newFinding(CodeSchemaInvalid, where, "from_template: must be a non-empty string")
		return "", &f
	}
	if s == "" || strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\r\n\\") {
		f := newFinding(CodeMalformedRef, where, "from_template: must be a non-empty URL or path without whitespace")
		return "", &f
	}
	r, err := ParseRef(s)
	if err != nil {
		if f, ok := err.(Finding); ok {
			f.Path = where
			f.Message = fmt.Sprintf("from_template: %s", f.Message)
			return "", &f
		}
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("from_template: %v", err))
		return "", &f
	}
	if r.Anchor != "" {
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("from_template: %q may not carry an anchor", s))
		return "", &f
	}
	if r.Mode == "tree" || r.TrailingSlash || strings.HasSuffix(s, "/") {
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("from_template: %q must reference a .%s.grain.md.jinja2 file, not a directory", s, sourceKind))
		return "", &f
	}
	wantSuf := "." + string(sourceKind) + ".grain.md.jinja2"
	if r.Path == "" || !strings.HasSuffix(r.Path, wantSuf) {
		f := newFinding(CodeMalformedRef, where, fmt.Sprintf("from_template: %q on a %s grainfile must end with %s", s, sourceKind, wantSuf))
		return "", &f
	}
	if sourcePath != "" {
		if f := checkLocalClimb(s, "from_template", where, sourcePath); f != nil {
			return "", f
		}
	}
	return s, nil
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
	for _, k := range []Kind{KindTask, KindProcess, KindCapability} {
		for _, suf := range []string{"." + string(k) + ".grain.md", "." + string(k) + ".grain.md.jinja2", "." + string(k) + ".md"} {
			if strings.HasSuffix(r.Path, suf) || strings.HasSuffix(r.Repo, suf) {
				f := newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a %s grainfile, expected a repository or folder", field, s, k))
				return &f
			}
		}
	}
	if f := checkLocalClimb(s, field, where, where); f != nil {
		return f
	}
	return nil
}

func validateCapabilitiesSlice(raw any, where, sourcePath string) ([]string, Errors) {
	list, ok := raw.([]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, "capabilities: must be a list of strings")}
	}
	var out []string
	var errs Errors
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" || strings.ContainsAny(s, " \t\r\n\\") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("capabilities.%d: must be a non-empty reference string", i)))
			continue
		}
		out = append(out, s)
		if f := validateSingleRef(s, "capabilities", where, false, sourcePath); f != nil {
			errs = append(errs, *f)
			continue
		}
		if r, err := ParseRef(s); err == nil {
			if r.Mode == "tree" || r.TrailingSlash || !isCapabilityTargetPath(r.Path) {
				errs = append(errs, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("capabilities: %q is not a concrete .capability.grain.md file", s)))
			}
		}
	}
	return out, errs
}

func validateDependsOnSlice(raw any, sourceKind Kind, where, sourcePath string) ([]string, Errors) {
	list, ok := raw.([]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, "depends_on: must be a list of strings")}
	}
	var out []string
	var errs Errors
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" || strings.ContainsAny(s, " \t\r\n\\") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("depends_on.%d: must be a non-empty reference string", i)))
			continue
		}
		out = append(out, s)
		if f := validateSingleRef(s, "depends_on", where, true, sourcePath); f != nil {
			errs = append(errs, *f)
			continue
		}
		if sourceKind == KindTask {
			if r, err := ParseRef(s); err == nil {
				strictTaskSource := strings.HasSuffix(sourcePath, ".task.grain.md") || strings.HasSuffix(sourcePath, ".task.grain.md.jinja2")
				if !isGrainfileTargetPath(r.Path) ||
					strings.HasSuffix(r.Path, ".jinja2") ||
					strings.HasSuffix(r.Path, ".process.grain.md") ||
					strings.HasSuffix(r.Path, ".capability.grain.md") ||
					(strictTaskSource && !strings.HasSuffix(r.Path, ".task.grain.md")) {
					errs = append(errs, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("depends_on: %q is not a concrete .task.grain.md file", s)))
				}
			}
		}
	}
	return out, errs
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
		if !isGrainfileTargetPath(r.Path) {
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
	return validateWhenTriggers(raw, where)
}

func validateWhenTriggers(raw any, where string) (Triggers, Errors) {
	var t Triggers
	var errs Errors

	m, ok := raw.(map[string]any)
	if !ok {
		return t, Errors{newFinding(CodeSchemaInvalid, where, "when: must be a mapping")}
	}
	if len(m) == 0 {
		return t, Errors{newFinding(CodeSchemaInvalid, where, "when: must have at least one trigger source")}
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		val := m[k]
		if !knownTriggerSources[k] {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("unknown trigger source %q in when", k)))
			continue
		}
		switch k {
		case TriggerSchedule:
			sch, schErrs := validateScheduleTrigger(val, "when.schedule", where)
			errs = append(errs, schErrs...)
			if len(schErrs) == 0 {
				t.Schedule = sch
			}

		case TriggerPush, TriggerMerge, TriggerPullRequest:
			ef, efErrs := validateEventFilter(val, "when."+k, where)
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

		case TriggerWorkflow:
			wfs, wfErrs := validateWorkflowTriggers(val, "when.workflow", where)
			errs = append(errs, wfErrs...)
			if len(wfErrs) == 0 {
				t.Workflow = wfs
			}

		case TriggerManual:
			mm, ok := val.(map[string]any)
			if !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.manual: must be an empty mapping {}"))
				continue
			}
			if len(mm) > 0 {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.manual: does not accept properties"))
				continue
			}
			t.Manual = &ManualTrigger{}

		case TriggerScenario:
			switch sv := val.(type) {
			case string:
				if sv == "" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.scenario: must be a non-empty string"))
				} else {
					t.Scenario = &ScenarioTrigger{Description: sv}
				}
			case map[string]any:
				for sk := range sv {
					if sk != "description" {
						errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("when.scenario: unknown property %q", sk)))
					}
				}
				rawDesc, hasDesc := sv["description"]
				if !hasDesc {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.scenario: description is required"))
				} else if desc, ok := rawDesc.(string); !ok || desc == "" {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.scenario.description: must be a non-empty string"))
				} else {
					t.Scenario = &ScenarioTrigger{Description: desc}
				}
			default:
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "when.scenario: must be a string or {description} mapping"))
			}
		}
	}

	return t, errs
}

// NormalizeRepoSlugOrURL validates and normalizes either an https:// repository
// URL ("https://github.com/owner/repo") or an "owner/repo" slug into its
// canonical lower-case repository URL ("https://github.com/owner/repo").
func NormalizeRepoSlugOrURL(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", fmt.Errorf("invalid repository %q", raw)
	}
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") || strings.HasPrefix(s, "http:") || strings.HasPrefix(s, "https:") || strings.HasPrefix(s, "ssh:") || strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "file:") {
		r, err := ParseRef(s)
		if err != nil {
			return "", err
		}
		if r.Form != RefRemote || r.Path != "" || r.GitRef != "" || r.Mode != "" || r.Anchor != "" {
			return "", fmt.Errorf("repository %q must be a repository root URL without branch/path/anchor", raw)
		}
		return r.Repo, nil
	}
	cleaned := strings.TrimSuffix(s, ".git")
	if !repoSlugRe.MatchString(cleaned) {
		return "", fmt.Errorf("repository %q must be an https:// URL or owner/repo slug", raw)
	}
	owner, repo, _ := strings.Cut(cleaned, "/")
	if owner == "." || owner == ".." || repo == "" || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid repository slug %q", raw)
	}
	return "https://github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(repo), nil
}

func validateAndNormalizeWhenPath(raw, fieldPath, where string, allowURL bool) (string, *Finding) {
	if raw == "" || strings.ContainsAny(raw, "\\\x00") || hasEncodedPathHazard(raw) {
		f := newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: invalid path %q", fieldPath, raw))
		return "", &f
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "http:") || strings.HasPrefix(raw, "https:") || strings.HasPrefix(raw, "ssh:") || strings.HasPrefix(raw, "git@") || strings.HasPrefix(raw, "file:") {
		if !allowURL {
			f := newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: URL not allowed in %q", fieldPath, raw))
			return "", &f
		}
		r, err := ParseRef(raw)
		if err != nil || r.Form != RefRemote || r.Anchor != "" {
			f := newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: invalid forge URL %q", fieldPath, raw))
			return "", &f
		}
		return r.CanonicalURL(), nil
	}
	trimmed := strings.TrimPrefix(raw, "/")
	for _, seg := range strings.Split(strings.TrimSuffix(trimmed, "/"), "/") {
		if seg == ".." {
			f := newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: '..' segment not allowed in %q", fieldPath, raw))
			return "", &f
		}
	}
	if trimmed == "" {
		return "/", nil
	}
	return trimmed, nil
}

func validateEventFilter(raw any, fieldPath, where string) (*EventFilter, Errors) {
	if list, isList := raw.([]any); isList {
		if len(list) == 0 {
			return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a non-empty list of filter mappings")}
		}
		var errs Errors
		var filters []EventFilter
		for i, item := range list {
			itemPath := fmt.Sprintf("%s.%d", fieldPath, i)
			single, sErrs := validateSingleEventFilter(item, itemPath, where)
			errs = append(errs, sErrs...)
			if single != nil {
				filters = append(filters, *single)
			}
		}
		if len(errs) > 0 || len(filters) == 0 {
			return nil, errs
		}
		out := &EventFilter{
			Repo:     filters[0].Repo,
			Repos:    filters[0].Repos,
			Branches: filters[0].Branches,
			Paths:    filters[0].Paths,
			Ignore:   filters[0].Ignore,
			Filters:  filters,
		}
		return out, nil
	}
	return validateSingleEventFilter(raw, fieldPath, where)
}

func validateSingleEventFilter(raw any, fieldPath, where string) (*EventFilter, Errors) {
	var errs Errors
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a mapping")}
	}
	for k := range m {
		if k != "repo" && k != "repos" && k != "branches" && k != "paths" && k != "ignore" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: unknown property %q", fieldPath, k)))
		}
	}

	ef := &EventFilter{}
	if rawRepo, ok := m["repo"]; ok {
		s, ok := rawRepo.(string)
		if !ok || s == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+".repo: must be a non-empty string"))
		} else if normRepo, err := NormalizeRepoSlugOrURL(s); err != nil {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.repo: %v", fieldPath, err)))
		} else {
			ef.Repo = normRepo
		}
	}
	if rawRepos, ok := m["repos"]; ok {
		rs, rErrs := validateStringSlice(rawRepos, fieldPath+".repos", where, true)
		errs = append(errs, rErrs...)
		for i, s := range rs {
			normRepo, err := NormalizeRepoSlugOrURL(s)
			if err != nil {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.repos.%d: %v", fieldPath, i, err)))
			} else {
				ef.Repos = append(ef.Repos, normRepo)
			}
		}
	}
	if rawBranches, ok := m["branches"]; ok {
		bs, bErrs := validateStringSlice(rawBranches, fieldPath+".branches", where, true)
		errs = append(errs, bErrs...)
		ef.Branches = bs
	}
	if rawPaths, ok := m["paths"]; ok {
		ps, pErrs := validateStringSlice(rawPaths, fieldPath+".paths", where, true)
		errs = append(errs, pErrs...)
		for i, p := range ps {
			normP, f := validateAndNormalizeWhenPath(p, fmt.Sprintf("%s.paths.%d", fieldPath, i), where, true)
			if f != nil {
				errs = append(errs, *f)
			} else {
				ef.Paths = append(ef.Paths, normP)
			}
		}
	}
	if rawIgnore, ok := m["ignore"]; ok {
		ig, iErrs := validateStringSlice(rawIgnore, fieldPath+".ignore", where, false)
		errs = append(errs, iErrs...)
		for i, p := range ig {
			normP, f := validateAndNormalizeWhenPath(p, fmt.Sprintf("%s.ignore.%d", fieldPath, i), where, false)
			if f != nil {
				errs = append(errs, *f)
			} else {
				ef.Ignore = append(ef.Ignore, normP)
			}
		}
	}

	_, hasRepo := m["repo"]
	_, hasRepos := m["repos"]
	_, hasBranches := m["branches"]
	_, hasPaths := m["paths"]
	if !hasRepo && !hasRepos && !hasBranches && !hasPaths {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+": at least one of repo, repos, branches, or paths is required"))
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

func validateScheduleTrigger(raw any, fieldPath, where string) (*ScheduleTrigger, Errors) {
	if list, isList := raw.([]any); isList {
		if len(list) == 0 {
			return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a non-empty list of schedule mappings")}
		}
		var errs Errors
		var items []ScheduleTrigger
		for i, item := range list {
			itemPath := fmt.Sprintf("%s.%d", fieldPath, i)
			single, sErrs := validateSingleScheduleTrigger(item, itemPath, where)
			errs = append(errs, sErrs...)
			if single != nil {
				items = append(items, *single)
			}
		}
		if len(errs) > 0 || len(items) == 0 {
			return nil, errs
		}
		return &ScheduleTrigger{
			Cron:      items[0].Cron,
			Timezone:  items[0].Timezone,
			Schedules: items,
		}, nil
	}
	return validateSingleScheduleTrigger(raw, fieldPath, where)
}

func validateSingleScheduleTrigger(raw any, fieldPath, where string) (*ScheduleTrigger, Errors) {
	var errs Errors
	sm, ok := raw.(map[string]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a mapping")}
	}
	for sk := range sm {
		if sk != "cron" && sk != "timezone" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: unknown property %q", fieldPath, sk)))
		}
	}
	var cronStr string
	rawCron, hasCron := sm["cron"]
	if !hasCron {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+": cron is required"))
	} else if cs, ok := rawCron.(string); !ok || cs == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+".cron: must be a non-empty string"))
	} else if len(strings.Fields(cs)) != 5 {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "schedule.cron must be a five-field cron expression"))
	} else {
		cronStr = cs
	}

	tz := "UTC"
	if rawTZ, hasTZ := sm["timezone"]; hasTZ {
		tzStr, ok := rawTZ.(string)
		if !ok || tzStr == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+".timezone: must be a non-empty string"))
		} else if !ValidIANATimezone(tzStr) {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.timezone: invalid IANA timezone %q", fieldPath, tzStr)))
		} else {
			tz = tzStr
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &ScheduleTrigger{
		Cron:     cronStr,
		Timezone: tz,
	}, nil
}

// NormalizeWorkflowEvent normalizes a workflow event or alias to one of the
// canonical lifecycle transition names: "triggered", "succeeded", or "failed".
func NormalizeWorkflowEvent(ev string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ev)) {
	case "triggered", "started", "requested", "in_progress":
		return "triggered", true
	case "succeeded", "success":
		return "succeeded", true
	case "failed", "failure", "timed_out", "startup_failure":
		return "failed", true
	default:
		return "", false
	}
}

func normalizeDeclaredWorkflowEvent(ev string) (string, bool) {
	switch ev {
	case "triggered", "started":
		return "triggered", true
	case "succeeded", "success":
		return "succeeded", true
	case "failed", "failure":
		return "failed", true
	default:
		return "", false
	}
}

func validateWorkflowTriggers(raw any, fieldPath, where string) ([]WorkflowFilter, Errors) {
	if list, isList := raw.([]any); isList {
		if len(list) == 0 {
			return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a non-empty list of workflow filter mappings")}
		}
		var errs Errors
		var filters []WorkflowFilter
		for i, item := range list {
			itemPath := fmt.Sprintf("%s.%d", fieldPath, i)
			single, sErrs := validateSingleWorkflowFilter(item, itemPath, where)
			errs = append(errs, sErrs...)
			if single != nil {
				filters = append(filters, *single)
			}
		}
		if len(errs) > 0 || len(filters) == 0 {
			return nil, errs
		}
		return filters, nil
	}
	single, errs := validateSingleWorkflowFilter(raw, fieldPath, where)
	if len(errs) > 0 || single == nil {
		return nil, errs
	}
	return []WorkflowFilter{*single}, nil
}

func validateSingleWorkflowFilter(raw any, fieldPath, where string) (*WorkflowFilter, Errors) {
	var errs Errors
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, Errors{newFinding(CodeSchemaInvalid, where, fieldPath+": must be a mapping")}
	}
	for k := range m {
		if k != "workflow" && k != "workflows" && k != "events" && k != "repo" && k != "repos" && k != "branches" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s: unknown property %q", fieldPath, k)))
		}
	}

	wf := &WorkflowFilter{}
	if rawWF, ok := m["workflow"]; ok {
		s, ok := rawWF.(string)
		if !ok || strings.TrimSpace(s) == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+".workflow: must be a non-empty string"))
		} else {
			wf.Workflow = strings.TrimSpace(s)
		}
	}
	if rawWFs, ok := m["workflows"]; ok {
		ws, wErrs := validateStringSlice(rawWFs, fieldPath+".workflows", where, true)
		errs = append(errs, wErrs...)
		for i, w := range ws {
			if strings.TrimSpace(w) == "" {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.workflows.%d: must be a non-empty string", fieldPath, i)))
			} else {
				wf.Workflows = append(wf.Workflows, strings.TrimSpace(w))
			}
		}
	}
	if rawEvents, ok := m["events"]; ok {
		es, eErrs := validateStringSlice(rawEvents, fieldPath+".events", where, true)
		errs = append(errs, eErrs...)
		for i, ev := range es {
			canon, valid := normalizeDeclaredWorkflowEvent(ev)
			if !valid {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.events.%d: invalid workflow event %q (expected triggered, succeeded, or failed)", fieldPath, i, ev)))
			} else {
				wf.Events = append(wf.Events, canon)
			}
		}
	} else {
		wf.Events = []string{"succeeded", "failed"}
	}
	if rawRepo, ok := m["repo"]; ok {
		s, ok := rawRepo.(string)
		if !ok || s == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+".repo: must be a non-empty string"))
		} else if normRepo, err := NormalizeRepoSlugOrURL(s); err != nil {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.repo: %v", fieldPath, err)))
		} else {
			wf.Repo = normRepo
		}
	}
	if rawRepos, ok := m["repos"]; ok {
		rs, rErrs := validateStringSlice(rawRepos, fieldPath+".repos", where, true)
		errs = append(errs, rErrs...)
		for i, s := range rs {
			normRepo, err := NormalizeRepoSlugOrURL(s)
			if err != nil {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.repos.%d: %v", fieldPath, i, err)))
			} else {
				wf.Repos = append(wf.Repos, normRepo)
			}
		}
	}
	if rawBranches, ok := m["branches"]; ok {
		bs, bErrs := validateStringSlice(rawBranches, fieldPath+".branches", where, true)
		errs = append(errs, bErrs...)
		for i, br := range bs {
			if strings.TrimSpace(br) == "" {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.branches.%d: must be a non-empty string", fieldPath, i)))
			} else {
				wf.Branches = append(wf.Branches, strings.TrimSpace(br))
			}
		}
	}

	_, hasWF := m["workflow"]
	_, hasWFs := m["workflows"]
	if !hasWF && !hasWFs {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, fieldPath+": at least one of workflow or workflows is required"))
	}

	return wf, errs
}

// MaterializedReference is a fetched reference file tagged with its source URL
// and immutable git commit SHA for structural delimiting from a Grainfile's
// Markdown prompt body (docs/grainfiles-spec.md §11.5).
type MaterializedReference struct {
	URL       string `json:"url"`
	CommitSHA string `json:"commit_sha"`
	Content   string `json:"content"`
}

// MaterializedPRDiff is an untrusted pull-request diff tagged with its source
// repository URL, base commit SHA, and head commit SHA (docs/grainfiles-spec.md §11.5).
type MaterializedPRDiff struct {
	RepoURL       string `json:"repo_url"`
	BaseCommitSHA string `json:"base_commit_sha"`
	HeadCommitSHA string `json:"head_commit_sha"`
	Diff          string `json:"diff"`
}

func sanitizeDelimitAttr(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
}

func escapeClosingDelimiters(s string) string {
	r := strings.NewReplacer(
		"</grainfile_prompt", "<\\/grainfile_prompt",
		"</grainfile_reference", "<\\/grainfile_reference",
		"</pull_request_diff", "<\\/pull_request_diff",
	)
	return r.Replace(s)
}

// FormatExecutionPrompt structurally delimits the trusted Grainfile Markdown
// prompt body from untrusted materialized references and pull-request diffs,
// tagging each context block with its source URL and commit SHA so embedded
// closing tags cannot escape the untrusted data block (docs/grainfiles-spec.md §11.5).
func FormatExecutionPrompt(g *Grainfile, refs []MaterializedReference, prDiff *MaterializedPRDiff) string {
	var b strings.Builder
	urlAttr := ""
	commitAttr := ""
	if g != nil {
		urlAttr = sanitizeDelimitAttr(g.URL)
		commitAttr = sanitizeDelimitAttr(g.Commit)
	}
	fmt.Fprintf(&b, "<grainfile_prompt url=%q commit=%q>\n", urlAttr, commitAttr)
	if g != nil {
		body := strings.TrimRight(g.Body, "\r\n")
		if body != "" {
			b.WriteString(body)
			b.WriteByte('\n')
		}
	}
	b.WriteString("</grainfile_prompt>\n")

	for _, ref := range refs {
		fmt.Fprintf(&b, "<grainfile_reference url=%q commit=%q trust=\"untrusted_data\">\n",
			sanitizeDelimitAttr(ref.URL), sanitizeDelimitAttr(ref.CommitSHA))
		content := escapeClosingDelimiters(strings.TrimRight(ref.Content, "\r\n"))
		if content != "" {
			b.WriteString(content)
			b.WriteByte('\n')
		}
		b.WriteString("</grainfile_reference>\n")
	}

	if prDiff != nil {
		fmt.Fprintf(&b, "<pull_request_diff repo=%q base_commit=%q head_commit=%q trust=\"untrusted_data\">\n",
			sanitizeDelimitAttr(prDiff.RepoURL),
			sanitizeDelimitAttr(prDiff.BaseCommitSHA),
			sanitizeDelimitAttr(prDiff.HeadCommitSHA))
		diff := escapeClosingDelimiters(strings.TrimRight(prDiff.Diff, "\r\n"))
		if diff != "" {
			b.WriteString(diff)
			b.WriteByte('\n')
		}
		b.WriteString("</pull_request_diff>\n")
	}

	return b.String()
}
