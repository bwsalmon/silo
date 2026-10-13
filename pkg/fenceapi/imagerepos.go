package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"time"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// KindRegistryDelete is the audit kind of untags, manifest deletes and
// repo deletes (local-registry.md, "Audit", call L67). The registry
// service writes the other registry records.
const KindRegistryDelete = "registry.delete"

// ImageReposClass is the count class an image repo holds while it
// exists (local-registry.md, §3, call L58), handed down like repos. The
// root's capacity of it is unlimited unless bootstrap is given one.
var ImageReposClass = ledger.Key{Kind: "silo", Class: "image_repos", Region: ledger.Global}

// The Ops image repo calls resolve their silo with, for the
// ManageChecker: a grant's registry resource type at read, or at write.
const (
	opRegistryRead  Op = "registry.read"
	opRegistryWrite Op = "registry.write"
)

// ImageRepoObject is an image repo, as the API shows it
// (local-registry.md, §3).
type ImageRepoObject struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	Silo         string    `json:"silo"`
	State        string    `json:"state"`
	Ref          string    `json:"ref"`
	RefByID      string    `json:"ref_by_id"`
	Tags         int       `json:"tags"`
	Manifests    int       `json:"manifests"`
	ChargedBytes int64     `json:"charged_bytes"`
	CreateTime   time.Time `json:"create_time"`
	LastPushTime time.Time `json:"last_push_time"`
}

// ImageTag is one tag of a repo, and the manifest it points at.
type ImageTag struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// ImageManifest is one manifest a repo holds, with the tags on it and
// its charged size: its own and its references', each once. Referenced
// is set when another manifest of the repo references it, as an index
// does its children, which `silo registry prune` leaves to their index.
type ImageManifest struct {
	Digest       string    `json:"digest"`
	MediaType    string    `json:"media_type"`
	ChargedBytes int64     `json:"charged_bytes"`
	Tags         []string  `json:"tags"`
	Referenced   bool      `json:"referenced"`
	CreateTime   time.Time `json:"create_time"`
}

func imageRepoObject(ctx context.Context, tx store.Tx, t *tree.Silo, r *imagerepos.Repo) (*ImageRepoObject, error) {
	ms, err := imagerepos.Manifests(ctx, tx, r.ID)
	if err != nil {
		return nil, err
	}
	tags, err := imagerepos.Tags(ctx, tx, r.ID)
	if err != nil {
		return nil, err
	}
	return &ImageRepoObject{Name: r.Name, ID: r.ID, Silo: t.Path, State: r.State,
		Ref: imagerepos.Ref(t.Path, r.Name), RefByID: imagerepos.RefByID(t.ID, r.Name),
		Tags: len(tags), Manifests: len(ms), ChargedBytes: imagerepos.Charged(ms),
		CreateTime: r.Created, LastPushTime: r.LastPush}, nil
}

func imageRepoChargeObject(id string) string { return "image_repo:" + id }

// imageRepoSilo resolves the silo of an image repo call (local-registry.md,
// "Who may do what", call L63): the silo itself and its ancestors may
// make any, a manage grant's holder those its registry resource type's
// access covers, and anyone else gets NOT_FOUND.
func (s *Server) imageRepoSilo(ctx context.Context, tx store.Tx, c *Caller, ref string, write bool) ([]*tree.Silo, error) {
	op := opRegistryRead
	if write {
		op = opRegistryWrite
	}
	chain, _, err := s.resolve(ctx, tx, c, ref, op)
	return chain, err
}

func imageRepoNotFound(t *tree.Silo, name string) *Error {
	return errorf(NotFound, "%s has no image repo %q", t.Path, name).with("silo", t.Path).with("image_repo", name)
}

