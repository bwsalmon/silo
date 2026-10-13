package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/client"
)

// The registry commands (local-registry.md, §3 "Operations", call
// L58). An image repo is named [silo/]name, as a git repo is, with a
// :tag or @digest after it where a command takes one. There is no
// create: a repo is made by its first push.

func (c *cli) registryList(ctx context.Context, args []string) error {
	fs := c.flags("registry list")
	recursive := fs.Bool("recursive", false, "list the descendants' image repos too")
	pos, err := c.parse(fs, args, 0, 1, "[--recursive] [silo]")
	if err != nil {
		return err
	}
	silo, err := c.api.RelativeSilo(ctx, arg(pos, 0, "self"))
	if err != nil {
		return err
	}
	rs, err := c.api.ImageRepos(ctx, silo, *recursive)
	if err != nil {
		return err
	}
	if rs == nil {
		rs = []*client.ImageRepo{}
	}
	return c.print(map[string]any{"image_repos": rs}, func(w *tw) { writeImageRepos(w, rs) })
}

// imageRepoArg reads a command's one [silo/]name argument, with no tag
// or digest.
func (c *cli) imageRepoArg(ctx context.Context, ref string) (silo, name string, err error) {
	silo, name, tag, digest, err := c.api.SplitImageRef(ctx, ref)
	if err == nil && (tag != "" || digest != "") {
		err = fmt.Errorf("image repo %q: want [silo/]name, without a tag or digest", ref)
	}
	if err != nil {
		return "", "", usageError{err.Error()}
	}
	return silo, name, nil
}

func (c *cli) registryGet(ctx context.Context, args []string) error {
	fs := c.flags("registry get")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>")
	if err != nil {
		return err
	}
	silo, name, err := c.imageRepoArg(ctx, pos[0])
	if err != nil {
		return err
	}
	r, err := c.api.ImageRepo(ctx, silo, name)
	if err != nil {
		return err
	}
	return c.print(r, func(w *tw) { writeImageRepo(w, r) })
}

func (c *cli) registryTags(ctx context.Context, args []string) error {
	fs := c.flags("registry tags")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>")
	if err != nil {
		return err
	}
	silo, name, err := c.imageRepoArg(ctx, pos[0])
	if err != nil {
		return err
	}
	ts, err := c.api.ImageTags(ctx, silo, name)
	if err != nil {
		return err
	}
	return c.print(ts, func(w *tw) {
		if len(ts.Manifests) == 0 {
			fmt.Fprintln(w, "no manifests")
			return
		}
		w.row("DIGEST", "TAGS", "SIZE", "MEDIA TYPE")
		for _, m := range ts.Manifests {
			tags := strings.Join(m.Tags, ",")
			if tags == "" {
				tags = "<untagged>"
			}
			w.row(m.Digest, tags, strconv.FormatInt(m.ChargedBytes, 10), m.MediaType)
		}
	})
}

// registryURL prints the name the caller pushes to and pulls from. It
// needs no repo to exist, since the first push makes it.
func (c *cli) registryURL(ctx context.Context, args []string) error {
	fs := c.flags("registry url")
	byID := fs.Bool("by-id", false, "the name by the silo's id, which survives renames and moves")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name> [--by-id]")
	if err != nil {
		return err
	}
	silo, name, err := c.imageRepoArg(ctx, pos[0])
	if err != nil {
		return err
	}
	s, err := c.api.Describe(ctx, silo, false)
	if err != nil {
		return err
	}
	u := client.ImageRef(s.Path, s.ID, name, *byID)
	return c.print(map[string]string{"url": u}, func(w *tw) { fmt.Fprintln(w, u) })
}

