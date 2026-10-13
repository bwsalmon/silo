package local

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/units"
	"github.com/bwsalmon/silo/pkg/install"
)

// ---- the data directory ----

// dataStep makes the data directory's layout (call L35) before anything
// is written into it, since silod refuses a directory that isn't empty
// and has no layout.json. Uninstall keeps it unless --delete-data.
type dataStep struct {
	c Config
	h Host
}

type dataObserved struct {
	Layout bool `json:"layout"` // layout.json exists
	Exists bool `json:"exists"` // the directory does
}

func (s *dataStep) Name() string                  { return "data" }
func (s *dataStep) NewObserved() install.Observed { return &dataObserved{} }

func (s *dataStep) Observe(context.Context) (install.Observed, error) {
	o := &dataObserved{}
	_, err := s.h.Stat(filepath.Join(s.c.DataDir, "layout.json"))
	o.Layout = err == nil
	_, err = s.h.Stat(s.c.DataDir)
	o.Exists = err == nil || o.Layout
	return o, nil
}

func (s *dataStep) Plan(obs install.Observed) []install.Change {
	if obs.(*dataObserved).Layout {
		return []install.Change{{Action: install.ActionOK, Object: s.c.DataDir, Detail: "a data directory (layout.json)"}}
	}
	return []install.Change{{Action: install.ActionCreate, Object: s.c.DataDir,
		Detail: "layout.json, state/, keys/, silos/, containerd/ and cache/, owned by " + s.c.User}}
}

func (s *dataStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*dataObserved)
	switch {
	case !o.Exists:
		return []install.Change{{Action: install.ActionOK, Object: s.c.DataDir, Detail: "not there"}}
	case s.c.DeleteData:
		return []install.Change{{Action: install.ActionDelete, Object: s.c.DataDir,
			Detail: "the store, the keys, and every silo's volumes, logs, repos and images"}}
	}
	return []install.Change{{Action: install.ActionKeep, Object: s.c.DataDir,
		Detail: "the store, the keys and every silo's data; --delete-data removes it"}}
}

func (s *dataStep) Apply(ctx context.Context, changes []install.Change) error {
	for _, ch := range changes {
		switch ch.Action {
		case install.ActionDelete:
			if err := s.h.RemoveAll(s.c.DataDir); err != nil {
				return err
			}
			fmt.Fprintf(s.h.Progress, "  removed %s\n", s.c.DataDir)
		case install.ActionCreate:
			if err := s.h.OpenDataDir(s.c.DataDir); err != nil {
				return err
			}
			if s.c.Mode == units.System {
				// Made by root, for silod's account; new, so nothing in
				// it belongs to a container's subuids yet.
				if _, err := s.h.Run(ctx, "chown", "-R", s.c.User+":"+s.c.User, s.c.DataDir); err != nil {
					return err
				}
			}
			fmt.Fprintf(s.h.Progress, "  made %s\n", s.c.DataDir)
		}
	}
	return nil
}

// ---- the pinned runtimes ----

// Gvisor is the pinned gVisor release: CI's (the nsd job's), from
// gVisor's release bucket. The release publishes one SHA-512, the
// tarball's, which is pinned here with each binary's own sum, so that
// an installed binary is checked without the tarball.
var Gvisor = GvisorRelease{
	Version: "20261005",
	Arch: map[string]GvisorArch{
		"x86_64": {
			Tarball:   "79869ae9a589355a46d46fdac068d9a954e741741ffe77c64e38c56ea955498c1cb04e9bce44e1cce5155333e6b76bee72347c6661bb7c78fd2fe3aad198c8fe",
			Runsc:     "0e0a278f73a3017c45d6fa790ca06a3f62c272c873b504c514efafb3ddacc9ea0f372376128bff8ffe3d766d7cd5f22ceda8bab8075ea10e1064e065d21d8266",
			RunscShim: "1a30ac91bef05cd8f00a2516a341ffcbe9a6f8a034792f17bfe1957617dfec3d64d41fa6b84ad7bcd5b13406889a6737e672fdd501be3cb349f788ff8dccf961",
		},
		"aarch64": {
			Tarball:   "1bc58be6f881264ced1df8ff61db0205531abedcace005585ac5637c5c987831062b365f99f270ef0939849f130344b84e0f1a663294406f798b8f7c8cd326ac",
			Runsc:     "3062bddd4e6ed8f8f7f1db95ba4412e9751207f3a2be8a39ae68b51d15a63306d78ad3e6833f711361afddf9f354e103c29b788a6db125ba5d0690ea1d306173",
			RunscShim: "8ad9d9e0a94241a9767c2f7a1668e589826fcd433c16e1e26676a356967f24e2ebf7050e5aa3b0ae5a3cce8b19e19f18bfaa2ad887ce77b38a987128fceceb57",
		},
	},
}

