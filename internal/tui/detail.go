package tui

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// renderDetail builds the task detail overlay text. history is a list of
// pre-formatted event lines; spec is the (possibly empty) spec excerpt.
func renderDetail(t store.Task, spec string, history []string, memory string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", styleTitle.Render(t.ID), styleBold.Render(t.Title))
	fmt.Fprintf(&b, "status: %s   assignee: %s   created by: %s   review rounds: %d\n", t.Status, orDash(t.Assignee), orDash(t.CreatedBy), t.ReviewRounds)
	fmt.Fprintf(&b, "branch: %s\n", orDash(t.Branch))
	fmt.Fprintf(&b, "spec:   %s\n", orDash(t.SpecPath))
	if t.ParentID != "" {
		fmt.Fprintf(&b, "parent: %s\n", t.ParentID)
	}
	b.WriteString("\n" + styleBold.Render("Description") + "\n")
	if strings.TrimSpace(t.Description) == "" {
		b.WriteString(styleDim.Render("(none)") + "\n")
	} else {
		b.WriteString(t.Description + "\n")
	}
	if spec != "" {
		b.WriteString("\n" + styleBold.Render("Spec") + "\n" + spec + "\n")
	}
	b.WriteString(renderACReview(t))
	if memory != "" {
		b.WriteString("\n" + memory)
	}
	b.WriteString("\n" + styleBold.Render("History / messages / review comments") + "\n")
	if len(history) == 0 {
		b.WriteString(styleDim.Render("(no history for this task)") + "\n")
	}
	for _, h := range history {
		b.WriteString(h + "\n")
	}
	b.WriteString("\n" + styleDim.Render("esc/q close · ↑↓ scroll"))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

// renderACReview is the "Acceptance criteria" section of the task detail: what the
// engineer claimed for each criterion of the spec and what the reviewer answered.
// It is empty for a task that never carried claims.
func renderACReview(t store.Task) string {
	if len(t.ACClaims) == 0 && len(t.ACVerdicts) == 0 {
		return ""
	}
	verdict := map[string]store.ACVerdict{}
	for _, v := range t.ACVerdicts {
		verdict[store.ACKey(v.AC)] = v
	}
	var b strings.Builder
	b.WriteString("\n" + styleBold.Render("Acceptance criteria (latest review request)") + "\n")
	b.WriteString(styleDim.Render(fmt.Sprintf("  %-10s %-14s %s", "criterion", "engineer", "reviewer")) + "\n")
	row := func(ac, claim, rev, note string) {
		fmt.Fprintf(&b, "  %-10s %-14s %s\n", ac, claim, rev)
		if note != "" {
			b.WriteString(styleDim.Render("             "+note) + "\n")
		}
	}
	shown := map[string]bool{}
	for _, c := range t.ACClaims {
		claim := edgeGreen.Render("✓ met")
		if !c.Met {
			claim = styleErr.Render("✗ not met")
		}
		rev := styleDim.Render("– no verdict")
		note := c.Note
		if v, ok := verdict[store.ACKey(c.AC)]; ok {
			shown[store.ACKey(c.AC)] = true
			rev = edgeGreen.Render("pass")
			if !v.Pass {
				rev = styleErr.Render("FAIL")
			}
			if v.Note != "" {
				note = strings.TrimSpace(note + "  ⇄ " + v.Note)
			}
		}
		row(c.AC, claim, rev, note)
	}
	for _, v := range t.ACVerdicts { // verdicts on a criterion the engineer never claimed
		if shown[store.ACKey(v.AC)] {
			continue
		}
		rev := edgeGreen.Render("pass")
		if !v.Pass {
			rev = styleErr.Render("FAIL")
		}
		row(v.AC, styleDim.Render("(no claim)"), rev, v.Note)
	}
	return b.String()
}
