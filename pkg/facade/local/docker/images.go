package docker

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
)

// Images pulls images for silos (local-backend.md, "Images"). The
// production puller runs in silod, held to the silo's egress policy
// through the gateway's egress proxy, into silo-containerd's shared
// content store; tests give a fake.
type Images interface {
	// Pull fetches ref (a familiar reference: "alpine:3",
	// "ghcr.io/o/r@sha256:…") for silo. auth is the request's
	// X-Registry-Auth, used for this pull only and never kept.
	Pull(ctx context.Context, silo, ref, auth string) (*Pulled, error)
}

// Pulled is an image as a pull found it.
type Pulled struct {
	// ID is the image's config digest, sha256:<hex>, which the drivers
	// run it by.
	ID string
	// RepoDigest is the manifest's, name@sha256:<hex>.
	RepoDigest string
	// Size is the unpacked size in bytes, which the silo is charged in
	// full whether or not another silo pulled it first.
	Size    int64
	Created time.Time
	Config  ImageConfig
	// Manifest is the digest of the manifest the pull stored in the
	// content store, of media type MediaType; empty if it stored none.
	Manifest  string
	MediaType string
}

// ImageConfig is the part of an image's config a container is made
// from.
type ImageConfig struct {
	Entrypoint   []string            `json:",omitempty"`
	Cmd          []string            `json:",omitempty"`
	Env          []string            `json:",omitempty"`
	WorkingDir   string              `json:",omitempty"`
	User         string              `json:",omitempty"`
	StopSignal   string              `json:",omitempty"`
	ExposedPorts map[string]struct{} `json:",omitempty"`
	Volumes      map[string]struct{} `json:",omitempty"`
	Labels       map[string]string   `json:",omitempty"`
}

var refPart = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)

// normalize writes ref in Docker's familiar form: with a tag (latest if
// it has neither tag nor digest), and without docker.io/library/.
func normalize(ref string) (string, error) {
	name, digest, hasDigest := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "docker.io/"), "library/")
	parts := strings.Split(name, "/")
	for i, p := range parts {
		if i == 0 && len(parts) > 1 && strings.ContainsAny(p, ".:") {
			continue // a registry host
		}
		if !refPart.MatchString(p) {
			return "", invalid("invalid reference format: repository name (%s) must be lowercase", ref)
		}
	}
	switch {
	case hasDigest:
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
			return "", invalid("invalid reference format: %s", ref)
		}
		return name + "@" + digest, nil
	case tag == "":
		tag = "latest"
	}
	return name + ":" + tag, nil
}

func (s *Server) images(ctx context.Context, tx store.Tx, silo string) ([]*ImageRecord, error) {
	return rows[ImageRecord](ctx, tx, silo, kindImage)
}

