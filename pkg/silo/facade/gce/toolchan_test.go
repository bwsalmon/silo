package gce

import (
	"errors"
	"net/netip"
	"strconv"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// toolChannelBinding checks the binding of a tool channel's attach to
// agent id, whose tool VM TestAgentToolVM just made through the facade
// (agents.md, "The tool channel", SV-106): only the tool VM's own
// address, in the agent's own silo, with the GCE instance id silod
// recorded behind it, is the agent's. And the tool server's connect and
// disconnect show on the agent.
func toolChannelBinding(t *testing.T, e *computeEnv, api *fenceapi.Server, fence func(caller, method, path string, body any) (int, obj), id string) {
	t.Helper()
	_, a := fence(e.a, "GET", "/silo/v1/self/agents/fixer", nil)
	vm := a["tool_vm"].(obj)
	ip := mustAddr(t, vm["ip"].(string))
	real, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-agent-fixer")
	if err != nil {
		t.Fatal(err)
	}
	if vm["instance_id"] == "" || vm["instance_id"] != jsonID(real.Id) {
		t.Fatalf("recorded instance id %v, want GCE's %d", vm["instance_id"], real.Id)
	}
	denied := func(what string, silo string, at netip.Addr) {
		t.Helper()
		got, err := api.ToolChannelAgent(ctx, silo, at)
		var fe *fenceapi.Error
		if !errors.As(err, &fe) || fe.Code != fenceapi.PermissionDenied {
			t.Errorf("%s: %q %v, want PERMISSION_DENIED", what, got, err)
		}
	}

	// The right address and instance.
	if got, err := api.ToolChannelAgent(ctx, e.a, ip); err != nil || got != id {
		t.Fatalf("the tool VM's own attach: %q %v, want %s", got, err, id)
	}
	// An address in the silo with no agent's tool VM.
	denied("an address with no agent", e.a, ip.Next())
	// The tool VM's address, but claimed for another silo.
	denied("another silo", e.b, ip)
	// The right address, but not the instance silod made: as if the
	// tool VM had gone and another instance had its address.
	setInstanceID := func(v string) {
		t.Helper()
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agents SET tool_vm = json_set(tool_vm, '$.instance_id', ?) WHERE id = ?`, v, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setInstanceID("1")
	denied("another instance at the address", e.a, ip)
	// No recorded id matches nothing.
	setInstanceID("")
	denied("no recorded id", e.a, ip)
	setInstanceID(jsonID(real.Id))

	// Connect and disconnect show as tool_server.
	if err := api.ToolServerConnected(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, a := fence(e.a, "GET", "/silo/v1/self/agents/fixer", nil); a["tool_vm"].(obj)["tool_server"] != agents.ToolServerConnected {
		t.Errorf("after connecting: %v", a["tool_vm"])
	}
	if err := api.ToolServerDisconnected(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, a := fence(e.a, "GET", "/silo/v1/self/agents/fixer", nil); a["tool_vm"].(obj)["tool_server"] != agents.ToolServerDisconnected {
		t.Errorf("after disconnecting: %v", a["tool_vm"])
	}
}

func jsonID(id uint64) string { return strconv.FormatUint(id, 10) }
