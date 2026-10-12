package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
)

// The agent commands (fence-api.md, "Agents"; agents.md). An agent is
// named <silo>/agents/<name>, as /team-a/agents/fixer, or by its id.

func (c *cli) agentCreate(ctx context.Context, args []string) error {
	fs := c.flags("agent create")
	var req client.AgentRequest
	fs.StringVar(&req.Harness.Adapter, "adapter", "claude-code", "the harness: claude-code or codex")
	fs.StringVar(&req.Harness.Credential, "credential", "", "the deployment credential it uses")
	fs.StringVar(&req.Model, "model", "", "the model, which must have a price")
	fs.StringVar(&req.Budget.USD, "budget", "", "dollars of model use per budget period, reserved from the silo's")
	fs.StringVar(&req.ToolVM.Zone, "tool-zone", "", "the tool VM's zone")
	fs.StringVar(&req.ToolVM.Subnet, "tool-subnet", "", "the tool VM's subnet: one of the silo's, by its name, which starts with the silo's id (k7f3qa-work)")
	fs.StringVar(&req.ToolVM.MachineType, "machine-type", "", "the tool VM's machine type (default e2-standard-2)")
	fs.StringVar(&req.ToolVM.Image, "image", "", "the tool VM's image (default Debian 12)")
	fs.Int64Var(&req.ToolVM.DiskGB, "disk-gb", 0, "the tool VM's boot disk, in GB (default 20)")
	fs.BoolVar(&req.ToolVM.Spot, "spot", false, "make the tool VM a Spot VM")
	fs.StringVar(&req.Lease.TTL, "ttl", "", "the lease, from now (4h); default the silo's max_lease")
	fs.StringVar(&req.Lease.ExpireTime, "expire-time", "", "the lease's end, as an RFC 3339 time")
	fs.StringVar(&req.Prompt, "prompt", "", "the session's first message")
	promptFile := fs.String("prompt-file", "", "read the first message from a file (- for stdin)")
	labels := fs.String("labels", "", "key=value,...")
	wait := fs.Bool("wait", false, "wait for the tool server to connect")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<silo>/agents/<name> --credential ... --model ... --budget ... --tool-zone ... --tool-subnet ...")
	if err != nil {
		return err
	}
	silo, name, ok := strings.Cut(pos[0], "/agents/")
	if !ok || name == "" {
		return usagef("name the agent <silo>/agents/<name>, as /team-a/agents/fixer")
	}
	if silo == "" {
		silo = "/"
	}
	req.Name = name
	if *labels != "" {
		if req.Labels, err = parseMap("labels", *labels); err != nil {
			return usageError{err.Error()}
		}
	}
	if *promptFile != "" {
		var b []byte
		if *promptFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(*promptFile)
		}
		if err != nil {
			return usageError{err.Error()}
		}
		req.Prompt = string(b)
	}
	op, raw, err := c.api.CreateAgent(ctx, silo, req, *m)
	if err != nil {
		return err
	}
	if m.ValidateOnly {
		return c.print(raw, func(w *tw) { fmt.Fprintf(w, "validate only: %s would be created; nothing changed\n", pos[0]) })
	}
	if *wait && !op.Done {
		if c.output == "table" {
			fmt.Fprintf(c.stderr, "creating %s (operation %s)...\n", op.Target, op.ID)
		}
		if op, err = c.api.WaitOperation(ctx, op.ID, c.poll); err != nil {
			return fmt.Errorf("creating %s: %w", pos[0], err)
		}
	}
	return c.print(op, func(w *tw) {
		if op.Done {
			fmt.Fprintf(w, "created %s\n", op.Target)
		} else {
			fmt.Fprintf(w, "creating %s: operation %s is %s; it finishes when the tool server connects\n", op.Target, op.ID, op.State)
		}
	})
}

func (c *cli) agentList(ctx context.Context, args []string) error {
	fs := c.flags("agent list")
	pos, err := c.parse(fs, args, 0, 1, "[silo]")
	if err != nil {
		return err
	}
	l, err := c.api.Agents(ctx, arg(pos, 0, "self"))
	if err != nil {
		return err
	}
	if l == nil {
		l = []client.Agent{}
	}
	return c.print(map[string]any{"agents": l}, func(w *tw) { writeAgents(w, l) })
}

