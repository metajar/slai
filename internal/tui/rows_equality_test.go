package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	webagents "github.com/oorbt/slai/internal/web/agents"
)

// P3 AC1 (serve.md): for the same board input, the TUI's agent rows and the web list's rows
// carry identical state, task, age and reason, across the liveness states. Both are built by
// internal/agentrows; this test pins that nothing downstream re-words them.
func TestTUIAndWebAgentRowsAgreeAcrossLivenessStates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	cases := []struct {
		name   string
		agent  store.Agent
		health service.Health
	}{
		{"working with a task", store.Agent{ID: "eng-1", Role: "engineer", Model: "claude-sonnet-4", Status: "working", StatusAt: ago(3 * time.Minute), CurrentTask: "T-0007"}, service.Health{Liveness: "alive"}},
		{"idle, last tool shown", store.Agent{ID: "eng-2", Role: "engineer", Model: "opus", Status: "idle", StatusAt: ago(2 * time.Hour)}, service.Health{Liveness: "idle"}},
		{"waiting on the user (blocked)", store.Agent{ID: "pm", Role: "product_manager", Model: "haiku", Status: "waiting_on_user", StatusAt: ago(40 * time.Second)}, service.Health{Liveness: "blocked", Reason: "permission dialog open"}},
		{"dead", store.Agent{ID: "fg-power", Role: "focus_group", Model: "haiku", Status: "dead", StatusAt: ago(3 * 24 * time.Hour)}, service.Health{Liveness: "dead", Reason: "session exited"}},
		{"gated by a team hold", store.Agent{ID: "eng-3", Role: "engineer", Model: "sonnet", Status: "idle", StatusAt: ago(15 * time.Minute)}, service.Health{Liveness: "gated", Reason: "team hold"}},
		{"stale", store.Agent{ID: "eng-4", Role: "engineer", Model: "sonnet", Status: "working", StatusAt: ago(30 * time.Minute), CurrentTask: "T-0009"}, service.Health{Liveness: "stale", Reason: "no hook events for 12m"}},
		{"rate limited", store.Agent{ID: "fg-skeptic", Role: "focus_group", Model: "haiku", Status: "idle", StatusAt: ago(6 * time.Minute)}, service.Health{Liveness: "rate_limited", Reason: "resets 14:20"}},
		{"finished", store.Agent{ID: "principal", Role: "principal_engineer", Model: "opus", Status: "finished", StatusAt: ago(90 * time.Minute)}, service.Health{Liveness: "alive"}},
		{"status says working, session dead", store.Agent{ID: "orch", Role: "orchestrator", Model: "opus", Status: "working", StatusAt: ago(5 * time.Minute)}, service.Health{Liveness: "dead", Reason: "tmux session gone"}},
		{"never recorded", store.Agent{ID: "fg-newbie", Role: "focus_group", Model: "haiku", Status: "idle"}, service.Health{Liveness: "idle"}},
	}
	tasks := []store.Task{{ID: "T-0007", Title: "Add the thing"}, {ID: "T-0009", Title: "Fix the other thing"}}
	events := []store.Event{{ID: 9, Agent: "eng-2", Kind: "hook:posttooluse", Data: `{"tool":"Bash"}`}}
	var agents []store.Agent
	health := map[string]service.Health{}
	for _, c := range cases {
		agents = append(agents, c.agent)
		health[c.agent.ID] = c.health
	}
	rows := webagents.RowsFrom(webagents.Inputs{Agents: agents, Tasks: tasks, Events: events, Health: health, Now: now})
	byID := map[string]webagents.Row{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	items := buildItems(agents, nil)
	lines := renderListH(items, -1, 140, agents, tasks, events, rowExtras{health: health, now: now}, false)
	for _, c := range cases {
		w := byID[c.agent.ID]
		// state: the web's labels and chip are the TUI's
		label, detail := healthChip(c.health, c.agent.Status)
		if w.ChipLabel != label {
			t.Errorf("%s: chip web %q, tui %q", c.name, w.ChipLabel, label)
		}
		if w.StatusLabel != statusLabel(c.agent.Status) || w.Waiting != isWaiting(c.agent.Status) {
			t.Errorf("%s: status label web %q (waiting %v), tui %q", c.name, w.StatusLabel, w.Waiting, statusLabel(c.agent.Status))
		}
		if w.Task != c.agent.CurrentTask || w.Model != shortModel(c.agent.Model) || w.Tag != harnessTagFor(w.Harness) {
			t.Errorf("%s: task/model/tag differ: %+v", c.name, w)
		}
		// age and reason: the TUI's second line is exactly the web row's fields joined
		head := w.Activity
		if w.Task != "" {
			head = w.Task
		}
		want := []string{head}
		if w.Ago != "" {
			want = append(want, w.Ago)
		}
		if w.Reason != "" {
			want = append(want, w.Reason)
		}
		// the TUI word the dead hint differently; compare the shared parts only
		got := rowStatusLine(c.agent, tasks, events, c.health, now, "")
		if w.Task == "" && c.agent.Status == "dead" {
			got = strings.Replace(got, "not running · v to revive", "not running", 1)
		}
		if got != strings.Join(want, " · ") {
			t.Errorf("%s: TUI line %q, web fields %q", c.name, got, strings.Join(want, " · "))
		}
		if w.Ago != agentrows.StatusAge(c.agent, now) {
			t.Errorf("%s: age %q", c.name, w.Ago)
		}
		// and what the TUI paints for that row carries the same words
		var l1, l2 string
		for i, l := range lines {
			if strings.Contains(ansi.Strip(l.text), c.agent.ID+" ") && i+1 < len(lines) && l.item == lines[i+1].item {
				l1, l2 = ansi.Strip(l.text), ansi.Strip(lines[i+1].text)
				break
			}
		}
		shown := w.StatusLabel
		if w.ChipLabel != "" {
			shown = w.ChipLabel
		}
		if !strings.Contains(l1, shown) {
			t.Errorf("%s: the TUI row %q does not show %q", c.name, l1, shown)
		}
		for _, part := range []string{w.Task, w.Ago} {
			if part != "" && !strings.Contains(l2, part) {
				t.Errorf("%s: the TUI second line %q lacks %q", c.name, l2, part)
			}
		}
		if reason := firstNonEmpty(w.Reason, detail); reason != "" && !isWaiting(c.agent.Status) && !strings.Contains(l2, reason) {
			t.Errorf("%s: the TUI second line %q lacks the reason %q", c.name, l2, reason)
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// One implementation: the row logic lives only in internal/agentrows. The TUI and the web
// package keep no copy of the status wording, grouping, chip, age or activity rules.
func TestRowLogicHasASingleImplementation(t *testing.T) {
	if got := agentrows.StatusLabel("waiting_on_user"); got != "needs you" || statusLabel("waiting_on_user") != got {
		t.Errorf("status label: %q", got)
	}
	if agentrows.Age(3*time.Minute) != ageShort(3*time.Minute) || agentrows.ShortModel("claude-opus-4") != shortModel("claude-opus-4") {
		t.Error("age or model wording drifted")
	}
	if groupKeyFor("principal_engineer") != agentrows.GroupKey("principal_engineer") || webagents.GroupTitle("planning") != agentrows.GroupTitle("planning") {
		t.Error("grouping drifted")
	}
}
