// Package fake is a runtime.Driver in memory (local-backend.md §6, §13):
// containers move through their states, keep their files and output,
// and exit when stopped, killed or told to, but nothing runs. Tests of
// the local backend and the Docker facade run on it.
package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// Driver is the fake. Its zero value is not usable; call New.
type Driver struct {
	class runtime.Class
	// Volumes is the storage driver's root, which volume mounts must be
	// under (runtime.Spec.Validate).
	Volumes string
	// ExecFunc runs an exec'd process. Nil exits 0 having done nothing.
	ExecFunc func(id string, p runtime.Process, stdio runtime.ExecIO) int

	mu       sync.Mutex
	probe    runtime.Probe
	prepared map[string]bool
	cs       map[string]*container
	now      func() time.Time
}

type container struct {
	spec  runtime.Spec
	state runtime.State
	exit  *runtime.ExitStatus
	done  chan struct{}
	// prev is the run before a restart's exit, for a Wait woken by
	// that run's done after the restart reset exit.
	prev    *runtime.ExitStatus
	files   map[string][]byte // regular files by path; dirs are "path/"
	deleted map[string]bool   // the image's files Remove deleted
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	stdin   bytes.Buffer
	starts  int
	signals []syscall.Signal
}

var _ runtime.Driver = (*Driver)(nil)

// New returns a fake driver for class, whose probe passes.
func New(class runtime.Class) *Driver {
	return &Driver{class: class, probe: runtime.Probe{OK: true}, prepared: map[string]bool{},
		cs: map[string]*container{}, now: time.Now}
}

// Class implements runtime.Driver.
func (d *Driver) Class() runtime.Class { return d.class }

// SetProbe sets what Probe reports.
func (d *Driver) SetProbe(p runtime.Probe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probe = p
}

// Probe implements runtime.Driver.
func (d *Driver) Probe(context.Context) runtime.Probe {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probe
}

// Prepare implements runtime.Driver.
func (d *Driver) Prepare(_ context.Context, image string) error {
	if !strings.HasPrefix(image, "sha256:") {
		return fmt.Errorf("fake: image %q is not a digest", image)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prepared[image] = true
	return nil
}

// Prepared is whether Prepare was called for image.
func (d *Driver) Prepared(image string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.prepared[image]
}

// Create implements runtime.Driver. It refuses an image Prepare hasn't
// seen, as the vm class must.
func (d *Driver) Create(_ context.Context, spec runtime.Spec) error {
	if err := spec.Validate(d.Volumes); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.prepared[spec.Image] {
		return fmt.Errorf("fake: image %s is not prepared", spec.Image)
	}
	if _, ok := d.cs[spec.ID]; ok {
		return fmt.Errorf("fake: container %s exists", spec.ID)
	}
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	spec.Labels = labels
	d.cs[spec.ID] = &container{spec: spec, state: runtime.Created, files: map[string][]byte{}, done: make(chan struct{})}
	return nil
}

func (d *Driver) get(id string) (*container, error) {
	c, ok := d.cs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	}
	return c, nil
}

// Start implements runtime.Driver. A stopped container starts again,
// as Docker restarts one.
func (d *Driver) Start(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	switch c.state {
	case runtime.Created:
	case runtime.Stopped:
		c.prev, c.exit, c.done = c.exit, nil, make(chan struct{})
	default:
		return fmt.Errorf("%w: start of a %s container", runtime.ErrState, c.state)
	}
	c.state = runtime.Running
	c.starts++
	return nil
}

// exit stops c with status; d.mu is held.
func (d *Driver) exit(c *container, code int, sig syscall.Signal) {
	if c.state == runtime.Stopped {
		return
	}
	c.state = runtime.Stopped
	c.exit = &runtime.ExitStatus{Code: code, Signal: sig, Finished: d.now()}
	close(c.done)
}

// Exit makes a running container's workload exit with code by itself.
func (d *Driver) Exit(id string, code int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	if c.state != runtime.Running {
		return fmt.Errorf("%w: exit of a %s container", runtime.ErrState, c.state)
	}
	d.exit(c, code, 0)
	return nil
}

