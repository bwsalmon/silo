package grainfiles

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsTemplatePath reports whether relPath is a .task.grain.md.jinja2 or
// .process.grain.md.jinja2 template path.
func IsTemplatePath(relPath string) bool {
	norm := strings.ToLower(strings.TrimSpace(relPath))
	return strings.HasSuffix(norm, ".task.grain.md.jinja2") ||
		strings.HasSuffix(norm, ".process.grain.md.jinja2")
}

// StripTemplateSuffix strips a trailing ".jinja2" suffix if relPath ends with
// ".grain.md.jinja2" and reports whether it was a template path.
func StripTemplateSuffix(relPath string) (stripped string, isTemplate bool) {
	if strings.HasSuffix(relPath, ".grain.md.jinja2") {
		return strings.TrimSuffix(relPath, ".jinja2"), true
	}
	return relPath, false
}

// RenderTemplate parses, validates, discovers undeclared variables in, and
// renders a sandboxed Jinja2 Grainfile template (<name>.task.grain.md.jinja2 or
// <name>.process.grain.md.jinja2).
//
// Supported constructs:
//   - variable interpolation: {{ var }}
//   - filters: default (and alias d), lower, upper, trim, replace
//   - comments: {# ... #}
//   - conditionals: {% if ... %}...{% elif ... %}...{% else %}...{% endif %}
//   - loops: {% for x in ... %}...{% endfor %}
//
// Forbidden tags (include, extends, import, from) fail with CodeCycle (GF034).
// Syntax or evaluation errors fail with CodeTemplateSyntax (GF033).
// Input or rendered output exceeding MaxFileBytes fails with CodeLimitExceeded (GF006).
func RenderTemplate(raw string, vars map[string]string) (string, []string, error) {
	return renderTemplateAtPath("", raw, vars)
}

