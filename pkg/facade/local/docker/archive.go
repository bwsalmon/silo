package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// save, load and commit (local-registry.md §7, "load and save" and
// "commit"). None of them unpacks an archive on the host: save reads
// blobs out of the content store, load writes each entry of an archive
// into an ingest under the digest its path names, and commit stores a
// container's writable layer as one blob, all hashed on the way in.

// imageSave is GET /images/{name}/get and GET /images/get?names=…:
// an archive of images the target holds records of, in both of
// docker save's forms at once: the OCI image layout (oci-layout,
// index.json, blobs/sha256/) and manifest.json, whose paths name the
// same blobs.
func (s *Server) imageSave(q *request) error {
	names := q.r.URL.Query()["names"]
	if n := q.params["name"]; n != "" {
		names = []string{n}
	}
	if len(names) == 0 {
		return invalid("silo: name the images to save")
	}
	if s.opts.Content == nil {
		return errNoContent
	}
	type saved struct {
		img  *ImageRecord
		m    *imageManifest
		raw  []byte
		tags []string
	}
	var out []*saved
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		for _, n := range names {
			img, err := s.image(q.ctx(), tx, q.target.ID, n)
			if err != nil {
				return err
			}
			var sv *saved
			for _, o := range out {
				if o.img.ID == img.ID {
					sv = o
				}
			}
			if sv == nil {
				sv = &saved{img: img}
				out = append(out, sv)
			}
			if t, err := normalize(n); err == nil && slices.Contains(img.Tags, t) && !slices.Contains(sv.tags, t) {
				sv.tags = append(sv.tags, t)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, sv := range out {
		if sv.m, sv.raw, err = s.manifestOf(q.ctx(), sv.img); err != nil {
			return err
		}
	}
	q.w.Header().Set("Content-Type", "application/x-tar")
	q.w.WriteHeader(http.StatusOK)
	tw := tar.NewWriter(q.w)
	now := time.Unix(0, 0)
	file := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	blobPath := func(d string) string { return "blobs/sha256/" + strings.TrimPrefix(d, "sha256:") }
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: now}); err != nil {
		return nil
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/sha256/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: now}); err != nil {
		return nil
	}
	written := map[string]bool{}
	index := imageManifest{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: []descriptor{}}
	var legacy []map[string]any
	for _, sv := range out {
		if !written[sv.img.Manifest] {
			written[sv.img.Manifest] = true
			if err := file(blobPath(sv.img.Manifest), sv.raw); err != nil {
				return nil
			}
		}
		for _, d := range sv.m.blobs() {
			if written[d.Digest] {
				continue
			}
			written[d.Digest] = true
			ra, n, closer, err := s.opts.Content.Open(q.ctx(), d.Digest)
			if err != nil {
				return nil // the stream is cut short, as Docker's is
			}
			err = tw.WriteHeader(&tar.Header{Name: blobPath(d.Digest), Mode: 0o644, Size: n, ModTime: now, Typeflag: tar.TypeReg})
			if err == nil {
				_, err = io.Copy(tw, io.NewSectionReader(ra, 0, n))
			}
			closer.Close()
			if err != nil {
				return nil
			}
		}
		md := descriptor{MediaType: sv.m.MediaType, Digest: sv.img.Manifest, Size: int64(len(sv.raw))}
		if len(sv.tags) == 0 {
			index.Manifests = append(index.Manifests, md)
		}
		for _, t := range sv.tags {
			d := md
			_, tag, _ := strings.Cut(t[strings.LastIndex(t, "/")+1:], ":")
			d.Annotations = map[string]string{"io.containerd.image.name": t, "org.opencontainers.image.ref.name": tag}
			index.Manifests = append(index.Manifests, d)
		}
		var layers []string
		for _, l := range sv.m.Layers {
			layers = append(layers, blobPath(l.Digest))
		}
		legacy = append(legacy, map[string]any{"Config": blobPath(sv.m.Config.Digest), "RepoTags": nonNil(sv.tags), "Layers": nonNil(layers)})
	}
	ib, _ := json.Marshal(index)
	lb, _ := json.Marshal(legacy)
	if file("index.json", ib) != nil || file("manifest.json", lb) != nil ||
		file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)) != nil {
		return nil
	}
	_ = tw.Close()
	return nil
}

