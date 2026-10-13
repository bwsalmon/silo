package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
)

// strs is a JSON list of strings that older clients may send as one
// string (Entrypoint and Cmd before API 1.25).
type strs []string

func (s *strs) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*s = strings.Fields(one)
		return nil
	}
	var list []string
	err := json.Unmarshal(b, &list)
	*s = list
	return err
}

type createBody struct {
	Image           string
	Cmd             strs
	Entrypoint      strs
	Env             []string
	WorkingDir      string
	User            string
	StopSignal      string
	StopTimeout     *int
	Tty             bool
	OpenStdin       bool
	NetworkDisabled bool
	Labels          map[string]string
	ExposedPorts    map[string]struct{}
	Volumes         map[string]struct{}
	HostConfig      struct {
		Binds           []string
		NetworkMode     string
		PortBindings    map[string][]struct{ HostIp, HostPort string }
		PublishAllPorts bool
		Mounts          []struct {
			Type, Source, Target string
			ReadOnly             bool
			TmpfsOptions         *struct{ SizeBytes int64 }
		}
		Tmpfs      map[string]string
		Memory     int64
		NanoCpus   int64
		PidsLimit  *int64
		StorageOpt map[string]string
		Runtime    string
		AutoRemove bool
	}
	NetworkingConfig struct {
		EndpointsConfig map[string]endpointConfig
	}
}

// The canonical runtime names of the classes (local-backend.md, "How a
// tenant asks for a class through the Docker API").
const (
	RuntimeVM      = "silo-vm"
	RuntimeSandbox = "silo-sandbox"
)

func runtimeName(c runtime.Class) string {
	if c == runtime.VM {
		return RuntimeVM
	}
	return RuntimeSandbox
}

// classOf is the class HostConfig.Runtime names, or "" for the silo's
// default.
func classOf(name string) (runtime.Class, error) {
	switch name {
	case "", "runc", "default":
		return "", nil
	case RuntimeVM, "kata", "granule", "vm":
		return runtime.VM, nil
	case RuntimeSandbox, "runsc", "sandbox":
		return runtime.Sandbox, nil
	}
	return "", invalid("silo: unknown runtime %q; use %s or %s", name, RuntimeVM, RuntimeSandbox)
}

// refuseHost refuses, with 403, what a container create may ask for
// that is a fence or a policy and not a shape (local-backend.md,
// "Refused"): privilege, host paths, devices, host namespaces, kernel
// tuning, raw host ports and other logging drivers.
func refuseHost(body map[string]any) error {
	hc, _ := body["HostConfig"].(map[string]any)
	if hc == nil {
		return nil
	}
	set := func(k string) bool { v, ok := hc[k]; return ok && !isDefault(v) }
	str := func(k string) string { s, _ := hc[k].(string); return s }
	notAllowed := func(what string) error { return forbidden("silo: %s is not allowed in a silo", what) }
	switch {
	case set("Privileged"):
		return notAllowed("a privileged container")
	case set("CapAdd"):
		return notAllowed("adding capabilities (CapAdd)")
	case set("Devices"), set("DeviceRequests"), set("DeviceCgroupRules"):
		return notAllowed("access to host devices")
	case set("Sysctls"):
		return notAllowed("setting sysctls")
	case set("CpusetCpus"), set("CpusetMems"):
		return notAllowed("pinning to host CPUs or memory nodes")
	}
	if v, ok := hc["OomScoreAdj"].(float64); ok && v < 0 {
		return notAllowed("a negative OomScoreAdj")
	}
	for _, k := range []string{"NetworkMode", "PidMode", "IpcMode", "UTSMode", "UsernsMode", "CgroupnsMode"} {
		v := str(k)
		if v == "host" {
			return notAllowed(k + " host (the host's namespaces)")
		}
		if strings.HasPrefix(v, "container:") {
			return notAllowed(k + " " + v + " (another container's namespaces)")
		}
	}
	if so, ok := hc["SecurityOpt"].([]any); ok {
		for _, o := range so {
			s, _ := o.(string)
			if s != "no-new-privileges" && s != "no-new-privileges:true" && s != "no-new-privileges=true" {
				return notAllowed(fmt.Sprintf("security option %q", s))
			}
		}
	}
	if us, ok := hc["Ulimits"].([]any); ok {
		for _, u := range us {
			if h, _ := dig(u, "Hard").(float64); h > 1<<20 || h < 0 {
				return notAllowed(fmt.Sprintf("ulimit %v above the class's limit", dig(u, "Name")))
			}
		}
	}
	if binds, ok := hc["Binds"].([]any); ok {
		for _, b := range binds {
			s, _ := b.(string)
			if src, _, _ := strings.Cut(s, ":"); strings.ContainsAny(src[:min(1, len(src))], "/.~") {
				return forbidden("silo: bind mounts of host paths are not allowed; use a volume")
			}
		}
	}
	if ms, ok := hc["Mounts"].([]any); ok {
		for _, m := range ms {
			switch t, _ := dig(m, "Type").(string); t {
			case "bind":
				return forbidden("silo: bind mounts of host paths are not allowed; use a volume")
			case "volume", "tmpfs", "":
			default:
				return notAllowed(fmt.Sprintf("a mount of type %q", t))
			}
		}
	}
	if d := str("VolumeDriver"); d != "" && d != "local" {
		return notAllowed(fmt.Sprintf("volume driver %q", d))
	}
	if pb, ok := hc["PortBindings"].(map[string]any); ok {
		for _, bs := range pb {
			list, _ := bs.([]any)
			for _, b := range list {
				if ip, _ := dig(b, "HostIp").(string); ip != "" {
					return forbidden("silo: a published port has no host address (HostIp %s); it is published on %s", ip, Domain)
				}
			}
		}
	}
	if lt, _ := dig(hc["LogConfig"], "Type").(string); lt != "" && lt != "json-file" && lt != "local" {
		return notAllowed(fmt.Sprintf("logging driver %q", lt))
	}
	return nil
}

