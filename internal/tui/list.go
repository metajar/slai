package tui

import (
	"fmt"
	"github.com/oorbt/slai/internal/agentrows"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// groupDef orders the agent list: roles are bucketed into foldable groups.
// groupKeyFor is the group a role is listed under (the shared grouping, agentrows.Groups).
func groupKeyFor(role string) string { return agentrows.GroupKey(role) }

// item is one navigable row of the agent list: a group header or an agent.
type item struct {
	group  string // group key (always set)
	title  string // header title (headers only)
	agent  string // agent id; "" for headers
	count  int    // header: agents in group
	needs  int    // header: agents waiting
	folded bool   // header
	paused bool   // agent: held by slai (loop guard / manual)
}

func (it item) isHeader() bool { return it.agent == "" }

// buildItems groups agents by role bucket; within a group, waiting agents come
// first. Folded groups show only their header.
func buildItems(agents []store.Agent, folded map[string]bool) []item {
	byGroup := map[string][]store.Agent{}
	for _, a := range agents {
		k := groupKeyFor(a.Role)
		byGroup[k] = append(byGroup[k], a)
	}
	var out []item
	for _, g := range agentrows.Groups {
		as := byGroup[g.Key]
		if len(as) == 0 {
			continue
		}
		sort.SliceStable(as, func(i, j int) bool { return isWaiting(as[i].Status) && !isWaiting(as[j].Status) })
		needs := 0
		for _, a := range as {
			if isWaiting(a.Status) {
				needs++
			}
		}
		out = append(out, item{group: g.Key, title: g.Title, count: len(as), needs: needs, folded: folded[g.Key]})
		if folded[g.Key] {
			continue
		}
		for _, a := range as {
			out = append(out, item{group: g.Key, agent: a.ID})
		}
	}
	return out
}

// The row wording lives in internal/agentrows so the web list says the same; these are the
// TUI's names for it.
func shortModel(m string) string       { return agentrows.ShortModel(m) }
func statusLabel(status string) string { return agentrows.StatusLabel(status) }

// tuiRow is an agent's row (agentrows.Build) worded for the TUI: the same builder the web list
// uses, with the TUI's activity wording. kind may be "" (claude).
func tuiRow(a store.Agent, tasks []store.Task, events []store.Event, h service.Health, kind string, now time.Time) agentrows.Row {
	return agentrows.Build(a, h, agentrows.Input{
		Titles: map[string]string{a.CurrentTask: taskTitleFor(tasks, a.CurrentTask)}, Events: events, Harness: kind, Now: now,
		Text: agentrows.ActivityText{NotRunning: "not running · v to revive", Event: func(e store.Event) string { return trunc(formatEvent(e), 60) }},
	})
}

// agentActivity is the dim second line of a row (agentrows.Activity), worded for the TUI: a
// dead agent points at the revive key, and the latest event is shown formatted.
func agentActivity(a store.Agent, tasks []store.Task, events []store.Event) string {
	return agentrows.Activity(a, taskTitleFor(tasks, a.CurrentTask), events, agentrows.ActivityText{
		NotRunning: "not running · v to revive",
		Event:      func(e store.Event) string { return trunc(formatEvent(e), 60) },
	})
}

type listLine struct {
	text string
	item int // index into items
}

// renderList renders items as lines of width w (2 lines per agent). The
// cursor row is highlighted. Every line is tagged with its item for mouse hits.
func renderList(items []item, cur int, w int, agents []store.Agent, tasks []store.Task, events []store.Event, listFocused bool) []listLine {
	return renderListH(items, cur, w, agents, tasks, events, rowExtras{}, listFocused)
}

// rowExtras is the per-agent data renderListH shows beyond the store rows.
type rowExtras struct {
	health      map[string]service.Health
	oldBuild    map[string]bool
	tags        map[string]string // harness tags
	spend       map[string]float64
	perAgentCap float64
	now         time.Time // zero = time.Now()
}

// ageShort renders a compact age: 45s, 4m, 5h, 3d (agentrows.Age).
func ageShort(d time.Duration) string { return agentrows.Age(d) }

// renderListH is renderList plus per-agent health: a liveness chip replaces the
// status label (and its reason the activity line) when status alone misleads.
func renderListH(items []item, cur int, w int, agents []store.Agent, tasks []store.Task, events []store.Event, x rowExtras, listFocused bool) []listLine {
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	idw := 4
	for _, a := range agents {
		if n := len([]rune(a.ID)); n > idw {
			idw = n
		}
	}
	idw = min(idw, max(w-12, 4))
	now := x.now
	if now.IsZero() {
		now = time.Now()
	}
	n := 0 // 1-based position among the visible agents: the digit key that jumps to it
	var out []listLine
	for i, it := range items {
		if it.isHeader() {
			fold := "▾"
			if it.folded {
				fold = "▸"
			}
			t := fmt.Sprintf("%s %s (%d)", fold, it.title, it.count)
			needs := ""
			if it.needs > 0 {
				needs = fmt.Sprintf("▲ %d needs you", it.needs)
			}
			line := ansi.Truncate(styleBold.Render(t)+" "+styleAmber.Render(needs), w, "…")
			if i == cur {
				line = selRow(ansi.Strip(line), w, listFocused)
			}
			out = append(out, listLine{line, i})
			continue
		}
		a := byID[it.agent]
		n++
		lead := " "
		if n <= 9 {
			lead = styleDim.Render(strconv.Itoa(n))
		}
		row := tuiRow(a, tasks, events, x.health[a.ID], "", now)
		st := statusStyle(a.Status)
		label := st.Render(row.StatusLabel)
		chip, chipWhy := row.ChipLabel, row.ChipDetail
		if chip != "" {
			label = styleAmber.Render(chip)
		}
		capped := agentSpendCapped(x.spend[a.ID], x.perAgentCap)
		if capped {
			label = styleAmber.Render("capped")
		}
		if it.paused {
			label = styleAmber.Render("⏸ paused")
		}
		id := fmt.Sprintf("%-*s", idw, ansi.Truncate(a.ID, idw, ""))
		tag := ""
		if t := x.tags[a.ID]; t != "" { // non-claude harness: a short dim tag after the id
			tag = " " + styleDim.Render(t)
		}
		left := lead + st.Render(glyph(a.Status)) + " " + styleBold.Render(id) + tag + " " + styleDim.Render(shortModel(a.Model))
		pad := w - lipgloss.Width(left) - lipgloss.Width(label) - 1
		if x.oldBuild[a.ID] { // dim chip: keep the status label, give up the model name before the chip
			chipped := styleDim.Render("old build") + " " + label
			bare := lead + st.Render(glyph(a.Status)) + " " + styleBold.Render(id) + tag
			switch {
			case w-lipgloss.Width(left)-lipgloss.Width(chipped)-1 >= 1:
				label, pad = chipped, w-lipgloss.Width(left)-lipgloss.Width(chipped)-1
			case w-lipgloss.Width(bare)-lipgloss.Width(chipped)-1 >= 1:
				left, label, pad = bare, chipped, w-lipgloss.Width(bare)-lipgloss.Width(chipped)-1
			}
		}
		var l1 string
		if pad >= 1 {
			l1 = left + strings.Repeat(" ", pad) + label + " "
		} else {
			l1 = ansi.Truncate(left, w, "…")
		}
		l2 := "   " + styleDim.Render(ansi.Truncate(rowStatusLine(a, tasks, events, x.health[a.ID], now, ""), max(w-4, 1), "…"))
		if isWaiting(a.Status) {
			l2 = "   " + styleAmber.Render(ansi.Truncate("▲ "+rowStatusLine(a, tasks, events, x.health[a.ID], now, agentActivityWaiting(a, events)), max(w-4, 1), "…"))
		}
		if chip != "" && !isWaiting(a.Status) {
			why := chip
			if chipWhy != "" {
				why += ": " + chipWhy
			}
			l2 = "   " + styleAmber.Render(ansi.Truncate("⚠ "+why+" · "+rowStatusLine(a, tasks, events, service.Health{}, now, ""), max(w-4, 1), "…"))
		}
		if capped && !it.paused {
			l2 = "   " + styleAmber.Render(ansi.Truncate("⏸ per-agent spend cap reached · no wake-ups", max(w-4, 1), "…"))
		}
		if it.paused {
			l2 = "   " + styleAmber.Render(ansi.Truncate("⏸ held by slai · u to resume", max(w-4, 1), "…"))
		}
		if i == cur {
			l1 = selRow(ansi.Strip(l1), w, listFocused)
			l2 = selRow(ansi.Strip(l2), w, listFocused)
		}
		out = append(out, listLine{l1, i}, listLine{l2, i})
	}
	return out
}

// agentActivityWaiting is the second line for a waiting agent: its question.
func agentActivityWaiting(a store.Agent, events []store.Event) string {
	if a.Status == "waiting_on_user" {
		for _, e := range events {
			if e.Kind == "ask_user" && e.Agent == a.ID {
				if q := firstString(parseData(e.Data), "question", "reason"); q != "" {
					return trunc(q, 80)
				}
			}
		}
		return "waiting for your answer"
	}
	return "permission prompt — focus to answer"
}

var (
	styleSelFocus = lipgloss.NewStyle().Reverse(true)
	styleSelDim   = lipgloss.NewStyle().Background(lipgloss.Color("238")).Bold(true)
)

func selRow(plain string, w int, focused bool) string {
	plain = ansi.Truncate(plain, w, "")
	plain += strings.Repeat(" ", max(w-ansi.StringWidth(plain), 0))
	if focused {
		return styleSelFocus.Render(plain)
	}
	return styleSelDim.Render(plain)
}

// rowStatusLine is the agent row's second line: current task (or what it last
// did), the age of its status, and why when liveness is blocked, gated, stale,
// dead or rate-limited. why, when given, replaces the liveness reason. It is one
// line; the caller truncates it to the row width.
func rowStatusLine(a store.Agent, tasks []store.Task, events []store.Event, h service.Health, now time.Time, why string) string {
	r := tuiRow(a, tasks, events, h, "", now)
	head := r.Activity
	if r.Task != "" {
		head = r.Task // the task title does not fit next to age and reason
	}
	parts := []string{head}
	if r.Age != "" {
		parts = append(parts, r.Age)
	}
	if why == "" {
		why = r.Reason
	}
	if why != "" {
		parts = append(parts, why)
	}
	return strings.Join(parts, " · ")
}