func renderTemplateAtPath(path, raw string, vars map[string]string) (string, []string, error) {
	if len(raw) > MaxFileBytes {
		return "", nil, Errors{newFinding(CodeLimitExceeded, path, "grainfile template exceeds 1 MiB")}
	}
	if strings.HasPrefix(raw, "version https://git-lfs") {
		return "", nil, Errors{newFinding(CodeMalformedFile, path, "a grainfile may not be an LFS pointer")}
	}
	if strings.IndexByte(raw, 0) >= 0 || strings.HasPrefix(raw, "\xef\xbb\xbf") {
		return "", nil, Errors{newFinding(CodeMalformedFile, path, "NUL or byte-order mark")}
	}
	if !utf8.ValidString(raw) {
		return "", nil, Errors{newFinding(CodeMalformedFile, path, "not UTF-8")}
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")

	tokens, fErr := lexTemplate(path, raw)
	if fErr != nil {
		return "", nil, Errors{*fErr}
	}

	nodes, undeclared, fErr := parseTemplateAST(path, tokens)
	if fErr != nil {
		return "", nil, Errors{*fErr}
	}

	env := make(map[string]tmplVal, len(vars))
	for k, v := range vars {
		env[k] = tmplVal{defined: true, kind: valString, str: v}
	}

	var out strings.Builder
	if fErr := evalNodes(path, nodes, env, &out); fErr != nil {
		return "", undeclared, Errors{*fErr}
	}

	rendered := out.String()
	if len(rendered) > MaxFileBytes {
		return "", undeclared, Errors{newFinding(CodeLimitExceeded, path, "rendered grainfile template exceeds 1 MiB")}
	}
	return rendered, undeclared, nil
}

type tokenKind int

const (
	tokText tokenKind = iota
	tokExpr           // {{ ... }}
	tokStmt           // {% ... %}
)

type tmplToken struct {
	kind      tokenKind
	content   string
	trimLeft  bool
	trimRight bool
}

func lexTemplate(path, src string) ([]tmplToken, *Finding) {
	var rawTokens []tmplToken
	i := 0
	n := len(src)

	for i < n {
		nextOpen := -1
		openKind := ""
		for j := i; j+1 < n; j++ {
			if src[j] == '{' && (src[j+1] == '{' || src[j+1] == '%' || src[j+1] == '#') {
				nextOpen = j
				openKind = src[j : j+2]
				break
			}
		}
		if nextOpen < 0 {
			rawTokens = append(rawTokens, tmplToken{kind: tokText, content: src[i:]})
			break
		}
		if nextOpen > i {
			rawTokens = append(rawTokens, tmplToken{kind: tokText, content: src[i:nextOpen]})
		}

		var closeSeq string
		switch openKind {
		case "{{":
			closeSeq = "}}"
		case "{%":
			closeSeq = "%}"
		case "{#":
			closeSeq = "#}"
		}

		bodyStart := nextOpen + 2
		trimL := false
		if bodyStart < n && src[bodyStart] == '-' {
			trimL = true
			bodyStart++
		}

		closeIdx := findTagClose(src, bodyStart, closeSeq)
		if closeIdx < 0 {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("unclosed Jinja2 %s tag", openKind))
			return nil, &f
		}

		bodyEnd := closeIdx
		trimR := false
		if bodyEnd > bodyStart && src[bodyEnd-1] == '-' {
			trimR = true
			bodyEnd--
		}

		inner := strings.TrimSpace(src[bodyStart:bodyEnd])
		switch openKind {
		case "{#":
			// Comment is stripped, but keep a placeholder token during the whitespace
			// trim pass so a following {%- or {{- does not trim text across the comment.
			rawTokens = append(rawTokens, tmplToken{
				kind:      tokStmt,
				content:   "",
				trimLeft:  trimL,
				trimRight: trimR,
			})
		case "{{":
			if inner == "" {
				f := newFinding(CodeTemplateSyntax, path, "empty Jinja2 expression {{ }}")
				return nil, &f
			}
			rawTokens = append(rawTokens, tmplToken{
				kind:      tokExpr,
				content:   inner,
				trimLeft:  trimL,
				trimRight: trimR,
			})
		case "{%":
			if inner == "" {
				f := newFinding(CodeTemplateSyntax, path, "empty Jinja2 statement {% %}")
				return nil, &f
			}
			rawTokens = append(rawTokens, tmplToken{
				kind:      tokStmt,
				content:   inner,
				trimLeft:  trimL,
				trimRight: trimR,
			})
		}

		i = closeIdx + 2
	}

	// Apply {%- / -%} / {{- / -}} whitespace trimming on adjacent text tokens.
	for idx := 0; idx < len(rawTokens); idx++ {
		t := rawTokens[idx]
		if t.kind == tokText {
			continue
		}
		if t.trimLeft && idx > 0 && rawTokens[idx-1].kind == tokText {
			rawTokens[idx-1].content = strings.TrimRightFunc(rawTokens[idx-1].content, unicode.IsSpace)
		}
		if t.trimRight && idx+1 < len(rawTokens) && rawTokens[idx+1].kind == tokText {
			rawTokens[idx+1].content = strings.TrimLeftFunc(rawTokens[idx+1].content, unicode.IsSpace)
		}
	}

	var tokens []tmplToken
	for _, t := range rawTokens {
		if t.kind == tokStmt && t.content == "" {
			continue
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

func findTagClose(src string, start int, closeSeq string) int {
	inQuote := byte(0)
	escaped := false
	for i := start; i+1 < len(src); i++ {
		ch := src[i]
		if inQuote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if closeSeq != "#}" && (ch == '\'' || ch == '"') {
			inQuote = ch
			continue
		}
		if src[i:i+2] == closeSeq {
			return i
		}
	}
	return -1
}

type astNode interface {
	isASTNode()
}

type textNode struct {
	text string
}

func (textNode) isASTNode() {}

type exprNode struct {
	expr *pipelineExpr
}

func (exprNode) isASTNode() {}

type ifBranch struct {
	cond *condExpr // nil for else
	body []astNode
}

type ifNode struct {
	branches []ifBranch
}

func (ifNode) isASTNode() {}

type forNode struct {
	loopVar  string
	iterable *pipelineExpr
	body     []astNode
}

func (forNode) isASTNode() {}

func parseTemplateAST(path string, tokens []tmplToken) ([]astNode, []string, *Finding) {
	idx := 0
	var undeclaredSet = map[string]bool{}
	var scopeStack []map[string]bool

	isBound := func(ident string) bool {
		for i := len(scopeStack) - 1; i >= 0; i-- {
			if scopeStack[i][ident] {
				return true
			}
		}
		return false
	}
	recordVar := func(ident string) {
		if ident != "" && !isBound(ident) {
			undeclaredSet[ident] = true
		}
	}

	var parseBlock func(stopTags map[string]bool) ([]astNode, string, string, *Finding)
	parseBlock = func(stopTags map[string]bool) ([]astNode, string, string, *Finding) {
		var nodes []astNode
		for idx < len(tokens) {
			tok := tokens[idx]
			switch tok.kind {
			case tokText:
				idx++
				nodes = append(nodes, textNode{text: tok.content})
			case tokExpr:
				idx++
				pe, fErr := parsePipelineExpr(path, tok.content, recordVar)
				if fErr != nil {
					return nil, "", "", fErr
				}
				nodes = append(nodes, exprNode{expr: pe})
			case tokStmt:
				kw, rest := splitStmtKeyword(tok.content)
				switch kw {
				case "include", "extends", "import", "from":
					f := newFinding(CodeCycle, path, fmt.Sprintf("forbidden Jinja2 tag %q in sandboxed grainfile template", kw))
					return nil, "", "", &f
				}
				if stopTags != nil && stopTags[kw] {
					idx++
					return nodes, kw, rest, nil
				}
				switch kw {
				case "if":
					idx++
					if strings.TrimSpace(rest) == "" {
						f := newFinding(CodeTemplateSyntax, path, "missing condition in {% if %}")
						return nil, "", "", &f
					}
					cond, fErr := parseCondExpr(path, rest, recordVar)
					if fErr != nil {
						return nil, "", "", fErr
					}
					var branches []ifBranch
					curCond := cond
					sawElse := false
					for {
						stops := map[string]bool{"elif": true, "else": true, "endif": true}
						if sawElse {
							stops = map[string]bool{"endif": true}
						}
						body, endKw, endRest, fErr := parseBlock(stops)
						if fErr != nil {
							return nil, "", "", fErr
						}
						if endKw == "" {
							f := newFinding(CodeTemplateSyntax, path, "unclosed {% if %} block (missing {% endif %})")
							return nil, "", "", &f
						}
						branches = append(branches, ifBranch{cond: curCond, body: body})
						if endKw == "endif" {
							if strings.TrimSpace(endRest) != "" {
								f := newFinding(CodeTemplateSyntax, path, "unexpected arguments in {% endif %}")
								return nil, "", "", &f
							}
							break
						}
						if endKw == "elif" {
							if strings.TrimSpace(endRest) == "" {
								f := newFinding(CodeTemplateSyntax, path, "missing condition in {% elif %}")
								return nil, "", "", &f
							}
							nextCond, fErr := parseCondExpr(path, endRest, recordVar)
							if fErr != nil {
								return nil, "", "", fErr
							}
							curCond = nextCond
						} else if endKw == "else" {
							if strings.TrimSpace(endRest) != "" {
								f := newFinding(CodeTemplateSyntax, path, "unexpected arguments in {% else %}")
								return nil, "", "", &f
							}
							curCond = nil
							sawElse = true
						}
					}
					nodes = append(nodes, ifNode{branches: branches})

				case "for":
					idx++
					loopVar, iterStr, ok := parseForHeader(rest)
					if !ok {
						f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed {%% for %%} statement: %q", tok.content))
						return nil, "", "", &f
					}
					iterExpr, fErr := parsePipelineExpr(path, iterStr, recordVar)
					if fErr != nil {
						return nil, "", "", fErr
					}
					scopeStack = append(scopeStack, map[string]bool{loopVar: true, "loop": true})
					body, endKw, endRest, fErr := parseBlock(map[string]bool{"endfor": true})
					scopeStack = scopeStack[:len(scopeStack)-1]
					if fErr != nil {
						return nil, "", "", fErr
					}
					if endKw != "endfor" {
						f := newFinding(CodeTemplateSyntax, path, "unclosed {% for %} block (missing {% endfor %})")
						return nil, "", "", &f
					}
					if strings.TrimSpace(endRest) != "" {
						f := newFinding(CodeTemplateSyntax, path, "unexpected arguments in {% endfor %}")
						return nil, "", "", &f
					}
					nodes = append(nodes, forNode{loopVar: loopVar, iterable: iterExpr, body: body})

				default:
					f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("unexpected or unsupported Jinja2 statement %q", tok.content))
					return nil, "", "", &f
				}
			}
		}
		return nodes, "", "", nil
	}

	nodes, _, _, fErr := parseBlock(nil)
	if fErr != nil {
		return nil, nil, fErr
	}

	undeclared := make([]string, 0, len(undeclaredSet))
	for k := range undeclaredSet {
		undeclared = append(undeclared, k)
	}
	sort.Strings(undeclared)
	return nodes, undeclared, nil
}

func splitStmtKeyword(s string) (string, string) {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if unicode.IsSpace(rune(s[i])) {
			return s[:i], strings.TrimSpace(s[i+1:])
		}
	}
	return s, ""
}

func parseForHeader(rest string) (loopVar, iterExpr string, ok bool) {
	// Expect: <ident> in <expr>
	rest = strings.TrimSpace(rest)
	i := 0
	for i < len(rest) && !unicode.IsSpace(rune(rest[i])) {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	loopVar = rest[:i]
	if !isValidIdent(loopVar) {
		return "", "", false
	}
	rem := strings.TrimSpace(rest[i:])
	if !strings.HasPrefix(rem, "in") || (len(rem) > 2 && !unicode.IsSpace(rune(rem[2]))) {
		return "", "", false
	}
	iterExpr = strings.TrimSpace(rem[2:])
	if iterExpr == "" {
		return "", "", false
	}
	return loopVar, iterExpr, true
}

func isValidIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(unicode.IsLetter(r) || r == '_') {
				return false
			}
		} else {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
				return false
			}
		}
	}
	switch s {
	case "true", "false", "True", "False", "none", "None", "and", "or", "not", "in", "if", "elif", "else", "for", "endfor", "endif":
		return false
	}
	return true
}

