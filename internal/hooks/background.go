package hooks

import (
	"regexp"
	"strconv"
	"strings"
)

// Claude Code keeps working after its turn ends while a background shell or a Monitor
// runs; its footer (the last line of the pane) then counts them, e.g. (Claude Code
// 2.1.306, fixtures in testdata/panes/claude):
//
//	⏸ manual mode on · 1 shell, 1 monitor · ← 11 agents · ↓ to manage
//	⏵⏵ auto mode on (shift+tab to cycle) · 1 shell · ← 11 agents
//
// The Stop hook has fired by then, so without the footer an agent that is waiting on
// its e2e run looks idle for as long as it takes.
//
// Only the footer is believed. The turn-summary line above the prompt ("… · 1 shell, 1
// monitor still running") stays on screen after the task has ended, and "← 11 agents"
// is the team's other sessions, not a background task.
var backgroundRe = regexp.MustCompile(`(?i)\b(\d+)\s+(shells?|monitors?|background\s+(?:tasks?|shells?|jobs?|commands?)|bg\s+tasks?)\b`)

// Claude Code 2.1.307 also draws, just above the input box, one line while subagents (the
// Agent tool) run for the session, whether the main turn is mid-work or already over:
//
//	● 2 agents working · /team
//	● 1 agent working · /team
//
// The parent session fires no hooks meanwhile. The " · /team" suffix is what tells it from
// the footer's "← 12 agents" (the team's other sessions) and from prose; it must sit in the
// pane's bottom lines (subagentLines), since agent replies are drawn with the same bullet.
var subagentsRe = regexp.MustCompile(`(?m)^● (\d+) agents? working · /team\b`)

// subagentLines is how many trailing non-empty lines can hold the subagents line: it is
// drawn above the input box (rule, prompt, rule, usage line, footer).
const subagentLines = 8

// SubagentsWith reports how many subagents the pane says are working under the harness's
// rules (0: none, or the harness draws no such line).
func SubagentsWith(r Rules, pane string) int {
	if r.Subagents == nil {
		return 0
	}
	var tail []string
	for _, l := range strings.Split(pane, "\n") {
		if strings.TrimSpace(l) != "" {
			tail = append(tail, l)
		}
	}
	live := strings.Join(tail[max(len(tail)-subagentLines, 0):], "\n")
	m := r.Subagents.FindStringSubmatch(live)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// BackgroundWith reports what the pane says the agent is waiting on under the harness's
// rules: the footer's shells and monitors, e.g. "1 shell, 1 monitor", and the subagents
// line, e.g. "2 subagents"; "" when nothing, only zero counts, or the harness has neither.
func BackgroundWith(r Rules, pane string) string {
	subs := ""
	if n := SubagentsWith(r, pane); n > 0 {
		subs = strconv.Itoa(n) + " subagent"
		if n != 1 {
			subs += "s"
		}
	}
	if r.Background == nil {
		return subs
	}
	footer := ""
	for _, l := range strings.Split(pane, "\n") {
		if strings.TrimSpace(l) != "" {
			footer = l
		}
	}
	var found []string
	for _, m := range r.Background.FindAllStringSubmatch(footer, -1) {
		if len(m) >= 3 && m[1] != "0" {
			found = append(found, m[1]+" "+strings.ToLower(strings.Join(strings.Fields(m[2]), " ")))
		}
	}
	if subs != "" {
		found = append(found, subs)
	}
	return strings.Join(found, ", ")
}
