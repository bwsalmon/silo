package grainfiles

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// RefForm is the syntactic form of a Grainfile reference (docs/grainfiles-spec.md §8.1).
type RefForm string

const (
	// RefRemote is a plain https:// (or file:// in tests) URL.
	RefRemote RefForm = "remote"
	// RefRooted is a repository-root-relative path starting with '/'.
	RefRooted RefForm = "rooted"
	// RefRelative is a path relative to the referring Grainfile's directory.
	RefRelative RefForm = "relative"
)

// Ref is a parsed reference from `recipient`, `requestor`, `references`,
// `depends_on`, or `trigger_with` (docs/grainfiles-spec.md §8.1).
type Ref struct {
	Form          RefForm
	Repo          string
	Path          string
	GitRef        string
	Anchor        string
	Mode          string // "blob", "tree", or ""
	Forge         string // "github", "gitlab", "gitiles", "file", or ""
	TrailingSlash bool
}

// CanonicalURL formats a remote Ref into its canonical URL string
// (docs/grainfiles-spec.md §8.5). For local references that have not yet been
// resolved against a repository, it returns the normalized local path.
func (r Ref) CanonicalURL() string {
	var base string
	if r.Form == RefRemote {
		cleanSub := ""
		if r.Path != "" {
			cleanSub = cleanPosixPath(strings.Trim(r.Path, "/"))
			if cleanSub == "." {
				cleanSub = ""
			}
		}
		switch {
		case r.Forge == "gitlab" && (r.Mode == "blob" || r.Mode == "tree"):
			if cleanSub != "" {
				base = fmt.Sprintf("%s/-/%s/%s/%s", r.Repo, r.Mode, r.GitRef, cleanSub)
			} else {
				base = fmt.Sprintf("%s/-/%s/%s", r.Repo, r.Mode, r.GitRef)
			}
		case r.Forge == "gitiles" && r.GitRef != "":
			if cleanSub != "" {
				base = fmt.Sprintf("%s/+/%s/%s", r.Repo, r.GitRef, cleanSub)
			} else {
				base = fmt.Sprintf("%s/+/%s", r.Repo, r.GitRef)
			}
		case r.Mode == "blob" || r.Mode == "tree":
			if cleanSub != "" {
				base = fmt.Sprintf("%s/%s/%s/%s", r.Repo, r.Mode, r.GitRef, cleanSub)
			} else {
				base = fmt.Sprintf("%s/%s/%s", r.Repo, r.Mode, r.GitRef)
			}
		default:
			if cleanSub != "" {
				base = r.Repo + "/" + cleanSub
			} else {
				base = r.Repo
			}
		}
	} else if r.Form == RefRooted {
		base = "/" + strings.TrimPrefix(r.Path, "/")
	} else {
		base = r.Path
	}
	if r.Anchor != "" {
		return base + "#" + r.Anchor
	}
	return base
}

var (
	anchorRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repoSegSlugRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	urlSchemeRe   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	headingRe     = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	nonSlugRe     = regexp.MustCompile(`[^\w\s-]`)
	spaceSlugRe   = regexp.MustCompile(`\s`)
)

func hasEncodedPathHazard(s string) bool {
	if strings.ContainsRune(s, '\\') || strings.ContainsRune(s, '\x00') {
		return true
	}
	lower := strings.ToLower(s)
	return strings.Contains(lower, "%2e") ||
		strings.Contains(lower, "%2f") ||
		strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%00")
}

func trimDotGitSuffix(s string) string {
	for len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".git") {
		s = s[:len(s)-4]
	}
	return s
}

func validRepoSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "@\\") {
		return false
	}
	return repoSegSlugRe.MatchString(seg)
}

func hasDotSegment(p string) (hasDot, hasDotDot bool) {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case ".":
			hasDot = true
		case "..":
			hasDotDot = true
		}
	}
	return hasDot, hasDotDot
}

// CanonicalRepoURL returns the canonical base URL of a git repository
// (`https://<lower-host>/<repo-path>`, with default port `:443`, trailing `/`,
// and trailing `.git` stripped; docs/grainfiles-spec.md §8.5).
func CanonicalRepoURL(rawURL string) (string, error) {
	if rawURL == "" {
		return "", newFinding(CodeMalformedRef, "", "empty repository URL")
	}
	if hasEncodedPathHazard(rawURL) {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("encoded path separators or traversal sequences are not allowed in repository URL: %q", rawURL))
	}
	if strings.HasPrefix(rawURL, "file://") {
		rest := strings.Trim(rawURL[len("file://"):], "/")
		if rest == "" || strings.Contains(rest, "//") {
			return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", rawURL))
		}
		segs := strings.Split(rest, "/")
		repoName := trimDotGitSuffix(segs[0])
		if !validRepoSegment(repoName) {
			return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid repository path: %q", rawURL))
		}
		return "file://" + repoName, nil
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("only https:// (and file:// in tests) URLs are allowed: %q", rawURL))
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed URL %q: %v", rawURL, err))
	}
	if u.User != nil || strings.Contains(u.Host, "@") || strings.Contains(u.Path, "@") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("credentials or @ authority characters are not allowed in reference URLs: %q", rawURL))
	}
	if u.RawQuery != "" || strings.Contains(rawURL, "?") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("query parameters are not allowed in references (%q)", rawURL))
	}
	if strings.Contains(u.Path, "//") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("empty path segments (//) are not allowed in reference URLs: %q", rawURL))
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("missing host in reference URL: %q", rawURL))
	}
	host := hostname
	if portStr := u.Port(); portStr != "" {
		portNum, err := strconv.Atoi(portStr)
		if err != nil || portNum <= 0 {
			return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid port in reference URL: %q", rawURL))
		}
		if portNum != 443 {
			host = fmt.Sprintf("%s:%d", hostname, portNum)
		}
	}
	trimmedPath := strings.TrimRight(u.Path, "/")
	trimmedPath = trimDotGitSuffix(trimmedPath)
	if trimmedPath == "" || trimmedPath == "/" {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", rawURL))
	}
	rawRepoPath := strings.TrimPrefix(trimmedPath, "/")
	if hasDot, hasDotDot := hasDotSegment(rawRepoPath); hasDot || hasDotDot {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid repository path: %q", rawURL))
	}
	for _, seg := range strings.Split(rawRepoPath, "/") {
		if !validRepoSegment(seg) {
			return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid repository path segment %q in %q", seg, rawURL))
		}
	}
	cleaned := cleanPosixPath(rawRepoPath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid repository path: %q", rawURL))
	}
	return "https://" + host + "/" + cleaned, nil
}