type valKind int

const (
	valUndefined valKind = iota
	valString
	valBool
	valNumber
	valList
)

type tmplVal struct {
	defined bool
	kind    valKind
	str     string
	b       bool
	num     float64
	list    []tmplVal
}

func (v tmplVal) String() string {
	if !v.defined {
		return ""
	}
	switch v.kind {
	case valString:
		return v.str
	case valBool:
		if v.b {
			return "True"
		}
		return "False"
	case valNumber:
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	case valList:
		parts := make([]string, len(v.list))
		for i, item := range v.list {
			parts[i] = item.String()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return ""
	}
}

func (v tmplVal) Truthy() bool {
	if !v.defined {
		return false
	}
	switch v.kind {
	case valString:
		return v.str != ""
	case valBool:
		return v.b
	case valNumber:
		return v.num != 0
	case valList:
		return len(v.list) > 0
	default:
		return false
	}
}

type atomExpr struct {
	isVar   bool
	varName string
	literal tmplVal
	list    []*pipelineExpr
}

type filterCall struct {
	name string
	args []*pipelineExpr
}

type pipelineExpr struct {
	base    *atomExpr
	filters []filterCall
}

func parsePipelineExpr(path, s string, recordVar func(string)) (*pipelineExpr, *Finding) {
	parts, ok := splitTopLevel(s, '|')
	if !ok || len(parts) == 0 {
		f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed Jinja2 expression %q", s))
		return nil, &f
	}
	base, fErr := parseAtomExpr(path, strings.TrimSpace(parts[0]), recordVar)
	if fErr != nil {
		return nil, fErr
	}
	pe := &pipelineExpr{base: base}
	for _, rawFilter := range parts[1:] {
		fc, fErr := parseFilterCall(path, strings.TrimSpace(rawFilter), recordVar)
		if fErr != nil {
			return nil, fErr
		}
		pe.filters = append(pe.filters, fc)
	}
	return pe, nil
}

func isLoopAttr(s string) bool {
	switch s {
	case "loop.index", "loop.index0", "loop.revindex", "loop.revindex0", "loop.first", "loop.last", "loop.length":
		return true
	}
	return false
}

func parseAtomExpr(path, s string, recordVar func(string)) (*atomExpr, *Finding) {
	s = strings.TrimSpace(s)
	if s == "" {
		f := newFinding(CodeTemplateSyntax, path, "empty expression atom")
		return nil, &f
	}
	if (strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2) ||
		(strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") && len(s) >= 2) {
		unq, ok := unquoteJinjaString(s)
		if !ok {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed string literal %s", s))
			return nil, &f
		}
		return &atomExpr{literal: tmplVal{defined: true, kind: valString, str: unq}}, nil
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return &atomExpr{literal: tmplVal{defined: true, kind: valList}}, nil
		}
		items, ok := splitTopLevel(inner, ',')
		if !ok {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed list literal %s", s))
			return nil, &f
		}
		var exprs []*pipelineExpr
		for _, itemStr := range items {
			itemStr = strings.TrimSpace(itemStr)
			if itemStr == "" {
				continue
			}
			pe, fErr := parsePipelineExpr(path, itemStr, recordVar)
			if fErr != nil {
				return nil, fErr
			}
			exprs = append(exprs, pe)
		}
		return &atomExpr{list: exprs, literal: tmplVal{defined: true, kind: valList}}, nil
	}
	switch s {
	case "true", "True":
		return &atomExpr{literal: tmplVal{defined: true, kind: valBool, b: true}}, nil
	case "false", "False":
		return &atomExpr{literal: tmplVal{defined: true, kind: valBool, b: false}}, nil
	case "none", "None":
		return &atomExpr{literal: tmplVal{defined: false, kind: valUndefined}}, nil
	}
	if num, err := strconv.ParseFloat(s, 64); err == nil {
		return &atomExpr{literal: tmplVal{defined: true, kind: valNumber, num: num}}, nil
	}
	if isLoopAttr(s) {
		recordVar("loop")
		return &atomExpr{isVar: true, varName: s}, nil
	}
	if isValidIdent(s) {
		recordVar(s)
		return &atomExpr{isVar: true, varName: s}, nil
	}
	f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("invalid Jinja2 expression %q", s))
	return nil, &f
}

