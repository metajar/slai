package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/store"
)

type layout struct {
	lw, rw      int // list / preview widths (including borders)
	top         int // screen row of the body's first line
	bodyH       int
	pw, ph      int // preview content size = tmux window size
	showList    bool
	showPreview bool
	bannerLines []string
}

const minSplitW = 60

// orchestratorID is the team's orchestrator, from team.yaml (never assumed to be "orch").
func (m *Model) orchestratorID() string {
	if m.opts.Team != nil {
		for _, a := range m.opts.Team.Agents {
			if a.Role == "orchestrator" {
				return a.ID
			}
		}
	}
	for _, a := range m.agents {
		if a.Role == "orchestrator" {
			return a.ID
		}
	}
	return ""
}

// goalHint is the first-run line shown while the board has no tasks.
func (m *Model) goalHint() string {
	orch := m.orchestratorID()
	if len(m.tasks) > 0 || orch == "" {
		return ""
	}
	return styleAmber.Render("▸ Type your goal to " + orch + " — press o (or slai send " + orch + ` "..."). The rest of the team is driven by it.`)
}

func (m *Model) bannerLines() []string {
	out := escBannerLines(m.health, m.dismissed, m.opts.Now(), m.w)
	banner := attentionBanner(m.agents, m.events)
	if sys := systemBanner(m.events, m.opts.Now()); sys != "" {
		if banner != "" {
			banner += "  "
		}
		banner += sys
	}
	if m.held {
		if banner != "" {
			banner += "  "
		}
		banner += styleAmber.Render(holdBannerLine(m.holdName, m.holdSince, m.opts.Now()))
	}
	if g := wakeGateLine(m.paused && !m.held, m.spend, m.dailyCap(), len(m.heldAgents), queuedCount(m.health.byAgent)); g != "" {
		if banner != "" {
			banner += "  "
		}
		banner += styleAmber.Render(g)
	}
	if m.runDone != "" {
		if banner != "" {
			banner += "  "
		}
		banner += edgeGreen.Render("✔ run finished: " + trunc(strings.Join(strings.Fields(m.runDone), " "), 140))
	}
	if banner != "" {
		out = append(out, ansi.Truncate(banner, m.w, "…"))
	}
	if o := m.orphanBannerLine(); o != "" {
		out = append(out, ansi.Truncate(o, m.w, "…"))
	}
	if h := m.goalHint(); h != "" {
		out = append(out, ansi.Truncate(h, m.w, "…"))
	}
	if m.warn != "" {
		lines := strings.Split(ansi.Wordwrap(m.warn, max(m.w-1, 10), ""), "\n")
		if len(lines) > 3 {
			lines = lines[:3]
		}
		for _, l := range lines {
			out = append(out, styleAmber.Render(ansi.Truncate(l, m.w, "…")))
		}
	}
	return out
}

func (m *Model) layout() layout {
	var l layout
	l.bannerLines = m.bannerLines()
	l.top = 1 + len(l.bannerLines)
	l.bodyH = max(m.h-l.top-1, 3)
	if m.w >= minSplitW {
		l.lw = clamp(m.w/4, 26, 40)
		l.rw = m.w - l.lw
		l.showList, l.showPreview = true, true
	} else if m.mode == mFocus {
		l.rw = m.w
		l.showPreview = true
	} else {
		l.lw = m.w
		l.showList = true
		l.rw = m.w // size the window as if previewed full-width
	}
	l.pw, l.ph = max(l.rw, 1), max(l.bodyH-2, 1) // the preview has no side bars, only top and bottom rules
	return l
}

// previewSize is the size (cells) the selected agent's tmux window is set to.
func (m *Model) previewSize() (int, int) {
	l := m.layout()
	return l.pw, l.ph
}

// ---- view ----

