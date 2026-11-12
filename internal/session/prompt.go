package session

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
)

// Instructions go to the CLI in the router's initialize result.
func Instructions(fallback bool) string {
	s := "silo's tools are named <toolset>.<tool>. repo-read reads the repository; repo-write.propose_change proposes a commit " +
		"the person approves; scratch.define creates a one-off tool the person approves; silo.refresh pulls commits made outside the session."
	if fallback {
		s += " Tools added mid-session may not appear in your tool list: use silo.list_tools and silo.call to reach them."
	}
	return s
}

// ComposePrompt builds the system prompt: silo's preamble, the
// repository's .silo/prompt.md, and its .silo/context.
func ComposePrompt(co *gitrepo.Checkout, repo, branch string, rc *config.RepoConfig, fallback bool) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `You are working in a silo session on the context repository %s, checked out on branch %s.

You have no shell and no file-editing tools. Everything you do goes through the tools of the MCP server "silo"
(your CLI may show them with a prefix, such as mcp__silo__):

- repo-read.*: list, read and grep the repository's files, and read its git history (git_log, git_diff, git_show).
- repo-write.propose_change: propose a commit as file edits plus a commit message. The person sees the full diff and
  approves or rejects it; only then is it written, committed and pushed. A rejection comes back with their reason.
  Committed changes under .silo/tools/ are loaded as tool sets straight away.
- scratch.*: define a one-off tool set for this session (a manifest plus MCP server code). The person approves its code
  first. scratch.promote commits one to the repository.
- silo.refresh: pull in commits made outside this session and reload the tool sets they change.
- <toolset>.*: the repository's own tool sets, from .silo/tools/.
`, repo, branch)
	if fallback {
		b.WriteString("\nYour CLI may not pick up tools added during the session. silo.list_tools lists every current tool, and silo.call calls one by name.\n")
	}

	prompt, err := os.ReadFile(filepath.Join(co.Dir, config.PromptFile))
	switch {
	case err == nil:
		b.WriteString("\n# Objective\n\n")
		b.Write(bytes.TrimSpace(prompt))
		b.WriteString("\n")
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}

	ctxDir := filepath.Join(co.Dir, config.ContextDir)
	var paths []string
	filepath.WalkDir(ctxDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(co.Dir, p)
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if len(paths) == 0 {
		return b.String(), nil
	}
	b.WriteString("\n# Context\n")
	budget := rc.Context.MaxInlineBytes
	var listed []string
	for _, rel := range paths {
		if rc.Context.Mode != "inline" {
			listed = append(listed, rel)
			continue
		}
		data, err := os.ReadFile(filepath.Join(co.Dir, rel))
		if err != nil || len(data) > budget || bytes.IndexByte(data, 0) >= 0 {
			listed = append(listed, rel)
			continue
		}
		budget -= len(data)
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", rel, bytes.TrimSpace(data))
	}
	if len(listed) > 0 {
		b.WriteString("\nMore context files; read them with repo-read.read_file when they are relevant:\n")
		for _, rel := range listed {
			b.WriteString("- " + rel + "\n")
		}
	}
	return b.String(), nil
}