var (
	blobEntry   = regexp.MustCompile(`^blobs/sha256/([a-f0-9]{64})$`)
	layerEntry  = regexp.MustCompile(`^[a-f0-9]{64}/layer\.tar$`)
	legacyEntry = regexp.MustCompile(`^([a-f0-9]{64}\.json|[a-f0-9]{64}/(json|VERSION))$`)
)

// loadArchive is one docker load in progress: every blob the archive
// carries, in an ingest that is committed only once the whole archive
// has been read and checked, so that a refused load writes nothing.
type loadArchive struct {
	s     *Server
	id    string
	n     int
	open  map[string]registry.Ingest // by digest, not yet committed
	sizes map[string]int64
	small map[string][]byte // index.json, manifest.json and legacy configs, by name
	paths map[string]string // a legacy layer.tar's digest, by path
}

func (l *loadArchive) ingest(ctx context.Context, r io.Reader) (registry.Ingest, int64, error) {
	l.n++
	in, err := l.s.opts.Content.Ingest(ctx, fmt.Sprintf("silo-load-%s-%d", l.id, l.n), "silo-load-"+l.id)
	if err != nil {
		return nil, 0, err
	}
	if err := in.Truncate(0); err != nil {
		in.Close()
		return nil, 0, err
	}
	n, err := io.Copy(in, r)
	if err != nil {
		in.Close()
		return nil, 0, err
	}
	return in, n, nil
}

// keep holds in as digest's bytes, or drops it if the archive already
// carried them.
func (l *loadArchive) keep(ctx context.Context, digest string, in registry.Ingest, n int64) {
	if _, ok := l.open[digest]; ok {
		l.drop(ctx, in)
		return
	}
	l.open[digest], l.sizes[digest] = in, n
}

func (l *loadArchive) drop(ctx context.Context, in registry.Ingest) { in.Close() }

// abort drops every ingest the load opened.
func (l *loadArchive) abort(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for i := 1; i <= l.n; i++ {
		_ = l.s.opts.Content.Abort(ctx, fmt.Sprintf("silo-load-%s-%d", l.id, i))
	}
	for _, in := range l.open {
		in.Close()
	}
	_ = l.s.opts.Content.DeleteLease(ctx, "silo-load-"+l.id)
}

// blob is digest's bytes from the archive, for a manifest or a config.
func (l *loadArchive) blob(digest string) ([]byte, bool) {
	if _, ok := l.open[digest]; !ok {
		return nil, false
	}
	b, ok := l.small[digest]
	return b, ok
}

// entry reads one entry of the archive.
func (l *loadArchive) entry(ctx context.Context, h *tar.Header, r io.Reader) error {
	name := strings.TrimPrefix(h.Name, "./")
	clean := path.Clean(name)
	if strings.HasPrefix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(name, "\\") ||
		slices.Contains(strings.Split(strings.TrimSuffix(name, "/"), "/"), "..") {
		return invalid("silo: load: entry %q is outside the archive", h.Name)
	}
	if h.Typeflag == tar.TypeDir {
		return nil
	}
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
		return invalid("silo: load: entry %q is not a regular file", h.Name)
	}
	switch {
	case blobEntry.MatchString(clean):
		want := "sha256:" + blobEntry.FindStringSubmatch(clean)[1]
		var buf bytes.Buffer
		in, n, err := l.ingest(ctx, io.TeeReader(r, &limitedBuffer{&buf, maxJSON}))
		if err != nil {
			return err
		}
		if got := in.Digest("sha256"); got != want {
			l.drop(ctx, in)
			return invalid("silo: load: %s does not match its digest (it is %s)", h.Name, got)
		}
		if n <= maxJSON {
			l.small[want] = buf.Bytes()
		}
		l.keep(ctx, want, in, n)
	case layerEntry.MatchString(clean):
		in, n, err := l.ingest(ctx, r)
		if err != nil {
			return err
		}
		d := in.Digest("sha256")
		l.paths[clean] = d
		l.keep(ctx, d, in, n)
	case clean == "index.json" || clean == "manifest.json" || clean == "oci-layout" || clean == "repositories" || legacyEntry.MatchString(clean):
		b, err := io.ReadAll(io.LimitReader(r, maxJSON+1))
		if err != nil {
			return err
		}
		if len(b) > maxJSON {
			return invalid("silo: load: %s is over %d bytes", h.Name, maxJSON)
		}
		l.small[clean] = b
	default:
		return invalid("silo: load: unexpected entry %q", h.Name)
	}
	return nil
}

