package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Container is a container to create, in the terms of the Docker
// Engine API's create request (only the fields sessiond sets).
type Container struct {
	Name       string            `json:"-"`
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	User       string            `json:"User"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels"`
	// NetworkDisabled is set with network none.
	NetworkDisabled bool       `json:"NetworkDisabled,omitempty"`
	HostConfig      HostConfig `json:"HostConfig"`
}

// HostConfig is the create request's HostConfig.
type HostConfig struct {
	NetworkMode    string            `json:"NetworkMode"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	Privileged     bool              `json:"Privileged"`
	Mounts         []DockerMount     `json:"Mounts"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	CPUShares      int64             `json:"CpuShares,omitempty"`
	PidsLimit      int64             `json:"PidsLimit,omitempty"`
	IpcMode        string            `json:"IpcMode,omitempty"`
	PidMode        string            `json:"PidMode,omitempty"`
}

// DockerMount is a bind mount.
type DockerMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

func limits(s *Spec, h *HostConfig) {
	h.Memory, h.MemorySwap, h.CPUShares, h.PidsLimit = s.MemoryBytes, s.MemoryBytes, s.CPUShares, s.Pids
}

func lockdown(h *HostConfig) {
	h.ReadonlyRootfs = true
	h.CapDrop = []string{"ALL"}
	h.CapAdd = []string{}
	// no-new-privileges, and no seccomp= option: the runtime's default
	// profile applies.
	h.SecurityOpt = []string{"no-new-privileges:true"}
	h.IpcMode = "private"
}

// HarnessContainer is the container one turn of s runs in, image the
// harness image: as s's uid, with no capability, no_new_privs, the
// default seccomp profile and a read-only root, in the network
// namespace of NetName(s.Agent), with s's two mounts and limits.
func HarnessContainer(s *Spec, image string) Container {
	user := strconv.Itoa(s.UID)
	c := Container{
		Name: ContainerName(s.Agent), Image: image, Entrypoint: s.Argv[:1], Cmd: s.Argv[1:], Env: s.Env,
		User: user + ":" + user, WorkingDir: s.WorkDir,
		Labels: map[string]string{LabelSession: s.Agent},
		HostConfig: HostConfig{
			NetworkMode: "container:" + NetName(s.Agent),
			Tmpfs:       map[string]string{"/tmp": fmt.Sprintf("rw,noexec,nosuid,nodev,size=%d", s.TmpBytes)},
		},
	}
	lockdown(&c.HostConfig)
	limits(s, &c.HostConfig)
	for _, m := range s.Mounts {
		c.HostConfig.Mounts = append(c.HostConfig.Mounts, DockerMount{Type: "bind", Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return c
}

// NetContainer holds s's network namespace: network none, so only a
// loopback, as s's uid and as locked down as the harness. It runs
// silod pause.
func NetContainer(s *Spec, image string) Container {
	user := strconv.Itoa(s.UID)
	c := Container{
		Name: NetName(s.Agent), Image: image, Entrypoint: []string{Init}, Cmd: []string{"pause"},
		User: user + ":" + user, Labels: map[string]string{LabelSession: s.Agent}, NetworkDisabled: true,
		HostConfig: HostConfig{NetworkMode: "none"},
	}
	lockdown(&c.HostConfig)
	c.HostConfig.Memory, c.HostConfig.MemorySwap, c.HostConfig.PidsLimit = 16<<20, 16<<20, 4
	return c
}

// FirewallScript is what FirewallContainer runs: drop the metadata
// server, and check that the rule is there.
const FirewallScript = "iptables -w -A OUTPUT -d " + MetadataServer + " -j DROP && " +
	"iptables -w -C OUTPUT -d " + MetadataServer + " -j DROP && " +
	"{ ip6tables -w -A OUTPUT -d " + MetadataServerV6 + " -j DROP 2>/dev/null || true; }"

// FirewallContainer is the one-shot container that puts the metadata
// rule in s's namespace before the harness first runs. It alone has
// NET_ADMIN, and nothing but the rules runs in it; the harness, with
// no capability, can't remove them.
func FirewallContainer(s *Spec, image string) Container {
	c := Container{
		Name: FirewallName(s.Agent), Image: image, Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", FirewallScript},
		User: "0:0", Labels: map[string]string{LabelSession: s.Agent},
		// iptables takes its lock in /run, which the read-only root
		// doesn't give it.
		HostConfig: HostConfig{NetworkMode: "container:" + NetName(s.Agent), Tmpfs: map[string]string{"/run": "rw,noexec,nosuid,nodev,size=1m"}},
	}
	lockdown(&c.HostConfig)
	c.HostConfig.CapAdd = []string{"NET_ADMIN", "NET_RAW"}
	return c
}

// Docker is a client of the Docker Engine API on a Unix socket.
type Docker struct {
	Socket string // /var/run/docker.sock if empty
	hc     *http.Client
}

func (d *Docker) client() *http.Client {
	if d.hc == nil {
		sock := d.Socket
		if sock == "" {
			sock = "/var/run/docker.sock"
		}
		d.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		}}}
	}
	return d.hc
}

