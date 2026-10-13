package docker

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// docker cp is HEAD, GET and PUT /containers/{id}/archive, through the
// driver's CopyOut and CopyIn (local-backend.md §4, "Archive (docker
// cp)"). A path is the container's: it is cleaned as a path in the
// container's root, never looked up on the host, and the driver
// resolves it there without following links out of it (the O_NOFOLLOW
// discipline of pkg/harness). An archive from the tenant is parsed
// here, entry by entry, before the driver sees it.

// pathStat is Docker's X-Docker-Container-Path-Stat.
type pathStat struct {
	Name       string      `json:"name"`
	Size       int64       `json:"size"`
	Mode       fs.FileMode `json:"mode"`
	Mtime      time.Time   `json:"mtime"`
	LinkTarget string      `json:"linkTarget"`
}

// archivePath is the call's path, in the container's root.
func archivePath(q *request) (string, error) {
	p := q.query("path")
	switch {
	case p == "":
		return "", invalid("silo: no path given")
	case strings.ContainsRune(p, 0):
		return "", invalid("silo: path %q has a NUL in it", p)
	}
	return path.Clean("/" + p), nil
}

// copyOut is p in c as a tar archive, and what it is: the archive's
// first entry, or a directory if that is something under p. head is
// the archive's bytes read so far, which come before rest.
func (s *Server) copyOut(q *request, c *Container, p string) (st pathStat, head []byte, rest io.ReadCloser, err error) {
	d := s.drivers[c.Class]
	if d == nil {
		return st, nil, nil, conflict("silo: container %s is gone from its runtime; remove it", c.ID)
	}
	notThere := notFound("Could not find the file %s in container %s", p, q.params["id"])
	rc, err := d.CopyOut(q.ctx(), c.ID, p)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil, nil, notThere
	} else if err != nil {
		return st, nil, nil, runtimeErr(err, c)
	}
	var buf bytes.Buffer
	h, err := tar.NewReader(io.TeeReader(rc, &buf)).Next()
	if err != nil {
		_ = rc.Close()
		if err == io.EOF || errors.Is(err, fs.ErrNotExist) {
			return st, nil, nil, notThere
		}
		return st, nil, nil, runtimeErr(err, c)
	}
	st = pathStat{Name: path.Base(p), Mode: fs.ModeDir | 0o755}
	if strings.TrimSuffix(path.Clean(h.Name), "/") == path.Base(p) {
		st.Size, st.Mode, st.Mtime, st.LinkTarget = h.Size, h.FileInfo().Mode(), h.ModTime.UTC(), h.Linkname
	}
	return st, buf.Bytes(), rc, nil
}

func setPathStat(q *request, st pathStat) {
	b, _ := json.Marshal(st)
	q.w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(b))
}

// archiveStat is HEAD /containers/{id}/archive: what path is, in the
// stat header.
func (s *Server) archiveStat(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	p, err := archivePath(q)
	if err != nil {
		return err
	}
	st, _, rc, err := s.copyOut(q, c, p)
	if err != nil {
		return err
	}
	_ = rc.Close()
	setPathStat(q, st)
	q.w.WriteHeader(http.StatusOK)
	return nil
}

// archiveGet is GET /containers/{id}/archive: path as a tar archive,
// with its stat header.
func (s *Server) archiveGet(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	p, err := archivePath(q)
	if err != nil {
		return err
	}
	st, head, rc, err := s.copyOut(q, c, p)
	if err != nil {
		return err
	}
	defer rc.Close()
	setPathStat(q, st)
	q.w.Header().Set("Content-Type", "application/x-tar")
	q.w.WriteHeader(http.StatusOK)
	if _, err := q.w.Write(head); err == nil {
		_, _ = io.Copy(q.w, rc)
	}
	return nil
}

