// Adapted from github.com/bwsalmon/grain pkg/orchestrator/k8s_granule_sandboxes.go and pkg/mcp/kubectl_exec_runner.go at 28f163de7e579d358d4d66b58a4b3a2830e6b8d6, Apache License 2.0.

// Package k8s is the Kubernetes sandbox backend (design §7.3, journey
// 9): a granule sandbox with backend: {k8s: {context, namespace}} runs
// as a pod whose one container is its granule VM image, booting the
// guest with `granule run` (the image's own entrypoint), and silo
// reaches the guest with `kubectl exec <pod> -- granule exec`, which
// granule relays to its in-guest agent over the VM's vsock device.
//
// The pod is the shape of gke-labs/granule deploy/k8s: it is not
// privileged, and it gets /dev/kvm (and, for a guest with a network,
// /dev/net/tun) by requesting the devic.es/kvm and devic.es/tun
// resources that granule's KVM device plugin (kvm-device-plugin.yaml)
// publishes. A guest with network none has no network device at all
// (CHV_NET_MODE=none), needs neither NET_ADMIN nor tun, runs as nobody
// and passes the restricted Pod Security Standard; a guest with network
// open takes over the pod's own address (CHV_NET_MODE=inline), which
// needs root and NET_ADMIN in the pod's own network namespace. Like
// grain's k8s-granule backend (grain docs/k8s-granule-design.md), the
// pod carries `granule ready` as its readiness probe, so Ready means the
// guest answers, and sets args rather than command, so the image's
// entrypoint still boots the VM.
//
// silo drives the cluster with the system kubectl (location/k8s.Kubectl),
// and labels each pod as the k8s location labels its own (managed-by
// silo, the session, an origin annotation), so `silo gc --k8s` and a
// crashed session's record remove sandbox pods too.
package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/v2/internal/headtail"
	k8sloc "github.com/bwsalmon/silo/v2/location/k8s"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/granule"
)

// Labels and annotations a sandbox pod carries beside the k8s
// location's (k8sloc.ManagedByLabel, SessionLabel, OriginAnnotation).
const (
	// RoleLabel tells sandbox pods from the location's agent pod.
	RoleLabel = "silo.dev/role"
	Role      = "sandbox"
	// NameAnnotation is the sandbox's name in the Silofile, which need
	// not be a valid label value.
	NameAnnotation = "silo.dev/sandbox"
	// WorkdirAnnotation is the sandbox's workdir, which a found pod
	// runs commands in.
	WorkdirAnnotation = "silo.dev/workdir"
)

// The device-plugin resources (gke-labs/granule deploy/k8s
// kvm-device-plugin.yaml).
const (
	KVMResource = "devic.es/kvm"
	TunResource = "devic.es/tun"
)

// Container is the pod's one container, which runs the VM.
const Container = "vm"

// Granule is granule's binary in a granule VM image.
const Granule = "/usr/local/bin/granule"

// MemoryOverhead is what the pod's memory limit allows beyond the
// guest's own memory: cloud-hypervisor, granule's supervisor and the
// page cache of the guest disk.
const MemoryOverhead = 512 << 20

// NobodyUID is the user a guest with no network runs its VMM as
// (granule gke-pod-example.yaml).
const NobodyUID = 65534

// DefaultReadyTimeout bounds the wait for the guest to answer,
// pulling the image included: granule VM images carry a guest disk and
// are gigabytes.
const DefaultReadyTimeout = 10 * time.Minute

// Backend runs granule sandboxes as pods.
type Backend struct {
	// Kubectl is the kubectl binary and kubeconfig; Context and
	// Namespace come from each sandbox's target.
	Kubectl k8sloc.Kubectl
	// Targets are where Find and Sessions look: every cluster and
	// namespace the Silofile's sandboxes name.
	Targets []sandbox.K8sTarget
	// Origin is ssh.Origin() when empty: which machine and user made a
	// pod, as the k8s location records it.
	Origin string
	// ReadyTimeout is DefaultReadyTimeout when zero.
	ReadyTimeout time.Duration
	// NodeSelector, when set, pins sandbox pods to nodes that carry
	// these labels (a node pool with nested virtualisation).
	NodeSelector map[string]string
}

var (
	_ sandbox.Backend       = (*Backend)(nil)
	_ sandbox.SessionLister = (*Backend)(nil)
)

