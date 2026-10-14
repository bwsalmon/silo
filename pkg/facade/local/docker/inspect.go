package docker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/store"
)

// status is Docker's State.Status for a runtime state.
func status(rc runtime.Container) string {
	switch rc.State {
	case runtime.Stopped:
		return "exited"
	case "":
		return "created"
	}
	return string(rc.State)
}

// statusText is the list's Status column: "Up 3 minutes", "Exited (0)
// 2 hours ago".
func statusText(c *Container, rc runtime.Container, health string) string {
	switch rc.State {
	case runtime.Running:
		switch health {
		case healthStarting:
			return "Up " + ago(c.StartedAt) + " (health: starting)"
		case healthHealthy, healthUnhealthy:
			return "Up " + ago(c.StartedAt) + " (" + health + ")"
		}
		return "Up " + ago(c.StartedAt)
	case runtime.Paused:
		return "Up " + ago(c.StartedAt) + " (Paused)"
	case runtime.Stopped:
		code := 0
		if rc.Exit != nil {
			code = exitCode(*rc.Exit)
		}
		return "Exited (" + strconv.Itoa(code) + ") " + ago(c.FinishedAt) + " ago"
	case runtime.Created:
		return "Created"
	}
	return "Dead"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "Less than a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " hours"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days"
}

// portsJSON is NetworkSettings.Ports: each published port at
// 0.0.0.0, as Docker shows one, and each exposed one with no binding.
func portsJSON(c *Container, running bool) map[string]any {
	out := map[string]any{}
	for _, p := range c.Ports {
		k := strconv.Itoa(p.Private) + "/" + p.Proto
		if !running {
			out[k] = nil
			continue
		}
		list, _ := out[k].([]map[string]string)
		out[k] = append(list, map[string]string{"HostIp": "0.0.0.0", "HostPort": strconv.Itoa(p.Public)})
	}
	return out
}

func networksJSON(c *Container, nets map[string]*Network) map[string]any {
	out := map[string]any{}
	for name, e := range c.Networks {
		bits, gw := 0, ""
		if n := nets[e.NetworkID]; n != nil {
			bits, gw = n.Subnet.Bits(), n.Gateway().String()
		}
		out[name] = map[string]any{
			"IPAMConfig": nil, "Links": nil, "Aliases": nonNil(e.Aliases), "NetworkID": e.NetworkID,
			"EndpointID": e.EndpointID, "Gateway": gw, "IPAddress": e.Addr.String(), "IPPrefixLen": bits,
			"IPv6Gateway": "", "GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0, "MacAddress": e.MAC,
			"DNSNames": append([]string{c.Name, c.ID[:12]}, e.Aliases...),
		}
	}
	return out
}

func mountsJSON(c *Container) []map[string]any {
	out := []map[string]any{}
	for _, m := range c.Mounts {
		j := map[string]any{"Type": m.Type, "Destination": m.Target, "RW": !m.ReadOnly, "Propagation": "", "Source": "", "Mode": ""}
		if m.Type == "volume" {
			j["Name"], j["Driver"] = m.Name, "local"
		}
		out = append(out, j)
	}
	return out
}

// containerJSON is c as inspect shows it, with health its
// State.Health, if it has one.
func containerJSON(c *Container, rc runtime.Container, nets map[string]*Network, health map[string]any) map[string]any {
	st := status(rc)
	state := map[string]any{
		"Status": st, "Running": rc.State == runtime.Running || rc.State == runtime.Paused,
		"Paused": rc.State == runtime.Paused, "Restarting": false, "OOMKilled": false, "Dead": st == "dead",
		"Pid": 0, "ExitCode": 0, "Error": c.Error, "StartedAt": zeroTime(c.StartedAt), "FinishedAt": zeroTime(c.FinishedAt),
	}
	if rc.Exit != nil {
		state["ExitCode"] = exitCode(*rc.Exit)
	}
	if st == "dead" && c.Error == "" {
		state["Error"] = "silo: the container is gone from its runtime"
	}
	if health != nil {
		state["Health"] = health
	}
	cfg := map[string]any{}
	for k, v := range c.Config {
		cfg[k] = v
	}
	cfg["Image"] = c.Image
	cfg["Cmd"] = c.Process.Args
	cfg["Env"] = nonNil(c.Process.Env)
	cfg["WorkingDir"] = c.Process.Workdir
	cfg["User"] = c.Process.User
	cfg["Hostname"] = c.ID[:12]
	cfg["Tty"] = c.Process.TTY
	cfg["OpenStdin"] = c.Process.Stdin
	if c.Healthcheck != nil {
		cfg["Healthcheck"] = c.Healthcheck
	}
	if _, ok := cfg["Labels"]; !ok {
		cfg["Labels"] = map[string]string{}
	}
	ep := c.firstEndpoint()
	ns := map[string]any{"Ports": portsJSON(c, rc.State == runtime.Running), "Networks": networksJSON(c, nets),
		"Bridge": "", "SandboxID": "", "SandboxKey": "", "IPAddress": "", "IPPrefixLen": 0, "Gateway": "", "MacAddress": ""}
	if ep != nil {
		ns["IPAddress"], ns["MacAddress"] = ep.Addr.String(), ep.MAC
		if n := nets[ep.NetworkID]; n != nil {
			ns["IPPrefixLen"], ns["Gateway"] = n.Subnet.Bits(), n.Gateway().String()
		}
	}
	return map[string]any{
		"Id": c.ID, "Created": c.Created.Format(time.RFC3339Nano), "Path": first(c.Process.Args),
		"Args": rest(c.Process.Args), "State": state, "Image": c.ImageID, "Name": "/" + c.Name,
		"RestartCount": c.RestartCount, "Driver": "silo", "Platform": "linux", "MountLabel": "", "ProcessLabel": "",
		"AppArmorProfile": "", "ExecIDs": nil, "HostConfig": c.HostConfig, "Config": cfg,
		"NetworkSettings": ns, "Mounts": mountsJSON(c),
	}
}

func zeroTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.Format(time.RFC3339Nano)
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func rest(s []string) []string {
	if len(s) < 2 {
		return []string{}
	}
	return s[1:]
}

// netsByID are the silo's networks, by id.
func (s *Server) netsByID(ctx context.Context, tx store.Tx, silo string) (map[string]*Network, error) {
	ns, err := s.networks(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	out := map[string]*Network{}
	for _, n := range ns {
		out[n.ID] = n
	}
	return out, nil
}

func (s *Server) containerInspect(q *request) error {
	var c *Container
	var nets map[string]*Network
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if c, err = s.container(q.ctx(), tx, q.target.ID, q.params["id"]); err != nil {
			return err
		}
		nets, err = s.netsByID(q.ctx(), tx, q.target.ID)
		return err
	})
	if err != nil {
		return err
	}
	return ok(q, containerJSON(c, s.state(q.ctx(), c), nets, s.healthJSON(c.ID)))
}

func (s *Server) containerList(q *request) error {
	f, err := filters(q, "label", "name", "id", "status", "network", "ancestor", "volume", "exited", "health")
	if err != nil {
		return err
	}
	var cs []*Container
	var nets map[string]*Network
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		if cs, err = s.containers(q.ctx(), tx, q.target.ID); err != nil {
			return err
		}
		nets, err = s.netsByID(q.ctx(), tx, q.target.ID)
		return err
	})
	if err != nil {
		return err
	}
	all := q.flag("all") || len(f["status"]) > 0 || len(f["exited"]) > 0
	out := []map[string]any{}
	for _, c := range cs {
		rc := s.state(q.ctx(), c)
		st := status(rc)
		health := s.healthStatus(c.ID)
		labels := stringMap(asMap(c.Config["Labels"]))
		if (!all && st != "running" && st != "paused") ||
			!f.labels(labels) ||
			!f.match("name", func(v string) bool { return strings.Contains(c.Name, strings.TrimPrefix(v, "/")) }) ||
			!f.match("id", func(v string) bool { return strings.HasPrefix(c.ID, v) }) ||
			!f.match("status", func(v string) bool { return v == st }) ||
			!f.match("network", func(v string) bool { return c.Networks[v] != nil || hasNetworkID(c, v) }) ||
			!f.match("ancestor", func(v string) bool {
				return v == c.Image || v == c.ImageID || strings.HasPrefix(imageKey(c.ImageID), v)
			}) ||
			!f.match("volume", func(v string) bool { return hasVolume(c, v) }) ||
			!f.match("exited", func(v string) bool { return rc.Exit != nil && strconv.Itoa(exitCode(*rc.Exit)) == v }) ||
			!f.match("health", func(v string) bool { return v == health || (v == "none" && !c.Healthcheck.probes()) }) {
			continue
		}
		var ports []map[string]any
		for _, p := range c.Ports {
			j := map[string]any{"PrivatePort": p.Private, "Type": p.Proto}
			if rc.State == runtime.Running {
				j["PublicPort"], j["IP"] = p.Public, "0.0.0.0"
			}
			ports = append(ports, j)
		}
		out = append(out, map[string]any{
			"Id": c.ID, "Names": []string{"/" + c.Name}, "Image": c.Image, "ImageID": c.ImageID,
			"Command": strings.Join(c.Process.Args, " "), "Created": c.Created.Unix(), "Ports": nonNil(ports),
			"Labels": labels, "State": st, "Status": statusText(c, rc, health),
			"HostConfig":      map[string]any{"NetworkMode": c.HostConfig["NetworkMode"]},
			"NetworkSettings": map[string]any{"Networks": networksJSON(c, nets)}, "Mounts": mountsJSON(c),
		})
	}
	return ok(q, out)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func hasNetworkID(c *Container, id string) bool {
	for _, e := range c.Networks {
		if e.NetworkID == id {
			return true
		}
	}
	return false
}

