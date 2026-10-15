package compose_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

type obj = map[string]any

// classes are the isolation classes every flow runs in (call L11).
var classes = []string{"sandbox", "vm"}

// env is one silo of a target's install, making containers in one
// class through its Docker facade, as compose and testcontainers do
// with DOCKER_HOST=tcp://docker.silo.internal:2375 inside the silo.
type env struct {
	conformance.F
	silo, class string
}

// eachClass runs fn per target and per class, in a fresh silo team-a
// of the target's install.
func eachClass(t *testing.T, fn func(t *testing.T, e env)) {
	conformance.ForEachFence(t, targets.Pulling(), func(t *testing.T, fe conformance.Fence) {
		for _, class := range classes {
			t.Run(class, func(t *testing.T) {
				f := conformance.F{Fence: fe, T: t}
				fn(t, env{F: f, silo: f.Mk("", "team-"+class, 20, nil), class: class})
			})
		}
	})
}

// simulated skips t on a target whose containers don't run their
// images.
func (e env) simulated(t *testing.T) {
	t.Helper()
	if s, ok := e.Fence.(conformance.Simulated); ok {
		t.Skipf("not offered: %s", s.Simulated())
	}
}

// do is a Docker API call that must answer 2xx; its body is decoded
// into out when out isn't nil.
func (e env) do(method, path string, body any, out any) {
	e.T.Helper()
	r := e.DockerDo(e.silo, method, "/v1.47"+path, body)
	if r.Code/100 != 2 {
		e.T.Fatalf("%s %s: %d %s", method, path, r.Code, r.Body)
	}
	if out != nil {
		if err := json.Unmarshal(r.Body, out); err != nil {
			e.T.Fatalf("%s %s: %v: %s", method, path, err, r.Body)
		}
	}
}

// pull pulls ref, as both tools do before a create.
func (e env) pull(ref string) {
	e.T.Helper()
	name, tag, _ := strings.Cut(ref, ":")
	e.do("POST", "/images/create?fromImage="+url.QueryEscape(name)+"&tag="+tag, nil, nil)
}

// create creates container name from body in the env's class, pulling
// its image first, and returns its id.
func (e env) create(name string, body obj) string {
	e.T.Helper()
	e.pull(body["Image"].(string))
	hc, _ := body["HostConfig"].(obj)
	if hc == nil {
		hc = obj{}
	}
	hc["Runtime"] = e.class
	body["HostConfig"] = hc
	var out struct{ Id string }
	e.do("POST", "/containers/create?name="+url.QueryEscape(name), body, &out)
	return out.Id
}

// script is a container's command: sh -c script.
func script(s string) []string { return []string{"sh", "-c", s} }

type inspected struct {
	Name  string
	State struct {
		Status   string
		Running  bool
		ExitCode int
		Health   *struct {
			Status        string
			FailingStreak int
			Log           []struct {
				ExitCode int
				Output   string
			}
		}
	}
	NetworkSettings struct {
		Ports    map[string][]struct{ HostIp, HostPort string }
		Networks map[string]struct {
			IPAddress string
			Aliases   []string
		}
	}
}

func (e env) inspect(id string) inspected {
	e.T.Helper()
	var in inspected
	e.do("GET", "/containers/"+id+"/json", nil, &in)
	return in
}

// eventually polls cond until it holds or d has passed.
func eventually(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// demux splits a multiplexed stream (no TTY) into stdout and stderr.
func demux(b []byte) (stdout, stderr string) {
	var o, e strings.Builder
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if len(b) < 8+n {
			n = len(b) - 8
		}
		if b[0] == 2 {
			e.Write(b[8 : 8+n])
		} else {
			o.Write(b[8 : 8+n])
		}
		b = b[8+n:]
	}
	return o.String(), e.String()
}

// exec runs cmd in id as compose exec and testcontainers' ExecInContainer
// do: create the exec, start it attached, read its output to the end,
// then inspect it for the exit code.
func (e env) exec(id string, cmd ...string) (code int, stdout, stderr string) {
	e.T.Helper()
	var ex struct{ Id string }
	e.do("POST", "/containers/"+id+"/exec", obj{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true}, &ex)
	r := e.DockerDo(e.silo, "POST", "/v1.47/exec/"+ex.Id+"/start", obj{"Detach": false, "Tty": false})
	if r.Code != 200 {
		e.T.Fatalf("exec start %v: %d %s", cmd, r.Code, r.Body)
	}
	stdout, stderr = demux(r.Body)
	var in struct {
		Running  bool
		ExitCode int
	}
	eventually(5*time.Second, func() bool {
		e.do("GET", "/exec/"+ex.Id+"/json", nil, &in)
		return !in.Running
	})
	return in.ExitCode, stdout, stderr
}

// logs is id's output so far.
func (e env) logs(id string) (stdout, stderr string) {
	e.T.Helper()
	r := e.DockerDo(e.silo, "GET", "/v1.47/containers/"+id+"/logs?stdout=1&stderr=1", nil)
	if r.Code != 200 {
		e.T.Fatalf("logs: %d %s", r.Code, r.Body)
	}
	return demux(r.Body)
}

// waitLog waits for want in id's output, as testcontainers' log wait
// strategy polls for it: up to two minutes, which a real module's
// image (Kafka's) needs.
func (e env) waitLog(id, want string) {
	e.T.Helper()
	var out, errOut string
	if !eventually(2*time.Minute, func() bool {
		out, errOut = e.logs(id)
		return strings.Contains(out+errOut, want)
	}) {
		e.T.Fatalf("%s's output never had %q: stdout %q, stderr %q", id, want, out, errOut)
	}
}

// follow reads a streamed call (logs?follow=1, attach?stream=1) until
// its demultiplexed output holds want, as compose logs -f and compose up
// in the foreground show it, and reports whether it did within 10s.
func (e env) follow(method, path, want string) (bool, string) {
	e.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, nil)
	res, err := e.Docker(e.silo, req)
	if err != nil {
		e.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		e.T.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	var got bytes.Buffer
	br := bufio.NewReader(res.Body)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return false, got.String()
		}
		if _, err := io.CopyN(&got, br, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			return false, got.String()
		}
		if strings.Contains(got.String(), want) {
			return true, got.String()
		}
	}
}

// tarOf is a tar of regular files, name to content.
func tarOf(files map[string]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	tw.Close()
	return b.Bytes()
}

// putArchive is docker cp into id at dir, as testcontainers'
// copyFileToContainer sends it.
func (e env) putArchive(id, dir string, archive []byte) {
	e.T.Helper()
	req, _ := http.NewRequest("PUT", "http://docker/v1.47/containers/"+id+"/archive?path="+url.QueryEscape(dir), bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := e.Docker(e.silo, req)
	if err != nil {
		e.T.Fatalf("put archive: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		e.T.Fatalf("put archive: %d %s", res.StatusCode, b)
	}
}

// listed is the silo's containers whose label filter matches.
func (e env) listed(label string) []struct {
	Id, State string
	Names     []string
	Labels    map[string]string
} {
	e.T.Helper()
	f, _ := json.Marshal(obj{"label": []string{label}})
	var out []struct {
		Id, State string
		Names     []string
		Labels    map[string]string
	}
	e.do("GET", "/containers/json?all=1&filters="+url.QueryEscape(string(f)), nil, &out)
	return out
}