func isDefault(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// parseSignal reads a signal as Docker takes one: SIGTERM, TERM or 15.
func parseSignal(s string) (syscall.Signal, error) {
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	for n := syscall.Signal(1); n < 32; n++ {
		if strings.TrimPrefix(signalNames[n], "SIG") == name {
			return n, nil
		}
	}
	return 0, invalid("Invalid signal: %s", s)
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGILL: "SIGILL",
	syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE",
	syscall.SIGKILL: "SIGKILL", syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM", syscall.SIGCHLD: "SIGCHLD",
	syscall.SIGCONT: "SIGCONT", syscall.SIGSTOP: "SIGSTOP", syscall.SIGTSTP: "SIGTSTP", syscall.SIGTTIN: "SIGTTIN",
	syscall.SIGTTOU: "SIGTTOU", syscall.SIGURG: "SIGURG", syscall.SIGXCPU: "SIGXCPU", syscall.SIGXFSZ: "SIGXFSZ",
	syscall.SIGVTALRM: "SIGVTALRM", syscall.SIGPROF: "SIGPROF", syscall.SIGWINCH: "SIGWINCH", syscall.SIGIO: "SIGIO",
	syscall.SIGSYS: "SIGSYS",
}

// mergeEnv is base with over's variables replacing base's of the same
// name.
func mergeEnv(base, over []string) []string {
	out := slices.Clone(base)
	for _, e := range over {
		k, _, _ := strings.Cut(e, "=")
		i := slices.IndexFunc(out, func(o string) bool { ok, _, _ := strings.Cut(o, "="); return ok == k })
		if i >= 0 {
			out[i] = e
		} else {
			out = append(out, e)
		}
	}
	return out
}

// classFor is the class a create asks for, checked against the silo's
// effective isolation and the host: 403 for a class the silo isn't
// allowed, 409 for one the host can't run. It never falls back.
func (s *Server) classFor(ctx context.Context, q *request, asked runtime.Class) (runtime.Class, error) {
	var iso fenceapi.Isolation
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		iso, _, err = fenceapi.EffectiveIsolation(ctx, tx, q.chain)
		return err
	})
	if err != nil {
		return "", err
	}
	c := asked
	if c == "" {
		if iso.Default == "" {
			return "", forbidden("silo: no isolation class is allowed in this silo")
		}
		c = runtime.Class(iso.Default)
	}
	if !slices.Contains(iso.Allowed, string(c)) {
		return "", forbidden("silo: isolation class %q is not allowed in this silo (allowed: %s)", c, strings.Join(iso.Allowed, ", "))
	}
	d := s.drivers[c]
	if d == nil {
		return "", conflict("silo: this host cannot run isolation class %q (no driver for it)", c)
	}
	if p := d.Probe(ctx); !p.OK {
		return "", conflict("silo: this host cannot run isolation class %q (%s)", c, p.Why)
	}
	return c, nil
}

