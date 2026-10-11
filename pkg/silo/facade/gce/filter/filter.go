// Package filter parses and evaluates the compute/v1 list `filter`
// and `orderBy` parameters, so that the facade applies them itself to
// the tenant's view of its own objects (docs/silo/gcp-backend.md,
// "Lists and filters", SV-74). The facade never passes a tenant's
// filter on to GCE (call 26): a filter evaluated here sees only what
// the tenant would see, so a filter naming a real, prefixed name or a
// hidden silo label matches nothing.
//
// # Grammar
//
// The grammar is the one gcloud and terraform send:
//
//	expr    := seq
//	seq     := or { ["AND"] or }          adjacent terms are ANDed
//	or      := unary { "OR" unary }
//	unary   := "NOT" unary | "-" unary | "(" seq ")" | compare
//	compare := path op value
//	path    := name { "." name }
//	op      := "=" | "!=" | "<" | "<=" | ">" | ">=" | ":" | "eq" | "ne"
//	value   := "double quoted" | 'single quoted' | bare
//
// OR binds tighter than AND, as in AIP-160, which GCE's list filter
// follows: `a AND b OR c` is `a AND (b OR c)`. Anything else -- gcloud's
// client-side forms such as `~`, `name:(a b)` or `name.basename()`, an
// unbalanced parenthesis, a missing value -- is an *Error, which the
// facade returns as 400 invalid.
//
// # Semantics
//
// A path walks nested objects (`scheduling.automaticRestart`,
// `labels.env`), and fans out over lists on the way
// (`networkInterfaces.networkIP`, `tags.items`, `disks.deviceName`). A
// comparison holds when any of the values the path reaches satisfies
// it; `!=` and `ne` are the negations of `=` and `eq`, so they hold for
// an object without the field.
//
//   - `eq` and `ne` take an RE2 regular expression that must match the
//     whole value, as GCE's do. Inside a quoted value a backslash
//     escapes only the quote; any other backslash is kept, so gcloud's
//     `".*\bweb\b.*"` reaches the regular expression unchanged.
//   - `=`, `!=` and `:` compare for equality; a `*` in the value is a
//     wildcard. `:*` holds when the field is present (`labels.env:*`),
//     and `:` against an object holds when it has the key
//     (`labels:env`).
//   - `<`, `<=`, `>`, `>=` compare numerically when both sides are
//     numbers (GCE's int64 fields arrive as strings) and as strings
//     otherwise, which orders RFC 3339 timestamps.
//   - Numbers compare as numbers, and booleans as `true`/`false` in
//     any case; strings compare exactly.
//
// # Samples
//
// The table tests use requests captured from a local logging server
// standing in for the compute endpoint
// (api_endpoint_overrides/compute, compute_custom_endpoint), on
// 2026-10-05:
//
//   - gcloud (Google Cloud SDK 587.0.0): `gcloud compute images list
//     --no-standard-images`, `regions list`, `zones list`,
//     `firewall-rules list`, `operations list` and `instances list
//     --zones=...` with `--filter`. gcloud rewrites the parts of its
//     own filter that the server can evaluate into the eq/ne form, for
//     example `name:web AND NOT status:STOPPED` becomes
//     `(name eq ".*\bweb\b.*")(status ne STOPPED)`, and evaluates the
//     rest (and every filter of a bare `instances list`, which uses
//     aggregatedList) on the client.
//   - terraform v1.16.5 with hashicorp/google v8.5.0: data sources
//     google_compute_zones and google_compute_regions with `status =
//     "UP"` (` (status eq UP)` and `(status eq UP)`), and
//     google_compute_images, google_compute_image and
//     google_compute_instance_template, which pass the configuration's
//     own `filter` through unchanged.
package filter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Error is an expression outside the grammar.
type Error struct {
	Pos int // byte offset in the expression
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("%s at position %d", e.Msg, e.Pos) }

// Expr is a parsed filter.
type Expr struct {
	n node // nil matches everything
}

