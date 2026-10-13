package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
)

func (c *cli) describe(ctx context.Context, args []string) error {
	fs := c.flags("describe")
	subtree := fs.Bool("subtree", false, "add the silo's descendants (ancestors only)")
	pos, err := c.parse(fs, args, 0, 1, "[silo] [--subtree]")
	if err != nil {
		return err
	}
	s, err := c.api.Describe(ctx, arg(pos, 0, "self"), *subtree)
	if err != nil {
		return err
	}
	return c.print(s, func(w *tw) {
		writeSilo(w, s)
		if *subtree {
			fmt.Fprintln(w, "\nSubtree:")
			w.row("PATH", "ID", "STATE", "BLOCKS", "CONTROL")
			writeTree(w, s, 0)
		}
	})
}

func (c *cli) children(ctx context.Context, args []string) error {
	fs := c.flags("children")
	pos, err := c.parse(fs, args, 0, 1, "[silo]")
	if err != nil {
		return err
	}
	kids, err := c.api.Children(ctx, arg(pos, 0, "self"))
	if err != nil {
		return err
	}
	if kids == nil {
		kids = []*client.Silo{}
	}
	return c.print(map[string]any{"silos": kids}, func(w *tw) {
		writeSilos(w, kids)
	})
}

// allocFlags are the flags create and patch share.
type allocFlags struct {
	fs                             *flag.FlagSet
	address, regions, quota, model string
	egressAllow, egressDeny        string
	grantPolicy, control, labels   string
	maxBlocks                      int
	agentPolicy                    string
	backing, ingress               string
	reposMirror                    string
	isolation, isolationDefault    string
}

func addAllocFlags(fs *flag.FlagSet) *allocFlags {
	a := &allocFlags{fs: fs}
	fs.StringVar(&a.address, "address", "", "the block: a prefix length silod places (/20), or a range (10.1.16.0/20)")
	fs.StringVar(&a.regions, "regions", "", "comma-separated regions (create: default the parent's)")
	fs.StringVar(&a.quota, "quota", "", "region:class=amount,... (us-central1:vcpu=16,memory=64GiB,global:agent_sessions=2)")
	fs.StringVar(&a.model, "model-budget", "", "the model budget, in dollars")
	fs.IntVar(&a.maxBlocks, "max-blocks", 0, "the most blocks the silo may hold")
	fs.StringVar(&a.egressAllow, "egress-allow", "", "comma-separated hosts the silo may reach (create: default none)")
	fs.StringVar(&a.egressDeny, "egress-deny", "", "comma-separated hosts denied")
	fs.StringVar(&a.grantPolicy, "grant-policy", "", "kind=allow|deny,... (exports=allow,links=deny)")
	fs.StringVar(&a.control, "control", "", "full, read or none (patch: inherit goes back to the parent's)")
	fs.StringVar(&a.agentPolicy, "agent-policy", "", "the agent policy, as JSON")
	fs.StringVar(&a.backing, "backing", "", "create: subnet or local (default the parent's)")
	fs.StringVar(&a.isolation, "isolation", "", "comma-separated isolation classes allowed: vm, sandbox (create: default the parent's)")
	fs.StringVar(&a.isolationDefault, "isolation-default", "", "the isolation class a container gets when it names none (default the strongest allowed)")
	fs.StringVar(&a.ingress, "ingress", "", "host or none: whether published ports also listen on the host's loopback (create: default none)")
	fs.StringVar(&a.reposMirror, "repos-mirror", "", "true or false: whether the silo's repos are mirrored into the uber repo (create: default the parent's)")
	fs.StringVar(&a.labels, "labels", "", "key=value,... (patch replaces them all)")
	return a
}

// set lists the flags given on the command line.
func (a *allocFlags) set() map[string]bool {
	out := map[string]bool{}
	a.fs.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}

