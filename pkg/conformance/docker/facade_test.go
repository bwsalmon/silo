package docker_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// The Docker facade's cases of local-backend.md §13 that aren't the
// registry's: every refusal of §4's Refused, names, ids and 404s across
// silos by every id prefix, and /info showing no host facts. They run
// against every target's Fence; one without a Docker facade skips them
// as not offered.

func eachFence(t *testing.T, fn func(t *testing.T, f conformance.F, a, b string)) {
	conformance.ForEachFence(t, targets.All(), func(t *testing.T, fe conformance.Fence) {
		f := conformance.F{Fence: fe, T: t}
		fn(t, f, f.Mk("", "team-a", 20, nil), f.Mk("", "team-b", 20, nil))
	})
}

// create pulls alpine:3 into silo and creates a container from body
// (Image defaulting to it), returning its id.
func create(f conformance.F, silo, name string, body obj) string {
	f.T.Helper()
	if r := f.DockerDo(silo, "POST", "/v1.47/images/create?fromImage=alpine&tag=3", nil); r.Code != 200 {
		f.T.Fatalf("pull: %d %s", r.Code, r.Body)
	}
	if body == nil {
		body = obj{}
	}
	if body["Image"] == nil {
		body["Image"] = "alpine:3"
	}
	r := f.DockerDo(silo, "POST", "/v1.47/containers/create?name="+name, body)
	if r.Code != 201 && r.Code != 200 {
		f.T.Fatalf("create %s: %d %s", name, r.Code, r.Body)
	}
	var out struct{ Id string }
	json.Unmarshal(r.Body, &out)
	return out.Id
}

// Every refusal of §4's Refused table, each refused before anything is
// made: the silo has no container afterwards.
func TestDockerRefused(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F, a, _ string) {
		create(f, a, "seed", nil)
		hc := func(k string, v any) obj { return obj{"HostConfig": obj{k: v}} }
		for name, body := range map[string]obj{
			"bind of a host path":  hc("Binds", []string{"/etc:/host-etc"}),
			"bind mount":           hc("Mounts", []obj{{"Type": "bind", "Source": "/", "Target": "/host"}}),
			"privileged":           hc("Privileged", true),
			"cap-add":              hc("CapAdd", []string{"NET_ADMIN"}),
			"seccomp unconfined":   hc("SecurityOpt", []string{"seccomp=unconfined"}),
			"apparmor unconfined":  hc("SecurityOpt", []string{"apparmor=unconfined"}),
			"devices":              hc("Devices", []obj{{"PathOnHost": "/dev/kvm", "PathInContainer": "/dev/kvm", "CgroupPermissions": "rwm"}}),
			"device requests":      hc("DeviceRequests", []obj{{"Driver": "nvidia", "Count": -1}}),
			"device cgroup rules":  hc("DeviceCgroupRules", []string{"c 10:232 rwm"}),
			"host network":         hc("NetworkMode", "host"),
			"host pid":             hc("PidMode", "host"),
			"host ipc":             hc("IpcMode", "host"),
			"host uts":             hc("UTSMode", "host"),
			"host userns":          hc("UsernsMode", "host"),
			"host cgroupns":        hc("CgroupnsMode", "host"),
			"another's netns":      hc("NetworkMode", "container:seed"),
			"another's pidns":      hc("PidMode", "container:seed"),
			"sysctls":              hc("Sysctls", obj{"net.ipv4.ip_forward": "1"}),
			"negative oom score":   hc("OomScoreAdj", -500),
			"cpuset":               hc("CpusetCpus", "0"),
			"volume driver":        hc("VolumeDriver", "nfs"),
			"raw host port":        {"ExposedPorts": obj{"80/tcp": obj{}}, "HostConfig": obj{"PortBindings": obj{"80/tcp": []obj{{"HostIp": "0.0.0.0", "HostPort": "8080"}}}}},
			"logging driver":       hc("LogConfig", obj{"Type": "syslog"}),
			"silo's label":         {"Labels": obj{"dev.silo.class": "vm"}},
			"field outside tables": {"NotAField": true},
		} {
			r := f.DockerDo(a, "POST", "/v1.47/containers/create?name=refused", body)
			if r.Code/100 != 4 {
				t.Errorf("%s: %d %s, want refused", name, r.Code, r.Body)
			}
		}
		for _, c := range []struct {
			method, path string
			want         int
		}{
			{"POST", "/v1.47/build", 501},
			{"POST", "/v1.47/images/create?fromSrc=-", 501},
			{"GET", "/v1.47/containers/seed/export", 501},
			{"POST", "/v1.47/containers/seed/update", 501},
			{"GET", "/v1.47/swarm", 503},
			{"GET", "/v1.47/nodes", 503},
			{"GET", "/v1.47/plugins", 501},
			{"GET", "/v1.47/distribution/alpine/json", 501},
		} {
			if r := f.DockerDo(a, c.method, c.path, nil); r.Code != c.want {
				t.Errorf("%s %s: %d %s, want %d", c.method, c.path, r.Code, r.Body, c.want)
			}
		}
		var list []any
		json.Unmarshal(f.DockerDo(a, "GET", "/v1.47/containers/json?all=1", nil).Body, &list)
		if len(list) != 1 {
			t.Errorf("a refused create left %d containers, want only the seed: %v", len(list), list)
		}
	})
}

