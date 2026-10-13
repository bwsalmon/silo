//go:build linux

package granule

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// dockerRun is a recorded `docker run`, in the terms the check compares.
type dockerRun struct {
	devices, capDrop, capAdd, securityOpts, volumes, env []string
	image                                                string
	args                                                 []string
}

func readDockerRun(t *testing.T, name string) dockerRun {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(argv) < 2 || argv[0] != "run" || argv[1] != "-d" {
		t.Fatalf("%s: not a docker run -d: %q", name, argv)
	}
	var r dockerRun
	i := 2
	for ; i < len(argv) && strings.HasPrefix(argv[i], "-"); i += 2 {
		if i+1 >= len(argv) {
			t.Fatalf("%s: %s has no value", name, argv[i])
		}
		v := argv[i+1]
		switch argv[i] {
		case "--name", "--label":
			// granule's own names for its container; silod's are
			// containerd's id and labels.
		case "--device":
			r.devices = append(r.devices, v)
		case "--cap-drop":
			r.capDrop = append(r.capDrop, v)
		case "--cap-add":
			r.capAdd = append(r.capAdd, "CAP_"+v)
		case "--security-opt":
			r.securityOpts = append(r.securityOpts, v)
		case "-v":
			r.volumes = append(r.volumes, v)
		case "-e":
			r.env = append(r.env, v)
		default:
			t.Fatalf("%s: granule passes %s to docker run, and the driver doesn't know what it means", name, argv[i])
		}
	}
	if i >= len(argv) {
		t.Fatalf("%s: no image", name)
	}
	r.image, r.args = argv[i], argv[i+1:]
	return r
}

