// Package k8s is the k8s location (design §5.4, journey 8): the agent
// CLI runs in a pod made from a container image, one pod per session,
// while silo, the gateway, credentials, sandboxes and the local
// executor stay on the invoking machine. silo drives the cluster with
// the system kubectl; the gateway reaches the pod through a tunnel
// sidecar (internal/tunnel) that silo connects to with kubectl
// port-forward, because Kubernetes has no reverse port-forward.
package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/internal/procgroup"
	"github.com/bwsalmon/silo/v2/internal/tunnel"
	"github.com/bwsalmon/silo/v2/location"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
)

// Config configures the k8s location.
type Config struct {
	Kubectl Kubectl
	// Image is the agent's image; TunnelImage the sidecar's,
	// DefaultTunnelImage when empty.
	Image, TunnelImage string
	Resources          Resources
	// ReadyTimeout bounds the wait for the pod to be ready, image pull
	// included; 5 minutes when zero.
	ReadyTimeout time.Duration
	// GC, when its Live is set, has Prepare remove this origin's stale
	// pods in the namespace, as silo gc --k8s does.
	GC GCOptions
	// Stderr receives the tunnel's messages; discarded when nil.
	Stderr io.Writer
}

// Location runs the CLI in a pod.
type Location struct {
	cfg Config
	k   *Kubectl
	s   *location.Session
	pod string
	// secret lists the variables the Secret holds, by name.
	secret  map[string]string
	created bool
	removed []string

	mu   sync.Mutex
	torn bool
	tun  *Tunnel
}

var _ location.Location = (*Location)(nil)

// New returns a k8s location.
func New(cfg Config) *Location {
	if cfg.TunnelImage == "" {
		cfg.TunnelImage = DefaultTunnelImage
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 5 * time.Minute
	}
	return &Location{cfg: cfg, k: &cfg.Kubectl}
}

// KubeTarget is the context and namespace the session's pod is in, which
// session.json records so that silo gc can delete the pod of a session
// that crashed.
func (l *Location) KubeTarget() (kubeContext, namespace string) {
	return l.k.Context, l.k.Namespace
}

// Pod is the session's pod, once Prepare named it.
func (l *Location) Pod() string { return l.pod }

// RemoteGC lists the stale pods and Secrets Prepare removed.
func (l *Location) RemoteGC() []string { return l.removed }

func (l *Location) where() string { return l.k.Where() }

// Prepare removes this origin's stale pods, creates the session's pod
// and Secret, waits for the pod to be ready and runs the probe in it.
func (l *Location) Prepare(ctx context.Context, s *location.Session) (location.HostInfo, error) {
	l.s = s
	l.pod = PodName(s.ID)
	if l.cfg.Image == "" {
		return location.HostInfo{}, errors.New("k8s: no image")
	}
	origin := l.cfg.GC.Origin
	if origin == "" {
		origin = sshloc.Origin()
	}
	if l.cfg.GC.Live != nil {
		opts := l.cfg.GC
		opts.Keep = s.ID
		var err error
		if l.removed, err = GC(ctx, l.k, opts); err != nil {
			return location.HostInfo{}, l.reachError(err)
		}
	}
	for k, v := range s.Secrets {
		if !envName.MatchString(k) {
			return location.HostInfo{}, fmt.Errorf("k8s: %q is not an environment variable name", k)
		}
		if l.secret == nil {
			l.secret = map[string]string{}
		}
		l.secret[k] = v
	}
	l.created = true
	uid, err := l.k.must(ctx, "creating pod "+l.pod+" in "+l.where(),
		podSpec(l.pod, s.ID, origin, l.cfg.Image, l.cfg.TunnelImage, l.cfg.Resources, l.secret != nil),
		"create", "-f", "-", "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return location.HostInfo{}, l.reachError(err)
	}
	if l.secret != nil {
		if _, err := l.k.must(ctx, "creating Secret "+l.pod, secretSpec(l.pod, s.ID, origin, strings.TrimSpace(uid), l.secret), "create", "-f", "-", "-o", "name"); err != nil {
			return location.HostInfo{}, err
		}
	}
	if err := l.waitReady(ctx); err != nil {
		return location.HostInfo{}, err
	}
	res, err := l.k.exec(ctx, l.pod, AgentContainer, "exec /bin/sh -s", []byte(sshloc.ProbeScript(s)))
	if err != nil {
		return location.HostInfo{}, err
	}
	if res.Code != 0 {
		return location.HostInfo{}, &location.PrerequisiteError{What: res.err("the probe in pod " + l.pod).Error(), Fix: "check that " + l.cfg.Image + " has /bin/sh"}
	}
	p := sshloc.ParseProbe(res.Stdout)
	info := location.HostInfo{OS: p.OS, Arch: p.Arch, Home: p.Home, Binary: p.Binary, Version: p.Version}
	return info, l.checkProbe(p, s)
}

