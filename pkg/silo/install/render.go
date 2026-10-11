package install

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// The plan table's columns (install.md "Plan output").
const (
	nameWidth   = 18
	actionWidth = 8
	lineWidth   = 100
)

var indent = strings.Repeat(" ", 2+nameWidth+actionWidth)

// RenderTable writes plans as install.md's plan table: the header, a
// row per step, the summary line and the time estimate.
func RenderTable(w io.Writer, header string, plans []StepPlan) error {
	var b strings.Builder
	b.WriteString(header + "\n\n")
	for _, p := range plans {
		renderStep(&b, p)
	}
	b.WriteString("\n" + Summary(plans) + "\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// renderStep writes one step's rows. Consecutive changes with the same
// action and no detail share a row, comma-separated and wrapped; every
// other change gets a row of its own, with the action shown only where
// it differs from the row above.
func renderStep(b *strings.Builder, p StepPlan) {
	name := p.Step
	prev := Action("")
	row := func(a Action, text string) {
		col := ""
		if a != prev {
			col = string(a)
		}
		prev = a
		line := fmt.Sprintf("  %-*s %-*s %s", nameWidth-1, name, actionWidth-1, col, wrap(text))
		b.WriteString(strings.TrimRight(line, " ") + "\n")
		name = ""
	}
	if p.Err != nil {
		row(ActionFail, "couldn't read the cloud: "+p.Err.Error())
		return
	}
	if len(p.Changes) == 0 {
		row(ActionOK, "")
		return
	}
	for i := 0; i < len(p.Changes); {
		c := p.Changes[i]
		if c.Detail == "" && c.Why == "" && c.Fix == "" {
			j := i
			var objs []string
			for ; j < len(p.Changes); j++ {
				d := p.Changes[j]
				if d.Action != c.Action || d.Detail != "" || d.Why != "" || d.Fix != "" {
					break
				}
				objs = append(objs, d.Object)
			}
			row(c.Action, strings.Join(objs, ", "))
			i = j
			continue
		}
		text := c.Detail
		if c.Object != "" {
			text = strings.TrimSpace(c.Object + " " + c.Detail)
		}
		row(c.Action, text)
		if c.Why != "" {
			fmt.Fprintf(b, "%s%s\n", indent, wrap("why: "+c.Why))
		}
		if c.Fix != "" {
			fmt.Fprintf(b, "%s%s\n", indent, wrap("fix: "+c.Fix))
		}
		if c.Why != "" || c.Fix != "" {
			// The next change's action shows again, to set it apart.
			prev = ""
		}
		i++
	}
}

// wrap breaks text at spaces to fit lineWidth after the table's
// indent, indenting each continuation to the text column.
func wrap(text string) string {
	room := lineWidth - len(indent)
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && len(line)+1+len(word) > room {
			lines = append(lines, line)
			line = word
			continue
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"+indent)
}

// Summary is the plan's last two lines: how many steps do what, and
// how long it will take.
func Summary(plans []StepPlan) string {
	var create, update, del, ok, keep, fail int
	var total time.Duration
	var longest Change
	for _, p := range plans {
		switch a := p.Action(); a {
		case ActionCreate, ActionEnable, ActionApply:
			create++
		case ActionUpdate:
			update++
		case ActionDelete:
			del++
		case ActionKeep:
			keep++
		case ActionFail:
			fail++
		default:
			ok++
		}
		for _, c := range p.Changes {
			if c.Action.Changes() {
				total += c.Estimate
				if c.Estimate > longest.Estimate {
					longest = c
				}
			}
		}
	}
	var parts []string
	if del > 0 || keep > 0 {
		parts = append(parts, fmt.Sprintf("%d to delete", del), fmt.Sprintf("%d to keep", keep))
	} else {
		parts = append(parts, fmt.Sprintf("%d to create or apply", create), fmt.Sprintf("%d to update", update))
	}
	parts = append(parts, fmt.Sprintf("%d ok", ok))
	if fail > 0 {
		parts = append(parts, fmt.Sprintf("%d failing", fail))
	}
	s := fmt.Sprintf("  %d steps: %s\n", len(plans), strings.Join(parts, ", "))
	switch {
	case fail > 0:
		s += "  Nothing can be applied until every failing check passes."
	case create+update+del == 0:
		s += "  Nothing to change."
	default:
		mins := int((total + time.Minute - 1) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		unit := "minutes"
		if mins == 1 {
			unit = "minute"
		}
		s += fmt.Sprintf("  Estimated time: about %d %s", mins, unit)
		if longest.Waiting != "" && longest.Estimate*2 > total {
			s += ", most of it waiting for " + longest.Waiting
		}
		s += "."
	}
	return s
}

// jsonStep is a step in --output json.
type jsonStep struct {
	Step    string   `json:"step"`
	Action  Action   `json:"action"`
	Changes []Change `json:"changes"`
	Error   string   `json:"error,omitempty"`
}

// RenderJSON writes plans as --dry-run --output json does: one JSON
// object per step, one per line.
func RenderJSON(w io.Writer, plans []StepPlan) error {
	enc := json.NewEncoder(w)
	for _, p := range plans {
		s := jsonStep{Step: p.Step, Action: p.Action(), Changes: p.Changes}
		if s.Changes == nil {
			s.Changes = []Change{}
		}
		if p.Err != nil {
			s.Error = p.Err.Error()
		}
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}