// TargetsOf lists the distinct contexts and namespaces of specs, in
// order.
func TargetsOf(specs []sandbox.SandboxSpec) []sandbox.K8sTarget {
	var out []sandbox.K8sTarget
	seen := map[sandbox.K8sTarget]bool{}
	for _, s := range specs {
		if s.K8s == nil {
			continue
		}
		t := sandbox.K8sTarget{Context: s.K8s.Context, Namespace: s.K8s.Namespace}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (b *Backend) kubectl(t sandbox.K8sTarget) *k8sloc.Kubectl {
	k := b.Kubectl
	k.Context, k.Namespace = t.Context, t.Namespace
	return &k
}

func (b *Backend) origin() string {
	if b.Origin != "" {
		return b.Origin
	}
	return sshloc.Origin()
}

// PodName is the pod of a session's sandbox: the session's own name,
// as the k8s location gives it, and a digest of the sandbox's, which
// need not be a DNS label.
func PodName(session, name string) string {
	sum := sha256.Sum256([]byte(session + "/" + name))
	return k8sloc.PodName(session) + "-" + strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:8]
}

type obj = map[string]any

// PodSpec is the pod manifest for spec, as JSON (which kubectl reads
// as YAML).
func PodSpec(spec sandbox.SandboxSpec, origin string, nodeSelector map[string]string) ([]byte, error) {
	open := false
	switch spec.Network {
	case "", "none":
	case "open":
		open = true
	default:
		return nil, fmt.Errorf("sandbox %q: network %q is not none or open", spec.Name, spec.Network)
	}
	labels := obj{
		k8sloc.ManagedByLabel: k8sloc.ManagedBy,
		k8sloc.SessionLabel:   spec.Session,
		RoleLabel:             Role,
	}
	annotations := obj{k8sloc.OriginAnnotation: origin, NameAnnotation: spec.Name}
	if spec.Workdir != "" {
		annotations[WorkdirAnnotation] = spec.Workdir
	}
	env := []obj{}
	add := func(k, v string) { env = append(env, obj{"name": k, "value": v}) }
	limits := obj{KVMResource: "1"}
	requests := obj{}
	if open {
		// The guest takes over the pod's address and MAC (granule's
		// netshim), so the pod is reached as any other pod is.
		add("CHV_NET_MODE", "inline")
		add("NETSHIM_VM", Container)
		limits[TunResource] = "1"
	} else {
		add("CHV_NET_MODE", "none")
	}
	if spec.CPUs > 0 {
		add("CHV_CPUS", strconv.Itoa(int(math.Ceil(spec.CPUs))))
		cpu := strconv.FormatFloat(spec.CPUs, 'f', -1, 64)
		requests["cpu"] = cpu
	}
	if spec.Memory > 0 {
		mb := (spec.Memory + 1<<20 - 1) >> 20
		add("CHV_MEMORY_MB", strconv.FormatInt(mb, 10))
		mem := strconv.FormatInt(mb<<20+MemoryOverhead, 10)
		requests["memory"], limits["memory"] = mem, mem
	}
	if spec.Disk > 0 {
		add("CHV_DISK_SIZE_MB", strconv.FormatInt((spec.Disk+1<<20-1)>>20, 10))
	}
	add("CHV_SHUTDOWN_TIMEOUT", "20s")
	resources := obj{"limits": limits}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	csc := obj{
		"allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem":   true,
		"capabilities":             obj{"drop": []string{"ALL"}},
	}
	podSC := obj{"seccompProfile": obj{"type": "RuntimeDefault"}}
	if open {
		// NET_ADMIN makes the tap and splices it onto eth0, in the
		// pod's own network namespace only; nothing else is added.
		csc["capabilities"] = obj{"drop": []string{"ALL"}, "add": []string{"NET_ADMIN"}}
	} else {
		podSC["runAsNonRoot"] = true
		podSC["runAsUser"] = NobodyUID
		podSC["runAsGroup"] = NobodyUID
		if spec.K8s != nil && spec.K8s.KVMGroup != 0 {
			podSC["supplementalGroups"] = []int64{spec.K8s.KVMGroup}
		}
	}
	pod := obj{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": obj{
			"name":        PodName(spec.Session, spec.Name),
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": obj{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			// Beyond CHV_SHUTDOWN_TIMEOUT, so granule's own shutdown
			// finishes before the kubelet kills the container.
			"terminationGracePeriodSeconds": 40,
			"securityContext":               podSC,
			"containers": []obj{{
				"name":  Container,
				"image": spec.Image,
				// args, not command: the image's entrypoint is granule's
				// shim, and "run" boots the VM.
				"args": []string{"run"},
				"env":  env,
				"readinessProbe": obj{
					"exec":           obj{"command": []string{Granule, "ready", "-timeout", "0"}},
					"periodSeconds":  2,
					"timeoutSeconds": 5,
				},
				"resources":       resources,
				"securityContext": csc,
				"volumeMounts": []obj{
					{"name": "run", "mountPath": "/run"},
					{"name": "tmp", "mountPath": "/tmp"},
					{"name": "overlay", "mountPath": "/var/lib/granule/overlay"},
				},
			}},
			"volumes": []obj{
				{"name": "run", "emptyDir": obj{"medium": "Memory"}},
				{"name": "tmp", "emptyDir": obj{}},
				{"name": "overlay", "emptyDir": obj{}},
			},
		},
	}
	if len(nodeSelector) > 0 {
		pod["spec"].(obj)["nodeSelector"] = nodeSelector
	}
	return json.MarshalIndent(pod, "", "  ")
}

