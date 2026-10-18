package grainfiles

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ForgeEvent describes a forge webhook event (`push`, `pull_request`, or
// `merge`) evaluated against a `.process.md` file's `on` trigger rules
// (docs/grainfiles-spec.md §7.2.1, §9; docs/hub.md §6.4).
type ForgeEvent struct {
	// Source is the trigger source: TriggerPush ("push"),
	// TriggerPullRequest ("pull_request"), or TriggerMerge ("merge").
	Source string
	// Branch is the target branch name or ref (e.g. "main" or "refs/heads/main").
	Branch string
	// ChangedPaths is the list of repository-relative file paths touched by
	// the push, pull request, or merge.
	ChangedPaths []string
	// At is the UTC timestamp at which the event is evaluated against
	// `not_before`. If zero, `time.Now().UTC()` is used.
	At time.Time
}

// ActiveAt reports whether g is eligible to execute or fire at the given instant
// according to `draft` and `not_before` (docs/grainfiles-spec.md §6.3, §6.4, §9):
//   - If `g.Draft` is true, ActiveAt always returns false.
//   - If `g.NotBefore` is set and `at` is before `g.NotBefore`, ActiveAt returns false.
//   - Otherwise ActiveAt returns true.
//
// If `at` is zero, `time.Now().UTC()` is used.
func (g *Grainfile) ActiveAt(at time.Time) bool {
	if g == nil || g.Draft {
		return false
	}
	if !g.NotBefore.IsZero() {
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if at.Before(g.NotBefore) {
			return false
		}
	}
	return true
}

// MatchesEvent reports whether a `.process.md` Grainfile g fires on ev,
// respecting `draft: true`, `not_before`, and the `branches`, `paths`, and
// `ignore` glob rules for `ev.Source` (`push`, `pull_request`, or `merge`).
func (g *Grainfile) MatchesEvent(ev ForgeEvent) bool {
	return g.MatchesTrigger(ev.Source, ev.Branch, ev.ChangedPaths, ev.At)
}

// MatchesTrigger reports whether a `.process.md` Grainfile g fires for the
// given forge event source (`push`, `pull_request`, or `merge`), target branch,
// changed repository-relative file paths, and evaluation time `at`.
func (g *Grainfile) MatchesTrigger(source, branch string, changedPaths []string, at time.Time) bool {
	if g == nil || g.Kind != KindProcess {
		return false
	}
	if !g.ActiveAt(at) {
		return false
	}
	triggers := g.When
	if triggers.IsZero() {
		triggers = g.On
	}
	var filter *EventFilter
	switch source {
	case TriggerPush:
		filter = triggers.Push
	case TriggerPullRequest:
		filter = triggers.PullRequest
	case TriggerMerge:
		filter = triggers.Merge
	default:
		return false
	}
	if filter == nil {
		return false
	}
	return filter.Matches(branch, changedPaths)
}

// Matches reports whether branch and changedPaths satisfy the EventFilter's
// `branches`, `paths`, and `ignore` glob rules (docs/grainfiles-spec.md §7.2.1).
func (f *EventFilter) Matches(branch string, changedPaths []string) bool {
	if f == nil {
		return false
	}
	if len(f.Branches) == 0 && len(f.Paths) == 0 {
		return false
	}

	normBranch := normalizeBranchName(branch)
	if len(f.Branches) > 0 {
		if normBranch == "" {
			return false
		}
		branchMatched := false
		for _, pat := range f.Branches {
			if MatchGlob(normalizeBranchName(pat), normBranch) {
				branchMatched = true
				break
			}
		}
		if !branchMatched {
			return false
		}
	}

	// When `paths` globs are specified, at least one changed file must match a
	// pattern in `paths` and NOT match any pattern in `ignore`.
	if len(f.Paths) > 0 {
		for _, rawPath := range changedPaths {
			p := normalizeRepoRelPath(rawPath)
			if p == "" {
				continue
			}
			if matchesAnyGlob(f.Paths, p) && !matchesAnyGlob(f.Ignore, p) {
				return true
			}
		}
		return false
	}

	// Only `branches` (and optionally `ignore`) was specified.
	if len(f.Ignore) > 0 && len(changedPaths) > 0 {
		for _, rawPath := range changedPaths {
			p := normalizeRepoRelPath(rawPath)
			if p == "" {
				continue
			}
			if !matchesAnyGlob(f.Ignore, p) {
				return true
			}
		}
		return false
	}

	return true
}

func normalizeBranchName(b string) string {
	b = strings.TrimSpace(b)
	b = strings.TrimPrefix(b, "refs/heads/")
	return b
}

func normalizeRepoRelPath(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		return ""
	}
	cleaned := cleanPosixPath(p)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func matchesAnyGlob(patterns []string, value string) bool {
	for _, pat := range patterns {
		if MatchGlob(pat, value) {
			return true
		}
	}
	return false
}

// MatchGlob reports whether name matches the glob pattern, supporting `*`
// (any sequence of non-`/` characters), `**` (zero or more directories/path
// segments across `/`), `?` (any single non-`/` character), and `[...]`
// character classes.
func MatchGlob(pattern, name string) bool {
	pat := normalizeRepoRelPath(pattern)
	val := normalizeRepoRelPath(name)
	if pat == "" {
		return val == ""
	}
	reStr := globToRegex(pat)
	re, err := regexp.Compile(reStr)
	if err != nil {
		return pat == val
	}
	return re.MatchString(val)
}

func globToRegex(pat string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pat); {
		if strings.HasPrefix(pat[i:], "**/") {
			b.WriteString("(?:.+/)?")
			i += 3
			continue
		}
		if strings.HasPrefix(pat[i:], "/**/") {
			b.WriteString("(?:/|/.+/)")
			i += 4
			continue
		}
		if pat[i:] == "/**" {
			b.WriteString("/.+")
			i += 3
			continue
		}
		if strings.HasPrefix(pat[i:], "**") {
			b.WriteString(".*")
			i += 2
			continue
		}
		switch pat[i] {
		case '*':
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '[':
			end := strings.IndexByte(pat[i+1:], ']')
			if end >= 0 {
				cls := pat[i+1 : i+1+end]
				if strings.HasPrefix(cls, "!") {
					cls = "^" + cls[1:]
				}
				b.WriteByte('[')
				b.WriteString(cls)
				b.WriteByte(']')
				i += end + 2
			} else {
				b.WriteString(`\[`)
				i++
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(pat[i])))
			i++
		}
	}
	b.WriteByte('$')
	return b.String()
}