func (a *allocFlags) createRequest() (client.CreateRequest, error) {
	var req client.CreateRequest
	set := a.set()
	if !set["address"] {
		return req, usagef("--address is required: a prefix length (/20) or a range")
	}
	var err error
	if req.Address, err = parseAddress(a.address); err != nil {
		return req, usageError{err.Error()}
	}
	if set["regions"] {
		req.Regions = parseList(a.regions)
	}
	if set["quota"] {
		if req.Quota, err = parseQuota(a.quota); err != nil {
			return req, usageError{err.Error()}
		}
	}
	req.ModelBudgetUSD = a.model
	req.MaxBlocks = a.maxBlocks
	if set["egress-allow"] || set["egress-deny"] {
		req.Egress = &client.Egress{Allow: parseList(a.egressAllow), Deny: parseList(a.egressDeny)}
	}
	if set["grant-policy"] {
		if req.GrantPolicy, err = parseMap("grant-policy", a.grantPolicy); err != nil {
			return req, usageError{err.Error()}
		}
	}
	req.Control = a.control
	if set["backing"] {
		req.Backing = &client.Backing{Kind: a.backing}
	}
	if set["isolation"] || set["isolation-default"] {
		req.Isolation = &client.Isolation{Allowed: parseList(a.isolation), Default: a.isolationDefault}
	}
	req.Ingress = a.ingress
	if set["repos-mirror"] {
		v, err := strconv.ParseBool(a.reposMirror)
		if err != nil {
			return req, usagef("--repos-mirror %q: want true or false", a.reposMirror)
		}
		req.Repos = &client.ReposPolicy{Mirror: v}
	}
	if set["agent-policy"] {
		req.AgentPolicy = &client.AgentPolicy{}
		if err := json.Unmarshal([]byte(a.agentPolicy), req.AgentPolicy); err != nil {
			return req, usagef("--agent-policy: %v", err)
		}
	}
	if set["labels"] {
		if req.Labels, err = parseMap("labels", a.labels); err != nil {
			return req, usageError{err.Error()}
		}
	}
	return req, nil
}

// patchBody sends only the fields whose flags were given.
func (a *allocFlags) patchBody() (client.Patch, error) {
	set := a.set()
	p := client.Patch{}
	if set["address"] {
		return nil, usagef("--address can't be patched: use silo grow or silo block add|release")
	}
	if set["backing"] {
		return nil, usagef("--backing can't be patched: a silo keeps the backing it was created with")
	}
	if set["regions"] {
		p["regions"] = parseList(a.regions)
	}
	if set["quota"] {
		q, err := parseQuota(a.quota)
		if err != nil {
			return nil, usageError{err.Error()}
		}
		p["quota"] = q
	}
	if set["model-budget"] {
		p["model_budget_usd"] = a.model
	}
	if set["max-blocks"] {
		p["max_blocks"] = a.maxBlocks
	}
	if set["egress-allow"] || set["egress-deny"] {
		e := map[string]any{}
		if set["egress-allow"] {
			e["allow"] = parseList(a.egressAllow)
		}
		if set["egress-deny"] {
			e["deny"] = parseList(a.egressDeny)
		}
		p["egress"] = e
	}
	if set["grant-policy"] {
		g, err := parseMap("grant-policy", a.grantPolicy)
		if err != nil {
			return nil, usageError{err.Error()}
		}
		// inherit drops a key, which then means deny.
		m := map[string]any{}
		for k, v := range g {
			if v == "inherit" {
				m[k] = nil
			} else {
				m[k] = v
			}
		}
		p["grant_policy"] = m
	}
	if set["control"] {
		if a.control == "inherit" {
			p["control"] = nil
		} else {
			p["control"] = a.control
		}
	}
	if set["isolation"] || set["isolation-default"] {
		i := map[string]any{}
		if set["isolation"] {
			i["allowed"] = parseList(a.isolation)
		}
		if set["isolation-default"] {
			i["default"] = a.isolationDefault
		}
		p["isolation"] = i
	}
	if set["repos-mirror"] {
		v, err := strconv.ParseBool(a.reposMirror)
		if err != nil {
			return nil, usagef("--repos-mirror %q: want true or false", a.reposMirror)
		}
		p["repos"] = map[string]any{"mirror": v}
	}
	if set["ingress"] {
		p["ingress"] = a.ingress
	}
	if set["agent-policy"] {
		var v map[string]any
		if err := json.Unmarshal([]byte(a.agentPolicy), &v); err != nil {
			return nil, usagef("--agent-policy: %v", err)
		}
		p["agent_policy"] = v
	}
	if set["labels"] {
		l, err := parseMap("labels", a.labels)
		if err != nil {
			return nil, usageError{err.Error()}
		}
		p["labels"] = l
	}
	if len(p) == 0 {
		return nil, usagef("nothing to change: give at least one of the flags")
	}
	return p, nil
}

func (c *cli) create(ctx context.Context, args []string) error {
	fs := c.flags("create")
	a := addAllocFlags(fs)
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<path> --address /n|<cidr> [flags]")
	if err != nil {
		return err
	}
	req, err := a.createRequest()
	if err != nil {
		return err
	}
	s, err := c.api.Create(ctx, pos[0], req, *m)
	if err != nil {
		return err
	}
	return c.printSilo(s, "created", *m)
}

func (c *cli) patch(ctx context.Context, args []string) error {
	fs := c.flags("patch")
	a := addAllocFlags(fs)
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<silo> [flags]")
	if err != nil {
		return err
	}
	body, err := a.patchBody()
	if err != nil {
		return err
	}
	s, err := c.api.Patch(ctx, pos[0], body, *m)
	if err != nil {
		return err
	}
	return c.printSilo(s, "patched", *m)
}