// Parse parses a filter. The empty filter matches everything.
func Parse(s string) (*Expr, error) {
	p := &parser{s: s}
	n, err := p.seq(false)
	if err != nil {
		return nil, err
	}
	if p.skip(); p.pos < len(p.s) {
		return nil, p.errorf("unexpected %q", p.s[p.pos])
	}
	return &Expr{n: n}, nil
}

// Match reports whether obj, an object as JSON decodes it, satisfies
// the filter.
func (e *Expr) Match(obj map[string]any) bool {
	return e == nil || e.n == nil || e.n.eval(obj)
}

type node interface{ eval(obj map[string]any) bool }

type andNode struct{ l, r node }

func (n andNode) eval(o map[string]any) bool { return n.l.eval(o) && n.r.eval(o) }

type orNode struct{ l, r node }

func (n orNode) eval(o map[string]any) bool { return n.l.eval(o) || n.r.eval(o) }

type notNode struct{ n node }

func (n notNode) eval(o map[string]any) bool { return !n.n.eval(o) }

// cmp is one comparison. ne and != are notNodes over eq and =.
type cmp struct {
	path []string
	op   string // = : < <= > >= eq
	lit  string
	re   *regexp.Regexp // eq's expression, or the wildcard pattern of = and :
}

func (c *cmp) eval(o map[string]any) bool {
	vals := resolve(o, c.path, nil)
	if c.op == ":" && c.lit == "*" {
		return len(vals) > 0
	}
	for _, v := range vals {
		if c.test(v) {
			return true
		}
	}
	return false
}

func (c *cmp) test(v any) bool {
	if m, ok := v.(map[string]any); ok {
		if c.op != ":" {
			return false
		}
		for k := range m {
			if c.equal(k) {
				return true
			}
		}
		return false
	}
	s, ok := scalar(v)
	if !ok {
		return false
	}
	switch c.op {
	case "eq":
		return c.re.MatchString(s)
	case "=", ":":
		if c.re != nil {
			return c.re.MatchString(s)
		}
		switch v.(type) {
		case float64:
			a, err1 := strconv.ParseFloat(s, 64)
			b, err2 := strconv.ParseFloat(c.lit, 64)
			return err1 == nil && err2 == nil && a == b
		case bool:
			return strings.EqualFold(s, c.lit)
		}
		return s == c.lit
	}
	r := compare(v, c.lit)
	switch c.op {
	case "<":
		return r < 0
	case "<=":
		return r <= 0
	case ">":
		return r > 0
	case ">=":
		return r >= 0
	}
	return false
}

func (c *cmp) equal(s string) bool {
	if c.re != nil {
		return c.re.MatchString(s)
	}
	return s == c.lit
}

// resolve appends to out the values path reaches from v, fanning out
// over lists.
func resolve(v any, path []string, out []any) []any {
	if l, ok := v.([]any); ok {
		for _, e := range l {
			out = resolve(e, path, out)
		}
		return out
	}
	if len(path) == 0 {
		if v != nil {
			out = append(out, v)
		}
		return out
	}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	c, ok := m[path[0]]
	if !ok {
		return out
	}
	return resolve(c, path[1:], out)
}

// scalar is v as a string, for a string, number or boolean.
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// compare orders the value v against the literal lit: numerically if
// both are numbers, as strings otherwise.
func compare(v any, lit string) int {
	s, _ := scalar(v)
	if a, err := strconv.ParseFloat(s, 64); err == nil {
		if b, err := strconv.ParseFloat(lit, 64); err == nil {
			switch {
			case a < b:
				return -1
			case a > b:
				return 1
			}
			return 0
		}
	}
	if _, ok := v.(bool); ok {
		return strings.Compare(s, strings.ToLower(lit))
	}
	return strings.Compare(s, lit)
}

// ---- parsing ----

type parser struct {
	s   string
	pos int
}