// limitedBuffer keeps the first max bytes written to it, and counts the
// rest away.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if room := w.max + 1 - w.b.Len(); room > 0 {
		w.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// loadedImage is one image an archive names.
type loadedImage struct {
	refs     []string
	manifest []byte
	mt       string
	digest   string
}

// images are the archive's images, from index.json if it has one (the
// OCI layout, which docker save writes from Docker 25 on) or else
// manifest.json, every one checked as a pushed manifest is and every
// blob it names carried by the archive at its size.
func (l *loadArchive) images(ctx context.Context) ([]*loadedImage, error) {
	var out []*loadedImage
	check := func(m *imageManifest) error {
		for _, d := range m.blobs() {
			n, ok := l.sizes[d.Digest]
			if !ok {
				return invalid("silo: load: the archive does not carry %s", d.Digest)
			}
			if n != d.Size {
				return invalid("silo: load: %s is %d bytes, not the %d its manifest says", d.Digest, n, d.Size)
			}
		}
		return nil
	}
	if ib, ok := l.small["index.json"]; ok {
		idx, err := parseManifest(ib, mediaOCIIndex)
		if err != nil {
			return nil, invalid("silo: load: index.json: %v", err)
		}
		for _, d := range idx.Manifests {
			mt, digest := d.MediaType, d.Digest
			b, ok := l.blob(digest)
			if !ok || int64(len(b)) != d.Size {
				return nil, invalid("silo: load: the archive does not carry manifest %s", digest)
			}
			if isIndex(mt) {
				child, err := parseManifest(b, mt)
				if err != nil {
					return nil, invalid("silo: load: %s: %v", digest, err)
				}
				c, ok := platformChild(child)
				if !ok {
					return nil, invalid("silo: load: %s has no manifest for linux/%s", digest, goarch())
				}
				if b, ok = l.blob(c.Digest); !ok {
					return nil, invalid("silo: load: the archive does not carry manifest %s", c.Digest)
				}
				mt, digest = c.MediaType, c.Digest
			}
			m, err := parseManifest(b, mt)
			if err != nil {
				return nil, invalid("silo: load: %s: %v", digest, err)
			}
			if err := check(m); err != nil {
				return nil, err
			}
			img := &loadedImage{manifest: b, mt: m.MediaType, digest: digest}
			if n := d.Annotations["io.containerd.image.name"]; n != "" {
				if ref, err := normalize(n); err == nil {
					img.refs = append(img.refs, ref)
				}
			}
			out = append(out, img)
		}
		return out, nil
	}
	mb, ok := l.small["manifest.json"]
	if !ok {
		return nil, invalid("silo: load: the archive has neither index.json nor manifest.json")
	}
	var entries []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(mb, &entries); err != nil {
		return nil, invalid("silo: load: manifest.json: %v", err)
	}
	digestAt := func(p string) (string, error) {
		p = path.Clean(p)
		if m := blobEntry.FindStringSubmatch(p); m != nil {
			return "sha256:" + m[1], nil
		}
		if d, ok := l.paths[p]; ok {
			return d, nil
		}
		if b, ok := l.small[p]; ok && legacyEntry.MatchString(p) {
			d := digestOf(b)
			if _, ok := l.open[d]; !ok {
				in, n, err := l.ingest(ctx, bytes.NewReader(b))
				if err != nil {
					return "", err
				}
				l.small[d] = b
				l.keep(ctx, d, in, n)
			}
			return d, nil
		}
		return "", invalid("silo: load: manifest.json names %q, which the archive does not carry", p)
	}
	for _, e := range entries {
		cd, err := digestAt(e.Config)
		if err != nil {
			return nil, err
		}
		m := imageManifest{SchemaVersion: 2, MediaType: mediaOCIManifest,
			Config: &descriptor{MediaType: mediaOCIConfig, Digest: cd, Size: l.sizes[cd]}, Layers: []descriptor{}}
		for _, lp := range e.Layers {
			ld, err := digestAt(lp)
			if err != nil {
				return nil, err
			}
			m.Layers = append(m.Layers, descriptor{MediaType: mediaOCILayer, Digest: ld, Size: l.sizes[ld]})
		}
		if err := check(&m); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(m)
		d := digestOf(b)
		if _, ok := l.open[d]; !ok {
			in, n, err := l.ingest(ctx, bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			l.small[d] = b
			l.keep(ctx, d, in, n)
		}
		img := &loadedImage{manifest: b, mt: mediaOCIManifest, digest: d}
		for _, t := range e.RepoTags {
			if ref, err := normalize(t); err == nil {
				img.refs = append(img.refs, ref)
			}
		}
		out = append(out, img)
	}
	return out, nil
}

// commit stores every blob the archive carried. Each ingest's digest
// was checked when it was read, and Commit checks it again.
func (l *loadArchive) commit(ctx context.Context) error {
	for d, in := range l.open {
		if err := in.Commit(ctx, l.sizes[d], d); err != nil {
			return fmt.Errorf("storing %s: %w", d, err)
		}
		in.Close()
		delete(l.open, d)
	}
	return nil
}

// countingReader refuses to read past max bytes.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

var errTooBig = errors.New("over the silo's free disk")

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, errTooBig
	}
	return n, err
}

