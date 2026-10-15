package k8s_test

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/internal/granuletest"
	k8sloc "github.com/bwsalmon/silo/v2/location/k8s"
	"github.com/bwsalmon/silo/v2/sandbox"
	"github.com/bwsalmon/silo/v2/sandbox/k8s"
)

// RequireEnv makes a machine that cannot run granule pods on a kind
// cluster -- Docker, kind, kubectl and a /dev/kvm this user may open --
// a failure rather than a skip. Without it the test is skipped: it
// loads a guest image of several gigabytes into the cluster.
// grain CI sets it.
const RequireEnv = "SILO_V2_REQUIRE_K8S_KVM"

// The images as the kind node holds them, loaded rather than pulled.
const (
	guestImage  = "silo-test/granule-guest:k8s"
	pluginImage = "silo-test/generic-device-plugin:k8s"
	// pluginSource is the device plugin granule's manifest pins.
	pluginSource = "ghcr.io/squat/generic-device-plugin@sha256:dc192e164c69b03f156765793a1be62ca437709ae477b27ca7d8f3dcf5021576"
	namespace    = "silo-sandboxes"
)

type cluster struct {
	name, kubeconfig, dir string
	created               bool
	// kvmGroup owns /dev/kvm on the host, and so on the node.
	kvmGroup int64
}

var (
	fixOnce sync.Once
	fix     *cluster
	fixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix != nil {
		if fix.created {
			exec.Command("kind", "delete", "cluster", "--name", fix.name).Run()
		}
		os.RemoveAll(fix.dir)
	}
	os.Exit(code)
}

