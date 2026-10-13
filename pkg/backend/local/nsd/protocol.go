// Package nsd is silo-nsd, the local backend's namespace holder
// (docs/silo/local-rootless-storage.md, calls L27-L29), and silod's
// client for it.
//
// The holder runs in its own user namespace (root inside, mapped to
// silod's user and the user's /etc/subuid range through newuidmap),
// with a private mount namespace and silo-net, the network namespace
// the fence lives in. silod stays in the host's namespaces with no
// capabilities, so that SO_PEERCRED on the front door still reads
// host uids, and asks the holder over a typed RPC on nsd.sock for what
// it can't do itself: netlink and nftables sockets in silo-net,
// listening sockets there, the dial pool, container namespaces, links,
// and volumes whose files belong to subuids. There is no "run this
// command". The namespace work that must happen before the Go runtime
// starts is in package nsexec.
package nsd

import (
	"net/netip"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// Op names one RPC. Each has its own request and response fields;
// nothing else is served.
type Op string

const (
	OpInfo       Op = "info"        // the holder's pid, mapping and storage driver
	OpNetlink    Op = "netlink"     // a netlink socket in silo-net: Proto "route" or "netfilter"
	OpListen     Op = "listen"      // a listening socket in silo-net: Network, Addr
	OpDialPool   Op = "dial_pool"   // Count unconnected TCP sockets in silo-net
	OpContainer  Op = "container"   // create and pin a container's user namespace and netns
	OpDelete     Op = "delete"      // unpin a container's namespaces and delete its veth
	OpBridge     Op = "bridge"      // create a bridge in silo-net with Addrs
	OpVeth       Op = "veth"        // a veth from a bridge into a container's netns
	OpDelLink    Op = "del_link"    // delete a link in silo-net
	OpVolCreate  Op = "vol_create"  // storage.Driver.Create
	OpVolDelete  Op = "vol_delete"  // storage.Driver.Delete
	OpVolUsage   Op = "vol_usage"   // storage.Driver.Usage
	maxDialBatch    = 64
)

// Request is one call. Only the fields its Op names are read.
type Request struct {
	Op Op `json:"op"`

	Proto   string `json:"proto,omitempty"`   // netlink
	Network string `json:"network,omitempty"` // listen: tcp, tcp4, tcp6, udp, udp4, udp6
	Addr    string `json:"addr,omitempty"`    // listen
	Count   int    `json:"count,omitempty"`   // dial_pool

	Container string `json:"container,omitempty"` // container, delete, veth
	// Range is the container range (ContainerRange) its user
	// namespace maps.
	Range uint32 `json:"range,omitempty"`

	Name    string           `json:"name,omitempty"`    // bridge, del_link, veth (host side)
	Bridge  string           `json:"bridge,omitempty"`  // veth
	Addrs   []netip.Prefix   `json:"addrs,omitempty"`   // bridge; veth (container side)
	MAC     string           `json:"mac,omitempty"`     // veth (container side)
	Gateway netip.Addr       `json:"gateway,omitzero"`  // veth: default route in the container
	Volume  *storage.Ref     `json:"volume,omitempty"`  // vol_*
	Size    int64            `json:"size,omitempty"`    // vol_create
}

// Response is a call's answer. Descriptors travel beside it with
// SCM_RIGHTS, in the order the Op documents.
type Response struct {
	Error string `json:"error,omitempty"`

	Info  *Info          `json:"info,omitempty"`
	NS    *ContainerNS   `json:"ns,omitempty"`
	Path  string         `json:"path,omitempty"`  // vol_create
	Usage *storage.Usage `json:"usage,omitempty"` // vol_usage
}

// Info describes the holder.
type Info struct {
	Pid        int                `json:"pid"`
	Mapping    Mapping            `json:"mapping"`
	Driver     string             `json:"driver,omitempty"`
	Guarantees storage.Guarantees `json:"guarantees"`
}

// ContainerNS is where a container's pinned namespaces are, inside the
// holder's mount namespace. The OCI spec names them: runc joins both by
// path; runsc is started inside User and names only Net (call L30).
type ContainerNS struct {
	User  string `json:"user"`
	Net   string `json:"net"`
	Range Range  `json:"range"`
}