// imageLoad is POST /images/load: an archive in either of docker save's
// forms, read as a stream and never extracted (§7, "load and save").
// It is capped at the target's free disk_gb, and makes records in its
// namespace, charged as pulls are.
func (s *Server) imageLoad(q *request) error {
	if s.opts.Content == nil {
		return s.refuse(q, errNoContent)
	}
	free, err := s.freeDisk(q.ctx(), q.target.ID)
	if err != nil {
		return s.refuse(q, err)
	}
	tooBig := forbidden("silo: load: the archive is over the silo's free disk_gb (%d bytes)", free)
	if q.r.ContentLength > free {
		return s.refuse(q, tooBig)
	}
	ctx := q.ctx()
	l := &loadArchive{s: s, id: newID()[:16], open: map[string]registry.Ingest{}, sizes: map[string]int64{},
		small: map[string][]byte{}, paths: map[string]string{}}
	_ = s.opts.Content.Lease(ctx, "silo-load-"+l.id, time.Now().Add(time.Hour))
	tr := tar.NewReader(&countingReader{r: q.r.Body, max: free})
	var imgs []*loadedImage
	err = func() error {
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err := l.entry(ctx, h, tr); err != nil {
				return err
			}
		}
		var err error
		if imgs, err = l.images(ctx); err != nil {
			return err
		}
		if len(imgs) == 0 {
			return invalid("silo: load: the archive holds no image")
		}
		return nil
	}()
	if errors.Is(err, errTooBig) {
		err = tooBig
	} else if err != nil && asError(err).Code == http.StatusInternalServerError {
		err = invalid("silo: load: reading the archive: %v", err)
	}
	if err == nil {
		err = l.commit(context.WithoutCancel(ctx))
	}
	if err != nil {
		l.abort(ctx)
		return s.refuse(q, err)
	}
	defer l.abort(ctx) // only the lease is left
	var pulled []*Pulled
	for _, i := range imgs {
		p, err := s.pulledFrom(ctx, i.mt, i.manifest, "")
		if err != nil {
			return s.refuse(q, invalid("silo: load: %v", err))
		}
		pulled = append(pulled, p)
	}
	var lines []string
	s.mu.Lock()
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		lines = nil
		for n, i := range imgs {
			if _, err := s.recordPull(ctx, tx, q, pulled[n], i.refs, map[string]any{"load": i.digest, "refs": i.refs}); err != nil {
				return err
			}
			if len(i.refs) == 0 {
				lines = append(lines, "Loaded image ID: "+pulled[n].ID)
			}
			for _, r := range i.refs {
				lines = append(lines, "Loaded image: "+r)
			}
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		return s.refuse(q, err)
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	enc := jsonLines(q.w)
	for _, l := range lines {
		enc(map[string]any{"stream": l + "\n"})
	}
	return nil
}