func (l *Location) checkProbe(p sshloc.Probe, s *location.Session) error {
	img := "image " + l.cfg.Image
	if p.Binary == "" {
		if s.Binary != "" {
			return &location.PrerequisiteError{What: fmt.Sprintf("agent.binary %s is not an executable file in %s", s.Binary, img), Fix: "check agent.binary; silo never installs a CLI"}
		}
		return &location.PrerequisiteError{
			What: fmt.Sprintf("the agent CLI %s was not found on the PATH of %s", s.CLI, img),
			Fix:  fmt.Sprintf("build the image with %s in it (silo never installs a CLI), or set agent.binary to its absolute path there", s.CLI),
		}
	}
	if len(s.VersionArgs) > 0 && p.VersionExit != 0 {
		return &location.PrerequisiteError{
			What: fmt.Sprintf("%s %s in %s exited %d: %s", p.Binary, strings.Join(s.VersionArgs, " "), img, p.VersionExit, p.Version),
			Fix:  "check that the CLI runs in the image",
		}
	}
	if p.LoginChecked && !p.LoggedIn {
		return &location.PrerequisiteError{
			What: fmt.Sprintf("%s is not logged in in %s (agent.auth: ambient)", s.CLI, img),
			Fix:  "use agent.auth: {credential: ...}, which silo delivers as a Secret; a pod has no login of its own",
		}
	}
	return nil
}

// reachError explains a cluster silo could not reach.
func (l *Location) reachError(err error) error {
	var pe *location.PrerequisiteError
	if errors.As(err, &pe) {
		return err
	}
	return &location.PrerequisiteError{What: err.Error(), Fix: fmt.Sprintf("check that `%s` works", strings.Join(l.k.Argv("get", "pods"), " "))}
}

func (l *Location) status(ctx context.Context) (*podStatus, error) {
	out, err := l.k.must(ctx, "reading pod "+l.pod, nil, "get", "pod", l.pod, "-o", "json")
	if err != nil {
		return nil, err
	}
	var p podStatus
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return nil, fmt.Errorf("reading pod %s: %w", l.pod, err)
	}
	return &p, nil
}