func parseFilterCall(path, s string, recordVar func(string)) (filterCall, *Finding) {
	if s == "" {
		f := newFinding(CodeTemplateSyntax, path, "empty filter after '|'")
		return filterCall{}, &f
	}
	name := s
	var argsStr string
	hasParens := false
	if openIdx := strings.IndexByte(s, '('); openIdx >= 0 {
		if !strings.HasSuffix(s, ")") {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("unclosed filter call %q", s))
			return filterCall{}, &f
		}
		name = strings.TrimSpace(s[:openIdx])
		argsStr = strings.TrimSpace(s[openIdx+1 : len(s)-1])
		hasParens = true
	}
	switch name {
	case "default", "d", "lower", "upper", "trim", "replace":
	default:
		f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("unsupported Jinja2 filter %q", name))
		return filterCall{}, &f
	}
	var args []*pipelineExpr
	if hasParens && argsStr != "" {
		rawArgs, ok := splitTopLevel(argsStr, ',')
		if !ok {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed arguments in filter %q", s))
			return filterCall{}, &f
		}
		for argIdx, ra := range rawArgs {
			ra = strings.TrimSpace(ra)
			// Support optional keyword `boolean=true` / `boolean=false` on default()
			if (name == "default" || name == "d") && argIdx == 1 {
				if eqIdx := strings.IndexByte(ra, '='); eqIdx >= 0 && strings.TrimSpace(ra[:eqIdx]) == "boolean" {
					ra = strings.TrimSpace(ra[eqIdx+1:])
				}
			}
			pe, fErr := parsePipelineExpr(path, ra, recordVar)
			if fErr != nil {
				return filterCall{}, fErr
			}
			args = append(args, pe)
		}
	}
	switch name {
	case "default", "d":
		if len(args) < 1 || len(args) > 2 {
			f := newFinding(CodeTemplateSyntax, path, "filter default() requires 1 or 2 arguments")
			return filterCall{}, &f
		}
	case "lower", "upper", "trim":
		if len(args) != 0 {
			f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("filter %s takes no arguments", name))
			return filterCall{}, &f
		}
	case "replace":
		if len(args) != 2 {
			f := newFinding(CodeTemplateSyntax, path, "filter replace(old, new) requires 2 arguments")
			return filterCall{}, &f
		}
	}
	return filterCall{name: name, args: args}, nil
}