// Stop implements runtime.Driver: the fake's workload honours the stop
// signal at once, exiting 128+signal as a shell would report it.
func (d *Driver) Stop(ctx context.Context, id string, _ time.Duration) error {
	d.mu.Lock()
	c, err := d.get(id)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	sig := c.spec.StopSignal
	if sig == 0 {
		sig = syscall.SIGTERM
	}
	return d.Kill(ctx, id, sig)
}

// Kill implements runtime.Driver. Every signal but SIGCONT, SIGWINCH
// and SIGCHLD ends the fake's workload.
func (d *Driver) Kill(_ context.Context, id string, sig syscall.Signal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	if c.state != runtime.Running && c.state != runtime.Paused {
		return fmt.Errorf("%w: kill of a %s container", runtime.ErrState, c.state)
	}
	c.signals = append(c.signals, sig)
	switch sig {
	case syscall.SIGCONT, syscall.SIGWINCH, syscall.SIGCHLD:
		return nil
	}
	d.exit(c, 128+int(sig), sig)
	return nil
}

// Signals are the signals a container was sent.
func (d *Driver) Signals(id string) []syscall.Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cs[id]; ok {
		return append([]syscall.Signal(nil), c.signals...)
	}
	return nil
}

// Pause implements runtime.Driver.
func (d *Driver) Pause(_ context.Context, id string) error {
	return d.move(id, runtime.Running, runtime.Paused)
}

// Resume implements runtime.Driver.
func (d *Driver) Resume(_ context.Context, id string) error {
	return d.move(id, runtime.Paused, runtime.Running)
}

func (d *Driver) move(id string, from, to runtime.State) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	if c.state != from {
		return fmt.Errorf("%w: %s container, want %s", runtime.ErrState, c.state, from)
	}
	c.state = to
	return nil
}

// Delete implements runtime.Driver.
func (d *Driver) Delete(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	if c.state != runtime.Created && c.state != runtime.Stopped {
		return fmt.Errorf("%w: delete of a %s container", runtime.ErrState, c.state)
	}
	delete(d.cs, id)
	return nil
}

// Wait implements runtime.Driver.
func (d *Driver) Wait(ctx context.Context, id string) (runtime.ExitStatus, error) {
	d.mu.Lock()
	c, err := d.get(id)
	var done chan struct{}
	if err == nil {
		done = c.done
	}
	d.mu.Unlock()
	if err != nil {
		return runtime.ExitStatus{}, err
	}
	select {
	case <-ctx.Done():
		return runtime.ExitStatus{}, ctx.Err()
	case <-done:
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.done != done {
		// Restarted since this run ended.
		return *c.prev, nil
	}
	return *c.exit, nil
}

// Exec implements runtime.Driver.
func (d *Driver) Exec(_ context.Context, id string, p runtime.Process, stdio runtime.ExecIO) (int, error) {
	d.mu.Lock()
	c, err := d.get(id)
	if err == nil && c.state != runtime.Running {
		err = fmt.Errorf("%w: exec in a %s container", runtime.ErrState, c.state)
	}
	f := d.ExecFunc
	d.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if len(p.Args) == 0 {
		return 0, fmt.Errorf("fake: exec with no args")
	}
	if f == nil {
		return 0, nil
	}
	return f(id, p, stdio), nil
}

// Output appends to a container's workload output, as if it had
// written it.
func (d *Driver) Output(id, stdout, stderr string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	c.stdout.WriteString(stdout)
	c.stderr.WriteString(stderr)
	return nil
}

// Stdin is what was written to a container's stdin.
func (d *Driver) Stdin(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cs[id]; ok {
		return c.stdin.String()
	}
	return ""
}

type stdinWriter struct {
	d *Driver
	c *container
}

func (w stdinWriter) Write(b []byte) (int, error) {
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	return w.c.stdin.Write(b)
}

func (stdinWriter) Close() error { return nil }

// Streams implements runtime.Driver: the output so far, and stdin when
// the workload asked for it.
func (d *Driver) Streams(_ context.Context, id string) (runtime.Streams, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return runtime.Streams{}, err
	}
	s := runtime.Streams{
		Stdout: io.NopCloser(bytes.NewReader(bytes.Clone(c.stdout.Bytes()))),
		Stderr: io.NopCloser(bytes.NewReader(bytes.Clone(c.stderr.Bytes()))),
	}
	if c.spec.Process.Stdin {
		s.Stdin = stdinWriter{d, c}
	}
	return s, nil
}