func (m *Model) View() string {
	if m.w < 20 || m.h < 6 {
		return "terminal too small"
	}
	var base string
	switch m.ov {
	case ovHelp:
		return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, paneBorderFocus.Padding(0, 2).Render(helpText))
	case ovUsage:
		cfg := os.Getenv("CLAUDE_CONFIG_DIR")
		if cfg == "" {
			cfg = "~/.claude (CLAUDE_CONFIG_DIR unset)"
		} else {
			cfg += " (CLAUDE_CONFIG_DIR)"
		}
		body := usageDetails(m.usage, m.spend, m.month, m.opts.Now(), cfg, m.harnessUsageLines())
		return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, paneBorderFocus.Padding(0, 2).Render(body))
	case ovDetail, ovDiff:
		return fit(styleTitle.Render(m.ovTitle)+"\n"+m.vp.View()+"\n"+
			styleDim.Render(fmt.Sprintf("esc/q back · ↑↓/pgup/pgdn scroll · %3.0f%%", m.vp.ScrollPercent()*100)), m.w, m.h)
	case ovTasks:
		return m.tasksView()
	case ovLog:
		return m.logView()
	case ovMemory:
		return m.memoryView()
	case ovStats:
		return m.statsView()
	}
	base = m.mainView()
	switch m.mode {
	case mQuick:
		lead := "to " + m.quickTo + ": "
		if m.quickHold {
			lead = "hold the team — name (optional): "
		} else if m.quickRedirect {
			lead = "interrupt " + m.quickTo + " and tell it: "
		}
		body := styleBold.Render(lead) + m.quick.View()
		if h := m.completionHint(); h != "" {
			body += "\n" + h
		}
		box := paneBorderFocus.Padding(0, 1).Render(body)
		return overlayBox(base, box, m.w, m.h)
	case mAddAgent:
		return overlayBox(base, m.form.view(), m.w, m.h)
	case mConfirm:
		verb := "Stop"
		extra := "kills its tmux session (v revives it)"
		if m.confirm.what == "restart" {
			verb, extra = "Restart", "starts a FRESH conversation"
		}
		box := paneBorderFocus.Padding(0, 2).Render(styleAmber.Render(verb+" "+m.confirm.agent+"?") + "\n" + styleDim.Render(extra) + "\n\ny / enter = yes · any other key = no")
		return overlayBox(base, box, m.w, m.h)
	}
	return base
}

// fit pads / clips s to exactly h lines of at most w cells.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "")
	}
	return strings.Join(lines, "\n")
}

// overlayBox draws box centred over base (both ANSI strings).
func overlayBox(base, box string, w, h int) string {
	bl := strings.Split(base, "\n")
	xl := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	x := max((w-bw)/2, 0)
	y := max((h-len(xl))/2, 0)
	for i, line := range xl {
		r := y + i
		if r >= len(bl) {
			break
		}
		row := bl[r]
		lw := ansi.StringWidth(row)
		if lw < w {
			row += strings.Repeat(" ", w-lw)
		}
		bl[r] = ansi.Truncate(row, x, "") + "\x1b[0m" + line + "\x1b[0m" + ansi.TruncateLeft(row, x+lipgloss.Width(line), "")
	}
	return strings.Join(bl, "\n")
}

func (m *Model) headerView() string {
	left := styleTitle.Render("slai") + " · " + m.opts.Project
	right := headerStyled(m.agents, m.spend, m.dailyCap(), m.paused)
	if m.opts.Supervisor != "" {
		right += styleDim.Render(" · supervisor: " + m.opts.Supervisor)
	}
	if m.health.builds.Current != "" {
		right += styleDim.Render(" · ") + headerBuild(m.health.builds)
	}
	fits := func(r string) (string, bool) {
		gap := m.w - lipgloss.Width(left) - lipgloss.Width(r) - 1
		if gap < 1 {
			return "", false
		}
		return left + strings.Repeat(" ", gap) + r, true
	}
	if !m.usageOff {
		for _, u := range usageTiers(m.usage, m.month, m.opts.Now(), m.w) {
			if line, ok := fits(right + " · " + u); ok {
				return line
			}
		}
	}
	if line, ok := fits(right); ok {
		return line
	}
	return ansi.Truncate(left+"  "+right, m.w, "…")
}

func (m *Model) perAgentCap() float64 {
	if m.opts.Team != nil {
		return m.opts.Team.Defaults.Caps.PerAgentUSD
	}
	return 0
}

func (m *Model) dailyCap() float64 {
	if m.opts.Team != nil {
		return m.opts.Team.Defaults.Caps.DailyUSD
	}
	return 0
}

func (m *Model) mainView() string {
	l := m.layout()
	parts := []string{m.headerView()}
	parts = append(parts, l.bannerLines...)
	var body string
	switch {
	case l.showList && l.showPreview:
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listPane(l.lw, l.bodyH), m.previewPane(l, l.rw, l.bodyH))
	case l.showPreview:
		body = m.previewPane(l, l.rw, l.bodyH)
	default:
		body = m.listPane(l.lw, l.bodyH)
	}
	parts = append(parts, body, m.footerView())
	return strings.Join(parts, "\n")
}