func (p *parser) errorf(format string, args ...any) *Error {
	return &Error{Pos: p.pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) skip() {
	for p.pos < len(p.s) && isSpace(p.s[p.pos]) {
		p.pos++
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// keyword reports whether kw is next, ending at a space, a parenthesis
// or the end, and consumes it if so.
func (p *parser) keyword(kw string) bool {
	p.skip()
	if !strings.HasPrefix(p.s[p.pos:], kw) {
		return false
	}
	end := p.pos + len(kw)
	if end < len(p.s) && !isSpace(p.s[end]) && p.s[end] != '(' {
		return false
	}
	p.pos = end
	return true
}

// seq is terms ANDed, explicitly or by being adjacent, up to the end
// or, nested, the closing parenthesis.
func (p *parser) seq(nested bool) (node, error) {
	var n node
	for {
		p.skip()
		if p.pos == len(p.s) || p.s[p.pos] == ')' {
			if nested && p.pos == len(p.s) {
				return nil, p.errorf("missing )")
			}
			return n, nil
		}
		if p.keyword("AND") {
			if n == nil {
				return nil, p.errorf("AND with nothing before it")
			}
			if p.skip(); p.pos == len(p.s) || p.s[p.pos] == ')' {
				return nil, p.errorf("AND with nothing after it")
			}
		}
		t, err := p.or()
		if err != nil {
			return nil, err
		}
		if n == nil {
			n = t
		} else {
			n = andNode{n, t}
		}
	}
}

func (p *parser) or() (node, error) {
	n, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		n = orNode{n, r}
	}
	return n, nil
}

func (p *parser) unary() (node, error) {
	if p.keyword("NOT") {
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	p.skip()
	if p.pos == len(p.s) {
		return nil, p.errorf("expected a term")
	}
	switch p.s[p.pos] {
	case '-':
		p.pos++
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	case '(':
		p.pos++
		n, err := p.seq(true)
		if err != nil {
			return nil, err
		}
		if n == nil {
			return nil, p.errorf("empty parentheses")
		}
		p.pos++ // the ), which seq stopped at
		return n, nil
	case ')':
		return nil, p.errorf("unexpected )")
	}
	return p.compare()
}

func (p *parser) compare() (node, error) {
	start := p.pos
	for p.pos < len(p.s) && !isSpace(p.s[p.pos]) && !strings.ContainsRune("()=!<>:~", rune(p.s[p.pos])) {
		p.pos++
	}
	raw := p.s[start:p.pos]
	path, err := parsePath(raw)
	if err != nil {
		p.pos = start
		return nil, p.errorf("%s", err)
	}
	switch raw {
	case "AND", "OR", "NOT":
		p.pos = start
		return nil, p.errorf("unexpected %s", raw)
	}
	p.skip()
	op := ""
	for _, o := range []string{"!=", "<=", ">=", "=", "<", ">", ":"} {
		if strings.HasPrefix(p.s[p.pos:], o) {
			op = o
			p.pos += len(o)
			break
		}
	}
	if op == "" {
		for _, o := range []string{"eq", "ne"} {
			if strings.HasPrefix(p.s[p.pos:], o) && p.pos+2 < len(p.s) && isSpace(p.s[p.pos+2]) {
				op = o
				p.pos += 2
				break
			}
		}
	}
	if op == "" {
		return nil, p.errorf("expected a comparison after %s", raw)
	}
	p.skip()
	lit, err := p.value(op == "eq" || op == "ne")
	if err != nil {
		return nil, err
	}
	c := &cmp{path: path, op: op, lit: lit}
	neg := false
	switch op {
	case "!=":
		c.op, neg = "=", true
	case "ne":
		c.op, neg = "eq", true
	}
	switch c.op {
	case "eq":
		re, err := regexp.Compile(`^(?:` + lit + `)$`)
		if err != nil {
			return nil, p.errorf("invalid regular expression %q: %v", lit, err)
		}
		c.re = re
	case "=", ":":
		if c.op == ":" && lit == "*" {
			break // presence, in eval
		}
		if strings.Contains(lit, "*") {
			parts := strings.Split(lit, "*")
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			c.re = regexp.MustCompile(`^` + strings.Join(parts, ".*") + `$`)
		}
	}
	if neg {
		return notNode{c}, nil
	}
	return c, nil
}

// value is a quoted string or a bare word. In a quoted string a
// backslash escapes the quote and is otherwise kept, for regular
// expressions. A bare word runs to a space or an unbalanced ), and
// only a regular expression may start with one.
func (p *parser) value(regex bool) (string, error) {
	if p.pos == len(p.s) {
		return "", p.errorf("expected a value")
	}
	if q := p.s[p.pos]; q == '"' || q == '\'' {
		start := p.pos
		p.pos++
		var b strings.Builder
		for p.pos < len(p.s) {
			c := p.s[p.pos]
			switch {
			case c == q:
				p.pos++
				return b.String(), nil
			case c == '\\' && p.pos+1 < len(p.s) && p.s[p.pos+1] == q:
				b.WriteByte(q)
				p.pos += 2
			case c == '\\' && p.pos+1 < len(p.s):
				b.WriteString(p.s[p.pos : p.pos+2])
				p.pos += 2
			default:
				b.WriteByte(c)
				p.pos++
			}
		}
		p.pos = start
		return "", p.errorf("unterminated string")
	}
	if p.s[p.pos] == '(' && !regex {
		return "", p.errorf("a parenthesised list of values isn't supported")
	}
	start, depth := p.pos, 0
	for p.pos < len(p.s) && !isSpace(p.s[p.pos]) {
		if c := p.s[p.pos]; c == '(' {
			depth++
		} else if c == ')' {
			if depth == 0 {
				break
			}
			depth--
		}
		p.pos++
	}
	if p.pos == start {
		return "", p.errorf("expected a value")
	}
	return p.s[start:p.pos], nil
}

// parsePath splits a dotted field path, whose names are letters,
// digits, _ and -.
func parsePath(s string) ([]string, error) {
	if s == "" {
		return nil, fmt.Errorf("expected a field")
	}
	parts := strings.Split(s, ".")
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid field %q", s)
		}
		for _, r := range part {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
				return nil, fmt.Errorf("invalid field %q", s)
			}
		}
	}
	return parts, nil
}

