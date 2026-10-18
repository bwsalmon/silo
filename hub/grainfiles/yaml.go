package grainfiles

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const maxYAMLDepth = 32

var forbiddenYAMLRe = regexp.MustCompile(`(?m)(^|[\s\[{,:])[&*][A-Za-z0-9_-]+|<<\s*:|(^|[\s\[{,:])!`)

type yamlLine struct {
	lineNum int
	indent  int
	content string
	raw     string
}

func parseYAMLFrontMatter(path, src string) (map[string]any, *Finding) {
	if hasForbiddenYAMLConstruct(src) {
		f := newFinding(CodeInvalidYAML, path, "anchors, aliases, merge keys and tags are not allowed")
		return nil, &f
	}

	rawLines := strings.Split(src, "\n")
	var lines []yamlLine
	for i, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "---" || trimmed == "..." {
			f := newFinding(CodeInvalidYAML, path, "front matter must be a single YAML document")
			return nil, &f
		}
		indent := 0
		for j := 0; j < len(raw); j++ {
			if raw[j] == ' ' {
				indent++
			} else if raw[j] == '\t' {
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: tab character in indentation", i+2))
				return nil, &f
			} else {
				break
			}
		}
		content, fErr := stripYAMLComment(raw[indent:], path, i+2)
		if fErr != nil {
			return nil, fErr
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		lines = append(lines, yamlLine{
			lineNum: i + 2,
			indent:  indent,
			content: content,
			raw:     raw,
		})
	}

	if len(lines) == 0 {
		return map[string]any{}, nil
	}

	// Join multi-line flow collections if a line opens unmatched [ or {.
	joined, fErr := joinMultilineFlow(lines, path)
	if fErr != nil {
		return nil, fErr
	}
	lines = joined

	first := lines[0].content
	if strings.HasPrefix(first, "{") {
		if len(lines) > 1 {
			f := newFinding(CodeMalformedFile, path, "YAML: unexpected content after top-level flow mapping")
			return nil, &f
		}
		val, rest, fErr := parseFlowValue(first, path, lines[0].lineNum, 0)
		if fErr != nil {
			return nil, fErr
		}
		if strings.TrimSpace(rest) != "" {
			f := newFinding(CodeMalformedFile, path, "YAML: unexpected trailing characters after flow mapping")
			return nil, &f
		}
		m, ok := val.(map[string]any)
		if !ok {
			f := newFinding(CodeInvalidYAML, path, "front matter is not a mapping")
			return nil, &f
		}
		return m, nil
	}

	if strings.HasPrefix(first, "-") && (len(first) == 1 || first[1] == ' ') || strings.HasPrefix(first, "[") {
		f := newFinding(CodeInvalidYAML, path, "front matter is not a mapping")
		return nil, &f
	}

	if _, _, ok, fErr := splitKeyValue(first, path, lines[0].lineNum); fErr != nil {
		return nil, fErr
	} else if !ok {
		f := newFinding(CodeInvalidYAML, path, "front matter is not a mapping")
		return nil, &f
	}

	val, nextIdx, fErr := parseBlockMapping(lines, 0, lines[0].indent, rawLines, path, 0)
	if fErr != nil {
		return nil, fErr
	}
	if nextIdx < len(lines) {
		f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected indentation", lines[nextIdx].lineNum))
		return nil, &f
	}
	return val, nil
}

// hasForbiddenYAMLConstruct checks for anchors (&), aliases (*), merge keys (<<:),
// and explicit tags (!) outside of single- or double-quoted strings.
func hasForbiddenYAMLConstruct(src string) bool {
	cleaned := stripQuotedContent(src)
	return forbiddenYAMLRe.MatchString(cleaned)
}

