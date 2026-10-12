package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runtime runs sessions' turns on a control VM. silod's is a Client of
// its VM's sessiond; sessiond's is Sessiond, on the container runtime.
type Runtime interface {
	// Turn runs one turn as spec says, calling line with each line of
	// the CLI's stdout, and returns its exit code and the saved state
	// of spec.StatePaths.
	Turn(ctx context.Context, spec Spec, line func([]byte)) (Result, error)
	// Remove removes agent's containers and its private directory.
	Remove(ctx context.Context, agent string) error
	// List lists the agents with session containers on this VM.
	List(ctx context.Context) ([]string, error)
	// Login reads a subscription session's login files at paths under
	// its private directory, as its harness left them, for silod to
	// write back when the session stops. A path with no file is left
	// out.
	Login(ctx context.Context, agent string, paths []string) (map[string][]byte, error)
}

// Result is how a turn ended.
type Result struct {
	Exit  int    `json:"exit"`
	State []byte `json:"state,omitempty"`
	// Login are a subscription session's login files after the turn,
	// at the paths of Spec.Login, as the harness left them.
	Login map[string][]byte `json:"login,omitempty"`
}

// Engine is what Sessiond needs of the container runtime; *Docker is
// it.
type Engine interface {
	Create(ctx context.Context, c Container) error
	Start(ctx context.Context, name string) error
	Wait(ctx context.Context, name string) (int, error)
	Remove(ctx context.Context, name string) error
	Running(ctx context.Context, name string) (bool, error)
	Labelled(ctx context.Context, label string) (map[string]string, error)
	Logs(ctx context.Context, name string, line func([]byte)) error
	// EgressNetwork makes the subscription sessions' network if it
	// isn't there, and returns its gateway address.
	EgressNetwork(ctx context.Context, name string) (netip.Addr, error)
	// Address is a container's address in a network.
	Address(ctx context.Context, name, network string) (netip.Addr, error)
}

// Sessiond is the session launcher (silod sessiond): the only process
// on a control VM that holds the container runtime, run as root beside
// silod, which reaches it on a Unix socket only silod's user can open.
// It refuses any spec that fails Validate, runs only its own Image,
// gives no two live sessions the same uid (and removes every session
// container as it starts, so the uids it knows are the ones running),
// puts the metadata rule in each session's namespace before its harness
// first runs, and takes nothing in silod's socket directory on trust
// (socket). So silod never holds the runtime's socket, which is root on
// the VM, and whatever silod asks for, a session's container has no
// capability, the default seccomp profile, a read-only root, its
// limits, a session uid, and no mount but a socket silod made and its
// own private directory, and no network but, for a subscription
// session, a route to the egress proxy sessiond serves, for its
// provider's hosts, with its login files mounted read-only. What runs
// inside it (argv, env, login files) is silod's to say: that is the
// session, not more authority.
type Sessiond struct {
	Engine Engine
	// Image is the harness image, the only one it runs.
	Image  string
	Layout Layout
	// Chown sets a path's owner; os.Lchown if nil.
	Chown func(path string, uid, gid int) error
	// Owner is silod's uid: a session socket must be a socket silod
	// made. -1 skips the check (tests that run as one user).
	Owner int
	// ListenEgress listens for the egress proxy at addr; net.Listen if
	// nil.
	ListenEgress func(addr netip.AddrPort) (net.Listener, error)
	// Proxy is the subscription sessions' egress proxy, which sessiond
	// serves on EgressNetwork's gateway once a subscription session
	// starts; a new one if nil.
	Proxy *EgressProxy

	mu   sync.Mutex
	uids map[string]int
	// nets are the network modes of the sessions' namespaces, and
	// addrs the subscription sessions' addresses, admitted to Proxy.
	nets     map[string]string
	addrs    map[string]netip.Addr
	egressAt netip.AddrPort
}

