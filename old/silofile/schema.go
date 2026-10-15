package silofile

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

//go:generate go run ./internal/genschema

// schemaJSON is the JSON Schema of the Silofile, generated from these
// types by GenerateSchema and checked in as silofile.schema.json, so the
// binary carries it (design §2.1). TestSchemaIsGenerated keeps the two
// in step: after changing a type, run go generate ./silofile.
//
//go:embed silofile.schema.json
var schemaJSON []byte

// Schema returns the JSON Schema of the Silofile embedded in the binary.
func Schema() []byte { return schemaJSON }

// SectionSchema returns the schema of one top-level key, such as agent or
// sandboxes, as indented JSON.
func SectionSchema(section string) ([]byte, error) {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return nil, err
	}
	raw, ok := s.Properties[section]
	if !ok {
		return nil, fmt.Errorf("no section %q (sections: %s)", section, joinKeys(s.Properties))
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return marshalSchema(v)
}

// Sections lists the Silofile's top-level keys.
func Sections() []string {
	return fieldsOf(typeOf[Silofile]()).keys
}

func joinKeys(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

// GenerateSchema builds the Silofile's JSON Schema from its Go types.
func GenerateSchema() ([]byte, error) {
	s := schemaOf(typeOf[Silofile]())
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = "https://github.com/bwsalmon/silo/old/silofile.schema.json"
	s["title"] = "Silofile"
	s["description"] = "A Silo v2 Silofile (design §2.3). Generated from the Go types in github.com/bwsalmon/silo/old/silofile; do not edit."
	props := s["properties"].(map[string]any)
	props["silo"] = map[string]any{"const": Version, "description": "Schema version. Must be the first key, and must be v2."}
	return marshalSchema(s)
}

func marshalSchema(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// schemaer is a type that describes its own schema: one of the
// several-shape fields of values.go.
type schemaer interface {
	jsonSchema() map[string]any
}

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

func schemaOf(t reflect.Type) map[string]any {
	if t.Kind() != reflect.Pointer {
		if s, ok := reflect.New(t).Elem().Interface().(schemaer); ok {
			return s.jsonSchema()
		}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaOf(t.Elem())
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for _, f := range fieldsOf(t).list {
			p := schemaOf(t.Field(f.index).Type)
			if f.doc != "" {
				if _, ok := p["description"]; !ok {
					p["description"] = f.doc
				} else {
					p["description"] = f.doc + " " + p["description"].(string)
				}
			}
			props[f.key] = p
			if f.required {
				required = append(required, f.key)
			}
		}
		return object(props, required...)
	case reflect.Slice:
		return array(schemaOf(t.Elem()))
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem())}
	case reflect.String:
		return str()
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Float64:
		return map[string]any{"type": "number", "minimum": 0}
	}
	panic(fmt.Sprintf("silofile: no schema for %s", t))
}

func str() map[string]any { return map[string]any{"type": "string"} }

func array(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func enum(words ...string) map[string]any {
	return map[string]any{"type": "string", "enum": words}
}

func oneOf(alts ...map[string]any) map[string]any {
	return map[string]any{"oneOf": alts}
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}
