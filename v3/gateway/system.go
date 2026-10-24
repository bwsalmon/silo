package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The gateway reaches the host only through the interfaces in this file, so
// that unit tests run unprivileged with fakes.

// Clock is the gateway's source of time.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Property is one systemd unit property, Key=Value.
type Property struct{ Key, Value string }

func (p Property) String() string { return p.Key + "=" + p.Value }

// UnitSpec describes a transient service to start.
type UnitSpec struct {
	Name        string // silo-agent-<id>.service
	Slice       string // the slice it is placed in
	Description string
	Argv        []string
	Env         map[string]string
	Properties  []Property // User=, sandboxing and limits
}

// UnitStatus is what the gateway needs to know about a unit.
type UnitStatus struct {
	Exists         bool   `json:"exists"`
	ActiveState    string `json:"active_state,omitempty"`
	SubState       string `json:"sub_state,omitempty"`
	Result         string `json:"result,omitempty"`
	MainPID        int    `json:"main_pid,omitempty"`
	ExecMainStatus int    `json:"exec_main_status,omitempty"`
}

// Active reports whether the unit is running or on its way up or down.
func (s UnitStatus) Active() bool {
	switch s.ActiveState {
	case "active", "activating", "reloading", "deactivating":
		return s.Exists
	}
	return false
}

func (s UnitStatus) String() string {
	if !s.Exists {
		return "gone"
	}
	return s.ActiveState + "/" + s.SubState
}

// Systemd manages transient units.
type Systemd interface {
	StartService(ctx context.Context, spec UnitSpec) error
	// SetProperties sets runtime properties on a loaded unit (used for
	// slice limits).
	SetProperties(ctx context.Context, unit string, props []Property) error
	// Stop stops a unit (a slice: everything in it) and waits for it.
	Stop(ctx context.Context, unit string) error
	// Kill sends SIGKILL to every process of a unit.
	Kill(ctx context.Context, unit string) error
	Status(ctx context.Context, unit string) (UnitStatus, error)
	// ResetFailed forgets a failed unit so its name can be reused.
	ResetFailed(ctx context.Context, unit string) error
}

// UserInfo is a Unix user.
type UserInfo struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Home string `json:"home"`
}

// Users looks up and creates Unix users and groups.
type Users interface {
	// Lookup returns nil, nil if the user does not exist.
	Lookup(name string) (*UserInfo, error)
	// UIDTaken reports whether any user has uid.
	UIDTaken(uid int) (bool, error)
	// Create creates a user with its own group, the given uid and a mode-700
	// home, no password and no login shell.
	Create(name string, uid int, home string) (*UserInfo, error)
	// EnsureGroup creates a system group if missing and returns its gid.
	EnsureGroup(name string) (int, error)
}

// Procs reads facts about processes.
type Procs interface {
	// Cgroup returns the cgroup v2 path of pid, e.g.
	// "/silo.slice/silo-agent.slice/silo-agent-x.slice/silo-agent-x.service".
	Cgroup(pid int) (string, error)
}

// ---- real implementations ------------------------------------------------

// Runner runs a command and returns its combined output. It is a variable
// in tests of the real implementations.
type Runner func(ctx context.Context, argv ...string) ([]byte, error)