func (d *Sessiond) chown(path string, uid int) error {
	if d.Chown != nil {
		return d.Chown(path, uid, uid)
	}
	return os.Lchown(path, uid, uid)
}

// claim records that agent's session runs as uid, refusing a uid
// another live session has.
func (d *Sessiond) claim(agent string, uid int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.uids == nil {
		d.uids = map[string]int{}
	}
	for a, u := range d.uids {
		if u == uid && a != agent {
			return fmt.Errorf("uid %d is session %s's", uid, a)
		}
	}
	if u, ok := d.uids[agent]; ok && u != uid {
		return fmt.Errorf("session %s runs as uid %d, not %d: remove it first", agent, u, uid)
	}
	d.uids[agent] = uid
	return nil
}

// Turn implements Runtime.
func (d *Sessiond) Turn(ctx context.Context, spec Spec, line func([]byte)) (Result, error) {
	if err := spec.Validate(d.Layout); err != nil {
		return Result{}, err
	}
	if spec.Image != "" && spec.Image != d.Image {
		return Result{}, fmt.Errorf("session spec refused: image %s is not the harness image %s", spec.Image, d.Image)
	}
	if err := d.claim(spec.Agent, spec.UID); err != nil {
		return Result{}, err
	}
	if err := d.private(&spec); err != nil {
		return Result{}, fmt.Errorf("the private directory: %w", err)
	}
	if spec.Network == NetworkEgress {
		if err := d.login(&spec); err != nil {
			return Result{}, fmt.Errorf("the login files: %w", err)
		}
	}
	link, err := d.socket(&spec)
	if err != nil {
		return Result{}, fmt.Errorf("the session socket: %w", err)
	}
	// The container mounts sessiond's link, never a path in silod's
	// directory.
	spec.Mounts = slices.Clone(spec.Mounts)
	for i := range spec.Mounts {
		if spec.Mounts[i].Target == SocketPath {
			spec.Mounts[i].Source = link
		}
	}
	if err := d.namespace(ctx, &spec); err != nil {
		return Result{}, fmt.Errorf("the session's network namespace: %w", err)
	}
	if spec.Network == NetworkEgress {
		// The CLIs reach their provider through the proxy; the session's
		// own endpoint, on the loopback, stays direct.
		proxy := "http://" + d.egressAddr().String()
		spec.Env = append(slices.Clone(spec.Env), "HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	}
	name := ContainerName(spec.Agent)
	if err := d.Engine.Remove(ctx, name); err != nil {
		return Result{}, err
	}
	if err := d.Engine.Create(ctx, HarnessContainer(&spec, d.Image)); err != nil {
		return Result{}, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		d.Engine.Remove(rctx, name)
	}()
	if err := d.Engine.Start(ctx, name); err != nil {
		return Result{}, err
	}
	logErr := d.Engine.Logs(ctx, name, line)
	code, err := d.Engine.Wait(ctx, name)
	if err != nil {
		return Result{}, err
	}
	if logErr != nil {
		log.Printf("sessiond: %s's output: %v", spec.Agent, logErr)
	}
	st, err := SaveState(d.Layout.Private(spec.Agent), spec.StatePaths)
	if err != nil {
		return Result{Exit: code}, fmt.Errorf("saving the session's state: %w", err)
	}
	res := Result{Exit: code, State: st}
	if spec.Network == NetworkEgress {
		if res.Login, err = d.Login(ctx, spec.Agent, slices.Collect(maps.Keys(spec.Login))); err != nil {
			return res, fmt.Errorf("reading the login files back: %w", err)
		}
	}
	return res, nil
}

// maxLoginFile is the largest login file sessiond reads back.
const maxLoginFile = 1 << 20

// login puts spec's login files in its login directory, fresh, owned
// by its uid: the directory sessiond mounts read-only at LoginPath.
// It is sessiond's own (LoginDir, root's), so, unlike the private
// directory, nothing in it is the harness's to have planted; the
// harness's copy in its home is made by harness-init, as the
// session's uid.
func (d *Sessiond) login(spec *Spec) error {
	if d.Layout.LoginDir == "" {
		return errors.New("no login directory")
	}
	if err := os.MkdirAll(d.Layout.LoginDir, 0o711); err != nil {
		return err
	}
	if fi, err := os.Lstat(d.Layout.LoginDir); err != nil || !fi.IsDir() || fileOwner(fi) != os.Geteuid() || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is not sessiond's own directory", d.Layout.LoginDir)
	}
	dir := d.Layout.Login(spec.Agent)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	for rel, b := range spec.Login {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return d.chown(p, spec.UID)
	})
}

