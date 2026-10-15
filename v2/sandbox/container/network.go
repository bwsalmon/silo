package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/v2/sandbox"
)

// networkName is the session's private network.
func networkName(session string) string { return "silo-" + session }

// CreateNetwork implements sandbox.Networker: an internal bridge
// network (--internal), labelled with the session, whose sandboxes
// reach the invoking machine on the bridge's gateway address and have
// no route anywhere else (design §9.3). A rootless Podman network's
// gateway is inside Podman's own network namespace, not on the host, so
// nothing silo listens on can be reached there; the caller finds that
// out when it cannot listen on HostIP.
func (b *Backend) CreateNetwork(ctx context.Context, session string) (sandbox.Network, error) {
	if session == "" {
		return sandbox.Network{}, fmt.Errorf("a network needs a session")
	}
	name := networkName(session)
	if _, err := b.cli(ctx, nil, nil, "network", "create", "--internal",
		"--label", sandbox.SessionLabel+"="+session,
		"--label", sandbox.UserLabel+"="+strconv.Itoa(os.Getuid()),
		name); err != nil {
		return sandbox.Network{}, fmt.Errorf("creating the session's network: %w", err)
	}
	ip, err := b.networkGateway(ctx, name)
	if err != nil {
		_ = b.RemoveNetwork(context.WithoutCancel(ctx), name)
		return sandbox.Network{}, err
	}
	return sandbox.Network{Name: name, HostIP: ip}, nil
}

// networkGateway reads a network's gateway address from network inspect,
// in Docker's shape (IPAM.Config[].Gateway) or Podman's
// (subnets[].gateway).
func (b *Backend) networkGateway(ctx context.Context, name string) (string, error) {
	out, err := b.cli(ctx, nil, nil, "network", "inspect", name)
	if err != nil {
		return "", err
	}
	var nets []struct {
		IPAM struct {
			Config []struct {
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
		Subnets []struct {
			Gateway string `json:"gateway"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal(out, &nets); err != nil {
		return "", fmt.Errorf("network inspect %s: %w", name, err)
	}
	var gws []string
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			gws = append(gws, c.Gateway)
		}
		for _, s := range n.Subnets {
			gws = append(gws, s.Gateway)
		}
	}
	for _, gw := range gws {
		if ip := net.ParseIP(gw); ip != nil && ip.To4() != nil {
			return gw, nil
		}
	}
	return "", fmt.Errorf("network %s has no IPv4 gateway address", name)
}

// RemoveNetwork implements sandbox.Networker.
func (b *Backend) RemoveNetwork(ctx context.Context, name string) error {
	if _, err := b.cli(ctx, nil, nil, "network", "rm", name); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(err.Error(), "no such network") {
			return nil
		}
		return err
	}
	return nil
}