type condKind int

const (
	condOr condKind = iota
	condAnd
	condNot
	condCmp
	condVal
)

type condExpr struct {
	kind  condKind
	op    string // "==", "!=", "in", "not in"
	left  *condExpr
	right *condExpr
	val   *pipelineExpr
}

func stripOuterParens(s string) (string, bool) {
	for {
		s = strings.TrimSpace(s)
		if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
			return s, true
		}
		inQuote := byte(0)
		escaped := false
		parenDepth := 0
		bracketDepth := 0
		wrapsAll := true
		for i := 0; i < len(s); i++ {
			ch := s[i]
			if inQuote != 0 {
				if escaped {
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == inQuote {
					inQuote = 0
				}
				continue
			}
			switch ch {
			case '\'', '"':
				inQuote = ch
			case '(':
				parenDepth++
			case ')':
				parenDepth--
				if parenDepth < 0 {
					return "", false
				}
				if parenDepth == 0 && i < len(s)-1 {
					wrapsAll = false
				}
			case '[':
				bracketDepth++
			case ']':
				bracketDepth--
				if bracketDepth < 0 {
					return "", false
				}
			}
		}
		if inQuote != 0 || parenDepth != 0 || bracketDepth != 0 {
			return "", false
		}
		if !wrapsAll {
			return s, true
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return "", false
		}
		s = inner
	}
}

