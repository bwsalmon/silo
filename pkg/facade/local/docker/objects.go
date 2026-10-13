package docker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/store"
)

// The kinds of docker_objects row.
const (
	kindContainer = "container"
	kindNetwork   = "network"
	kindVolume    = "volume"
	kindImage     = "image"
)

// Container is a container as the facade keeps it.
type Container struct {
	ID      string
	Name    string
	Class   runtime.Class
	Created time.Time
	// Image is the image as the create named it, and ImageID its id.
	Image   string
	ImageID string
	// Config and HostConfig are the create's, as allowlisted, with
	// HostConfig.Runtime the class's canonical name.
	Config     map[string]any
	HostConfig map[string]any
	// Process is what the driver runs.
	Process runtime.Process
	// Networks are its endpoints, by network name.
	Networks map[string]*Endpoint
	// Ports are its published ports.
	Ports  []PortBinding
	Mounts []MountPoint
	// DiskGB is its writable layer's cap, charged while it exists.
	DiskGB int64
	// VCPUm and MemoryMB are charged while it runs.
	VCPUm, MemoryMB int64
	StartedAt       time.Time
	FinishedAt      time.Time
	RestartCount    int
	// Error is why it couldn't start, last time it didn't.
	Error string
}

// Endpoint is a container's place on one network.
type Endpoint struct {
	NetworkID  string
	EndpointID string
	Addr       netip.Addr
	MAC        string
	Aliases    []string
}

// PortBinding is one published port: the container's Private port,
// published at Public on docker.silo.internal.
type PortBinding struct {
	Proto   string // tcp or udp
	Private int
	Public  int
	// Fixed is set when the create asked for Public; otherwise each
	// start gives it a free number.
	Fixed bool
}

// MountPoint is a volume or tmpfs in a container.
type MountPoint struct {
	Type     string // volume or tmpfs
	Name     string // the volume's name
	Target   string
	ReadOnly bool
	SizeMB   int64 // tmpfs
}

// Network is a Docker network: a subnet of the silo's blocks, and a
// bridge in silo-net.
type Network struct {
	ID       string
	Name     string
	Created  time.Time
	Subnet   netip.Prefix
	Internal bool
	Labels   map[string]string
	// Attachable is kept for inspect; every network is.
	Attachable bool
}

// Gateway is the network's gateway address, its first: the bridge holds
// it, as the hop to the gateway.
func (n *Network) Gateway() netip.Addr { return n.Subnet.Addr().Next() }

// Bridge is the network's bridge in silo-net.
func (n *Network) Bridge() string { return "sb-" + n.ID[:12] }

// Volume is a named or anonymous volume.
type Volume struct {
	Name      string
	Created   time.Time
	Labels    map[string]string
	Size      int64 // bytes, as charged
	Anonymous bool
}

// ImageRecord is an image a silo pulled or tagged.
type ImageRecord struct {
	ID          string // sha256:<hex>
	Tags        []string
	RepoDigests []string
	Size        int64
	Created     time.Time
	Config      ImageConfig
	// Manifest is the digest of the image's manifest in the content
	// store, of media type MediaType, which push, save and commit read.
	Manifest  string `json:",omitempty"`
	MediaType string `json:",omitempty"`
}

// newID is 64 hex characters.
func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// rows are one silo's objects of a kind.
func rows[T any](ctx context.Context, tx store.Tx, silo, kind string) ([]*T, error) {
	rs, err := tx.Query(ctx, `SELECT body FROM docker_objects WHERE silo = ? AND kind = ? ORDER BY created_at, id`, silo, kind)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*T
	for rs.Next() {
		var b string
		if err := rs.Scan(&b); err != nil {
			return nil, err
		}
		t := new(T)
		if err := json.Unmarshal([]byte(b), t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

// row is one object by id, or nil.
func row[T any](ctx context.Context, tx store.Tx, silo, kind, id string) (*T, error) {
	var b string
	err := tx.QueryRow(ctx, `SELECT body FROM docker_objects WHERE silo = ? AND kind = ? AND id = ?`, silo, kind, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := new(T)
	return t, json.Unmarshal([]byte(b), t)
}

// put writes an object, inserting it or replacing it.
func put(ctx context.Context, tx store.Tx, silo, kind, id, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	n, err := tx.Exec(ctx, `UPDATE docker_objects SET name = ?, body = ?, updated_at = ? WHERE silo = ? AND kind = ? AND id = ?`,
		name, string(b), store.Millis(now), silo, kind, id)
	if err != nil || n > 0 {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO docker_objects (silo, kind, id, name, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		silo, kind, id, name, string(b), store.Millis(now), store.Millis(now))
	return err
}

func remove(ctx context.Context, tx store.Tx, silo, kind, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM docker_objects WHERE silo = ? AND kind = ? AND id = ?`, silo, kind, id)
	return err
}

// allRows are every silo's objects of a kind, by silo.
func allRows[T any](ctx context.Context, tx store.Tx, kind string) (map[string][]*T, error) {
	rs, err := tx.Query(ctx, `SELECT silo, body FROM docker_objects WHERE kind = ? ORDER BY silo, created_at, id`, kind)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string][]*T{}
	for rs.Next() {
		var silo, b string
		if err := rs.Scan(&silo, &b); err != nil {
			return nil, err
		}
		t := new(T)
		if err := json.Unmarshal([]byte(b), t); err != nil {
			return nil, err
		}
		out[silo] = append(out[silo], t)
	}
	return out, rs.Err()
}

// find resolves ref among objects by exact id, then exact name, then a
// unique id prefix, within the silo only.
func find[T any](objs []*T, ref string, id, name func(*T) string) (*T, bool) {
	ref = strings.TrimPrefix(ref, "/")
	for _, o := range objs {
		if id(o) == ref {
			return o, true
		}
	}
	for _, o := range objs {
		if name(o) == ref {
			return o, true
		}
	}
	var hit *T
	n := 0
	for _, o := range objs {
		if ref != "" && strings.HasPrefix(id(o), ref) {
			hit, n = o, n+1
		}
	}
	return hit, n == 1
}

func (s *Server) containers(ctx context.Context, tx store.Tx, silo string) ([]*Container, error) {
	return rows[Container](ctx, tx, silo, kindContainer)
}

// container resolves ref to one of silo's containers: 404 "No such
// container", as Docker words it, for anything else.
func (s *Server) container(ctx context.Context, tx store.Tx, silo, ref string) (*Container, error) {
	cs, err := s.containers(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	c, ok := find(cs, ref, func(c *Container) string { return c.ID }, func(c *Container) string { return c.Name })
	if !ok {
		return nil, notFound("No such container: %s", ref)
	}
	return c, nil
}

func (s *Server) networks(ctx context.Context, tx store.Tx, silo string) ([]*Network, error) {
	return rows[Network](ctx, tx, silo, kindNetwork)
}

func (s *Server) network(ctx context.Context, tx store.Tx, silo, ref string) (*Network, error) {
	ns, err := s.networks(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	n, ok := find(ns, ref, func(n *Network) string { return n.ID }, func(n *Network) string { return n.Name })
	if !ok {
		return nil, notFound("network %s not found", ref)
	}
	return n, nil
}

func (s *Server) volume(ctx context.Context, tx store.Tx, silo, name string) (*Volume, error) {
	v, err := row[Volume](ctx, tx, silo, kindVolume, name)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, notFound("get %s: no such volume", name)
	}
	return v, nil
}

// sortedKeys are m's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