// Login implements Runtime: the files at paths in agent's private
// directory, reached through real directories only and not through a
// symlink, so the harness can't have sessiond read anything else.
func (d *Sessiond) Login(_ context.Context, agent string, paths []string) (map[string][]byte, error) {
	if !ValidAgent(agent) {
		return nil, fmt.Errorf("bad agent %q", agent)
	}
	out := map[string][]byte{}
	for _, rel := range paths {
		if !ValidLoginPath(rel) {
			return nil, fmt.Errorf("login file %q must be relative, under the home", rel)
		}
		p, ok := underDir(d.Layout.Private(agent), rel)
		if !ok {
			continue
		}
		f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxLoginFile {
			f.Close()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, maxLoginFile))
		f.Close()
		if err != nil {
			return nil, err
		}
		out[rel] = b
	}
	return out, nil
}

// socket hands spec's session socket to its uid, safely against silod.
// The socket is in silod's directory, where silod could put a symlink
// in its place (to the runtime's socket, or a file to have chmodded);
// the runtime follows symlinks in a bind mount's source. So sessiond
// hard-links it into LinkDir, its own directory, which nobody else can
// change: link(2) doesn't follow a symlink, and refuses (EXDEV) a path
// that leads off the mount silod's directory is on. Only a socket that
// silod owns is accepted, and it is that link, checked, that is
// chowned, chmodded and mounted.
func (d *Sessiond) socket(spec *Spec) (string, error) {
	if err := d.linkDir(); err != nil {
		return "", err
	}
	link := d.Layout.Link(spec.Agent)
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Link(d.Layout.Socket(spec.Agent), link); err != nil {
		return "", err
	}
	fi, err := os.Lstat(link)
	if err != nil {
		return "", err
	}
	if fi.Mode().Type() != fs.ModeSocket || d.Owner >= 0 && fileOwner(fi) != d.Owner {
		os.Remove(link)
		return "", fmt.Errorf("%s is not a socket of silod's (uid %d)", d.Layout.Socket(spec.Agent), d.Owner)
	}
	// Only the session's uid (and root) may connect to it.
	if err := d.chown(link, spec.UID); err != nil {
		return "", err
	}
	if err := os.Chmod(link, 0o600); err != nil {
		return "", err
	}
	return link, nil
}

