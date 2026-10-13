package rootless

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// Namespace is an OCI spec's linux.namespaces entry.
type Namespace struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// Mount is an OCI spec's mounts entry.
type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Patch is what a class changes in a container's OCI spec to run
// rootless. Apply edits a config.json with it, leaving every field it
// doesn't name as it was, so that it composes with whatever built the
// rest of the spec.
type Patch struct {
	// Namespaces replace the spec's entries of the same types.
	Namespaces []Namespace
	// Drop removes namespace types from the spec.
	Drop []string
	// NoIDMappings removes linux.uidMappings and gidMappings: the
	// runtime is started inside the user namespace already, or joins
	// one that is already mapped.
	NoIDMappings bool
	Annotations  map[string]string
	// Mounts are added, replacing any at the same destination.
	Mounts []Mount
	// AdditionalGids are added to process.user.additionalGids.
	AdditionalGids []uint32
	// CgroupsPath is linux.cgroupsPath, in the systemd driver's
	// "slice:prefix:name" form (CgroupsPath).
	CgroupsPath string
}

// Apply returns config, an OCI runtime spec, with p applied.
func (p Patch) Apply(config []byte) ([]byte, error) {
	var spec map[string]any
	if err := json.Unmarshal(config, &spec); err != nil {
		return nil, fmt.Errorf("rootless: spec: %w", err)
	}
	obj := func(m map[string]any, key string) map[string]any {
		if v, ok := m[key].(map[string]any); ok {
			return v
		}
		v := map[string]any{}
		m[key] = v
		return v
	}
	linux := obj(spec, "linux")
	var nss []any
	if v, ok := linux["namespaces"].([]any); ok {
		for _, n := range v {
			typ, _ := n.(map[string]any)["type"].(string)
			if slices.Contains(p.Drop, typ) || slices.ContainsFunc(p.Namespaces, func(ns Namespace) bool { return ns.Type == typ }) {
				continue
			}
			nss = append(nss, n)
		}
	}
	for _, ns := range p.Namespaces {
		nss = append(nss, ns)
	}
	linux["namespaces"] = nss
	if p.NoIDMappings {
		delete(linux, "uidMappings")
		delete(linux, "gidMappings")
	}
	if p.CgroupsPath != "" {
		linux["cgroupsPath"] = p.CgroupsPath
	}
	if len(p.Annotations) > 0 {
		a := obj(spec, "annotations")
		for k, v := range p.Annotations {
			a[k] = v
		}
	}
	if len(p.Mounts) > 0 {
		var ms []any
		if v, ok := spec["mounts"].([]any); ok {
			for _, m := range v {
				dst, _ := m.(map[string]any)["destination"].(string)
				if !slices.ContainsFunc(p.Mounts, func(pm Mount) bool { return pm.Destination == dst }) {
					ms = append(ms, m)
				}
			}
		}
		for _, m := range p.Mounts {
			ms = append(ms, m)
		}
		spec["mounts"] = ms
	}
	if len(p.AdditionalGids) > 0 {
		user := obj(obj(spec, "process"), "user")
		gids, _ := user["additionalGids"].([]any)
		for _, g := range p.AdditionalGids {
			gids = append(gids, g)
		}
		user["additionalGids"] = gids
	}
	return json.MarshalIndent(spec, "", "\t")
}

// AnnotationUserNS names the pinned user namespace a sandbox-class
// container's runsc is started in. runsc can't join a user namespace
// named in linux.namespaces (its gofer calls setns from a
// multi-threaded process), so the namespace travels as an annotation
// and `silod runsc` enters it before runsc starts.
const AnnotationUserNS = "dev.silo.userns"

// AnnotationRoot is the container root's uid and gid in the holder's
// namespace (its range's first id). runsc runs as that root and writes
// its log and pid file in the bundle, which containerd made as the
// holder's root, so `silod runsc` gives the bundle directory to it.
const AnnotationRoot = "dev.silo.root"

// SandboxPatch is the sandbox class's change to a spec (call L30): runsc
// is started inside ns.User, so the spec names no user namespace and no
// mappings, only the netns silo-nsd made inside it, which netstack
// scrapes for the veth's address, MAC, MTU and routes.
func SandboxPatch(ns nsd.ContainerNS, cgroupsPath string) Patch {
	return Patch{
		Namespaces:   []Namespace{{Type: "network", Path: ns.Net}},
		Drop:         []string{"user"},
		NoIDMappings: true,
		Annotations:  map[string]string{AnnotationUserNS: ns.User, AnnotationRoot: strconv.FormatUint(uint64(ns.Range.Outside), 10)},
		CgroupsPath:  cgroupsPath,
	}
}

// AnnotationKeepGroups is crun's annotation that keeps the caller's
// supplementary groups across the workload's setgroups (what Podman's
// --group-add keep-groups sets).
const AnnotationKeepGroups = "run.oci.keep_original_groups"

// VMDevices are bind-mounted into a vm-class container: a rootless
// runtime can't mknod or use a device cgroup. /dev/kvm is reached
// through the kvm group; /dev/net/tun is 0666.
var VMDevices = []string{"/dev/kvm", "/dev/net/tun"}

// VMPatch is the vm class's change to a spec (call L31): crun joins the
// container's user namespace and netns by path, keeps silod's
// supplementary groups (kvm among them), and gets the devices as bind
// mounts. With kvmGID set, the fallback is used instead: the kvm group
// is mapped into the holder's namespace (an /etc/subgid line, a root
// step) and kvmGID, its id inside the container's namespace, is added
// to the process's groups.
func VMPatch(ns nsd.ContainerNS, cgroupsPath string, kvmGID *uint32) Patch {
	p := Patch{
		Namespaces: []Namespace{{Type: "user", Path: ns.User}, {Type: "network", Path: ns.Net}},
		// The namespace is mapped already; crun refuses mappings for
		// a joined user namespace.
		NoIDMappings: true,
		CgroupsPath:  cgroupsPath,
	}
	if kvmGID != nil {
		p.AdditionalGids = []uint32{*kvmGID}
	} else {
		p.Annotations = map[string]string{AnnotationKeepGroups: "1"}
	}
	for _, d := range VMDevices {
		p.Mounts = append(p.Mounts, Mount{Destination: d, Type: "bind", Source: d, Options: []string{"bind", "rw", "nosuid", "noexec"}})
	}
	return p
}