func hasVolume(c *Container, v string) bool {
	for _, m := range c.Mounts {
		if m.Name == v || m.Target == v {
			return true
		}
	}
	return false
}

// containerLogs writes the workload's output: raw for a TTY, and in
// Docker's multiplexed framing otherwise. Logs are silod's: there are
// no timestamps to give.
func (s *Server) containerLogs(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	if q.flag("timestamps") {
		return invalid("silo: log timestamps are not kept")
	}
	out, errs := q.flag("stdout"), q.flag("stderr")
	if !out && !errs {
		return invalid("You must choose at least one stream")
	}
	st, err := s.drivers[c.Class].Streams(q.ctx(), c.ID)
	if err != nil {
		return err
	}
	if st.Stdin != nil {
		_ = st.Stdin.Close()
	}
	defer discard(st.Stdout)
	defer discard(st.Stderr)
	tail := -1
	if t := q.query("tail"); t != "" && t != "all" {
		if tail, err = strconv.Atoi(t); err != nil {
			return invalid("invalid tail: %s", t)
		}
	}
	read := func(r io.Reader) []byte {
		b, _ := io.ReadAll(r)
		if tail < 0 {
			return b
		}
		lines := bytes.SplitAfter(b, []byte("\n"))
		if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
			lines = lines[:len(lines)-1]
		}
		return bytes.Join(lines[max(0, len(lines)-tail):], nil)
	}
	tty := c.Process.TTY
	if tty {
		q.w.Header().Set("Content-Type", mediaRaw)
	} else {
		q.w.Header().Set("Content-Type", mediaMultiplexed)
	}
	q.w.WriteHeader(http.StatusOK)
	stdout, stderr := outputs(q.w, tty)
	if out {
		_, _ = stdout.Write(read(st.Stdout))
	}
	if errs {
		_, _ = stderr.Write(read(st.Stderr))
	}
	if fl, ok := q.w.(http.Flusher); ok {
		fl.Flush()
	}
	if q.flag("follow") {
		// The output so far is written; follow waits for the
		// container to stop, as docker logs -f does.
		_, _ = s.drivers[c.Class].Wait(q.ctx(), c.ID)
	}
	return nil
}

// containerStats writes the container's use, from its cgroup, in
// Docker's shape: once with stream=false or one-shot, and every second
// until the caller goes away otherwise.
func (s *Server) containerStats(q *request) error {
	c, err := s.get(q)
	if err != nil {
		return err
	}
	d := s.drivers[c.Class]
	stream := q.query("stream") == "" || q.flag("stream")
	if q.flag("one-shot") {
		stream = false
	}
	var prev runtime.Stats
	sample := func() (map[string]any, error) {
		st, err := d.Stats(q.ctx(), c.ID)
		if err != nil {
			return nil, err
		}
		j := map[string]any{
			"read": st.SampledAt.Format(time.RFC3339Nano), "preread": zeroTime(prev.SampledAt),
			"name": "/" + c.Name, "id": c.ID,
			"pids_stats": map[string]any{"current": st.Processes},
			"cpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": st.CPUNanos},
				"online_cpus": (c.VCPUm + 999) / 1000},
			"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": prev.CPUNanos},
				"online_cpus": (c.VCPUm + 999) / 1000},
			"memory_stats": map[string]any{"usage": st.MemoryBytes, "limit": c.MemoryMB << 20},
			"blkio_stats": map[string]any{"io_service_bytes_recursive": []map[string]any{
				{"major": 0, "minor": 0, "op": "read", "value": st.ReadBytes},
				{"major": 0, "minor": 0, "op": "write", "value": st.WriteBytes}}},
			"networks": map[string]any{},
		}
		prev = st
		return j, nil
	}
	j, err := sample()
	if err != nil {
		return err
	}
	q.w.Header().Set("Content-Type", "application/json")
	q.w.WriteHeader(http.StatusOK)
	enc := jsonLines(q.w)
	enc(j)
	for stream {
		select {
		case <-q.ctx().Done():
			return nil
		case <-time.After(time.Second):
		}
		if j, err = sample(); err != nil {
			return nil
		}
		enc(j)
	}
	return nil
}