// registryDelete deletes a repo, one of its tags (name:tag), or a
// manifest and every tag on it (name@digest).
func (c *cli) registryDelete(ctx context.Context, args []string) error {
	fs := c.flags("registry delete")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name>[:tag|@digest]")
	if err != nil {
		return err
	}
	silo, name, tag, digest, err := c.api.SplitImageRef(ctx, pos[0])
	if err != nil {
		return usageError{err.Error()}
	}
	if tag != "" || digest != "" {
		var r *client.ImageRepo
		what := "tag " + tag
		if digest != "" {
			what = "manifest " + digest
			r, err = c.api.DeleteImageManifest(ctx, silo, name, digest, *m)
		} else {
			r, err = c.api.UntagImage(ctx, silo, name, tag, *m)
		}
		if err != nil {
			return err
		}
		return c.print(r, func(w *tw) {
			if m.ValidateOnly {
				fmt.Fprintf(w, "validate only: %s of %s would be deleted; nothing changed\n", what, pos[0])
				return
			}
			fmt.Fprintf(w, "deleted %s of %s\n", what, r.Ref)
			writeImageRepo(w, r)
		})
	}
	op, err := c.api.DeleteImageRepo(ctx, silo, name, *m)
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

// registryPrune deletes every manifest of a repo that no tag points at
// and no other manifest references (local-registry.md, §10).
func (c *cli) registryPrune(ctx context.Context, args []string) error {
	fs := c.flags("registry prune")
	m := mutation(fs)
	untagged := fs.Bool("untagged", false, "delete the manifests no tag points at (required)")
	pos, err := c.parse(fs, args, 1, 1, "[silo/]<name> --untagged")
	if err != nil {
		return err
	}
	if !*untagged {
		return usagef("registry prune: say what to prune: --untagged")
	}
	silo, name, err := c.imageRepoArg(ctx, pos[0])
	if err != nil {
		return err
	}
	ts, err := c.api.ImageTags(ctx, silo, name)
	if err != nil {
		return err
	}
	// An index's children are its to keep: one referenced by another
	// manifest is left, and goes with its index if that is pruned.
	var gone []string
	for _, mf := range ts.Manifests {
		if len(mf.Tags) > 0 || mf.Referenced {
			continue
		}
		if _, err := c.api.DeleteImageManifest(ctx, silo, name, mf.Digest, *m); err != nil {
			return err
		}
		gone = append(gone, mf.Digest)
	}
	return c.print(map[string]any{"deleted": gone, "validate_only": m.ValidateOnly}, func(w *tw) {
		verb := "deleted"
		if m.ValidateOnly {
			verb = "validate only: would delete"
		}
		if len(gone) == 0 {
			fmt.Fprintln(w, "no untagged manifests")
		}
		for _, d := range gone {
			fmt.Fprintf(w, "%s %s\n", verb, d)
		}
	})
}

func writeImageRepo(w *tw, r *client.ImageRepo) {
	w.row("Name:", r.Name)
	w.row("ID:", r.ID)
	w.row("Silo:", r.Silo)
	w.row("Ref:", r.Ref)
	w.row("By id:", r.RefByID)
	w.row("Tags:", strconv.Itoa(r.Tags))
	w.row("Manifests:", strconv.Itoa(r.Manifests))
	w.row("Charged:", strconv.FormatInt(r.ChargedBytes, 10)+" bytes")
	w.row("Created:", r.CreateTime.Format("2006-01-02 15:04:05Z07:00"))
	w.row("Last push:", r.LastPushTime.Format("2006-01-02 15:04:05Z07:00"))
}

func writeImageRepos(w *tw, rs []*client.ImageRepo) {
	if len(rs) == 0 {
		fmt.Fprintln(w, "no image repos")
		return
	}
	w.row("SILO", "NAME", "TAGS", "MANIFESTS", "CHARGED", "REF")
	for _, r := range rs {
		w.row(r.Silo, r.Name, strconv.Itoa(r.Tags), strconv.Itoa(r.Manifests), strconv.FormatInt(r.ChargedBytes, 10), r.Ref)
	}
}

// registryCacheStatus is silo registry cache status: the pull-through
// cache's configuration (local-registry.md, "Configuration"). The
// root's only.
func (c *cli) registryCacheStatus(ctx context.Context, args []string) error {
	fs := c.flags("registry cache status")
	if _, err := c.parse(fs, args, 0, 0, ""); err != nil {
		return err
	}
	st, err := c.api.RegistryCache(ctx)
	if err != nil {
		return err
	}
	return c.printCache(st)
}

// registryCacheSet is silo registry cache set: it changes what its
// flags name and keeps the rest. --upstream, given at all, replaces the
// whole list. The upstream list is not an allowlist: each silo's own
// egress still decides what it may get from the cache.
func (c *cli) registryCacheSet(ctx context.Context, args []string) error {
	fs := c.flags("registry cache set")
	var ups stringList
	fs.Var(&ups, "upstream", "name=endpoint[,alias...], repeated; replaces the upstream list")
	none := fs.Bool("no-upstreams", false, "configure no upstreams at all")
	stale := fs.String("stale-tag-max", "", "how long a tag is served when its upstream can't be reached (24h)")
	size := fs.Float64("size-limit-gb", -1, "the cache's size limit in GB")
	public := fs.String("public-ttl", "", "how long a public classification holds (24h)")
	auth := fs.String("auth-ttl", "", "how long a private pull's authorisation holds (300s)")
	if _, err := c.parse(fs, args, 0, 0, "[--upstream name=endpoint[,alias...]]... [--no-upstreams] [--stale-tag-max d] [--size-limit-gb n] [--public-ttl d] [--auth-ttl d]"); err != nil {
		return err
	}
	if *none && len(ups) > 0 {
		return usagef("--upstream and --no-upstreams don't go together")
	}
	st, err := c.api.RegistryCache(ctx)
	if err != nil {
		return err
	}
	if len(ups) > 0 || *none {
		st.Upstreams = []client.CacheUpstream{}
	}
	for _, u := range ups {
		name, rest, ok := strings.Cut(u, "=")
		if !ok || name == "" || rest == "" {
			return usagef("--upstream %q: want name=endpoint[,alias...]", u)
		}
		parts := strings.Split(rest, ",")
		st.Upstreams = append(st.Upstreams, client.CacheUpstream{Name: name, Endpoint: parts[0], Aliases: parts[1:]})
	}
	if *stale != "" {
		st.StaleTagMax = *stale
	}
	if *size >= 0 {
		st.SizeLimitGB = *size
	}
	if *public != "" {
		st.PublicTTL = *public
	}
	if *auth != "" {
		st.AuthTTL = *auth
	}
	out, err := c.api.SetRegistryCache(ctx, *st)
	if err != nil {
		return err
	}
	return c.printCache(out)
}

func (c *cli) printCache(st *client.CacheSettings) error {
	return c.print(st, func(w *tw) {
		if len(st.Upstreams) == 0 {
			w.row("Upstreams:", "none")
		}
		for i, u := range st.Upstreams {
			label := ""
			if i == 0 {
				label = "Upstreams:"
			}
			v := u.Name + " at " + u.Endpoint
			if len(u.Aliases) > 0 {
				v += " (also " + strings.Join(u.Aliases, ", ") + ")"
			}
			w.row(label, v)
		}
		w.row("Size limit:", strconv.FormatFloat(st.SizeLimitGB, 'f', -1, 64)+" GB")
		w.row("Stale tags:", "served for up to "+st.StaleTagMax+" when the upstream can't be reached")
		w.row("Public TTL:", st.PublicTTL)
		w.row("Auth TTL:", st.AuthTTL)
	})
}