func (s *Server) containerCreate(q *request) error {
	b, err := decode[createBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	if eps, ok := dig(q.body["NetworkingConfig"], "EndpointsConfig").(map[string]any); ok {
		for name, ep := range eps {
			if err := checkEndpoint(name, ep); err != nil {
				return s.refuse(q, err)
			}
		}
	}
	name := strings.TrimPrefix(q.query("name"), "/")
	if name != "" && (!validName.MatchString(name) || len(name) < 2) {
		return s.refuse(q, invalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name))
	}
	asked, err := classOf(b.HostConfig.Runtime)
	if err != nil {
		return s.refuse(q, err)
	}
	class, err := s.classFor(q.ctx(), q, asked)
	if err != nil {
		return s.refuse(q, err)
	}
	if b.OpenStdin && class == runtime.VM {
		return s.refuse(q, invalid("silo: stdin (OpenStdin) is not available in the vm class yet"))
	}
	stop, err := parseSignal(b.StopSignal)
	if err != nil {
		return s.refuse(q, err)
	}
	for k := range b.HostConfig.StorageOpt {
		if k != "size" {
			return s.refuse(q, invalid("silo: storage option %q is not supported; only size is", k))
		}
	}
	c := &Container{ID: newID(), Name: name, Class: class, Image: b.Image, DiskGB: DefaultDiskGB,
		VCPUm: DefaultVCPUm, MemoryMB: DefaultMemoryMB, Agent: agentCall(q.ctx())}
	if c.Name == "" {
		c.Name = "silo_" + c.ID[:12]
	}
	if v := b.HostConfig.StorageOpt["size"]; v != "" {
		n, err := parseSize(v)
		if err != nil {
			return s.refuse(q, err)
		}
		c.DiskGB = gib(n)
	}
	if b.HostConfig.NanoCpus > 0 {
		c.VCPUm = (b.HostConfig.NanoCpus + 999_999) / 1_000_000
	}
	if b.HostConfig.Memory > 0 {
		c.MemoryMB = (b.HostConfig.Memory + 1<<20 - 1) >> 20
	}
	// The create's Config and HostConfig as inspect shows them.
	c.Config = map[string]any{}
	for k, v := range q.body {
		if k != "HostConfig" && k != "NetworkingConfig" {
			c.Config[k] = v
		}
	}
	c.HostConfig, _ = q.body["HostConfig"].(map[string]any)
	if c.HostConfig == nil {
		c.HostConfig = map[string]any{}
	}
	c.HostConfig["Runtime"] = runtimeName(class)
	mode := b.HostConfig.NetworkMode
	if b.NetworkDisabled {
		mode = "none"
	}
	if mode == "" || mode == "default" {
		mode = DefaultNetwork
	}
	c.HostConfig["NetworkMode"] = mode

	var made []*Volume
	var img *ImageRecord
	s.mu.Lock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		ctx := q.ctx()
		cs, err := s.containers(ctx, tx, q.target.ID)
		if err != nil {
			return err
		}
		if o, ok := find(cs, c.Name, func(*Container) string { return "" }, func(o *Container) string { return o.Name }); ok {
			return conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", c.Name, o.ID)
		}
		if img, err = s.image(ctx, tx, q.target.ID, b.Image); err != nil {
			return err
		}
		c.ImageID = img.ID
		if err := buildProcess(c, &b, img); err != nil {
			return err
		}
		var charges []fenceapi.ChargeLine
		// Networks: the mode's, then any other the endpoints name.
		if mode != "none" {
			var n *Network
			if mode == DefaultNetwork {
				n, err = s.defaultNetwork(ctx, tx, q, &charges)
			} else {
				n, err = s.network(ctx, tx, q.target.ID, mode)
			}
			if err != nil {
				return err
			}
			if err := s.attach(ctx, tx, q.target.ID, c, n, b.NetworkingConfig.EndpointsConfig[mode]); err != nil {
				return err
			}
			for _, other := range sortedKeys(b.NetworkingConfig.EndpointsConfig) {
				if other == mode || other == n.Name || other == n.ID {
					continue
				}
				n, err := s.network(ctx, tx, q.target.ID, other)
				if err != nil {
					return err
				}
				if err := s.attach(ctx, tx, q.target.ID, c, n, b.NetworkingConfig.EndpointsConfig[other]); err != nil {
					return err
				}
			}
		}
		if err := buildPorts(c, &b, img); err != nil {
			return err
		}
		// Mounts, making the volumes that don't exist yet.
		mounts, err := buildMounts(c, &b, img)
		if err != nil {
			return err
		}
		var anon []string
		for i := range mounts {
			m := &mounts[i]
			if m.Type != "volume" {
				continue
			}
			if m.Name == "" {
				m.Name = newID()
				anon = append(anon, m.Name)
			}
			v, err := row[Volume](ctx, tx, q.target.ID, kindVolume, m.Name)
			if err != nil {
				return err
			}
			if v != nil {
				continue
			}
			v = &Volume{Name: m.Name, Size: storage.DefaultVolumeSize, Anonymous: slices.Contains(anon, m.Name)}
			ch, err := s.makeVolume(ctx, tx, q, v)
			if err != nil {
				return err
			}
			charges = append(charges, ch...)
			made = append(made, v)
		}
		c.Mounts = mounts
		ch, err := s.charge(ctx, tx, q, Object(q.target.ID, kindContainer, c.ID), line{ClassContainers, 1}, line{ClassDiskGB, c.DiskGB})
		if err != nil {
			return err
		}
		charges = append(charges, ch...)
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		c.Created = now
		if c.Seq, err = objseq.Next(ctx, tx); err != nil {
			return err
		}
		if err := put(ctx, tx, q.target.ID, kindContainer, c.ID, c.Name, c); err != nil {
			return err
		}
		return s.auditTx(ctx, tx, q, "OK", map[string]any{"name": c.Name, "id": c.ID, "class": string(class), "image": c.ImageID}, charges)
	})
	s.mu.Unlock()
	if err != nil {
		return s.refuse(q, err)
	}
	if err := s.makeContainer(q.ctx(), q.target.ID, c, made, stop); err != nil {
		s.undoCreate(q, c, made)
		return err
	}
	writeJSON(q.w, http.StatusCreated, map[string]any{"Id": c.ID, "Warnings": []string{}})
	return nil
}

