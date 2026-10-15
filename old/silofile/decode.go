package silofile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the one schema version this package reads.
const Version = "v2"

// Pos is a position in a Silofile. Line and Column are 1-based; zero
// means unknown.
type Pos struct {
	Line, Column int
}

// Error is one problem with a Silofile: the file, the YAML path of the
// value at fault (such as sandboxes[0].kind) and where it was written.
type Error struct {
	File string
	Path string
	Pos
	Msg string
	// Later is set when the value is one v2.0 reserves but does not ship:
	// it names the stage (or the design section) that adds it.
	Later string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d:%d", e.Line, e.Column)
		}
		b.WriteString(": ")
	} else if e.Line > 0 {
		fmt.Fprintf(&b, "line %d:%d: ", e.Line, e.Column)
	}
	if e.Path != "" {
		b.WriteString(e.Path)
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors is a list of problems, in file order. As an error it prints one
// per line.
type Errors []*Error

func (l Errors) Error() string {
	s := make([]string, len(l))
	for i, e := range l {
		s[i] = e.Error()
	}
	return strings.Join(s, "\n")
}

func (l Errors) sort() {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].Line != l[j].Line {
			return l[i].Line < l[j].Line
		}
		return l[i].Column < l[j].Column
	})
}

// Parse strictly decodes a Silofile (design §2.1). file names it in
// errors. The error, if any, is Errors.
//
// Strict means: an unknown or repeated key is an error; every value silo
// reads as a string stays the string it was written as (no, on and 0700
// are not reinterpreted); booleans are true or false; durations and
// sizes have their own syntax; anchors and aliases are followed, but
// merge keys (<<) and tags are refused; there is one YAML document, and
// its first key is silo: v2.
func Parse(data []byte, file string) (*Silofile, error) {
	d := &decoder{file: file, pos: map[string]Pos{}}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, Errors{{File: file, Msg: "empty file: a Silofile starts with silo: " + Version}}
		}
		return nil, Errors{yamlError(file, err)}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, Errors{{File: file, Pos: Pos{extra.Line, extra.Column}, Msg: "a Silofile is one YAML document; found a second"}}
	} else if !errors.Is(err, io.EOF) {
		return nil, Errors{yamlError(file, err)}
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode || len(root.Content) == 0 {
		return nil, Errors{{File: file, Pos: Pos{root.Line, root.Column}, Msg: "a Silofile is a mapping whose first key is silo: " + Version}}
	}
	first, val := root.Content[0], root.Content[1]
	if first.Value != "silo" {
		msg := "the first key must be silo: " + Version
		for i := 2; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "silo" {
				msg = "silo: " + Version + " must be the first key, not " + strconv.Quote(first.Value)
			}
		}
		return nil, Errors{{File: file, Pos: Pos{first.Line, first.Column}, Msg: msg}}
	}
	if val.Kind != yaml.ScalarNode || val.Value != Version {
		return nil, Errors{{File: file, Path: "silo", Pos: Pos{val.Line, val.Column},
			Msg: fmt.Sprintf("schema version %q is not one this silo reads (want %s)", val.Value, Version)}}
	}
	f := &Silofile{}
	d.value(root, f, "")
	if len(d.errs) > 0 {
		d.errs.sort()
		return nil, d.errs
	}
	f.pos = d.pos
	return f, nil
}

// yamlError turns a yaml.v3 syntax error ("yaml: line 3: ...") into an
// Error with its line.
func yamlError(file string, err error) *Error {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	e := &Error{File: file, Msg: msg}
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if num, after, ok := strings.Cut(rest, ": "); ok {
			if n, err := strconv.Atoi(num); err == nil {
				e.Line, e.Msg = n, after
			}
		}
	}
	return e
}

// Pos returns where path, or the nearest enclosing value that was
// written, appears in the file.
func (f *Silofile) Pos(path string) Pos {
	for {
		if p, ok := f.pos[path]; ok {
			return p
		}
		i := strings.LastIndexAny(path, ".[")
		if i < 0 {
			return f.pos[""]
		}
		path = path[:i]
	}
}