func run(stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

// kindKVM returns a kind cluster whose node has the host's /dev/kvm
// and granule's KVM device plugin, and a namespace that enforces the
// restricted Pod Security Standard.
func kindKVM(t *testing.T) *cluster {
	t.Helper()
	var missing error
	if os.Getenv(RequireEnv) == "" {
		t.Skipf("granule pods on kind are not run here: set %s=1 on a machine with Docker, kind, kubectl and /dev/kvm (grain CI does)", RequireEnv)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		missing = fmt.Errorf("docker is not working: %v", err)
	}
	for _, b := range []string{"kind", "kubectl"} {
		if _, err := exec.LookPath(b); err != nil && missing == nil {
			missing = err
		}
	}
	if kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil && missing == nil {
		missing = err
	} else if err == nil {
		kvm.Close()
	}
	if missing != nil {
		t.Fatalf("%s is set, but granule pods cannot run on kind here: %v", RequireEnv, missing)
	}
	fixOnce.Do(func() { fix, fixErr = startCluster() })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return fix
}

func startCluster() (*cluster, error) {
	dir, err := os.MkdirTemp("", "silo-kind-kvm-")
	if err != nil {
		return nil, err
	}
	c := &cluster{dir: dir, kubeconfig: filepath.Join(dir, "kubeconfig"), name: os.Getenv("SILO_V2_KIND_KVM_CLUSTER")}
	fi, err := os.Stat("/dev/kvm")
	if err != nil {
		return c, err
	}
	c.kvmGroup = int64(fi.Sys().(*syscall.Stat_t).Gid)
	source := os.Getenv(granuletest.ImageEnv)
	if source == "" {
		source = granuletest.DefaultImage
	}
	for _, img := range [][2]string{{source, guestImage}, {pluginSource, pluginImage}} {
		if _, err := run(nil, "docker", "image", "inspect", img[0]); err != nil {
			if _, err := run(nil, "docker", "pull", "-q", img[0]); err != nil {
				return c, err
			}
		}
		// Built rather than tagged: kind load imports every platform of
		// a multi-platform image, which the daemon holds only one of.
		if _, err := run([]byte("FROM "+img[0]+"\n"), "docker", "build", "-q", "-t", img[1], "-"); err != nil {
			return c, err
		}
	}
	if c.name != "" {
		if _, err := run(nil, "kind", "export", "kubeconfig", "--name", c.name, "--kubeconfig", c.kubeconfig); err != nil {
			return c, err
		}
	} else {
		c.name = fmt.Sprintf("silo-v2-kvm-%d", os.Getpid())
		c.created = true
		config := filepath.Join(dir, "kind.yaml")
		os.WriteFile(config, []byte(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/kvm
        containerPath: /dev/kvm
`), 0o644)
		if _, err := run(nil, "kind", "create", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig, "--config", config, "--wait", "180s"); err != nil {
			return c, err
		}
	}
	os.Setenv("KUBECONFIG", c.kubeconfig)
	// The node's udev gives /dev/kvm the node's kvm group, which, the
	// device being the host's, takes it from this machine's kvm group
	// (and the granule tests that run after this one). Give it back.
	if _, err := run(nil, "docker", "exec", c.name+"-control-plane", "chgrp", fmt.Sprint(c.kvmGroup), "/dev/kvm"); err != nil {
		return c, err
	}
	for _, img := range []string{pluginImage, guestImage} {
		if _, err := run(nil, "kind", "load", "docker-image", "--name", c.name, img); err != nil {
			return c, err
		}
	}
	plugin, err := os.ReadFile("testdata/kvm-device-plugin.yaml")
	if err != nil {
		return c, err
	}
	plugin = bytes.ReplaceAll(plugin, []byte("PLUGIN_IMAGE"), []byte(pluginImage))
	kc := "kind-" + c.name
	if _, err := run(plugin, "kubectl", "--context", kc, "apply", "-f", "-"); err != nil {
		return c, err
	}
	ns := fmt.Sprintf(`{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": %q, "labels": {"pod-security.kubernetes.io/enforce": "restricted"}}}`, namespace)
	if _, err := run([]byte(ns), "kubectl", "--context", kc, "apply", "-f", "-"); err != nil {
		return c, err
	}
	// The plugin registers with the kubelet before the node offers the
	// resource.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := run(nil, "kubectl", "--context", kc, "get", "nodes", "-o", `jsonpath={.items[*].status.allocatable.devic\.es/kvm}`)
		if strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
			return c, nil
		}
		if time.Now().After(deadline) {
			logs, _ := run(nil, "kubectl", "--context", kc, "-n", "kube-system", "logs", "-l", "app.kubernetes.io/name=granule-device-plugin", "--tail", "20")
			return c, fmt.Errorf("the kind node never offered %s:\n%s", k8s.KVMResource, logs)
		}
		time.Sleep(2 * time.Second)
	}
}

func (c *cluster) target() sandbox.K8sTarget {
	return sandbox.K8sTarget{Context: "kind-" + c.name, Namespace: namespace, KVMGroup: c.kvmGroup}
}

// TestGranulePod runs a granule sandbox as a pod in a namespace that
// enforces the restricted Pod Security Standard, execs into its guest,
// uses the file tools, and destroys it.
func TestGranulePod(t *testing.T) {
	c := kindKVM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	target := c.target()
	b := &k8s.Backend{Targets: []sandbox.K8sTarget{target}, ReadyTimeout: 5 * time.Minute}

	if detail, err := b.CheckDevices(ctx, target, false); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(detail, k8s.KVMResource) {
		t.Errorf("CheckDevices = %q", detail)
	}

	session := fmt.Sprintf("s-kindkvm%04d", os.Getpid()%10000)
	spec := sandbox.SandboxSpec{
		Session: session, Name: "dev", Kind: "granule", Image: guestImage,
		Workdir: "/work", CPUs: 1, Memory: 1 << 30, Network: "none",
		Env: map[string]string{"GREETING": "it's here"}, K8s: &target,
	}
	sb, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	pod := sb.(*k8s.Pod)
	t.Cleanup(func() { sb.Destroy(context.Background()) })
	if pod.Image.Digest == "" {
		t.Errorf("no image digest recorded")
	}

	res, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", `echo "$GREETING"; pwd; uname -s; cat; exit 3`}, Stdin: []byte("in\n")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res.Stdout), "it's here\n/work\nLinux\nin\n"; got != want || res.ExitCode != 3 {
		t.Errorf("exec: stdout %q, exit %d, stderr %q; want %q, exit 3", got, res.ExitCode, res.Stderr, want)
	}
	// The guest has no network device: only virtual ones (loopback,
	// and the guest image's own docker0), none with a device behind it.
	res, err = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "for d in /sys/class/net/*; do [ -e $d/device ] && echo ${d##*/}; done; true"}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "" {
		t.Errorf("guest network devices: %q %q %v; want none", res.Stdout, res.Stderr, err)
	}

	if err := sb.WriteFile(ctx, "sub/note.txt", []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.ReadFile(ctx, "/work/sub/note.txt", 3); err != nil || string(got) != "hel" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if err := sb.DeliverFile(ctx, "/run/creds/token", []byte("secret-1")); err != nil {
		t.Fatal(err)
	}
	if err := sb.DeliverFile(ctx, "/run/creds/token", []byte("secret-2")); err != nil {
		t.Fatal(err)
	}
	res, _ = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "stat -c %a /run/creds/token; cat /run/creds/token"}})
	if got := string(res.Stdout); got != "400\nsecret-2" {
		t.Errorf("delivered file: %q %q", got, res.Stderr)
	}
	var mode fs.FileMode = 0o640
	res, _ = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"stat", "-c", "%a", "sub/note.txt"}})
	if got := strings.TrimSpace(string(res.Stdout)); got != fmt.Sprintf("%o", mode) {
		t.Errorf("written file mode %s", got)
	}

	// A cancelled exec returns promptly and kills the command.
	cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
	start := time.Now()
	_, err = sb.Exec(cctx, sandbox.ExecRequest{Argv: []string{"sleep", "120"}})
	ccancel()
	if err == nil || time.Since(start) > 30*time.Second {
		t.Errorf("cancelled exec: %v after %s", err, time.Since(start))
	}
	res, _ = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "pgrep -x sleep || true"}})
	if strings.TrimSpace(string(res.Stdout)) != "" {
		t.Errorf("sleep still runs in the guest: %q", res.Stdout)
	}

	found, err := b.Find(ctx, session)
	if err != nil || len(found) != 1 || found[0].(*k8s.Pod).Name != "dev" || found[0].(*k8s.Pod).Workdir != "/work" {
		t.Errorf("Find = %v, %v", found, err)
	}
	sessions, err := b.Sessions(ctx)
	if err != nil || !contains(sessions, session) {
		t.Errorf("Sessions = %v, %v", sessions, err)
	}

	// kubectl logs is the guest console.
	k := &k8sloc.Kubectl{Context: target.Context, Namespace: target.Namespace}
	if logs, err := k.Output(ctx, "logs", nil, "logs", pod.Pod, "-c", k8s.Container); err != nil || logs == "" {
		t.Errorf("kubectl logs: %q %v", logs, err)
	}

	if err := found[0].Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := k.Output(ctx, "get", nil, "get", "pods", "-l", k8sloc.SessionLabel+"="+session, "-o", "name")
		if strings.TrimSpace(out) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod not gone: %s", out)
		}
		time.Sleep(2 * time.Second)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