// checkEndpoint holds one EndpointsConfig entry to its allowlist.
func checkEndpoint(name string, ep any) error {
	m, _ := ep.(map[string]any)
	for k, v := range m {
		sub, ok := endpointFields[k]
		if !ok && !isDefault(v) {
			return invalid("silo: field NetworkingConfig.EndpointsConfig.%s.%s is not allowed", name, k)
		}
		if sm, isMap := v.(map[string]any); ok && sub != nil && isMap {
			for sk, sv := range sm {
				if _, ok := sub[sk]; !ok && !isDefault(sv) {
					return invalid("silo: field NetworkingConfig.EndpointsConfig.%s.%s.%s is not allowed", name, k, sk)
				}
			}
		}
	}
	return nil
}

// buildProcess is the container's process: the image's config,
// overridden by the create's as Docker overrides it.
func buildProcess(c *Container, b *createBody, img *ImageRecord) error {
	entry, cmd := img.Config.Entrypoint, img.Config.Cmd
	if b.Entrypoint != nil {
		entry, cmd = b.Entrypoint, nil
	}
	if b.Cmd != nil {
		cmd = b.Cmd
	}
	args := append(slices.Clone([]string(entry)), cmd...)
	if len(args) == 0 {
		return invalid("no command specified")
	}
	p := runtime.Process{Args: args, Env: mergeEnv(img.Config.Env, b.Env), User: img.Config.User,
		Workdir: img.Config.WorkingDir, TTY: b.Tty, Stdin: b.OpenStdin}
	if b.User != "" {
		p.User = b.User
	}
	if b.WorkingDir != "" {
		if !path.IsAbs(b.WorkingDir) {
			return invalid("the working directory '%s' is invalid, it needs to be an absolute path", b.WorkingDir)
		}
		p.Workdir = b.WorkingDir
	}
	c.Process = p
	return nil
}

