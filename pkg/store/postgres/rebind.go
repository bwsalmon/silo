package postgres

import (
	"strconv"
	"strings"
)

// rebind turns the ? placeholders of the shared SQL subset into
// Postgres's $1, $2, ... A ? inside a string literal, a quoted
// identifier, a dollar-quoted body or a comment is left alone.
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); {
		switch c := q[i]; {
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		case c == '\'' || c == '"':
			// A doubled quote inside is an escaped one, which this
			// reads as the literal ending and another starting.
			end := strings.IndexByte(q[i+1:], c)
			if end < 0 {
				b.WriteString(q[i:])
				return b.String()
			}
			b.WriteString(q[i : i+end+2])
			i += end + 2
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			end := strings.IndexByte(q[i:], '\n')
			if end < 0 {
				end = len(q) - i
			}
			b.WriteString(q[i : i+end])
			i += end
		case c == '/' && strings.HasPrefix(q[i:], "/*"):
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				end = len(q) - i - 2
			} else {
				end += 2
			}
			b.WriteString(q[i : i+2+end])
			i += 2 + end
		case c == '$':
			tag := dollarTag(q[i:])
			if tag == "" {
				b.WriteByte(c)
				i++
				continue
			}
			end := strings.Index(q[i+len(tag):], tag)
			if end < 0 {
				end = len(q) - i - len(tag)
			} else {
				end += len(tag)
			}
			b.WriteString(q[i : i+len(tag)+end])
			i += len(tag) + end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// dollarTag returns the $tag$ that opens a dollar-quoted string at the
// start of s, or "" if s does not start with one.
func dollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '$':
			return s[:j+1]
		case c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || j > 1 && '0' <= c && c <= '9':
		default:
			return ""
		}
	}
	return ""
}
