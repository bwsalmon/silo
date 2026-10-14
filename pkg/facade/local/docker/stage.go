package docker

import (
	"context"
	"fmt"

	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// stageImage records silo's image id in the namespace the drivers run
// images from (Options.RunNamespace), so that a driver's Prepare finds
// it there. A pull, load or commit leaves the image's blobs in the
// content store under the registry's namespaces; the record links
// them, without copying a byte, and keeps them from containerd's GC
// while it lasts. Without a RunNamespace or a content store, or for a
// record with no manifest, it does nothing, and Prepare says what is
// missing.
func (s *Server) stageImage(ctx context.Context, silo, id string) error {
	if s.opts.RunNamespace == "" || s.opts.Content == nil {
		return nil
	}
	var img *ImageRecord
	if err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		img, err = row[ImageRecord](ctx, tx, silo, kindImage, imageKey(id))
		return err
	}); err != nil {
		return err
	}
	if img == nil || img.Manifest == "" {
		return nil
	}
	refs, size, err := s.imageRefs(ctx, img.Manifest, img.MediaType)
	if err != nil {
		return fmt.Errorf("staging image %s: %w", imageKey(id)[:12], err)
	}
	return s.opts.Content.PutImage(ctx, s.opts.RunNamespace, registry.Image{Name: StagedName(img.Manifest),
		Digest: img.Manifest, MediaType: img.MediaType, Size: size, Refs: refs})
}

// StagedName is the name stageImage records a manifest under in
// Options.RunNamespace.
func StagedName(manifest string) string { return "silo.internal/staged@" + manifest }

// imageRefs are the blobs a manifest names, by digest: its config and
// layers, or, for an index, its child for this host's platform and that
// child's; and the manifest's own size.
func (s *Server) imageRefs(ctx context.Context, digest, mt string) ([]string, int64, error) {
	b, err := s.readBlob(ctx, digest, maxJSON)
	if err != nil {
		return nil, 0, err
	}
	m, err := parseManifest(b, mt)
	if err != nil {
		return nil, 0, err
	}
	if isIndex(m.MediaType) {
		child, ok := platformChild(m)
		if !ok {
			return nil, 0, fmt.Errorf("index %s has no manifest for linux/%s", digest, goarch())
		}
		refs, _, err := s.imageRefs(ctx, child.Digest, child.MediaType)
		return append([]string{child.Digest}, refs...), int64(len(b)), err
	}
	var refs []string
	for _, d := range m.blobs() {
		refs = append(refs, d.Digest)
	}
	return refs, int64(len(b)), nil
}
