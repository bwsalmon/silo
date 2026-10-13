package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
)

func TestTable(t *testing.T) {
	tb := Table[string]{
		Resources: map[string]bool{"disks": true},
		Refused:   map[string]bool{"routes": true},
		Methods:   map[string]string{"disks.get": "get", "routes.list": "list"},
	}
	for res, want := range map[string]bool{"disks": true, "routes": true, "widgets": false} {
		if tb.Known(res) != want {
			t.Errorf("Known(%s) = %v", res, !want)
		}
	}
	if h, ok := tb.Lookup("disks", "disks.get"); !ok || h != "get" {
		t.Errorf("disks.get = %q, %v", h, ok)
	}
	if _, ok := tb.Lookup("disks", "disks.delete"); ok {
		t.Error("disks.delete served")
	}
	if _, ok := tb.Lookup("routes", "routes.list"); ok {
		t.Error("a refused resource's method served")
	}
}

func TestFields(t *testing.T) {
	tr := AllowFields([]string{"name", "labels", "networkInterfaces[].subnetwork", "metadata.items"})
	var body any
	json.Unmarshal([]byte(`{"name": "x", "labels": {"a": "b"}, "networkInterfaces": [{"subnetwork": "s", "accessConfigs": []}],
		"canIpForward": false}`), &body)
	if err := CheckFields(body, tr); err != nil {
		t.Errorf("CheckFields = %v", err)
	}
	json.Unmarshal([]byte(`{"networkInterfaces": [{"subnetwork": "s"}, {"accessConfigs": [{"type": "ONE_TO_ONE_NAT"}]}]}`), &body)
	var fd *FieldDenied
	if err := CheckFields(body, tr); !errors.As(err, &fd) || fd.Path != "networkInterfaces[1].accessConfigs" {
		t.Errorf("CheckFields = %v, want networkInterfaces[1].accessConfigs refused", err)
	}
	var r Refusal = fd
	_ = r

	json.Unmarshal([]byte(`{"name": "x", "id": "1", "metadata": {"items": [1], "fingerprint": "f"},
		"networkInterfaces": [{"subnetwork": "s", "networkIP": "10.0.0.2"}]}`), &body)
	if got := fmt.Sprint(tr.Prune(body)); got != "map[metadata:map[items:[1]] name:x networkInterfaces:[map[subnetwork:s]]]" {
		t.Errorf("Prune = %s", got)
	}
}

func TestIsDefault(t *testing.T) {
	for _, v := range []any{nil, false, 0.0, "", []any{}, map[string]any{"a": false, "b": map[string]any{}}} {
		if !IsDefault(v) {
			t.Errorf("IsDefault(%#v) = false", v)
		}
	}
	for _, v := range []any{true, 1.0, "x", []any{nil}, map[string]any{"a": "x"}} {
		if IsDefault(v) {
			t.Errorf("IsDefault(%#v) = true", v)
		}
	}
}

func TestVia(t *testing.T) {
	for _, c := range []struct {
		caller fenceapi.Caller
		want   string
	}{
		{fenceapi.Caller{Silo: "a"}, ledger.ViaFacade},
		{fenceapi.Caller{Silo: "p"}, "ancestor:p"},
		{fenceapi.Caller{Silo: "h", Manage: "g1"}, "manage:h"},
	} {
		if got := Via(c.caller, "a"); got != c.want {
			t.Errorf("Via(%+v) = %s, want %s", c.caller, got, c.want)
		}
	}
}