// Create starts spec's pod and waits for its guest to answer.
func (b *Backend) Create(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Sandbox, error) {
	switch {
	case spec.Session == "" || spec.Name == "":
		return nil, errors.New("a sandbox needs a session and a name")
	case spec.K8s == nil:
		return nil, fmt.Errorf("sandbox %q: no k8s backend", spec.Name)
	case spec.Kind != "granule":
		return nil, fmt.Errorf("sandbox %q: backend k8s runs kind granule only, not %q", spec.Name, spec.Kind)
	case spec.Image == "":
		return nil, fmt.Errorf("sandbox %q: kind granule needs image: a granule VM image", spec.Name)
	case spec.Build != "":
		return nil, fmt.Errorf("sandbox %q: a granule sandbox is booted from image:, not build:", spec.Name)
	case len(spec.Mounts) > 0:
		return nil, fmt.Errorf("sandbox %q: kind granule takes no mounts", spec.Name)
	}
	if err := granule.CheckEnv(spec.Env); err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	manifest, err := PodSpec(spec, b.origin(), b.NodeSelector)
	if err != nil {
		return nil, err
	}
	k := b.kubectl(*spec.K8s)
	p := &Pod{k: k, Pod: PodName(spec.Session, spec.Name), Name: spec.Name, Session: spec.Session, Workdir: spec.Workdir}
	if _, err := k.Output(ctx, "creating pod "+p.Pod+" in "+k.Where(), manifest, "create", "-f", "-", "-o", "name"); err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	cleanup := func() { _ = p.Destroy(context.WithoutCancel(ctx)) }
	timeout := b.ReadyTimeout
	if timeout == 0 {
		timeout = DefaultReadyTimeout
	}
	digest, err := p.waitReady(ctx, timeout)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("sandbox %q: %w", spec.Name, err)
	}
	p.Image = sandbox.Image{Ref: spec.Image, Digest: digest}
	if err := p.guest().Setup(ctx, spec.Workdir, spec.Env); err != nil {
		cleanup()
		return nil, fmt.Errorf("sandbox %q: setting up the guest: %w", spec.Name, err)
	}
	return p, nil
}

