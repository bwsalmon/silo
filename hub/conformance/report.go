package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// WriteText writes the report for a human: a line per scenario.
func (r *Report) WriteText(w io.Writer) error {
	if r.Node != nil {
		fmt.Fprintf(w, "Grain %s (version %q): %d slots, repos %v, features %v\n\n", r.Node.ID, r.Node.Version, r.Node.Slots, r.Node.Repos, r.Node.Features)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	counts := map[Status]int{}
	for _, v := range r.Scenarios {
		counts[v.Status]++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.Status, v.Name, v.Duration, v.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	verdict := "CONFORMS"
	if !r.Passed {
		verdict = "DOES NOT CONFORM"
	}
	_, err := fmt.Fprintf(w, "\n%s: %d passed, %d failed, %d skipped\n", verdict, counts[Pass], counts[Fail], counts[Skip])
	return err
}

// WriteJSON writes the report's machine-readable verdict.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
