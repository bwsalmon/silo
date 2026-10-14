package silofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const examples = "../docs/examples"

// TestExamples validates every example silo folder in docs/examples.
//
// Two are not ordinary user silos, and are checked as what they are:
// create/ is the built-in Silofile silo create runs, a template with
// {{cli}}, {{mode}} and {{target}} substituted before parsing and allowed
// builtin_tools and the folder sandbox; grain-run/ shows a grain run with
// the sandbox it will have once stage 8 and grain.md §2.3/§2.5 land, so
// it decodes in full and the only errors are later values, each naming
// what adds it.
func TestExamples(t *testing.T) {
	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n++
		dir := filepath.Join(examples, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			switch e.Name() {
			case "create":
				data, err := os.ReadFile(filepath.Join(dir, FileName))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.NewReplacer("{{cli}}", "claude", "{{mode}}", "create", "{{target}}", t.TempDir()).Replace(string(data)))
				f, err := Parse(data, filepath.Join(dir, FileName))
				if err != nil {
					t.Fatal(err)
				}
				abs, _ := filepath.Abs(dir)
				f.Folder, f.File = abs, filepath.Join(abs, FileName)
				if err := f.Validate(Options{Builtin: true}).Err(); err != nil {
					t.Fatal(err)
				}
				if err := f.Validate(Options{}).Err(); err == nil || !strings.Contains(err.Error(), "built-in silos only") {
					t.Errorf("as a user silo: %v, want the built-in-only refusals", err)
				}
			case "grain-run":
				f, err := Load(dir)
				if err != nil {
					t.Fatal(err)
				}
				res := f.Validate(Options{})
				if len(res.Errors) == 0 {
					t.Fatal("grain-run validated; if every value it uses has shipped, move it to the ordinary case")
				}
				for _, e := range res.Errors {
					if e.Later == "" {
						t.Errorf("not a later value: %v", e)
					}
				}
			default:
				f, err := Load(dir)
				if err != nil {
					t.Fatal(err)
				}
				res := f.Validate(Options{})
				if err := res.Err(); err != nil {
					t.Fatal(err)
				}
				if len(res.Warnings) > 0 {
					t.Errorf("warnings: %v", res.Warnings)
				}
			}
		})
	}
	if n < 5 {
		t.Errorf("found %d example folders, want at least 5", n)
	}
}
