package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
)

// tw writes aligned columns.
type tw struct{ *tabwriter.Writer }

func (w *tw) row(cols ...string) { fmt.Fprintln(w, strings.Join(cols, "\t")) }

// print writes v as JSON with --output json, and with table otherwise.
func (c *cli) print(v any, table func(w *tw)) error {
	if c.output == "json" || table == nil {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(c.stdout, "%s\n", b)
		return err
	}
	w := &tw{tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)}
	table(w)
	return w.Flush()
}

// orNone is a list for a table cell.
func orNone(l []string) string {
	if len(l) == 0 {
		return "-"
	}
	return strings.Join(l, ",")
}

// orAny is an agent policy allowlist for a table cell: a missing one
// allows whatever the parent's does.
func orAny(l []string) string {
	if len(l) == 0 {
		return "any"
	}
	return strings.Join(l, ",")
}

func kvs(m map[string]string) string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return orNone(out)
}

// writeSilo writes one silo as fields, then its quota as a table.
func writeSilo(w *tw, s *client.Silo) {
	parent := "-"
	if s.Parent != nil {
		parent = *s.Parent
	}
	w.row("Path:", s.Path)
	w.row("ID:", s.ID)
	w.row("Parent:", parent)
	w.row("State:", s.State)
	w.row("Backing:", s.Backing.Kind)
	w.row("Facade:", s.Facade.Project+" at "+s.Facade.Endpoint)
	w.row("Name prefix:", s.Facade.NamePrefix)
	w.row("Regions:", orNone(s.Regions))
	w.row("Blocks:", fmt.Sprintf("%s (max %d)", orNone(s.Address.Blocks), s.Address.MaxBlocks))
	for _, d := range s.Address.Delegated {
		w.row("Delegated:", d.Range+" to "+d.Child)
	}
	for _, o := range s.Address.OwnRanges {
		w.row("Own range:", o.Range+" subnet "+o.Subnet)
	}
	w.row("Control:", fmt.Sprintf("own %s, effective %s (from %s)", s.Control.Own, s.Control.Effective, s.Control.EffectiveFrom))
	w.row("Egress:", fmt.Sprintf("allow %s, deny %s (own: allow %s, deny %s)",
		orNone(s.Egress.Effective.Allow), orNone(s.Egress.Effective.Deny), orNone(s.Egress.Own.Allow), orNone(s.Egress.Own.Deny)))
	w.row("Isolation:", fmt.Sprintf("%s, default %s (own: %s, default %s)", orNone(s.Isolation.Effective.Allowed),
		orDash(s.Isolation.Effective.Default), orNone(s.Isolation.Own.Allowed), orDash(s.Isolation.Own.Default)))
	w.row("Ingress:", fmt.Sprintf("%s (own: %s)", s.Ingress.Effective, s.Ingress.Own))
	w.row("Repos mirror:", fmt.Sprintf("%s, from %s (own: %s)", onOff(s.Repos.Effective.Mirror), s.Repos.EffectiveFrom, onOff(s.Repos.Own.Mirror)))
	w.row("Images sharing:", fmt.Sprintf("%s, from %s (own: %s)", s.Images.Effective.Sharing, s.Images.EffectiveFrom, s.Images.Own.Sharing))
	w.row("Grant policy:", fmt.Sprintf("%s (own: %s)", kvs(s.GrantPolicy.Effective), kvs(s.GrantPolicy.Own)))
	if a := s.AgentPolicy.Effective; len(a.Credentials)+len(a.Adapters)+len(a.Models) > 0 || a.MaxLease != "" {
		w.row("Agent policy:", fmt.Sprintf("credentials %s, adapters %s, models %s, max lease %s",
			orAny(a.Credentials), orAny(a.Adapters), orAny(a.Models), a.MaxLease))
	}
	b := s.ModelBudget
	w.row("Model budget:", fmt.Sprintf("$%s granted, $%s delegated, $%s used, per %s", b.GrantedUSD, b.DelegatedUSD, b.UsedUSD, b.Period))
	if u := s.Usage; u != nil {
		w.row("Usage:", usageLine(u))
	}
	w.row("Labels:", kvs(s.Labels))
	w.row("Created:", s.CreateTime.UTC().Format(time.RFC3339))
	for _, o := range s.OverCommitted {
		w.row("Over-committed:", o.Region+" "+o.Class)
	}
	if len(s.Quota) == 0 {
		w.row("Quota:", "-")
		return
	}
	fmt.Fprintln(w, "\nQuota:")
	w.row("REGION", "CLASS", "GRANTED", "DELEGATED", "USED", "FREE")
	for _, r := range sortedKeys(s.Quota) {
		for _, cl := range sortedKeys(s.Quota[r]) {
			a := s.Quota[r][cl]
			w.row(r, cl, string(a.Granted), string(a.Delegated), string(a.Used), free(a))
		}
	}
}

// free is granted - delegated - used, exactly.
func free(a client.QuotaAmount) string {
	n := func(v json.Number) *big.Rat {
		r, ok := new(big.Rat).SetString(string(v))
		if !ok {
			return new(big.Rat)
		}
		return r
	}
	f := new(big.Rat).Sub(n(a.Granted), n(a.Delegated))
	f.Sub(f, n(a.Used))
	if f.IsInt() {
		return f.Num().String()
	}
	return strings.TrimRight(strings.TrimRight(f.FloatString(6), "0"), ".")
}

// writeTree writes s's descendants, indented by depth.
func writeTree(w *tw, s *client.Silo, depth int) {
	w.row(strings.Repeat("  ", depth)+s.Path, s.ID, s.State, orNone(s.Address.Blocks), s.Control.Effective)
	for _, k := range s.Children {
		writeTree(w, k, depth+1)
	}
}

