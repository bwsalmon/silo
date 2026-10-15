package k8s

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/v2/sandbox"
)

var update = flag.Bool("update", false, "rewrite the golden pod specs")

func spec(network string) sandbox.SandboxSpec {
	return sandbox.SandboxSpec{
		Session: "s-abcdefghijkl", Name: "dev", Kind: "granule",
		Image: "ghcr.io/acme/dev-guest:2026-10", Workdir: "/work",
		CPUs: 1.5, Memory: 2 << 30, Disk: 20 << 30, Network: network,
		Env: map[string]string{"SECRET": "never in the manifest"},
		K8s: &sandbox.K8sTarget{Context: "kind-x", Namespace: "silo", KVMGroup: 994},
	}
}

func TestPodSpecGolden(t *testing.T) {
	for _, tc := range []struct {
		golden string
		spec   sandbox.SandboxSpec
		nodes  map[string]string
	}{
		{"pod-none.json", spec("none"), nil},
		{"pod-open.json", spec("open"), map[string]string{"cloud.google.com/gke-nodepool": "kvm"}},
		{"pod-defaults.json", sandbox.SandboxSpec{Session: "s-abcdefghijkl", Name: "bare", Kind: "granule", Image: "guest:1", Network: "none"}, nil},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := PodSpec(tc.spec, "debian@host", tc.nodes)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("%s differs (go test -update rewrites it):\n%s", path, got)
			}
			if strings.Contains(string(got), "never in the manifest") {
				t.Errorf("the sandbox's env is in the pod manifest")
			}
		})
	}
}

type pod struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		HostNetwork     bool           `json:"hostNetwork"`
		HostPID         bool           `json:"hostPID"`
		HostIPC         bool           `json:"hostIPC"`
		SecurityContext map[string]any `json:"securityContext"`
		Volumes         []map[string]any
		Containers      []struct {
			SecurityContext map[string]any `json:"securityContext"`
			Resources       struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
			Args    []string `json:"args"`
			Command []string `json:"command"`
		} `json:"containers"`
	} `json:"spec"`
}

