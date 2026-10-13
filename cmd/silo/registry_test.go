package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/client"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string([]byte{c}), 64) }

// push records a push into silo's image repo name as the registry
// does: the repo on its first push, then each manifest, tagged where
// a tag is given.
func (d *silod) push(silo, name string, ms map[string]*imagerepos.Manifest) {
	d.t.Helper()
	ctx := context.Background()
	err := d.db.Tx(ctx, func(tx store.Tx) error {
		r, err := imagerepos.ByName(ctx, tx, silo, name)
		if err == imagerepos.ErrNotFound {
			chain, err := tree.Ancestry(ctx, tx, silo)
			if err != nil {
				return err
			}
			if r, err = fenceapi.CreateImageRepo(ctx, tx, chain, fenceapi.Caller{Silo: silo, Via: fenceapi.ViaOrigin}, name); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		for tag, m := range ms {
			m.Repo = r.ID
			if err := imagerepos.PutManifest(ctx, tx, m); err != nil {
				return err
			}
			if !strings.HasPrefix(tag, "-") {
				if err := imagerepos.SetTag(ctx, tx, r.ID, tag, m.Digest); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		d.t.Fatal(err)
	}
}

// silo registry list|get|tags|url|delete|prune (local-registry.md, §3
// "Operations").
func TestRegistryCommands(t *testing.T) {
	d := startSilod(t)
	var a client.Silo
	d.json(d.root, &a, "create", "/team-a", "--address", "/16", "--quota", "image_repos=5")
	var x client.Silo
	d.json(d.root, &x, "create", "/team-a/x", "--address", "/24", "--quota", "global:image_repos=2")

	// url needs no repo: the first push makes it.
	if got := strings.TrimSpace(d.ok(x.ID, "registry", "url", "app")); got != "registry.silo.internal/team-a/x/app" {
		t.Errorf("registry url = %q", got)
	}
	if got := strings.TrimSpace(d.ok(a.ID, "registry", "url", "x/app", "--by-id")); got != "registry.silo.internal/id_"+x.ID+"/app" {
		t.Errorf("registry url --by-id = %q", got)
	}
	if got := strings.TrimSpace(d.ok(d.root, "registry", "url", "/app")); got != "registry.silo.internal/app" {
		t.Errorf("registry url of the root = %q", got)
	}
	contains(t, "empty list", d.ok(x.ID, "registry", "list"), "no image repos")

	layer := imagerepos.Blob{Digest: digestOf('a'), Size: 1000}
	img := func(c byte) *imagerepos.Manifest {
		return &imagerepos.Manifest{Digest: digestOf(c), MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 100,
			Refs: []imagerepos.Blob{layer}}
	}
	child := img('2')
	index := &imagerepos.Manifest{Digest: digestOf('3'), MediaType: "application/vnd.oci.image.index.v1+json", Size: 50,
		Refs: []imagerepos.Blob{{Digest: child.Digest, Size: 100}}}
	d.push(x.ID, "app", map[string]*imagerepos.Manifest{"1": img('1'), "multi": index, "-child": child, "-old": img('4')})
	d.push(a.ID, "base", map[string]*imagerepos.Manifest{"1": img('5')})

	contains(t, "registry list", d.ok(a.ID, "registry", "list", "--recursive"), "/team-a ", "base", "/team-a/x", "app",
		"registry.silo.internal/team-a/x/app")
	var l struct {
		ImageRepos []client.ImageRepo `json:"image_repos"`
	}
	d.json(d.root, &l, "registry", "list", "/team-a/x")
	if len(l.ImageRepos) != 1 || l.ImageRepos[0].Manifests != 4 || l.ImageRepos[0].ChargedBytes != 1350 {
		t.Errorf("registry list --output json: %+v", l)
	}
	contains(t, "registry get", d.ok(x.ID, "registry", "get", "/team-a/x/app"), "Ref:", "registry.silo.internal/team-a/x/app", "Charged:", "1350 bytes")
	contains(t, "registry tags", d.ok(x.ID, "registry", "tags", "app"), digestOf('1'), "multi", "<untagged>")
	res := d.silo(x.ID, "registry", "get", "/team-a/base")
	contains(t, "a parent's repo", res.stderr, "NOT_FOUND")
	res = d.silo(x.ID, "registry", "get", "app:1")
	contains(t, "get with a tag", res.stderr, "without a tag")

	// prune deletes the untagged manifest and leaves the index's child.
	out := d.ok(x.ID, "registry", "prune", "app", "--untagged")
	contains(t, "registry prune", out, "deleted "+digestOf('4'))
	var ts client.ImageTags
	d.json(x.ID, &ts, "registry", "tags", "app")
	if len(ts.Manifests) != 3 {
		t.Errorf("after prune: %+v", ts.Manifests)
	}

	contains(t, "untag", d.ok(x.ID, "registry", "delete", "app:1"), "deleted tag 1 of registry.silo.internal/team-a/x/app")
	contains(t, "delete manifest", d.ok(a.ID, "registry", "delete", "x/app@"+digestOf('3')), "deleted manifest "+digestOf('3'), "Manifests:")
	d.json(x.ID, &ts, "registry", "tags", "app")
	if len(ts.Tags) != 0 || len(ts.Manifests) != 1 {
		t.Errorf("after the deletes: %+v", ts)
	}
	contains(t, "registry delete", d.ok(x.ID, "registry", "delete", "app"), "deleted /team-a/x/app")
	res = d.silo(x.ID, "registry", "get", "app")
	contains(t, "deleted", res.stderr, "NOT_FOUND")
}

// silo registry cache status and set: install's upstreams until set,
// a set changes only what its flags name, and only the root may.
func TestRegistryCacheCommands(t *testing.T) {
	d := startSilod(t)
	contains(t, "cache status", d.ok(d.root, "registry", "cache", "status"), "docker.io at https://registry-1.docker.io",
		"index.docker.io", "ghcr.io", "Stale tags:", "24h")
	out := d.ok(d.root, "registry", "cache", "set", "--upstream", "docker.io=https://mirror.example.com,index.docker.io",
		"--upstream", "ghcr.io=https://ghcr.io", "--stale-tag-max", "2h")
	contains(t, "cache set", out, "docker.io at https://mirror.example.com (also index.docker.io)", "ghcr.io at https://ghcr.io", "2h")
	var st client.CacheSettings
	d.json(d.root, &st, "registry", "cache", "status")
	if len(st.Upstreams) != 2 || st.StaleTagMax != "2h" || st.AuthTTL != "300s" {
		t.Errorf("after set: %+v", st)
	}
	res := d.silo(d.root, "registry", "cache", "set", "--upstream", "nodot=https://x.io")
	contains(t, "a bad upstream", res.stderr, "INVALID_ARGUMENT")
	var a client.Silo
	d.json(d.root, &a, "create", "/team-a", "--address", "/16")
	res = d.silo(a.ID, "registry", "cache", "status")
	contains(t, "a child's status", res.stderr, "PERMISSION_DENIED")
	res = d.silo(a.ID, "registry", "cache", "evict")
	contains(t, "a child's evict", res.stderr, "PERMISSION_DENIED")
	res = d.silo(d.root, "registry", "cache", "evict", "docker.io/library/alpine@nope")
	contains(t, "a bad evict ref", res.stderr, "INVALID_ARGUMENT")
	// This fence API is given no registry, as silod gives none to a
	// backend other than the local one.
	res = d.silo(d.root, "registry", "cache", "evict", "docker.io/library/alpine")
	contains(t, "evict with no registry", res.stderr, "FAILED_PRECONDITION")
}