func writeSilos(w *tw, ss []*client.Silo) {
	if len(ss) == 0 {
		fmt.Fprintln(w, "no children")
		return
	}
	w.row("PATH", "ID", "STATE", "BLOCKS", "REGIONS", "CONTROL")
	for _, s := range ss {
		w.row(s.Path, s.ID, s.State, orNone(s.Address.Blocks), orNone(s.Regions), s.Control.Effective)
	}
}

func writePreview(w *tw, p *client.PreviewResponse) {
	if p.Fits {
		fmt.Fprintln(w, "Fits: yes")
	} else {
		fmt.Fprintln(w, "Fits: no")
	}
	if p.Refusal != nil {
		fmt.Fprintf(w, "Refusal: %s: %s\n", p.Refusal.Code, p.Refusal.Message)
	}
	fmt.Fprintln(w, "\nCharges:")
	w.row("PAYER", "HOME", "VIA", "KIND", "CLASS", "REGION", "AMOUNT")
	for _, c := range p.Charges {
		w.row(c.Payer, dash(c.Home), dash(c.Via), c.Kind, c.Class, c.Region, string(c.Amount))
	}
	fmt.Fprintln(w, "\nFree by level:")
	w.row("SILO", "CLASS", "REGION", "BEFORE", "AFTER", "FITS")
	for _, l := range p.Levels {
		fits := "yes"
		if !l.Fits {
			fits = "no"
		}
		w.row(l.Silo, l.Class, l.Region, string(l.FreeBefore), string(l.FreeAfter), fits)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeMembers(w *tw, ms []client.Member) {
	if len(ms) == 0 {
		fmt.Fprintln(w, "no members")
		return
	}
	w.row("IDENTITY", "SILO", "ID", "UPDATED")
	for _, m := range ms {
		w.row(m.Identity, m.Path, m.Silo, m.UpdateTime.UTC().Format(time.RFC3339))
	}
}

func writeAudit(w *tw, rs []client.AuditRecord) {
	if len(rs) == 0 {
		fmt.Fprintln(w, "no records")
		return
	}
	w.row("TIME", "SILO", "KIND", "ACTOR", "VIA", "RESULT", "REQUEST")
	for _, r := range rs {
		actor := r.Actor.Silo
		if r.Actor.Member != "" {
			actor += " (" + r.Actor.Member + ")"
		}
		w.row(r.Time.UTC().Format(time.RFC3339), r.Silo.Path, r.Kind, actor, dash(r.Via), r.Result, dash(r.RequestID))
	}
}

// renderError writes a failed command's error to stderr: an API
// refusal with its code and details (as the API's own JSON with
// --output json), and a call that never got an answer with what is
// probably wrong.
func (c *cli) renderError(ctx context.Context, err error) {
	var e *client.Error
	if errors.As(err, &e) {
		if c.output == "json" {
			b, _ := json.MarshalIndent(map[string]any{"error": e}, "", "  ")
			fmt.Fprintf(c.stderr, "%s\n", b)
			return
		}
		fmt.Fprintf(c.stderr, "silo: %v\n", err)
		for _, k := range sortedKeys(e.Details) {
			v, _ := json.Marshal(e.Details[k])
			fmt.Fprintf(c.stderr, "  %s: %s\n", k, strings.Trim(string(v), `"`))
		}
		return
	}
	fmt.Fprintf(c.stderr, "silo: %v\n", err)
	if hint := c.hint(ctx, err); hint != "" {
		fmt.Fprintf(c.stderr, "%s\n", hint)
	}
}

// hint says what is probably wrong when a call got no answer.
func (c *cli) hint(ctx context.Context, err error) string {
	ep, perr := url.Parse(c.endpoint)
	if perr != nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		if ep.Hostname() == "silo.internal" {
			return "silo.internal resolves only inside the cloud. From outside, give --project (or $SILO_PROJECT) to go through the front door,\n" +
				"or run silo proxy and point --endpoint (or $SILO_ENDPOINT) at it."
		}
		return ""
	}
	if !isTimeout(err) {
		return ""
	}
	// A sealed silo's packets to the API port are dropped by the fence,
	// so a call times out instead of being refused, while egress
	// through the proxy keeps working (gateway.md, "Layer 2", SV-81).
	proxy := c.proxyAddr
	if proxy == "" {
		proxy = net.JoinHostPort(ep.Hostname(), "3128")
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	conn, derr := (&net.Dialer{}).DialContext(pctx, "tcp", proxy)
	if derr != nil {
		return fmt.Sprintf("The API port (%s) did not answer, and neither did the egress proxy (%s): silod or the gateway may be down, or this machine is not in a silo.", ep.Host, proxy)
	}
	conn.Close()
	return fmt.Sprintf("The API port (%s) timed out while the egress proxy (%s) answers: this silo is probably sealed (control none),\n"+
		"so the fence drops its packets to the API port. An ancestor can see it with silo describe, and loosen it with silo patch <path> --control read.",
		ep.Host, proxy)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// usageLine is a host resource reading in a line: metering, not a charge.
func usageLine(u *client.ResourceUsage) string {
	mib := func(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
	mem := mib(u.MemoryBytes)
	if u.MemoryPeakBytes > 0 {
		mem += " (peak " + mib(u.MemoryPeakBytes) + ")"
	}
	return fmt.Sprintf("%.1fs CPU, %s memory, %s read, %s written, at %s", float64(u.CPUUsec)/1e6, mem,
		mib(u.ReadBytes), mib(u.WriteBytes), u.SampleTime.UTC().Format(time.RFC3339))
}
