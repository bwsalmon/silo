package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/bwsalmon/silo/pkg/client"
)

// The silo mirror commands: the outbound mirror into the uber repo
// (docs/silo/local-git.md, §8). All are the root's.

// mirrorSet is silo mirror set --url u --credential-file f.
func (c *cli) mirrorSet(ctx context.Context, args []string) error {
	fs := c.flags("mirror set")
	u := fs.String("url", "", "the uber repo: https://... with a token, or an ssh URL with a deploy key")
	cred := fs.String("credential-file", "", "an SSH deploy key with write access, or a file holding an HTTPS token")
	prefix := fs.String("prefix", "", "the first component of every branch silod pushes (default silo)")
	index := fs.String("index-branch", "", "the submodule index's branch (default main)")
	if _, err := c.parse(fs, args, 0, 0, "--url u --credential-file f [--prefix p] [--index-branch b]"); err != nil {
		return err
	}
	if *u == "" || *cred == "" {
		return usagef("silo mirror set needs --url and --credential-file")
	}
	b, err := os.ReadFile(*cred)
	if err != nil {
		return err
	}
	st, err := c.api.SetMirror(ctx, client.MirrorSettings{URL: *u, Credential: string(b), Prefix: *prefix, IndexBranch: *index})
	if err != nil {
		return err
	}
	return c.printMirror(st)
}

// mirrorStatus is silo mirror status [--all].
func (c *cli) mirrorStatus(ctx context.Context, args []string) error {
	fs := c.flags("mirror status")
	all := fs.Bool("all", false, "list every repo, not only those out of sync")
	if _, err := c.parse(fs, args, 0, 0, "[--all]"); err != nil {
		return err
	}
	st, err := c.api.MirrorStatus(ctx, *all)
	if err != nil {
		return err
	}
	return c.printMirror(st)
}

// mirrorRetry is silo mirror retry [repo].
func (c *cli) mirrorRetry(ctx context.Context, args []string) error {
	fs := c.flags("mirror retry")
	pos, err := c.parse(fs, args, 0, 1, "[[silo/]<repo>]")
	if err != nil {
		return err
	}
	repo := ""
	if len(pos) == 1 {
		if repo, err = c.mirrorRepo(ctx, pos[0]); err != nil {
			return err
		}
	}
	st, err := c.api.RetryMirror(ctx, repo)
	if err != nil {
		return err
	}
	return c.printMirror(st)
}

// mirrorAdopt is silo mirror adopt <repo> [--ref r]... [--keep-external].
func (c *cli) mirrorAdopt(ctx context.Context, args []string) error {
	fs := c.flags("mirror adopt")
	var refs stringList
	fs.Var(&refs, "ref", "a diverged ref to adopt, by branch or ref name (repeatable; default all)")
	keep := fs.Bool("keep-external", false, "record the uber repo's tips as mirrored instead of archiving and overwriting them")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<repo> [--ref r]... [--keep-external]")
	if err != nil {
		return err
	}
	repo, err := c.mirrorRepo(ctx, pos[0])
	if err != nil {
		return err
	}
	st, err := c.api.AdoptMirror(ctx, repo, refs, *keep)
	if err != nil {
		return err
	}
	return c.printMirror(st)
}

// mirrorRepo is a repo reference as the mirror's calls take it: its
// silo's path and its name.
func (c *cli) mirrorRepo(ctx context.Context, ref string) (string, error) {
	silo, name, err := c.api.SplitRepo(ctx, ref)
	if err != nil {
		return "", usageError{err.Error()}
	}
	if silo == "self" {
		silo = "/"
	}
	return path.Join(silo, name), nil
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func (c *cli) printMirror(st *client.MirrorStatus) error {
	return c.print(st, func(w *tw) {
		if !st.Configured {
			w.row("Mirror:", "not set (silo mirror set)")
			return
		}
		w.row("Mirror:", st.URL)
		w.row("Prefix:", st.Prefix)
		w.row("Index branch:", st.IndexBranch)
		w.row("Credential:", st.Credential)
		if st.Paused != "" {
			w.row("Paused:", st.Paused)
		}
		w.row("Refs:", fmt.Sprint(st.Refs))
		w.row("Estimated size:", fmt.Sprintf("%d bytes", st.EstimatedBytes))
		w.row("In sync:", fmt.Sprint(st.InSync))
		if st.PendingDeletes > 0 {
			w.row("Pending deletes:", fmt.Sprint(st.PendingDeletes))
		}
		if len(st.Repos) == 0 {
			return
		}
		w.row("")
		w.row("REPO", "STATE", "LAG", "ERROR")
		for _, r := range st.Repos {
			lag := ""
			if r.LagSeconds > 0 {
				lag = fmt.Sprintf("%ds", r.LagSeconds)
			}
			w.row(r.Path, r.State, lag, r.LastError)
			for _, d := range r.Diverged {
				w.row("  "+d.Ref, "DIVERGED", "", fmt.Sprintf("expected %s, uber repo has %s", short(d.Expected), short(d.Actual)))
			}
		}
	})
}

func short(sha string) string {
	if sha == "" {
		return "nothing"
	}
	return sha[:min(len(sha), 12)]
}