// ---- ordering ----

// Order is a parsed orderBy: keys, each ascending unless desc.
type Order []Key

// Key is one orderBy key.
type Key struct {
	Path []string
	Desc bool
}

// ParseOrder parses orderBy: comma-separated field paths, each
// optionally followed by asc or desc ("creationTimestamp desc"). The
// empty orderBy keeps the list's own order.
func ParseOrder(s string) (Order, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var o Order
	pos := 0
	for _, part := range strings.Split(s, ",") {
		f := strings.Fields(part)
		if len(f) == 0 || len(f) > 2 {
			return nil, &Error{Pos: pos, Msg: fmt.Sprintf("invalid sort key %q", strings.TrimSpace(part))}
		}
		path, err := parsePath(f[0])
		if err != nil {
			return nil, &Error{Pos: pos, Msg: err.Error()}
		}
		k := Key{Path: path}
		if len(f) == 2 {
			switch strings.ToLower(f[1]) {
			case "asc":
			case "desc":
				k.Desc = true
			default:
				return nil, &Error{Pos: pos, Msg: fmt.Sprintf("invalid sort direction %q", f[1])}
			}
		}
		o = append(o, k)
		pos += len(part) + 1
	}
	return o, nil
}

// Less reports whether a sorts before b. An object without a key's
// field sorts first, ascending.
func (o Order) Less(a, b map[string]any) bool {
	for _, k := range o {
		c := compareValues(first(a, k.Path), first(b, k.Path))
		if c == 0 {
			continue
		}
		if k.Desc {
			return c > 0
		}
		return c < 0
	}
	return false
}

func first(o map[string]any, path []string) any {
	if v := resolve(o, path, nil); len(v) > 0 {
		return v[0]
	}
	return nil
}

func compareValues(a, b any) int {
	sa, oka := scalar(a)
	sb, okb := scalar(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	if x, err := strconv.ParseFloat(sa, 64); err == nil {
		if y, err := strconv.ParseFloat(sb, 64); err == nil {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
	}
	return strings.Compare(sa, sb)
}