// TestVMCreateMatchesGranule checks the driver's VM container against
// what `granule vm create`, at the pinned commit, passes to docker run
// for the same VM (testdata/record.sh): the same devices, capabilities,
// security options, mounts and environment, and the image's entrypoint
// in run mode.
func TestVMCreateMatchesGranule(t *testing.T) {
	cases := []struct {
		golden  string
		spec    runtime.Spec
		gateway string
	}{
		{
			golden: "vmcreate-basic.args",
			spec: runtime.Spec{
				ID: "c1",
				Process: runtime.Process{
					Args: []string{"/bin/sh", "-c", "echo hi"}, Env: []string{"PATH=/usr/bin:/bin", "A=b"},
					User: "1000:1000", Workdir: "/srv",
				},
				Resources: runtime.Resources{VCPUm: 1000, MemoryMB: 512, DiskGB: 10},
			},
			gateway: "10.88.0.1",
		},
		{
			golden: "vmcreate-tty-volumes.args",
			spec: runtime.Spec{
				ID:        "c2",
				Process:   runtime.Process{Args: []string{"bash"}, Env: []string{"TERM=xterm"}, TTY: true, Stdin: true},
				Resources: runtime.Resources{VCPUm: 2500, MemoryMB: 2048, DiskGB: 20},
				Mounts: []runtime.Mount{
					{Kind: runtime.MountVolume, Source: "/vol/a", Target: "/data"},
					{Kind: runtime.MountVolume, Source: "/vol/b", Target: "/etc/app", ReadOnly: true},
				},
			},
			gateway: "10.88.4.1",
		},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			want := readDockerRun(t, c.golden)
			c.spec.UserNS, c.spec.NetNS = "/proc/1/ns/user", "/proc/1/ns/net"
			c.spec.Root = runtime.IDRange{Outside: 100000, Size: 65536}
			v, err := NewVM(c.spec, "/wraps/sha256-0123", "/io/"+c.spec.ID, c.gateway)
			if err != nil {
				t.Fatal(err)
			}
			opts, err := SpecOpts(v, c.spec, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := namespaces.WithNamespace(context.Background(), "silo")
			s, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: c.spec.ID}, opts...)
			if err != nil {
				t.Fatal(err)
			}

			// Devices: bind mounts, the rootless runtime's way of
			// giving them (rootless.VMPatch).
			if !slices.Equal(want.devices, Devices) || !slices.Equal(Devices, rootless.VMDevices) {
				t.Errorf("devices: granule asks for %q, the driver gives %q (rootless: %q)", want.devices, Devices, rootless.VMDevices)
			}
			mounts := map[string]string{}
			for _, m := range s.Mounts {
				mounts[m.Destination] = m.Source + ":" + strings.Join(m.Options, ",")
			}
			for _, d := range want.devices {
				if !strings.HasPrefix(mounts[d], d+":") {
					t.Errorf("device %s isn't bind-mounted: %q", d, mounts[d])
				}
			}
			for _, vol := range want.volumes {
				parts := strings.Split(vol, ":")
				got, ok := mounts[parts[1]]
				ro := len(parts) > 2 && parts[2] == "ro"
				if !ok || !strings.HasPrefix(got, parts[0]+":") || strings.Contains(got, ",ro") != ro {
					t.Errorf("-v %s: the spec mounts %q", vol, got)
				}
			}
			if !slices.Equal(want.capDrop, []string{"ALL"}) {
				t.Errorf("granule drops %q, not ALL", want.capDrop)
			}
			caps := s.Process.Capabilities
			for _, set := range [][]string{caps.Bounding, caps.Effective, caps.Permitted} {
				if !slices.Equal(set, want.capAdd) {
					t.Errorf("capabilities %q, granule's %q", set, want.capAdd)
				}
			}
			if len(caps.Inheritable) != 0 || len(caps.Ambient) != 0 {
				t.Errorf("inheritable %q, ambient %q", caps.Inheritable, caps.Ambient)
			}
			if !slices.Equal(want.securityOpts, []string{"no-new-privileges"}) || !s.Process.NoNewPrivileges {
				t.Errorf("security options %q, no_new_privileges %v", want.securityOpts, s.Process.NoNewPrivileges)
			}

			var env []string
			for _, e := range s.Process.Env {
				if !strings.HasPrefix(e, "PATH=") {
					env = append(env, e)
				}
			}
			slices.Sort(env)
			slices.Sort(want.env)
			if !slices.Equal(env, want.env) {
				for _, e := range want.env {
					if !slices.Contains(env, e) {
						t.Errorf("granule sets %s, the driver doesn't", e)
					}
				}
				for _, e := range env {
					if !slices.Contains(want.env, e) {
						t.Errorf("the driver sets %s, granule doesn't", e)
					}
				}
			}
			if !slices.Equal(want.args, []string{RunArg}) || !slices.Equal(s.Process.Args, []string{Entrypoint, RunArg}) {
				t.Errorf("args: granule runs the image with %q, the driver runs %q", want.args, s.Process.Args)
			}

			// The driver's own additions: the namespaces silod made.
			var nss []string
			for _, n := range s.Linux.Namespaces {
				if n.Path != "" {
					nss = append(nss, string(n.Type)+"="+n.Path)
				}
			}
			if !slices.Contains(nss, "user=/proc/1/ns/user") || !slices.Contains(nss, "network=/proc/1/ns/net") {
				t.Errorf("namespaces %q", nss)
			}
			if s.Linux.UIDMappings != nil {
				t.Errorf("a joined user namespace has mappings: %v", s.Linux.UIDMappings)
			}
			if got := s.Linux.Resources.Memory.Limit; got == nil || *got != (v.MemoryMB+VMMOverheadMB)<<20 {
				t.Errorf("memory limit %v", got)
			}
		})
	}
}

func TestNewVMRefuses(t *testing.T) {
	base := runtime.Spec{ID: "c", Process: runtime.Process{Args: []string{"true"}}}
	if _, err := NewVM(base, "", "/io", "10.0.0.1"); err == nil {
		t.Error("a VM without a wrap")
	}
	tmpfs := base
	tmpfs.Mounts = []runtime.Mount{{Kind: runtime.MountTmpfs, Target: "/tmp"}}
	if _, err := NewVM(tmpfs, "/w", "/io", "10.0.0.1"); err == nil {
		t.Error("a tmpfs in the vm class")
	}
	small := base
	small.Resources.MemoryMB = 64
	if _, err := NewVM(small, "/w", "/io", "10.0.0.1"); err == nil {
		t.Error("64 MiB")
	}
	v, err := NewVM(base, "/w", "/io", "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if v.CPUs != 1 || v.MemoryMB != defaultMemoryMB || v.DiskSizeMB != 0 {
		t.Errorf("defaults: %+v", v)
	}
	env, _ := v.Env()
	for _, e := range env {
		if strings.HasPrefix(e, "CHV_DISK_SIZE_MB=") {
			t.Errorf("no disk charge, but %s", e)
		}
	}
}
