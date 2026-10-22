// Package board is the Kanban board (P4) and the task detail page (P5) of `slai serve`.
//
// The board never has more power than the TUI and CLI: the only moves it performs are the
// ones the spec allows (reorder within a column through the priority function, cancel,
// unblock), and they go through the same store/service paths. Everything else is refused
// with the reason, naming who may do it.
package board

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// Column is one board column.
type Column struct {
	Key   string
	Label string
	Hint  string // one line under the title: what being here means
}

// Columns are the board's columns, in order.
var Columns = []Column{
	{"assigned", "Assigned", "handed to an engineer, not started"},
	{"in_progress", "In progress", "being worked on (or being reworked after a review)"},
	{"blocked", "Blocked", "waiting on other tasks, or stuck on a problem"},
	{"in_review", "In review", "the principal is reviewing it"},
	{"merged", "Merged", "on the integration branch (or finished non-code work)"},
	{"cancelled", "Cancelled", "dropped"},
	{"reverted", "Reverted", "merged, then taken back out with slai revert"},
}

var columnLabel = func() map[string]string {
	m := map[string]string{}
	for _, c := range Columns {
		m[c.Key] = c.Label
	}
	return m
}()

// ColumnOf is the column a task status is shown in. changes_requested is work going back to
// the engineer, and done is finished non-code work (a spec): they share a column with their
// nearest neighbour and carry a chip of their own.
func ColumnOf(status string) string {
	switch status {
	case store.StatusChangesRequested:
		return "in_progress"
	case store.StatusDone:
		return "merged"
	}
	return status
}

func isFinal(col string) bool { return col == "merged" || col == "cancelled" || col == "reverted" }

// Move kinds a verdict can allow.
const (
	MoveReorder = "reorder"
	MoveCancel  = "cancel"
	MoveUnblock = "unblock"
)

// Verdict is the answer to "may this card go from this column to that one".
type Verdict struct {
	OK     bool
	Kind   string // MoveReorder, MoveCancel or MoveUnblock when OK
	Reason string // why not, naming who may do what; empty when OK
}

const youCan = " You can: cancel, unblock, reorder."

// MoveVerdict decides a drag. from is the card's current column, to the column it was dropped
// on; waiting lists the dependencies a blocked task is still waiting for (an unblock does
// not apply to those: it unblocks itself when they merge).
func MoveVerdict(id, from, to string, waiting []string) Verdict {
	if from == to {
		return Verdict{OK: true, Kind: MoveReorder}
	}
	if _, ok := columnLabel[to]; !ok {
		return Verdict{Reason: fmt.Sprintf("there is no %q column.%s", to, youCan)}
	}
	if isFinal(from) {
		extra := " Create a new task instead."
		if from == "merged" {
			extra = fmt.Sprintf(" Only `slai revert %s` (command line) takes a merged task back out.", id)
		}
		return Verdict{Reason: fmt.Sprintf("%s is %s, which is final.%s%s", id, columnLabel[from], extra, youCan)}
	}
	switch to {
	case "cancelled":
		return Verdict{OK: true, Kind: MoveCancel}
	case "assigned", "in_progress":
		if from == "blocked" {
			if len(waiting) > 0 {
				return Verdict{Reason: fmt.Sprintf("%s is waiting on %s: it unblocks itself when they merge, nobody needs to act. Unblock is for a task stuck on a problem.%s", id, strings.Join(waiting, ", "), youCan)}
			}
			return Verdict{OK: true, Kind: MoveUnblock}
		}
		if from == "in_review" {
			return Verdict{Reason: fmt.Sprintf("illegal move in review → %s: only the principal sends a review back (request_changes).%s", strings.ToLower(columnLabel[to]), youCan)}
		}
		return Verdict{Reason: fmt.Sprintf("illegal move %s → %s: engineers move their own work forward with report and request_review.%s", strings.ToLower(columnLabel[from]), strings.ToLower(columnLabel[to]), youCan)}
	case "blocked":
		return Verdict{Reason: fmt.Sprintf("%s blocks itself: its engineer calls report(blocked) with what it needs, and you answer in the Inbox.%s", id, youCan)}
	case "in_review":
		return Verdict{Reason: fmt.Sprintf("%s enters review when its engineer calls request_review.%s", id, youCan)}
	case "merged":
		return Verdict{Reason: fmt.Sprintf("only the principal's approve moves a task to merged (the web never approves or merges).%s", youCan)}
	case "reverted":
		return Verdict{Reason: fmt.Sprintf("reverting is `slai revert %s` on the command line; the Commit preview page shows the plan.%s", id, youCan)}
	}
	return Verdict{Reason: "that move is not allowed." + youCan}
}

// PriorityValue turns what the card menu sends (p0..p3, a bare number, up, down, top) into
// the spec the priority function takes (a number, up, down or top).
func PriorityValue(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "up", "down", "top":
		return v, nil
	}
	n := strings.TrimPrefix(v, "p")
	if n == "" {
		return "", fmt.Errorf("priority must be p0..p3, up, down or top, got %q", v)
	}
	for i, r := range n {
		if !(r >= '0' && r <= '9') && !(i == 0 && r == '-') {
			return "", fmt.Errorf("priority must be p0..p3, up, down or top, got %q", v)
		}
	}
	return n, nil
}

// PrioLabel is the badge text of a priority: p0, p2, p-1.
func PrioLabel(p int) string { return fmt.Sprintf("p%d", p) }