// buildPorts records the create's published ports. A HostPort of "" or
// 0 is given one at start.
func buildPorts(c *Container, b *createBody, img *ImageRecord) error {
	seen := map[string]bool{}
	add := func(spec string, host string) error {
		port, proto, _ := strings.Cut(spec, "/")
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return invalid("silo: protocol %q is not supported for a published port", proto)
		}
		priv, err := strconv.Atoi(port)
		if err != nil || priv < 1 || priv > 65535 {
			return invalid("invalid port specification: %q", spec)
		}
		pub := 0
		if host != "" {
			if pub, err = strconv.Atoi(host); err != nil || pub < 0 || pub > 65535 {
				return invalid("silo: invalid host port %q (ranges are not supported)", host)
			}
		}
		if pub == Port || pub == 53 {
			return invalid("silo: port %d on %s is the facade's or DNS's, and can't be published", pub, Domain)
		}
		c.Ports = append(c.Ports, PortBinding{Proto: proto, Private: priv, Public: pub, Fixed: pub != 0})
		seen[strconv.Itoa(priv)+"/"+proto] = true
		return nil
	}
	for _, spec := range sortedKeys(b.HostConfig.PortBindings) {
		for _, pb := range b.HostConfig.PortBindings[spec] {
			if err := add(spec, pb.HostPort); err != nil {
				return err
			}
		}
	}
	if b.HostConfig.PublishAllPorts {
		exposed := map[string]struct{}{}
		for k := range img.Config.ExposedPorts {
			exposed[k] = struct{}{}
		}
		for k := range b.ExposedPorts {
			exposed[k] = struct{}{}
		}
		for _, spec := range sortedKeys(exposed) {
			k := spec
			if !strings.Contains(k, "/") {
				k += "/tcp"
			}
			if !seen[k] {
				if err := add(spec, ""); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// buildMounts are the create's mounts: named volumes from Binds and
// Mounts, tmpfs from Tmpfs and Mounts, and an anonymous volume for each
// of the image's and the create's Volumes that nothing else mounts,
// whose name is left empty for the caller to give.
func buildMounts(c *Container, b *createBody, img *ImageRecord) ([]MountPoint, error) {
	var out []MountPoint
	targets := map[string]bool{}
	add := func(m MountPoint) error {
		if !path.IsAbs(m.Target) || path.Clean(m.Target) != m.Target {
			return invalid("invalid mount config: mount path %q must be absolute and clean", m.Target)
		}
		if targets[m.Target] {
			return invalid("Duplicate mount point: %s", m.Target)
		}
		if m.Type == "volume" && m.Name != "" && !validName.MatchString(m.Name) {
			return invalid("silo: invalid volume name %q", m.Name)
		}
		targets[m.Target] = true
		out = append(out, m)
		return nil
	}
	for _, bind := range b.HostConfig.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, invalid("invalid volume specification: '%s'", bind)
		}
		m := MountPoint{Type: "volume", Name: parts[0], Target: parts[1]}
		if len(parts) == 3 {
			for _, o := range strings.Split(parts[2], ",") {
				switch o {
				case "ro":
					m.ReadOnly = true
				case "rw", "z", "Z", "nocopy":
				default:
					return nil, invalid("invalid mode: %s", o)
				}
			}
		}
		if err := add(m); err != nil {
			return nil, err
		}
	}
	for _, mt := range b.HostConfig.Mounts {
		switch mt.Type {
		case "volume", "":
			if err := add(MountPoint{Type: "volume", Name: mt.Source, Target: mt.Target, ReadOnly: mt.ReadOnly}); err != nil {
				return nil, err
			}
		case "tmpfs":
			size := c.MemoryMB
			if mt.TmpfsOptions != nil && mt.TmpfsOptions.SizeBytes > 0 {
				size = (mt.TmpfsOptions.SizeBytes + 1<<20 - 1) >> 20
			}
			if err := addTmpfs(add, c, mt.Target, size); err != nil {
				return nil, err
			}
		}
	}
	for _, target := range sortedKeys(b.HostConfig.Tmpfs) {
		size := c.MemoryMB
		for _, o := range strings.Split(b.HostConfig.Tmpfs[target], ",") {
			if v, ok := strings.CutPrefix(o, "size="); ok {
				n, err := parseSize(v)
				if err != nil {
					return nil, err
				}
				size = (n + 1<<20 - 1) >> 20
			}
		}
		if err := addTmpfs(add, c, target, size); err != nil {
			return nil, err
		}
	}
	anon := map[string]struct{}{}
	for k := range img.Config.Volumes {
		anon[k] = struct{}{}
	}
	for k := range b.Volumes {
		anon[k] = struct{}{}
	}
	for _, target := range sortedKeys(anon) {
		if targets[path.Clean(target)] {
			continue
		}
		if err := add(MountPoint{Type: "volume", Target: path.Clean(target)}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func addTmpfs(add func(MountPoint) error, c *Container, target string, sizeMB int64) error {
	if sizeMB > c.MemoryMB {
		return invalid("silo: tmpfs at %s (%d MiB) is larger than the container's memory (%d MiB)", target, sizeMB, c.MemoryMB)
	}
	return add(MountPoint{Type: "tmpfs", Target: target, SizeMB: sizeMB})
}

// makeContainer makes, outside the store's transaction, what the
// create kept: the volumes' directories, the class's image, the network
// namespace and the runtime's container.
func (s *Server) makeContainer(ctx context.Context, silo string, c *Container, made []*Volume, stop syscall.Signal) error {
	for _, v := range made {
		if err := s.createVolumeDir(ctx, silo, v); err != nil {
			return err
		}
	}
	d := s.drivers[c.Class]
	if err := d.Prepare(ctx, c.ImageID); err != nil {
		return err
	}
	netns := "/run/silo/netns/" + c.ID
	if s.opts.Wire != nil {
		var err error
		if netns, err = s.opts.Wire(ctx, c); err != nil {
			return err
		}
	}
	spec := runtime.Spec{ID: c.ID, Silo: silo, Image: c.ImageID, Process: c.Process,
		Resources: runtime.Resources{VCPUm: c.VCPUm, MemoryMB: c.MemoryMB, DiskGB: c.DiskGB},
		NetNS:     netns, StopSignal: stop,
		Labels: map[string]string{runtime.LabelSilo: silo, runtime.LabelName: c.Name}}
	if pl, ok := c.HostConfig["PidsLimit"].(float64); ok && pl > 0 {
		spec.Resources.PIDs = int64(pl)
	}
	if e := c.firstEndpoint(); e != nil {
		spec.Labels[runtime.LabelAddr] = e.Addr.String()
	}
	if c.Agent != "" {
		// An agent's tool container: in the sandbox class silo-tools is
		// silod's own file, mounted read-only; the vm class's tool image
		// carries it.
		spec.Labels[runtime.LabelAgent] = c.Agent
		spec.SiloTools = c.Class == runtime.Sandbox
	}
	if c.Seq != 0 {
		spec.Labels[runtime.LabelSeq] = objseq.Format(c.Seq)
	}
	for _, m := range c.Mounts {
		switch m.Type {
		case "volume":
			p, err := s.volumePath(ctx, silo, m.Name)
			if err != nil {
				return err
			}
			spec.Mounts = append(spec.Mounts, runtime.Mount{Kind: runtime.MountVolume, Source: p, Target: m.Target, ReadOnly: m.ReadOnly})
		case "tmpfs":
			spec.Mounts = append(spec.Mounts, runtime.Mount{Kind: runtime.MountTmpfs, Target: m.Target, SizeMB: m.SizeMB})
		}
	}
	return d.Create(ctx, spec)
}

// firstEndpoint is the container's endpoint on its first network, by
// name, or nil.
func (c *Container) firstEndpoint() *Endpoint {
	if mode, _ := c.HostConfig["NetworkMode"].(string); c.Networks[mode] != nil {
		return c.Networks[mode]
	}
	for _, k := range sortedKeys(c.Networks) {
		return c.Networks[k]
	}
	return nil
}

// undoCreate forgets a container the runtime couldn't make, with its
// charges and the volumes its create made.
func (s *Server) undoCreate(q *request, c *Container, made []*Volume) {
	ctx := context.WithoutCancel(q.ctx())
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		if _, err := refund(ctx, tx, Object(q.target.ID, kindContainer, c.ID)); err != nil {
			return err
		}
		for _, v := range made {
			if _, err := s.deleteVolume(ctx, tx, q.target.ID, v); err != nil {
				return err
			}
		}
		return remove(ctx, tx, q.target.ID, kindContainer, c.ID)
	})
	for _, v := range made {
		s.deleteVolumeDir(ctx, q.target.ID, v.Name)
	}
}

// state is what the container's driver says of it. A container the
// driver has lost is dead.
func (s *Server) state(ctx context.Context, c *Container) runtime.Container {
	d := s.drivers[c.Class]
	if d != nil {
		cs, err := d.List(ctx)
		if err == nil {
			for _, rc := range cs {
				if rc.ID == c.ID {
					return rc
				}
			}
		}
	}
	return runtime.Container{ID: c.ID, Class: c.Class, State: "dead"}
}

// get is the call's container, read in a transaction of its own.
func (s *Server) get(q *request) (*Container, error) {
	var c *Container
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		c, err = s.container(q.ctx(), tx, q.target.ID, q.params["id"])
		return err
	})
	if err == nil && q.route.class() == control.Mutation {
		err = s.refuseToolContainer(q, c)
	}
	return c, err
}