func execRunner(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv[:min(len(argv), 2)], " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// SystemdRun implements Systemd with systemd-run and systemctl. The design
// calls for D-Bus (StartTransientUnit); that is the same operation, and the
// command-line tools keep the gateway free of a D-Bus dependency until the
// bridge needs fd passing (M1b).
type SystemdRun struct{ Run Runner }

func NewSystemdRun() *SystemdRun { return &SystemdRun{Run: execRunner} }

// systemdRunArgs renders spec as a systemd-run command line.
func systemdRunArgs(spec UnitSpec) []string {
	args := []string{"systemd-run", "--quiet", "--no-ask-password", "--service-type=exec",
		"--unit=" + spec.Name}
	if spec.Slice != "" {
		args = append(args, "--slice="+spec.Slice)
	}
	if spec.Description != "" {
		args = append(args, "--description="+spec.Description)
	}
	for _, p := range spec.Properties {
		args = append(args, "--property="+p.String())
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--setenv="+k+"="+spec.Env[k])
	}
	args = append(args, "--")
	return append(args, spec.Argv...)
}

func (s *SystemdRun) StartService(ctx context.Context, spec UnitSpec) error {
	_, err := s.Run(ctx, systemdRunArgs(spec)...)
	return err
}

func (s *SystemdRun) SetProperties(ctx context.Context, unit string, props []Property) error {
	args := []string{"systemctl", "set-property", "--runtime", "--", unit}
	for _, p := range props {
		args = append(args, p.String())
	}
	_, err := s.Run(ctx, args...)
	return err
}

func (s *SystemdRun) Stop(ctx context.Context, unit string) error {
	_, err := s.Run(ctx, "systemctl", "stop", "--", unit)
	return err
}

func (s *SystemdRun) Kill(ctx context.Context, unit string) error {
	_, err := s.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", "--kill-whom=all", "--", unit)
	return err
}

func (s *SystemdRun) ResetFailed(ctx context.Context, unit string) error {
	_, err := s.Run(ctx, "systemctl", "reset-failed", "--", unit)
	return err
}

func (s *SystemdRun) Status(ctx context.Context, unit string) (UnitStatus, error) {
	out, err := s.Run(ctx, "systemctl", "show", "--property=LoadState,ActiveState,SubState,Result,MainPID,ExecMainStatus", "--", unit)
	if err != nil {
		return UnitStatus{}, err
	}
	return parseShow(out), nil
}

func parseShow(out []byte) UnitStatus {
	var st UnitStatus
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			st.Exists = v != "not-found" && v != ""
		case "ActiveState":
			st.ActiveState = v
		case "SubState":
			st.SubState = v
		case "Result":
			st.Result = v
		case "MainPID":
			st.MainPID, _ = strconv.Atoi(v)
		case "ExecMainStatus":
			st.ExecMainStatus, _ = strconv.Atoi(v)
		}
	}
	return st
}

// ShadowUsers implements Users with os/user and the shadow-utils commands.
type ShadowUsers struct{ Run Runner }

func NewShadowUsers() *ShadowUsers { return &ShadowUsers{Run: execRunner} }

func (u *ShadowUsers) Lookup(name string) (*UserInfo, error) {
	pw, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return toUserInfo(pw)
}

func toUserInfo(pw *user.User) (*UserInfo, error) {
	uid, err := strconv.Atoi(pw.Uid)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(pw.Gid)
	if err != nil {
		return nil, err
	}
	return &UserInfo{Name: pw.Username, UID: uid, GID: gid, Home: pw.HomeDir}, nil
}

func (u *ShadowUsers) UIDTaken(uid int) (bool, error) {
	_, err := user.LookupId(strconv.Itoa(uid))
	var unknown user.UnknownUserIdError
	if errors.As(err, &unknown) {
		return false, nil
	}
	return err == nil, err
}

func (u *ShadowUsers) Create(name string, uid int, home string) (*UserInfo, error) {
	ctx := context.Background()
	if _, err := u.Run(ctx, "useradd", "--system", "--uid", strconv.Itoa(uid), "--user-group",
		"--home-dir", home, "--create-home", "--shell", "/usr/sbin/nologin",
		"--comment", "silo agent", "--", name); err != nil {
		return nil, err
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return nil, err
	}
	return u.Lookup(name)
}

func (u *ShadowUsers) EnsureGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		if _, err := u.Run(context.Background(), "groupadd", "--system", "--", name); err != nil {
			return 0, err
		}
		g, err = user.LookupGroup(name)
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// ProcFS implements Procs by reading /proc.
type ProcFS struct{ Root string }

func (p ProcFS) Cgroup(pid int) (string, error) {
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", root, pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path, nil
		}
	}
	return "", fmt.Errorf("pid %d: no cgroup v2 entry", pid)
}