func parseCondExpr(path, s string, recordVar func(string)) (*condExpr, *Finding) {
	unwrapped, ok := stripOuterParens(s)
	if !ok {
		f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed condition %q", s))
		return nil, &f
	}
	s = unwrapped

	orParts, ok := splitWordOperator(s, "or")
	if !ok {
		f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed condition %q", s))
		return nil, &f
	}
	if len(orParts) > 1 {
		left, fErr := parseCondExpr(path, orParts[0], recordVar)
		if fErr != nil {
			return nil, fErr
		}
		for _, p := range orParts[1:] {
			right, fErr := parseCondExpr(path, p, recordVar)
			if fErr != nil {
				return nil, fErr
			}
			left = &condExpr{kind: condOr, left: left, right: right}
		}
		return left, nil
	}

	andParts, ok := splitWordOperator(s, "and")
	if !ok {
		f := newFinding(CodeTemplateSyntax, path, fmt.Sprintf("malformed condition %q", s))
		return nil, &f
	}
	if len(andParts) > 1 {
		left, fErr := parseCondExpr(path, andParts[0], recordVar)
		if fErr != nil {
			return nil, fErr
		}
		for _, p := range andParts[1:] {
			right, fErr := parseCondExpr(path, p, recordVar)
			if fErr != nil {
				return nil, fErr
			}
			left = &condExpr{kind: condAnd, left: left, right: right}
		}
		return left, nil
	}

	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "not ") || strings.HasPrefix(trimmed, "not\t") || strings.HasPrefix(trimmed, "not(") {
		sub, fErr := parseCondExpr(path, strings.TrimSpace(trimmed[3:]), recordVar)
		if fErr != nil {
			return nil, fErr
		}
		return &condExpr{kind: condNot, left: sub}, nil
	}

	if lhs, rhs, op, found := splitComparison(trimmed); found {
		lExpr, fErr := parsePipelineExpr(path, lhs, recordVar)
		if fErr != nil {
			return nil, fErr
		}
		rExpr, fErr := parsePipelineExpr(path, rhs, recordVar)
		if fErr != nil {
			return nil, fErr
		}
		return &condExpr{
			kind:  condCmp,
			op:    op,
			left:  &condExpr{kind: condVal, val: lExpr},
			right: &condExpr{kind: condVal, val: rExpr},
		}, nil
	}

	pe, fErr := parsePipelineExpr(path, trimmed, recordVar)
	if fErr != nil {
		return nil, fErr
	}
	return &condExpr{kind: condVal, val: pe}, nil
}

func evalNodes(path string, nodes []astNode, env map[string]tmplVal, out *strings.Builder) *Finding {
	for _, n := range nodes {
		switch node := n.(type) {
		case textNode:
			out.WriteString(node.text)
		case exprNode:
			v, fErr := evalPipeline(path, node.expr, env)
			if fErr != nil {
				return fErr
			}
			out.WriteString(v.String())
		case ifNode:
			for _, br := range node.branches {
				if br.cond == nil {
					if fErr := evalNodes(path, br.body, env, out); fErr != nil {
						return fErr
					}
					break
				}
				matched, fErr := evalCond(path, br.cond, env)
				if fErr != nil {
					return fErr
				}
				if matched {
					if fErr := evalNodes(path, br.body, env, out); fErr != nil {
						return fErr
					}
					break
				}
			}
		case forNode:
			iterVal, fErr := evalPipeline(path, node.iterable, env)
			if fErr != nil {
				return fErr
			}
			items := iterValToList(iterVal)
			prev, hadPrev := env[node.loopVar]
			loopKeys := []string{"loop.index", "loop.index0", "loop.revindex", "loop.revindex0", "loop.first", "loop.last", "loop.length"}
			savedLoop := make(map[string]tmplVal, len(loopKeys))
			hadLoopKey := make(map[string]bool, len(loopKeys))
			for _, k := range loopKeys {
				if v, ok := env[k]; ok {
					savedLoop[k] = v
					hadLoopKey[k] = true
				}
			}
			total := len(items)
			for idx, item := range items {
				env[node.loopVar] = item
				env["loop.index"] = tmplVal{defined: true, kind: valNumber, num: float64(idx + 1)}
				env["loop.index0"] = tmplVal{defined: true, kind: valNumber, num: float64(idx)}
				env["loop.revindex"] = tmplVal{defined: true, kind: valNumber, num: float64(total - idx)}
				env["loop.revindex0"] = tmplVal{defined: true, kind: valNumber, num: float64(total - 1 - idx)}
				env["loop.first"] = tmplVal{defined: true, kind: valBool, b: idx == 0}
				env["loop.last"] = tmplVal{defined: true, kind: valBool, b: idx == total-1}
				env["loop.length"] = tmplVal{defined: true, kind: valNumber, num: float64(total)}
				if fErr := evalNodes(path, node.body, env, out); fErr != nil {
					return fErr
				}
			}
			if hadPrev {
				env[node.loopVar] = prev
			} else {
				delete(env, node.loopVar)
			}
			for _, k := range loopKeys {
				if hadLoopKey[k] {
					env[k] = savedLoop[k]
				} else {
					delete(env, k)
				}
			}
		}
	}
	return nil
}

