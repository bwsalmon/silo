package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/gvisor"
)

// silod's sandbox driver finds the shim's options beside the holder's
// state, where `silod containerd` writes them, runs `silod runsc` with
// the runsc on its PATH, and puts each container in its silo's slice.
func TestSandboxDriverConfig(t *testing.T) {
	dir := t.TempDir()
	c := sandboxConfig{Exe: "/usr/bin/silod", State: filepath.Join(dir, "rt", "nsd.state"), Containerd: filepath.Join(dir, "rt", "containerd.sock"),
		Dir: filepath.Join(dir, "cache", "containers"), Volumes: filepath.Join(dir, "silos"), Runsc: "/usr/bin/runsc", Toolbox: "/usr/bin/busybox",
		SiloPaths: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"child": {"root", "child"}}, nil
		}}
	cfg := c.gvisor()
	if cfg.Options != filepath.Join(dir, "rt", "runsc.toml") || cfg.Address != c.Containerd || cfg.Dir != c.Dir || cfg.Volumes != c.Volumes || cfg.Toolbox != c.Toolbox {
		t.Errorf("config: %+v", cfg)
	}
	if !slices.Equal(cfg.Logger, []string{"/usr/bin/silod", gvisor.LogArg}) || !slices.Equal(cfg.Runsc, []string{"runsc", "--runsc", "/usr/bin/runsc"}) {
		t.Errorf("logger %v, runsc %v", cfg.Logger, cfg.Runsc)
	}
	if got := cfg.CgroupsPath(runtime.Spec{ID: "c1", Silo: "child"}); got != "silo-root-child.slice:silo:c1" {
		t.Errorf("cgroupsPath: %s", got)
	}
	if got := cfg.CgroupsPath(runtime.Spec{ID: "c2", Silo: "gone"}); got != "silo-gone.slice:silo:c2" {
		t.Errorf("cgroupsPath of a silo the store doesn't list: %s", got)
	}
	// With no holder, the command joins pid 0, which fails, rather than
	// a pid the state once named. nsexec execs the arguments after the
	// first: silod again, inside.
	cmd := cfg.HolderCmd("runsc", "--version")
	if !slices.Contains(cmd.Env, "_SILO_NSEXEC_PID=0") || !slices.Equal(cmd.Args, []string{"/usr/bin/silod", "/usr/bin/silod", "runsc", "--version"}) {
		t.Errorf("holder command: %v %v", cmd.Args, cmd.Env[len(cmd.Env)-3:])
	}
	if _, err := cfg.InHolder(context.Background(), "true"); err == nil {
		t.Errorf("a command ran in no holder")
	}
	c.Runsc, c.SiloPaths = "", nil
	if cfg := c.gvisor(); cfg.Runsc != nil || cfg.CgroupsPath != nil {
		t.Errorf("without runsc or silo paths: %v, %v", cfg.Runsc, cfg.CgroupsPath != nil)
	}

	// With no silo-containerd, the class is there and its probe says why
	// it can't run.
	d, err := sandboxDriver(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.(interface{ Close() error }).Close()
	if pr := d.Probe(context.Background()); pr.OK || !strings.Contains(pr.Why, "silo-containerd doesn't answer") {
		t.Errorf("probe: %+v", pr)
	}
}