// nodeDecoder is a type that decodes itself from a YAML node: a field
// with more than one shape, or a value with its own syntax.
type nodeDecoder interface {
	decodeYAML(d *decoder, n *yaml.Node, path string)
}

type decoder struct {
	file string
	errs Errors
	pos  map[string]Pos
}

func (d *decoder) errorf(n *yaml.Node, path, format string, args ...any) {
	d.errs = append(d.errs, &Error{File: d.file, Path: path, Pos: Pos{n.Line, n.Column}, Msg: fmt.Sprintf(format, args...)})
}

// coreTags are the tags a plain YAML scalar, mapping or sequence
// resolves to. Writing any tag explicitly is refused.
var coreTags = map[string]bool{"!!str": true, "!!int": true, "!!bool": true, "!!float": true, "!!null": true, "!!map": true, "!!seq": true}

// node follows aliases and refuses explicit tags. It returns nil after
// recording an error.
func (d *decoder) node(n *yaml.Node, path string) *yaml.Node {
	d.pos[path] = Pos{n.Line, n.Column}
	for depth := 0; n.Kind == yaml.AliasNode; depth++ {
		if n.Alias == nil || depth > 100 {
			d.errorf(n, path, "alias does not resolve")
			return nil
		}
		n = n.Alias
	}
	if n.Style&yaml.TaggedStyle != 0 {
		if coreTags[n.Tag] {
			d.errorf(n, path, "tags are refused (%s): silo reads every value by its key, so write the value plainly", n.Tag)
		} else {
			d.errorf(n, path, "tags are refused (%s)", n.Tag)
		}
		return nil
	}
	return n
}

// value decodes n into the value v points to.
func (d *decoder) value(n *yaml.Node, v any, path string) {
	d.decode(n, reflect.ValueOf(v).Elem(), path)
}

func (d *decoder) decode(n *yaml.Node, v reflect.Value, path string) {
	if n = d.node(n, path); n == nil {
		return
	}
	if v.Kind() != reflect.Pointer && v.CanAddr() {
		if nd, ok := v.Addr().Interface().(nodeDecoder); ok {
			if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
				d.errorf(n, path, "has no value")
				return
			}
			if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode || n.Kind == yaml.ScalarNode {
				nd.decodeYAML(d, n, path)
			}
			return
		}
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		d.errorf(n, path, "has no value")
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		d.decode(n, p.Elem(), path)
		v.Set(p)
	case reflect.Struct:
		d.structure(n, v, path)
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			d.errorf(n, path, "want a list, found %s", kindName(n))
			return
		}
		s := reflect.MakeSlice(v.Type(), len(n.Content), len(n.Content))
		for i, c := range n.Content {
			d.decode(c, s.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
		v.Set(s)
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			d.errorf(n, path, "want a mapping, found %s", kindName(n))
			return
		}
		m := reflect.MakeMapWithSize(v.Type(), len(n.Content)/2)
		d.pairs(n, path, func(key string, kn, vn *yaml.Node) {
			e := reflect.New(v.Type().Elem()).Elem()
			d.decode(vn, e, join(path, key))
			m.SetMapIndex(reflect.ValueOf(key), e)
		})
		v.Set(m)
	case reflect.String:
		if s, ok := d.scalar(n, path); ok {
			v.SetString(s)
		}
	case reflect.Bool:
		s, ok := d.scalar(n, path)
		if !ok {
			return
		}
		switch s {
		case "true":
			v.SetBool(true)
		case "false":
		default:
			d.errorf(n, path, "want true or false, found %q", s)
		}
	case reflect.Int:
		s, ok := d.scalar(n, path)
		if !ok {
			return
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil || i < 0 || (len(s) > 1 && s[0] == '0') {
			d.errorf(n, path, "want a whole number, found %q", s)
			return
		}
		v.SetInt(i)
	case reflect.Float64:
		s, ok := d.scalar(n, path)
		if !ok {
			return
		}
		x, err := strconv.ParseFloat(s, 64)
		if err != nil || x < 0 || strings.ContainsAny(s, "xXeE_") {
			d.errorf(n, path, "want a number, found %q", s)
			return
		}
		v.SetFloat(x)
	default:
		panic(fmt.Sprintf("silofile: no decoder for %s", v.Type()))
	}
}

