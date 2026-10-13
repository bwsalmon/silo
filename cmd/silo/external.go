package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
)

// Mapping back (local-git.md, §9): where a repo is in the uber repo,
// which repo an uber-repo ref is, and a clone from the uber repo whose
// submodules work outside Silo (§6, "External view: not rewritten").

// siloGit is where every in-silo submodule URL starts. Outside Silo it
// means nothing, but every commit such a URL names is in the uber repo.
const siloGit = "http://silo.internal/git/"

func (c *cli) repoExternal(ctx context.Context, args []string) error {
	fs := c.flags("repo external")
	branch := fs.String("branch", "", "print this one branch's uber-repo ref")
	tag := fs.String("tag", "", "print this one tag's uber-repo ref")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name> [--branch b | --tag t]")
	if err != nil {
		return err
	}
	if *branch != "" && *tag != "" {
		return usagef("--branch and --tag: name one ref")
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	x, err := c.api.RepoExternal(ctx, silo, name, *branch, *tag)
	if err != nil {
		return err
	}
	return c.print(x, func(w *tw) {
		if x.URL == "" {
			w.row("silo", x.Silo+" ("+x.SiloID+")")
			w.row("mirror", externalState(x))
			return
		}
		if x.Ref != nil {
			w.row("repository", x.URL)
			w.row("local", x.Ref.Local)
			w.row("external", x.Ref.External)
			if x.Ref.Commit != "" {
				w.row("commit", x.Ref.Commit)
			} else {
				w.row("commit", "(not mirrored)")
			}
			w.row("mirror", externalState(x))
			w.row("clone", cloneLine(x, shortRef(x.Ref.External)))
			return
		}
		w.row("repository", x.URL)
		w.row("silo", x.Silo+" ("+x.SiloID+")")
		w.row("branches", x.Branches+"*")
		w.row("default", x.DefaultBranch)
		w.row("tags", x.Tags+"*")
		w.row("archives", fmt.Sprintf("%s*   (%d)", x.Archives, x.ArchiveRefs))
		if x.Index != "" {
			w.row("index", x.Index)
		}
		w.row("mirror", externalState(x))
		w.row("clone", cloneLine(x, x.DefaultBranch))
	})
}

func externalState(x *client.RepoExternal) string {
	switch x.State {
	case "OFF":
		return "off (by " + x.From + ")"
	case "UNCONFIGURED":
		return "UNCONFIGURED: no uber repo is set (silo mirror set)"
	}
	s := x.State
	if x.MirroredTime != nil {
		s += " at " + x.MirroredTime.UTC().Format(time.RFC3339)
	}
	if x.LastError != "" {
		s += ": " + x.LastError
	}
	return s
}

func shortRef(ref string) string {
	if s, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return s
	}
	return strings.TrimPrefix(ref, "refs/tags/")
}

func cloneLine(x *client.RepoExternal, branch string) string {
	return "git clone --single-branch -b " + branch + " " + x.URL + " " + path.Base(x.Repo)
}

func (c *cli) repoWhich(ctx context.Context, args []string) error {
	fs := c.flags("repo which")
	pos, err := c.parse(fs, args, 1, 1, "<uber-repo ref>")
	if err != nil {
		return err
	}
	w, err := c.api.WhichRef(ctx, pos[0])
	if err != nil {
		return err
	}
	return c.print(w, func(t *tw) {
		t.row("ref", w.Ref)
		repo := w.Repo
		if !w.Exists {
			repo += "   (no longer there)"
		}
		t.row("repo", repo)
		silo := w.Silo
		switch {
		case w.SiloID != "" && w.From == "index":
			silo += " (" + w.SiloID + ", from the index)"
		case w.SiloID != "":
			silo += " (" + w.SiloID + ")"
		}
		t.row("silo", silo)
		switch w.Kind {
		case "branch":
			t.row("branch", strings.TrimPrefix(w.Local, "refs/heads/"))
		case "tag":
			t.row("tag", strings.TrimPrefix(w.Local, "refs/tags/"))
		case "archive":
			if w.Local != "" {
				t.row("archive", w.Local)
			} else {
				t.row("archive", "an uber-repo tip silo mirror adopt replaced")
			}
		case "index":
			t.row("index", "its gitlink, at the tip of its default branch "+strings.TrimPrefix(w.Local, "refs/heads/"))
		}
		if w.Commit != "" {
			t.row("commit", w.Commit)
		}
	})
}

// repoExternalClone is silo repo external-clone: the repo's default
// branch (or --branch) cloned from the uber repo, with every submodule
// that points at a silo repo pointed at the uber repo instead, all the
// way down. It needs the caller's own access to the uber repo.
func (c *cli) repoExternalClone(ctx context.Context, args []string) error {
	fs := c.flags("repo external-clone")
	branch := fs.String("branch", "", "clone this branch rather than the default one")
	uber := fs.String("url", "", "clone the uber repo from here (default the URL silo mirror set was given)")
	pos, err := c.parse(fs, args, 1, 2, "[silo/]<name> [directory] [--branch b] [--url u]")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	x, err := c.api.RepoExternal(ctx, silo, name, *branch, "")
	if err != nil {
		return err
	}
	if x.URL == "" {
		return fmt.Errorf("%s isn't in an uber repo: mirror %s", x.Repo, externalState(x))
	}
	b := x.DefaultBranch
	if x.Ref != nil {
		b = shortRef(x.Ref.External)
	}
	u := x.URL
	if *uber != "" {
		u = *uber
	}
	dir := arg(pos, 1, path.Base(x.Repo))
	if err := c.git(ctx, "", "clone", "--single-branch", "-b", b, u, dir); err != nil {
		return err
	}
	n, err := c.externalSubmodules(ctx, dir, u)
	if err != nil {
		return err
	}
	return c.print(map[string]any{"directory": dir, "branch": b, "url": u, "submodules": n}, func(w *tw) {
		fmt.Fprintf(w, "cloned %s from %s into %s, with %d submodules from the uber repo\n", b, u, dir, n)
	})
}

// externalSubmodules points dir's submodules whose URLs are silo repos
// at uber, initializes them, and does the same in each, recursively,
// as the index's README tells someone without silo to. It returns how
// many submodules it pointed at uber.
func (c *cli) externalSubmodules(ctx context.Context, dir, uber string) (int, error) {
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	urls, err := gitConfigs(ctx, dir, `^submodule\..*\.url$`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, kv := range urls {
		if strings.HasPrefix(kv[1], siloGit) {
			if err := c.git(ctx, dir, "config", kv[0], uber); err != nil {
				return n, err
			}
			n++
		}
	}
	if len(urls) == 0 {
		return 0, nil
	}
	if err := c.git(ctx, dir, "submodule", "update", "--init", "--single-branch"); err != nil {
		return n, err
	}
	paths, err := gitConfigs(ctx, dir, `^submodule\..*\.path$`)
	if err != nil {
		return n, err
	}
	for _, kv := range paths {
		m, err := c.externalSubmodules(ctx, filepath.Join(dir, kv[1]), uber)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// git runs git in dir, with its output on stderr.
func (c *cli) git(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, c.stderr, c.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// gitConfigs is .gitmodules' keys matching re, and their values.
func gitConfigs(ctx context.Context, dir, re string) ([][2]string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "-f", ".gitmodules", "--get-regexp", re)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, nil // none
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s/.gitmodules: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var kvs [][2]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			kvs = append(kvs, [2]string{k, v})
		}
	}
	return kvs, nil
}