// Names, ids and 404s across silos: another silo's container is
// answered as one that doesn't exist, by name and by its id and every
// prefix of it down to four characters (shorter, team-b's own ids
// could share it), on inspect, logs, start and delete; and it isn't
// listed.
func TestDockerAcrossSilos(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F, a, b string) {
		id := create(f, a, "secret", nil)
		create(f, b, "mine", nil)
		// A name no container has; team-b's own ids could share a
		// prefix with any hex string.
		const ghost = "no-such-container"
		for _, ref := range []string{"secret", id, id[:12], id[:6], id[:4]} {
			for _, c := range []struct{ method, suffix string }{
				{"GET", "/json"}, {"GET", "/logs?stdout=1"}, {"POST", "/start"}, {"DELETE", "?force=1"},
			} {
				got := f.DockerDo(b, c.method, "/v1.47/containers/"+ref+c.suffix, nil)
				missing := f.DockerDo(b, c.method, "/v1.47/containers/"+ghost+c.suffix, nil)
				if got.Code != 404 {
					t.Errorf("team-b %s %s%s: %d %s, want 404", c.method, ref, c.suffix, got.Code, got.Body)
				}
				// The answer is the one for a reference that matches
				// nothing, the reference itself echoed as Docker does.
				want := strings.ReplaceAll(string(missing.Body), ghost, ref)
				if got.Code != missing.Code || string(got.Body) != want {
					t.Errorf("team-b %s %s%s: %d %s, but %d %s for one that doesn't exist", c.method, ref, c.suffix,
						got.Code, got.Body, missing.Code, missing.Body)
				}
			}
		}
		list := string(f.DockerDo(b, "GET", "/v1.47/containers/json?all=1", nil).Body)
		if strings.Contains(list, id) || strings.Contains(list, "secret") || !strings.Contains(list, "mine") {
			t.Errorf("team-b's list: %s", list)
		}
		// A name is the silo's own: team-b may use team-a's.
		create(f, b, "secret", nil)
		if r := f.DockerDo(a, "GET", "/v1.47/containers/"+id+"/json", nil); r.Code != 200 {
			t.Errorf("team-a's own container after team-b took its name: %d %s", r.Code, r.Body)
		}
	})
}

// /info describes the calling silo, not the host: no hostname, no
// host kernel, its quota as NCPU and MemTotal, its own counts.
func TestDockerInfoHasNoHostFacts(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F, a, b string) {
		create(f, b, "other", nil)
		r := f.DockerDo(a, "GET", "/v1.47/info", nil)
		if r.Code != 200 {
			t.Fatalf("info: %d %s", r.Code, r.Body)
		}
		var info map[string]any
		json.Unmarshal(r.Body, &info)
		if info["OperatingSystem"] != "Silo (local backend)" {
			t.Errorf("OperatingSystem: %v", info["OperatingSystem"])
		}
		if n, _ := info["Containers"].(float64); n != 0 {
			t.Errorf("team-a's info counts %v containers; team-b's aren't its", n)
		}
		// Mk gives a top-level silo 8000m and 8192 MiB.
		if info["NCPU"] != float64(8) || info["MemTotal"] != float64(8192<<20) {
			t.Errorf("NCPU %v, MemTotal %v: want team-a's quota", info["NCPU"], info["MemTotal"])
		}
		host, _ := os.Hostname()
		body := string(r.Body)
		if host != "" && strings.Contains(body, `"`+host+`"`) {
			t.Errorf("info names the host %q: %s", host, body)
		}
		if k, _ := os.ReadFile("/proc/sys/kernel/osrelease"); len(k) > 0 && strings.Contains(body, strings.TrimSpace(string(k))) {
			t.Errorf("info names the host's kernel: %s", body)
		}
	})
}