func iterValToList(v tmplVal) []tmplVal {
	if !v.defined {
		return nil
	}
	if v.kind == valList {
		return v.list
	}
	if v.kind == valString {
		s := strings.TrimSpace(v.str)
		if s == "" {
			return nil
		}
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			var arr []string
			if err := json.Unmarshal([]byte(s), &arr); err == nil {
				out := make([]tmplVal, len(arr))
				for i, a := range arr {
					out[i] = tmplVal{defined: true, kind: valString, str: a}
				}
				return out
			}
		}
		parts := strings.Split(v.str, ",")
		out := make([]tmplVal, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, tmplVal{defined: true, kind: valString, str: p})
			}
		}
		return out
	}
	return nil
}

func evalCond(path string, c *condExpr, env map[string]tmplVal) (bool, *Finding) {
	switch c.kind {
	case condOr:
		l, fErr := evalCond(path, c.left, env)
		if fErr != nil || l {
			return l, fErr
		}
		return evalCond(path, c.right, env)
	case condAnd:
		l, fErr := evalCond(path, c.left, env)
		if fErr != nil || !l {
			return false, fErr
		}
		return evalCond(path, c.right, env)
	case condNot:
		v, fErr := evalCond(path, c.left, env)
		return !v, fErr
	case condCmp:
		lv, fErr := evalPipeline(path, c.left.val, env)
		if fErr != nil {
			return false, fErr
		}
		rv, fErr := evalPipeline(path, c.right.val, env)
		if fErr != nil {
			return false, fErr
		}
		switch c.op {
		case "==":
			return valuesEqual(lv, rv), nil
		case "!=":
			return !valuesEqual(lv, rv), nil
		case "in":
			return valueIn(lv, rv), nil
		case "not in":
			return !valueIn(lv, rv), nil
		}
		return false, nil
	case condVal:
		v, fErr := evalPipeline(path, c.val, env)
		if fErr != nil {
			return false, fErr
		}
		return v.Truthy(), nil
	default:
		return false, nil
	}
}

func valuesEqual(a, b tmplVal) bool {
	if !a.defined && !b.defined {
		return true
	}
	if a.kind == valBool && b.kind == valBool {
		return a.b == b.b
	}
	if a.kind == valNumber && b.kind == valNumber {
		return a.num == b.num
	}
	return a.String() == b.String()
}

func valueIn(needle, haystack tmplVal) bool {
	if !haystack.defined {
		return false
	}
	if haystack.kind == valList {
		for _, item := range haystack.list {
			if valuesEqual(needle, item) {
				return true
			}
		}
		return false
	}
	return strings.Contains(haystack.String(), needle.String())
}

func evalPipeline(path string, pe *pipelineExpr, env map[string]tmplVal) (tmplVal, *Finding) {
	v, fErr := evalAtom(path, pe.base, env)
	if fErr != nil {
		return tmplVal{}, fErr
	}
	for _, fc := range pe.filters {
		var argVals []tmplVal
		for _, a := range fc.args {
			av, fErr := evalPipeline(path, a, env)
			if fErr != nil {
				return tmplVal{}, fErr
			}
			argVals = append(argVals, av)
		}
		switch fc.name {
		case "default", "d":
			checkTruthy := len(argVals) >= 2 && argVals[1].Truthy()
			if !v.defined || (checkTruthy && !v.Truthy()) {
				v = argVals[0]
			}
		case "lower":
			v = tmplVal{defined: true, kind: valString, str: strings.ToLower(v.String())}
		case "upper":
			v = tmplVal{defined: true, kind: valString, str: strings.ToUpper(v.String())}
		case "trim":
			v = tmplVal{defined: true, kind: valString, str: strings.TrimSpace(v.String())}
		case "replace":
			v = tmplVal{defined: true, kind: valString, str: strings.ReplaceAll(v.String(), argVals[0].String(), argVals[1].String())}
		}
	}
	return v, nil
}