// archivePut is PUT /containers/{id}/archive: a tar archive extracted
// at path, a directory in the container. It lands in the container's
// writable layer, or a volume, whose disk_gb is charged already, so it
// is capped at the container's disk_gb rather than charged again.
func (s *Server) archivePut(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	dir, err := archivePath(q)
	if err != nil {
		return s.refuse(q, err)
	}
	if !writable(c, dir) {
		return s.refuse(q, forbidden("silo: %s is on a read-only filesystem in container %s", dir, q.params["id"]))
	}
	d := s.drivers[c.Class]
	if d == nil {
		return s.refuse(q, conflict("silo: container %s is gone from its runtime; remove it", c.ID))
	}
	limit := c.DiskGB << 30
	tooBig := forbidden("silo: cp: the archive is over the container's disk_gb (%d GiB)", c.DiskGB)
	if q.r.ContentLength > limit {
		return s.refuse(q, tooBig)
	}
	in := &countingReader{r: q.r.Body, max: limit}
	pr, pw := io.Pipe()
	checked := make(chan error, 1)
	go func() {
		err := copyArchive(pw, in, limit, q.flag("copyUIDGID"))
		pw.CloseWithError(err)
		checked <- err
	}()
	err = d.CopyIn(q.ctx(), c.ID, dir, pr)
	pr.Close()
	switch cerr := <-checked; {
	case errors.Is(cerr, errTooBig):
		err = tooBig
	case cerr != nil && !errors.Is(cerr, io.ErrClosedPipe):
		if asError(cerr).Code == http.StatusInternalServerError {
			cerr = invalid("silo: cp: reading the archive: %v", cerr)
		}
		err = cerr
	case errors.Is(err, fs.ErrNotExist):
		err = notFound("Could not find the file %s in container %s", dir, q.params["id"])
	case err != nil:
		err = runtimeErr(err, c)
	}
	if err != nil {
		return s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"id": c.ID, "path": dir, "bytes": in.n}, nil)
	q.w.WriteHeader(http.StatusOK)
	return nil
}

// writable is whether dir is on a writable filesystem in c: its
// deepest mount's, or its root's.
func writable(c *Container, dir string) bool {
	ro, _ := c.HostConfig["ReadonlyRootfs"].(bool)
	depth := -1
	for _, m := range c.Mounts {
		if (dir == m.Target || strings.HasPrefix(dir, strings.TrimSuffix(m.Target, "/")+"/")) && len(m.Target) > depth {
			ro, depth = m.ReadOnly, len(m.Target)
		}
	}
	return !ro
}

// copyArchive checks a tenant's archive for docker cp and writes it
// again to w, as the driver extracts it: each name inside the
// destination, regular files, directories and links only, no extended
// attributes, and owned by the container's root unless keepOwners
// (docker cp -a). Its files may hold no more than max bytes. A name
// under a symlink the archive itself made is refused, so that an
// extraction never follows a link the archive planted; the driver
// keeps to the container's root for the links already in it.
func copyArchive(w io.Writer, r io.Reader, max int64, keepOwners bool) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	links := map[string]bool{}
	var size int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name, err := entryName(h.Name)
		if err != nil {
			return err
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if links[p] {
				return invalid("silo: cp: archive entry %q is under %s, a symlink the archive made", h.Name, p)
			}
		}
		out := &tar.Header{Name: name, Typeflag: h.Typeflag, Mode: h.Mode & 0o7777, ModTime: h.ModTime, Format: tar.FormatPAX}
		if keepOwners {
			out.Uid, out.Gid = h.Uid, h.Gid
		}
		delete(links, name)
		switch h.Typeflag {
		case tar.TypeReg:
			if size += h.Size; size > max {
				return errTooBig
			}
			out.Typeflag, out.Size = tar.TypeReg, h.Size
		case tar.TypeDir:
			if name == "." {
				continue // the destination itself
			}
		case tar.TypeSymlink:
			out.Linkname = h.Linkname
			links[name] = true
		case tar.TypeLink:
			if out.Linkname, err = entryName(h.Linkname); err != nil {
				return err
			}
		case tar.TypeChar, tar.TypeBlock:
			return forbidden("silo: cp: archive entry %q is a device, which is not allowed in a silo", h.Name)
		default:
			return invalid("silo: cp: archive entry %q is of type %q, which cp does not copy", h.Name, h.Typeflag)
		}
		if name == "." {
			return invalid("silo: cp: archive entry %q is the destination itself", h.Name)
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// entryName is an archive entry's name relative to the destination,
// which it may not leave.
func entryName(n string) (string, error) {
	if strings.ContainsRune(n, 0) {
		return "", invalid("silo: cp: archive entry %q has a NUL in it", n)
	}
	clean := path.Clean(strings.TrimLeft(n, "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", invalid("silo: cp: archive entry %q leaves the destination", n)
	}
	return clean, nil
}