// save writes c back, in a transaction of its own, with an audit
// record of the call and its charges.
func (s *Server) save(q *request, c *Container, fields map[string]any, charges []fenceapi.ChargeLine) error {
	return s.st.Tx(context.WithoutCancel(q.ctx()), func(tx store.Tx) error {
		if err := put(q.ctx(), tx, q.target.ID, kindContainer, c.ID, c.Name, c); err != nil {
			return err
		}
		if fields == nil {
			fields = map[string]any{}
		}
		fields["id"] = c.ID
		return s.auditTx(q.ctx(), tx, q, "OK", fields, charges)
	})
}

func (s *Server) containerStart(q *request) error {
	switch err := s.start(q); {
	case errors.Is(err, errStarted):
		return nil
	case err != nil:
		return s.refuse(q, err)
	}
	return noContent(q)
}

// start starts the call's container: it charges its CPU and memory,
// gives its published ports their numbers, and starts it.
func (s *Server) start(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	switch s.state(q.ctx(), c).State {
	case runtime.Running:
		q.w.WriteHeader(http.StatusNotModified)
		return errStarted
	case runtime.Paused:
		return conflict("cannot start a paused container, try unpause instead")
	case "dead":
		return conflict("silo: container %s is gone from its runtime; remove it", c.ID)
	}
	for _, m := range c.Mounts {
		if m.Type == "volume" {
			if err := s.checkMount(q.target.ID, m.Name); err != nil {
				return err
			}
		}
	}
	var charges []fenceapi.ChargeLine
	s.mu.Lock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		if err := s.assignPorts(q.ctx(), tx, q.target.ID, c); err != nil {
			return err
		}
		var err error
		charges, err = s.charge(q.ctx(), tx, q, runObject(q.target.ID, c.ID), line{ClassVCPUm, c.VCPUm}, line{ClassMemoryMB, c.MemoryMB})
		if err != nil {
			return err
		}
		return put(q.ctx(), tx, q.target.ID, kindContainer, c.ID, c.Name, c)
	})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.drivers[c.Class].Start(q.ctx(), c.ID); err != nil {
		s.refundLater(q.ctx(), runObject(q.target.ID, c.ID))
		c.Error = err.Error()
		_ = s.save(q, c, map[string]any{"error": c.Error}, nil)
		return err
	}
	c.Error = ""
	c.StartedAt = time.Now().UTC()
	if !c.FinishedAt.IsZero() {
		c.RestartCount++
	}
	if err := s.save(q, c, nil, charges); err != nil {
		return err
	}
	s.watch(q.target.ID, c)
	return nil
}

// errStarted is start's answer for a container already running, whose
// 304 it has written.
var errStarted = errors.New("already started")

// assignPorts gives each published port with no number one from the
// range, and refuses one another running container of the silo has:
// the listener on docker.silo.internal tells silos apart, not
// containers.
func (s *Server) assignPorts(ctx context.Context, tx store.Tx, silo string, c *Container) error {
	cs, err := s.containers(ctx, tx, silo)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, o := range cs {
		if o.ID == c.ID || s.state(ctx, o).State != runtime.Running {
			continue
		}
		for _, p := range o.Ports {
			taken[p.Proto+"/"+strconv.Itoa(p.Public)] = true
		}
	}
	for i := range c.Ports {
		p := &c.Ports[i]
		if !p.Fixed {
			p.Public = 0 // a number given at a previous start is given again
		}
		k := func(n int) string { return p.Proto + "/" + strconv.Itoa(n) }
		if p.Public != 0 {
			if taken[k(p.Public)] {
				return conflict("Bind for 0.0.0.0:%d failed: port is already allocated", p.Public)
			}
			taken[k(p.Public)] = true
			continue
		}
		for n := s.opts.PublishPorts[0]; n <= s.opts.PublishPorts[1]; n++ {
			if !taken[k(n)] {
				p.Public = n
				taken[k(n)] = true
				break
			}
		}
		if p.Public == 0 {
			return conflict("silo: no free port is left to publish %d/%s on", p.Private, p.Proto)
		}
	}
	return nil
}