// linkDir makes LinkDir, root's alone, and keeps a file in it, so
// that once it is checked nobody else can remove or replace it (it
// sits in silod's directory, but a non-empty directory nobody else
// may write can't be removed or renamed by anyone else).
func (d *Sessiond) linkDir() error {
	dir := d.Layout.LinkDir
	if dir == "" {
		return errors.New("no link directory")
	}
	if err := os.Mkdir(dir, 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	keep := filepath.Join(dir, ".sessiond")
	if f, err := os.OpenFile(keep, os.O_CREATE|os.O_WRONLY|noFollow, 0o600); err == nil {
		f.Close()
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fileOwner(fi) != os.Geteuid() || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is not sessiond's own directory", dir)
	}
	if ki, err := os.Lstat(keep); err != nil || !ki.Mode().IsRegular() || fileOwner(ki) != os.Geteuid() {
		return fmt.Errorf("%s is not sessiond's", keep)
	}
	return nil
}

// Reset removes every session container, as sessiond starts: none
// outlives the sessiond that ran it, so the uids it hands out are
// always the ones running.
func (d *Sessiond) Reset(ctx context.Context) error {
	agents, err := d.List(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if err := d.Remove(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// private makes spec's private directory, owned by its uid and mode
// 0700, from spec.Restore if it is set, and the working directory in
// it.
func (d *Sessiond) private(spec *Spec) error {
	dir := d.Layout.Private(spec.Agent)
	if err := os.MkdirAll(d.Layout.StateDir, 0o711); err != nil {
		return err
	}
	if spec.Restore != nil {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	fresh := false
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		fresh = true
	}
	if fresh && spec.Restore != nil {
		if err := RestoreState(dir, spec.StatePaths, spec.Restore); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, strings.TrimPrefix(WorkDir, Home+"/")), 0o700); err != nil {
		return err
	}
	if !fresh {
		return d.chown(dir, spec.UID)
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return d.chown(p, spec.UID)
	})
}

// namespace starts spec's namespace container, if it isn't running
// in spec's network mode, and has the firewall container put its rules
// in it. A subscription session's is on EgressNetwork, admitted to the
// egress proxy for its provider.
func (d *Sessiond) namespace(ctx context.Context, spec *Spec) error {
	netName := NetName(spec.Agent)
	d.mu.Lock()
	mode := d.nets[spec.Agent]
	d.mu.Unlock()
	if up, err := d.Engine.Running(ctx, netName); err != nil || up && mode == spec.Network {
		return err
	}
	d.forget(spec.Agent)
	for _, n := range []string{ContainerName(spec.Agent), FirewallName(spec.Agent), netName} {
		if err := d.Engine.Remove(ctx, n); err != nil {
			return err
		}
	}
	var proxy netip.AddrPort
	if spec.Network == NetworkEgress {
		var err error
		if proxy, err = d.egress(ctx); err != nil {
			return fmt.Errorf("the egress proxy: %w", err)
		}
	}
	if err := d.Engine.Create(ctx, NetContainer(spec, d.Image)); err != nil {
		return err
	}
	if err := d.Engine.Start(ctx, netName); err != nil {
		return err
	}
	fw := FirewallName(spec.Agent)
	if err := d.Engine.Create(ctx, FirewallContainer(spec, d.Image, proxy)); err != nil {
		return err
	}
	defer d.Engine.Remove(context.WithoutCancel(ctx), fw)
	if err := d.Engine.Start(ctx, fw); err != nil {
		return err
	}
	code, err := d.Engine.Wait(ctx, fw)
	if err == nil && code != 0 {
		err = fmt.Errorf("the firewall rules weren't put in place (exit %d)", code)
	}
	if err == nil && spec.Network == NetworkEgress {
		var addr netip.Addr
		if addr, err = d.Engine.Address(ctx, netName, EgressNetwork); err == nil {
			d.mu.Lock()
			if d.addrs == nil {
				d.addrs = map[string]netip.Addr{}
			}
			d.addrs[spec.Agent] = addr
			d.mu.Unlock()
			d.Proxy.Admit(addr, spec.Provider)
		}
	}
	if err != nil {
		// No namespace without the rules: the harness doesn't run.
		d.Engine.Remove(context.WithoutCancel(ctx), netName)
		return err
	}
	d.mu.Lock()
	if d.nets == nil {
		d.nets = map[string]string{}
	}
	d.nets[spec.Agent] = spec.Network
	d.mu.Unlock()
	return nil
}

// egress makes EgressNetwork and serves the egress proxy on its
// gateway, once, and returns the proxy's address.
func (d *Sessiond) egress(ctx context.Context) (netip.AddrPort, error) {
	d.mu.Lock()
	at := d.egressAt
	d.mu.Unlock()
	if at.IsValid() {
		return at, nil
	}
	gw, err := d.Engine.EgressNetwork(ctx, EgressNetwork)
	if err != nil {
		return at, err
	}
	at = netip.AddrPortFrom(gw, EgressPort)
	listen := d.ListenEgress
	if listen == nil {
		listen = func(a netip.AddrPort) (net.Listener, error) { return net.Listen("tcp", a.String()) }
	}
	l, err := listen(at)
	if err != nil {
		return netip.AddrPort{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.egressAt.IsValid() {
		l.Close()
		return d.egressAt, nil
	}
	if d.Proxy == nil {
		d.Proxy = &EgressProxy{}
	}
	d.egressAt = at
	go func() {
		if err := d.Proxy.Serve(l); err != nil {
			log.Printf("sessiond: the egress proxy: %v", err)
		}
	}()
	return at, nil
}

func (d *Sessiond) egressAddr() netip.AddrPort {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.egressAt
}

// forget stops admitting agent's session to the egress proxy.
func (d *Sessiond) forget(agent string) {
	d.mu.Lock()
	addr, ok := d.addrs[agent]
	delete(d.addrs, agent)
	delete(d.nets, agent)
	d.mu.Unlock()
	if ok && d.Proxy != nil {
		d.Proxy.Forget(addr)
	}
}

// Remove implements Runtime.
func (d *Sessiond) Remove(ctx context.Context, agent string) error {
	if !ValidAgent(agent) {
		return fmt.Errorf("bad agent %q", agent)
	}
	for _, n := range []string{ContainerName(agent), FirewallName(agent), NetName(agent)} {
		if err := d.Engine.Remove(ctx, n); err != nil {
			return err
		}
	}
	d.forget(agent)
	d.mu.Lock()
	delete(d.uids, agent)
	d.mu.Unlock()
	if d.Layout.LoginDir != "" {
		if err := os.RemoveAll(d.Layout.Login(agent)); err != nil {
			return err
		}
	}
	if d.Layout.LinkDir != "" {
		if err := os.Remove(d.Layout.Link(agent)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(d.Layout.Private(agent))
}

// List implements Runtime.
func (d *Sessiond) List(ctx context.Context) ([]string, error) {
	names, err := d.Engine.Labelled(ctx, LabelSession)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range names {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out, nil
}

// maxState is the most saved state sessiond returns or restores.
const maxState = 256 << 20

// underDir reports whether rel, under base, is reached through real
// directories only: a harness can't point a state path at anything
// outside its private directory with a symlink.
func underDir(base, rel string) (string, bool) {
	p := base
	for _, c := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		fi, err := os.Lstat(p)
		if err != nil || !fi.IsDir() {
			return "", false
		}
		p = filepath.Join(p, c)
	}
	return p, true
}

// SaveState is a tar.gz of the regular files and directories under
// paths in dir. Symlinks and anything else are left out.
func SaveState(dir string, paths []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var total int64
	for _, rel := range paths {
		root, ok := underDir(dir, rel)
		if !ok {
			continue
		}
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			name, _ := filepath.Rel(dir, p)
			switch {
			case e.IsDir():
				return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o700})
			case e.Type().IsRegular():
				f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0)
				if err != nil {
					return err
				}
				defer f.Close()
				fi, err := f.Stat()
				if err != nil || !fi.Mode().IsRegular() {
					return err
				}
				if total += fi.Size(); total > maxState {
					return fmt.Errorf("the session's state is over %d bytes", maxState)
				}
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
					return err
				}
				_, err = io.CopyN(tw, f, fi.Size())
				return err
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RestoreState unpacks a SaveState archive into dir, which is new:
// only directories and regular files under paths.
func RestoreState(dir string, paths []string, state []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(state))
	if err != nil {
		return err
	}
	tr := tar.NewReader(io.LimitReader(gz, maxState))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == "." || strings.HasPrefix(name, "..") || !underAny(name, paths) {
			return fmt.Errorf("saved state: %q is outside the state paths", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("saved state: %q is not a file or directory", h.Name)
		}
	}
}

// underAny reports whether name, a cleaned relative archive name, is
// one of paths or under one, component by component. The archive holds
// no symlinks (RestoreState refuses them) and is unpacked into a new
// directory, so the names alone say where each file lands.
func underAny(name string, paths []string) bool {
	parts := strings.Split(name, string(filepath.Separator))
	for _, p := range paths {
		want := strings.Split(filepath.Clean(p), string(filepath.Separator))
		if len(parts) >= len(want) && slices.Equal(parts[:len(want)], want) {
			return true
		}
	}
	return false
}

// frame is one line of sessiond's answer to a turn: a line of the
// CLI's output, then the result or an error.
type frame struct {
	Out    string  `json:"out,omitempty"`
	Result *Result `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// Handler serves sessiond's API: POST /v1/turn with a Spec, answered
// as frames; POST /v1/remove?agent=; GET /v1/sessions.
func (d *Sessiond) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/turn", func(w http.ResponseWriter, r *http.Request) {
		var spec Spec
		if err := json.NewDecoder(io.LimitReader(r.Body, maxState*2)).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		var mu sync.Mutex
		enc := json.NewEncoder(w)
		fl, _ := w.(http.Flusher)
		send := func(f frame) {
			mu.Lock()
			defer mu.Unlock()
			enc.Encode(f)
			if fl != nil {
				fl.Flush()
			}
		}
		res, err := d.Turn(r.Context(), spec, func(b []byte) { send(frame{Out: string(b)}) })
		if err != nil {
			send(frame{Error: err.Error()})
			return
		}
		send(frame{Result: &res})
	})
	mux.HandleFunc("POST /v1/remove", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Remove(r.Context(), r.URL.Query().Get("agent")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/login", func(w http.ResponseWriter, r *http.Request) {
		files, err := d.Login(r.Context(), r.URL.Query().Get("agent"), r.URL.Query()["path"])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		list, err := d.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"agents": list})
	})
	return mux
}

// Listen makes sessiond's socket at path, which only owner's uid may
// connect to.
func Listen(path string, owner int, chown func(string, int, int) error) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if chown == nil {
		chown = os.Lchown
	}
	if err := chown(path, owner, owner); err != nil {
		l.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Client is a Runtime that is a sessiond on a Unix socket.
type Client struct {
	Socket string
	hc     *http.Client
}

func (c *Client) client() *http.Client {
	if c.hc == nil {
		c.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		}}}
	}
	return c.hc
}

// Turn implements Runtime.
func (c *Client) Turn(ctx context.Context, spec Spec, line func([]byte)) (Result, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sessiond/v1/turn", bytes.NewReader(b))
	if err != nil {
		return Result{}, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("sessiond: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("sessiond: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var res *Result
	var ferr error
	err = scanLines(resp.Body, func(b []byte) {
		var f frame
		if json.Unmarshal(b, &f) != nil {
			return
		}
		switch {
		case f.Error != "":
			ferr = errors.New(f.Error)
		case f.Result != nil:
			res = f.Result
		default:
			line([]byte(f.Out))
		}
	})
	if ferr != nil {
		return Result{}, ferr
	}
	if res == nil {
		if err == nil {
			err = errors.New("sessiond: the turn's answer ended without a result")
		}
		return Result{}, err
	}
	return *res, nil
}

// Remove implements Runtime.
func (c *Client) Remove(ctx context.Context, agent string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sessiond/v1/remove?agent="+agent, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("sessiond: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sessiond: removing %s: %s: %s", agent, resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

// Login implements Runtime.
func (c *Client) Login(ctx context.Context, agent string, paths []string) (map[string][]byte, error) {
	q := url.Values{"agent": {agent}, "path": paths}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sessiond/v1/login?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("sessiond: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("sessiond: reading %s's login: %s: %s", agent, resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		Files map[string][]byte `json:"files"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Files, err
}

// List implements Runtime.
func (c *Client) List(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sessiond/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("sessiond: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Agents []string `json:"agents"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sessiond: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Agents, err
}
