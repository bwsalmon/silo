package docker

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/tree"
)

// An agent's tool container is made through the facade as the caller,
// in the class asked or the silo's default, runs silo-tools (silod's
// own, mounted, in the sandbox class), and is the agent's alone to
// change.
func TestToolContainers(t *testing.T) {
	e := newEnv(t)
	tc := e.facade.ToolContainers()
	caller := fenceapi.Caller{Silo: e.a, Via: fenceapi.ViaInternal}
	silo := &tree.Silo{ID: e.a}

	for _, bad := range []fenceapi.ToolVMSpec{{Subnet: "work"}, {Zone: "us-central1-a"}, {MachineType: "e2-small"}, {Isolation: "kvm"}} {
		if err := tc.CheckToolSpec(&bad); err == nil {
			t.Errorf("CheckToolSpec(%+v) passed", bad)
		}
	}
	spec := fenceapi.ToolVMSpec{Image: "alpine", Isolation: "sandbox"}
	if err := tc.CheckToolSpec(&spec); err != nil || spec.Zone != ToolZone {
		t.Fatalf("CheckToolSpec: %+v %v", spec, err)
	}

	e.pull(e.a, "alpine")
	ip, err := tc.Create(ctx, caller, silo, "agent1", "agent-fixer", spec, "")
	if err != nil {
		t.Fatal(err)
	}
	id, at, err := tc.Identify(ctx, caller, silo, "agent1", ToolZone, "agent-fixer")
	if err != nil || ip == "" || at != ip {
		t.Fatalf("identify: %s %s (made at %s) %v", id, at, ip, err)
	}
	rs, ok := e.sbx.Spec(id)
	if !ok {
		t.Fatal("not in the sandbox class")
	}
	if !rs.SiloTools || rs.Labels[runtime.LabelAgent] != "agent1" || len(rs.Process.Args) == 0 || rs.Process.Args[0] != runtime.ToolsPath {
		t.Errorf("tool container spec: tools %v, labels %v, args %q", rs.SiloTools, rs.Labels, rs.Process.Args)
	}
	if !slices.Contains(rs.Process.Args, "--name-prefix=-") {
		t.Errorf("silo-tools args %q", rs.Process.Args)
	}
	if in := e.ok(e.a, "GET", "/v1.47/containers/agent-fixer/json", nil); in["State"].(obj)["Running"] != true {
		t.Errorf("not started: %v", in["State"])
	}

	// The silo's own writes to it are refused, naming the agent; prune
	// passes it by.
	e.fail(403, "is the tool container of agent", e.a, "POST", "/v1.47/containers/agent-fixer/stop", nil)
	e.fail(403, "is the tool container of agent", e.a, "DELETE", "/v1.47/containers/agent-fixer?force=1", nil)
	if _, err := tc.Create(ctx, caller, silo, "agent2", "agent-other", fenceapi.ToolVMSpec{Isolation: "vm"}, ""); err == nil ||
		!strings.Contains(err.Error(), "tool_vm.image is required") {
		t.Errorf("a vm-class tool container with no tool image: %v", err)
	}

	if err := tc.Delete(ctx, caller, silo, "agent1", ToolZone, "agent-fixer"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.sbx.Spec(id); ok {
		t.Error("the tool container stayed")
	}
	if err := tc.Delete(ctx, caller, silo, "agent1", ToolZone, "agent-fixer"); err != nil {
		t.Errorf("deleting one that is gone: %v", err)
	}
	var fe *fenceapi.Error
	if _, _, err := tc.Identify(ctx, caller, silo, "agent1", ToolZone, "agent-fixer"); !errors.As(err, &fe) || fe.Code != fenceapi.NotFound {
		t.Errorf("identify of one that is gone: %v", err)
	}

	// In the vm class the image carries silo-tools: nothing is mounted.
	e.facade.opts.VMToolImage = "alpine"
	if _, err := tc.Create(ctx, caller, silo, "agent3", "agent-vm", fenceapi.ToolVMSpec{Isolation: "vm"}, ""); err != nil {
		t.Fatal(err)
	}
	id, _, _ = tc.Identify(ctx, caller, silo, "agent3", ToolZone, "agent-vm")
	if rs, ok := e.vm.Spec(id); !ok || rs.SiloTools || rs.Labels[runtime.LabelAgent] != "agent3" {
		t.Errorf("vm tool container: %v %+v", ok, rs)
	}
}