// ParseRef parses a reference string according to docs/grainfiles-spec.md §8.1.
func ParseRef(s string) (Ref, error) {
	if s == "" || strings.ContainsAny(s, " \t\r\n\\") || hasEncodedPathHazard(s) {
		return Ref{}, newFinding(CodeMalformedRef, "", "a reference must be a non-empty string without whitespace, backslashes, or encoded path traversal")
	}
	var anchor string
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		anchor = s[idx+1:]
		s = s[:idx]
		if anchor == "" || !anchorRe.MatchString(anchor) {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed anchor in %q", s))
		}
	}
	if s == "" {
		return Ref{}, newFinding(CodeMalformedRef, "", "empty reference path before anchor")
	}
	if strings.HasPrefix(s, "git::") || strings.Contains(s, "::") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("go-getter git:: syntax is not allowed; use a plain https:// URL (%q)", s))
	}
	if strings.Contains(s, "?") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("query parameters are not allowed in references (%q)", s))
	}
	if strings.HasPrefix(s, "git@") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("SSH references are not allowed; use https:// (%q)", s))
	}

	if strings.Contains(s, "://") || urlSchemeRe.MatchString(s) {
		if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "file://") {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("only https:// (and file:// in tests) URLs are allowed: %q", s))
		}
		if strings.HasPrefix(s, "https://") {
			return parseHTTPSRef(s, anchor)
		}
		return parseFileRef(s, anchor)
	}

	if strings.Contains(s, "//") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("empty path segments (//) are not allowed in references: %q", s))
	}
	trailing := strings.HasSuffix(s, "/") || s == "." || s == ".." || strings.HasSuffix(s, "/.") || strings.HasSuffix(s, "/..")
	if strings.HasPrefix(s, "/") {
		return Ref{
			Form:          RefRooted,
			Path:          s[1:],
			Anchor:        anchor,
			TrailingSlash: trailing,
		}, nil
	}
	return Ref{
		Form:          RefRelative,
		Path:          s,
		Anchor:        anchor,
		TrailingSlash: trailing,
	}, nil
}

func parseHTTPSRef(s, anchor string) (Ref, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed reference URL: %q", s))
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("credentials are not allowed in reference URLs: %q", s))
	}
	if strings.Contains(u.Path, "//") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("empty path segments (//) are not allowed in reference URLs: %q", s))
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || !strings.Contains(hostname, ".") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("missing or invalid host in reference URL: %q", s))
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) < 2 {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("repository URL must have at least <owner>/<repo>: %q", s))
	}
	trailing := strings.HasSuffix(u.Path, "/")

	var (
		repoSegs []string
		mode     string
		gitRef   string
		subSegs  []string
		forge    string
	)
	if dashIdx := slices.Index(segs, "-"); dashIdx >= 0 {
		if dashIdx < 2 || dashIdx+2 >= len(segs) || (segs[dashIdx+1] != "blob" && segs[dashIdx+1] != "tree") {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed GitLab reference URL: %q", s))
		}
		repoSegs = segs[:dashIdx]
		mode = segs[dashIdx+1]
		gitRef = segs[dashIdx+2]
		subSegs = segs[dashIdx+3:]
		forge = "gitlab"
	} else if plusIdx := slices.Index(segs, "+"); plusIdx >= 0 {
		if plusIdx == 0 || plusIdx+1 >= len(segs) {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed Gitiles reference URL: %q", s))
		}
		repoSegs = segs[:plusIdx]
		mode = ""
		gitRef = segs[plusIdx+1]
		subSegs = segs[plusIdx+2:]
		forge = "gitiles"
	} else if len(segs) >= 3 && (segs[2] == "blob" || segs[2] == "tree") {
		if len(segs) < 4 {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("missing ref in %s URL: %q", segs[2], s))
		}
		repoSegs = segs[:2]
		mode = segs[2]
		gitRef = segs[3]
		subSegs = segs[4:]
		forge = "github"
	} else {
		for _, seg := range segs[:len(segs)-1] {
			if len(seg) >= 4 && strings.EqualFold(seg[len(seg)-4:], ".git") {
				return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("sub-path in repository URL must use /blob/<ref>/ or /tree/<ref>/: %q", s))
			}
		}
		repoSegs = segs
		forge = "github"
	}

	if mode == "blob" && (len(subSegs) == 0 || trailing) {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("blob URL must name a file path: %q", s))
	}
	if gitRef == "." || gitRef == ".." || strings.ContainsAny(gitRef, "@\\") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid git ref %q in reference URL: %q", gitRef, s))
	}

	host := hostname
	if portStr := u.Port(); portStr != "" {
		portNum, err := strconv.Atoi(portStr)
		if err != nil || portNum <= 0 {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid port in reference URL: %q", s))
		}
		if portNum != 443 {
			host = fmt.Sprintf("%s:%d", hostname, portNum)
		}
	}
	repoURL, err := CanonicalRepoURL(fmt.Sprintf("https://%s/%s", host, strings.Join(repoSegs, "/")))
	if err != nil {
		return Ref{}, err
	}
	sub := strings.Join(subSegs, "/")
	if sub != "" {
		normSub := cleanPosixPath(sub)
		hasDot, hasDotDot := hasDotSegment(sub)
		if normSub == ".." || strings.HasPrefix(normSub, "../") || hasDotDot {
			return Ref{}, newFinding(CodeEscapesRepo, "", fmt.Sprintf("%q climbs above the repository root", s))
		}
		if hasDot {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("dot segments (.) are not allowed in remote reference URLs: %q", s))
		}
		sub = normSub
		if trailing {
			sub += "/"
		}
	}
	return Ref{
		Form:          RefRemote,
		Repo:          repoURL,
		Path:          sub,
		GitRef:        gitRef,
		Anchor:        anchor,
		Mode:          mode,
		Forge:         forge,
		TrailingSlash: trailing || (sub == "" && mode != "blob") || mode == "tree",
	}, nil
}

func parseFileRef(s, anchor string) (Ref, error) {
	raw := strings.TrimLeft(s[len("file://"):], "/")
	if raw == "" || strings.Contains(raw, "//") {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", s))
	}
	trailing := strings.HasSuffix(raw, "/")
	var segs []string
	for _, seg := range strings.Split(raw, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", s))
	}
	var (
		repoName string
		mode     string
		gitRef   string
		subSegs  []string
	)
	if len(segs) >= 2 && (segs[1] == "blob" || segs[1] == "tree") {
		if len(segs) < 3 {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("missing ref in %s URL: %q", segs[1], s))
		}
		repoName = segs[0]
		mode = segs[1]
		gitRef = segs[2]
		subSegs = segs[3:]
	} else {
		repoName = segs[0]
		subSegs = segs[1:]
	}
	if mode == "blob" && (len(subSegs) == 0 || trailing) {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("blob URL must name a file path: %q", s))
	}
	repoURL, err := CanonicalRepoURL("file://" + repoName)
	if err != nil {
		return Ref{}, err
	}
	sub := strings.Join(subSegs, "/")
	if sub != "" {
		normSub := cleanPosixPath(sub)
		hasDot, hasDotDot := hasDotSegment(sub)
		if normSub == ".." || strings.HasPrefix(normSub, "../") || hasDotDot {
			return Ref{}, newFinding(CodeEscapesRepo, "", fmt.Sprintf("%q climbs above the repository root", s))
		}
		if hasDot {
			return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("dot segments (.) are not allowed in remote reference URLs: %q", s))
		}
		sub = normSub
		if trailing {
			sub += "/"
		}
	}
	return Ref{
		Form:          RefRemote,
		Repo:          repoURL,
		Path:          sub,
		GitRef:        gitRef,
		Anchor:        anchor,
		Mode:          mode,
		Forge:         "file",
		TrailingSlash: trailing || (sub == "" && mode != "blob") || mode == "tree",
	}, nil
}