// waitReady polls the pod until both its containers are ready, and
// fails at once on an image that cannot be pulled.
func (l *Location) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.cfg.ReadyTimeout)
	defer cancel()
	for {
		p, err := l.status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if p.ready() {
			return nil
		}
		if c, reason, msg := p.failure(); c != "" {
			img := l.cfg.Image
			if c == TunnelContainer {
				img = l.cfg.TunnelImage
			}
			return &location.PrerequisiteError{
				What: fmt.Sprintf("pod %s: container %s (%s): %s: %s", l.pod, c, img, reason, msg),
				Fix:  "check that the image exists and that the cluster's nodes can pull it (an imagePullSecret on the namespace's default service account)",
			}
		}
		if p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
			return fmt.Errorf("pod %s ended (%s) before it was ready", l.pod, p.Status.Phase)
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
			continue
		}
		break
	}
	return &location.PrerequisiteError{
		What: fmt.Sprintf("pod %s was not ready within %s", l.pod, l.cfg.ReadyTimeout),
		Fix:  fmt.Sprintf("see `%s`", strings.Join(l.k.Argv("describe", "pod", l.pod), " ")),
	}
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// Gateway connects to the tunnel sidecar with kubectl port-forward and
// returns the gateway URL on the pod's loopback. A goroutine keeps the
// connection up until Teardown: if the port-forward or the control
// connection drops, it opens them again.
func (l *Location) Gateway(ctx context.Context, local net.Addr) (string, error) {
	if l.pod == "" {
		return "", errors.New("k8s: Gateway before Prepare")
	}
	t, err := StartTunnel(ctx, l.k, l.pod, ControlPort, local.String(), l.logf)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	l.tun = t
	l.mu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", GatewayPort), nil
}

func (l *Location) logf(format string, args ...any) {
	if l.cfg.Stderr != nil {
		fmt.Fprintf(l.cfg.Stderr, "silo: k8s tunnel: "+format+"\n", args...)
	}
}

// Tunnel is silo's end of a tunnel sidecar (internal/tunnel): kubectl
// port-forward to the sidecar's control port, over which every
// connection the sidecar accepts arrives as a stream that silo splices
// to a target on the invoking machine.
type Tunnel struct {
	k      *Kubectl
	pod    string
	port   int
	target string
	logf   func(string, ...any)
	stop   context.CancelFunc
	done   chan struct{}
}

// StartTunnel connects to the sidecar of pod on its control port and
// splices its streams to target. A goroutine keeps the connection up
// until Close: if the port-forward or the control connection drops, it
// opens them again, saying so through logf, which may be nil.
func StartTunnel(ctx context.Context, k *Kubectl, pod string, port int, target string, logf func(string, ...any)) (*Tunnel, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := &Tunnel{k: k, pod: pod, port: port, target: target, logf: logf, done: make(chan struct{})}
	c, pf, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	bg, stop := context.WithCancel(context.WithoutCancel(ctx))
	t.stop = stop
	go t.keep(bg, c, pf)
	return t, nil
}

// Close ends the tunnel and waits for its port-forward to exit.
func (t *Tunnel) Close() {
	t.stop()
	<-t.done
}

// keep holds the tunnel up until bg ends.
func (t *Tunnel) keep(bg context.Context, c *tunnel.Client, pf *exec.Cmd) {
	defer close(t.done)
	for {
		select {
		case <-c.Done():
		case <-bg.Done():
		}
		c.Close()
		pf.Process.Kill()
		pf.Wait()
		if bg.Err() != nil {
			return
		}
		t.logf("the connection to pod %s dropped; reconnecting", t.pod)
		for {
			select {
			case <-bg.Done():
				return
			case <-time.After(time.Second):
			}
			cctx, cancel := context.WithTimeout(bg, 30*time.Second)
			var err error
			c, pf, err = t.connect(cctx)
			cancel()
			if err == nil {
				break
			}
			t.logf("%v", err)
		}
	}
}

// connect starts kubectl port-forward to the sidecar's control port on
// a free local port, and says hello over it.
func (t *Tunnel) connect(ctx context.Context) (*tunnel.Client, *exec.Cmd, error) {
	argv := t.k.Argv("port-forward", "--address", "127.0.0.1", "pod/"+t.pod, fmt.Sprintf(":%d", t.port))
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	var errb capBuf
	cmd.Stderr = &errb
	procgroup.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, &location.PrerequisiteError{What: fmt.Sprintf("cannot run kubectl: %v", err), Fix: "install kubectl"}
	}
	fail := func(err error) (*tunnel.Client, *exec.Cmd, error) {
		cmd.Process.Kill()
		cmd.Wait()
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, nil, fmt.Errorf("the tunnel to pod %s: %w", t.pod, err)
	}
	ports := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := forwarding.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case ports <- m[1]:
				default:
				}
			}
		}
		io.Copy(io.Discard, out)
	}()
	var port string
	select {
	case port = <-ports:
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-time.After(30 * time.Second):
		return fail(errors.New("kubectl port-forward did not start"))
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return fail(err)
	}
	c, err := tunnel.Connect(ctx, conn)
	if err != nil {
		conn.Close()
		return fail(err)
	}
	go c.Serve(t.target)
	return c, cmd, nil
}