// printSilo shows a mutation's resulting silo.
func (c *cli) printSilo(s *client.Silo, verb string, m client.Mutation) error {
	return c.print(s, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be %s; nothing changed\n\n", s.Path, verb)
		} else {
			fmt.Fprintf(w, "%s %s\n\n", verb, s.Path)
		}
		writeSilo(w, s)
	})
}

func (c *cli) delete(ctx context.Context, args []string) error {
	fs := c.flags("delete")
	force := fs.Bool("force", false, "delete the subtree and everything in it, bottom-up")
	noWait := fs.Bool("no-wait", false, "return the operation rather than waiting for it")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<silo> [--force]")
	if err != nil {
		return err
	}
	res, err := c.api.Delete(ctx, pos[0], *force, *m)
	if err != nil {
		return err
	}
	if res.ValidateOnly {
		return c.print(map[string]any{"validate_only": true, "deletes": res.Deletes}, func(w *tw) {
			fmt.Fprintf(w, "validate only: would delete %s; nothing changed\n", strings.Join(res.Deletes, ", "))
		})
	}
	op := &res.Operation
	if !*noWait && !op.Done {
		if c.output == "table" {
			fmt.Fprintf(c.stderr, "deleting %s (operation %s)...\n", op.Target, op.ID)
		}
		op, err = c.api.WaitOperation(ctx, op.ID, c.poll)
		if err != nil {
			if op != nil && c.output == "json" {
				c.print(op, nil)
			}
			return fmt.Errorf("deleting %s: %w", res.Target, err)
		}
	}
	return c.print(op, func(w *tw) {
		if op.Done {
			fmt.Fprintf(w, "deleted %s\n", op.Target)
		} else {
			fmt.Fprintf(w, "deleting %s: operation %s is %s\n", op.Target, op.ID, op.State)
		}
	})
}

func (c *cli) grow(ctx context.Context, args []string) error {
	fs := c.flags("grow")
	to := fs.String("to", "", "the new prefix length (/19)")
	block := fs.String("block", "", "the block to widen (default the silo's only block)")
	add := fs.Bool("add-block", false, "if the block can't be widened, add a new block of that size")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<silo> --to /n [--block <cidr>] [--add-block]")
	if err != nil {
		return err
	}
	n, err := parsePrefixLength(*to)
	if err != nil {
		return usagef("--to %v", err)
	}
	s, err := c.api.Grow(ctx, pos[0], client.GrowRequest{Block: *block, ToPrefixLength: n, AddBlock: *add}, *m)
	if err != nil {
		return err
	}
	return c.printSilo(s, "grown", *m)
}

func (c *cli) blockAdd(ctx context.Context, args []string) error {
	fs := c.flags("block add")
	addr := fs.String("address", "", "a prefix length silod places (/22), or a range")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<silo> --address /n|<cidr>")
	if err != nil {
		return err
	}
	want, err := parseAddress(*addr)
	if err != nil {
		return usageError{err.Error()}
	}
	s, err := c.api.AddBlock(ctx, pos[0], *want, *m)
	if err != nil {
		return err
	}
	return c.printSilo(s, "given a block:", *m)
}

func (c *cli) blockRelease(ctx context.Context, args []string) error {
	fs := c.flags("block release")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 2, 2, "<silo> <cidr>")
	if err != nil {
		return err
	}
	s, err := c.api.ReleaseBlock(ctx, pos[0], pos[1], *m)
	if err != nil {
		return err
	}
	return c.printSilo(s, "released "+pos[1]+" from", *m)
}

func (c *cli) quotaPreview(ctx context.Context, args []string) error {
	fs := c.flags("quota preview")
	quota := fs.String("quota", "", "the allocation to price, as in create")
	model := fs.String("model-budget", "", "a model budget to price, in dollars")
	facade := fs.String("facade-request", "", "a facade call to price instead, as JSON {method, path, body}, or @file")
	pos, err := c.parse(fs, args, 0, 1, "<silo> --quota ... | --facade-request <json>")
	if err != nil {
		return err
	}
	var req client.PreviewRequest
	switch {
	case *facade != "" && len(pos) == 0 && *quota == "" && *model == "":
		b := []byte(*facade)
		if name, ok := strings.CutPrefix(*facade, "@"); ok {
			if b, err = os.ReadFile(name); err != nil {
				return err
			}
		}
		req.FacadeRequest = &client.FacadeRequest{}
		if err := json.Unmarshal(b, req.FacadeRequest); err != nil {
			return usagef("--facade-request: %v", err)
		}
	case *facade == "" && len(pos) == 1 && (*quota != "" || *model != ""):
		req.Allocation = &client.Allocation{Silo: pos[0], ModelBudgetUSD: *model}
		if *quota != "" {
			if req.Allocation.Quota, err = parseQuota(*quota); err != nil {
				return usageError{err.Error()}
			}
		}
	default:
		return usagef("give a silo and --quota or --model-budget, or --facade-request alone")
	}
	res, err := c.api.PreviewQuota(ctx, req)
	if err != nil {
		return err
	}
	return c.print(res, func(w *tw) { writePreview(w, res) })
}

