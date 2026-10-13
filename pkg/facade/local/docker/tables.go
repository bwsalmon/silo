package docker

import (
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/facade"
)

// The facade's tables, as data (local-backend.md, "Endpoints served",
// "Refused"): the routes it serves, the routes Docker has that it
// refuses, and each write's request allowlist. A route that is in
// neither is Docker's 404 "page not found".

// A handler serves one route. It writes its own response, which may be
// a stream; an error it returns is written in Docker's shape, if
// nothing has been written yet.
type handler func(s *Server, q *request) error

// route is one served endpoint. A pattern's {x} matches one path
// segment and {x+} one or more, for image names with slashes in them.
type route struct {
	method   string
	pattern  string
	resource string // what a manage grant has to cover
	// read is set for the reads that aren't GETs (call 52): classified
	// by endpoint, not by verb.
	read bool
	h    handler
}

// class is the route's class under pkg/control.
func (r *route) class() control.Class {
	if r.read || r.method == http.MethodGet || r.method == http.MethodHead {
		return control.Reading
	}
	return control.Mutation
}

// routes are the endpoints served. Exec, attach and archive are served
// by later steps of the plan, and answer 501 until then.
var routes = []route{
	// System.
	{method: "GET", pattern: "/_ping", resource: "containers", h: (*Server).ping},
	{method: "HEAD", pattern: "/_ping", resource: "containers", h: (*Server).ping},
	{method: "GET", pattern: "/version", resource: "containers", h: (*Server).version},
	{method: "GET", pattern: "/info", resource: "containers", h: (*Server).info},
	{method: "GET", pattern: "/events", resource: "containers", h: (*Server).events},
	{method: "GET", pattern: "/system/df", resource: "containers", h: (*Server).systemDF},

	// Containers.
	{method: "POST", pattern: "/containers/create", resource: "containers", h: (*Server).containerCreate},
	{method: "GET", pattern: "/containers/json", resource: "containers", h: (*Server).containerList},
	{method: "POST", pattern: "/containers/prune", resource: "containers", h: (*Server).containerPrune},
	{method: "GET", pattern: "/containers/{id}/json", resource: "containers", h: (*Server).containerInspect},
	{method: "POST", pattern: "/containers/{id}/start", resource: "containers", h: (*Server).containerStart},
	{method: "POST", pattern: "/containers/{id}/stop", resource: "containers", h: (*Server).containerStop},
	{method: "POST", pattern: "/containers/{id}/restart", resource: "containers", h: (*Server).containerRestart},
	{method: "POST", pattern: "/containers/{id}/kill", resource: "containers", h: (*Server).containerKill},
	{method: "POST", pattern: "/containers/{id}/wait", resource: "containers", read: true, h: (*Server).containerWait},
	{method: "POST", pattern: "/containers/{id}/pause", resource: "containers", h: (*Server).containerPause},
	{method: "POST", pattern: "/containers/{id}/unpause", resource: "containers", h: (*Server).containerUnpause},
	{method: "POST", pattern: "/containers/{id}/rename", resource: "containers", h: (*Server).containerRename},
	{method: "POST", pattern: "/containers/{id}/resize", resource: "containers", h: (*Server).containerResize},
	{method: "DELETE", pattern: "/containers/{id}", resource: "containers", h: (*Server).containerDelete},
	{method: "GET", pattern: "/containers/{id}/logs", resource: "containers", h: (*Server).containerLogs},
	{method: "GET", pattern: "/containers/{id}/stats", resource: "containers", h: (*Server).containerStats},
	{method: "GET", pattern: "/containers/{id}/top", resource: "containers", h: (*Server).notYet},
	{method: "POST", pattern: "/containers/{id}/attach", resource: "containers", h: (*Server).notYet},
	{method: "POST", pattern: "/containers/{id}/exec", resource: "containers", h: (*Server).notYet},
	{method: "POST", pattern: "/exec/{id}/start", resource: "containers", h: (*Server).notYet},
	{method: "POST", pattern: "/exec/{id}/resize", resource: "containers", h: (*Server).notYet},
	{method: "GET", pattern: "/exec/{id}/json", resource: "containers", h: (*Server).notYet},
	{method: "HEAD", pattern: "/containers/{id}/archive", resource: "containers", h: (*Server).notYet},
	{method: "GET", pattern: "/containers/{id}/archive", resource: "containers", h: (*Server).notYet},
	{method: "PUT", pattern: "/containers/{id}/archive", resource: "containers", h: (*Server).notYet},

	// Images.
	{method: "POST", pattern: "/images/create", resource: "images", h: (*Server).imageCreate},
	{method: "GET", pattern: "/images/json", resource: "images", h: (*Server).imageList},
	{method: "POST", pattern: "/images/prune", resource: "images", h: (*Server).imagePrune},
	{method: "GET", pattern: "/images/{name+}/json", resource: "images", h: (*Server).imageInspect},
	{method: "GET", pattern: "/images/{name+}/history", resource: "images", h: (*Server).imageHistory},
	{method: "POST", pattern: "/images/{name+}/tag", resource: "images", h: (*Server).imageTag},
	{method: "DELETE", pattern: "/images/{name+}", resource: "images", h: (*Server).imageDelete},

	// Networks.
	{method: "GET", pattern: "/networks", resource: "networks", h: (*Server).networkList},
	{method: "POST", pattern: "/networks/create", resource: "networks", h: (*Server).networkCreate},
	{method: "POST", pattern: "/networks/prune", resource: "networks", h: (*Server).networkPrune},
	{method: "GET", pattern: "/networks/{id}", resource: "networks", h: (*Server).networkInspect},
	{method: "DELETE", pattern: "/networks/{id}", resource: "networks", h: (*Server).networkDelete},
	{method: "POST", pattern: "/networks/{id}/connect", resource: "networks", h: (*Server).networkConnect},
	{method: "POST", pattern: "/networks/{id}/disconnect", resource: "networks", h: (*Server).networkDisconnect},

	// Volumes.
	{method: "GET", pattern: "/volumes", resource: "volumes", h: (*Server).volumeList},
	{method: "POST", pattern: "/volumes/create", resource: "volumes", h: (*Server).volumeCreate},
	{method: "POST", pattern: "/volumes/prune", resource: "volumes", h: (*Server).volumePrune},
	{method: "GET", pattern: "/volumes/{name}", resource: "volumes", h: (*Server).volumeInspect},
	{method: "DELETE", pattern: "/volumes/{name}", resource: "volumes", h: (*Server).volumeDelete},
}