// Stage writes each file into /silo/agent in the pod with umask 077.
func (l *Location) Stage(ctx context.Context, files []location.StagedFile) (string, error) {
	if l.pod == "" {
		return "", errors.New("k8s: Stage before Prepare")
	}
	dir := PodDir + "/agent"
	for _, f := range files {
		name := path.Clean(f.Name)
		if path.IsAbs(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("k8s: staged file %q is outside the staging directory", f.Name)
		}
		p := dir + "/" + name
		script := "umask 077 && mkdir -p -- " + sshloc.Quote(path.Dir(p)) + " && "
		stdin := []byte{}
		if f.Link != "" {
			script += "ln -sfn -- " + sshloc.Quote(f.Link) + " " + sshloc.Quote(p)
		} else {
			script += "cat > " + sshloc.Quote(p)
			if f.Mode != 0 && f.Mode.Perm() != 0o600 {
				script += fmt.Sprintf(" && chmod %o %s", f.Mode.Perm(), sshloc.Quote(p))
			}
			stdin = f.Data
		}
		res, err := l.k.exec(ctx, l.pod, AgentContainer, script, stdin)
		if err != nil {
			return "", err
		}
		if res.Code != 0 {
			return "", res.err("staging " + f.Name + " in pod " + l.pod)
		}
	}
	return dir, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Start execs the CLI into the agent container: kubectl exec -i -t
// (interactive) or -i (headless). A variable whose value the Secret
// holds is read from its mounted file; the rest go in a 0600 env file
// on the pod's emptyDir, which the preamble sources and deletes, so no
// value appears in an argv.
func (l *Location) Start(ctx context.Context, c location.Command, sio location.IO) (location.Agent, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("k8s: empty command")
	}
	if l.pod == "" {
		return nil, errors.New("k8s: Start before Prepare")
	}
	envFile, pidFile := PodDir+"/env", PodDir+"/pid"
	var env, fromSecret bytes.Buffer
	for _, kv := range c.Env {
		k, v, _ := strings.Cut(kv, "=")
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("k8s: %q is not an environment variable name", k)
		}
		if sv, ok := l.secret[k]; ok && sv == v {
			fmt.Fprintf(&fromSecret, "%s=$(cat %s) && export %s && ", k, sshloc.Quote(SecretDir+"/"+k), k)
			continue
		}
		fmt.Fprintf(&env, "export %s=%s\n", k, sshloc.Quote(v))
	}
	if env.Len() > 0 {
		res, err := l.k.exec(ctx, l.pod, AgentContainer, "umask 077 && cat > "+sshloc.Quote(envFile), env.Bytes())
		if err != nil {
			return nil, err
		}
		if res.Code != 0 {
			return nil, res.err("writing the agent's environment in pod " + l.pod)
		}
	}
	wd := c.Workdir
	pre := ""
	if wd == "" {
		wd = PodDir + "/work"
		pre = "mkdir -m 700 -p -- " + sshloc.Quote(wd) + " && "
	}
	q := sshloc.Quote
	script := pre + "cd -- " + q(wd) + " || exit 1; " +
		"if [ -f " + q(envFile) + " ]; then . " + q(envFile) + "; rm -f -- " + q(envFile) + "; fi; " +
		fromSecret.String() + "echo $$ > " + q(pidFile) + " && exec " + sshloc.QuoteArgv(c.Argv)
	args := []string{"exec", "--quiet", "-i"}
	if c.TTY {
		args = append(args, "-t")
	}
	argv := l.k.Argv(append(args, l.pod, "-c", AgentContainer, "--", "/bin/sh", "-c", script)...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = sio.Stdout, sio.Stderr
	if c.TTY {
		// kubectl puts silo's terminal in raw mode and carries
		// window-size changes itself.
		cmd.Stdin = sio.Stdin
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = bytes.NewReader(c.Stdin)
		if c.Input != nil {
			cmd.Stdin = c.Input
		}
	}
	procgroup.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &agent{l: l, cmd: cmd, pidFile: pidFile, done: make(chan struct{})}
	go func() {
		a.err = cmd.Wait()
		close(a.done)
	}()
	return a, nil
}