// scalar returns a scalar's text exactly as written.
func (d *decoder) scalar(n *yaml.Node, path string) (string, bool) {
	if n.Kind != yaml.ScalarNode {
		d.errorf(n, path, "want a single value, found %s", kindName(n))
		return "", false
	}
	return n.Value, true
}

// word decodes a scalar that must be one of words. ok reports whether it
// was.
func (d *decoder) word(n *yaml.Node, path string, words ...string) (string, bool) {
	s, ok := d.scalar(n, path)
	if !ok {
		return "", false
	}
	for _, w := range words {
		if s == w {
			return s, true
		}
	}
	d.errorf(n, path, "%q is not one of %s", s, strings.Join(words, ", "))
	return "", false
}

// pairs calls fn for each key of a mapping, refusing merge keys,
// non-scalar keys and repeated keys.
func (d *decoder) pairs(n *yaml.Node, path string, fn func(key string, kn, vn *yaml.Node)) {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		if kn.Kind == yaml.AliasNode {
			d.errorf(kn, path, "an alias cannot be a key")
			continue
		}
		if kn.Tag == "!!merge" {
			d.errorf(kn, path, "merge keys (<<) are refused: write the keys out")
			continue
		}
		if kn.Style&yaml.TaggedStyle != 0 {
			d.errorf(kn, path, "tags are refused (%s)", kn.Tag)
			continue
		}
		if kn.Kind != yaml.ScalarNode {
			d.errorf(kn, path, "a key must be a single value")
			continue
		}
		if seen[kn.Value] {
			d.errorf(kn, join(path, kn.Value), "key %q is repeated", kn.Value)
			continue
		}
		seen[kn.Value] = true
		fn(kn.Value, kn, vn)
	}
}

func (d *decoder) structure(n *yaml.Node, v reflect.Value, path string) {
	if n.Kind != yaml.MappingNode {
		d.errorf(n, path, "want a mapping, found %s", kindName(n))
		return
	}
	fields := fieldsOf(v.Type())
	seen := map[string]bool{}
	d.pairs(n, path, func(key string, kn, vn *yaml.Node) {
		f, ok := fields.byKey[key]
		if !ok {
			d.errorf(kn, join(path, key), "unknown key %q (known here: %s)", key, strings.Join(fields.keys, ", "))
			return
		}
		seen[key] = true
		d.decode(vn, v.Field(f.index), join(path, key))
	})
	for _, f := range fields.list {
		if f.required && !seen[f.key] {
			d.errorf(n, path, "missing required key %q", f.key)
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return strconv.Quote(n.Value)
	}
	return "something else"
}

type field struct {
	key      string
	index    int
	required bool
	doc      string
}

type fieldSet struct {
	list  []field
	byKey map[string]field
	keys  []string
}

// fieldsOf lists a struct's keys from its yaml tags. Every exported
// field must have one; "-" skips it.
func fieldsOf(t reflect.Type) fieldSet {
	fs := fieldSet{byKey: map[string]field{}}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		key := sf.Tag.Get("yaml")
		if key == "" {
			panic(fmt.Sprintf("silofile: %s.%s has no yaml tag", t.Name(), sf.Name))
		}
		if key == "-" {
			continue
		}
		f := field{key: key, index: i, required: sf.Tag.Get("schema") == "required", doc: sf.Tag.Get("doc")}
		fs.list = append(fs.list, f)
		fs.byKey[key] = f
		fs.keys = append(fs.keys, key)
	}
	return fs
}