func evalAtom(path string, a *atomExpr, env map[string]tmplVal) (tmplVal, *Finding) {
	if a.isVar {
		if v, ok := env[a.varName]; ok {
			return v, nil
		}
		return tmplVal{defined: false, kind: valUndefined}, nil
	}
	if len(a.list) > 0 {
		items := make([]tmplVal, len(a.list))
		for i, pe := range a.list {
			iv, fErr := evalPipeline(path, pe, env)
			if fErr != nil {
				return tmplVal{}, fErr
			}
			items[i] = iv
		}
		return tmplVal{defined: true, kind: valList, list: items}, nil
	}
	return a.literal, nil
}

func splitTopLevel(s string, sep byte) ([]string, bool) {
	var parts []string
	inQuote := byte(0)
	escaped := false
	parenDepth := 0
	bracketDepth := 0
	start := 0

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inQuote = ch
		case '(':
			parenDepth++
		case ')':
			parenDepth--
			if parenDepth < 0 {
				return nil, false
			}
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
			if bracketDepth < 0 {
				return nil, false
			}
		case sep:
			if parenDepth == 0 && bracketDepth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if inQuote != 0 || parenDepth != 0 || bracketDepth != 0 {
		return nil, false
	}
	parts = append(parts, s[start:])
	return parts, true
}

func splitWordOperator(s, word string) ([]string, bool) {
	var parts []string
	inQuote := byte(0)
	escaped := false
	parenDepth := 0
	bracketDepth := 0
	start := 0
	wlen := len(word)

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inQuote = ch
		case '(':
			parenDepth++
		case ')':
			parenDepth--
			if parenDepth < 0 {
				return nil, false
			}
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
			if bracketDepth < 0 {
				return nil, false
			}
		default:
			if parenDepth == 0 && bracketDepth == 0 && i+wlen <= len(s) && s[i:i+wlen] == word {
				prevSpace := i > start && unicode.IsSpace(rune(s[i-1]))
				nextSpace := i+wlen < len(s) && unicode.IsSpace(rune(s[i+wlen]))
				if prevSpace && nextSpace {
					parts = append(parts, strings.TrimSpace(s[start:i]))
					start = i + wlen
					i += wlen - 1
				}
			}
		}
	}
	if inQuote != 0 || parenDepth != 0 || bracketDepth != 0 {
		return nil, false
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts, true
}

func splitComparison(s string) (lhs, rhs, op string, found bool) {
	inQuote := byte(0)
	escaped := false
	parenDepth := 0
	bracketDepth := 0

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inQuote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inQuote = ch
		case '(':
			parenDepth++
		case ')':
			parenDepth--
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
		default:
			if parenDepth == 0 && bracketDepth == 0 {
				if i+2 <= len(s) && (s[i:i+2] == "==" || s[i:i+2] == "!=") {
					return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:]), s[i : i+2], true
				}
				if i+6 <= len(s) && s[i:i+6] == "not in" &&
					i > 0 && unicode.IsSpace(rune(s[i-1])) &&
					i+6 < len(s) && unicode.IsSpace(rune(s[i+6])) {
					return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+6:]), "not in", true
				}
				if i+2 <= len(s) && s[i:i+2] == "in" &&
					i > 0 && unicode.IsSpace(rune(s[i-1])) &&
					i+2 < len(s) && unicode.IsSpace(rune(s[i+2])) {
					return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:]), "in", true
				}
			}
		}
	}
	return "", "", "", false
}

func unquoteJinjaString(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	quote := s[0]
	if (quote != '\'' && quote != '"') || s[len(s)-1] != quote {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' {
			i++
			if i >= len(s)-1 {
				return "", false
			}
			switch s[i] {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case '\\', '\'', '"':
				out.WriteByte(s[i])
			default:
				out.WriteByte(s[i])
			}
		} else if s[i] == quote {
			return "", false
		} else {
			out.WriteByte(s[i])
		}
	}
	return out.String(), true
}