// GvisorRelease is a gVisor release and its sums by architecture
// (gVisor's names: x86_64, aarch64).
type GvisorRelease struct {
	Version string
	Arch    map[string]GvisorArch
}

// GvisorArch is one architecture's SHA-512 sums, in hex.
type GvisorArch struct {
	Tarball, Runsc, RunscShim string
}

// URL is the release's tarball for arch.
func (g GvisorRelease) URL(arch string) string {
	return fmt.Sprintf("https://storage.googleapis.com/gvisor/releases/release/%s/%s/gvisor.tar.bz2", g.Version, arch)
}

// gvisorArch is gVisor's name for a GOARCH.
func gvisorArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goarch
}

// RuntimesDir is where install puts the pinned gVisor: the data
// directory's cache/runtimes/runsc-<version> (call L35's layout).
func RuntimesDir(c Config) string {
	return filepath.Join(c.DataDir, "cache", "runtimes", "runsc-"+Gvisor.Version)
}

// pinned reports whether there is a pinned gVisor for h's architecture.
func pinned(h Host) (GvisorArch, bool) {
	a, ok := Gvisor.Arch[gvisorArch(h.Arch)]
	return a, ok
}

// runtimesStep fetches the pinned runsc and containerd-shim-runsc-v1
// into RuntimesDir, checked against their pinned SHA-512s, and the
// units put that directory first on PATH. containerd, crun and buildkitd
// are the distribution's packages, which the prerequisites check. The
// vm class's granule images are pinned with its driver (step 10).
type runtimesStep struct {
	c Config
	h Host
}

type runtimesObserved struct {
	// Have is each binary's state: "ok", "missing", or what is wrong.
	Have map[string]string `json:"have"`
	Dir  bool              `json:"dir"`
}

func (s *runtimesStep) Name() string                  { return "runtimes" }
func (s *runtimesStep) NewObserved() install.Observed { return &runtimesObserved{} }

// binaries are the release's binaries install takes, with their sums.
func binaries(a GvisorArch) [][2]string {
	return [][2]string{{"runsc", a.Runsc}, {"containerd-shim-runsc-v1", a.RunscShim}}
}

func (s *runtimesStep) Observe(context.Context) (install.Observed, error) {
	o := &runtimesObserved{Have: map[string]string{}}
	_, err := s.h.Stat(RuntimesDir(s.c))
	o.Dir = err == nil
	a, ok := pinned(s.h)
	if !ok {
		return o, nil
	}
	for _, b := range binaries(a) {
		o.Have[b[0]] = fileState(s.h, filepath.Join(RuntimesDir(s.c), b[0]), b[1])
	}
	return o, nil
}

// fileState is "ok" when name's SHA-512 is sum, "missing" when there is
// no such file, and what is wrong otherwise.
func fileState(h Host, name, sum string) string {
	f, err := h.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing"
	} else if err != nil {
		return err.Error()
	}
	defer f.Close()
	d := sha512.New()
	if _, err := io.Copy(d, f); err != nil {
		return err.Error()
	}
	if hex.EncodeToString(d.Sum(nil)) != sum {
		return "its SHA-512 is not the pinned one"
	}
	return "ok"
}

