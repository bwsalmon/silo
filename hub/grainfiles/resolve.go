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
	anchorRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	urlSchemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	headingRe   = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	nonSlugRe   = regexp.MustCompile(`[^\w\s-]`)
	spaceSlugRe = regexp.MustCompile(`\s`)
)

// CanonicalRepoURL returns the canonical base URL of a git repository
// (`https://<lower-host>/<repo-path>`, with default port `:443`, trailing `/`,
// and trailing `.git` stripped; docs/grainfiles-spec.md §8.5).
func CanonicalRepoURL(rawURL string) (string, error) {
	if rawURL == "" {
		return "", newFinding(CodeMalformedRef, "", "empty repository URL")
	}
	if strings.HasPrefix(rawURL, "file://") {
		rest := strings.Trim(rawURL[len("file://"):], "/")
		if rest == "" || strings.Contains(rest, "//") {
			return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", rawURL))
		}
		segs := strings.Split(rest, "/")
		repoName := strings.TrimSuffix(segs[0], ".git")
		return "file://" + repoName, nil
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("only https:// (and file:// in tests) URLs are allowed: %q", rawURL))
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("malformed URL %q: %v", rawURL, err))
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("credentials are not allowed in reference URLs: %q", rawURL))
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
	trimmedPath = strings.TrimSuffix(trimmedPath, ".git")
	if trimmedPath == "" || trimmedPath == "/" {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", rawURL))
	}
	cleaned := cleanPosixPath(strings.TrimPrefix(trimmedPath, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", newFinding(CodeMalformedRef, "", fmt.Sprintf("invalid repository path: %q", rawURL))
	}
	return "https://" + host + "/" + cleaned, nil
}

