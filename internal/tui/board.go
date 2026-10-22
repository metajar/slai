package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/store"
)

var boardOrder = map[string]int{
	store.StatusInReview: 0, store.StatusInProgress: 1, store.StatusChangesRequested: 2,
	store.StatusAssigned: 3, store.StatusBlocked: 4, store.StatusMerged: 5, store.StatusCancelled: 6, store.StatusDone: 5, store.StatusReverted: 6,
}

// boardRows groups tasks by status (review first, terminal last); within a group the
// highest priority comes first, then ID order (so equal priorities keep the old order).
func boardRows(tasks []store.Task) []store.Task {
	out := append([]store.Task(nil), tasks...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := boardOrder[out[i].Status], boardOrder[out[j].Status]
		if oi != oj {
			return oi < oj
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// boardLine formats one task row: "T-0003 in_review  eng-2  Invoice list UI  r1".
func boardLine(t store.Task) string { return boardLineWaits(t, nil) }

// boardLineWaits is boardLine plus "waits on T-x" for dependencies that are not
// yet merged or cancelled; status maps task id -> status (nil = all waiting).
func boardLineWaits(t store.Task, status map[string]string) string {
	asg := t.Assignee
	if asg == "" {
		asg = "-"
	}
	s := fmt.Sprintf("%-6s %-17s %-4s %-10s %s", t.ID, t.Status, fmt.Sprintf("p%d", t.Priority), asg, t.Title)
	if t.ReviewRounds > 0 {
		s += fmt.Sprintf("  r%d", t.ReviewRounds)
	}
	var waits []string
	for _, d := range t.DependsOn {
		if st, ok := status[d]; !ok || !store.DepSatisfied(st) {
			waits = append(waits, d)
		}
	}
	if len(waits) > 0 && !store.DepSatisfied(t.Status) && t.Status != store.StatusDone {
		s += "  waits on " + strings.Join(waits, ", ")
	}
	return s
}

// renderTasks renders tasks grouped by status under bold headers, scrolled so
// the cursor row (index into boardRows) stays visible. h is the visible height.
func renderTasks(tasks []store.Task, cursor, w, h int) []string {
	rows := boardRows(tasks)
	if len(rows) == 0 {
		return []string{styleDim.Render("no tasks yet — ask the orchestrator for something (esc, then enter on orch)")}
	}
	counts := map[string]int{}
	for _, t := range rows {
		counts[t.Status]++
	}
	status := map[string]string{}
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	var lines []string
	curLine := 0
	prev := ""
	for i, t := range rows {
		if t.Status != prev {
			if prev != "" {
				lines = append(lines, "")
			}
			lines = append(lines, styleBold.Render(fmt.Sprintf("%s (%d)", strings.ToUpper(strings.ReplaceAll(t.Status, "_", " ")), counts[t.Status])))
			prev = t.Status
		}
		l := ansi.Truncate("  "+boardLineWaits(t, status), w, "…")
		if i == cursor {
			curLine = len(lines)
			l = selRow(ansi.Strip(l), w, true)
		}
		lines = append(lines, l)
	}
	start := 0
	if curLine >= h {
		start = curLine - h + 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	return lines[start:min(start+h, len(lines))]
}