// watch waits, in the background, for c to stop on its own, and then
// settles it: its run charge refunded, and AutoRemove honoured.
func (s *Server) watch(silo string, c *Container) {
	d := s.drivers[c.Class]
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if _, err := d.Wait(s.bg, c.ID); err != nil {
			return
		}
		s.settle(s.bg, silo, c.ID)
	}()
}

// settle records a container that has stopped: it refunds its run
// charge and notes when it finished, or removes it if it asked to be.
func (s *Server) settle(ctx context.Context, silo, id string) {
	var auto *Container
	s.mu.Lock()
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		c, err := row[Container](ctx, tx, silo, kindContainer, id)
		if err != nil || c == nil {
			return err
		}
		if _, err := refund(ctx, tx, runObject(silo, id)); err != nil {
			return err
		}
		if rc := s.state(ctx, c); rc.Exit != nil {
			c.FinishedAt = rc.Exit.Finished
		} else {
			c.FinishedAt = time.Now().UTC()
		}
		if a, _ := c.HostConfig["AutoRemove"].(bool); a {
			auto = c
		}
		return put(ctx, tx, silo, kindContainer, id, c.Name, c)
	})
	s.mu.Unlock()
	if auto != nil {
		_ = s.removeContainer(ctx, silo, auto, true, nil)
	}
}

func (s *Server) containerStop(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	st := s.state(q.ctx(), c).State
	if st != runtime.Running && st != runtime.Paused {
		q.w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if err := s.stopContainer(q, c); err != nil {
		return s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"id": c.ID}, nil)
	return noContent(q)
}

// stopContainer stops c, waiting up to t seconds (or its StopTimeout, or 10)
// before the kill, and settles it.
func (s *Server) stopContainer(q *request, c *Container) error {
	timeout := 10 * time.Second
	if v, ok := c.Config["StopTimeout"].(float64); ok {
		timeout = time.Duration(v) * time.Second
	}
	if t := q.query("t"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil {
			return invalid("invalid value for t: %s", t)
		}
		timeout = time.Duration(n) * time.Second
	}
	d := s.drivers[c.Class]
	if err := d.Stop(q.ctx(), c.ID, timeout); err != nil && !errors.Is(err, runtime.ErrState) {
		return err
	}
	if _, err := d.Wait(q.ctx(), c.ID); err != nil {
		return err
	}
	s.settle(context.WithoutCancel(q.ctx()), q.target.ID, c.ID)
	return nil
}

func (s *Server) containerRestart(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if st := s.state(q.ctx(), c).State; st == runtime.Running || st == runtime.Paused {
		if err := s.stopContainer(q, c); err != nil {
			return s.refuse(q, err)
		}
	}
	if err := s.start(q); err != nil && !errors.Is(err, errStarted) {
		return s.refuse(q, err)
	}
	return noContent(q)
}

func (s *Server) containerKill(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	sig := syscall.SIGKILL
	if v := q.query("signal"); v != "" {
		if sig, err = parseSignal(v); err != nil {
			return s.refuse(q, err)
		}
	}
	if st := s.state(q.ctx(), c).State; st != runtime.Running && st != runtime.Paused {
		return s.refuse(q, conflict("Container %s is not running", c.ID))
	}
	if err := s.drivers[c.Class].Kill(q.ctx(), c.ID, sig); err != nil {
		if errors.Is(err, runtime.ErrState) {
			return s.refuse(q, conflict("Container %s is not running", c.ID))
		}
		return s.refuse(q, err)
	}
	if s.state(q.ctx(), c).State == runtime.Stopped {
		s.settle(context.WithoutCancel(q.ctx()), q.target.ID, c.ID)
	}
	s.audit(q, "OK", map[string]any{"id": c.ID, "signal": int(sig)}, nil)
	return noContent(q)
}

func (s *Server) containerWait(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	cond := q.query("condition")
	switch cond {
	case "", "not-running", "next-exit", "removed":
	default:
		return invalid("invalid condition: %q", cond)
	}
	d := s.drivers[c.Class]
	rc := s.state(q.ctx(), c)
	running := rc.State == runtime.Running || rc.State == runtime.Paused
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	if fl, ok := q.w.(http.Flusher); ok {
		fl.Flush()
	}
	code := 0
	switch {
	case !running && cond != "next-exit" && cond != "removed":
		if rc.Exit != nil {
			code = exitCode(*rc.Exit)
		}
	default:
		ex, err := d.Wait(q.ctx(), c.ID)
		if err != nil {
			_ = json.NewEncoder(q.w).Encode(map[string]any{"StatusCode": -1, "Error": map[string]string{"Message": err.Error()}})
			return nil
		}
		code = exitCode(ex)
	}
	_ = json.NewEncoder(q.w).Encode(map[string]any{"StatusCode": code, "Error": nil})
	return nil
}