// ParseRef parses a reference string according to docs/grainfiles-spec.md §8.1.
func ParseRef(s string) (Ref, error) {
	if s == "" || strings.ContainsAny(s, " \t\r\n\\") {
		return Ref{}, newFinding(CodeMalformedRef, "", "a reference must be a non-empty string without whitespace or backslashes")
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
	if hostname == "" {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("missing host in reference URL: %q", s))
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("no repository path: %q", s))
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
		if dashIdx == 0 || dashIdx+2 >= len(segs) || (segs[dashIdx+1] != "blob" && segs[dashIdx+1] != "tree") {
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
			if strings.HasSuffix(seg, ".git") {
				return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("sub-path in repository URL must use /blob/<ref>/ or /tree/<ref>/: %q", s))
			}
		}
		repoSegs = segs
		forge = "github"
	}

	if mode == "blob" && (len(subSegs) == 0 || trailing) {
		return Ref{}, newFinding(CodeMalformedRef, "", fmt.Sprintf("blob URL must name a file path: %q", s))
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
		if normSub == ".." || strings.HasPrefix(normSub, "../") {
			return Ref{}, newFinding(CodeEscapesRepo, "", fmt.Sprintf("%q climbs above the repository root", s))
		}
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
		if normSub == ".." || strings.HasPrefix(normSub, "../") {
			return Ref{}, newFinding(CodeEscapesRepo, "", fmt.Sprintf("%q climbs above the repository root", s))
		}
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

	// Resolve recipient (always a repository or folder deployment identity).
	resolvedRecipient, _, err := ResolveRefWithSource(Source{
		RepoURL: canonRepo,
		Ref:     urlRef,
		Commit:  src.Commit,
		Path:    cleanFile,
	}, g.Recipient, "tree")
	if err != nil {
		return wrapFieldError("recipient", cleanFile, err)
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

	// Resolve trigger_with (always grainfiles -> "blob").
	var resolvedTW []string
	if len(g.TriggerWith) > 0 {
		resolvedTW = make([]string, len(g.TriggerWith))
		for i, raw := range g.TriggerWith {
			u, _, err := ResolveRefWithSource(Source{
				RepoURL: canonRepo,
				Ref:     urlRef,
				Commit:  src.Commit,
				Path:    cleanFile,
			}, raw, "blob")
			if err != nil {
				return wrapFieldError("trigger_with", cleanFile, err)
			}
			resolvedTW[i] = u
		}
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
	g.Recipient = resolvedRecipient
	g.Requestor = resolvedRequestor
	g.References = resolvedRefs
	g.DependsOn = resolvedDeps
	g.TriggerWith = resolvedTW
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
			suf := "." + string(k) + ".md"
			if strings.HasSuffix(r.Path, suf) || strings.HasSuffix(r.Repo, suf) {
				return "", Ref{}, newFinding(CodeWrongTargetKind, src.Path, fmt.Sprintf("%q is a %s grainfile, expected a repository or folder", rawRef, k))
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

// IsSameRepo reports whether repoA and repoB refer to the same canonical
// repository (docs/grainfiles-spec.md §8.5; docs/hub.md §6.4.1).
func IsSameRepo(repoA, repoB string) bool {
	ca, errA := canonicalRepoFromAnyRef(repoA)
	cb, errB := canonicalRepoFromAnyRef(repoB)
	if errA != nil || errB != nil || ca == "" || cb == "" {
		return false
	}
	return ca == cb
}

func canonicalRepoFromAnyRef(s string) (string, error) {
	if r, err := ParseRef(s); err == nil && r.Form == RefRemote && r.Repo != "" {
		return r.Repo, nil
	}
	return CanonicalRepoURL(s)
}

// AuthorizedReferences partitions g's `references` into:
//   - sameRepo: references targeting sourceRepoURL (which a non-draft Grainfile
//     in sourceRepoURL is authorized to grant `recipient` read access to through
//     Grain Hub; docs/hub.md §6.4.1, docs/hub-interface.md).
//   - crossRepo: references targeting any other repository (which a Grainfile in
//     sourceRepoURL cannot authorize on its own: "a repository can only grant
//     access to itself").
//
// If any reference in g.References is still repo-local (`/`-rooted or relative),
// it is resolved against sourceRepoURL using g.Ref, g.Commit, and g.Path.
func (g *Grainfile) AuthorizedReferences(sourceRepoURL string) (sameRepo, crossRepo []string) {
	if g == nil || len(g.References) == 0 {
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
		if r.Repo == canonSource {
			sameRepo = append(sameRepo, u)
		} else {
			crossRepo = append(crossRepo, u)
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
//  3. requestedRef must match an entry in g.References (exact file path,
//     a path under a `/tree/` folder reference in g.References, or any path in
//     sourceRepoURL when g.References includes sourceRepoURL's root).
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
	if target.Repo != canonSource {
		return Ref{}, fmt.Errorf("cross-origin reference not authorized by source repository")
	}

	targetPath := cleanPosixPath(strings.Trim(target.Path, "/"))
	if targetPath == "." {
		targetPath = ""
	}

	for _, raw := range g.References {
		_, allowed, err := ResolveRefWithSource(Source{
			RepoURL: canonSource,
			Ref:     urlRef,
			Commit:  g.Commit,
			Path:    g.Path,
		}, raw, "blob")
		if err != nil || allowed.Repo != canonSource {
			continue
		}
		allowedPath := cleanPosixPath(strings.Trim(allowed.Path, "/"))
		if allowedPath == "." {
			allowedPath = ""
		}
		// Repository root grant covers everything in canonSource.
		if allowedPath == "" && allowed.Mode != "blob" {
			return target, nil
		}
		// Exact file or folder match (ignoring #anchor).
		if allowedPath == targetPath {
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

			kind, stem, isGrain, warnCode := ClassifyPath(rel)
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
			fm, body, fErr := splitAndParseFrontMatter(where, raw)
			if fErr != nil {
				findings = append(findings, *fErr)
				continue
			}
			it.ok = true
			gf := &Grainfile{
				Kind: kind,
				Name: stem,
				Path: rel,
				Body: body,
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
		for _, s := range gf.TriggerWith {
			refs = append(refs, fieldRef{"trigger_with", s, true})
		}

		for _, fr := range refs {
			tKind, tItem, fErr := resolveInTree(it, fr.ref, fr.field)
			if fErr != nil {
				fErr.Path = where
				findings = append(findings, *fErr)
				continue
			}
			if fr.want {
				if tItem != nil {
					if !tItem.kind.Valid() {
						findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is not a task or process", fr.field, fr.ref)))
					}
				} else if tKind == "asset" || tKind == "folder" {
					findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is a %s, expected task or process", fr.field, fr.ref, tKind)))
				} else if tKind == "external" {
					r, _ := ParseRef(fr.ref)
					if r.Mode == "tree" || (!strings.HasSuffix(r.Path, ".task.md") && !strings.HasSuffix(r.Path, ".process.md")) {
						findings = append(findings, newFinding(CodeWrongTargetKind, where, fmt.Sprintf("%s: %q is not a task or process grainfile", fr.field, fr.ref)))
					}
				}
			}
			if tItem != nil {
				it.targets[fr.field] = append(it.targets[fr.field], tItem)
			}
		}
	}

	// Cycle detection in depends_on and trigger_with (§8.4).
	for _, field := range []string{"depends_on", "trigger_with"} {
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

	if rawTitle, ok := fm["title"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: required field missing"))
	} else if title, ok := rawTitle.(string); !ok || title == "" {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title: must be a non-empty string"))
	} else if strings.ContainsAny(title, "\r\n") {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "title must be a single line"))
	} else {
		g.Title = title
	}

	if rawRec, ok := fm["recipient"]; !ok {
		errs = append(errs, newFinding(CodeSchemaInvalid, where, "recipient: required field missing"))
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

	if rawDraft, ok := fm["draft"]; ok {
		if b, ok := rawDraft.(bool); !ok {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "draft: must be a boolean"))
		} else {
			g.Draft = b
		}
	}

	if rawCA, ok := fm["created_at"]; ok {
		if ca, ok := rawCA.(string); !ok || ca == "" {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "created_at: must be an RFC 3339 UTC timestamp string"))
		} else if t, err := ParseTime(ca); err != nil {
			errs = append(errs, newFinding(CodeSchemaInvalid, where, fmt.Sprintf("created_at: %v", err)))
		} else {
			g.CreatedAt = t
			g.CreatedAtRaw = ca
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
	if rawDeps, ok := fm["depends_on"]; ok {
		g.DependsOn = extractRefStrings(rawDeps, "depends_on")
	}

	_, hasTriggerWith := fm["trigger_with"]
	if hasTriggerWith {
		switch v := fm["trigger_with"].(type) {
		case string:
			if v == "" || strings.ContainsAny(v, " \t\r\n\\") {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be a valid reference"))
			} else {
				g.TriggerWith = []string{v}
			}
		case []any:
			if len(v) == 0 {
				errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be non-empty"))
			} else {
				g.TriggerWith = extractRefStrings(v, "trigger_with")
			}
		default:
			errs = append(errs, newFinding(CodeSchemaInvalid, where, "trigger_with: must be a string or non-empty list"))
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

func cleanPosixPath(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

func cleanPosixDir(p string) string {
	return path.Dir(filepath.ToSlash(p))
}
