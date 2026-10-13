package granule

import (
	"context"
	"encoding/json"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// VMMOverheadMB is what the VM container's cgroup allows beyond the
// guest's memory: cloud-hypervisor, granule-shim and netshim. The
// guest's own memory is capped by CHV_MEMORY_MB, inside the VM.
const VMMOverheadMB = 256

// SpecOpts is the VM container's OCI spec: v's process, environment and
// mounts, the capabilities granule asks of Docker, the cgroup limits of
// spec's charges, and the vm class's rootless patch (call L31), which
// joins the container's user namespace and netns and bind-mounts
// Devices. kvmGID is rootless.VMPatch's: nil for crun's
// keep_original_groups.
func SpecOpts(v VM, spec runtime.Spec, cgroupsPath string, kvmGID *uint32) ([]oci.SpecOpts, error) {
	env, err := v.Env()
	if err != nil {
		return nil, err
	}
	opts := []oci.SpecOpts{
		oci.WithProcessArgs(v.Args()...),
		oci.WithEnv(env),
		oci.WithProcessCwd("/"),
		// Root inside the container's own user namespace, which is a
		// subuid range on the host: granule needs no more.
		oci.WithUIDGID(0, 0),
		oci.WithHostname(hostname(v.Name)),
		withCapabilities(Capabilities),
		oci.WithNoNewPrivileges,
	}
	r := spec.Resources
	opts = append(opts, oci.WithMemoryLimit(uint64(v.MemoryMB+VMMOverheadMB)<<20))
	if r.VCPUm > 0 {
		opts = append(opts, oci.WithCPUCFS(r.VCPUm*100, 100000))
	}
	if r.PIDs > 0 {
		opts = append(opts, oci.WithPidsLimit(r.PIDs))
	}
	var ms []specs.Mount
	for _, b := range v.Binds() {
		o := []string{"rbind", "rw"}
		if b.ReadOnly {
			o[1] = "ro"
		}
		ms = append(ms, specs.Mount{Destination: b.Target, Type: "bind", Source: b.Source, Options: o})
	}
	opts = append(opts, oci.WithMounts(ms))
	patch := rootless.VMPatch(nsd.ContainerNS{User: spec.UserNS, Net: spec.NetNS,
		Range: nsd.Range{Outside: spec.Root.Outside, Size: spec.Root.Size}}, cgroupsPath, kvmGID)
	return append(opts, withPatch(patch)), nil
}

func hostname(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// withCapabilities sets every capability set to caps: Docker's
// --cap-drop ALL --cap-add for each.
func withCapabilities(caps []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		c := func() []string { return append([]string{}, caps...) }
		s.Process.Capabilities = &specs.LinuxCapabilities{Bounding: c(), Effective: c(), Permitted: c()}
		return nil
	}
}

// withPatch applies a rootless.Patch to the spec being built.
func withPatch(p rootless.Patch) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if b, err = p.Apply(b); err != nil {
			return err
		}
		var out oci.Spec
		if err := json.Unmarshal(b, &out); err != nil {
			return err
		}
		*s = out
		return nil
	}
}