// exitCode is Docker's exit code for ex: 128+N for a signal.
func exitCode(ex runtime.ExitStatus) int {
	if ex.Signal != 0 && ex.Code == 0 {
		return 128 + int(ex.Signal)
	}
	return ex.Code
}

func (s *Server) containerPause(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if err := s.drivers[c.Class].Pause(q.ctx(), c.ID); err != nil {
		if errors.Is(err, runtime.ErrState) {
			return s.refuse(q, conflict("Container %s is not running", c.ID))
		}
		return s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"id": c.ID}, nil)
	return noContent(q)
}

func (s *Server) containerUnpause(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if err := s.drivers[c.Class].Resume(q.ctx(), c.ID); err != nil {
		if errors.Is(err, runtime.ErrState) {
			return s.refuse(q, conflict("Container %s is not paused", c.ID))
		}
		return s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"id": c.ID}, nil)
	return noContent(q)
}

func (s *Server) containerRename(q *request) error {
	name := strings.TrimPrefix(q.query("name"), "/")
	if !validName.MatchString(name) || len(name) < 2 {
		return s.refuse(q, invalid("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		c, err := s.container(q.ctx(), tx, q.target.ID, q.params["id"])
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		for _, o := range cs {
			if o.Name == name && o.ID != c.ID {
				return conflict("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, o.ID)
			}
		}
		old := c.Name
		c.Name = name
		if err := put(q.ctx(), tx, q.target.ID, kindContainer, c.ID, c.Name, c); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"from": old, "to": name}, nil)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return noContent(q)
}

// containerResize is a TTY resize: the workload's terminal follows the
// attach stream's, which a later step serves, so there is nothing to
// resize yet.
func (s *Server) containerResize(q *request) error {
	if _, err := s.get(q); err != nil {
		return err
	}
	q.w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) containerDelete(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if st := s.state(q.ctx(), c).State; (st == runtime.Running || st == runtime.Paused) && !q.flag("force") {
		return s.refuse(q, conflict("You cannot remove a running container %s. Stop the container before attempting removal or force remove", c.ID))
	}
	if err := s.removeContainer(q.ctx(), q.target.ID, c, q.flag("v"), q); err != nil {
		return s.refuse(q, err)
	}
	return noContent(q)
}

// removeContainer kills c if it runs, deletes it from its runtime and
// forgets it, with its charges and, with volumes, its anonymous
// volumes. q, if set, is the call to audit it under.
func (s *Server) removeContainer(ctx context.Context, silo string, c *Container, volumes bool, q *request) error {
	ctx = context.WithoutCancel(ctx)
	d := s.drivers[c.Class]
	if d != nil {
		if err := d.Kill(ctx, c.ID, syscall.SIGKILL); err == nil {
			if _, err := d.Wait(ctx, c.ID); err != nil {
				return err
			}
		}
		if err := d.Delete(ctx, c.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	var gone []string
	s.mu.Lock()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var charges []fenceapi.ChargeLine
		for _, o := range []string{runObject(silo, c.ID), Object(silo, kindContainer, c.ID)} {
			ch, err := refund(ctx, tx, o)
			if err != nil {
				return err
			}
			charges = append(charges, ch...)
		}
		if err := remove(ctx, tx, silo, kindContainer, c.ID); err != nil {
			return err
		}
		if volumes {
			cs, err := s.containers(ctx, tx, silo)
			if err != nil {
				return err
			}
			for _, m := range c.Mounts {
				if m.Type != "volume" || len(usedBy(cs, m.Name)) > 0 {
					continue
				}
				v, err := row[Volume](ctx, tx, silo, kindVolume, m.Name)
				if err != nil {
					return err
				}
				if v == nil || !v.Anonymous {
					continue
				}
				ch, err := s.deleteVolume(ctx, tx, silo, v)
				if err != nil {
					return err
				}
				charges = append(charges, ch...)
				gone = append(gone, v.Name)
			}
		}
		if q == nil {
			return nil
		}
		return s.auditTx(ctx, tx, q, "OK", map[string]any{"id": c.ID, "volumes": gone}, charges)
	})
	s.mu.Unlock()
	for _, v := range gone {
		s.deleteVolumeDir(ctx, silo, v)
	}
	return err
}

func (s *Server) containerPrune(q *request) error {
	f, err := filters(q, "label", "until")
	if err != nil {
		return s.refuse(q, err)
	}
	var cs []*Container
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		cs, err = s.containers(q.ctx(), tx, q.target.ID)
		return err
	})
	if err != nil {
		return err
	}
	deleted := []string{}
	for _, c := range cs {
		labels, _ := c.Config["Labels"].(map[string]any)
		if st := s.state(q.ctx(), c).State; st == runtime.Running || st == runtime.Paused || !f.labels(stringMap(labels)) || c.Agent != "" {
			continue
		}
		if err := s.removeContainer(q.ctx(), q.target.ID, c, false, q); err != nil {
			return s.refuse(q, err)
		}
		deleted = append(deleted, c.ID)
	}
	return ok(q, map[string]any{"ContainersDeleted": deleted, "SpaceReclaimed": 0})
}

func stringMap(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
