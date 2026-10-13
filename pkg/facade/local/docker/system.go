package docker

import (
	"net/http"
	goruntime "runtime"
	"slices"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
)

// OperatingSystem is what /info says the facade runs on.
const OperatingSystem = "Silo (local backend)"

func (s *Server) ping(q *request) error {
	h := q.w.Header()
	h.Set("Api-Version", APIVersion)
	h.Set("Docker-Experimental", "false")
	h.Set("Ostype", "linux")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	q.w.WriteHeader(http.StatusOK)
	if q.r.Method != http.MethodHead {
		_, _ = q.w.Write([]byte("OK"))
	}
	return nil
}

// kernelVersion is the class's kernel, which /version and /info give
// instead of the host's.
func kernelVersion(c runtime.Class) string {
	if c == runtime.VM {
		return "granule"
	}
	return "gVisor"
}

// classes are the silo's effective isolation, narrowed to the classes
// the host can run now: what /info reports as Runtimes.
func (s *Server) classes(q *request) (avail []runtime.Class, def runtime.Class, err error) {
	var iso fenceapi.Isolation
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		iso, _, err = fenceapi.EffectiveIsolation(q.ctx(), tx, q.chain)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	for _, c := range iso.Allowed {
		if d := s.drivers[runtime.Class(c)]; d != nil && d.Probe(q.ctx()).OK {
			avail = append(avail, runtime.Class(c))
		}
	}
	return avail, runtime.Class(iso.Default), nil
}

func (s *Server) version(q *request) error {
	_, def, err := s.classes(q)
	if err != nil {
		return err
	}
	v := s.opts.Version
	if v == "" {
		v = "dev"
	}
	details := map[string]string{"ApiVersion": APIVersion, "MinAPIVersion": MinAPIVersion, "Os": "linux",
		"Arch": goruntime.GOARCH, "KernelVersion": kernelVersion(def), "GoVersion": goruntime.Version()}
	return ok(q, map[string]any{
		"Platform":   map[string]string{"Name": OperatingSystem},
		"Components": []map[string]any{{"Name": "Engine", "Version": v, "Details": details}},
		"Version":    v, "ApiVersion": APIVersion, "MinAPIVersion": MinAPIVersion, "GitCommit": "",
		"GoVersion": goruntime.Version(), "Os": "linux", "Arch": goruntime.GOARCH,
		"KernelVersion": kernelVersion(def), "BuildTime": "",
	})
}

// info describes the calling silo, not the host (local-backend.md,
// "What /info says"): its quota for CPUs and memory, its own counts,
// and the classes it may run.
func (s *Server) info(q *request) error {
	avail, def, err := s.classes(q)
	if err != nil {
		return err
	}
	var cs []*Container
	var images int
	var cpus, mem int64
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if cs, err = s.containers(q.ctx(), tx, q.target.ID); err != nil {
			return err
		}
		is, err := s.images(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		images = len(is)
		if cpus, err = granted(q.ctx(), tx, q.target.ID, ClassVCPUm); err != nil {
			return err
		}
		mem, err = granted(q.ctx(), tx, q.target.ID, ClassMemoryMB)
		return err
	})
	if err != nil {
		return err
	}
	var running, paused, stopped int
	for _, c := range cs {
		switch s.state(q.ctx(), c).State {
		case runtime.Running:
			running++
		case runtime.Paused:
			paused++
		default:
			stopped++
		}
	}
	runtimes := map[string]any{}
	for _, c := range avail {
		runtimes[runtimeName(c)] = map[string]string{"path": runtimeName(c)}
	}
	defName := ""
	if slices.Contains(avail, def) {
		defName = runtimeName(def)
	}
	v := s.opts.Version
	if v == "" {
		v = "dev"
	}
	return ok(q, map[string]any{
		"ID": q.target.ID, "Name": q.target.Path, "Containers": len(cs), "ContainersRunning": running,
		"ContainersPaused": paused, "ContainersStopped": stopped, "Images": images,
		"Driver": "silo", "DriverStatus": [][2]string{}, "Plugins": map[string]any{"Volume": []string{"local"},
			"Network": []string{"bridge"}, "Log": []string{"json-file", "local"}},
		"MemoryLimit": true, "SwapLimit": false, "CpuCfsPeriod": true, "CpuCfsQuota": true, "CPUShares": true,
		"PidsLimit": true, "IPv4Forwarding": true, "Debug": false, "OomKillDisable": false,
		"NFd": 0, "NGoroutines": 0, "SystemTime": time.Now().UTC().Format(time.RFC3339Nano),
		"LoggingDriver": "json-file", "CgroupDriver": "systemd", "CgroupVersion": "2", "NEventsListener": 0,
		"KernelVersion": kernelVersion(def), "OperatingSystem": OperatingSystem, "OSVersion": "",
		"OSType": "linux", "Architecture": archName(), "IndexServerAddress": "https://index.docker.io/v1/",
		"RegistryConfig": map[string]any{"IndexConfigs": map[string]any{}, "Mirrors": []string{}},
		"NCPU":           max(cpus/1000, 0), "MemTotal": mem << 20, "GenericResources": nil,
		"DockerRootDir": "", "HttpProxy": "", "HttpsProxy": "", "NoProxy": "", "Labels": []string{},
		"ExperimentalBuild": false, "ServerVersion": v, "Runtimes": runtimes, "DefaultRuntime": defName,
		"Swarm": map[string]any{"NodeID": "", "NodeAddr": "", "LocalNodeState": "inactive",
			"ControlAvailable": false, "Error": "", "RemoteManagers": nil},
		"LiveRestoreEnabled": false, "Isolation": "", "InitBinary": "",
		"SecurityOptions": []string{"name=seccomp,profile=builtin", "name=no-new-privileges"},
		"Warnings":        []string{},
	})
}

// systemDF is the silo's disk use, by what it is charged.
func (s *Server) systemDF(q *request) error {
	var out map[string]any
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		is, err := s.images(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		vs, err := rows[Volume](q.ctx(), tx, q.target.ID, kindVolume)
		if err != nil {
			return err
		}
		var layers int64
		images := []map[string]any{}
		for _, i := range is {
			layers += i.Size
			images = append(images, imageSummary(i))
		}
		ctrs := []map[string]any{}
		for _, c := range cs {
			ctrs = append(ctrs, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Image": c.Image,
				"ImageID": c.ImageID, "Created": c.Created.Unix(), "State": status(s.state(q.ctx(), c)),
				"SizeRw": 0, "SizeRootFs": 0})
		}
		vols := []map[string]any{}
		for _, v := range vs {
			j := volumeJSON(v)
			j["UsageData"] = map[string]any{"Size": -1, "RefCount": len(usedBy(cs, v.Name))}
			vols = append(vols, j)
		}
		out = map[string]any{"LayersSize": layers, "Images": images, "Containers": ctrs, "Volumes": vols, "BuildCache": []any{}}
		return nil
	})
	if err != nil {
		return err
	}
	return ok(q, out)
}

// events streams the silo's events. silod keeps no event log yet, so
// the stream stays open and empty until the caller goes away, or the
// until it gives has passed.
func (s *Server) events(q *request) error {
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	if fl, ok := q.w.(http.Flusher); ok {
		fl.Flush()
	}
	if q.query("until") != "" {
		return nil
	}
	<-q.ctx().Done()
	return nil
}
