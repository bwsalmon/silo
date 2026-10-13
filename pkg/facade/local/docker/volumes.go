package docker

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
)

// validName is what a container, network or volume name may be, as
// Docker allows them.
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// parseSize reads a size as Docker's options give one: bytes, or a
// number with k, m, g or t (and an optional b or ib).
func parseSize(v string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "b"), "i")
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
		if mult > 1 {
			s = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, invalid("silo: invalid size %q", v)
	}
	return n * mult, nil
}

type volumeCreateBody struct {
	Name       string
	Driver     string
	DriverOpts map[string]string
	Labels     map[string]string
}

func (s *Server) volumeCreate(q *request) error {
	b, err := decode[volumeCreateBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	if b.Driver != "" && b.Driver != "local" {
		return s.refuse(q, forbidden("silo: volume driver %q is not allowed in a silo; only local is", b.Driver))
	}
	size := storage.DefaultVolumeSize
	for k, v := range b.DriverOpts {
		if k != "size" {
			return s.refuse(q, forbidden("silo: volume driver option %q is not allowed in a silo; only size is", k))
		}
		if size, err = parseSize(v); err != nil {
			return s.refuse(q, err)
		}
	}
	if b.Name == "" {
		b.Name = newID()
	} else if !validName.MatchString(b.Name) || len(b.Name) == 1 {
		return s.refuse(q, invalid("create %s: %q includes invalid characters for a local volume name, only \"[a-zA-Z0-9][a-zA-Z0-9_.-]\" are allowed", b.Name, b.Name))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v *Volume
	var charges []fenceapi.ChargeLine
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if v, err = row[Volume](q.ctx(), tx, q.target.ID, kindVolume, b.Name); err != nil || v != nil {
			return err // Docker's create of a volume that exists returns it
		}
		v = &Volume{Name: b.Name, Labels: b.Labels, Size: size}
		if charges, err = s.makeVolume(q.ctx(), tx, q, v); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"name": v.Name, "size": v.Size}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	if err := s.createVolumeDir(q.ctx(), q.target.ID, v); err != nil {
		s.dropVolume(q.ctx(), q.target.ID, v)
		return s.refuse(q, err)
	}
	writeJSON(q.w, 201, volumeJSON(v))
	return nil
}

// makeVolume charges v and keeps it, inside tx. Its directory is made
// once the charge has committed (createVolumeDir).
func (s *Server) makeVolume(ctx context.Context, tx store.Tx, q *request, v *Volume) ([]fenceapi.ChargeLine, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	v.Created = now
	charges, err := s.charge(ctx, tx, q, Object(q.target.ID, kindVolume, v.Name), line{ClassVolumes, 1}, line{ClassDiskGB, gib(v.Size)})
	if err != nil {
		return nil, err
	}
	return charges, put(ctx, tx, q.target.ID, kindVolume, v.Name, v.Name, v)
}

// createVolumeDir makes v's directory through the storage driver: the
// silo's subtree first, which is idempotent.
func (s *Server) createVolumeDir(ctx context.Context, silo string, v *Volume) error {
	if s.opts.Storage == nil {
		return unavailable("silo: this silod keeps no volumes")
	}
	if _, err := s.opts.Storage.Create(ctx, storage.Ref{Silo: silo}, 0); err != nil {
		return err
	}
	_, err := s.opts.Storage.Create(ctx, storage.Ref{Silo: silo, Volume: v.Name}, v.Size)
	return err
}

// volumePath is where v's directory is, for a mount.
func (s *Server) volumePath(ctx context.Context, silo, name string) (string, error) {
	if s.opts.Storage == nil {
		return "", unavailable("silo: this silod keeps no volumes")
	}
	return s.opts.Storage.Create(ctx, storage.Ref{Silo: silo, Volume: name}, 0)
}

// dropVolume undoes a volume whose directory couldn't be made: its row
// and its charge.
func (s *Server) dropVolume(ctx context.Context, silo string, v *Volume) {
	_ = s.st.Tx(context.WithoutCancel(ctx), func(tx store.Tx) error {
		if _, err := refund(ctx, tx, Object(silo, kindVolume, v.Name)); err != nil {
			return err
		}
		return remove(ctx, tx, silo, kindVolume, v.Name)
	})
}

func volumeJSON(v *Volume) map[string]any {
	opts := map[string]string{}
	if v.Size != storage.DefaultVolumeSize {
		opts["size"] = strconv.FormatInt(v.Size, 10)
	}
	return map[string]any{
		"Name": v.Name, "Driver": "local", "Mountpoint": "", "CreatedAt": v.Created.Format(time.RFC3339),
		"Labels": nonNilMap(v.Labels), "Scope": "local", "Options": opts,
	}
}

func (s *Server) volumeList(q *request) error {
	f, err := filters(q, "name", "label", "dangling", "driver")
	if err != nil {
		return err
	}
	out := []map[string]any{}
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		vs, err := rows[Volume](q.ctx(), tx, q.target.ID, kindVolume)
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		for _, v := range vs {
			if !f.match("name", func(x string) bool { return strings.Contains(v.Name, x) }) ||
				!f.match("driver", func(x string) bool { return x == "local" }) || !f.labels(v.Labels) {
				continue
			}
			if d, ok := f.one("dangling"); ok && (d == "true" || d == "1") == (len(usedBy(cs, v.Name)) > 0) {
				continue
			}
			out = append(out, volumeJSON(v))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ok(q, map[string]any{"Volumes": out, "Warnings": []string{}})
}

// usedBy are the containers that mount volume name.
func usedBy(cs []*Container, name string) []string {
	var out []string
	for _, c := range cs {
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name == name {
				out = append(out, c.ID)
				break
			}
		}
	}
	return out
}

func (s *Server) volumeInspect(q *request) error {
	var v *Volume
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		v, err = s.volume(q.ctx(), tx, q.target.ID, q.params["name"])
		return err
	})
	if err != nil {
		return err
	}
	return ok(q, volumeJSON(v))
}