// image resolves ref to one of silo's images: by id, a unique id
// prefix, or a reference it is tagged with.
func (s *Server) image(ctx context.Context, tx store.Tx, silo, ref string) (*ImageRecord, error) {
	is, err := s.images(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	if n, err := normalize(ref); err == nil {
		for _, i := range is {
			if slices.Contains(i.Tags, n) || slices.Contains(i.RepoDigests, n) {
				return i, nil
			}
		}
	}
	hexOf := func(i *ImageRecord) string { return strings.TrimPrefix(i.ID, "sha256:") }
	if h := strings.TrimPrefix(ref, "sha256:"); len(h) >= 4 {
		if i, ok := find(is, h, hexOf, func(*ImageRecord) string { return "" }); ok {
			return i, nil
		}
	}
	return nil, notFound("No such image: %s", ref)
}

// retag moves tag to img, from whichever of silo's images had it.
func (s *Server) retag(ctx context.Context, tx store.Tx, silo string, img *ImageRecord, tag string) error {
	is, err := s.images(ctx, tx, silo)
	if err != nil {
		return err
	}
	for _, o := range is {
		if o.ID == img.ID || !slices.Contains(o.Tags, tag) {
			continue
		}
		o.Tags = slices.DeleteFunc(o.Tags, func(t string) bool { return t == tag })
		if err := put(ctx, tx, silo, kindImage, imageKey(o.ID), imageKey(o.ID), o); err != nil {
			return err
		}
	}
	if !slices.Contains(img.Tags, tag) {
		img.Tags = append(img.Tags, tag)
	}
	return nil
}

// imageKey is an image's row id: its digest's hex.
func imageKey(id string) string { return strings.TrimPrefix(id, "sha256:") }

// imageCreate pulls fromImage and tag, and nothing else: fromSrc is an
// import, which isn't served.
func (s *Server) imageCreate(q *request) error {
	if q.query("fromSrc") != "" {
		return s.refuse(q, errIO)
	}
	for _, k := range []string{"repo", "message", "changes"} {
		if q.query(k) != "" {
			return s.refuse(q, invalid("silo: parameter %s is not allowed", k))
		}
	}
	if p := q.query("platform"); p != "" && p != "linux" && !strings.HasPrefix(p, "linux/") {
		return s.refuse(q, invalid("silo: platform %s is not available", p))
	}
	from := q.query("fromImage")
	if from == "" {
		return s.refuse(q, invalid("silo: fromImage is required"))
	}
	if t := q.query("tag"); t != "" {
		if strings.HasPrefix(t, "sha256:") {
			from += "@" + t
		} else {
			from += ":" + t
		}
	}
	ref, err := normalize(from)
	if err != nil {
		return s.refuse(q, err)
	}
	var p *Pulled
	pullID := newPullID()
	if _, _, local := localName(ref); local {
		p, err = s.pullLocal(q, ref)
	} else if s.opts.Fetcher != nil {
		p, err = s.pullCache(q, ref, pullID)
	} else if s.opts.Images == nil {
		err = unavailable("silo: this silod pulls no images")
	} else {
		p, err = s.opts.Images.Pull(q.ctx(), q.target.ID, ref, q.r.Header.Get("X-Registry-Auth"))
	}
	if err != nil {
		return s.refuse(q, err)
	}
	s.mu.Lock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		_, err := s.recordPull(q.ctx(), tx, q, p, []string{ref}, map[string]any{"image": ref, "pull_id": pullID})
		return err
	})
	s.mu.Unlock()
	if err != nil {
		return s.refuse(q, err)
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	enc := jsonLines(q.w)
	enc(map[string]any{"status": "Pulling from " + name, "id": tag})
	enc(map[string]any{"status": "Digest: " + strings.TrimPrefix(p.RepoDigest, name+"@")})
	enc(map[string]any{"status": "Status: Downloaded newer image for " + ref})
	return nil
}

func (s *Server) imageList(q *request) error {
	f, err := filters(q, "dangling", "reference", "label")
	if err != nil {
		return err
	}
	var out []map[string]any
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		is, err := s.images(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		for _, i := range is {
			if d, ok := f.one("dangling"); ok && (d == "true") != (len(i.Tags) == 0) {
				continue
			}
			if refs := f["reference"]; len(refs) > 0 && !slices.ContainsFunc(i.Tags, func(t string) bool { return matchRef(refs, t) }) {
				continue
			}
			if !f.labels(i.Config.Labels) {
				continue
			}
			out = append(out, imageSummary(i))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if out == nil {
		out = []map[string]any{}
	}
	return ok(q, out)
}

// matchRef reports whether tag matches one of the reference filters: a
// name, or name:tag.
func matchRef(refs []string, tag string) bool {
	name, _, _ := strings.Cut(tag, ":")
	for _, r := range refs {
		if r == tag || r == name {
			return true
		}
		if n, err := normalize(r); err == nil && n == tag {
			return true
		}
	}
	return false
}

func imageSummary(i *ImageRecord) map[string]any {
	return map[string]any{
		"Id": i.ID, "ParentId": "", "RepoTags": nonNil(i.Tags), "RepoDigests": nonNil(i.RepoDigests),
		"Created": i.Created.Unix(), "Size": i.Size, "SharedSize": -1, "VirtualSize": i.Size,
		"Labels": nonNilMap(i.Config.Labels), "Containers": -1,
	}
}

func (s *Server) imageInspect(q *request) error {
	var img *ImageRecord
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		img, err = s.image(q.ctx(), tx, q.target.ID, q.params["name"])
		return err
	})
	if err != nil {
		return err
	}
	return ok(q, map[string]any{
		"Id": img.ID, "RepoTags": nonNil(img.Tags), "RepoDigests": nonNil(img.RepoDigests), "Parent": "",
		"Comment": "", "Created": img.Created.Format(time.RFC3339Nano), "Config": imageConfigJSON(img.Config),
		"Architecture": goarch(), "Os": "linux", "Size": img.Size, "VirtualSize": img.Size,
		"RootFS": map[string]any{"Type": "layers"}, "Metadata": map[string]any{"LastTagTime": ""},
	})
}