func stripQuotedContent(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inSingle := false
	inDouble := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if inSingle {
			if ch == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
				b.WriteByte('\'')
			}
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(src) {
				i++
				continue
			}
			if ch == '"' {
				inDouble = false
				b.WriteByte('"')
			}
			continue
		}
		if ch == '#' && (i == 0 || src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n') {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				b.WriteByte('\n')
			}
			continue
		}
		if ch == '\'' {
			inSingle = true
			b.WriteByte('\'')
			continue
		}
		if ch == '"' {
			inDouble = true
			b.WriteByte('"')
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func stripYAMLComment(s, path string, lineNum int) (string, *Finding) {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				if i+1 >= len(s) {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: trailing backslash in double-quoted string", lineNum))
					return "", &f
				}
				i++
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		if ch == '\'' {
			inSingle = true
			continue
		}
		if ch == '"' {
			inDouble = true
			continue
		}
		if ch == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i], nil
		}
	}
	if inSingle || inDouble {
		f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unclosed quoted string", lineNum))
		return "", &f
	}
	return s, nil
}

func joinMultilineFlow(lines []yamlLine, path string) ([]yamlLine, *Finding) {
	var out []yamlLine
	for i := 0; i < len(lines); i++ {
		cur := lines[i]
		depth := flowBracketDepth(cur.content)
		if depth < 0 {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unmatched closing bracket", cur.lineNum))
			return nil, &f
		}
		for depth > 0 {
			i++
			if i >= len(lines) {
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unclosed flow collection", cur.lineNum))
				return nil, &f
			}
			cur.content += " " + lines[i].content
			depth = flowBracketDepth(cur.content)
			if depth < 0 {
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unmatched closing bracket", lines[i].lineNum))
				return nil, &f
			}
		}
		out = append(out, cur)
	}
	return out, nil
}

func flowBracketDepth(s string) int {
	depth := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return -1
			}
		}
	}
	return depth
}

func parseBlockMapping(lines []yamlLine, startIdx, baseIndent int, rawLines []string, path string, depth int) (map[string]any, int, *Finding) {
	if depth > maxYAMLDepth {
		f := newFinding(CodeLimitExceeded, path, fmt.Sprintf("YAML: nesting depth exceeds maximum (%d)", maxYAMLDepth))
		return nil, startIdx, &f
	}
	out := make(map[string]any)
	i := startIdx
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < baseIndent {
			break
		}
		if ln.indent > baseIndent {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected indentation", ln.lineNum))
			return nil, i, &f
		}
		rawKey, rawVal, ok, fErr := splitKeyValue(ln.content, path, ln.lineNum)
		if fErr != nil {
			return nil, i, fErr
		}
		if !ok {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: expected mapping key: value", ln.lineNum))
			return nil, i, &f
		}
		key, fErr := parseMappingKey(rawKey, path, ln.lineNum)
		if fErr != nil {
			return nil, i, fErr
		}
		if _, exists := out[key]; exists {
			f := newFinding(CodeInvalidYAML, path, fmt.Sprintf("duplicate key %q", key))
			return nil, i, &f
		}

		if rawVal == "|" || rawVal == "|-" || rawVal == "|+" || rawVal == ">" || rawVal == ">-" || rawVal == ">+" {
			strVal, nextIdx, fErr := parseBlockScalar(lines, i, baseIndent, rawVal, rawLines, path)
			if fErr != nil {
				return nil, i, fErr
			}
			out[key] = strVal
			i = nextIdx
			continue
		}

		if rawVal != "" {
			val, fErr := parseInlineValue(rawVal, path, ln.lineNum, depth+1)
			if fErr != nil {
				return nil, i, fErr
			}
			out[key] = val
			i++
			continue
		}

		// Empty rawVal: look ahead for an indented block on subsequent lines.
		i++
		if i >= len(lines) || lines[i].indent <= baseIndent {
			if i < len(lines) && lines[i].indent == baseIndent && isSequenceItem(lines[i].content) {
				seqVal, nextIdx, fErr := parseBlockSequence(lines, i, baseIndent, rawLines, path, depth+1)
				if fErr != nil {
					return nil, i, fErr
				}
				out[key] = seqVal
				i = nextIdx
				continue
			}
			out[key] = nil
			continue
		}

		childIndent := lines[i].indent
		if isSequenceItem(lines[i].content) {
			seqVal, nextIdx, fErr := parseBlockSequence(lines, i, childIndent, rawLines, path, depth+1)
			if fErr != nil {
				return nil, i, fErr
			}
			out[key] = seqVal
			i = nextIdx
		} else {
			mapVal, nextIdx, fErr := parseBlockMapping(lines, i, childIndent, rawLines, path, depth+1)
			if fErr != nil {
				return nil, i, fErr
			}
			out[key] = mapVal
			i = nextIdx
		}
	}
	return out, i, nil
}

