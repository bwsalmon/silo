package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The facade's half of the local registry (local-registry.md §7, call
// L68): push to registry.silo.internal, pulls of local repos without
// egress, save and load, commit for the sandbox class, and docker
// login. Every image these make or read is in silo-containerd's content
// store (Options.Content), each blob once by digest, and an image
// record names its manifest there.

// LocalRegistry is what the facade asks of the registry service;
// *registry.Service is it.
type LocalRegistry interface {
	PushHeld(ctx context.Context, c fenceapi.Caller, name, ref, mediaType string, manifest []byte) (string, error)
	Pull(ctx context.Context, c fenceapi.Caller, name, ref string) (mediaType string, manifest []byte, digest string, err error)
}

var _ LocalRegistry = (*registry.Service)(nil)

// The audit kind of a facade push, beside the registry's registry.push
// (local-registry.md, "Audit", call L67).
const KindImagePush = "image.push"

// Media types the facade writes.
const (
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaOCIConfig      = "application/vnd.oci.image.config.v1+json"
	mediaOCILayer       = "application/vnd.oci.image.layer.v1.tar"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// maxJSON caps a manifest, config or index the facade reads.
const maxJSON = 4 << 20

var (
	errPushElsewhere = unsupported("silo: pushes go to registry.silo.internal only; this host copies nothing to external registries")
	errCommitVM      = unsupported("silo: commit of a vm container is not served: its writable layer is a guest disk")
	errNoContent     = unavailable("silo: this silod has no content store for images")
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

// imageManifest is an image manifest or an index, OCI's or Docker's.
type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        *descriptor  `json:"config,omitempty"`
	Layers        []descriptor `json:"layers,omitempty"`
	Manifests     []descriptor `json:"manifests,omitempty"`
}

func isIndex(mt string) bool { return mt == mediaOCIIndex || mt == mediaDockerList }

// parseManifest parses b as an image manifest of media type mt, and
// checks its shape: a config and digests in the form the store keeps.
func parseManifest(b []byte, mt string) (*imageManifest, error) {
	var m imageManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("the manifest is not JSON: %v", err)
	}
	if mt == "" {
		mt = m.MediaType
	}
	if m.SchemaVersion != 2 || (m.MediaType != "" && m.MediaType != mt) {
		return nil, fmt.Errorf("the manifest's schemaVersion or mediaType is wrong")
	}
	ds := m.Layers
	if isIndex(mt) {
		ds = m.Manifests
	} else if m.Config == nil {
		return nil, fmt.Errorf("the manifest has no config")
	} else {
		ds = append([]descriptor{*m.Config}, ds...)
	}
	for _, d := range ds {
		if !imagerepos.ValidDigest(d.Digest) || !strings.HasPrefix(d.Digest, "sha256:") || d.Size < 0 {
			return nil, fmt.Errorf("the manifest names %q, which is not a sha256 digest", d.Digest)
		}
	}
	m.MediaType = mt
	return &m, nil
}

// blobs are the manifest's config and layers.
func (m *imageManifest) blobs() []descriptor {
	return append([]descriptor{*m.Config}, m.Layers...)
}

// platformChild is an index's manifest for this host's platform.
func platformChild(m *imageManifest) (descriptor, bool) {
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == goarch() {
			return d, true
		}
	}
	return descriptor{}, false
}