// refusal is a route Docker has and the facade refuses. An empty
// method is any; prefix matches the pattern and anything below it.
type refusal struct {
	method  string
	pattern string
	prefix  bool
	err     *Error
}

var (
	errBuild = unsupported("silo: building images is not supported by this facade; build with kaniko or BuildKit in a container")
	errIO    = unsupported("silo: image import and export are not supported by this facade")
)

// refused are the refused routes, before routes are looked at: the
// order matters where a refusal is narrower than a served pattern.
var refused = []refusal{
	{method: "POST", pattern: "/build", err: errBuild},
	{method: "POST", pattern: "/build/prune", err: errBuild},
	{pattern: "/session", err: errBuild},
	{method: "GET", pattern: "/containers/{id}/export", err: errIO},
	{method: "GET", pattern: "/containers/{id}/changes", err: errIO},
	{method: "GET", pattern: "/images/search", err: errIO},
	{method: "POST", pattern: "/commit", err: errIO},
	{method: "POST", pattern: "/images/{name+}/push", err: errIO},
	{method: "GET", pattern: "/images/get", err: errIO},
	{method: "GET", pattern: "/images/{name+}/get", err: errIO},
	{method: "POST", pattern: "/images/load", err: errIO},
	{method: "POST", pattern: "/auth", err: unsupported("silo: registry login is not supported; credentials go with each pull in X-Registry-Auth")},
	{method: "POST", pattern: "/containers/{id}/update", err: unsupported("silo: updating a container's resources is not supported")},
	{method: "GET", pattern: "/containers/{id}/attach/ws", err: unsupported("silo: attach over websocket is not supported; use POST /containers/{id}/attach")},
	{pattern: "/containers/{id}/checkpoints", prefix: true, err: unsupported("silo: checkpoints are not supported")},
	{pattern: "/swarm", prefix: true, err: notSwarm},
	{pattern: "/nodes", prefix: true, err: notSwarm},
	{pattern: "/services", prefix: true, err: notSwarm},
	{pattern: "/tasks", prefix: true, err: notSwarm},
	{pattern: "/secrets", prefix: true, err: notSwarm},
	{pattern: "/configs", prefix: true, err: notSwarm},
	{pattern: "/plugins", prefix: true, err: unsupported("silo: plugins are not supported")},
	{method: "GET", pattern: "/distribution/{name+}/json", err: unsupported("silo: distribution inspect is not supported")},
}

// lookup finds path's route: a refusal, a route and its parameters,
// or neither.
func lookup(method, path string) (*route, map[string]string, *Error) {
	for _, r := range refused {
		if r.method != "" && r.method != method {
			continue
		}
		if _, ok := match(r.pattern, path, r.prefix); ok {
			return nil, nil, r.err
		}
	}
	for i := range routes {
		r := &routes[i]
		if r.method != method {
			continue
		}
		if p, ok := match(r.pattern, path, false); ok {
			return r, p, nil
		}
	}
	return nil, nil, pageNotFound
}

