package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// git-remote-silo and the repo commands that work a repo's refs
// (local-git.md, "Where it listens", call L42, and "Local archive
// refs", call L48).
//
// On the host, git can't speak HTTP over the front door, so silo ships
// git-remote-silo, a git remote helper: git clone silo::/team-a/x/foo
// runs it. It is this binary, run under that name (a link on $PATH).
// It reaches the fence API as every other command does, and speaks
// git's own protocol over one connection through the helper's connect
// capability.

// RemoteHelper is the name git runs silo by for silo:: URLs.
const RemoteHelper = "git-remote-silo"

// gitRemote is git-remote-silo <remote> <url>: url is a repo's path,
// absolute, relative to the caller's silo, or by id (-<silo id>/name),
// as git passes it with silo:: taken off.
func (c *cli) gitRemote(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usagef("usage: %s <remote> [<url>]", RemoteHelper)
	}
	// git's arguments aren't flags: a URL by id starts with "-".
	if _, err := c.parse(c.flags(RemoteHelper), nil, 0, 0, ""); err != nil {
		return err
	}
	u := strings.TrimPrefix(args[len(args)-1], "silo::")
	silo, name, err := c.api.SplitRepo(ctx, u)
	if err != nil {
		return usageError{err.Error()}
	}
	in := bufio.NewReader(c.stdin)
	for {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			if err == io.EOF {
				return nil
			}
			return err
		}
		cmd := strings.TrimSpace(line)
		switch {
		case cmd == "":
			return nil
		case cmd == "capabilities":
			fmt.Fprint(c.stdout, "connect\n\n")
		case strings.HasPrefix(cmd, "connect "):
			service := strings.TrimPrefix(cmd, "connect ")
			conn, err := c.api.GitConnect(ctx, silo, name, service, c.getenv("GIT_PROTOCOL"))
			if err != nil {
				return err
			}
			defer conn.Close()
			fmt.Fprint(c.stdout, "\n")
			go func() {
				io.Copy(conn, in)
			}()
			_, err = io.Copy(c.stdout, conn)
			return err
		default:
			return fmt.Errorf("git asked for %q, which %s doesn't do", cmd, RemoteHelper)
		}
	}
}

// repoClone is silo repo clone: git clone silo::<path>, with
// git-remote-silo found on $PATH, or this binary under that name if it
// isn't there.
func (c *cli) repoClone(ctx context.Context, args []string) error {
	fs := c.flags("repo clone")
	pos, err := c.parse(fs, args, 1, 2, "[silo/]<name> [directory]")
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
	env := append(os.Environ(), "SILO_ENDPOINT="+c.endpoint)
	if _, err := exec.LookPath(RemoteHelper); err != nil {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "silo-git")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := os.Symlink(exe, filepath.Join(dir, RemoteHelper)); err != nil {
			return err
		}
		env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		fmt.Fprintf(c.stderr, "silo: %s is not on $PATH: link it to %s for fetches and pushes after this clone\n", RemoteHelper, exe)
	}
	gargs := []string{"clone", "silo::" + path.Join(r.Silo, r.Name)}
	if len(pos) == 2 {
		gargs = append(gargs, pos[1])
	}
	cmd := exec.CommandContext(ctx, "git", gargs...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, c.stdin, c.stdout, c.stderr
	return cmd.Run()
}

// repoArchives is silo repo archives: a repo's local archive refs.
func (c *cli) repoArchives(ctx context.Context, args []string) error {
	fs := c.flags("repo archives")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	as, err := c.api.RepoArchives(ctx, silo, name)
	if err != nil {
		return err
	}
	return c.print(map[string]any{"archives": as}, func(w *tw) {
		if len(as) == 0 {
			fmt.Fprintln(w, "no archive refs")
			return
		}
		w.row("ARCHIVE", "REF", "COMMIT", "TIME")
		for _, a := range as {
			w.row(a.Name, a.Ref, a.Commit, a.Time.Format(time.RFC3339))
		}
	})
}

// repoRestoreRef is silo repo restore-ref: a new branch from an
// archive ref.
func (c *cli) repoRestoreRef(ctx context.Context, args []string) error {
	fs := c.flags("repo restore-ref")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 3, 3, "[silo/]<name> <archive> <branch>")
	if err != nil {
		return err
	}
	silo, name, err := c.api.SplitRepo(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	r, err := c.api.RestoreRef(ctx, silo, name, pos[1], pos[2], *m)
	if err != nil {
		return err
	}
	return c.print(r, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would get branch %s from %s; nothing changed\n", r.Repo, r.Branch, r.Archive)
			return
		}
		fmt.Fprintf(w, "made branch %s of %s at %s, from %s\n", r.Branch, r.Repo, r.Commit, r.Archive)
	})
}