func isSequenceItem(content string) bool {
	return content == "-" || strings.HasPrefix(content, "- ") || strings.HasPrefix(content, "-\t")
}

func parseBlockSequence(lines []yamlLine, startIdx, baseIndent int, rawLines []string, path string, depth int) ([]any, int, *Finding) {
	if depth > maxYAMLDepth {
		f := newFinding(CodeLimitExceeded, path, fmt.Sprintf("YAML: nesting depth exceeds maximum (%d)", maxYAMLDepth))
		return nil, startIdx, &f
	}
	var out []any
	i := startIdx
	for i < len(lines) {
		ln := lines[i]
		if ln.indent < baseIndent {
			break
		}
		if ln.indent > baseIndent {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected indentation in sequence", ln.lineNum))
			return nil, i, &f
		}
		if !isSequenceItem(ln.content) {
			break
		}
		itemText := strings.TrimSpace(ln.content[1:])
		if itemText == "" {
			i++
			if i >= len(lines) || lines[i].indent <= baseIndent {
				out = append(out, nil)
				continue
			}
			childIndent := lines[i].indent
			if isSequenceItem(lines[i].content) {
				nestedSeq, nextIdx, fErr := parseBlockSequence(lines, i, childIndent, rawLines, path, depth+1)
				if fErr != nil {
					return nil, i, fErr
				}
				out = append(out, nestedSeq)
				i = nextIdx
			} else {
				nestedMap, nextIdx, fErr := parseBlockMapping(lines, i, childIndent, rawLines, path, depth+1)
				if fErr != nil {
					return nil, i, fErr
				}
				out = append(out, nestedMap)
				i = nextIdx
			}
			continue
		}

		// Check if itemText is itself an inline mapping key: value (e.g. "- key: val").
		if !strings.HasPrefix(itemText, `"`) && !strings.HasPrefix(itemText, `'`) &&
			!strings.HasPrefix(itemText, `[`) && !strings.HasPrefix(itemText, `{`) {
			if kRaw, vRaw, ok, _ := splitKeyValue(itemText, path, ln.lineNum); ok {
				// Treat as a mapping starting on this line with virtual indent ln.indent + 2.
				synthLines := make([]yamlLine, 0, len(lines)-i+1)
				synthLines = append(synthLines, yamlLine{
					lineNum: ln.lineNum,
					indent:  ln.indent + 2,
					content: kRaw + ": " + vRaw,
					raw:     ln.raw,
				})
				synthLines = append(synthLines, lines[i+1:]...)
				nestedMap, consumed, fErr := parseBlockMapping(synthLines, 0, ln.indent+2, rawLines, path, depth+1)
				if fErr != nil {
					return nil, i, fErr
				}
				out = append(out, nestedMap)
				i = i + consumed
				continue
			}
		}

		val, fErr := parseInlineValue(itemText, path, ln.lineNum, depth+1)
		if fErr != nil {
			return nil, i, fErr
		}
		out = append(out, val)
		i++
	}
	return out, i, nil
}