// Source describes the authoritative repository location of a Grainfile
// (docs/grainfiles-spec.md §6.2, §8.2, §11.1; docs/hub.md §6.3).
type Source struct {
	// RepoURL is the repository base URL (e.g. "https://github.com/acme/widgets").
	RepoURL string
	// Ref is the git branch/ref or commit SHA (e.g. "main" or "refs/heads/main").
	Ref string
	// Commit is the optional git commit SHA.
	Commit string
	// Path is the repository-relative path of the Grainfile
	// (e.g. ".grain/tasks/2026-10-16-widget-schema-v2.task.md").
	Path string
}

func normalizeURLRef(ref, commit string) string {
	r := strings.TrimSpace(ref)
	r = strings.TrimPrefix(r, "refs/heads/")
	r = strings.TrimPrefix(r, "refs/tags/")
	if r != "" {
		return r
	}
	if c := strings.TrimSpace(commit); c != "" {
		return c
	}
	return "main"
}

// Resolve resolves all repo-local relative (`./...`, `../...`) and `/`-rooted
// (`/docs/...`) references in g (`recipient`, `requestor` when a URL/path,
// `references`, `depends_on`, and `trigger_with`) to canonical `https://` URLs
// against the Grainfile's authoritative source location (repoURL, ref, and
// filePath), rejecting any path that climbs above the repository root (GF036).
func (g *Grainfile) Resolve(repoURL, ref, filePath string) error {
	return g.ResolveSource(Source{
		RepoURL: repoURL,
		Ref:     ref,
		Path:    filePath,
	})
}

// ResolveSource resolves g against a Source struct, populating g.URL, g.Repo,
// g.Ref, g.Commit, g.Path, and resolving all reference fields in place to
// canonical URLs.
func (g *Grainfile) ResolveSource(src Source) error {
	canonRepo, err := CanonicalRepoURL(src.RepoURL)
	if err != nil {
		return err
	}
	relPath := src.Path
	if relPath == "" {
		relPath = g.Path
	}
	relPath = strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	if relPath == "" {
		return newFinding(CodeMalformedRef, "", "empty grainfile path in repository")
	}
	cleanFile := cleanPosixPath(relPath)
	if cleanFile == "." || cleanFile == ".." || strings.HasPrefix(cleanFile, "../") {
		return newFinding(CodeEscapesRepo, relPath, fmt.Sprintf("grainfile path %q climbs above the repository root", relPath))
	}

	urlRef := normalizeURLRef(src.Ref, src.Commit)

	var resolvedRecipient string
	if g.Kind == KindTask && g.Recipient == "" {
		g.Recipient = "/"
	}
	if g.Recipient != "" {
		// Resolve recipient (always a repository or folder deployment identity).
		var err error
		resolvedRecipient, _, err = ResolveRefWithSource(Source{
			RepoURL: canonRepo,
			Ref:     urlRef,
			Commit:  src.Commit,
			Path:    cleanFile,
		}, g.Recipient, "tree")
		if err != nil {
			return wrapFieldError("recipient", cleanFile, err)
		}
	}

	// Resolve requestor when it is a deployment / URL identity.
	resolvedRequestor := g.Requestor
	if g.Requestor != "" && !IsUserIdentity(g.Requestor) {
		reqURL, _, err := ResolveRefWithSource(Source{
			RepoURL: canonRepo,
			Ref:     urlRef,
			Commit:  src.Commit,
			Path:    cleanFile,
		}, g.Requestor, "tree")
		if err != nil {
			return wrapFieldError("requestor", cleanFile, err)
		}
		resolvedRequestor = reqURL
	}

	// Resolve references (default mode "blob" unless trailing slash or repo root).
	var resolvedRefs []string
	if len(g.References) > 0 {
		resolvedRefs = make([]string, len(g.References))
		for i, raw := range g.References {
			u, _, err := ResolveRefWithSource(Source{
				RepoURL: canonRepo,
				Ref:     urlRef,
				Commit:  src.Commit,
				Path:    cleanFile,
			}, raw, "blob")
			if err != nil {
				return wrapFieldError("references", cleanFile, err)
			}
			resolvedRefs[i] = u
		}
	}

	// Resolve depends_on (always grainfiles -> "blob").
	var resolvedDeps []string
	if len(g.DependsOn) > 0 {
		resolvedDeps = make([]string, len(g.DependsOn))
		for i, raw := range g.DependsOn {
			u, _, err := ResolveRefWithSource(Source{
				RepoURL: canonRepo,
				Ref:     urlRef,
				Commit:  src.Commit,
				Path:    cleanFile,
			}, raw, "blob")
			if err != nil {
				return wrapFieldError("depends_on", cleanFile, err)
			}
			resolvedDeps[i] = u
		}
	}

	// Resolve from_template (always a .grain.md.jinja2 template -> "blob").
	var resolvedFromTemplate string
	if g.FromTemplate != "" {
		u, _, err := ResolveRefWithSource(Source{
			RepoURL: canonRepo,
			Ref:     urlRef,
			Commit:  src.Commit,
			Path:    cleanFile,
		}, g.FromTemplate, "blob")
		if err != nil {
			return wrapFieldError("from_template", cleanFile, err)
		}
		resolvedFromTemplate = u
	}

	g.Repo = canonRepo
	if src.Ref != "" {
		g.Ref = src.Ref
	} else {
		g.Ref = urlRef
	}
	if src.Commit != "" {
		g.Commit = src.Commit
	}
	g.Path = cleanFile
	g.URL = formatCanonicalURL(canonRepo, urlRef, "blob", cleanFile, "")
	if g.Grainfile == "" {
		g.Grainfile = VersionV1
	}
	if spec, ok := LookupVersion(g.Grainfile); ok {
		if g.SpecSummaryPath == "" {
			g.SpecSummaryPath = spec.SpecSummaryPath
		}
		if g.SpecPath == "" {
			g.SpecPath = spec.SpecPath
		}
	}
	g.Recipient = resolvedRecipient
	g.Requestor = resolvedRequestor
	g.References = resolvedRefs
	g.DependsOn = resolvedDeps
	g.FromTemplate = resolvedFromTemplate
	return nil
}

func wrapFieldError(field, where string, err error) error {
	if f, ok := err.(Finding); ok {
		f.Path = where
		if !strings.HasPrefix(f.Message, field+": ") {
			f.Message = field + ": " + f.Message
		}
		return f
	}
	return newFinding(CodeMalformedRef, where, fmt.Sprintf("%s: %v", field, err))
}

// ResolveRef resolves a single reference (remote, `/`-rooted, or relative)
// against (repoURL, ref, grainfilePath) to a canonical URL.
func ResolveRef(repoURL, ref, grainfilePath, rawRef string) (string, error) {
	u, _, err := ResolveRefWithSource(Source{
		RepoURL: repoURL,
		Ref:     ref,
		Path:    grainfilePath,
	}, rawRef, "blob")
	return u, err
}