// imageRepo reads the silo at ref and its image repo name for a call.
// A repo being deleted is NOT_FOUND, as the registry answers it.
func (s *Server) imageRepo(ctx context.Context, tx store.Tx, q *request, ref, name string, write bool, kind string) ([]*tree.Silo, *imagerepos.Repo, error) {
	chain, err := s.imageRepoSilo(ctx, tx, &q.caller, ref, write)
	if err != nil {
		return nil, nil, err
	}
	t := chain[len(chain)-1]
	if kind != "" {
		q.note(t, kind)
	}
	r, err := imagerepos.ByName(ctx, tx, t.ID, name)
	if errors.Is(err, imagerepos.ErrNotFound) || err == nil && r.State == imagerepos.StateDeleting {
		return nil, nil, imageRepoNotFound(t, name)
	}
	return chain, r, err
}

// CreateImageRepo makes the image repo name in the last silo of chain,
// as a first push does (local-registry.md, "Created by the first push",
// call L58), charging it one of the silo's image_repos. The caller's
// write access to chain is the registry's to have checked; this checks
// the caller's own control policy, since creating a repo is a
// control-plane call that a sealed or read silo may not make (call
// L65). A refusal is the registry's to audit, as registry.denied, in a
// transaction of its own.
func CreateImageRepo(ctx context.Context, tx store.Tx, chain []*tree.Silo, c Caller, name string) (*imagerepos.Repo, error) {
	t := chain[len(chain)-1]
	if !imagerepos.ValidName(name) {
		return nil, errorf(InvalidArgument, "image repo name %q: want one component of [a-z0-9] separated by -, _ or __, 1 to 63 characters, not starting with id_", name).with("image_repo", name)
	}
	if mine, err := tree.Ancestry(ctx, tx, c.Silo); err == nil {
		eff, err := effectiveOf(ctx, tx, mine, nil)
		if err != nil {
			return nil, err
		}
		if eff.Control != ControlFull {
			return nil, errorf(PermissionDenied, "creating a repo is a control-plane call; this silo's control policy is %s", eff.Control).
				with("image_repo", name)
		}
	} else if !errors.Is(err, tree.ErrNotFound) {
		return nil, err
	}
	if err := lockChain(ctx, tx, chain); err != nil {
		return nil, err
	}
	if _, err := imagerepos.ByName(ctx, tx, t.ID, name); err == nil {
		return nil, errorf(AlreadyExists, "%s already has an image repo %s", t.Path, name).with("silo", t.Path).with("image_repo", name)
	} else if !errors.Is(err, imagerepos.ErrNotFound) {
		return nil, err
	}
	r := &imagerepos.Repo{ID: imagerepos.NewID(), Silo: t.ID, Name: name}
	for {
		if _, err := imagerepos.Get(ctx, tx, r.ID); errors.Is(err, imagerepos.ErrNotFound) {
			break
		} else if err != nil {
			return nil, err
		}
		r.ID = imagerepos.NewID()
	}
	if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: imageRepoChargeObject(r.ID), Silo: t.ID, Via: chargeVia(chain, c),
		Amounts: []ledger.Amount{{Key: ImageReposClass, Amount: ledger.Units(1)}}}); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(map[string]string{"silo": t.ID, "name": r.Name, "what": "create"})
	var err error
	if r.Seq, err = tx.Append(ctx, imagerepos.ChangeKind, r.ID, string(data)); err != nil {
		return nil, err
	}
	if err := imagerepos.Create(ctx, tx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// listImageRepos is GET .../imageRepos: the silo's image repos, and
// with recursive=true its descendants' too, each with its silo's path.
func (s *Server) listImageRepos(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	recursive := q.r.URL.Query().Get("recursive") == "true"
	var items []any
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, err := s.imageRepoSilo(ctx, tx, &q.caller, ref, false)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		silos := []string{t.ID}
		if recursive {
			if silos, err = tree.Subtree(ctx, tx, t.ID); err != nil {
				return err
			}
		}
		rs, err := imagerepos.Of(ctx, tx, silos...)
		if err != nil {
			return err
		}
		seen := map[string]*tree.Silo{}
		for _, r := range rs {
			if r.State == imagerepos.StateDeleting {
				continue
			}
			st, ok := seen[r.Silo]
			if !ok {
				if st, err = tree.Get(ctx, tx, r.Silo); err != nil {
					return err
				}
				seen[r.Silo] = st
			}
			o, err := imageRepoObject(ctx, tx, st, r)
			if err != nil {
				return err
			}
			items = append(items, o)
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := map[string]any{"image_repos": []any{}}
	if offset < len(items) {
		end := min(len(items), offset+size)
		out["image_repos"] = items[offset:end]
		if end < len(items) {
			out["next_page_token"] = nextToken(offset, size, true)
		}
	}
	writeJSON(q.w, http.StatusOK, out)
}

// getImageRepo is GET .../imageRepos/{repo}.
func (s *Server) getImageRepo(q *request, ref, name string) {
	ctx := q.r.Context()
	var out *ImageRepoObject
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, r, err := s.imageRepo(ctx, tx, q, ref, name, false, "")
		if err != nil {
			return err
		}
		out, err = imageRepoObject(ctx, tx, chain[len(chain)-1], r)
		return err
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// imageRepoTags is GET .../imageRepos/{repo}/tags: the repo's tags and
// the digests they point at, and every manifest it holds, untagged ones
// included, with each one's charged size.
func (s *Server) imageRepoTags(q *request, ref, name string) {
	ctx := q.r.Context()
	out := map[string]any{}
	err := s.tx(ctx, func(tx store.Tx) error {
		_, r, err := s.imageRepo(ctx, tx, q, ref, name, false, "")
		if err != nil {
			return err
		}
		ms, err := imagerepos.Manifests(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		tags, err := imagerepos.Tags(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		on := map[string][]string{}
		ts := []ImageTag{}
		for _, t := range tags {
			ts = append(ts, ImageTag{Tag: t.Tag, Digest: t.Digest})
			on[t.Digest] = append(on[t.Digest], t.Tag)
		}
		referenced := map[string]bool{}
		for _, m := range ms {
			for _, r := range m.Refs {
				referenced[r.Digest] = true
			}
		}
		mo := []ImageManifest{}
		for _, m := range ms {
			mo = append(mo, ImageManifest{Digest: m.Digest, MediaType: m.MediaType, ChargedBytes: imagerepos.SizeOf(m),
				Tags: append([]string{}, on[m.Digest]...), Referenced: referenced[m.Digest], CreateTime: m.Created})
		}
		out["tags"], out["manifests"] = ts, mo
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// untagImage is DELETE .../imageRepos/{repo}/tags/{tag}: the manifest
// stays, untagged, pullable by digest and charged (local-registry.md,
// §10).
func (s *Server) untagImage(q *request, ref, name, tag string) (any, error) {
	ctx := q.r.Context()
	var out *ImageRepoObject
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, r, err := s.imageRepo(ctx, tx, q, ref, name, true, KindRegistryDelete)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		if err := imagerepos.Untag(ctx, tx, r.ID, tag); errors.Is(err, imagerepos.ErrTagNotFound) {
			return errorf(NotFound, "%s's image repo %s has no tag %q", t.Path, r.Name, tag).with("image_repo", r.Name).with("tag", tag)
		} else if err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, KindRegistryDelete, q.requestID, "OK",
			map[string]any{"image_repo": r.Name, "id": r.ID, "reference": tag, "refunded_bytes": 0}); err != nil {
			return err
		}
		if out, err = imageRepoObject(ctx, tx, t, r); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// deleteImageManifest is DELETE .../imageRepos/{repo}/manifests/{digest}:
// the manifest and every tag on it, and an index's children nothing
// else in the repo holds. It refunds the bytes no other manifest of the
// repo references.
func (s *Server) deleteImageManifest(q *request, ref, name, digest string) (any, error) {
	ctx := q.r.Context()
	if !imagerepos.ValidDigest(digest) {
		return nil, errorf(InvalidArgument, "digest %q: want sha256:<64 hex> or sha512:<128 hex>", digest).with("field", "digest")
	}
	var out *ImageRepoObject
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, r, err := s.imageRepo(ctx, tx, q, ref, name, true, KindRegistryDelete)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		before, err := imagerepos.Manifests(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		gone, err := imagerepos.DeleteManifest(ctx, tx, r.ID, digest)
		if errors.Is(err, imagerepos.ErrManifestNotFound) {
			return errorf(NotFound, "%s's image repo %s has no manifest %s", t.Path, r.Name, digest).with("image_repo", r.Name).with("digest", digest)
		} else if err != nil {
			return err
		}
		after, err := imagerepos.Manifests(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, KindRegistryDelete, q.requestID, "OK",
			map[string]any{"image_repo": r.Name, "id": r.ID, "reference": digest, "manifests": gone,
				"refunded_bytes": imagerepos.Charged(before) - imagerepos.Charged(after)}); err != nil {
			return err
		}
		if out, err = imageRepoObject(ctx, tx, t, r); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// deleteImageRepo is DELETE .../imageRepos/{repo}: the repo and
// everything in it, as an operation. With no content store records to
// remove yet (local-registry.md, §4), it is done at once.
func (s *Server) deleteImageRepo(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var out any
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, r, err := s.imageRepo(ctx, tx, q, ref, name, true, KindRegistryDelete)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		if err := dropImageRepo(ctx, tx, t, r, q.caller, q.requestID, nil); err != nil {
			return err
		}
		if q.validateOnly {
			out = map[string]any{"validate_only": true, "deletes": imagerepos.Ref(t.Path, r.Name)}
			return errValidateOnly
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		cj, _ := json.Marshal(q.caller)
		opID := ids.New() + ids.New()
		if _, err := tx.Exec(ctx, `INSERT INTO silo_operations (id, kind, silo, path, caller, state, done, error, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
			opID, KindRegistryDelete, r.ID, path.Join(t.Path, r.Name), string(cj), OpDone, true, store.Millis(now), store.Millis(now)); err != nil {
			return err
		}
		op, err := readOp(ctx, tx, `id = ?`, opID)
		if err != nil {
			return err
		}
		out = op.Operation
		return s.finish(ctx, tx, q, out)
	})
	if err == errValidateOnly {
		return out, nil
	}
	return s.done(q, out, 0, err)
}

// dropImageRepo removes an image repo's rows and gives its silo its
// image_repos count back, audited under the silo with the bytes its
// manifests charged.
func dropImageRepo(ctx context.Context, tx store.Tx, t *tree.Silo, r *imagerepos.Repo, c Caller, requestID string, extra map[string]any) error {
	ms, err := imagerepos.Manifests(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	if _, err := ledger.Refund(ctx, tx, imageRepoChargeObject(r.ID), nil); err != nil {
		return err
	}
	if err := imagerepos.Delete(ctx, tx, r.ID); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"silo": t.ID, "name": r.Name, "what": "delete"})
	if _, err := tx.Append(ctx, imagerepos.ChangeKind, r.ID, string(data)); err != nil {
		return err
	}
	fields := map[string]any{"image_repo": r.Name, "id": r.ID, "reference": "", "refunded_bytes": imagerepos.Charged(ms)}
	for k, v := range extra {
		fields[k] = v
	}
	return Audit(ctx, tx, t, c, KindRegistryDelete, requestID, "OK", fields)
}

// deleteImageReposIn deletes silo t's image repos, as t is deleted with
// force (local-registry.md, "Deleting a silo"): there is no outbound
// copy, so nothing is archived.
func (s *Server) deleteImageReposIn(ctx context.Context, t *tree.Silo, c Caller) error {
	return s.tx(ctx, func(tx store.Tx) error {
		rs, err := imagerepos.Of(ctx, tx, t.ID)
		if err != nil || len(rs) == 0 {
			return err
		}
		chain, err := tree.Ancestry(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		for _, r := range rs {
			if err := dropImageRepo(ctx, tx, t, r, c, "", map[string]any{"reason": "silo deleted"}); err != nil {
				return err
			}
		}
		return nil
	})
}