func parseBlockScalar(lines []yamlLine, headerIdx, parentIndent int, indicator string, rawLines []string, path string) (string, int, *Finding) {
	nextIdx := headerIdx + 1
	for nextIdx < len(lines) && lines[nextIdx].indent > parentIndent {
		nextIdx++
	}
	startLine := lines[headerIdx].lineNum // 1-based in grainfile (rawLines is 0-based for front matter, where lineNum = rawIdx + 2)
	endRawIdx := len(rawLines)
	if nextIdx < len(lines) {
		endRawIdx = lines[nextIdx].lineNum - 2
	}
	startRawIdx := startLine - 1
	if startRawIdx >= endRawIdx {
		return "", nextIdx, nil
	}

	blockIndent := -1
	for rIdx := startRawIdx; rIdx < endRawIdx; rIdx++ {
		rl := rawLines[rIdx]
		if strings.TrimSpace(rl) == "" {
			continue
		}
		ind := 0
		for ind < len(rl) && rl[ind] == ' ' {
			ind++
		}
		if ind <= parentIndent {
			endRawIdx = rIdx
			break
		}
		if blockIndent < 0 || ind < blockIndent {
			blockIndent = ind
		}
	}
	if blockIndent < 0 {
		return "", nextIdx, nil
	}

	var parts []string
	for rIdx := startRawIdx; rIdx < endRawIdx; rIdx++ {
		rl := rawLines[rIdx]
		if len(rl) >= blockIndent {
			parts = append(parts, rl[blockIndent:])
		} else {
			parts = append(parts, "")
		}
	}

	sep := "\n"
	if strings.HasPrefix(indicator, ">") {
		sep = " "
	}
	joined := strings.Join(parts, sep)
	if strings.HasSuffix(indicator, "-") {
		return strings.TrimRight(joined, "\n "), nextIdx, nil
	}
	return strings.TrimRight(joined, "\n") + "\n", nextIdx, nil
}

func splitKeyValue(s, path string, lineNum int) (string, string, bool, *Finding) {
	inSingle := false
	inDouble := false
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
				keyPart := strings.TrimSpace(s[:i])
				valPart := strings.TrimSpace(s[i+1:])
				if keyPart == "" {
					f := newFinding(CodeInvalidYAML, path, "non-string key")
					return "", "", false, &f
				}
				return keyPart, valPart, true, nil
			}
		}
	}
	return "", "", false, nil
}

func parseMappingKey(rawKey, path string, lineNum int) (string, *Finding) {
	if rawKey == "<<" {
		f := newFinding(CodeInvalidYAML, path, "anchors, aliases, merge keys and tags are not allowed")
		return "", &f
	}
	if strings.HasPrefix(rawKey, "[") || strings.HasPrefix(rawKey, "{") {
		f := newFinding(CodeInvalidYAML, path, "non-string key")
		return "", &f
	}
	if strings.HasPrefix(rawKey, `"`) {
		s, rest, fErr := parseDoubleQuoted(rawKey, path, lineNum)
		if fErr != nil {
			return "", fErr
		}
		if strings.TrimSpace(rest) != "" {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid key %s", lineNum, rawKey))
			return "", &f
		}
		return s, nil
	}
	if strings.HasPrefix(rawKey, `'`) {
		s, rest, fErr := parseSingleQuoted(rawKey, path, lineNum)
		if fErr != nil {
			return "", fErr
		}
		if strings.TrimSpace(rest) != "" {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid key %s", lineNum, rawKey))
			return "", &f
		}
		return s, nil
	}

	// Unquoted key: in YAML 1.2, bare true/false, null/~, and numbers resolve
	// to non-string tags, which §5.3 forbids for mapping keys (GF004).
	// Notice bare `on`, `off`, `yes`, `no`, `y`, `n` ARE strings in YAML 1.2!
	val := parsePlainScalar(rawKey)
	s, ok := val.(string)
	if !ok {
		f := newFinding(CodeInvalidYAML, path, "non-string key")
		return "", &f
	}
	return s, nil
}