// ResolveDeploymentRef resolves a `recipient` or URL `requestor` deployment
// identity (remote, `/`-rooted, or relative) against (repoURL, ref,
// grainfilePath) to a canonical repository or `/tree/<ref>/<folder>` URL.
func ResolveDeploymentRef(repoURL, ref, grainfilePath, rawRef string) (string, error) {
	u, _, err := ResolveRefWithSource(Source{
		RepoURL: repoURL,
		Ref:     ref,
		Path:    grainfilePath,
	}, rawRef, "tree")
	return u, err
}

// ResolveRefWithSource resolves rawRef against src and returns both the
// canonical URL string and the resolved Ref struct.
func ResolveRefWithSource(src Source, rawRef, defaultMode string) (string, Ref, error) {
	r, err := ParseRef(rawRef)
	if err != nil {
		return "", Ref{}, err
	}
	if defaultMode == "tree" {
		if r.Anchor != "" {
			return "", Ref{}, newFinding(CodeMalformedRef, src.Path, fmt.Sprintf("deployment identity %q may not carry an anchor", rawRef))
		}
		if r.Mode == "blob" {
			return "", Ref{}, newFinding(CodeWrongTargetKind, src.Path, fmt.Sprintf("%q is a file reference (/blob/), expected a repository or folder", rawRef))
		}
		for _, k := range []Kind{KindTask, KindProcess} {
			for _, suf := range []string{"." + string(k) + ".grain.md", "." + string(k) + ".md"} {
				if strings.HasSuffix(r.Path, suf) || strings.HasSuffix(r.Repo, suf) {
					return "", Ref{}, newFinding(CodeWrongTargetKind, src.Path, fmt.Sprintf("%q is a %s grainfile, expected a repository or folder", rawRef, k))
				}
			}
		}
	}

	if r.Form == RefRemote {
		if r.Path != "" {
			norm := cleanPosixPath(strings.Trim(r.Path, "/"))
			if norm == ".." || strings.HasPrefix(norm, "../") {
				return "", Ref{}, newFinding(CodeEscapesRepo, src.Path, fmt.Sprintf("%q climbs above the repository root", rawRef))
			}
			if norm == "." {
				norm = ""
			}
			r.Path = norm
		}
		if r.TrailingSlash && r.Anchor != "" {
			return "", Ref{}, newFinding(CodeUnresolvedRef, src.Path, fmt.Sprintf("folder reference %q may not carry an anchor", rawRef))
		}
		return r.CanonicalURL(), r, nil
	}

	canonRepo, err := CanonicalRepoURL(src.RepoURL)
	if err != nil {
		return "", Ref{}, err
	}
	urlRef := normalizeURLRef(src.Ref, src.Commit)

	var resolvedPath string
	if r.Form == RefRooted {
		if r.Path != "" {
			hasDot, hasDotDot := hasDotSegment(r.Path)
			if hasDotDot {
				return "", Ref{}, newFinding(CodeEscapesRepo, src.Path, fmt.Sprintf("%q climbs above the repository root", rawRef))
			}
			if hasDot {
				return "", Ref{}, newFinding(CodeMalformedRef, src.Path, fmt.Sprintf("dot segments (.) are not allowed in rooted references: %q", rawRef))
			}
			resolvedPath = cleanPosixPath(r.Path)
		}
	} else {
		relFile := strings.TrimPrefix(filepath.ToSlash(src.Path), "/")
		dir := cleanPosixDir(relFile)
		joined := r.Path
		if dir != "" && dir != "." {
			joined = dir + "/" + r.Path
		}
		resolvedPath = cleanPosixPath(joined)
	}

	if resolvedPath == ".." || strings.HasPrefix(resolvedPath, "../") {
		return "", Ref{}, newFinding(CodeEscapesRepo, src.Path, fmt.Sprintf("%q climbs above the repository root", rawRef))
	}
	if resolvedPath == "." {
		resolvedPath = ""
	}

	isFolder := resolvedPath == "" || r.TrailingSlash || defaultMode == "tree"
	if isFolder && r.Anchor != "" {
		return "", Ref{}, newFinding(CodeUnresolvedRef, src.Path, fmt.Sprintf("folder reference %q may not carry an anchor", rawRef))
	}

	mode := defaultMode
	if mode == "" {
		mode = "blob"
	}
	if isFolder {
		if resolvedPath == "" {
			mode = ""
		} else {
			mode = "tree"
		}
	}

	canonURL := formatCanonicalURL(canonRepo, urlRef, mode, resolvedPath, r.Anchor)
	resolved := Ref{
		Form:          RefRemote,
		Repo:          canonRepo,
		Path:          resolvedPath,
		GitRef:        urlRef,
		Anchor:        r.Anchor,
		Mode:          mode,
		Forge:         "github",
		TrailingSlash: isFolder,
	}
	return canonURL, resolved, nil
}

func formatCanonicalURL(canonRepo, gitRef, mode, subPath, anchor string) string {
	var u string
	if subPath == "" || mode == "" {
		u = canonRepo
	} else if needsGitLabDash(canonRepo) {
		u = fmt.Sprintf("%s/-/%s/%s/%s", canonRepo, mode, gitRef, subPath)
	} else {
		u = fmt.Sprintf("%s/%s/%s/%s", canonRepo, mode, gitRef, subPath)
	}
	if anchor != "" {
		u += "#" + anchor
	}
	return u
}

func needsGitLabDash(canonRepo string) bool {
	if !strings.HasPrefix(canonRepo, "https://") {
		return false
	}
	rest := canonRepo[len("https://"):]
	slashIdx := strings.IndexByte(rest, '/')
	if slashIdx < 0 {
		return false
	}
	repoPath := strings.Trim(rest[slashIdx+1:], "/")
	return strings.Count(repoPath, "/") >= 2
}