func (s *Server) volumeDelete(q *request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v *Volume
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if v, err = s.volume(q.ctx(), tx, q.target.ID, q.params["name"]); err != nil {
			if q.flag("force") && asError(err).Code == 404 {
				v = nil
				return nil
			}
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		if users := usedBy(cs, v.Name); len(users) > 0 {
			return conflict("remove %s: volume is in use - %v", v.Name, users)
		}
		charges, err := s.deleteVolume(q.ctx(), tx, q.target.ID, v)
		if err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"name": v.Name}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	if v != nil {
		s.deleteVolumeDir(q.ctx(), q.target.ID, v.Name)
	}
	return noContent(q)
}

// deleteVolume refunds v and forgets it, inside tx. Its directory is
// removed once that has committed (deleteVolumeDir).
func (s *Server) deleteVolume(ctx context.Context, tx store.Tx, silo string, v *Volume) ([]fenceapi.ChargeLine, error) {
	charges, err := refund(ctx, tx, Object(silo, kindVolume, v.Name))
	if err != nil {
		return nil, err
	}
	return charges, remove(ctx, tx, silo, kindVolume, v.Name)
}

func (s *Server) deleteVolumeDir(ctx context.Context, silo, name string) {
	if s.opts.Storage != nil {
		_ = s.opts.Storage.Delete(context.WithoutCancel(ctx), storage.Ref{Silo: silo, Volume: name})
	}
}

// volumePrune deletes the volumes no container uses: the anonymous
// ones, or every one with the filter all=true, as Docker 23 and later
// prune.
func (s *Server) volumePrune(q *request) error {
	f, err := filters(q, "label", "all")
	if err != nil {
		return s.refuse(q, err)
	}
	all := false
	if a, ok := f.one("all"); ok {
		all = a == "true" || a == "1"
	}
	if q.minor < 42 {
		all = true // before 1.42, prune took named volumes too
	}
	deleted := []string{}
	var reclaimed int64
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		vs, err := rows[Volume](q.ctx(), tx, q.target.ID, kindVolume)
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		var charges []fenceapi.ChargeLine
		for _, v := range vs {
			if (!all && !v.Anonymous) || len(usedBy(cs, v.Name)) > 0 || !f.labels(v.Labels) {
				continue
			}
			c, err := s.deleteVolume(q.ctx(), tx, q.target.ID, v)
			if err != nil {
				return err
			}
			charges = append(charges, c...)
			deleted = append(deleted, v.Name)
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"deleted": deleted}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	for _, name := range deleted {
		s.deleteVolumeDir(q.ctx(), q.target.ID, name)
	}
	return ok(q, map[string]any{"VolumesDeleted": deleted, "SpaceReclaimed": reclaimed})
}

// checkMount refuses a mount of a volume over its charge (call L38).
func (s *Server) checkMount(silo, name string) error {
	if s.opts.Meter == nil {
		return nil
	}
	err := s.opts.Meter.CheckMount(storage.Ref{Silo: silo, Volume: name})
	if errors.Is(err, storage.ErrOverQuota) {
		return conflict("silo: %v", err)
	}
	return err
}