func parseInlineValue(rawVal, path string, lineNum, depth int) (any, *Finding) {
	rawVal = strings.TrimSpace(rawVal)
	if strings.HasPrefix(rawVal, "[") || strings.HasPrefix(rawVal, "{") ||
		strings.HasPrefix(rawVal, `"`) || strings.HasPrefix(rawVal, `'`) {
		v, rest, fErr := parseFlowValue(rawVal, path, lineNum, depth)
		if fErr != nil {
			return nil, fErr
		}
		if strings.TrimSpace(rest) != "" {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected trailing content %q", lineNum, rest))
			return nil, &f
		}
		return v, nil
	}
	// Check for an unquoted colon followed by space inside a plain scalar, which is an invalid nested mapping on the same line.
	if _, _, ok, _ := splitKeyValue(rawVal, path, lineNum); ok {
		f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected mapping in scalar value", lineNum))
		return nil, &f
	}
	return parsePlainScalar(rawVal), nil
}

func parseFlowValue(s, path string, lineNum, depth int) (any, string, *Finding) {
	if depth > maxYAMLDepth {
		f := newFinding(CodeLimitExceeded, path, fmt.Sprintf("YAML: line %d: nesting depth exceeds maximum (%d)", lineNum, maxYAMLDepth))
		return nil, "", &f
	}
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if s == "" {
		return nil, "", nil
	}
	switch s[0] {
	case '"':
		return parseDoubleQuoted(s, path, lineNum)
	case '\'':
		return parseSingleQuoted(s, path, lineNum)
	case '[':
		return parseFlowSequence(s, path, lineNum, depth+1)
	case '{':
		return parseFlowMapping(s, path, lineNum, depth+1)
	default:
		end := 0
		for end < len(s) {
			ch := s[end]
			if ch == ',' || ch == ']' || ch == '}' {
				break
			}
			if ch == ':' && (end+1 == len(s) || s[end+1] == ' ' || s[end+1] == '\t' || s[end+1] == ',' || s[end+1] == '}') {
				break
			}
			end++
		}
		token := strings.TrimSpace(s[:end])
		rest := s[end:]
		if token == "" {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unexpected token in flow collection", lineNum))
			return nil, "", &f
		}
		return parsePlainScalar(token), rest, nil
	}
}

func parseFlowSequence(s, path string, lineNum, depth int) ([]any, string, *Finding) {
	if depth > maxYAMLDepth {
		f := newFinding(CodeLimitExceeded, path, fmt.Sprintf("YAML: line %d: nesting depth exceeds maximum (%d)", lineNum, maxYAMLDepth))
		return nil, "", &f
	}
	rest := strings.TrimLeftFunc(s[1:], unicode.IsSpace)
	out := make([]any, 0)
	if strings.HasPrefix(rest, "]") {
		return out, rest[1:], nil
	}
	for {
		val, after, fErr := parseFlowValue(rest, path, lineNum, depth)
		if fErr != nil {
			return nil, "", fErr
		}
		out = append(out, val)
		rest = strings.TrimLeftFunc(after, unicode.IsSpace)
		if strings.HasPrefix(rest, ",") {
			rest = strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
			if strings.HasPrefix(rest, "]") {
				return out, rest[1:], nil
			}
			continue
		}
		if strings.HasPrefix(rest, "]") {
			return out, rest[1:], nil
		}
		f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: expected ',' or ']' in flow sequence", lineNum))
		return nil, "", &f
	}
}