// RepoSlug returns the normalized `owner/repo` slug (or repo path) for repoURL,
// or empty string if repoURL is invalid.
func RepoSlug(repoURL string) string {
	canon, err := NormalizeRepoSlugOrURL(repoURL)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(canon, "file://") {
		return strings.TrimPrefix(canon, "file://")
	}
	u, err := url.Parse(canon)
	if err != nil {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

// IsSameRepo reports whether repoA and repoB refer to the same canonical
// repository (docs/grainfiles-spec.md §8.5; docs/hub.md §6.4.1).
func IsSameRepo(repoA, repoB string) bool {
	ca, errA := canonicalRepoFromAnyRef(repoA)
	cb, errB := canonicalRepoFromAnyRef(repoB)
	if errA != nil || errB != nil || ca == "" || cb == "" {
		return false
	}
	return strings.EqualFold(ca, cb)
}

// CanonicalDeploymentURL constructs the canonical Hub deployment URL
// ("https://<hub-host>/deployments/<id>") for deployment id.
func CanonicalDeploymentURL(hubURL, id string) string {
	id = strings.Trim(strings.TrimSpace(id), "/")
	if id == "" {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if base == "" {
		return "/deployments/" + id
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return base + "/deployments/" + id
}

// DeploymentIDFromRecipient extracts the deployment ID <id> when raw is a
// canonical Hub deployment URL ("https://<hub-host>/deployments/<id>"), a
// rooted "/deployments/<id>" reference, or a repository tree URL whose path is
// "deployments/<id>" (produced when ResolveSource resolves "/deployments/<id>").
// It returns "" if raw does not name a "/deployments/<id>" recipient.
func DeploymentIDFromRecipient(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		s = s[:idx]
	}
	if !strings.Contains(s, "://") {
		trimmed := strings.Trim(s, "/")
		segs := strings.Split(trimmed, "/")
		if len(segs) == 2 && strings.EqualFold(segs[0], "deployments") && segs[1] != "" {
			return strings.ToLower(segs[1])
		}
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) == 2 && strings.EqualFold(segs[0], "deployments") && segs[1] != "" {
		return strings.ToLower(segs[1])
	}
	if r, err := ParseRef(s); err == nil && r.Form == RefRemote && r.Mode == "tree" {
		pSegs := strings.Split(strings.Trim(r.Path, "/"), "/")
		if len(pSegs) == 2 && strings.EqualFold(pSegs[0], "deployments") && pSegs[1] != "" {
			return strings.ToLower(pSegs[1])
		}
	}
	return ""
}

func canonicalRepoFromAnyRef(s string) (string, error) {
	if r, err := ParseRef(s); err == nil && r.Form == RefRemote && r.Repo != "" {
		return r.Repo, nil
	}
	return CanonicalRepoURL(s)
}

// AuthorizedReferences partitions g's `references` (and same-repo `from_template`) into:
//   - sameRepo: references targeting sourceRepoURL (which a non-draft Grainfile
//     in sourceRepoURL is authorized to grant `recipient` read access to through
//     Grain Hub; docs/hub.md §6.4.1, docs/hub-interface.md).
//   - crossRepo: references targeting any other repository (which a Grainfile in
//     sourceRepoURL cannot authorize on its own: "a repository can only grant
//     access to itself").
//
// If any reference in g.References (or g.FromTemplate) is still repo-local
// (`/`-rooted or relative), it is resolved against sourceRepoURL using g.Ref,
// g.Commit, and g.Path.
func (g *Grainfile) AuthorizedReferences(sourceRepoURL string) (sameRepo, crossRepo []string) {
	if g == nil || (len(g.References) == 0 && g.FromTemplate == "") {
		return nil, nil
	}
	repoURL := sourceRepoURL
	if repoURL == "" {
		repoURL = g.Repo
	}
	canonSource, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return nil, slices.Clone(g.References)
	}
	urlRef := normalizeURLRef(g.Ref, g.Commit)

	for _, raw := range g.References {
		u, r, err := ResolveRefWithSource(Source{
			RepoURL: canonSource,
			Ref:     urlRef,
			Commit:  g.Commit,
			Path:    g.Path,
		}, raw, "blob")
		if err != nil {
			continue
		}
		if IsSameRepo(r.Repo, canonSource) {
			if strings.TrimSpace(g.Commit) != "" && (r.Mode == "blob" || r.Mode == "tree") {
				r.GitRef = strings.TrimSpace(g.Commit)
				u = r.CanonicalURL()
			}
			sameRepo = append(sameRepo, u)
		} else {
			crossRepo = append(crossRepo, u)
		}
	}
	if g.FromTemplate != "" {
		u, r, err := ResolveRefWithSource(Source{
			RepoURL: canonSource,
			Ref:     urlRef,
			Commit:  g.Commit,
			Path:    g.Path,
		}, g.FromTemplate, "blob")
		if err == nil && IsSameRepo(r.Repo, canonSource) {
			if strings.TrimSpace(g.Commit) != "" && (r.Mode == "blob" || r.Mode == "tree") {
				r.GitRef = strings.TrimSpace(g.Commit)
				u = r.CanonicalURL()
			}
			if !slices.Contains(sameRepo, u) {
				sameRepo = append(sameRepo, u)
			}
		}
	}
	return sameRepo, crossRepo
}

// AuthorizedReferences partitions a slice of resolved (or repo-local root)
// reference strings against sourceRepoURL into same-repository references and
// cross-repository references.
func AuthorizedReferences(sourceRepoURL string, references []string) (sameRepo, crossRepo []string) {
	g := &Grainfile{References: references}
	return g.AuthorizedReferences(sourceRepoURL)
}

// AuthorizesReference reports whether a non-draft Grainfile g in sourceRepoURL
// authorizes reading requestedRef through Grain Hub's `fetch_reference` endpoint
// (docs/hub.md §6.4.1, docs/hub-interface.md):
//  1. g must not be a draft (`g.Draft == false`).
//  2. requestedRef must resolve to a target within sourceRepoURL (same-origin
//     authority rule: a Grainfile in Repo A cannot grant access to Repo C).
//  3. requestedRef must match an entry in g.References or a same-repo g.FromTemplate
//     (exact file path, a path under a `/tree/` folder reference in g.References,
//     or any path in sourceRepoURL when g.References includes sourceRepoURL's root).
func (g *Grainfile) AuthorizesReference(sourceRepoURL, requestedRef string) bool {
	_, err := g.CheckReferenceAccess(sourceRepoURL, requestedRef)
	return err == nil
}

// CheckReferenceAccess verifies whether g authorizes fetching requestedRef from
// sourceRepoURL and returns the resolved target Ref (with any `#anchor` stripped
// from Path).
func (g *Grainfile) CheckReferenceAccess(sourceRepoURL, requestedRef string) (Ref, error) {
	if g == nil {
		return Ref{}, fmt.Errorf("nil grainfile")
	}
	if g.Draft {
		return Ref{}, fmt.Errorf("draft grainfile cannot authorize reference access")
	}
	repoURL := sourceRepoURL
	if repoURL == "" {
		repoURL = g.Repo
	}
	canonSource, err := canonicalRepoFromAnyRef(repoURL)
	if err != nil {
		return Ref{}, err
	}
	urlRef := normalizeURLRef(g.Ref, g.Commit)

	_, target, err := ResolveRefWithSource(Source{
		RepoURL: canonSource,
		Ref:     urlRef,
		Commit:  g.Commit,
		Path:    g.Path,
	}, requestedRef, "blob")
	if err != nil {
		return Ref{}, err
	}
	if !IsSameRepo(target.Repo, canonSource) {
		return Ref{}, fmt.Errorf("cross-origin reference not authorized by source repository")
	}

	targetPath := cleanPosixPath(strings.Trim(target.Path, "/"))
	if targetPath == "." {
		targetPath = ""
	}

	commitTrim := strings.TrimSpace(g.Commit)
	refTrim := normalizeURLRef(g.Ref, "")

	candidates := slices.Clone(g.References)
	if g.FromTemplate != "" {
		candidates = append(candidates, g.FromTemplate)
	}

	for _, raw := range candidates {
		_, allowed, err := ResolveRefWithSource(Source{
			RepoURL: canonSource,
			Ref:     urlRef,
			Commit:  g.Commit,
			Path:    g.Path,
		}, raw, "blob")
		if err != nil || !IsSameRepo(allowed.Repo, canonSource) {
			continue
		}
		if target.GitRef != "" &&
			target.GitRef != urlRef &&
			target.GitRef != allowed.GitRef &&
			(commitTrim == "" || target.GitRef != commitTrim) &&
			(refTrim == "" || target.GitRef != refTrim) {
			continue
		}
		allowedPath := cleanPosixPath(strings.Trim(allowed.Path, "/"))
		if allowedPath == "." {
			allowedPath = ""
		}
		// Repository root grant covers everything in canonSource.
		if allowedPath == "" && allowed.Mode != "blob" {
			if targetPath == "" {
				target.Mode = "tree"
				target.TrailingSlash = true
			}
			return target, nil
		}
		// Exact file or folder match (ignoring #anchor).
		if allowedPath == targetPath {
			if allowed.Mode == "tree" || allowed.TrailingSlash {
				target.Mode = "tree"
				target.TrailingSlash = true
			}
			return target, nil
		}
		// Folder (/tree/) grant covers any path beneath that folder.
		if (allowed.Mode == "tree" || allowed.TrailingSlash) && allowedPath != "" {
			if strings.HasPrefix(targetPath, allowedPath+"/") {
				return target, nil
			}
		}
	}

	return Ref{}, fmt.Errorf("reference %q is not listed in grainfile references", requestedRef)
}

// HeadingSlug computes the GitHub-style slug for a Markdown heading
// (docs/grainfiles-spec.md §8.1.3).
func HeadingSlug(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = nonSlugRe.ReplaceAllString(s, "")
	s = spaceSlugRe.ReplaceAllString(s, "-")
	return s
}

// HasHeadingAnchor reports whether markdownBody contains a Markdown heading
// whose GitHub-style slug equals anchor (docs/grainfiles-spec.md §8.1.3).
func HasHeadingAnchor(markdownBody, anchor string) bool {
	if anchor == "" {
		return true
	}
	norm := strings.ReplaceAll(markdownBody, "\r\n", "\n")
	for _, line := range strings.Split(norm, "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			if HeadingSlug(m[2]) == anchor {
				return true
			}
		}
	}
	return false
}