func (c *cli) membersGet(ctx context.Context, args []string) error {
	fs := c.flags("members get")
	pos, err := c.parse(fs, args, 1, 2, "<silo> [identity]")
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		m, err := c.api.Member(ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		return c.print(m, func(w *tw) { writeMembers(w, []client.Member{*m}) })
	}
	ms, err := c.api.Members(ctx, pos[0])
	if err != nil {
		return err
	}
	if ms == nil {
		ms = []client.Member{}
	}
	return c.print(map[string]any{"members": ms}, func(w *tw) { writeMembers(w, ms) })
}

func (c *cli) membersPut(ctx context.Context, args []string) error {
	fs := c.flags("members put")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 2, 2, "<silo> <identity>")
	if err != nil {
		return err
	}
	mem, err := c.api.PutMember(ctx, pos[0], pos[1], *m)
	if err != nil {
		return err
	}
	if m.ValidateOnly && mem.Identity == "" {
		mem = &client.Member{Identity: pos[1], Path: pos[0]}
	}
	return c.print(mem, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be mapped to %s; nothing changed\n", pos[1], pos[0])
		} else {
			fmt.Fprintf(w, "mapped %s to %s\n", mem.Identity, mem.Path)
		}
	})
}

func (c *cli) membersDelete(ctx context.Context, args []string) error {
	fs := c.flags("members delete")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 2, 2, "<silo> <identity>")
	if err != nil {
		return err
	}
	if err := c.api.DeleteMember(ctx, pos[0], pos[1], *m); err != nil {
		return err
	}
	out := map[string]any{"identity": pos[1], "silo": pos[0], "deleted": !m.ValidateOnly}
	return c.print(out, func(w *tw) {
		if m.ValidateOnly {
			fmt.Fprintf(w, "validate only: %s would be removed from %s; nothing changed\n", pos[1], pos[0])
		} else {
			fmt.Fprintf(w, "removed %s from %s\n", pos[1], pos[0])
		}
	})
}

func (c *cli) audit(ctx context.Context, args []string) error {
	fs := c.flags("audit")
	since := fs.String("since", "", "records at or after this: a time, a date, or a duration back (24h, 7d)")
	kind := fs.String("kind", "", "only this kind (silo.create), or a prefix ending in * (silo.*)")
	subtree := fs.Bool("subtree", false, "include the silo's descendants (ancestors only)")
	limit := fs.Int("limit", 0, "at most this many records (default all)")
	pos, err := c.parse(fs, args, 0, 1, "[silo] [--since ...] [--kind ...] [--subtree]")
	if err != nil {
		return err
	}
	q := client.AuditQuery{Kind: *kind, Subtree: *subtree, Limit: *limit}
	if *since != "" {
		if q.Since, err = parseSince(*since, time.Now()); err != nil {
			return usageError{err.Error()}
		}
	}
	recs, err := c.api.Audit(ctx, arg(pos, 0, "self"), q)
	if err != nil {
		return err
	}
	return c.print(map[string]any{"records": recs}, func(w *tw) { writeAudit(w, recs) })
}

// reconcile shows silod's reconcile mode, or with --apply leaves
// report-only: after install adopts a restored database, silod only
// reports orphans until an operator runs this (state.md "Backups and
// recovery", SV-54).
func (c *cli) reconcile(ctx context.Context, args []string) error {
	fs := c.flags("reconcile")
	apply := fs.Bool("apply", false, "leave report-only mode: orphans are deleted again once their grace period has passed")
	m := mutation(fs)
	if _, err := c.parse(fs, args, 0, 0, "[--apply]"); err != nil {
		return err
	}
	var mode string
	var err error
	if *apply {
		mode, err = c.api.ReconcileApply(ctx, *m)
	} else {
		mode, err = c.api.ReconcileMode(ctx)
	}
	if err != nil {
		return err
	}
	out := map[string]any{"mode": mode}
	if *apply && m.ValidateOnly {
		out["validate_only"] = true
	}
	return c.print(out, func(w *tw) {
		w.row("MODE")
		w.row(mode)
	})
}
