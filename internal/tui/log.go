package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// logCtx supplies the context needed to resolve implicit event parties
// (who reviews, which task id a create_task produced, the integration branch).
type logCtx struct {
	team  *config.Team
	tasks []store.Task
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func trunc(s string, n int) string {
	r := []rune(oneLine(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// isNoise reports events hidden from the log by default.
func isNoise(e store.Event) bool {
	return strings.HasPrefix(e.Kind, "hook:") || strings.HasPrefix(e.Kind, "usage:")
}

func (c logCtx) roleAgent(role string) string {
	if c.team == nil {
		return ""
	}
	for _, a := range c.team.Agents {
		if a.Role == role {
			return a.ID
		}
	}
	return ""
}

func (c logCtx) task(id string) (store.Task, bool) {
	for _, t := range c.tasks {
		if t.ID == id {
			return t, true
		}
	}
	return store.Task{}, false
}

func (c logCtx) taskByTitle(title, assignee string) string {
	for i := len(c.tasks) - 1; i >= 0; i-- {
		t := c.tasks[i]
		if t.Title == title && (assignee == "" || t.Assignee == assignee) {
			return t.ID
		}
	}
	return ""
}

func (c logCtx) integration() string {
	if c.team != nil && c.team.Project.IntegrationBranch != "" {
		return c.team.Project.IntegrationBranch
	}
	return config.DefaultIntegrationBranch
}

// formatEvent renders "eng-1 → principal: request_review T-0004" without context.
func formatEvent(e store.Event) string { return logCtx{}.format(e) }

func (c logCtx) format(e store.Event) string {
	d := parseData(e.Data)
	agent := e.Agent
	taskID := firstString(d, "task_id", "task")
	line := func(from, to, rest string) string {
		if from == "" {
			from = "system"
		}
		if to != "" {
			from += " → " + to
		}
		return from + ": " + rest
	}
	join := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " ")
	}
	sup := func(rest string) string { return "supervisor: " + rest }

	switch e.Kind {
	case "wake":
		n, _ := d["count"].(float64)
		return line("supervisor", agent, fmt.Sprintf("wake (%d message(s))", int(n)))
	case "wake_error":
		return sup(join("wake_error", agent, taskID, trunc(firstString(d, "error"), 80)))
	case "merged":
		return sup(join("merged", taskID, "into", c.integration()))
	case "merge_conflict":
		files := ""
		if fs, ok := d["files"].([]any); ok && len(fs) > 0 {
			var names []string
			for _, f := range fs {
				names = append(names, fmt.Sprint(f))
			}
			files = "(" + trunc(strings.Join(names, ", "), 60) + ")"
		}
		return sup(join("merge_conflict", taskID, files, "→ sent back to", agent))
	case "merge_error":
		return sup(join("merge_error", taskID, trunc(firstString(d, "error"), 80)))
	case "merge_requested":
		return sup(join("merge_requested", taskID))
	case "spend_warn", "spend_cap":
		spent, _ := d["spent_usd"].(float64)
		capv, _ := d["cap_usd"].(float64)
		return sup(join(e.Kind, firstString(d, "scope"), fmt.Sprintf("$%.2f of $%.2f", spent, capv)))
	case "guard:max_turns":
		turns, _ := d["turns"].(float64)
		who := firstString(d, "agent")
		if who == "" {
			who = agent
		}
		return sup(join("guard:max_turns", who, fmt.Sprintf("%d turns on", int(turns)), firstString(d, "task")))
	case "ask_user":
		q := firstString(d, "question", "reason")
		return line(agent, "user", join("ask_user", taskID, trunc(q, 120)))
	case "create_task":
		title := firstString(d, "title")
		asg := firstString(d, "assignee")
		return line(agent, asg, join("create_task", c.taskByTitle(title, asg), trunc(title, 80)))
	case "send_message", "ask":
		return line(agent, firstString(d, "to"), join(e.Kind, trunc(firstString(d, "body", "question"), 100)))
	case "reply":
		return line(agent, "", join("reply", trunc(firstString(d, "body"), 100)))
	case "request_review":
		to := firstString(d, "to")
		if to == "" {
			to = c.roleAgent(config.RolePrincipal)
		}
		return line(agent, to, join("request_review", taskID, trunc(firstString(d, "summary"), 60)))
	case "approve", "request_changes", "cancel_task", "update_task":
		to := firstString(d, "to")
		if t, ok := c.task(taskID); ok && to == "" && t.Assignee != agent {
			to = t.Assignee
		}
		note := firstString(d, "comments", "notes", "reason", "title", "description")
		return line(agent, to, join(e.Kind, taskID, trunc(note, 80)))
	case "report":
		to := firstString(d, "to")
		if to == "" {
			to = c.roleAgent(config.RoleOrchestrator)
		}
		if to == agent {
			to = ""
		}
		return line(agent, to, join("report", taskID, firstString(d, "status"), trunc(firstString(d, "summary"), 60)))
	}
	if isEscKind(e.Kind) {
		who := agent
		if who == "" {
			who = firstString(d, "agent")
		}
		return sup(join(escKindLabel(e.Kind), who, trunc(firstString(d, "text", "message", "reason", "error"), 100)))
	}
	if strings.HasPrefix(e.Kind, "hook:") {
		return line(agent, "", join(e.Kind, firstString(d, "tool", "type")))
	}
	// Generic fallback.
	to := firstString(d, "to", "to_agent", "target")
	note := firstString(d, "title", "message", "summary", "note", "body")
	return line(agent, to, join(e.Kind, taskID, trunc(note, 100)))
}

// renderLog shows the newest h events (events are newest-first), oldest at top.
// Hook/usage noise is skipped unless showHooks is set.
func renderLog(events []store.Event, c logCtx, showHooks bool, w, h int) []string {
	var picked []store.Event
	for _, e := range events {
		if !showHooks && isNoise(e) {
			continue
		}
		picked = append(picked, e)
		if len(picked) >= h {
			break
		}
	}
	if len(picked) == 0 {
		return []string{styleDim.Render("no events yet")}
	}
	out := make([]string, 0, len(picked))
	for i := len(picked) - 1; i >= 0; i-- {
		e := picked[i]
		ts := time.Unix(e.At, 0).Format("15:04:05")
		text := c.format(e)
		switch e.Kind {
		case "rate_limited", "rate_limited_all", "modal_stuck", "revive_gave_up", "orch_down", "supervisor_down", "unread_stuck", "stuck_gave_up", "stalled":
			text = styleAmber.Render(text)
		case "ask_user", "spend_cap", "merge_conflict", "merge_error", "guard:max_turns", "wake_error":
			text = styleAmber.Render(text)
		case "merged":
			text = statusStyle("working").Render(text)
		default:
			if isNoise(e) {
				text = styleDim.Render(text)
			}
		}
		out = append(out, ansi.Truncate(styleDim.Render(ts)+" "+text, w, "…"))
	}
	return out
}