// whiteoutLayer copies a writable layer from runsc tar rootfs-upper
// into w as an OCI layer, checking every entry: a path outside the
// root is refused, an overlayfs whiteout (a 0/0 character device)
// becomes OCI's .wh.<name>, a directory overlayfs made opaque gains
// .wh..wh..opq, overlayfs's own xattrs are dropped, and any other
// device is refused.
func whiteoutLayer(w io.Writer, r io.Reader) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/")
		clean := path.Clean(name)
		if clean == "." || clean == "" {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("entry %q is outside the root", h.Name)
		}
		if h.Typeflag == tar.TypeLink {
			if l := path.Clean(strings.TrimPrefix(h.Linkname, "/")); l == ".." || strings.HasPrefix(l, "../") {
				return fmt.Errorf("hard link %q is outside the root", h.Name)
			}
		}
		opaque := false
		for k, v := range h.PAXRecords {
			if x, ok := strings.CutPrefix(k, "SCHILY.xattr."); ok && strings.HasPrefix(x, "trusted.overlay.") {
				opaque = opaque || (x == "trusted.overlay.opaque" && v == "y")
				delete(h.PAXRecords, k)
			}
		}
		for k, v := range h.Xattrs { //nolint:staticcheck // older writers
			if strings.HasPrefix(k, "trusted.overlay.") {
				opaque = opaque || (k == "trusted.overlay.opaque" && v == "y")
				delete(h.Xattrs, k) //nolint:staticcheck
			}
		}
		switch h.Typeflag {
		case tar.TypeChar:
			if h.Devmajor != 0 || h.Devminor != 0 {
				return fmt.Errorf("%s is a device", h.Name)
			}
			dir, base := path.Split(clean)
			if err := tw.WriteHeader(&tar.Header{Name: dir + ".wh." + base, Typeflag: tar.TypeReg, Mode: 0o600, ModTime: h.ModTime}); err != nil {
				return err
			}
			continue
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		default:
			return fmt.Errorf("%s is neither a file, a directory nor a link", h.Name)
		}
		h.Name = clean
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
		if opaque && h.Typeflag == tar.TypeDir {
			if err := tw.WriteHeader(&tar.Header{Name: clean + "/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o600, ModTime: h.ModTime}); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// applyChanges applies a commit's changes, Dockerfile instructions, to
// c.
func applyChanges(c *ImageConfig, changes []string) error {
	for _, ch := range changes {
		for _, line := range strings.Split(ch, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			ins, arg, _ := strings.Cut(line, " ")
			arg = strings.TrimSpace(arg)
			words := func() []string {
				var w []string
				if strings.HasPrefix(arg, "[") && json.Unmarshal([]byte(arg), &w) == nil {
					return w
				}
				return []string{"/bin/sh", "-c", arg}
			}
			pairs := func() (map[string]string, error) {
				out := map[string]string{}
				if !strings.Contains(strings.Fields(arg)[0], "=") {
					k, v, _ := strings.Cut(arg, " ")
					out[k] = strings.TrimSpace(v)
					return out, nil
				}
				for _, f := range strings.Fields(arg) {
					k, v, ok := strings.Cut(f, "=")
					if !ok {
						return nil, invalid("silo: %s %s: expected key=value", ins, arg)
					}
					if u, err := strconv.Unquote(v); err == nil {
						v = u
					}
					out[k] = v
				}
				return out, nil
			}
			if arg == "" {
				return invalid("silo: %s needs an argument", ins)
			}
			switch strings.ToUpper(ins) {
			case "CMD":
				c.Cmd = words()
			case "ENTRYPOINT":
				c.Entrypoint = words()
			case "ENV":
				kv, err := pairs()
				if err != nil {
					return err
				}
				var env []string
				for k, v := range kv {
					env = append(env, k+"="+v)
				}
				slices.Sort(env)
				c.Env = mergeEnv(c.Env, env)
			case "LABEL":
				kv, err := pairs()
				if err != nil {
					return err
				}
				for k := range kv {
					if reservedLabel(k) {
						return forbidden("silo: label %q is in Silo's own namespace (dev.silo., silo.), which is not the tenant's", k)
					}
				}
				if c.Labels == nil {
					c.Labels = map[string]string{}
				}
				for k, v := range kv {
					c.Labels[k] = v
				}
			case "EXPOSE":
				if c.ExposedPorts == nil {
					c.ExposedPorts = map[string]struct{}{}
				}
				for _, p := range strings.Fields(arg) {
					if !strings.Contains(p, "/") {
						p += "/tcp"
					}
					c.ExposedPorts[p] = struct{}{}
				}
			case "VOLUME":
				if c.Volumes == nil {
					c.Volumes = map[string]struct{}{}
				}
				vs := strings.Fields(arg)
				if strings.HasPrefix(arg, "[") {
					vs = nil
					_ = json.Unmarshal([]byte(arg), &vs)
				}
				for _, v := range vs {
					c.Volumes[v] = struct{}{}
				}
			case "USER":
				c.User = arg
			case "WORKDIR":
				c.WorkingDir = arg
			default:
				return invalid("silo: %s is not a change commit takes (CMD, ENTRYPOINT, ENV, EXPOSE, LABEL, USER, WORKDIR, VOLUME)", ins)
			}
		}
	}
	return nil
}

// containerConfig is the image config a commit of c starts from: its
// image's, with what its create set over it.
func containerConfig(base ImageConfig, c *Container) ImageConfig {
	out := base
	get := func(k string) any { return c.Config[k] }
	strsOf := func(v any) []string {
		var ss []string
		if l, ok := v.([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok {
					ss = append(ss, s)
				}
			}
		}
		if s, ok := v.(string); ok && s != "" {
			ss = []string{"/bin/sh", "-c", s}
		}
		return ss
	}
	if v := strsOf(get("Cmd")); len(v) > 0 {
		out.Cmd = v
	}
	if v := strsOf(get("Entrypoint")); len(v) > 0 {
		out.Entrypoint = v
	}
	if v := strsOf(get("Env")); len(v) > 0 {
		out.Env = mergeEnv(base.Env, v)
	}
	if v, ok := get("WorkingDir").(string); ok && v != "" {
		out.WorkingDir = v
	}
	if v, ok := get("User").(string); ok && v != "" {
		out.User = v
	}
	if m, ok := get("Labels").(map[string]any); ok && len(m) > 0 {
		out.Labels = map[string]string{}
		for k, v := range base.Labels {
			out.Labels[k] = v
		}
		for k, v := range stringMap(m) {
			if !reservedLabel(k) {
				out.Labels[k] = v
			}
		}
	}
	return out
}

// commit is POST /commit: a sandbox container's writable layer, from
// runsc tar rootfs-upper with its whiteouts checked, as a new image
// over the container's own (§7, "commit"). The vm class is 501.
func (s *Server) commit(q *request) error {
	ref := q.query("container")
	if ref == "" {
		return s.refuse(q, invalid("silo: container is required"))
	}
	if b := strings.TrimSpace(string(q.raw)); b != "" && b != "null" && b != "{}" {
		return s.refuse(q, invalid("silo: a commit's config body is not served; give the changes as changes"))
	}
	var tag string
	if repo := q.query("repo"); repo != "" {
		t := repo
		if tg := q.query("tag"); tg != "" {
			t += ":" + tg
		}
		var err error
		if tag, err = normalize(t); err != nil || strings.Contains(tag, "@") {
			return s.refuse(q, invalid("invalid reference format: %s", t))
		}
	}
	var c *Container
	var base *ImageRecord
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if c, err = s.container(q.ctx(), tx, q.target.ID, ref); err != nil {
			return err
		}
		base, err = row[ImageRecord](q.ctx(), tx, q.target.ID, kindImage, imageKey(c.ImageID))
		if err == nil && base == nil {
			err = conflict("silo: container %s's image is gone", c.ID[:12])
		}
		return err
	})
	if err != nil {
		return s.refuse(q, err)
	}
	if c.Class == runtime.VM {
		return s.refuse(q, errCommitVM)
	}
	cm, ok := s.drivers[c.Class].(runtime.Committer)
	if !ok {
		return s.refuse(q, unsupported("silo: commit of a %s container is not served by this host", c.Class))
	}
	if s.opts.Content == nil {
		return s.refuse(q, errNoContent)
	}
	cfg := containerConfig(base.Config, c)
	if err := applyChanges(&cfg, q.r.URL.Query()["changes"]); err != nil {
		return s.refuse(q, err)
	}
	m, _, err := s.manifestOf(q.ctx(), base)
	if err != nil {
		return s.refuse(q, err)
	}
	free, err := s.freeDisk(q.ctx(), q.target.ID)
	if err != nil {
		return s.refuse(q, err)
	}
	ctx := context.WithoutCancel(q.ctx())
	lease := "silo-commit-" + c.ID[:12] + "-" + newID()[:8]
	_ = s.opts.Content.Lease(ctx, lease, time.Now().Add(time.Hour))
	defer s.opts.Content.DeleteLease(ctx, lease)
	d := s.drivers[c.Class]
	paused := false
	if q.query("pause") == "" || q.flag("pause") {
		if err := d.Pause(q.ctx(), c.ID); err == nil {
			paused = true
		} else if !errors.Is(err, runtime.ErrState) {
			return s.refuse(q, err)
		}
	}
	layer, size, err := func() (string, int64, error) {
		if paused {
			defer d.Resume(ctx, c.ID)
		}
		rc, err := cm.RootfsUpper(q.ctx(), c.ID)
		if err != nil {
			return "", 0, err
		}
		defer rc.Close()
		in, err := s.opts.Content.Ingest(ctx, lease+"-layer", lease)
		if err != nil {
			return "", 0, err
		}
		defer in.Close()
		if err := in.Truncate(0); err != nil {
			return "", 0, err
		}
		cw := &cappedWriter{w: in, max: free}
		if err := whiteoutLayer(cw, rc); err != nil {
			_ = s.opts.Content.Abort(ctx, lease+"-layer")
			if errors.Is(err, errTooBig) {
				return "", 0, forbidden("silo: commit: the layer is over the silo's free disk_gb (%d bytes)", free)
			}
			return "", 0, invalid("silo: commit: the container's writable layer: %v", err)
		}
		dg := in.Digest("sha256")
		return dg, in.Offset(), in.Commit(ctx, in.Offset(), dg)
	}()
	if err != nil {
		return s.refuse(q, err)
	}
	// The config: the base image's, with the container's settings and
	// the changes, and the new layer's diff id (an uncompressed layer's
	// digest is its diff id).
	baseCfg, err := s.readBlob(ctx, m.Config.Digest, maxJSON)
	if err != nil {
		return s.refuse(q, err)
	}
	var file map[string]any
	if err := json.Unmarshal(baseCfg, &file); err != nil {
		return s.refuse(q, invalid("silo: the image's config is not JSON: %v", err))
	}
	now := time.Now().UTC()
	cj, _ := json.Marshal(cfg)
	var cmap map[string]any
	_ = json.Unmarshal(cj, &cmap)
	file["config"] = cmap
	file["created"] = now.Format(time.RFC3339Nano)
	if a := q.query("author"); a != "" {
		file["author"] = a
	}
	rootfs, _ := file["rootfs"].(map[string]any)
	if rootfs == nil {
		rootfs = map[string]any{"type": "layers"}
	}
	ids, _ := rootfs["diff_ids"].([]any)
	rootfs["diff_ids"] = append(ids, layer)
	file["rootfs"] = rootfs
	hist, _ := file["history"].([]any)
	file["history"] = append(hist, map[string]any{"created": now.Format(time.RFC3339Nano), "created_by": "silo commit " + c.ID[:12],
		"comment": q.query("comment"), "author": q.query("author")})
	fb, _ := json.Marshal(file)
	cfgDigest, err := s.writeBlob(ctx, lease+"-config", lease, fb)
	if err != nil {
		return s.refuse(q, err)
	}
	nm := imageManifest{SchemaVersion: 2, MediaType: mediaOCIManifest,
		Config: &descriptor{MediaType: mediaOCIConfig, Digest: cfgDigest, Size: int64(len(fb))},
		Layers: append(slices.Clone(m.Layers), descriptor{MediaType: mediaOCILayer, Digest: layer, Size: size})}
	mb, _ := json.Marshal(nm)
	mDigest, err := s.writeBlob(ctx, lease+"-manifest", lease, mb)
	if err != nil {
		return s.refuse(q, err)
	}
	img := &ImageRecord{ID: cfgDigest, Size: base.Size + size, Created: now, Config: cfg, Manifest: mDigest, MediaType: mediaOCIManifest}
	s.mu.Lock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		if old, err := row[ImageRecord](q.ctx(), tx, q.target.ID, kindImage, imageKey(img.ID)); err != nil || old != nil {
			return err
		}
		charges, err := s.charge(q.ctx(), tx, q, Object(q.target.ID, kindImage, imageKey(img.ID)), line{ClassDiskGB, gib(size)})
		if err != nil {
			return err
		}
		if tag != "" {
			if err := s.retag(q.ctx(), tx, q.target.ID, img, tag); err != nil {
				return err
			}
		}
		if err := put(q.ctx(), tx, q.target.ID, kindImage, imageKey(img.ID), imageKey(img.ID), img); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"container": c.ID, "id": img.ID, "tag": tag, "layer": layer, "bytes": size}, charges)
	})
	s.mu.Unlock()
	if err != nil {
		return s.refuse(q, err)
	}
	writeJSON(q.w, http.StatusCreated, map[string]string{"Id": img.ID})
	return nil
}