// match matches path against pattern. With prefix, anything below the
// pattern matches too.
func match(pattern, path string, prefix bool) (map[string]string, bool) {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	xs := strings.Split(strings.Trim(path, "/"), "/")
	params := map[string]string{}
	j := 0
	for i, p := range ps {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "+}") {
			// Greedy, leaving one segment for each fixed segment after it.
			n := len(xs) - j - (len(ps) - i - 1)
			if n < 1 {
				return nil, false
			}
			params[strings.Trim(p, "{+}")] = strings.Join(xs[j:j+n], "/")
			j += n
			continue
		}
		if j >= len(xs) {
			return nil, false
		}
		switch {
		case strings.HasPrefix(p, "{"):
			if xs[j] == "" {
				return nil, false
			}
			params[strings.Trim(p, "{}")] = xs[j]
		case p != xs[j]:
			return nil, false
		}
		j++
	}
	if j != len(xs) && !prefix {
		return nil, false
	}
	return params, true
}

// Request allowlists, by route pattern. A field outside its list that
// is set to anything but its default is 400, naming it. The refusals
// that are a fence or a policy, not a shape (local-backend.md,
// "Refused"), are checked first, with 403.
var requestFields = map[string]facade.Fields{
	"/containers/create": facade.AllowFields([]string{
		// Config.
		"Hostname", "Domainname", "User", "AttachStdin", "AttachStdout", "AttachStderr",
		"ExposedPorts", "Tty", "OpenStdin", "StdinOnce", "Env", "Cmd", "Healthcheck",
		"ArgsEscaped", "Image", "Volumes", "WorkingDir", "Entrypoint", "NetworkDisabled",
		"Labels", "StopSignal", "StopTimeout", "Shell",
		// HostConfig.
		"HostConfig.Binds", "HostConfig.NetworkMode", "HostConfig.PortBindings",
		"HostConfig.RestartPolicy", "HostConfig.AutoRemove", "HostConfig.VolumeDriver",
		"HostConfig.Mounts[].Type", "HostConfig.Mounts[].Source", "HostConfig.Mounts[].Target",
		"HostConfig.Mounts[].ReadOnly", "HostConfig.Mounts[].Consistency",
		"HostConfig.Mounts[].VolumeOptions.NoCopy", "HostConfig.Mounts[].VolumeOptions.Labels",
		"HostConfig.Mounts[].TmpfsOptions.SizeBytes", "HostConfig.Mounts[].TmpfsOptions.Mode",
		"HostConfig.Tmpfs", "HostConfig.CapDrop", "HostConfig.Init", "HostConfig.LogConfig",
		"HostConfig.SecurityOpt", "HostConfig.ReadonlyRootfs", "HostConfig.ShmSize",
		"HostConfig.Memory", "HostConfig.MemoryReservation", "HostConfig.MemorySwap",
		"HostConfig.NanoCpus", "HostConfig.CpuShares", "HostConfig.PidsLimit", "HostConfig.Ulimits",
		"HostConfig.ExtraHosts", "HostConfig.GroupAdd", "HostConfig.Runtime", "HostConfig.StorageOpt",
		"HostConfig.OomScoreAdj", "HostConfig.Isolation", "HostConfig.PublishAllPorts",
		"HostConfig.ConsoleSize", "HostConfig.IpcMode", "HostConfig.PidMode", "HostConfig.UTSMode",
		"HostConfig.UsernsMode", "HostConfig.CgroupnsMode", "HostConfig.Privileged",
		"HostConfig.CapAdd", "HostConfig.Devices", "HostConfig.DeviceRequests",
		"HostConfig.DeviceCgroupRules", "HostConfig.Sysctls", "HostConfig.CpusetCpus",
		"HostConfig.CpusetMems",
		// NetworkingConfig.
		"NetworkingConfig.EndpointsConfig",
	}),
	"/networks/create": facade.AllowFields([]string{
		"Name", "CheckDuplicate", "Driver", "Internal", "Attachable", "Labels",
		"IPAM.Driver", "IPAM.Config[].Subnet", "IPAM.Config[].Gateway",
	}),
	"/networks/{id}/connect": facade.AllowFields([]string{
		"Container", "EndpointConfig.IPAMConfig.IPv4Address", "EndpointConfig.Aliases",
		"EndpointConfig.NetworkID",
	}),
	"/networks/{id}/disconnect": facade.AllowFields([]string{"Container", "Force"}),
	"/volumes/create":           facade.AllowFields([]string{"Name", "Driver", "DriverOpts", "Labels"}),
}

// endpointFields is the allowlist of one entry of
// NetworkingConfig.EndpointsConfig.
var endpointFields = facade.AllowFields([]string{
	"IPAMConfig.IPv4Address", "Aliases", "NetworkID", "EndpointID", "Links", "DriverOpts",
})

// reservedLabel reports whether a label key is in Silo's namespace,
// which silod sets and responses strip (local-backend.md, "Refused").
// Docker's and compose's own (com.docker.*) are the tenant's.
func reservedLabel(k string) bool {
	return strings.HasPrefix(k, "dev.silo.") || strings.HasPrefix(k, "silo.")
}
