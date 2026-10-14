package silofile

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestSchemaIsGenerated holds silofile.schema.json, the schema embedded
// in the binary, to what GenerateSchema makes of the types today. If it
// fails, run go generate ./silofile.
func TestSchemaIsGenerated(t *testing.T) {
	got, err := GenerateSchema()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, Schema()) {
		t.Fatal("silofile.schema.json is out of date: run go generate ./silofile")
	}
}

func TestSchemaCoversEveryKey(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(Schema(), &s); err != nil {
		t.Fatal(err)
	}
	if s["additionalProperties"] != false {
		t.Error("top level allows unknown keys")
	}
	props := s["properties"].(map[string]any)
	for _, k := range Sections() {
		if _, ok := props[k]; !ok {
			t.Errorf("no schema for section %q", k)
		}
	}
	// Spot-check the shapes the schema has to get right.
	agent := props["agent"].(map[string]any)
	if !reflect.DeepEqual(agent["required"], []any{"cli"}) {
		t.Errorf("agent.required = %v", agent["required"])
	}
	if _, ok := props["remote_permissions"].(map[string]any)["oneOf"]; !ok {
		t.Error("remote_permissions is not oneOf a word or a list")
	}
	sandbox := props["sandboxes"].(map[string]any)["items"].(map[string]any)
	memory := sandbox["properties"].(map[string]any)["memory"].(map[string]any)
	if memory["pattern"] == nil {
		t.Errorf("sandboxes[].memory = %v, want a size pattern", memory)
	}
	if props["silo"].(map[string]any)["const"] != Version {
		t.Errorf("silo = %v", props["silo"])
	}
}

func TestSectionSchema(t *testing.T) {
	b, err := SectionSchema("location")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s["properties"].(map[string]any)["ssh"]; !ok {
		t.Errorf("location schema = %s", b)
	}
	if _, err := SectionSchema("nope"); err == nil {
		t.Error("SectionSchema(nope) succeeded")
	}
}
