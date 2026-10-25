package approval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Canonical encodes v as RFC 8785 (JSON Canonicalization Scheme) JSON: v is
// marshalled with encoding/json, then re-encoded with object members sorted
// by their UTF-16 code units, no insignificant whitespace, and strings
// escaped minimally. The batch hash is sha256 over this encoding (design
// §4.3), so the same batch always hashes the same whatever produced it.
//
// Numbers must be integers of at most 2^53 in magnitude, which is all a
// batch holds; anything else is an error rather than a guess at ES6
// number formatting.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := canonValue(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func canonValue(buf *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(v))
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil || n > 1<<53 || n < -(1<<53) {
			return fmt.Errorf("canonical json: number %s is not a safe integer", v)
		}
		buf.WriteString(strconv.FormatInt(n, 10))
	case string:
		canonString(buf, v)
	case []any:
		buf.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := canonValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			canonString(buf, k)
			buf.WriteByte(':')
			if err := canonValue(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical json: unexpected %T", v)
	}
	return nil
}

// canonString writes s the way RFC 8785 (via ECMAScript's JSON.stringify)
// does: only '"', '\\' and control characters are escaped.
func canonString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '"':
			buf.WriteString(`\"`)
		case r == '\\':
			buf.WriteString(`\\`)
		case r == '\b':
			buf.WriteString(`\b`)
		case r == '\f':
			buf.WriteString(`\f`)
		case r == '\n':
			buf.WriteString(`\n`)
		case r == '\r':
			buf.WriteString(`\r`)
		case r == '\t':
			buf.WriteString(`\t`)
		case r < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hex[r>>4])
			buf.WriteByte(hex[r&0xf])
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