func (s *runtimesStep) Plan(obs install.Observed) []install.Change {
	o := obs.(*runtimesObserved)
	a, ok := pinned(s.h)
	if !ok {
		return []install.Change{{Action: install.ActionWarn, Object: "gVisor",
			Detail: "no pinned gVisor for " + s.h.Arch + ": the sandbox class uses the runsc on PATH, if any",
			Why:    "the sandbox class's runtime, at the version silod is tested with"}}
	}
	var out []install.Change
	dir := RuntimesDir(s.c)
	for _, b := range binaries(a) {
		path := filepath.Join(dir, b[0])
		switch st := o.Have[b[0]]; st {
		case "ok":
			out = append(out, install.Change{Action: install.ActionOK, Object: b[0], Detail: path + ": gVisor " + Gvisor.Version + ", SHA-512 checked"})
		case "missing":
			out = append(out, install.Change{Action: install.ActionCreate, Object: b[0],
				Detail: fmt.Sprintf("%s: gVisor %s from %s, checked against its pinned SHA-512", path, Gvisor.Version, Gvisor.URL(gvisorArch(s.h.Arch)))})
		default:
			out = append(out, install.Change{Action: install.ActionUpdate, Object: b[0],
				Detail: fmt.Sprintf("%s %s: fetched again (gVisor %s)", path, st, Gvisor.Version)})
		}
	}
	return out
}

func (s *runtimesStep) PlanRemove(obs install.Observed) []install.Change {
	o := obs.(*runtimesObserved)
	if !o.Dir {
		return []install.Change{{Action: install.ActionOK, Object: RuntimesDir(s.c), Detail: "not there"}}
	}
	return []install.Change{{Action: install.ActionDelete, Object: RuntimesDir(s.c), Detail: "the pinned runsc and its shim"}}
}

func (s *runtimesStep) Apply(ctx context.Context, changes []install.Change) error {
	fetch := false
	for _, ch := range changes {
		switch ch.Action {
		case install.ActionDelete:
			if err := s.h.RemoveAll(RuntimesDir(s.c)); err != nil {
				return err
			}
			fmt.Fprintf(s.h.Progress, "  removed %s\n", RuntimesDir(s.c))
		case install.ActionCreate, install.ActionUpdate:
			fetch = true
		}
	}
	if !fetch {
		return nil
	}
	a, ok := pinned(s.h)
	if !ok {
		return nil
	}
	return s.fetch(ctx, a)
}

// fetch downloads the release, checks the tarball's sum before reading
// anything in it, and installs each binary, which is renamed into place
// only once its own sum has been checked.
func (s *runtimesStep) fetch(ctx context.Context, a GvisorArch) error {
	url := Gvisor.URL(gvisorArch(s.h.Arch))
	fmt.Fprintf(s.h.Progress, "  fetching %s\n", url)
	r, err := s.h.Fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("fetching gVisor %s: %w", Gvisor.Version, err)
	}
	defer r.Close()
	var buf bytes.Buffer
	d := sha512.New()
	if _, err := io.Copy(io.MultiWriter(&buf, d), r); err != nil {
		return fmt.Errorf("fetching gVisor %s: %w", Gvisor.Version, err)
	}
	if got := hex.EncodeToString(d.Sum(nil)); got != a.Tarball {
		return fmt.Errorf("%s: SHA-512 %s is not the pinned %s", url, got, a.Tarball)
	}
	want := map[string]string{}
	for _, b := range binaries(a) {
		want[b[0]] = b[1]
	}
	tr := tar.NewReader(bzip2.NewReader(&buf))
	for len(want) > 0 {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("%s: %w", url, err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		sum, ok := want[name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		path := filepath.Join(RuntimesDir(s.c), name)
		if err := s.h.Install(path, &checked{r: tr, d: sha512.New(), want: sum, name: name}); err != nil {
			return err
		}
		fmt.Fprintf(s.h.Progress, "  installed %s\n", path)
		delete(want, name)
	}
	for name := range want {
		return fmt.Errorf("%s has no %s", url, name)
	}
	return nil
}

// checked reads r, and fails at its end unless what it read has the
// SHA-512 want: an Install that sees the error renames nothing.
type checked struct {
	r          io.Reader
	d          hash.Hash
	want, name string
}

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.d.Write(p[:n])
	if err == io.EOF {
		if got := hex.EncodeToString(c.d.Sum(nil)); got != c.want {
			return n, fmt.Errorf("%s: SHA-512 %s is not the pinned %s", c.name, got, c.want)
		}
	}
	return n, err
}