// LintTree validates a directory (either a single repository or a workspace of
// repositories with an optional `stubs.yaml` mapping repository URLs to child
// directories) according to docs/grainfiles-spec.md §4–§10.
func LintTree(top string) ([]Finding, error) {
	return LintTreeWithVars(top, nil)
}

// LintTreeWithVars is like LintTree and supplies vars when rendering
// .grain.md.jinja2 templates during validation.
func LintTreeWithVars(top string, vars map[string]string) ([]Finding, error) {
	topClean := filepath.Clean(top)
	info, err := os.Stat(topClean)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", top)
	}

	stubs := map[string]string{}
	stubsPath := filepath.Join(topClean, "stubs.yaml")
	hasStubs := false
	if rawStubs, err := os.ReadFile(stubsPath); err == nil {
		hasStubs = true
		if m, fErr := parseYAMLFrontMatter(stubsPath, string(rawStubs)); fErr == nil {
			for k, v := range m {
				if vs, ok := v.(string); ok {
					if cu, err := CanonicalRepoURL(k); err == nil {
						stubs[cu] = vs
					}
				}
			}
		}
	}

	entries, err := os.ReadDir(topClean)
	if err != nil {
		return nil, err
	}
	var children []string
	childHasGit := false
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			childPath := filepath.Join(topClean, e.Name())
			children = append(children, childPath)
			if st, err := os.Stat(filepath.Join(childPath, ".git")); err == nil && st.IsDir() {
				childHasGit = true
			}
		}
	}
	sort.Strings(children)

	_, hasExpect := os.Stat(filepath.Join(topClean, "expect.txt"))
	_, topHasGit := os.Stat(filepath.Join(topClean, ".git"))

	var repoDirs []string
	if hasStubs || hasExpect == nil || (topHasGit != nil && childHasGit) {
		repoDirs = children
	} else {
		repoDirs = []string{topClean}
	}

	backStubs := map[string]string{}
	for k, v := range stubs {
		backStubs[v] = k
	}

	type lintRepo struct {
		dir     string
		name    string
		url     string
		files   []string
		fileSet map[string]bool
	}
	type lintItem struct {
		repo    *lintRepo
		rel     string
		kind    Kind
		name    string
		gf      *Grainfile
		ok      bool
		targets map[string][]*lintItem
	}

	var repos []*lintRepo
	for _, d := range repoDirs {
		name := filepath.Base(d)
		repoURL := backStubs[name]
		if repoURL == "" {
			repoURL = "file://" + name
		}
		var files []string
		fileSet := map[string]bool{}
		_ = filepath.WalkDir(d, func(p string, de os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if de.IsDir() && de.Name() == ".git" {
				return filepath.SkipDir
			}
			if !de.IsDir() {
				rel, err := filepath.Rel(d, p)
				if err == nil {
					posixRel := filepath.ToSlash(rel)
					files = append(files, posixRel)
					fileSet[posixRel] = true
				}
			}
			return nil
		})
		sort.Strings(files)
		repos = append(repos, &lintRepo{
			dir:     d,
			name:    name,
			url:     repoURL,
			files:   files,
			fileSet: fileSet,
		})
	}

	showPath := func(r *lintRepo, rel string) string {
		if r.dir != topClean {
			return r.name + "/" + rel
		}
		return rel
	}

	var findings []Finding
	items := map[string]*lintItem{} // key: r.dir + "\x00" + rel

	for _, r := range repos {
		lowerSeen := map[string]string{}
		for _, rel := range r.files {
			where := showPath(r, rel)
			low := strings.ToLower(rel)
			if prev, exists := lowerSeen[low]; exists {
				findings = append(findings, newFinding(CodeLimitExceeded, where, fmt.Sprintf("differs only in case from %s", prev)))
			}
			lowerSeen[low] = rel

			if len(rel) > MaxPathBytes {
				findings = append(findings, newFinding(CodeLimitExceeded, where, "path too long"))
			} else {
				for _, seg := range strings.Split(rel, "/") {
					if len(seg) > MaxSegmentBytes {
						findings = append(findings, newFinding(CodeLimitExceeded, where, "path too long"))
						break
					}
				}
			}

			kind, stem, isGrain, warnCode := ClassifyStrictPath(rel)
			if !isGrain {
				if warnCode != "" {
					findings = append(findings, newFinding(warnCode, where, "unregistered kind suffix"))
				}
				continue
			}

			it := &lintItem{
				repo:    r,
				rel:     rel,
				kind:    kind,
				name:    stem,
				targets: map[string][]*lintItem{},
			}
			items[r.dir+"\x00"+rel] = it

			if !ValidateStem(stem) {
				findings = append(findings, newFinding(CodeBadName, where, fmt.Sprintf("bad grainfile name %q", stem)))
			}

			raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
			if err != nil {
				findings = append(findings, newFinding(CodeMalformedFile, where, err.Error()))
				continue
			}
			isTmpl := IsTemplatePath(rel)
			var templateVars []string
			parseRaw := raw
			if isTmpl {
				rendered, tvars, rErr := renderTemplateAtPath(where, string(raw), vars)
				if rErr != nil {
					if rErrs, ok := rErr.(Errors); ok {
						findings = append(findings, rErrs...)
					} else {
						findings = append(findings, newFinding(CodeTemplateSyntax, where, rErr.Error()))
					}
					continue
				}
				templateVars = tvars
				parseRaw = []byte(rendered)
			}
			fm, body, fErr := splitAndParseFrontMatter(where, parseRaw)
			if fErr != nil {
				findings = append(findings, *fErr)
				continue
			}
			it.ok = true
			gf := &Grainfile{
				Kind:         kind,
				Name:         stem,
				Path:         rel,
				IsTemplate:   isTmpl,
				TemplateVars: templateVars,
				Body:         body,
			}
			it.gf = gf
			vErrs := validateFrontMatterForLinter(gf, fm, where)
			findings = append(findings, gf.Warnings...)
			findings = append(findings, vErrs...)
		}
	}

	resolveInTree := func(it *lintItem, s, field string) (kind string, item *lintItem, fErr *Finding) {
		r, err := ParseRef(s)
		if err != nil {
			if f, ok := err.(Finding); ok {
				res := newFinding(f.Code, "", fmt.Sprintf("%s: %s", field, f.Message))
				return "", nil, &res
			}
			res := newFinding(CodeMalformedRef, "", fmt.Sprintf("%s: %v", field, err))
			return "", nil, &res
		}
		var targetRepo *lintRepo
		var relTarget string
		var trailingSlash bool

		if r.Form == RefRemote {
			stubName := stubs[r.Repo]
			if stubName == "" && r.GitRef == "" {
				for stubURL := range stubs {
					if strings.HasPrefix(r.Repo, stubURL+"/") {
						res := newFinding(CodeMalformedRef, "", fmt.Sprintf("%s: sub-path in repository URL %q must use /blob/<ref>/ or /tree/<ref>/", field, s))
						return "", nil, &res
					}
				}
			}
			if stubName == "" && strings.HasPrefix(r.Repo, "file://") {
				cand := strings.Trim(r.Repo[len("file://"):], "/")
				if st, err := os.Stat(filepath.Join(topClean, cand)); err == nil && st.IsDir() {
					stubName = cand
				}
			}
			for _, x := range repos {
				if x.name == stubName {
					targetRepo = x
					break
				}
			}
			if targetRepo == nil {
				return "external", nil, nil
			}
			trimmed := strings.Trim(r.Path, "/")
			if trimmed != "" {
				relTarget = cleanPosixPath(trimmed)
			}
			trailingSlash = strings.HasSuffix(r.Path, "/") || r.Path == "" || r.Mode == "tree"
		} else {
			targetRepo = it.repo
			if r.Form == RefRooted {
				if r.Path != "" {
					relTarget = cleanPosixPath(r.Path)
				}
			} else {
				dir := cleanPosixDir(it.rel)
				joined := r.Path
				if dir != "" && dir != "." {
					joined = dir + "/" + r.Path
				}
				relTarget = cleanPosixPath(joined)
			}
			trailingSlash = strings.HasSuffix(r.Path, "/")
			if relTarget == ".." || strings.HasPrefix(relTarget, "../") {
				res := newFinding(CodeEscapesRepo, "", fmt.Sprintf("%s: %q climbs above the repository root", field, s))
				return "", nil, &res
			}
		}
		if relTarget == "." {
			relTarget = ""
		}

		isDir := relTarget == ""
		if !isDir {
			if st, err := os.Stat(filepath.Join(targetRepo.dir, filepath.FromSlash(relTarget))); err == nil && st.IsDir() {
				isDir = true
			} else {
				prefix := strings.TrimSuffix(relTarget, "/") + "/"
				for _, f := range targetRepo.files {
					if strings.HasPrefix(f, prefix) {
						isDir = true
						break
					}
				}
			}
		}

		if trailingSlash {
			if !isDir {
				res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: no folder %q", field, s))
				return "", nil, &res
			}
			if r.Anchor != "" {
				res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: folder reference %q may not carry an anchor", field, s))
				return "", nil, &res
			}
			return "folder", nil, nil
		}

		if targetRepo.fileSet[relTarget] {
			if targetItem, ok := items[targetRepo.dir+"\x00"+relTarget]; ok {
				if r.Anchor != "" && targetItem.ok && !HasHeadingAnchor(targetItem.gf.Body, r.Anchor) {
					res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: no heading #%s in %s", field, r.Anchor, relTarget))
					return "", nil, &res
				}
				return string(targetItem.kind), targetItem, nil
			}
			if r.Anchor != "" && strings.HasSuffix(relTarget, ".md") {
				bodyBytes, _ := os.ReadFile(filepath.Join(targetRepo.dir, filepath.FromSlash(relTarget)))
				if !HasHeadingAnchor(string(bodyBytes), r.Anchor) {
					res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: no heading #%s in %s", field, r.Anchor, relTarget))
					return "", nil, &res
				}
			}
			return "asset", nil, nil
		}

		if isDir && r.Mode != "blob" {
			if r.Anchor != "" {
				res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: folder reference %q may not carry an anchor", field, s))
				return "", nil, &res
			}
			return "folder", nil, nil
		}

		res := newFinding(CodeUnresolvedRef, "", fmt.Sprintf("%s: %q does not resolve", field, s))
		return "", nil, &res
	}

	checkDeploymentInTree := func(it *lintItem, s, field, where string) {
		if f := validateDeploymentIdentitySyntax(s, field, where); f != nil {
			findings = append(findings, *f)
			return
		}
		targetKind, _, fErr := resolveInTree(it, s, field)
		if fErr != nil {
			fErr.Path = where
			findings = append(findings, *fErr)
			return
		}
		if targetKind != "external" && targetKind != "folder" {
			findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q resolves to a %s, expected a repository or folder", field, s, targetKind)))
		}
	}

	itemKeys := make([]string, 0, len(items))
	for k := range items {
		itemKeys = append(itemKeys, k)
	}
	sort.Strings(itemKeys)

	for _, k := range itemKeys {
		it := items[k]
		if !it.ok {
			continue
		}
		where := showPath(it.repo, it.rel)
		gf := it.gf

		if gf.Requestor != "" && !IsUserIdentity(gf.Requestor) && !strings.ContainsAny(gf.Requestor, "<>@\\ \t\r\n") {
			checkDeploymentInTree(it, gf.Requestor, "requestor", where)
		}
		if gf.Recipient != "" && !strings.ContainsAny(gf.Recipient, "<>@\\ \t\r\n") {
			checkDeploymentInTree(it, gf.Recipient, "recipient", where)
		}

		type fieldRef struct {
			field string
			ref   string
			want  bool
		}
		var refs []fieldRef
		for _, s := range gf.DependsOn {
			refs = append(refs, fieldRef{"depends_on", s, true})
		}
		for _, s := range gf.References {
			refs = append(refs, fieldRef{"references", s, false})
		}
		if gf.FromTemplate != "" {
			refs = append(refs, fieldRef{"from_template", gf.FromTemplate, false})
		}

		for _, fr := range refs {
			tKind, tItem, fErr := resolveInTree(it, fr.ref, fr.field)
			if fErr != nil {
				fErr.Path = where
				findings = append(findings, *fErr)
				continue
			}
			if fr.want {
				if fr.field == "depends_on" && it.kind == KindTask {
					if tItem != nil {
						if IsTemplatePath(tItem.rel) || (tItem.gf != nil && tItem.gf.IsTemplate) {
							findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is an unrendered .jinja2 template, expected a concrete .task.grain.md file", fr.field, fr.ref)))
						} else if tItem.kind != KindTask {
							findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: task %q cannot depend on %s %q (only .task.grain.md allowed)", fr.field, it.rel, tItem.kind, fr.ref)))
						}
					} else if tKind == "asset" || tKind == "folder" {
						findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a %s, expected task grainfile", fr.field, fr.ref, tKind)))
					} else if tKind == "external" {
						r, _ := ParseRef(fr.ref)
						if r.Mode == "tree" || !strings.HasSuffix(r.Path, ".task.grain.md") {
							findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is not a .task.grain.md file", fr.field, fr.ref)))
						}
					}
				}
			}
			if tItem != nil {
				it.targets[fr.field] = append(it.targets[fr.field], tItem)
			}
		}
	}

	// Cycle detection in depends_on (§8.4).
	for _, field := range []string{"depends_on"} {
		state := map[*lintItem]int{}
		var stack []*lintItem
		var visit func(n *lintItem)
		visit = func(n *lintItem) {
			state[n] = 1
			stack = append(stack, n)
			for _, m := range n.targets[field] {
				if state[m] == 1 {
					idx := slices.Index(stack, m)
					cyc := stack[idx:]
					names := make([]string, len(cyc))
					for i, x := range cyc {
						names[i] = x.rel
					}
					findings = append(findings, newFinding(CodeCycle, showPath(cyc[0].repo, cyc[0].rel),
						fmt.Sprintf("%s cycle: %s", field, strings.Join(names, " -> "))))
				} else if state[m] == 0 {
					visit(m)
				}
			}
			stack = stack[:len(stack)-1]
			state[n] = 2
		}
		for _, k := range itemKeys {
			it := items[k]
			if it.ok && state[it] == 0 {
				visit(it)
			}
		}
	}

	return findings, nil
}