// Stats implements runtime.Driver: a running container uses one
// process and nothing else.
func (d *Driver) Stats(_ context.Context, id string) (runtime.Stats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return runtime.Stats{}, err
	}
	st := runtime.Stats{SampledAt: d.now()}
	if c.state == runtime.Running || c.state == runtime.Paused {
		st.Processes = 1
	}
	return st, nil
}

// CopyIn implements runtime.Driver, for regular files and directories.
func (d *Driver) CopyIn(_ context.Context, id, dir string, r io.Reader) error {
	if !path.IsAbs(dir) {
		return fmt.Errorf("fake: copy into %q, not an absolute path", dir)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p := path.Join(dir, h.Name)
		if !strings.HasPrefix(p+"/", path.Clean(dir)+"/") && path.Clean(dir) != "/" {
			return fmt.Errorf("fake: %q leaves %s", h.Name, dir)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			files[p+"/"] = nil
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			files[p] = b
		default:
			return fmt.Errorf("fake: %s: only files and directories", h.Name)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	for p, b := range files {
		c.files[p] = b
		delete(c.deleted, strings.TrimSuffix(p, "/"))
	}
	return nil
}

// Remove deletes p from the container's root, as its workload would: a
// file of its own goes, and a file of the image's becomes a deletion
// in its writable layer.
func (d *Driver) Remove(id, p string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, err := d.get(id)
	if err != nil {
		return err
	}
	p = path.Clean(p)
	delete(c.files, p)
	delete(c.files, p+"/")
	if c.deleted == nil {
		c.deleted = map[string]bool{}
	}
	c.deleted[p] = true
	return nil
}

var _ runtime.Committer = (*Driver)(nil)

// RootfsUpper implements runtime.Committer: the files CopyIn wrote, and
// Remove's deletions as overlayfs whiteouts (0/0 character devices),
// which is what the caller has to convert.
func (d *Driver) RootfsUpper(_ context.Context, id string) (io.ReadCloser, error) {
	d.mu.Lock()
	c, err := d.get(id)
	var names []string
	files := map[string][]byte{}
	if err == nil {
		for f, b := range c.files {
			names = append(names, f)
			files[f] = b
		}
		for f := range c.deleted {
			names = append(names, f+"\x00")
		}
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range names {
		switch {
		case strings.HasSuffix(f, "\x00"):
			tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(strings.TrimSuffix(f, "\x00"), "/"), Typeflag: tar.TypeChar, Mode: 0o600})
		case strings.HasSuffix(f, "/"):
			tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(f, "/"), Typeflag: tar.TypeDir, Mode: 0o755})
		default:
			tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(f, "/"), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[f]))})
			tw.Write(files[f])
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// CopyOut implements runtime.Driver. A path it doesn't have is
// fs.ErrNotExist.
func (d *Driver) CopyOut(_ context.Context, id, p string) (io.ReadCloser, error) {
	p = path.Clean(p)
	d.mu.Lock()
	c, err := d.get(id)
	var names []string
	files := map[string][]byte{}
	if err == nil {
		for f, b := range c.files {
			if f == p || f == p+"/" || strings.HasPrefix(f, strings.TrimSuffix(p, "/")+"/") {
				names = append(names, f)
				files[f] = b
			}
		}
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("fake: %s in %s: %w", p, id, fs.ErrNotExist)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Dir(p)
	for _, f := range names {
		rel := strings.TrimPrefix(strings.TrimPrefix(f, base), "/")
		if strings.HasSuffix(f, "/") {
			tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[f]))})
		tw.Write(files[f])
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// List implements runtime.Driver.
func (d *Driver) List(context.Context) ([]runtime.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []runtime.Container
	for id, c := range d.cs {
		labels := map[string]string{}
		for k, v := range c.spec.Labels {
			labels[k] = v
		}
		rc := runtime.Container{ID: id, Class: d.class, State: c.state, Labels: labels}
		if c.exit != nil {
			e := *c.exit
			rc.Exit = &e
		}
		out = append(out, rc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Spec is the spec a container was created with.
func (d *Driver) Spec(id string) (runtime.Spec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.cs[id]
	if !ok {
		return runtime.Spec{}, false
	}
	return c.spec, true
}
