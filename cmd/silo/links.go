package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/pkg/client"
)

// The link commands (fence-api.md, "Links (L3)", SV-15). Each acts as
// the caller's own silo, or with --silo as a descendant's on its behalf
// (SV-16).

func (c *cli) linkPropose(ctx context.Context, args []string) error {
	fs := c.flags("link propose")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the proposing silo: the caller's own, or a descendant's, on its behalf")
	peer := fs.String("peer", "", "the other silo's path")
	direction := fs.String("direction", "", "outbound (connections start here), inbound, or both")
	rules := fs.String("rules", "", "comma-separated protocol[:port[+port...]] rules: tcp:5432, udp:53, tcp:8000-8100+22, icmp, all")
	pos, err := c.parse(fs, args, 1, 1, "<name> --peer /silo --direction outbound|inbound|both --rules tcp:5432,...")
	if err != nil {
		return err
	}
	if *peer == "" || *direction == "" || *rules == "" {
		return usagef("--peer, --direction and --rules are required")
	}
	req := client.LinkRequest{Name: pos[0], Peer: *peer, Direction: *direction}
	for _, r := range parseList(*rules) {
		proto, ports, _ := strings.Cut(r, ":")
		lr := client.LinkRule{Protocol: proto}
		if ports != "" {
			lr.Ports = strings.Split(ports, "+")
		}
		req.Rules = append(req.Rules, lr)
	}
	l, err := c.api.ProposeLink(ctx, *silo, req, *m)
	if err != nil {
		return err
	}
	return c.print(l, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be proposed; nothing changed\n", pos[0])
			return
		}
		if l.State == "PENDING" {
			fmt.Fprintf(w, "proposed %s to %s; it is PENDING until %s proposes the mirror image (--direction %s)\n",
				l.Name, l.Peer, l.Peer, mirror(l.Direction))
		} else {
			fmt.Fprintf(w, "linked %s with %s: %s\n", l.Name, l.Peer, l.State)
		}
		writeLinks(w, []*client.Link{l})
	})
}

func mirror(d string) string {
	switch d {
	case "outbound":
		return "inbound"
	case "inbound":
		return "outbound"
	}
	return d
}

func (c *cli) linkList(ctx context.Context, args []string) error {
	fs := c.flags("link list")
	silo := fs.String("silo", "self", "whose links")
	if _, err := c.parse(fs, args, 0, 0, "[--silo s]"); err != nil {
		return err
	}
	ls, err := c.api.Links(ctx, *silo)
	if err != nil {
		return err
	}
	if ls == nil {
		ls = []*client.Link{}
	}
	return c.print(map[string]any{"links": ls}, func(w *tw) { writeLinks(w, ls) })
}

func (c *cli) linkDelete(ctx context.Context, args []string) error {
	fs := c.flags("link delete")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the silo whose side it is")
	pos, err := c.parse(fs, args, 1, 1, "<name>")
	if err != nil {
		return err
	}
	l, err := c.api.DeleteLink(ctx, *silo, pos[0], *m)
	if err != nil {
		return err
	}
	return c.print(l, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be deleted; nothing changed\n", pos[0])
			return
		}
		fmt.Fprintf(w, "deleted the link %s with %s\n", l.Name, l.Peer)
	})
}

func writeLinks(w *tw, ls []*client.Link) {
	if len(ls) == 0 {
		fmt.Fprintln(w, "no links")
		return
	}
	w.row("NAME", "PEER", "DIRECTION", "RULES", "STATE", "ID")
	for _, l := range ls {
		var rs []string
		for _, r := range l.Rules {
			s := r.Protocol
			if len(r.Ports) > 0 {
				s += ":" + strings.Join(r.Ports, "+")
			}
			rs = append(rs, s)
		}
		w.row(l.Name, l.Peer, l.Direction, strings.Join(rs, ","), l.State, dash(l.ID))
	}
}