// validateFrontMatterForLinter validates schema and field rules while deferring
// reference resolution and target-kind checks to LintTree's tree resolver.
func validateFrontMatterForLinter(g *Grainfile, fm map[string]any, where string) Errors {
	var errs Errors
	known := knownTaskFields
	if g.Kind == KindProcess {
		known = strictProcessFields
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
		if k == "trigger_with" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with is not allowed in Grainfile v1 (use depends_on and when instead)"))
			continue
		}
		if g.Kind == KindTask && (k == "when" || k == "on" || k == "gate") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s is only valid on .process.grain.md files", k)))
			continue
		}
		if g.Kind == KindProcess && (k == "recipient" || k == "requestor" || k == "depends_on") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s is only valid on .task.grain.md files", k)))
			continue
		}
		if !known[k] {
			g.Warnings = append(g.Warnings, newFinding(CodeUnknownField, where, fmt.Sprintf("unknown field %q", k)))
		}
	}

	if rawVer, ok := fm["grainfile"]; !ok {
		g.Grainfile = VersionV1
		g.SpecSummaryPath = SpecSummaryPathV1
		g.SpecPath = SpecPathV1
	} else if ver, ok := rawVer.(string); !ok || ver == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "grainfile: must be a non-empty version string (e.g. \"v1\")"))
	} else if spec, ok := LookupVersion(ver); !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("grainfile: unsupported version %q (supported: \"v1\")", ver)))
	} else {
		g.Grainfile = spec.Version
		g.SpecSummaryPath = spec.SpecSummaryPath
		g.SpecPath = spec.SpecPath
	}

	if rawTitle, ok := fm["title"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: required field missing"))
	} else if title, ok := rawTitle.(string); !ok || title == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: must be a non-empty string"))
	} else if strings.ContainsAny(title, "\r\n") {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title must be a single line"))
	} else {
		g.Title = title
	}

	if g.Kind == KindTask {
		if rawRec, ok := fm["recipient"]; !ok {
			g.Recipient = "/"
		} else if rec, ok := rawRec.(string); !ok || rec == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: must be a non-empty string"))
		} else if strings.ContainsAny(rec, "<>@\\ \t\r\n") {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: invalid deployment identity"))
		} else {
			g.Recipient = rec
		}

		if rawReq, ok := fm["requestor"]; ok {
			if req, ok := rawReq.(string); !ok || req == "" {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "requestor: must be a non-empty string"))
			} else {
				g.Requestor = req
				if !IsUserIdentity(req) && strings.ContainsAny(req, "<>@\\ \t\r\n") {
					errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("requestor: invalid identity %q", req)))
				}
			}
		}
	}

	if rawDraft, ok := fm["draft"]; ok {
		if b, ok := rawDraft.(bool); !ok {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "draft: must be a boolean"))
		} else {
			g.Draft = b
		}
	}

	if rawNB, ok := fm["not_before"]; ok {
		if nb, ok := rawNB.(string); !ok || nb == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "not_before: must be an RFC 3339 UTC timestamp or YYYY-MM-DD date string"))
		} else if t, err := ParseNotBefore(nb); err != nil {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("not_before: %v", err)))
		} else {
			g.NotBefore = t
			g.NotBeforeRaw = nb
		}
	}

	extractRefStrings := func(raw any, field string) []string {
		list, ok := raw.([]any)
		if !ok {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, field+": must be a list of strings"))
			return nil
		}
		var out []string
		for i, item := range list {
			s, ok := item.(string)
			if !ok || s == "" || strings.ContainsAny(s, " \t\r\n\\") {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("%s.%d: must be a non-empty reference string", field, i)))
				continue
			}
			out = append(out, s)
		}
		return out
	}

	if rawRefs, ok := fm["references"]; ok {
		g.References = extractRefStrings(rawRefs, "references")
	}
	if rawDeps, ok := fm["depends_on"]; ok && g.Kind == KindTask {
		g.DependsOn = extractRefStrings(rawDeps, "depends_on")
	}
	if rawFT, ok := fm["from_template"]; ok {
		ft, fErr := validateFromTemplate(rawFT, g.Kind, g.Path, where)
		if fErr != nil {
			errs = append(errs, *fErr)
		} else {
			g.FromTemplate = ft
		}
	}

	if g.Kind == KindProcess {
		if rawGate, ok := fm["gate"]; ok {
			if b, ok := rawGate.(bool); !ok {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "gate: must be a boolean"))
			} else {
				g.Gate = b
			}
		}
		_, hasWhen := fm["when"]
		if !hasWhen {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "when: required field missing on .process.grain.md"))
		} else {
			triggers, whenErrs := validateOnTriggers(fm["when"], where)
			errs = append(errs, whenErrs...)
			g.When = triggers
			g.On = triggers
		}
	}

	return errs
}

func cleanPosixPath(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

func cleanPosixDir(p string) string {
	return path.Dir(filepath.ToSlash(p))
}