// podStatus is the part of a pod's status Create reads.
type podStatus struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name    string `json:"name"`
			Ready   bool   `json:"ready"`
			ImageID string `json:"imageID"`
			State   struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
					Message  string `json:"message"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// waitReady polls the pod until its guest answers its readiness probe,
// and fails at once on an image that cannot be pulled or a VM that
// exits. It returns the image's digest as the node pulled it.
func (p *Pod) waitReady(ctx context.Context, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last string
	for {
		out, err := p.k.Output(ctx, "reading pod "+p.Pod, nil, "get", "pod", p.Pod, "-o", "json")
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return "", err
		}
		var st podStatus
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return "", fmt.Errorf("reading pod %s: %w", p.Pod, err)
		}
		for _, c := range st.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" && c.Message != "" {
				last = "not scheduled: " + c.Message
			}
		}
		for _, c := range st.Status.ContainerStatuses {
			if c.Name != Container {
				continue
			}
			if c.Ready {
				return c.ImageID, nil
			}
			if w := c.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
					return "", fmt.Errorf("pod %s: %s: %s", p.Pod, w.Reason, w.Message)
				}
				last = w.Reason
			}
			if t := c.State.Terminated; t != nil {
				return "", fmt.Errorf("pod %s: the VM exited (%d, %s) before its guest answered -- check that the image is a granule VM image and that the node has /dev/kvm:\n%s", p.Pod, t.ExitCode, t.Reason, p.logs())
			}
		}
		if st.Status.Phase == "Failed" || st.Status.Phase == "Succeeded" {
			return "", fmt.Errorf("pod %s: %s before its guest answered:\n%s", p.Pod, st.Status.Phase, p.logs())
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			continue
		}
		break
	}
	if err := context.Cause(ctx); errors.Is(err, context.Canceled) {
		return "", err
	}
	msg := fmt.Sprintf("pod %s: its guest did not answer within %s", p.Pod, timeout)
	if last != "" {
		msg += " (" + last + ")"
	}
	if strings.Contains(last, KVMResource) {
		msg += "; no node offers " + KVMResource + ": apply granule's deploy/k8s/kvm-device-plugin.yaml on nodes with nested virtualisation"
	}
	if logs := p.logs(); logs != "" {
		msg += ":\n" + logs
	}
	return "", errors.New(msg)
}

// logs is the end of the VM container's log: the guest console.
func (p *Pod) logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := p.k.Output(ctx, "", nil, "logs", p.Pod, "-c", Container, "--tail", "20")
	return strings.TrimSpace(out)
}

// list returns the sandbox pods in t matching selector.
func (b *Backend) list(ctx context.Context, t sandbox.K8sTarget, selector string) ([]*Pod, error) {
	k := b.kubectl(t)
	out, err := k.Output(ctx, "listing sandbox pods in "+k.Where(), nil, "get", "pods", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []podStatus `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil, fmt.Errorf("listing sandbox pods: %w", err)
	}
	var pods []*Pod
	for _, it := range l.Items {
		m := it.Metadata
		if m.Annotations[k8sloc.OriginAnnotation] != b.origin() {
			continue
		}
		pods = append(pods, &Pod{k: k, Pod: m.Name, Name: m.Annotations[NameAnnotation], Session: m.Labels[k8sloc.SessionLabel], Workdir: m.Annotations[WorkdirAnnotation]})
	}
	return pods, nil
}

// Find lists the sandbox pods of session in every target.
func (b *Backend) Find(ctx context.Context, session string) ([]sandbox.Sandbox, error) {
	var found []sandbox.Sandbox
	var errs []error
	for _, t := range b.Targets {
		pods, err := b.list(ctx, t, RoleLabel+"="+Role+","+k8sloc.SessionLabel+"="+session)
		errs = append(errs, err)
		for _, p := range pods {
			found = append(found, p)
		}
	}
	return found, errors.Join(errs...)
}

// Sessions lists the sessions this origin's sandbox pods belong to.
func (b *Backend) Sessions(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var sessions []string
	var errs []error
	for _, t := range b.Targets {
		pods, err := b.list(ctx, t, RoleLabel+"="+Role)
		errs = append(errs, err)
		for _, p := range pods {
			if p.Session != "" && !seen[p.Session] {
				seen[p.Session] = true
				sessions = append(sessions, p.Session)
			}
		}
	}
	return sessions, errors.Join(errs...)
}

// Pod is one granule sandbox pod.
type Pod struct {
	k *k8sloc.Kubectl
	// Pod is the pod's name.
	Pod string
	// Name is the sandbox's name in the Silofile.
	Name    string
	Session string
	// Workdir is where a command with no Cwd runs; empty means the
	// guest account's home directory.
	Workdir string
	// Image is what the image resolved to, for .silo/lock.json. Empty
	// for a sandbox found rather than created.
	Image sandbox.Image
}

var _ sandbox.Sandbox = (*Pod)(nil)

// OutputLimit bounds each of a command's output streams.
const OutputLimit = granule.OutputLimit

// killGrace is how long Exec waits, once its context is done and the
// command has been killed in the guest, for kubectl to return what it
// has before killing kubectl as well.
const killGrace = 3 * time.Second

// ExecArgs returns the kubectl arguments that run req in the guest as
// tag.
func (p *Pod) ExecArgs(req sandbox.ExecRequest, tag string) ([]string, error) {
	flags, argv, err := granule.ExecCommand(req, p.Workdir, tag)
	if err != nil {
		return nil, err
	}
	args := []string{"exec", "--quiet", p.Pod, "-c", Container}
	if req.Stdin != nil {
		args = append(args, "-i")
	}
	args = append(append(args, "--", Granule, "exec"), flags...)
	return append(append(args, "--"), argv...), nil
}

