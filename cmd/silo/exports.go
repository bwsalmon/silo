package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/pkg/client"
)

// The export and import commands (fence-api.md, "Exports (proxied)",
// SV-14). Each acts as the caller's own silo, or with --silo as a
// descendant's on its behalf (SV-16).

func (c *cli) exportOffer(ctx context.Context, args []string) error {
	fs := c.flags("export offer")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the exporting silo: the caller's own, or a descendant's, on its behalf")
	target := fs.String("target", "", "the target, ip:port, in the exporting silo's own space")
	instance := fs.String("target-instance", "", "the target instance, instead of --target: its primary IP, followed if it changes")
	zone := fs.String("target-zone", "", "the target instance's zone")
	port := fs.Int("target-port", 0, "the target instance's port")
	protocol := fs.String("protocol", "http", "http, tcp or registry")
	repos := fs.String("repos", "", "registry: comma-separated image repos of the exporting silo, by name or a prefix ending in *")
	to := fs.String("to", "", "comma-separated paths of the silos it is offered to")
	proxyProtocol := fs.Bool("proxy-protocol", false, "send a PROXY protocol v2 header naming the caller (tcp only)")
	pos, err := c.parse(fs, args, 1, 1, "<name> --target ip:port|--target-instance i --target-zone z --target-port p --to /silo,... [--protocol http|tcp] | --protocol registry --repos r,p* --to /silo,...")
	if err != nil {
		return err
	}
	req := client.OfferRequest{Name: pos[0], Protocol: *protocol, To: parseList(*to), ProxyProtocol: *proxyProtocol, Repos: parseList(*repos)}
	switch {
	case *protocol == "registry":
		if *target != "" || *instance != "" {
			return usagef("a registry export has no target; --repos names what it exports")
		}
		if len(req.Repos) == 0 {
			return usagef("--repos is required for a registry export")
		}
	case *target != "" && *instance != "":
		return usagef("give --target or --target-instance, not both")
	case *target != "":
		req.Target = *target
	case *instance != "":
		if *zone == "" || *port == 0 {
			return usagef("--target-instance needs --target-zone and --target-port")
		}
		req.Target = client.ExportTarget{Instance: *instance, Zone: *zone, Port: *port}
	default:
		return usagef("--target ip:port or --target-instance is required")
	}
	if len(req.To) == 0 {
		return usagef("--to is required: the silos it is offered to")
	}
	e, err := c.api.OfferExport(ctx, *silo, req, *m)
	if err != nil {
		return err
	}
	return c.print(e, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be offered; nothing changed\n", pos[0])
			return
		}
		fmt.Fprintf(w, "offered %s (%s) to %s\n", e.Name, e.Protocol, strings.Join(req.To, ", "))
		writeExports(w, []*client.Export{e})
	})
}

func (c *cli) exportList(ctx context.Context, args []string) error {
	fs := c.flags("export list")
	silo := fs.String("silo", "self", "whose exports")
	if _, err := c.parse(fs, args, 0, 0, "[--silo s]"); err != nil {
		return err
	}
	es, err := c.api.Exports(ctx, *silo)
	if err != nil {
		return err
	}
	if es == nil {
		es = []*client.Export{}
	}
	return c.print(map[string]any{"exports": es}, func(w *tw) { writeExports(w, es) })
}

func (c *cli) exportWithdraw(ctx context.Context, args []string) error {
	fs := c.flags("export withdraw")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the exporting silo")
	to := fs.String("to", "", "withdraw it from this silo alone (default: from everyone)")
	pos, err := c.parse(fs, args, 1, 1, "<name> [--to /silo]")
	if err != nil {
		return err
	}
	e, err := c.api.WithdrawExport(ctx, *silo, pos[0], *to, *m)
	if err != nil {
		return err
	}
	return c.print(e, func(w *tw) {
		switch {
		case m.ValidateOnly:
			fmt.Fprintf(w, "validate only: %s would be withdrawn; nothing changed\n", pos[0])
		case *to != "":
			fmt.Fprintf(w, "withdrew %s from %s\n", pos[0], *to)
		default:
			fmt.Fprintf(w, "withdrew %s\n", pos[0])
		}
	})
}

func (c *cli) importAccept(ctx context.Context, args []string) error {
	fs := c.flags("import accept")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the importing silo: the caller's own, or a descendant's, on its behalf")
	pos, err := c.parse(fs, args, 1, 1, "<exporter path>/<name>")
	if err != nil {
		return err
	}
	im, err := c.api.AcceptExport(ctx, *silo, pos[0], *m)
	if err != nil {
		return err
	}
	return c.print(im, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be accepted; nothing changed\n", pos[0])
			return
		}
		addr := im.Address
		if len(im.Repos) > 0 {
			addr = strings.Join(im.Repos, ", ")
		}
		fmt.Fprintf(w, "accepted %s (%s): %s\n", im.Export, im.State, addr)
	})
}

func (c *cli) importList(ctx context.Context, args []string) error {
	fs := c.flags("import list")
	silo := fs.String("silo", "self", "whose imports")
	if _, err := c.parse(fs, args, 0, 0, "[--silo s]"); err != nil {
		return err
	}
	ims, err := c.api.Imports(ctx, *silo)
	if err != nil {
		return err
	}
	offers, err := c.api.Offers(ctx, *silo)
	if err != nil {
		return err
	}
	if ims == nil {
		ims = []*client.Import{}
	}
	if offers == nil {
		offers = []*client.Import{}
	}
	return c.print(map[string]any{"imports": ims, "offers": offers}, func(w *tw) {
		writeImports(w, append(ims, offers...))
	})
}

func (c *cli) importDelete(ctx context.Context, args []string) error {
	fs := c.flags("import delete")
	m := mutation(fs)
	silo := fs.String("silo", "self", "the importing silo")
	pos, err := c.parse(fs, args, 1, 1, "<import id>")
	if err != nil {
		return err
	}
	im, err := c.api.DeleteImport(ctx, *silo, pos[0], *m)
	if err != nil {
		return err
	}
	return c.print(im, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be deleted; nothing changed\n", pos[0])
			return
		}
		fmt.Fprintf(w, "deleted the import of %s\n", im.Export)
	})
}

func writeExports(w *tw, es []*client.Export) {
	if len(es) == 0 {
		fmt.Fprintln(w, "no exports")
		return
	}
	w.row("NAME", "PROTOCOL", "TARGET", "TO", "STATE", "ADDRESS")
	for _, e := range es {
		target := fmt.Sprint(e.Target)
		if len(e.Repos) > 0 {
			target = "repos " + strings.Join(e.Repos, ",")
		} else if t, ok := e.Target.(map[string]any); ok {
			target = fmt.Sprintf("%v:%v (%v)", t["instance"], t["port"], dash(e.AddressIP))
		}
		if len(e.To) == 0 {
			w.row(e.Name, e.Protocol, target, "-", "-", "-")
		}
		for _, to := range e.To {
			w.row(e.Name, e.Protocol, target, to.Silo, to.State, dash(to.Address))
		}
	}
}

func writeImports(w *tw, ims []*client.Import) {
	if len(ims) == 0 {
		fmt.Fprintln(w, "no imports or offers")
		return
	}
	w.row("EXPORT", "PROTOCOL", "STATE", "ADDRESS", "ID")
	for _, im := range ims {
		addr := im.Address
		if len(im.Repos) > 0 {
			addr = strings.Join(im.Repos, ",")
		}
		w.row(im.Export, im.Protocol, im.State, dash(addr), dash(im.ID))
	}
}
