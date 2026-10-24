// Package tomlite parses the small subset of TOML that silo's configuration
// files use, so that silo can read /etc/silo/*.toml without a third-party
// dependency.
//
// Supported: comments, [table] and [a.b] headers, bare and quoted keys,
// dotted keys, basic ("...") and literal ('...') strings, integers, floats,
// booleans, arrays (which may span lines) and inline tables. Not supported:
// multi-line strings, dates and times, and arrays of tables ([[x]]). Anything
// unsupported is an error, never silently ignored.
package tomlite

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Parse parses a document into nested maps. Values are string, int64,
// float64, bool, []any or map[string]any.
func Parse(data []byte) (map[string]any, error) {
	p := &parser{src: string(data), line: 1}
	return p.document()
}

// Unmarshal parses data and decodes it into v using encoding/json's rules and
// struct tags, rejecting keys that v does not have.
func Unmarshal(data []byte, v any) error {
	m, err := Parse(data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type parser struct {
	src  string
	pos  int
	line int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("toml: line %d: %s", p.line, fmt.Sprintf(format, args...))
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

// skipSpace skips spaces and tabs, and also newlines and comments when nl is
// true.
func (p *parser) skipSpace(nl bool) {
	for !p.eof() {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			p.pos++
		case c == '#':
			for !p.eof() && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '\n' && nl:
			p.pos++
			p.line++
		default:
			return
		}
	}
}

// endOfLine requires only whitespace or a comment up to the next newline.
func (p *parser) endOfLine() error {
	p.skipSpace(false)
	if p.eof() {
		return nil
	}
	if p.src[p.pos] != '\n' {
		return p.errorf("unexpected %q after value", p.src[p.pos])
	}
	p.pos++
	p.line++
	return nil
}

func (p *parser) document() (map[string]any, error) {
	root := map[string]any{}
	cur := root
	defined := map[string]bool{}
	for {
		p.skipSpace(true)
		if p.eof() {
			return root, nil
		}
		if p.peek() == '[' {
			p.pos++
			if p.peek() == '[' {
				return nil, p.errorf("arrays of tables are not supported")
			}
			p.skipSpace(false)
			keys, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipSpace(false)
			if p.peek() != ']' {
				return nil, p.errorf("expected ] after table name")
			}
			p.pos++
			name := strings.Join(keys, ".")
			if defined[name] {
				return nil, p.errorf("table [%s] defined twice", name)
			}
			defined[name] = true
			t, err := p.table(root, keys)
			if err != nil {
				return nil, err
			}
			cur = t
			if err := p.endOfLine(); err != nil {
				return nil, err
			}
			continue
		}
		if err := p.keyValue(cur); err != nil {
			return nil, err
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

// table walks (creating as needed) the nested table named by keys.
func (p *parser) table(m map[string]any, keys []string) (map[string]any, error) {
	for _, k := range keys {
		next, ok := m[k]
		if !ok {
			t := map[string]any{}
			m[k] = t
			m = t
			continue
		}
		t, ok := next.(map[string]any)
		if !ok {
			return nil, p.errorf("key %q is not a table", k)
		}
		m = t
	}
	return m, nil
}

func (p *parser) keyValue(m map[string]any) error {
	keys, err := p.key()
	if err != nil {
		return err
	}
	p.skipSpace(false)
	if p.peek() != '=' {
		return p.errorf("expected = after key %q", strings.Join(keys, "."))
	}
	p.pos++
	p.skipSpace(false)
	v, err := p.value()
	if err != nil {
		return err
	}
	t, err := p.table(m, keys[:len(keys)-1])
	if err != nil {
		return err
	}
	last := keys[len(keys)-1]
	if _, dup := t[last]; dup {
		return p.errorf("key %q defined twice", strings.Join(keys, "."))
	}
	t[last] = v
	return nil
}

// key parses a possibly dotted key.
func (p *parser) key() ([]string, error) {
	var keys []string
	for {
		p.skipSpace(false)
		var k string
		switch c := p.peek(); {
		case c == '"':
			s, err := p.basicString()
			if err != nil {
				return nil, err
			}
			k = s
		case c == '\'':
			s, err := p.literalString()
			if err != nil {
				return nil, err
			}
			k = s
		default:
			start := p.pos
			for !p.eof() && isBare(p.src[p.pos]) {
				p.pos++
			}
			if start == p.pos {
				return nil, p.errorf("expected a key")
			}
			k = p.src[start:p.pos]
		}
		keys = append(keys, k)
		p.skipSpace(false)
		if p.peek() != '.' {
			return keys, nil
		}
		p.pos++
	}
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *parser) value() (any, error) {
	switch c := p.peek(); {
	case c == '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			return nil, p.errorf("multi-line strings are not supported")
		}
		return p.basicString()
	case c == '\'':
		if strings.HasPrefix(p.src[p.pos:], `'''`) {
			return nil, p.errorf("multi-line strings are not supported")
		}
		return p.literalString()
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	default:
		start := p.pos
		for !p.eof() && (isBare(p.src[p.pos]) || strings.IndexByte("+.:", p.src[p.pos]) >= 0) {
			p.pos++
		}
		tok := p.src[start:p.pos]
		switch tok {
		case "":
			return nil, p.errorf("expected a value")
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		clean := strings.ReplaceAll(tok, "_", "")
		if i, err := strconv.ParseInt(clean, 0, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(clean, 64); err == nil && !strings.ContainsAny(tok, "xob:") {
			return f, nil
		}
		return nil, p.errorf("unsupported value %q", tok)
	}
}

func (p *parser) array() (any, error) {
	p.pos++ // [
	out := []any{}
	for {
		p.skipSpace(true)
		if p.peek() == ']' {
			p.pos++
			return out, nil
		}
		if p.eof() {
			return nil, p.errorf("unterminated array")
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipSpace(true)
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
		default:
			return nil, p.errorf("expected , or ] in array")
		}
	}
}

func (p *parser) inlineTable() (any, error) {
	p.pos++ // {
	out := map[string]any{}
	p.skipSpace(false)
	if p.peek() == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.skipSpace(false)
		if err := p.keyValue(out); err != nil {
			return nil, err
		}
		p.skipSpace(false)
		switch p.peek() {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return out, nil
		default:
			return nil, p.errorf("expected , or } in inline table")
		}
	}
}

func (p *parser) literalString() (string, error) {
	p.pos++ // '
	end := strings.IndexAny(p.src[p.pos:], "'\n")
	if end < 0 || p.src[p.pos+end] != '\'' {
		return "", p.errorf("unterminated string")
	}
	s := p.src[p.pos : p.pos+end]
	p.pos += end + 1
	return s, nil
}

func (p *parser) basicString() (string, error) {
	p.pos++ // "
	var b strings.Builder
	for {
		if p.eof() || p.src[p.pos] == '\n' {
			return "", p.errorf("unterminated string")
		}
		c := p.src[p.pos]
		p.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.eof() {
				return "", p.errorf("unterminated string")
			}
			e := p.src[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '"', '\\':
				b.WriteByte(e)
			case 'u', 'U':
				n := 4
				if e == 'U' {
					n = 8
				}
				if p.pos+n > len(p.src) {
					return "", p.errorf("short unicode escape")
				}
				r, err := strconv.ParseUint(p.src[p.pos:p.pos+n], 16, 32)
				if err != nil || !utf8.ValidRune(rune(r)) {
					return "", p.errorf("bad unicode escape")
				}
				b.WriteRune(rune(r))
				p.pos += n
			default:
				return "", p.errorf("bad escape \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
	}
}