func imageConfigJSON(c ImageConfig) map[string]any {
	return map[string]any{
		"Entrypoint": c.Entrypoint, "Cmd": c.Cmd, "Env": nonNil(c.Env), "WorkingDir": c.WorkingDir,
		"User": c.User, "StopSignal": c.StopSignal, "ExposedPorts": c.ExposedPorts, "Volumes": c.Volumes,
		"Labels": nonNilMap(c.Labels),
	}
}

func (s *Server) imageHistory(q *request) error {
	var img *ImageRecord
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		img, err = s.image(q.ctx(), tx, q.target.ID, q.params["name"])
		return err
	})
	if err != nil {
		return err
	}
	return ok(q, []map[string]any{{"Id": img.ID, "Created": img.Created.Unix(), "CreatedBy": "",
		"Tags": nonNil(img.Tags), "Size": img.Size, "Comment": ""}})
}

func (s *Server) imageTag(q *request) error {
	repo := q.query("repo")
	if repo == "" {
		return s.refuse(q, invalid("repository name must have at least one component"))
	}
	if t := q.query("tag"); t != "" {
		repo += ":" + t
	}
	tag, err := normalize(repo)
	if err != nil || strings.Contains(tag, "@") {
		return s.refuse(q, invalid("invalid reference format: %s", repo))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		img, err := s.image(q.ctx(), tx, q.target.ID, q.params["name"])
		if err != nil {
			return err
		}
		if err := s.retag(q.ctx(), tx, q.target.ID, img, tag); err != nil {
			return err
		}
		if err := put(q.ctx(), tx, q.target.ID, kindImage, imageKey(img.ID), imageKey(img.ID), img); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"tag": tag}, nil)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	q.w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *Server) imageDelete(q *request) error {
	ref := q.params["name"]
	var out []map[string]string
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		img, err := s.image(q.ctx(), tx, q.target.ID, ref)
		if err != nil {
			return err
		}
		// A tag of an image with others is only untagged.
		if n, err := normalize(ref); err == nil && slices.Contains(img.Tags, n) && len(img.Tags) > 1 {
			img.Tags = slices.DeleteFunc(img.Tags, func(t string) bool { return t == n })
			out = append(out, map[string]string{"Untagged": n})
			if err := put(q.ctx(), tx, q.target.ID, kindImage, imageKey(img.ID), imageKey(img.ID), img); err != nil {
				return err
			}
			return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"untagged": n}, nil)
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.ImageID == img.ID && !q.flag("force") {
				return conflict("conflict: unable to delete %s (must be forced) - image is being used by container %s", imageKey(img.ID)[:12], c.ID[:12])
			}
		}
		charges, err := s.deleteImage(q.ctx(), tx, q.target.ID, img)
		if err != nil {
			return err
		}
		for _, t := range img.Tags {
			out = append(out, map[string]string{"Untagged": t})
		}
		out = append(out, map[string]string{"Deleted": img.ID})
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"deleted": img.ID}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return ok(q, out)
}

func (s *Server) deleteImage(ctx context.Context, tx store.Tx, silo string, img *ImageRecord) ([]fenceapi.ChargeLine, error) {
	charges, err := refund(ctx, tx, Object(silo, kindImage, imageKey(img.ID)))
	if err != nil {
		return nil, err
	}
	return charges, remove(ctx, tx, silo, kindImage, imageKey(img.ID))
}

// imagePrune deletes the images no container uses: the untagged ones,
// or every one with the filter dangling=false.
func (s *Server) imagePrune(q *request) error {
	f, err := filters(q, "dangling", "label", "until")
	if err != nil {
		return s.refuse(q, err)
	}
	all := false
	if d, ok := f.one("dangling"); ok {
		all = d == "false" || d == "0"
	}
	deleted := []map[string]string{}
	var reclaimed int64
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		is, err := s.images(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		var charges []fenceapi.ChargeLine
		for _, i := range is {
			if (!all && len(i.Tags) > 0) || slices.ContainsFunc(cs, func(c *Container) bool { return c.ImageID == i.ID }) || !f.labels(i.Config.Labels) {
				continue
			}
			c, err := s.deleteImage(q.ctx(), tx, q.target.ID, i)
			if err != nil {
				return err
			}
			charges = append(charges, c...)
			for _, t := range i.Tags {
				deleted = append(deleted, map[string]string{"Untagged": t})
			}
			deleted = append(deleted, map[string]string{"Deleted": i.ID})
			reclaimed += i.Size
		}
		return s.auditTx(q.ctx(), tx, q, "OK", nil, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return ok(q, map[string]any{"ImagesDeleted": deleted, "SpaceReclaimed": reclaimed})
}