func (d *Docker) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.41"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &DockerError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// DockerError is an Engine API error.
type DockerError struct {
	Status  int
	Message string
}

func (e *DockerError) Error() string { return fmt.Sprintf("docker: %d: %s", e.Status, e.Message) }

// Create creates c.
func (d *Docker) Create(ctx context.Context, c Container) error {
	return d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(c.Name), c, nil)
}

// Start starts the container name.
func (d *Docker) Start(ctx context.Context, name string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
}

// Wait waits for name to exit and returns its exit code.
func (d *Docker) Wait(ctx context.Context, name string) (int, error) {
	var out struct{ StatusCode int }
	err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/wait", nil, &out)
	return out.StatusCode, err
}

// Remove removes name, running or not; one that is gone is not an
// error.
func (d *Docker) Remove(ctx context.Context, name string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=1", nil, nil)
	var de *DockerError
	if errors.As(err, &de) && de.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Running reports whether name exists and is running.
func (d *Docker) Running(ctx context.Context, name string) (bool, error) {
	var out struct{ State struct{ Running bool } }
	err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &out)
	var de *DockerError
	if errors.As(err, &de) && de.Status == http.StatusNotFound {
		return false, nil
	}
	return out.State.Running, err
}

// Labelled lists the containers carrying label, by name, with the
// label's value.
func (d *Docker) Labelled(ctx context.Context, label string) (map[string]string, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var list []struct {
		Names  []string
		Labels map[string]string
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(f)), nil, &list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range list {
		for _, n := range c.Names {
			out[strings.TrimPrefix(n, "/")] = c.Labels[label]
		}
	}
	return out, nil
}

// Logs follows name's stdout, a line at a time, until it exits.
// Stderr is dropped: the CLIs' diagnostics aren't events.
func (d *Docker) Logs(ctx context.Context, name string, line func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/v1.41/containers/"+url.PathEscape(name)+"/logs?follow=1&stdout=1&stderr=0", nil)
	if err != nil {
		return err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &DockerError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(demux(resp.Body, pw)) }()
	return scanLines(pr, line)
}

// demux copies the stdout frames of a non-TTY container's stream.
func demux(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		dst := w
		if hdr[0] != 1 {
			dst = io.Discard
		}
		if _, err := io.CopyN(dst, br, n); err != nil {
			return err
		}
	}
}

// maxLine is the longest event line read; a CLI's line can carry a
// whole tool result.
const maxLine = 16 << 20

func scanLines(r io.Reader, line func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		if b := bytes.TrimSpace(sc.Bytes()); len(b) > 0 {
			line(b)
		}
	}
	return sc.Err()
}