func parseFlowMapping(s, path string, lineNum, depth int) (map[string]any, string, *Finding) {
	if depth > maxYAMLDepth {
		f := newFinding(CodeLimitExceeded, path, fmt.Sprintf("YAML: line %d: nesting depth exceeds maximum (%d)", lineNum, maxYAMLDepth))
		return nil, "", &f
	}
	rest := strings.TrimLeftFunc(s[1:], unicode.IsSpace)
	out := make(map[string]any)
	if strings.HasPrefix(rest, "}") {
		return out, rest[1:], nil
	}
	for {
		var key string
		var fErr *Finding
		if strings.HasPrefix(rest, `"`) {
			key, rest, fErr = parseDoubleQuoted(rest, path, lineNum)
			if fErr != nil {
				return nil, "", fErr
			}
		} else if strings.HasPrefix(rest, `'`) {
			key, rest, fErr = parseSingleQuoted(rest, path, lineNum)
			if fErr != nil {
				return nil, "", fErr
			}
		} else {
			colonIdx := -1
			for i := 0; i < len(rest); i++ {
				if rest[i] == ':' && (i+1 == len(rest) || rest[i+1] == ' ' || rest[i+1] == '\t' || rest[i+1] == '}' || rest[i+1] == ',') {
					colonIdx = i
					break
				}
				if rest[i] == ',' || rest[i] == '}' || rest[i] == '[' || rest[i] == '{' {
					break
				}
			}
			if colonIdx < 0 {
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: expected ':' in flow mapping", lineNum))
				return nil, "", &f
			}
			rawKey := strings.TrimSpace(rest[:colonIdx])
			key, fErr = parseMappingKey(rawKey, path, lineNum)
			if fErr != nil {
				return nil, "", fErr
			}
			rest = rest[colonIdx:]
		}

		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		if !strings.HasPrefix(rest, ":") {
			f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: expected ':' after key in flow mapping", lineNum))
			return nil, "", &f
		}
		if _, exists := out[key]; exists {
			f := newFinding(CodeInvalidYAML, path, fmt.Sprintf("duplicate key %q", key))
			return nil, "", &f
		}
		rest = strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
		if strings.HasPrefix(rest, ",") || strings.HasPrefix(rest, "}") {
			out[key] = nil
		} else {
			val, after, fErr := parseFlowValue(rest, path, lineNum, depth)
			if fErr != nil {
				return nil, "", fErr
			}
			out[key] = val
			rest = strings.TrimLeftFunc(after, unicode.IsSpace)
		}

		if strings.HasPrefix(rest, ",") {
			rest = strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
			if strings.HasPrefix(rest, "}") {
				return out, rest[1:], nil
			}
			continue
		}
		if strings.HasPrefix(rest, "}") {
			return out, rest[1:], nil
		}
		f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: expected ',' or '}' in flow mapping", lineNum))
		return nil, "", &f
	}
}

func parseDoubleQuoted(s, path string, lineNum int) (string, string, *Finding) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			return b.String(), s[i+1:], nil
		}
		if ch == '\\' {
			i++
			if i >= len(s) {
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unterminated escape in double-quoted string", lineNum))
				return "", "", &f
			}
			esc := s[i]
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			case 'x':
				if i+2 >= len(s) {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\x escape", lineNum))
					return "", "", &f
				}
				v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if err != nil {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\x escape", lineNum))
					return "", "", &f
				}
				b.WriteByte(byte(v))
				i += 2
			case 'u':
				if i+4 >= len(s) {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\u escape", lineNum))
					return "", "", &f
				}
				v, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
				if err != nil {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\u escape", lineNum))
					return "", "", &f
				}
				b.WriteRune(rune(v))
				i += 4
			case 'U':
				if i+8 >= len(s) {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\U escape", lineNum))
					return "", "", &f
				}
				v, err := strconv.ParseUint(s[i+1:i+9], 16, 32)
				if err != nil {
					f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: invalid \\U escape", lineNum))
					return "", "", &f
				}
				b.WriteRune(rune(v))
				i += 8
			default:
				f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unknown escape \\%c", lineNum, esc))
				return "", "", &f
			}
			continue
		}
		b.WriteByte(ch)
	}
	f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unclosed double-quoted string", lineNum))
	return "", "", &f
}

func parseSingleQuoted(s, path string, lineNum int) (string, string, *Finding) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String(), s[i+1:], nil
		}
		b.WriteByte(ch)
	}
	f := newFinding(CodeMalformedFile, path, fmt.Sprintf("YAML: line %d: unclosed single-quoted string", lineNum))
	return "", "", &f
}

func parsePlainScalar(s string) any {
	switch s {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	case "null", "Null", "NULL", "~":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if strings.Contains(s, ".") {
		if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".") {
			return f
		}
	}
	return s
}