// cappedWriter refuses to write past max bytes.
type cappedWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.max {
		return 0, errTooBig
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// auth is POST /auth, docker login, which validates and stores nothing
// (§7, "docker login"): registry.silo.internal needs no credential and
// answers Login Succeeded without looking; an upstream's is tried
// against its token endpoint, through the egress proxy as the target,
// by Options.Login. The credential stays in the client's config.
func (s *Server) auth(q *request) error {
	var body struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
		IdentityToken string `json:"identitytoken"`
	}
	b, err := io.ReadAll(io.LimitReader(q.r.Body, 1<<20))
	if err != nil {
		return invalid("silo: reading the body: %v", err)
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return invalid("invalid JSON: %v", err)
	}
	server := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(body.ServerAddress, "https://"), "http://"), "/")
	if h, _, ok := strings.Cut(server, "/"); ok {
		server = h
	}
	switch server {
	case "", "index.docker.io", "registry-1.docker.io":
		server = "docker.io"
	}
	if server == imagerepos.Host || strings.HasPrefix(server, imagerepos.Host+":") {
		return ok(q, map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
	}
	if s.opts.Login == nil {
		return unavailable("silo: this silod validates no upstream credentials")
	}
	if err := s.opts.Login(q.ctx(), q.target.ID, server, body.Username, body.Password); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return e
		}
		return errorf(http.StatusUnauthorized, "Get \"https://%s/v2/\": unauthorized: incorrect username or password", server)
	}
	return ok(q, map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
}
