package tui

import (
	"fmt"
	"github.com/oorbt/slai/internal/agentrows"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// harnessKind is the harness an agent runs on, from team.yaml (claude when unknown).
func (m *Model) harnessKind(id string) string { return agentrows.HarnessOf(m.opts.Team, id) }

// harnessTagFor is the short row tag for a harness; claude has none (agentrows.HarnessTag).
func harnessTagFor(kind string) string { return agentrows.HarnessTag(kind) }

// harnessTags maps agent id -> tag for the agents that do not run on claude.
func (m *Model) harnessTags() map[string]string {
	out := map[string]string{}
	for _, a := range m.agents {
		if t := harnessTagFor(m.harnessKind(a.ID)); t != "" {
			out[a.ID] = t
		}
	}
	return out
}

// harnessFeatures are the capabilities of a harness kind (the claude set if unknown).
func harnessFeatures(kind string) harness.Features {
	h, err := harness.For(harness.Kind(kind))
	if err != nil {
		h, _ = harness.For(harness.Claude)
	}
	return h.Features()
}

// degradedText renders the degraded capabilities of a harness, with the reason
// form the board uses ("not supported by pi").
func degradedText(kind string) string {
	d := harnessFeatures(kind).Degraded()
	if len(d) == 0 {
		return "none"
	}
	return strings.Join(d, ", ") + " (not supported by " + kind + ")"
}

// gaugesNA reports whether the 5h/weekly subscription gauges mean nothing for the harness.
func gaugesNA(kind string) bool { return !harnessFeatures(kind).Statusline }

// agentInfo is the agent detail text: harness, model and degraded features.
func (m *Model) agentInfo(a store.Agent) string {
	kind := m.harnessKind(a.ID)
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "  %-14s %s\n", k, v) }
	b.WriteString(styleBold.Render(a.ID+" · "+a.Role) + "\n\n")
	line("harness", kind)
	line("model", a.Model)
	line("status", a.Status)
	line("degraded", degradedText(kind))
	if gaugesNA(kind) {
		line("5h/weekly", "n/a — not supported by "+kind)
	}
	b.WriteString("\n" + styleBold.Render("Recent health events") + "\n")
	evs := m.recentHealthEvents(a.ID, 5)
	if len(evs) == 0 {
		b.WriteString(styleDim.Render("  none") + "\n")
	}
	for _, e := range evs {
		b.WriteString("  " + healthEventLine(e, m.opts.Now()) + "\n")
	}
	b.WriteString(m.laneDenialLines(a.ID))
	b.WriteString("\n" + styleBold.Render("Course corrections") + "\n")
	cc := m.courseCorrections(a.ID, 5)
	if len(cc) == 0 {
		b.WriteString(styleDim.Render("  none") + "\n")
	}
	for _, c := range cc {
		b.WriteString("  " + ageShort(m.opts.Now().Sub(time.Unix(c.CreatedAt, 0))) + " ago  " + trunc(strings.TrimPrefix(c.Body, supervisor.CoursePrefix), 100) + "\n")
	}
	return b.String()
}

// courseCorrections returns the newest course-correction messages sent to the agent.
func (m *Model) courseCorrections(id string, n int) []store.Message {
	if m.opts.Store == nil {
		return nil
	}
	msgs, err := m.opts.Store.RecentMessages(300)
	if err != nil {
		return nil
	}
	var out []store.Message
	for _, x := range msgs {
		if x.To == id && strings.HasPrefix(x.Body, supervisor.CoursePrefix) {
			if out = append(out, x); len(out) == n {
				break
			}
		}
	}
	return out
}

// harnessUsageLines extends the usage panel with the agents whose usage is not
// a Claude subscription: n/a (never 0%) for the gauges, plus their spend when known.
func (m *Model) harnessUsageLines() string {
	var b strings.Builder
	for _, a := range m.agents {
		kind := m.harnessKind(a.ID)
		if !gaugesNA(kind) {
			continue
		}
		spend := "spend n/a"
		if v, ok := m.agentSpend[a.ID]; ok && v > 0 {
			spend = fmt.Sprintf("~$%.2f today", v)
		}
		fmt.Fprintf(&b, "  %-14s 5h n/a · weekly n/a · %s (not supported by %s)\n", a.ID, spend, kind)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + styleBold.Render("Other harnesses") + "\n" + b.String()
}

// rowExtras gathers what the agent rows show beyond the store rows.
func (m *Model) rowExtras() rowExtras {
	return rowExtras{health: m.health.byAgent, oldBuild: oldBuildAgents(m.health.builds), tags: m.harnessTags(),
		spend: m.agentSpend, perAgentCap: m.perAgentCap(), now: m.opts.Now()}
}

// healthEventKinds are the event kinds the agent detail lists as "why did slai do
// that": reminders and restarts, escalations, upgrades and membership, lane denials.
var healthEventKinds = []string{
	"nudge", "unread_nudge", "stuck_detected", "stuck_nudge", "stuck_restart", "stuck_failed", "stuck_cleared", "stuck_recovered",
	"auto_revive", "revive_fresh", "agent_restarted", "agent_upgraded", "agent_added", "agent_removed",
	"mcp_restart", "mcp_dead", "stalled", "loop_released", "wake_gave_up", "spend_cap",
	"rate_limited", "rate_limited_all", "modal_stuck", "revive_gave_up", "orch_down", "unread_stuck", "stuck_gave_up",
	"guard:lane", "lane_denied", "guard_missing", "orch_restart", "agent_stopped",
}

// recentHealthEvents returns the agent's n newest health events, newest first.
func (m *Model) recentHealthEvents(agent string, n int) []store.Event {
	if m.opts.Store == nil {
		return nil
	}
	var all []store.Event
	for _, k := range healthEventKinds {
		if es, err := m.opts.Store.EventsByKind(agent, k, n); err == nil {
			all = append(all, es...)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// healthEventLine is "kind  3m ago  cause": the cause is the event's own reason,
// cause, text or tool field when it has one.
func healthEventLine(e store.Event, now time.Time) string {
	kind := e.Kind
	if kind == "guard:lane" {
		kind = "lane_denied"
	}
	cause := firstString(parseData(e.Data), "cause", "reason", "why", "text", "error", "tool")
	line := fmt.Sprintf("%-14s %5s ago", kind, ageShort(now.Sub(time.Unix(e.At, 0))))
	if cause != "" {
		line += "  " + trunc(cause, 70)
	}
	return line
}

// laneDenialLines is the agent detail's lane-guard section: the count over the last
// 24 hours and the newest three denials (tool + target).
func (m *Model) laneDenialLines(id string) string {
	var b strings.Builder
	b.WriteString("\n" + styleBold.Render("Lane denials") + "\n")
	if m.opts.Store == nil {
		return b.String() + styleDim.Render("  none") + "\n"
	}
	now := m.opts.Now()
	counts, _ := m.opts.Store.LaneDenialCounts(now.Add(-24 * time.Hour).Unix())
	evs, _ := m.opts.Store.EventsByKind(id, "lane_denied", 3)
	if counts[id] == 0 && len(evs) == 0 {
		return b.String() + styleDim.Render("  none in the last 24h") + "\n"
	}
	fmt.Fprintf(&b, "  %d in the last 24h\n", counts[id])
	for _, e := range evs {
		d := parseData(e.Data)
		what := firstString(d, "tool")
		if t := firstString(d, "path", "target"); t != "" {
			what += " " + trunc(t, 80)
		}
		fmt.Fprintf(&b, "  %5s ago  %s\n", ageShort(now.Sub(time.Unix(e.At, 0))), what)
	}
	return b.String()
}
