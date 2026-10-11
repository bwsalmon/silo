package fakecloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/install"
)

// ReleaseDigest is the digest the fake release registry gives tag.
func ReleaseDigest(tag string) string {
	sum := sha256.Sum256([]byte("silod:" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HarnessDigest is the digest the fake release registry gives the
// harness image's tag.
func HarnessDigest(tag string) string {
	sum := sha256.Sum256([]byte("silod-harness:" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type registry struct{ f *Fake }

// Resolve gives ReleaseRepository:<tag> ReleaseDigest(tag), unless
// MissingReleases names the tag.
func (r registry) Resolve(ctx context.Context, ref string) (string, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if _, d, ok := strings.Cut(ref, "@"); ok {
		return d, nil
	}
	if tag, ok := strings.CutPrefix(ref, install.ReleaseRepository+"-harness:"); ok {
		if r.f.MissingReleases[tag] {
			return "", notFound("manifest " + ref)
		}
		return HarnessDigest(tag), nil
	}
	tag, ok := strings.CutPrefix(ref, install.ReleaseRepository+":")
	if !ok || r.f.MissingReleases[tag] {
		return "", notFound("manifest " + ref)
	}
	return ReleaseDigest(tag), nil
}

func (r registry) Has(ctx context.Context, repository, digest string) (bool, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.f.Images[repository+"@"+digest], nil
}

func (r registry) Copy(ctx context.Context, src, digest, dst string) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	// The destination is the project's repository silo, which must
	// exist: <region>-docker.pkg.dev/<project>/silo/silod.
	parts := strings.Split(dst, "/")
	if len(parts) != 4 {
		return fmt.Errorf("copy to %q: not a repository of the project", dst)
	}
	region := strings.TrimSuffix(parts[0], "-docker.pkg.dev")
	if r.f.Repositories[region+"/"+parts[2]] == nil {
		return notFound("repository " + parts[2])
	}
	r.f.Images[dst+"@"+digest] = true
	return nil
}

// silod is silod, reached through the front door: it serves once the
// VM silo-control exists in the GCE fake and isn't stopped.
type silod struct{ f *Fake }

func (s silod) up(ctx context.Context) error {
	zones, err := s.f.Compute.ListZones(ctx, s.f.Project.ID)
	if err != nil {
		return err
	}
	for _, z := range zones {
		vm, err := s.f.Compute.GetInstance(ctx, s.f.Project.ID, z.Name, install.ControlVM)
		if err != nil {
			continue
		}
		if vm.Status != "" && vm.Status != "RUNNING" {
			return errors.New("connection refused: " + install.ControlVM + " is " + vm.Status)
		}
		return nil
	}
	return errors.New("opening the IAP tunnel: no VM " + install.ControlVM)
}

func (s silod) Health(ctx context.Context) (install.SilodHealth, error) {
	if err := s.up(ctx); err != nil {
		return install.SilodHealth{}, err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return install.SilodHealth{Status: "ok", Root: s.f.Root != nil}, nil
}

func (s silod) Bootstrap(ctx context.Context, req *install.BootstrapRequest) error {
	if err := s.up(ctx); err != nil {
		return err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.Root != nil {
		return errors.New("403 Forbidden: the root silo exists; bootstrap is accepted only before it does")
	}
	cp := *req
	s.f.Root = &cp
	s.f.Members = append([]string{}, req.Admins...)
	return nil
}

func (s silod) rooted(ctx context.Context) error {
	if err := s.up(ctx); err != nil {
		return err
	}
	if s.f.Root == nil {
		return errors.New("403 Forbidden: not a member of any silo")
	}
	return nil
}

func (s silod) Root(ctx context.Context) (*install.RootSilo, error) {
	if err := s.rooted(ctx); err != nil {
		return nil, err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	r := s.f.Root
	out := &install.RootSilo{Path: "/", Regions: r.Regions, Quota: map[string]map[string]string{}}
	space := netip.MustParsePrefix(r.AddressSpace)
	out.Blocks = []string{space.String()}
	for reg, classes := range r.Capacity {
		out.Quota[reg] = map[string]string{}
		for c, v := range classes {
			out.Quota[reg][c] = v
		}
	}
	return out, nil
}

func (s silod) Members(ctx context.Context, silo string) ([]string, error) {
	if err := s.rooted(ctx); err != nil {
		return nil, err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	out := append([]string{}, s.f.Members...)
	sort.Strings(out)
	return out, nil
}

func (s silod) PutMember(ctx context.Context, silo, identity string) error {
	if err := s.rooted(ctx); err != nil {
		return err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for _, m := range s.f.Members {
		if m == identity {
			return nil
		}
	}
	s.f.Members = append(s.f.Members, identity)
	return nil
}

func (s silod) Regions(ctx context.Context) ([]string, error) {
	if err := s.rooted(ctx); err != nil {
		return nil, err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return append([]string{}, s.f.Root.Regions...), nil
}
