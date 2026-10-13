package main

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/bwsalmon/silo/pkg/client"
)

// The repo commands (local-git.md, §2 "Operations", call L41). A repo
// is named [silo/]name; the silo is the caller's own unless named, and
// a path is relative to the caller's silo unless it starts with /.

func (c *cli) repoCreate(ctx context.Context, args []string) error {
	fs := c.flags("repo create")
	m := mutation(fs)
	branch := fs.String("default-branch", "", "the repo's default branch, its HEAD (default main, or the seed's)")
	seed := fs.String("seed", "", "a repo the caller may read, copied into the new one: /team-a/bar")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name> [--default-branch main] [--seed /team-a/bar]")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	req := client.RepoRequest{Name: name, DefaultBranch: *branch}
	if *seed != "" {
		req.Seed = &client.RepoSeed{Repo: *seed}
	}
	r, err := c.api.CreateRepo(ctx, silo, req, *m)
	if err != nil {
		return err
	}
	return c.print(r, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be created in %s; nothing changed\n", r.Name, r.Silo)
			return
		}
		fmt.Fprintf(w, "created %s in %s\n", r.Name, r.Silo)
		writeRepo(w, r)
	})
}

func (c *cli) repoList(ctx context.Context, args []string) error {
	fs := c.flags("repo list")
	recursive := fs.Bool("recursive", false, "list the descendants' repos too")
	pos, err := c.parse(fs, args, 0, 1, "[--recursive] [silo]")
	if err != nil {
		return err
	}
	silo, err := c.api.RelativeSilo(ctx, arg(pos, 0, "self"))
	if err != nil {
		return err
	}
	rs, err := c.api.Repos(ctx, silo, *recursive)
	if err != nil {
		return err
	}
	if rs == nil {
		rs = []*client.Repo{}
	}
	return c.print(map[string]any{"repos": rs}, func(w *tw) { writeRepos(w, rs) })
}

func (c *cli) repoGet(ctx context.Context, args []string) error {
	fs := c.flags("repo get")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	r, err := c.api.Repo(ctx, silo, name)
	if err != nil {
		return err
	}
	return c.print(r, func(w *tw) { writeRepo(w, r) })
}

func (c *cli) repoDelete(ctx context.Context, args []string) error {
	fs := c.flags("repo delete")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	op, err := c.api.DeleteRepo(ctx, silo, name, *m)
	if err != nil {
		return err
	}
	if m.ValidateOnly {
		return c.print(map[string]any{"validate_only": true}, func(w *tw) {
			fmt.Fprintf(w, "validate only: %s would be deleted; nothing changed\n", pos[0])
		})
	}
	if !op.Done && op.State != "DELETE_FAILED" {
		if op, err = c.api.WaitOperation(ctx, op.ID, c.poll); err != nil {
			return fmt.Errorf("deleting %s: %w", pos[0], err)
		}
	}
	return c.print(op, func(w *tw) {
		if op.Done {
			fmt.Fprintf(w, "deleted %s\n", op.Target)
		} else {
			fmt.Fprintf(w, "deleting %s: operation %s is %s\n", op.Target, op.ID, op.State)
		}
	})
}

// repoURL prints the URL the caller clones the repo by: the in-silo
// URL inside a silo, and git-remote-silo's silo:: form through the
// front door, on the host (local-git.md, "Where it listens").
func (c *cli) repoURL(ctx context.Context, args []string) error {
	fs := c.flags("repo url")
	byID := fs.Bool("by-id", false, "the URL by the silo's id, which survives renames and moves")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name> [--by-id]")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	r, err := c.api.Repo(ctx, silo, name)
	if err != nil {
		return err
	}
	u := r.CloneURL
	switch {
	case c.door != nil && *byID:
		s, err := c.api.Describe(ctx, r.Silo, false)
		if err != nil {
			return err
		}
		u = "silo::-" + s.ID + "/" + r.Name
	case c.door != nil:
		u = "silo::" + path.Join(r.Silo, r.Name)
	case *byID:
		u = r.CloneURLByID
	}
	return c.print(map[string]string{"url": u}, func(w *tw) { fmt.Fprintln(w, u) })
}

func writeRepo(w *tw, r *client.Repo) {
	w.row("Name:", r.Name)
	w.row("ID:", r.ID)
	w.row("Silo:", r.Silo)
	w.row("Default branch:", r.DefaultBranch)
	w.row("Clone URL:", r.CloneURL)
	w.row("By id:", r.CloneURLByID)
	w.row("Size:", strconv.FormatInt(r.SizeBytes, 10)+" bytes")
	w.row("Mirror:", mirrorState(r.Mirror))
	w.row("Created:", r.CreateTime.Format("2006-01-02 15:04:05Z07:00"))
}

func mirrorState(m client.RepoMirror) string {
	switch {
	case m.From != "":
		return m.State + " (repos.mirror off from " + m.From + ")"
	case m.ExternalPrefix != "":
		return m.State + " (" + m.ExternalPrefix + ")"
	}
	return m.State
}

func writeRepos(w *tw, rs []*client.Repo) {
	if len(rs) == 0 {
		fmt.Fprintln(w, "no repos")
		return
	}
	w.row("SILO", "NAME", "DEFAULT BRANCH", "SIZE", "MIRROR", "CLONE URL")
	for _, r := range rs {
		w.row(r.Silo, r.Name, r.DefaultBranch, strconv.FormatInt(r.SizeBytes, 10), r.Mirror.State, r.CloneURL)
	}
}
