package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
)

// The manifest media types the registry stores.
const (
	MediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	MediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

func isIndex(mt string) bool { return mt == MediaOCIIndex || mt == MediaDockerList }

// nonDistributable reports whether a layer's media type says its bytes
// are fetched from its urls rather than pushed, so that a manifest may
// name it without the repo holding it.
func nonDistributable(mt string) bool {
	return strings.HasPrefix(mt, "application/vnd.oci.image.layer.nondistributable.") ||
		mt == "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
}

type platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	Features     []string `json:"features,omitempty"`
}

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Data         []byte            `json:"data,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
}

// manifest is an image manifest or an index, as the registry parses
// either. Fields it doesn't know are kept in the stored bytes, as the
// distribution spec's custom fields are.
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *descriptor       `json:"config,omitempty"`
	Layers        []descriptor      `json:"layers,omitempty"`
	Manifests     []descriptor      `json:"manifests,omitempty"`
	Subject       *descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// parseManifest parses b as media type mt and checks it is one: the
// schema version, the media type it names, and the fields its type has.
func parseManifest(b []byte, mt string) (*manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a manifest: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("data after the manifest")
	}
	if m.SchemaVersion != 2 {
		return nil, fmt.Errorf("schemaVersion %d: want 2", m.SchemaVersion)
	}
	if m.MediaType != "" && m.MediaType != mt {
		return nil, fmt.Errorf("mediaType %q doesn't match Content-Type %q", m.MediaType, mt)
	}
	switch {
	case mt == MediaOCIManifest || mt == MediaDockerManifest:
		if m.Config == nil || m.Manifests != nil {
			return nil, fmt.Errorf("an image manifest has a config and layers, and no manifests")
		}
	case isIndex(mt):
		if m.Config != nil || m.Layers != nil {
			return nil, fmt.Errorf("an index has manifests, and no config or layers")
		}
	default:
		return nil, fmt.Errorf("media type %q: want %s, %s, %s or %s", mt, MediaOCIManifest, MediaOCIIndex, MediaDockerManifest, MediaDockerList)
	}
	for _, d := range m.descriptors() {
		if !imagerepos.ValidDigest(d.Digest) || d.Size < 0 {
			return nil, fmt.Errorf("descriptor %q: want a sha256 or sha512 digest and a size", d.Digest)
		}
	}
	if m.Subject != nil && !imagerepos.ValidDigest(m.Subject.Digest) {
		return nil, fmt.Errorf("subject %q: want a sha256 or sha512 digest", m.Subject.Digest)
	}
	return &m, nil
}

// descriptors are what m references: config and layers, or an index's
// manifests. Never its subject.
func (m *manifest) descriptors() []descriptor {
	var out []descriptor
	if m.Config != nil {
		out = append(out, *m.Config)
	}
	out = append(out, m.Layers...)
	return append(out, m.Manifests...)
}

// target resolves a manifest reference in repo: a tag to its digest,
// or a digest the repo holds.
func target(ctx context.Context, tx store.Tx, repo *imagerepos.Repo, ref string) (*imagerepos.Manifest, error) {
	digest := ref
	if !strings.Contains(ref, ":") {
		ts, err := imagerepos.Tags(ctx, tx, repo.ID)
		if err != nil {
			return nil, err
		}
		digest = ""
		for _, t := range ts {
			if t.Tag == ref {
				digest = t.Digest
			}
		}
	}
	ms, err := imagerepos.Manifests(ctx, tx, repo.ID)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.Digest == digest {
			return m, nil
		}
	}
	return nil, rerr(http.StatusNotFound, codeManifestUnknown, "manifest unknown to registry")
}

// getManifest is GET and HEAD /v2/<name>/manifests/<reference>.
func (s *Service) getManifest(q *request, ref string) {
	ctx := q.ctx()
	var m *imagerepos.Manifest
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		t, err := s.access(ctx, tx, q, false)
		if err != nil {
			return err
		}
		if m, err = target(ctx, tx, t.Repo, ref); err != nil {
			return err
		}
		return audit(ctx, tx, t, fenceapi.KindRegistryPull, map[string]any{"reference": ref, "digest": m.Digest,
			"bytes": m.Size, "status": http.StatusOK, "method": q.r.Method})
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	ra, n, closer, err := s.opts.Content.Open(ctx, m.Digest)
	if errors.Is(err, ErrUnknown) {
		log.Printf("registry: image_manifest_missing: %s@%s", q.name, m.Digest)
		writeErr(q.w, q.r, rerr(http.StatusNotFound, codeManifestUnknown, "manifest unknown to registry"))
		return
	}
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	defer closer.Close()
	q.w.Header().Set("Content-Type", m.MediaType)
	q.w.Header().Set("Docker-Content-Digest", m.Digest)
	q.w.Header().Set("Etag", `"`+m.Digest+`"`)
	serveContent(q.w, q.r, ra, n)
}

// serveContent serves n bytes of ra, with Range.
func serveContent(w http.ResponseWriter, r *http.Request, ra io.ReaderAt, n int64) {
	http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(ra, 0, n))
}

// putManifest is PUT /v2/<name>/manifests/<reference>: the manifest is
// stored and recorded in the repo, and a tag reference points at it.
// It may reference only blobs the repo may answer for and manifests it
// holds, or it is MANIFEST_BLOB_UNKNOWN; its subject may be absent.
func (s *Service) putManifest(q *request, ref string) {
	ctx := q.ctx()
	// The access check comes first, so that a caller who may not push
	// learns nothing from how its manifest is judged.
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		_, err := s.access(ctx, tx, q, true)
		return err
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	b, err := io.ReadAll(io.LimitReader(q.r.Body, s.opts.MaxManifest+1))
	if err != nil {
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeManifestInvalid, "silo: reading the manifest: %v", err))
		return
	}
	if int64(len(b)) > s.opts.MaxManifest {
		writeErr(q.w, q.r, rerr(http.StatusRequestEntityTooLarge, codeSizeInvalid, "silo: a manifest is at most %d bytes", s.opts.MaxManifest))
		return
	}
	mt, _, _ := strings.Cut(q.r.Header.Get("Content-Type"), ";")
	mt = strings.TrimSpace(mt)
	tag := ""
	digest := Digest("sha256", b)
	if strings.Contains(ref, ":") {
		alg, _, _ := strings.Cut(ref, ":")
		if !imagerepos.ValidDigest(ref) || Digest(alg, b) != ref {
			writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeDigestInvalid, "provided digest did not match uploaded content"))
			return
		}
		digest = ref
	} else if !imagerepos.ValidTag(ref) {
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeManifestInvalid, "silo: tag %q is not an OCI tag", ref))
		return
	} else {
		tag = ref
	}
	m, perr := parseManifest(b, mt)
	if mt == "" && perr != nil {
		// No Content-Type: take the one the manifest names.
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		if json.Unmarshal(b, &probe) == nil && probe.MediaType != "" {
			mt = probe.MediaType
			m, perr = parseManifest(b, mt)
		}
	}
	if perr != nil {
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeManifestInvalid, "silo: %v", perr))
		return
	}
	// The manifest's own lease keeps it until its record does.
	lease := "silo-registry-manifest-" + randomID()
	if err := s.opts.Content.Lease(context.WithoutCancel(ctx), lease, s.opts.Now().Add(s.opts.Grace)); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	defer s.opts.Content.DeleteLease(context.WithoutCancel(ctx), lease)
	in, err := s.opts.Content.Ingest(context.WithoutCancel(ctx), "silo-registry-manifest-"+digest, lease)
	if err == nil {
		_ = in.Truncate(0)
		if _, err = in.Write(b); err == nil {
			err = in.Commit(context.WithoutCancel(ctx), int64(len(b)), digest)
		}
		in.Close()
	}
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	var repo *imagerepos.Repo
	var refs []string
	var seq uint64
	err = s.push(q, func(tx store.Tx, t *fenceapi.RegistryTarget) error {
		row := &imagerepos.Manifest{Repo: t.Repo.ID, Digest: digest, MediaType: mt, Size: int64(len(b))}
		held, err := imagerepos.Manifests(ctx, tx, t.Repo.ID)
		if err != nil {
			return err
		}
		for _, d := range m.descriptors() {
			var size int64
			ok := false
			if isIndex(mt) {
				for _, h := range held {
					if h.Digest == d.Digest {
						size, ok = h.Size, true
					}
				}
			} else if q.held {
				size, err = s.opts.Content.Size(ctx, d.Digest)
				ok = err == nil
				if errors.Is(err, ErrUnknown) {
					err = nil
				}
				if err != nil {
					return err
				}
			} else {
				size, ok, err = s.blob(ctx, tx, t.Repo, d.Digest)
				if err != nil {
					return err
				}
			}
			if !ok && nonDistributable(d.MediaType) {
				continue // not here, and not charged
			}
			if !ok {
				return rerr(http.StatusBadRequest, codeManifestUnknownBl, "blob unknown to registry: %s", d.Digest)
			}
			if size != d.Size {
				return rerr(http.StatusBadRequest, codeManifestInvalid, "silo: %s is %d bytes, not %d", d.Digest, size, d.Size)
			}
			row.Refs = append(row.Refs, imagerepos.Blob{Digest: d.Digest, Size: size})
		}
		refs = nil
		for _, r := range row.Refs {
			refs = append(refs, r.Digest)
		}
		if err := imagerepos.PutManifest(ctx, tx, row); err != nil {
			return err
		}
		if tag != "" {
			if err := imagerepos.SetTag(ctx, tx, t.Repo.ID, tag, digest); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]string{"repo": t.Repo.ID, "digest": digest, "what": "put"})
		if _, err = tx.Append(ctx, ManifestChangeKind, t.Repo.ID+"@"+digest, string(data)); err != nil {
			return err
		}
		// The record is numbered from the object sequence, so that a
		// store restored to before this push is older than its label
		// (§4, "Restore").
		if seq, err = objseq.Next(ctx, tx); err != nil {
			return err
		}
		delta, err := s.charge(ctx, tx, t)
		if err != nil {
			return err
		}
		repo = t.Repo
		return audit(ctx, tx, t, fenceapi.KindRegistryPush, map[string]any{"tag": tag, "digest": digest,
			"bytes": len(b), "charged_delta": delta})
	})
	if err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	// The repo's record keeps the manifest, and through its references
	// the config, layers and child manifests, from containerd's GC.
	img := Image{Name: repo.ID + "@" + digest, Digest: digest, MediaType: mt, Size: int64(len(b)), Refs: refs,
		Labels: map[string]string{SeqLabel: strconv.FormatUint(seq, 10)}}
	if err := s.opts.Content.PutImage(context.WithoutCancel(ctx), Namespace, img); err != nil {
		log.Printf("registry: recording %s in %s: %v", img.Name, Namespace, err)
	}
	q.w.Header().Set("Location", "/v2/"+q.name.String()+"/manifests/"+digest)
	q.w.Header().Set("Docker-Content-Digest", digest)
	if m.Subject != nil {
		q.w.Header().Set("OCI-Subject", m.Subject.Digest)
	}
	q.w.Header().Set("Content-Length", "0")
	q.w.WriteHeader(http.StatusCreated)
}

// deleteManifest is DELETE /v2/<name>/manifests/<reference>: a tag is
// untagged, and a digest deleted with every tag on it, by a caller that
// may push to the repo.
func (s *Service) deleteManifest(q *request, ref string) {
	ctx := q.ctx()
	var gone []string
	var repo string
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		t, err := s.access(ctx, tx, q, true)
		if err != nil {
			return err
		}
		repo = t.Repo.ID
		gone = nil
		if !strings.Contains(ref, ":") {
			if err := imagerepos.Untag(ctx, tx, t.Repo.ID, ref); errors.Is(err, imagerepos.ErrTagNotFound) {
				return rerr(http.StatusNotFound, codeManifestUnknown, "manifest unknown to registry")
			} else if err != nil {
				return err
			}
		} else {
			var err error
			gone, err = imagerepos.DeleteManifest(ctx, tx, t.Repo.ID, ref)
			if errors.Is(err, imagerepos.ErrManifestNotFound) {
				return rerr(http.StatusNotFound, codeManifestUnknown, "manifest unknown to registry")
			} else if err != nil {
				return err
			}
		}
		delta, err := s.charge(ctx, tx, t)
		if err != nil {
			return err
		}
		return audit(ctx, tx, t, fenceapi.KindRegistryDelete, map[string]any{"reference": ref, "manifests": gone,
			"refunded_bytes": -delta})
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	s.Removed(context.WithoutCancel(ctx), repo, gone)
	q.w.WriteHeader(http.StatusAccepted)
}

// referrers is GET /v2/<name>/referrers/<digest>: the repo's manifests
// whose subject is digest, as an index, filtered by artifactType.
func (s *Service) referrers(q *request, digest string) {
	ctx := q.ctx()
	if !imagerepos.ValidDigest(digest) {
		if _, err := s.read(q); err != nil {
			writeErr(q.w, q.r, err)
			return
		}
		writeErr(q.w, q.r, rerr(http.StatusBadRequest, codeDigestInvalid, "silo: digest %q: want sha256:<64 hex> or sha512:<128 hex>", digest))
		return
	}
	var ms []*imagerepos.Manifest
	err := s.opts.Store.Tx(ctx, func(tx store.Tx) error {
		ms = nil
		t, err := s.access(ctx, tx, q, false)
		if err != nil {
			// A repo the caller would create by pushing refers to
			// nothing yet: the caller learns only of its own silo.
			if w, werr := s.opts.API.RegistryAccess(ctx, tx, q.caller, q.name, true); werr == nil && w.Repo == nil {
				return nil
			}
			return err
		}
		ms, err = imagerepos.Manifests(ctx, tx, t.Repo.ID)
		return err
	})
	if err = s.refused(q, err); err != nil {
		writeErr(q.w, q.r, err)
		return
	}
	want := q.r.URL.Query().Get("artifactType")
	out := []descriptor{}
	for _, row := range ms {
		ra, n, closer, err := s.opts.Content.Open(ctx, row.Digest)
		if err != nil {
			continue
		}
		b := make([]byte, n)
		_, err = ra.ReadAt(b, 0)
		closer.Close()
		if err != nil && err != io.EOF {
			continue
		}
		m, err := parseManifest(b, row.MediaType)
		if err != nil || m.Subject == nil || m.Subject.Digest != digest {
			continue
		}
		at := m.ArtifactType
		if at == "" && m.Config != nil {
			at = m.Config.MediaType
		}
		if want != "" && at != want {
			continue
		}
		out = append(out, descriptor{MediaType: row.MediaType, Digest: row.Digest, Size: row.Size, ArtifactType: at,
			Annotations: m.Annotations})
	}
	if want != "" {
		q.w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MediaOCIIndex, "manifests": out})
	q.w.Header().Set("Content-Type", MediaOCIIndex)
	q.w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if q.r.Method != http.MethodHead {
		q.w.Write(b)
	}
}