func (p *Pod) guest() granule.Guest { return granule.Guest{Exec: p.Exec, Workdir: p.Workdir} }

// Exec runs req.Argv in the guest through kubectl exec and granule's
// in-guest agent. When ctx is done first, the command is killed in the
// guest and Exec returns what it printed with ctx's error.
func (p *Pod) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if len(req.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("exec: empty command")
	}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	args, err := p.ExecArgs(req, tag)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	cliCtx, cancelCLI := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCLI()
	cmd := p.k.Command(cliCtx, args...)
	if req.Stdin != nil {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	stdout, stderr := headtail.New(OutputLimit), headtail.New(OutputLimit)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return sandbox.ExecResult{}, fmt.Errorf("kubectl was not found: %w", err)
		}
		return sandbox.ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr, ctxErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		ctxErr = ctx.Err()
		killCtx, cancel := context.WithTimeout(context.Background(), killGrace+5*time.Second)
		kill := append([]string{"exec", "--quiet", p.Pod, "-c", Container, "--", Granule, "exec", "--"}, granule.KillCommand(tag)...)
		_, _ = p.k.Output(killCtx, "", nil, kill...)
		cancel()
		select {
		case runErr = <-done:
		case <-time.After(killGrace):
			cancelCLI()
			runErr = <-done
		}
	}
	res := sandbox.ExecResult{
		Stdout:  stdout.Bytes(),
		Stderr:  stderr.Bytes(),
		Dropped: stdout.Dropped() + stderr.Dropped(),
	}
	if ctxErr != nil {
		res.ExitCode = -1
		return res, ctxErr
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return res, runErr
		}
		// kubectl exits with granule exec's status, which is the guest
		// command's; kubectl's and granule's own failures exit 1 with
		// their message on stderr, which the caller sees as it would the
		// command's own.
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}

// ReadFile returns at most limit bytes of the file at path; a relative
// path is relative to the sandbox's workdir.
func (p *Pod) ReadFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	return p.guest().ReadFile(ctx, path, limit)
}

// WriteFile writes data to path, creating its parent directories, and
// sets its mode.
func (p *Pod) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	return p.guest().WriteFile(ctx, path, data, mode)
}

// DeliverFile writes a credential file atomically, mode 0400, through
// kubectl exec's stdin, so that its value is on no command line
// (granule.Guest.DeliverFile).
func (p *Pod) DeliverFile(ctx context.Context, path string, data []byte) error {
	return p.guest().DeliverFile(ctx, path, data)
}

// Destroy deletes the pod; one already gone is not an error.
func (p *Pod) Destroy(ctx context.Context) error {
	_, err := p.k.Output(ctx, "deleting pod "+p.Pod+" in "+p.k.Where(), nil, "delete", "pod", p.Pod, "--ignore-not-found", "--wait=false")
	return err
}

// CheckDevices is --dry-run's check that some node in t offers the
// device resources a sandbox with network open (or not) needs, without
// creating anything. It returns which nodes do.
func (b *Backend) CheckDevices(ctx context.Context, t sandbox.K8sTarget, open bool) (string, error) {
	k := b.kubectl(t)
	out, err := k.Output(ctx, "listing nodes in "+k.Where(), nil, "get", "nodes", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("%w (reading nodes needs a ClusterRole that may list them)", err)
	}
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	want := []string{KVMResource}
	if open {
		want = append(want, TunResource)
	}
	var nodes []string
	for _, n := range l.Items {
		ok := true
		for _, r := range want {
			if q := n.Status.Allocatable[r]; q == "" || q == "0" {
				ok = false
			}
		}
		if ok {
			nodes = append(nodes, n.Metadata.Name)
		}
	}
	if len(nodes) == 0 {
		return "", fmt.Errorf("no node in %s offers %s: granule sandboxes need nodes with nested virtualisation and granule's KVM device plugin (gke-labs/granule deploy/k8s/kvm-device-plugin.yaml)", k.Where(), strings.Join(want, " and "))
	}
	return fmt.Sprintf("%s offered by %s", strings.Join(want, ", "), strings.Join(nodes, ", ")), nil
}