// boxed draws a rounded box of exactly w x h cells with title in the top border.
// rows must be h-2 lines of exactly w-2 cells.
func boxed(title string, rows []string, w, h int, border lipgloss.Style) string {
	b := func(s string) string { return border.Render(s) }
	iw := w - 2
	t := ""
	if title != "" {
		t = " " + ansi.Truncate(title, max(iw-3, 1), "…") + " "
	}
	fill := max(iw-ansi.StringWidth(t)-1, 0)
	var sb strings.Builder
	sb.WriteString(b("╭─") + t + b(strings.Repeat("─", fill)+"╮"))
	for i := 0; i < h-2; i++ {
		row := strings.Repeat(" ", max(w-2, 0))
		if i < len(rows) {
			row = padTo(rows[i], w-2)
		}
		sb.WriteString("\n" + b("│") + row + "\x1b[0m" + b("│"))
	}
	sb.WriteString("\n" + b("╰"+strings.Repeat("─", iw)+"╯"))
	return sb.String()
}

// boxedOpen is boxed without the left and right bars: a rule with the title on top, a
// rule at the bottom, and rows of exactly w cells between them. Terminal-native selection
// copies whole screen columns, so the preview must not put a "│" at either edge.
// rows must be h-2 lines of exactly w cells.
func boxedOpen(title string, rows []string, w, h int, border lipgloss.Style) string {
	b := func(s string) string { return border.Render(s) }
	t := ""
	if title != "" {
		t = " " + ansi.Truncate(title, max(w-5, 1), "…") + " "
	}
	fill := max(w-2-ansi.StringWidth(t), 0)
	var sb strings.Builder
	sb.WriteString(b("──") + t + b(strings.Repeat("─", fill)))
	for i := 0; i < h-2; i++ {
		row := strings.Repeat(" ", max(w, 0))
		if i < len(rows) {
			row = padTo(rows[i], w)
		}
		sb.WriteString("\n" + row + "\x1b[0m")
	}
	sb.WriteString("\n" + b(strings.Repeat("─", max(w, 0))))
	return sb.String()
}

