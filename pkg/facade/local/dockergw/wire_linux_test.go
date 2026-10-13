package dockergw

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
)

// holder records the wiring asked of it.
type holder struct {
	calls    []string
	failVeth bool
}

func (h *holder) Container(id string, n uint32) (*nsd.ContainerNS, error) {
	h.calls = append(h.calls, fmt.Sprintf("container %s %d", id[:4], n))
	return &nsd.ContainerNS{User: "/pins/" + id + "/user", Net: "/pins/" + id + "/net", Range: nsd.Range{Outside: 65537, Size: 65536}}, nil
}

func (h *holder) Bridge(name string, addrs ...netip.Prefix) error {
	h.calls = append(h.calls, fmt.Sprintf("bridge %s %v", name, addrs))
	return nil
}

func (h *holder) Veth(v nsd.Veth) error {
	if h.failVeth && v.Peer == "eth1" {
		return errors.New("no room")
	}
	h.calls = append(h.calls, fmt.Sprintf("veth %s on %s as %s %v %s gw=%v", v.Name, v.Bridge, v.Peer, v.Addrs, v.MAC, v.Gateway))
	return nil
}

func (h *holder) DelLink(name string) error {
	h.calls = append(h.calls, "del "+name)
	return nil
}

func (h *holder) DeleteContainer(id, veth string) error {
	h.calls = append(h.calls, fmt.Sprintf("delete %s %s", id[:4], veth))
	return nil
}

// TestWiring: a bridge per network and a veth per endpoint, pinned to
// its MAC and address, the first with the default route; the spec names
// the namespaces; and a failure undoes it all.
func TestWiring(t *testing.T) {
	id := strings.Repeat("c", 64)
	n1, n2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	nets := map[string]*docker.Network{
		n1: {ID: n1, Name: "front", Subnet: netip.MustParsePrefix("10.1.0.0/24")},
		n2: {ID: n2, Name: "back", Subnet: netip.MustParsePrefix("10.1.1.0/24")},
	}
	c := &docker.Container{ID: id, HostConfig: map[string]any{"NetworkMode": "front"}, Networks: map[string]*docker.Endpoint{
		"front": {NetworkID: n1, Addr: netip.MustParseAddr("10.1.0.2"), MAC: "02:42:0a:01:00:02"},
		"back":  {NetworkID: n2, Addr: netip.MustParseAddr("10.1.1.2"), MAC: "02:42:0a:01:01:02"},
	}}
	h := &holder{}
	w := &Wiring{Holder: h, Range: func(string) uint32 { return 3 }}
	spec := runtime.Spec{ID: id, Silo: "s1"}
	if err := w.Wire(ctx, c, nets, &spec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"container cccc 3",
		"bridge sb-111111111111 [10.1.0.1/24]",
		"veth svccccccc111111 on sb-111111111111 as eth0 [10.1.0.2/24] 02:42:0a:01:00:02 gw=10.1.0.1",
		"bridge sb-222222222222 [10.1.1.1/24]",
		"veth svccccccc222222 on sb-222222222222 as eth1 [10.1.1.2/24] 02:42:0a:01:01:02 gw=invalid IP",
	}
	if fmt.Sprint(h.calls) != fmt.Sprint(want) {
		t.Errorf("wiring:\n%s\nwant\n%s", strings.Join(h.calls, "\n"), strings.Join(want, "\n"))
	}
	if spec.NetNS != "/pins/"+id+"/net" || spec.UserNS != "/pins/"+id+"/user" || spec.Root.Outside != 65537 {
		t.Errorf("spec: %+v", spec)
	}
	h.calls = nil
	if err := w.Unwire(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(h.calls) != "[del svccccccc222222 delete cccc svccccccc111111]" {
		t.Errorf("unwiring: %v", h.calls)
	}
	h.calls, h.failVeth = nil, true
	if err := w.Wire(ctx, c, nets, &runtime.Spec{ID: id}); err == nil {
		t.Fatal("a veth that failed wired the container")
	}
	if last := h.calls[len(h.calls)-1]; last != "delete cccc svccccccc111111" {
		t.Errorf("after a failure: %v", h.calls)
	}
}