func writeAgents(w *tw, l []client.Agent) {
	w.row("NAME", "ID", "STATE", "MODEL", "BUDGET", "SPENT", "TOOL VM", "EXPIRES")
	for _, a := range l {
		w.row(a.Name, a.ID, a.State, a.Model, a.Budget.USD, a.Budget.SpentUSD, a.ToolVM.Instance+" "+orDash(a.ToolVM.IP),
			a.Lease.ExpireTime.Format(time.RFC3339))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeAgent(w *tw, a *client.Agent) {
	owner := "-"
	if a.Owner != nil {
		owner = *a.Owner
	}
	w.row("name", strings.TrimSuffix(a.SiloPath, "/")+"/agents/"+a.Name)
	w.row("id", a.ID)
	w.row("state", a.State+map[bool]string{true: " (" + a.StatusMessage + ")", false: ""}[a.StatusMessage != ""])
	w.row("harness", a.Harness.Adapter+" "+a.Harness.Version+", credential "+a.Harness.Credential)
	w.row("model", a.Model)
	w.row("budget", "$"+a.Budget.USD+" per period, $"+a.Budget.SpentUSD+" spent since "+a.Budget.PeriodStart.Format("2006-01-02"))
	w.row("tool vm", fmt.Sprintf("%s %s in %s, %s, %s, %d GB; tool server %s", a.ToolVM.Instance, orDash(a.ToolVM.IP),
		a.ToolVM.Zone, a.ToolVM.Subnet, a.ToolVM.MachineType, a.ToolVM.DiskGB, a.ToolVM.ToolServer))
	w.row("lease", "until "+a.Lease.ExpireTime.Format(time.RFC3339))
	w.row("owner", owner)
	w.row("labels", orDash(kvs(a.Labels)))
}

func (c *cli) agentGet(ctx context.Context, args []string) error {
	fs := c.flags("agent get")
	pos, err := c.parse(fs, args, 1, 1, "<agent>")
	if err != nil {
		return err
	}
	a, err := c.api.Agent(ctx, pos[0])
	if err != nil {
		return err
	}
	return c.print(a, func(w *tw) { writeAgent(w, a) })
}

func (c *cli) agentPatch(ctx context.Context, args []string) error {
	fs := c.flags("agent patch")
	budget := fs.String("budget", "", "a new budget.usd")
	labels := fs.String("labels", "", "key=value,... (replaces them all)")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<agent> [--budget ...] [--labels ...]")
	if err != nil {
		return err
	}
	var p client.AgentPatch
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["budget"] {
		p.Budget = &struct {
			USD string `json:"usd"`
		}{*budget}
	}
	if set["labels"] {
		l, err := parseMap("labels", *labels)
		if err != nil {
			return usageError{err.Error()}
		}
		p.Labels = &l
	}
	if p.Budget == nil && p.Labels == nil {
		return usagef("give --budget or --labels")
	}
	a, err := c.api.PatchAgent(ctx, pos[0], p, *m)
	if err != nil {
		return err
	}
	return c.print(a, func(w *tw) { writeAgent(w, a) })
}

func (c *cli) agentDelete(ctx context.Context, args []string) error {
	fs := c.flags("agent delete")
	noWait := fs.Bool("no-wait", false, "return the operation rather than waiting for it")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<agent>")
	if err != nil {
		return err
	}
	raw, err := c.api.DeleteAgent(ctx, pos[0], *m)
	if err != nil {
		return err
	}
	if m.ValidateOnly {
		return c.print(raw, func(w *tw) {
			fmt.Fprintf(w, "validate only: would delete %s, its tool VM, and what it owns: %v; nothing changed\n", pos[0], raw["deletes"])
		})
	}
	id, _ := raw["id"].(string)
	op, err := c.api.Operation(ctx, id)
	if err != nil {
		return err
	}
	if !*noWait && !op.Done {
		if c.output == "table" {
			fmt.Fprintf(c.stderr, "deleting %s (operation %s)...\n", op.Target, op.ID)
		}
		if op, err = c.api.WaitOperation(ctx, op.ID, c.poll); err != nil {
			return fmt.Errorf("deleting %s: %w", pos[0], err)
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

// agentAction is stop and resume.
func agentAction(verb string) command {
	return func(c *cli, ctx context.Context, args []string) error {
		fs := c.flags("agent " + verb)
		m := mutation(fs)
		pos, err := c.parse(fs, args, 1, 1, "<agent>")
		if err != nil {
			return err
		}
		a, err := c.api.AgentAction(ctx, pos[0], verb, nil, *m)
		if err != nil {
			return err
		}
		return c.print(a, func(w *tw) { fmt.Fprintf(w, "%s is %s\n", pos[0], a.State) })
	}
}

func (c *cli) agentRenew(ctx context.Context, args []string) error {
	fs := c.flags("agent renew")
	var l client.Lease
	fs.StringVar(&l.TTL, "ttl", "", "the new lease, from now (4h)")
	fs.StringVar(&l.ExpireTime, "expire-time", "", "the lease's new end, as an RFC 3339 time")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<agent> --ttl ...|--expire-time ...")
	if err != nil {
		return err
	}
	if (l.TTL == "") == (l.ExpireTime == "") {
		return usagef("give --ttl or --expire-time")
	}
	a, err := c.api.AgentAction(ctx, pos[0], "renew", l, *m)
	if err != nil {
		return err
	}
	return c.print(a, func(w *tw) {
		fmt.Fprintf(w, "%s is leased until %s\n", pos[0], a.Lease.ExpireTime.Format(time.RFC3339))
	})
}

func (c *cli) modelPriceList(ctx context.Context, args []string) error {
	fs := c.flags("model-price list")
	if _, err := c.parse(fs, args, 0, 0, ""); err != nil {
		return err
	}
	l, err := c.api.ModelPrices(ctx)
	if err != nil {
		return err
	}
	return c.print(map[string]any{"model_prices": l}, func(w *tw) {
		w.row("PROVIDER", "MODEL", "INPUT", "OUTPUT", "CACHE WRITE", "CACHE READ", "SOURCE")
		for _, p := range l {
			w.row(p.Provider, p.Model, p.Input, p.Output, p.CacheWrite, p.CacheRead, p.Source)
		}
	})
}

func (c *cli) modelPriceSet(ctx context.Context, args []string) error {
	fs := c.flags("model-price set")
	var p client.ModelPrice
	fs.StringVar(&p.Input, "input-usd", "", "dollars per million input tokens")
	fs.StringVar(&p.Output, "output-usd", "", "dollars per million output tokens")
	fs.StringVar(&p.CacheWrite, "cache-write-usd", "", "dollars per million cache-write tokens")
	fs.StringVar(&p.CacheRead, "cache-read-usd", "", "dollars per million cache-read tokens")
	del := fs.Bool("delete", false, "remove the override, so the release's price applies")
	m := mutation(fs)
	pos, err := c.parse(fs, args, 1, 1, "<provider>/<model> --input-usd ... --output-usd ... --cache-write-usd ... --cache-read-usd ...")
	if err != nil {
		return err
	}
	var ok bool
	if p.Provider, p.Model, ok = strings.Cut(pos[0], "/"); !ok || p.Model == "" {
		return usagef("name the model <provider>/<model>, as anthropic/claude-opus-5-5")
	}
	if *del {
		if err := c.api.DeleteModelPrice(ctx, p.Provider, p.Model, *m); err != nil {
			return err
		}
		return c.print(map[string]any{"provider": p.Provider, "model": p.Model, "deleted": !m.ValidateOnly}, func(w *tw) {
			fmt.Fprintf(w, "removed the override of %s\n", pos[0])
		})
	}
	if p.Input == "" || p.Output == "" || p.CacheWrite == "" || p.CacheRead == "" {
		return usagef("give --input-usd, --output-usd, --cache-write-usd and --cache-read-usd")
	}
	out, err := c.api.SetModelPrice(ctx, p, *m)
	if err != nil {
		return err
	}
	return c.print(out, func(w *tw) {
		fmt.Fprintf(w, "%s: $%s in, $%s out, $%s cache write, $%s cache read per million tokens\n", pos[0], out.Input, out.Output, out.CacheWrite, out.CacheRead)
	})
}