func decode(t *testing.T, s sandbox.SandboxSpec) pod {
	t.Helper()
	b, err := PodSpec(s, "o", nil)
	if err != nil {
		t.Fatal(err)
	}
	var p pod
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// restricted lists what in p the restricted Pod Security Standard
// (https://kubernetes.io/docs/concepts/security/pod-security-standards/)
// refuses, for the fields a sandbox pod sets.
func restricted(p pod) []string {
	var bad []string
	if p.Spec.HostNetwork || p.Spec.HostPID || p.Spec.HostIPC {
		bad = append(bad, "host namespaces")
	}
	for _, v := range p.Spec.Volumes {
		for k := range v {
			switch k {
			case "name", "emptyDir", "configMap", "secret", "projected", "downwardAPI", "persistentVolumeClaim", "ephemeral", "csi":
			default:
				bad = append(bad, "volume type "+k)
			}
		}
	}
	podSC := p.Spec.SecurityContext
	for _, c := range p.Spec.Containers {
		sc := c.SecurityContext
		get := func(k string) any {
			if v, ok := sc[k]; ok {
				return v
			}
			return podSC[k]
		}
		if sc["privileged"] == true {
			bad = append(bad, "privileged")
		}
		if sc["allowPrivilegeEscalation"] != false {
			bad = append(bad, "allowPrivilegeEscalation")
		}
		if get("runAsNonRoot") != true {
			bad = append(bad, "runAsNonRoot")
		}
		if u, ok := get("runAsUser").(float64); ok && u == 0 {
			bad = append(bad, "runAsUser 0")
		}
		if sp, _ := get("seccompProfile").(map[string]any); sp == nil || (sp["type"] != "RuntimeDefault" && sp["type"] != "Localhost") {
			bad = append(bad, "seccompProfile")
		}
		caps, _ := sc["capabilities"].(map[string]any)
		if drop, _ := caps["drop"].([]any); !reflect.DeepEqual(drop, []any{"ALL"}) {
			bad = append(bad, "capabilities.drop")
		}
		if add, _ := caps["add"].([]any); len(add) > 0 && !reflect.DeepEqual(add, []any{"NET_BIND_SERVICE"}) {
			bad = append(bad, "capabilities.add")
		}
	}
	return bad
}

func TestNoNetworkPassesRestricted(t *testing.T) {
	p := decode(t, spec("none"))
	if bad := restricted(p); len(bad) > 0 {
		t.Errorf("a guest with no network breaks restricted: %v", bad)
	}
	c := p.Spec.Containers[0]
	if _, ok := c.Resources.Limits[TunResource]; ok {
		t.Errorf("a guest with no network asks for %s", TunResource)
	}
	if c.Resources.Limits[KVMResource] != "1" {
		t.Errorf("limits = %v, want %s", c.Resources.Limits, KVMResource)
	}
	if c.Command != nil || !reflect.DeepEqual(c.Args, []string{"run"}) {
		t.Errorf("command %v args %v: the image's entrypoint has to boot the VM", c.Command, c.Args)
	}
	if p.Metadata.Labels[RoleLabel] != Role || p.Metadata.Labels["silo.dev/session"] != "s-abcdefghijkl" {
		t.Errorf("labels = %v", p.Metadata.Labels)
	}
}

func TestOpenNetworkAsksForTun(t *testing.T) {
	p := decode(t, spec("open"))
	c := p.Spec.Containers[0]
	if c.Resources.Limits[TunResource] != "1" || c.Resources.Limits[KVMResource] != "1" {
		t.Errorf("limits = %v, want %s and %s", c.Resources.Limits, KVMResource, TunResource)
	}
	caps := c.SecurityContext["capabilities"].(map[string]any)
	if !reflect.DeepEqual(caps["add"], []any{"NET_ADMIN"}) {
		t.Errorf("capabilities = %v, want NET_ADMIN alone", caps)
	}
	if c.SecurityContext["privileged"] != nil {
		t.Errorf("a networked guest's pod is privileged")
	}
	// NET_ADMIN is beyond baseline, so restricted refuses it: that is
	// what docs say, and what this checks the checker against.
	if bad := restricted(p); len(bad) == 0 {
		t.Errorf("a NET_ADMIN pod passed the restricted check")
	}
}

func TestPodSpecRefusesNetwork(t *testing.T) {
	s := spec("allow")
	if _, err := PodSpec(s, "o", nil); err == nil {
		t.Errorf("network allow accepted")
	}
}

func TestPodName(t *testing.T) {
	a, b := PodName("s-abcdefghijkl", "dev"), PodName("s-abcdefghijkl", "Dev Box")
	if a == b || !strings.HasPrefix(a, "silo-s-abcdefghijkl-") || len(a) > 63 {
		t.Errorf("PodName = %q, %q", a, b)
	}
}

func TestExecArgs(t *testing.T) {
	p := &Pod{Pod: "silo-s-x-abc", Workdir: "/work"}
	args, err := p.ExecArgs(sandbox.ExecRequest{Argv: []string{"ls"}, Stdin: []byte{}, Env: map[string]string{"A": "1"}}, "tag")
	if err != nil {
		t.Fatal(err)
	}
	head := []string{"exec", "--quiet", "silo-s-x-abc", "-c", Container, "-i", "--", Granule, "exec", "-w", "/work", "--", "/bin/sh", "-c"}
	if !reflect.DeepEqual(args[:len(head)], head) {
		t.Errorf("args = %q", args)
	}
	if tail := args[len(head)+1:]; !reflect.DeepEqual(tail, []string{"tag", "env", "A=1", "ls"}) {
		t.Errorf("args tail = %q", tail)
	}
	args, _ = p.ExecArgs(sandbox.ExecRequest{Argv: []string{"ls"}}, "tag")
	for _, a := range args {
		if a == "-i" {
			t.Errorf("-i without stdin: %q", args)
		}
	}
}

func TestTargetsOf(t *testing.T) {
	a := sandbox.K8sTarget{Context: "a"}
	got := TargetsOf([]sandbox.SandboxSpec{{K8s: &a}, {}, {K8s: &sandbox.K8sTarget{Context: "a"}}, {K8s: &sandbox.K8sTarget{Namespace: "n"}}})
	if want := []sandbox.K8sTarget{a, {Namespace: "n"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("TargetsOf = %v, want %v", got, want)
	}
}
