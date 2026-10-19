package grainfiles

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ForgeEvent describes a forge webhook event (`push`, `pull_request`, or
// `merge`) evaluated against a `.process.grain.md` file's `when` trigger rules
// (docs/grainfiles-spec.md §7.2.1, §9; docs/hub.md §6.4).
type ForgeEvent struct {
	// Source is the trigger source: TriggerPush ("push"),
	// TriggerPullRequest ("pull_request"), or TriggerMerge ("merge").
	Source string
	// Repo is the repository URL or `owner/repo` slug where the webhook event
	// occurred. When empty, it defaults to the repository containing the
	// `.process.grain.md`.
	Repo string
	// Branch is the target branch name or ref (e.g. "main" or "refs/heads/main").
	Branch string
	// CommitSHA is the commit SHA of the push, PR head/base, or merge commit.
	CommitSHA string
	// PRNumber is the pull request number for `pull_request` or `merge` events.
	PRNumber int
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

// MatchesEvent reports whether a `.process.grain.md` Grainfile g fires on ev,
// respecting `draft: true`, `not_before`, and the `repo`, `repos`, `branches`,
// `paths`, and `ignore` filter rules for `ev.Source` (`push`, `pull_request`,
// or `merge`).
func (g *Grainfile) MatchesEvent(ev ForgeEvent) bool {
	ok, _ := g.MatchedEventPaths(ev)
	return ok
}

// MatchedEventPaths reports whether a `.process.grain.md` Grainfile g fires on
// ev and returns the normalized repository-relative file paths from
// `ev.ChangedPaths` that matched the trigger filter.
func (g *Grainfile) MatchedEventPaths(ev ForgeEvent) (bool, []string) {
	if g == nil || g.Kind != KindProcess {
		return false, nil
	}
	if !g.ActiveAt(ev.At) {
		return false, nil
	}
	triggers := g.When
	if triggers.IsZero() {
		triggers = g.On
	}
	var filter *EventFilter
	switch ev.Source {
	case TriggerPush:
		filter = triggers.Push
	case TriggerPullRequest:
		filter = triggers.PullRequest
	case TriggerMerge:
		filter = triggers.Merge
	default:
		return false, nil
	}
	if filter == nil {
		return false, nil
	}
	return filter.MatchedPathsForRepo(g.Repo, ev.Repo, ev.Branch, ev.ChangedPaths)
}

// MatchesTrigger reports whether a `.process.grain.md` Grainfile g fires for the
// given forge event source (`push`, `pull_request`, or `merge`), target branch,
// changed repository-relative file paths, and evaluation time `at`.
func (g *Grainfile) MatchesTrigger(source, branch string, changedPaths []string, at time.Time) bool {
	return g.MatchesEvent(ForgeEvent{
		Source:       source,
		Branch:       branch,
		ChangedPaths: changedPaths,
		At:           at,
	})
}

// Matches reports whether branch and changedPaths satisfy the EventFilter's
// `branches`, `paths`, and `ignore` rules (docs/grainfiles-spec.md §7.2.1).
func (f *EventFilter) Matches(branch string, changedPaths []string) bool {
	ok, _ := f.MatchedPathsForRepo("", "", branch, changedPaths)
	return ok
}

// MatchesRepo reports whether eventRepo, branch, and changedPaths satisfy the
// EventFilter when evaluated for a `.process.grain.md` residing in defaultRepo.
func (f *EventFilter) MatchesRepo(defaultRepo, eventRepo, branch string, changedPaths []string) bool {
	ok, _ := f.MatchedPathsForRepo(defaultRepo, eventRepo, branch, changedPaths)
	return ok
}

// MatchedPathsForRepo evaluates all filter rules in f against (defaultRepo,
// eventRepo, branch, changedPaths) and returns whether any rule matched along
// with the deduplicated list of matching changed file paths.
func (f *EventFilter) MatchedPathsForRepo(defaultRepo, eventRepo, branch string, changedPaths []string) (bool, []string) {
	if f == nil {
		return false, nil
	}
	rules := f.Rules()
	if len(rules) == 0 {
		return false, nil
	}
	anyRuleMatched := false
	var allMatched []string
	for _, rule := range rules {
		ok, paths := matchSingleFilterRule(rule, defaultRepo, eventRepo, branch, changedPaths)
		if ok {
			anyRuleMatched = true
			for _, p := range paths {
				if !slices.Contains(allMatched, p) {
					allMatched = append(allMatched, p)
				}
			}
		}
	}
	if !anyRuleMatched {
		return false, nil
	}
	return true, allMatched
}

func matchSingleFilterRule(rule EventFilter, defaultRepo, eventRepo, branch string, changedPaths []string) (bool, []string) {
	if rule.Repo == "" && len(rule.Repos) == 0 && len(rule.Branches) == 0 && len(rule.Paths) == 0 {
		return false, nil
	}

	var canonDefault, canonEvent string
	if strings.TrimSpace(defaultRepo) != "" {
		if c, err := NormalizeRepoSlugOrURL(defaultRepo); err == nil {
			canonDefault = c
		} else {
			canonDefault = strings.TrimSpace(defaultRepo)
		}
	}
	if strings.TrimSpace(eventRepo) != "" {
		if c, err := NormalizeRepoSlugOrURL(eventRepo); err == nil {
			canonEvent = c
		} else {
			canonEvent = strings.TrimSpace(eventRepo)
		}
	}

	var explicitRepos []string
	if strings.TrimSpace(rule.Repo) != "" {
		explicitRepos = append(explicitRepos, rule.Repo)
	}
	explicitRepos = append(explicitRepos, rule.Repos...)

	if len(explicitRepos) > 0 && canonEvent != "" {
		repoMatched := false
		for _, r := range explicitRepos {
			normR, err := NormalizeRepoSlugOrURL(r)
			if err != nil {
				normR = strings.TrimSpace(r)
			}
			if IsSameRepo(normR, canonEvent) {
				repoMatched = true
				break
			}
		}
		if !repoMatched {
			return false, nil
		}
	}

	normBranch := normalizeBranchName(branch)
	if len(rule.Branches) > 0 {
		if normBranch == "" {
			return false, nil
		}
		branchMatched := false
		for _, pat := range rule.Branches {
			if MatchGlob(normalizeBranchName(pat), normBranch) {
				branchMatched = true
				break
			}
		}
		if !branchMatched {
			return false, nil
		}
	}

	// When `paths` entries are specified, at least one changed file must match a
	// pattern in `paths` (including any embedded repo/branch in a full URL
	// pattern) and NOT match any pattern in `ignore`.
	if len(rule.Paths) > 0 {
		var matched []string
		for _, rawPath := range changedPaths {
			p := normalizeRepoRelPath(rawPath)
			if p == "" {
				continue
			}
			if matchesAnyPathPattern(rule.Ignore, p) {
				continue
			}
			if matchesAnyRulePath(rule.Paths, len(explicitRepos) > 0, canonDefault, canonEvent, normBranch, p) {
				if !slices.Contains(matched, p) {
					matched = append(matched, p)
				}
			}
		}
		if len(matched) == 0 {
			return false, nil
		}
		return true, matched
	}

	// No `paths` specified: if no explicit `repo`/`repos` was set, the rule
	// watches `defaultRepo`.
	if len(explicitRepos) == 0 && canonEvent != "" && canonDefault != "" && !IsSameRepo(canonDefault, canonEvent) {
		return false, nil
	}

	if len(rule.Ignore) > 0 && len(changedPaths) > 0 {
		var matched []string
		for _, rawPath := range changedPaths {
			p := normalizeRepoRelPath(rawPath)
			if p == "" {
				continue
			}
			if !matchesAnyPathPattern(rule.Ignore, p) {
				if !slices.Contains(matched, p) {
					matched = append(matched, p)
				}
			}
		}
		if len(matched) == 0 {
			return false, nil
		}
		return true, matched
	}

	var matched []string
	for _, rawPath := range changedPaths {
		if p := normalizeRepoRelPath(rawPath); p != "" && !slices.Contains(matched, p) {
			matched = append(matched, p)
		}
	}
	return true, matched
}

func matchesAnyRulePath(patterns []string, hasExplicitRepo bool, canonDefault, canonEvent, normBranch, filePath string) bool {
	for _, pat := range patterns {
		trimmed := strings.TrimSpace(pat)
		if strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "file://") {
			r, err := ParseRef(trimmed)
			if err != nil {
				continue
			}
			if canonEvent != "" && !IsSameRepo(r.Repo, canonEvent) {
				continue
			}
			if r.GitRef != "" && normBranch != "" && !MatchGlob(normalizeBranchName(r.GitRef), normBranch) {
				continue
			}
			subPath := strings.TrimPrefix(r.Path, "/")
			if subPath == "" {
				return true
			}
			if r.Mode == "tree" || r.TrailingSlash {
				if MatchPathPattern(strings.TrimSuffix(subPath, "/")+"/", filePath) {
					return true
				}
				continue
			}
			if MatchPathPattern(subPath, filePath) {
				return true
			}
			continue
		}
		if !hasExplicitRepo && canonEvent != "" && canonDefault != "" && !IsSameRepo(canonDefault, canonEvent) {
			continue
		}
		if MatchPathPattern(trimmed, filePath) {
			return true
		}
	}
	return false
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

func matchesAnyPathPattern(patterns []string, value string) bool {
	for _, pat := range patterns {
		if MatchPathPattern(pat, value) {
			return true
		}
	}
	return false
}

// MatchPathPattern reports whether filePath matches pattern, where pattern may
// be a repo-relative file path (`src/handler.go` or `/src/handler.go`), a
// folder path (`src/auth/` or `/src/auth`, matching any edited file under
// `src/auth/**`), or a glob (`schemas/**/*.sql`).
func MatchPathPattern(pattern, filePath string) bool {
	val := normalizeRepoRelPath(filePath)
	if val == "" {
		return false
	}
	rawPat := filepath.ToSlash(strings.TrimSpace(pattern))
	if rawPat == "" {
		return false
	}
	if rawPat == "/" || rawPat == "./" {
		return true
	}
	isExplicitFolder := strings.HasSuffix(rawPat, "/")
	pat := normalizeRepoRelPath(rawPat)
	if pat == "" {
		return isExplicitFolder
	}
	if isExplicitFolder {
		return MatchGlob(pat+"/**", val)
	}
	if MatchGlob(pat, val) {
		return true
	}
	return MatchGlob(pat+"/**", val)
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
