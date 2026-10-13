// Package netfence is the local backend's network fence
// (docs/silo/local-backend.md §9, "The local network fence", call L8):
// the compiler that turns fence.Input and silo-net's topology into the
// nftables tables `inet silo` (the fence) and `bridge silo`
// (anti-spoofing), the applier that loads them into silo-net as one
// transaction, and an evaluator that runs a packet through the compiled
// rules for tests.
//
// The compiler emits a Ruleset, a small typed model of the tables:
// sets with their elements, and base chains whose rules are lists of
// matches. Render writes it as `nft -f` input, and Eval interprets the
// same model, so what the evaluator decides is what the rules loaded
// into the kernel say, not a second reading of the intent. The property
// test checks the evaluator's verdicts against the intent read straight
// from fence.Input.
package netfence

import (
	"fmt"
	"sort"
	"strings"
)

// Field is one thing a rule matches on, written as nft writes it.
type Field string

const (
	IIF       Field = "iifname"
	OIF       Field = "oifname"
	SAddr     Field = "ip saddr"
	DAddr     Field = "ip daddr"
	L4Proto   Field = "meta l4proto"
	DPort     Field = "th dport"
	CtState   Field = "ct state"
	EtherType Field = "ether type"
	EtherSrc  Field = "ether saddr"
	ArpSHA    Field = "arp saddr ether"
	ArpSPA    Field = "arp saddr ip"
)

// nftType is the set key type of a field.
var nftType = map[Field]string{
	IIF: "ifname", OIF: "ifname",
	SAddr: "ipv4_addr", DAddr: "ipv4_addr", ArpSPA: "ipv4_addr",
	L4Proto: "inet_proto", DPort: "inet_service",
	EtherSrc: "ether_addr", ArpSHA: "ether_addr",
}

// Match is one match of a rule: Fields (one, or several concatenated)
// looked up in the named Set, or one Field compared with Values (one
// value, or an anonymous set of them). Neg negates it.
type Match struct {
	Fields []Field
	Neg    bool
	Set    string
	Values []string
}

// Rule is a rule: every match must hold for its Verdict to be taken.
type Rule struct {
	Matches []Match
	Verdict string // accept or drop
	Comment string
}

// Chain is a base chain with its policy.
type Chain struct {
	Name     string
	Hook     string
	Priority int
	Policy   string
	Rules    []Rule
}

// Elem is one set element: a value per key field.
type Elem []string

// Set is a named set. Interval sets hold prefixes and ranges.
type Set struct {
	Name     string
	Key      []Field
	Interval bool
	Elems    []Elem
}

// Table is one nftables table.
type Table struct {
	Family, Name string
	Sets         []*Set
	Chains       []*Chain
}

// Ruleset is everything silod loads into silo-net: inet silo and bridge
// silo.
type Ruleset struct {
	Tables []*Table
}

// Table returns the table family name, or nil.
func (rs *Ruleset) Table(family string) *Table {
	for _, t := range rs.Tables {
		if t.Family == family {
			return t
		}
	}
	return nil
}

// Set returns the named set, or nil.
func (t *Table) Set(name string) *Set {
	for _, s := range t.Sets {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Chain returns the chain on hook, or nil.
func (t *Table) Chain(hook string) *Chain {
	for _, c := range t.Chains {
		if c.Hook == hook {
			return c
		}
	}
	return nil
}

// Render writes rs as one `nft -f` transaction: each table is created
// if missing, deleted, and defined again, so that the kernel switches
// from the old tables to the new ones at once and nothing of the old
// survives.
func (rs *Ruleset) Render() string {
	var b strings.Builder
	for _, t := range rs.Tables {
		fmt.Fprintf(&b, "table %s %s\ndelete table %s %s\n", t.Family, t.Name, t.Family, t.Name)
	}
	for _, t := range rs.Tables {
		fmt.Fprintf(&b, "table %s %s {\n", t.Family, t.Name)
		for _, s := range t.Sets {
			renderSet(&b, s)
		}
		for _, c := range t.Chains {
			fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority %d; policy %s;\n", c.Name, c.Hook, c.Priority, c.Policy)
			for _, r := range c.Rules {
				b.WriteString("\t\t")
				b.WriteString(r.String())
				b.WriteString("\n")
			}
			b.WriteString("\t}\n")
		}
		b.WriteString("}\n")
	}
	return b.String()
}

func renderSet(b *strings.Builder, s *Set) {
	var key []string
	for _, f := range s.Key {
		key = append(key, nftType[f])
	}
	fmt.Fprintf(b, "\tset %s {\n\t\ttype %s\n", s.Name, strings.Join(key, " . "))
	if s.Interval {
		b.WriteString("\t\tflags interval\n")
	}
	if len(s.Elems) > 0 {
		var es []string
		for _, e := range s.Elems {
			es = append(es, renderElem(s.Key, e))
		}
		fmt.Fprintf(b, "\t\telements = { %s }\n", strings.Join(es, ",\n\t\t\t"))
	}
	b.WriteString("\t}\n")
}

func renderElem(key []Field, e Elem) string {
	parts := make([]string, len(e))
	for i, v := range e {
		parts[i] = renderValue(key[i], v)
	}
	return strings.Join(parts, " . ")
}

// renderValue quotes interface names, the only free-form values; the
// compiler has already checked their characters.
func renderValue(f Field, v string) string {
	if f == IIF || f == OIF {
		return `"` + v + `"`
	}
	return v
}

// String is the rule as nft writes it.
func (r Rule) String() string {
	var parts []string
	for _, m := range r.Matches {
		parts = append(parts, m.String())
	}
	parts = append(parts, r.Verdict)
	if r.Comment != "" {
		parts = append(parts, fmt.Sprintf("comment %q", r.Comment))
	}
	return strings.Join(parts, " ")
}

// String is the match as nft writes it.
func (m Match) String() string {
	var fs []string
	for _, f := range m.Fields {
		fs = append(fs, string(f))
	}
	s := strings.Join(fs, " . ")
	if m.Neg {
		s += " !="
	}
	if m.Set != "" {
		return s + " @" + m.Set
	}
	vs := make([]string, len(m.Values))
	for i, v := range m.Values {
		vs[i] = renderValue(m.Fields[0], v)
	}
	if len(vs) == 1 {
		return s + " " + vs[0]
	}
	return s + " { " + strings.Join(vs, ", ") + " }"
}

// sortElems orders a set's elements and drops duplicates, so that the
// same Input always renders the same text and nft never sees an element
// twice.
func sortElems(s *Set) {
	key := func(e Elem) string { return strings.Join(e, "\x00") }
	sort.Slice(s.Elems, func(i, j int) bool { return key(s.Elems[i]) < key(s.Elems[j]) })
	var out []Elem
	for _, e := range s.Elems {
		if n := len(out); n > 0 && key(out[n-1]) == key(e) {
			continue
		}
		out = append(out, e)
	}
	s.Elems = out
}