// Teardown closes the tunnel and deletes the session's pod and Secret,
// by label so that one whose create call failed half way goes too. It
// does not wait for the pod to finish terminating. It runs once.
func (l *Location) Teardown(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.torn {
		return nil
	}
	l.torn = true
	if l.tun != nil {
		l.tun.Close()
	}
	if !l.created {
		return nil
	}
	return DeleteSession(ctx, l.k, l.s.ID)
}

// DeleteSession deletes session id's pod and Secret.
func DeleteSession(ctx context.Context, k *Kubectl, id string) error {
	_, err := k.must(ctx, "deleting the pod and Secret of session "+id+" in "+k.Where(), nil,
		"delete", "pods,secrets", "-l", SessionLabel+"="+id, "--ignore-not-found", "--wait=false")
	return err
}

type agent struct {
	l       *Location
	cmd     *exec.Cmd
	pidFile string
	done    chan struct{}
	err     error
}

// Wait returns the CLI's exit status, which kubectl exec passes on. A
// failure with the pod no longer running is ConnLost (exit 75).
func (a *agent) Wait() (location.ExitStatus, error) {
	<-a.done
	if a.err == nil {
		return location.ExitStatus{}, nil
	}
	var ee *exec.ExitError
	if !errors.As(a.err, &ee) {
		return location.ExitStatus{Code: 1}, a.err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return location.ExitStatus{Code: 128 + int(ws.Signal()), Signaled: ws.Signal().String()}, nil
	}
	code := ee.ExitCode()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if p, err := a.l.status(ctx); err != nil || p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != "" {
		return location.ExitStatus{Code: 75, ConnLost: true}, nil
	}
	return location.ExitStatus{Code: code}, nil
}

// Signal signals the CLI in the pod with another exec. A kill also ends
// the local kubectl, which ends the exec stream.
func (a *agent) Signal(sig location.Signal) error {
	select {
	case <-a.done:
		return nil
	default:
	}
	name := map[location.Signal]string{location.Interrupt: "INT", location.Terminate: "TERM", location.Kill: "KILL"}[sig]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := sshloc.Quote(a.pidFile)
	res, err := a.l.k.exec(ctx, a.l.pod, AgentContainer, fmt.Sprintf(`p=$(cat %s 2>/dev/null) && [ -n "$p" ] && { kill -s %s -- "-$p" 2>/dev/null || kill -s %s "$p"; }`, p, name, name), nil)
	sent := err == nil && res.Code == 0
	if sig == location.Kill || !sent {
		if err := a.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

// Resize does nothing: kubectl exec -t carries window-size changes
// itself.
func (a *agent) Resize(rows, cols int) error { return nil }

// PGID is the local kubectl's process group in headless mode, which
// session.json records for silo gc; 0 in interactive mode.
func (a *agent) PGID() int {
	if a.cmd.SysProcAttr == nil || !a.cmd.SysProcAttr.Setpgid {
		return 0
	}
	return a.cmd.Process.Pid
}

// capBuf keeps the first 16 KiB written to it.
type capBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *capBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := 16<<10 - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return n, nil
}

func (b *capBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