func padTo(s string, w int) string {
	s = ansi.Truncate(s, w, "")
	if n := ansi.StringWidth(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func (m *Model) listPane(w, h int) string {
	iw, ih := w-2, h-2
	lines := renderListH(m.items, m.cur, iw, m.agents, m.tasks, m.events, m.rowExtras(), m.mode == mList)
	if len(lines) == 0 {
		lines = []listLine{{text: styleDim.Render("no agents")}}
	}
	end := min(m.listTop+ih, len(lines))
	start := min(m.listTop, end)
	rows := make([]string, 0, ih)
	for _, ln := range lines[start:end] {
		rows = append(rows, padTo(ln.text, iw))
	}
	border := edgeDim
	if m.mode == mList {
		border = edgeBlue
	}
	return boxed(styleBold.Render("AGENTS"), rows, w, h, border)
}

func (m *Model) emptyState() bool {
	if len(m.tasks) > 0 {
		return false
	}
	for _, e := range m.events {
		if !isNoise(e) {
			return false
		}
	}
	return true
}

func (m *Model) previewPane(l layout, w, h int) string {
	a, ok := m.selAgent()
	if !ok {
		return boxedOpen(styleDim.Render("no agent selected"), nil, w, h, edgeDim)
	}
	focused := m.mode == mFocus
	var title string
	border := edgeDim
	if focused {
		border = edgeGreen
		title = styleFocusTag.Render(" FOCUSED: "+a.ID+" ") + styleDim.Render(" ctrl+q to return to list")
	} else {
		hdr := previewHeader(a, taskTitleFor(m.tasks, a.CurrentTask))
		if t := harnessTagFor(m.harnessKind(a.ID)); t != "" {
			hdr += " · " + m.harnessKind(a.ID)
		}
		title = styleBold.Render(hdr)
	}
	pw, ph := w, h-2
	if n := m.scroll[a.ID]; n > 0 {
		title += " " + styleAmber.Render(scrollNote(n))
	}
	var rows []string
	note := !focused && a.Role != "orchestrator" && ph > 4 && m.snapFor == a.ID && m.snapErr == ""
	switch {
	case m.snapFor != a.ID:
		rows = []string{styleDim.Render("loading…")}
	case m.snapErr != "":
		msg := "(not running: " + m.snapErr + ")"
		if a.Status == "dead" || a.Status == "errored" {
			msg = "(" + a.ID + " is not running — press v to revive it)"
		}
		rows = []string{styleDim.Render(msg)}
	default:
		if note {
			ph-- // the pane's last row gives way to the machine-traffic note
		}
		rows = previewRows(m.snap.Body, pw, ph)
		if focused && m.snap.CursorVisible && m.snap.Offset == 0 {
			rows = drawCursor(rows, m.snap.CursorX, m.snap.CursorY, pw)
		}
		if m.hl.active {
			rows = highlightRows(rows, m.hl.a, m.hl.b, pw)
		}
		if !focused && a.Role == "orchestrator" && m.emptyState() && ph > 2 {
			if strings.TrimSpace(ansi.Strip(rows[ph-1])) == "" {
				rows[ph-1] = centerIn(styleAmber.Render("press enter to talk to the orchestrator"), pw)
			}
		}
	}
	for i := range rows {
		rows[i] = padTo(rows[i], pw)
	}
	if note {
		for len(rows) < ph {
			rows = append(rows, "")
		}
		rows = append(rows[:ph], centerIn(styleDim.Render(machineTrafficNote(m.orchestratorID())), pw))
	}
	return boxedOpen(title, rows, w, h, border)
}

// machineTrafficNote is the preview footer of every agent but the orchestrator:
// their panes, wake lines included, are agent-to-agent traffic.
func machineTrafficNote(orch string) string {
	if orch == "" {
		orch = "the orchestrator"
	}
	return "driven by " + orch + "; you rarely need to type here"
}

// scrollNote is the "scrolled back" indicator shown in the preview title / footer.
func scrollNote(n int) string {
	return fmt.Sprintf("↑ %d %s back · wheel down / End / any typing returns", n, plural(n, "line", "lines"))
}

func centerIn(s string, w int) string {
	pad := (w - ansi.StringWidth(s)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + s
}

// previewRows maps a capture-pane -e body onto exactly h rows of w cells
// (top aligned, like the real pane), clipping anything wider or taller.
func previewRows(body string, w, h int) []string {
	body = strings.ReplaceAll(body, "\r", "")
	body = strings.TrimSuffix(body, "\n")
	lines := strings.Split(body, "\n")
	rows := make([]string, h)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			rows[i] = padTo(lines[i], w) + "\x1b[0m"
		} else {
			rows[i] = strings.Repeat(" ", w)
		}
	}
	return rows
}

// drawCursor paints a reverse-video block at (cx, cy) inside rows.
func drawCursor(rows []string, cx, cy, w int) []string {
	if cy < 0 || cy >= len(rows) || cx < 0 || cx >= w {
		return rows
	}
	line := rows[cy]
	ch := ansi.Strip(ansi.Cut(line, cx, cx+1))
	if ch == "" {
		ch = " "
	}
	out := append([]string(nil), rows...)
	out[cy] = ansi.Truncate(line, cx, "") + "\x1b[0;7m" + ch + "\x1b[0m" + ansi.TruncateLeft(line, cx+1, "")
	return out
}

// ---- footer ----

type hint struct{ key, desc string }

var listHints = []hint{
	{"↑↓", "move"}, {"enter/→", "talk to agent"}, {"space", "quick prompt"}, {"c", "redirect busy agent"}, {"o", "orchestrator"}, {"n", "new agent"}, {"g", "next waiting"},
	{"b", "tasks"}, {"l", "log"}, {"s", "stats"}, {"M", "memory"}, {"v", "revive"}, {"u", "resume"}, {"x", "stop"}, {"R", "restart"}, {"a", "attach"},
	{"p", "pause"}, {"H", "hold"}, {"t", "task"}, {"d", "diff"}, {"m", "mouse"},
}

func renderHints(hs []hint, tail []hint, w int) string {
	render := func(h hint) string { return styleBold.Render(h.key) + styleDim.Render(" "+h.desc) }
	sep := styleDim.Render(" · ")
	tailS := make([]string, len(tail))
	for i, h := range tail {
		tailS[i] = render(h)
	}
	tailStr := strings.Join(tailS, sep)
	used := ansi.StringWidth(tailStr)
	var parts []string
	for _, h := range hs {
		r := render(h)
		if used+ansi.StringWidth(r)+3 > w {
			break
		}
		parts = append(parts, r)
		used += ansi.StringWidth(r) + 3
	}
	parts = append(parts, tailStr)
	return strings.Join(parts, sep)
}

func (m *Model) footerView() string {
	if m.mode == mFocus {
		id := m.sel
		s := styleFocusTag.Render(" FOCUSED: "+id+" ") + styleDim.Render(" every key goes to the agent · ") +
			styleBold.Render("ctrl+q") + styleDim.Render(" return to list")
		if m.status != "" && m.opts.Now().Sub(m.statAt) < 8*time.Second {
			s += "  " + styleAmber.Render(m.status)
		}
		if m.paneKnown() && m.snap.MouseAny {
			s += "  " + styleDim.Render("mouse: app · drag selects")
		} else {
			s += "  " + styleDim.Render("select: drag")
		}
		if n := m.scroll[id]; n > 0 {
			s += "  " + styleAmber.Render(scrollNote(n))
		}
		if m.snd != nil {
			if e := m.snd.lastErr(); e != "" {
				s += " " + styleErr.Render(e)
			}
		}
		return ansi.Truncate(s, m.w, "…")
	}
	if !m.mouseOn {
		return ansi.Truncate(styleAmber.Render("mouse capture OFF — terminal selection / scroll active · ")+styleBold.Render("m")+styleDim.Render(" turns it back on"), m.w, "…")
	}
	if m.status != "" && m.opts.Now().Sub(m.statAt) < 8*1e9 {
		if m.statusE {
			return ansi.Truncate(styleErr.Render(m.status), m.w, "…")
		}
		return ansi.Truncate(styleAmber.Render(m.status), m.w, "…")
	}
	switch m.mode {
	case mQuick:
		if m.quickHold {
			return styleDim.Render("enter holds the team (no wakes, nudges or restarts; sessions stay up) · esc cancel")
		}
		if m.quickRedirect {
			return styleDim.Render("enter interrupts " + m.quickTo + " and sends this as a course correction · esc cancel")
		}
		return styleDim.Render("enter send to " + m.quickTo + " · tab completes task ids, branches, agents · esc cancel")
	case mAddAgent:
		return styleDim.Render("ctrl+s save · esc cancel")
	case mConfirm:
		return styleAmber.Render("y confirm · any other key cancels")
	}
	switch m.ov {
	case ovTasks:
		return styleDim.Render("↑↓ move · enter detail · d diff · +/- priority · T top · esc back")
	case ovStats:
		return styleDim.Render("esc back · numbers refresh with the board")
	case ovMemory:
		return m.memoryFooter()
	case ovLog:
		return styleDim.Render("h hook events " + onOff(m.showHooks) + " · esc back")
	}
	hs := listHints
	if m.emptyState() && len(m.agents) > 0 {
		hs = append([]hint{hs[0], {"enter", "talk to the orchestrator"}}, hs[2:]...)
	}
	return renderHints(hs, []hint{{"?", "help"}, {"q", "quit"}}, m.w)
}

// ---- full-screen overlays ----

func (m *Model) tasksView() string {
	title := styleTitle.Render("Tasks") + styleDim.Render(fmt.Sprintf("  %d total", len(m.tasks)))
	lines := renderTasks(m.tasks, m.tasksCur, m.w, m.h-2)
	return fit(title+"\n"+strings.Join(lines, "\n"), m.w, m.h-1) + "\n" + m.footerOverlay()
}

func (m *Model) logView() string {
	title := styleTitle.Render("Log") + styleDim.Render("  newest at the bottom · h toggles hook events ("+onOff(m.showHooks)+")")
	lines := renderLog(m.events, logCtx{team: m.opts.Team, tasks: m.tasks}, m.showHooks, m.w, m.h-2)
	return fit(title+"\n"+strings.Join(lines, "\n"), m.w, m.h-1) + "\n" + m.footerOverlay()
}

func (m *Model) footerOverlay() string {
	if m.status != "" && m.opts.Now().Sub(m.statAt) < 8*1e9 && m.statusE {
		return ansi.Truncate(styleErr.Render(m.status), m.w, "…")
	}
	return m.footerView()
}

var _ = store.StatusMerged