// digestOf is b's sha256 digest.
func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// readBlob is a stored blob, of at most max bytes.
func (s *Server) readBlob(ctx context.Context, digest string, max int64) ([]byte, error) {
	ra, n, closer, err := s.opts.Content.Open(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	if n > max {
		return nil, fmt.Errorf("%s is %d bytes, over %d", digest, n, max)
	}
	b := make([]byte, n)
	if _, err := ra.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

// writeBlob stores b by its digest, which the content store checks.
func (s *Server) writeBlob(ctx context.Context, ref, lease string, b []byte) (string, error) {
	d := digestOf(b)
	in, err := s.opts.Content.Ingest(ctx, ref, lease)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if err := in.Truncate(0); err != nil {
		return "", err
	}
	if _, err := in.Write(b); err != nil {
		return "", err
	}
	return d, in.Commit(ctx, int64(len(b)), d)
}

// configFile is the part of an image config the facade reads.
type configFile struct {
	Created time.Time   `json:"created"`
	Config  ImageConfig `json:"config"`
}

// pulledFrom is the image a stored manifest makes: its config, read
// from the content store, and its size, which is what it is charged.
func (s *Server) pulledFrom(ctx context.Context, mt string, manifest []byte, repoDigest string) (*Pulled, error) {
	m, err := parseManifest(manifest, mt)
	if err != nil {
		return nil, err
	}
	if isIndex(m.MediaType) {
		return nil, fmt.Errorf("an index is not an image")
	}
	cb, err := s.readBlob(ctx, m.Config.Digest, maxJSON)
	if err != nil {
		return nil, fmt.Errorf("the image's config: %w", err)
	}
	var cf configFile
	if err := json.Unmarshal(cb, &cf); err != nil {
		return nil, fmt.Errorf("the image's config is not JSON: %v", err)
	}
	p := &Pulled{ID: m.Config.Digest, RepoDigest: repoDigest, Created: cf.Created, Config: cf.Config,
		Manifest: digestOf(manifest), MediaType: m.MediaType}
	for _, d := range m.blobs() {
		p.Size += d.Size
	}
	return p, nil
}

// localName splits a reference to registry.silo.internal into the
// repo's name and its tag or digest; ok is false for any other
// registry.
func localName(ref string) (name, tagOrDigest string, ok bool) {
	rest, ok := strings.CutPrefix(ref, imagerepos.Host+"/")
	if !ok {
		return "", "", false
	}
	if n, d, ok := strings.Cut(rest, "@"); ok {
		return n, d, true
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		return rest[:i], rest[i+1:], true
	}
	return rest, "latest", true
}

// pullLocal pulls ref from the local registry, as the caller, with no
// egress check: the content is already on this host (§7, "Pulling
// from the local registry"). A repo the caller may not read is
// Docker's manifest unknown, the same as one that doesn't exist.
func (s *Server) pullLocal(q *request, ref string) (*Pulled, error) {
	if s.opts.Registry == nil || s.opts.Content == nil {
		return nil, unavailable("silo: this silod serves no local registry")
	}
	name, at, _ := localName(ref)
	unknown := notFound("manifest for %s not found: manifest unknown: manifest unknown", ref)
	mt, b, digest, err := s.opts.Registry.Pull(q.ctx(), q.caller, name, at)
	var re *registry.Error
	if errors.As(err, &re) && re.Status == http.StatusNotFound {
		return nil, unknown
	}
	if err != nil {
		return nil, err
	}
	repoDigest := imagerepos.Host + "/" + name + "@" + digest
	if isIndex(mt) {
		m, err := parseManifest(b, mt)
		if err != nil {
			return nil, invalid("silo: %s: %v", ref, err)
		}
		child, ok := platformChild(m)
		if !ok {
			return nil, notFound("no matching manifest for linux/%s in the manifest list entries", goarch())
		}
		if mt, b, _, err = s.opts.Registry.Pull(q.ctx(), q.caller, name, child.Digest); err != nil {
			return nil, unknown
		}
	}
	p, err := s.pulledFrom(q.ctx(), mt, b, repoDigest)
	if err != nil {
		return nil, invalid("silo: %s: %v", ref, err)
	}
	return p, nil
}

// recordPull makes or updates the target's record of p, tagged ref
// unless ref is by digest, charging a new record its size as every
// pull is (Images §4), and audits the call.
func (s *Server) recordPull(ctx context.Context, tx store.Tx, q *request, p *Pulled, refs []string, fields map[string]any) (*ImageRecord, error) {
	img, err := row[ImageRecord](ctx, tx, q.target.ID, kindImage, imageKey(p.ID))
	if err != nil {
		return nil, err
	}
	var charges []fenceapi.ChargeLine
	if img == nil {
		img = &ImageRecord{ID: p.ID, Size: p.Size, Created: p.Created, Config: p.Config}
		if charges, err = s.charge(ctx, tx, q, Object(q.target.ID, kindImage, imageKey(p.ID)), line{ClassDiskGB, gib(p.Size)}); err != nil {
			return nil, err
		}
	}
	if p.Manifest != "" {
		img.Manifest, img.MediaType = p.Manifest, p.MediaType
	}
	if p.RepoDigest != "" && !slices.Contains(img.RepoDigests, p.RepoDigest) {
		img.RepoDigests = append(img.RepoDigests, p.RepoDigest)
	}
	for _, ref := range refs {
		if !strings.Contains(ref, "@") {
			if err := s.retag(ctx, tx, q.target.ID, img, ref); err != nil {
				return nil, err
			}
		}
	}
	if err := put(ctx, tx, q.target.ID, kindImage, imageKey(img.ID), imageKey(img.ID), img); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["id"] = p.ID
	return img, s.auditTx(ctx, tx, q, "OK", fields, charges)
}

// manifestOf is an image record's manifest, from the content store.
func (s *Server) manifestOf(ctx context.Context, img *ImageRecord) (*imageManifest, []byte, error) {
	if img.Manifest == "" || s.opts.Content == nil {
		return nil, nil, unsupported("silo: image %s has no manifest in this host's content store", imageKey(img.ID)[:12])
	}
	b, err := s.readBlob(ctx, img.Manifest, maxJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("image %s's manifest: %w", imageKey(img.ID)[:12], err)
	}
	m, err := parseManifest(b, img.MediaType)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

// imagePush is POST /images/{name}/push: the record tagged name:tag in
// the target's namespace is written into the local repo name names,
// as the caller, over the content it already has (§7, "Pushing"). No
// bytes are copied. Any other registry is 501.
func (s *Server) imagePush(q *request) error {
	name := q.params["name"]
	tag := q.query("tag")
	if tag == "" {
		tag = "latest"
	}
	ref, err := normalize(name + ":" + tag)
	if err != nil {
		return s.refuse(q, err)
	}
	repo, _, ok := localName(ref)
	if !ok {
		return s.refuse(q, errPushElsewhere)
	}
	if s.opts.Registry == nil || s.opts.Content == nil {
		return s.refuse(q, unavailable("silo: this silod serves no local registry"))
	}
	var img *ImageRecord
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		is, err := s.images(q.ctx(), tx, q.target.ID)
		for _, i := range is {
			if slices.Contains(i.Tags, ref) {
				img = i
			}
		}
		if err == nil && img == nil {
			err = notFound("An image does not exist locally with the tag: %s", strings.TrimSuffix(ref, ":"+tag))
		}
		return err
	})
	if err != nil {
		return s.refuse(q, err)
	}
	m, b, err := s.manifestOf(q.ctx(), img)
	if err != nil {
		return s.refuse(q, err)
	}
	digest, err := s.opts.Registry.PushHeld(q.ctx(), q.caller, repo, tag, m.MediaType, b)
	var re *registry.Error
	if errors.As(err, &re) {
		switch re.Status {
		case http.StatusNotFound:
			err = notFound("silo: repository %s/%s not found: name unknown: repository name not known to registry", imagerepos.Host, repo)
		default:
			err = errorf(re.Status, "silo: %s", re.Message)
		}
	}
	if err != nil {
		return s.refuse(q, err)
	}
	var size int64
	for _, d := range m.blobs() {
		size += d.Size
	}
	repoDigest := imagerepos.Host + "/" + repo + "@" + digest
	s.mu.Lock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		cur, err := row[ImageRecord](q.ctx(), tx, q.target.ID, kindImage, imageKey(img.ID))
		if err != nil || cur == nil {
			return err
		}
		if !slices.Contains(cur.RepoDigests, repoDigest) {
			cur.RepoDigests = append(cur.RepoDigests, repoDigest)
		}
		if err := put(q.ctx(), tx, q.target.ID, kindImage, imageKey(cur.ID), imageKey(cur.ID), cur); err != nil {
			return err
		}
		fields := map[string]any{"image": ref, "id": img.ID, "repo": repo, "tag": tag, "digest": digest}
		if err := s.auditTx(q.ctx(), tx, q, "OK", fields, nil); err != nil {
			return err
		}
		t, err := tree.Get(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		return fenceapi.Audit(q.ctx(), tx, t, q.caller, KindImagePush, "", "OK", map[string]any{
			"image": ref, "id": img.ID, "digest": digest, "bytes": size})
	})
	s.mu.Unlock()
	if err != nil {
		return s.refuse(q, err)
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	enc := jsonLines(q.w)
	enc(map[string]any{"status": "The push refers to repository [" + strings.TrimSuffix(ref, ":"+tag) + "]"})
	for i := len(m.Layers) - 1; i >= 0; i-- {
		enc(map[string]any{"status": "Pushed", "progressDetail": map[string]any{}, "id": short(m.Layers[i].Digest)})
	}
	enc(map[string]any{"status": fmt.Sprintf("%s: digest: %s size: %d", tag, digest, len(b))})
	enc(map[string]any{"progressDetail": map[string]any{}, "aux": map[string]any{"Tag": tag, "Digest": digest, "Size": len(b)}})
	return nil
}

// short is a digest's first twelve hex characters, as Docker's progress
// names a layer.
func short(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// freeDisk is the bytes the target may still charge to disk_gb: what
// load and commit are capped at.
func (s *Server) freeDisk(ctx context.Context, silo string) (int64, error) {
	var free int64
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		r, err := ledger.Get(ctx, tx, silo, Key(ClassDiskGB))
		if err != nil {
			return err
		}
		free = r.Free()
		return nil
	})
	if free < 0 {
		free = 0
	}
	// The ledger counts millionths of a GiB.
	return free/ledger.Micro<<30 + free%ledger.Micro*(1<<30)/ledger.Micro, err
}
