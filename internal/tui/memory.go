package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// memFilters is the state filter's cycle: all, then each state.
var memFilters = []string{"", memory.StateFresh, memory.StateStale, memory.StateUnverified, memory.StateInvalid}

// memState is the Memory view: the facts with their state computed against integration.
type memState struct {
	views   []memory.View
	invalid []memory.Invalid
	loaded  bool
	err     string
	cur     int
	filter  int    // index into memFilters
	detail  bool   // the selected fact's file and blob comparison
	confirm string // id waiting for a y/n delete confirmation
	lines   []string
}

// memRow is one line of the list: a fact or an invalid file.
type memRow struct {
	ID, Kind, State, Path, Text, Reason string
	More                                int
}

type memLoadedMsg struct {
	views   []memory.View
	invalid []memory.Invalid
	err     error
}

type memDoneMsg struct {
	note string
	err  error
}

func (m *Model) memRows() []memRow {
	var rows []memRow
	for _, v := range m.mem.views {
		r := memRow{ID: v.ID, Kind: v.Kind, State: v.State, Text: v.Text, Reason: v.Reason}
		if len(v.Paths) > 0 {
			r.Path, r.More = v.Paths[0].Path, len(v.Paths)-1
		}
		rows = append(rows, r)
	}
	for _, b := range m.mem.invalid {
		rows = append(rows, memRow{ID: b.ID, Kind: "-", State: memory.StateInvalid, Text: b.Reason})
	}
	f := memFilters[m.mem.filter]
	if f == "" {
		return rows
	}
	var out []memRow
	for _, r := range rows {
		if r.State == f {
			out = append(out, r)
		}
	}
	return out
}

func (m *Model) openMemory() tea.Cmd {
	if m.opts.Memory == nil {
		m.setStatus("no team memory here (needs a project with .slai/)", true)
		return nil
	}
	m.ov = ovMemory
	m.mem.detail, m.mem.confirm = false, ""
	return m.memLoadCmd()
}

// memLoadCmd computes the facts' states off the UI goroutine (one git call for all of them).
func (m *Model) memLoadCmd() tea.Cmd {
	mm := m.opts.Memory
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		vs, bad, err := mm.Views(ctx)
		return memLoadedMsg{views: vs, invalid: bad, err: err}
	}
}

func (m *Model) onMemLoaded(msg memLoadedMsg) {
	if msg.err != nil {
		m.mem.err = msg.err.Error()
		return
	}
	m.mem.err, m.mem.loaded = "", true
	m.mem.views, m.mem.invalid = msg.views, msg.invalid
	sort.SliceStable(m.mem.views, func(i, j int) bool { return memRank(m.mem.views[i].State) < memRank(m.mem.views[j].State) })
	if n := len(m.memRows()); m.mem.cur >= n {
		m.mem.cur = max(n-1, 0)
	}
	m.mem.lines = nil
	if m.mem.detail {
		m.mem.lines = m.memDetailLines()
	}
}

// memRank puts what needs attention first: stale, invalid, unverified, then fresh.
func memRank(state string) int {
	switch state {
	case memory.StateStale:
		return 0
	case memory.StateInvalid:
		return 1
	case memory.StateUnverified:
		return 2
	}
	return 3
}

func (m *Model) selMem() (memRow, bool) {
	rows := m.memRows()
	if m.mem.cur < 0 || m.mem.cur >= len(rows) {
		return memRow{}, false
	}
	return rows[m.mem.cur], true
}

func (m *Model) memoryKey(k tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if m.mem.confirm != "" { // y deletes; anything else keeps the fact
		id := m.mem.confirm
		m.mem.confirm = ""
		if key == "y" || key == "Y" {
			m.setStatus("deleting "+id+"…", false)
			return m, m.memDeleteCmd(id)
		}
		m.setStatus("kept "+id, false)
		return m, nil
	}
	n := len(m.memRows())
	switch key {
	case "esc", "q", "M":
		if m.mem.detail {
			m.mem.detail = false
			return m, nil
		}
		m.ov = ovNone
	case "up", "k":
		m.mem.cur = clamp(m.mem.cur-1, 0, max(n-1, 0))
	case "down", "j":
		m.mem.cur = clamp(m.mem.cur+1, 0, max(n-1, 0))
	case "home", "g":
		m.mem.cur = 0
	case "end", "G":
		m.mem.cur = max(n-1, 0)
	case "f":
		m.mem.filter = (m.mem.filter + 1) % len(memFilters)
		m.mem.cur = 0
	case "enter":
		if _, ok := m.selMem(); ok {
			m.mem.detail = true
			m.mem.lines = m.memDetailLines()
			return m, m.memDetailCmd()
		}
	case "d":
		if r, ok := m.selMem(); ok {
			m.mem.confirm = r.ID
		}
	case "e":
		if r, ok := m.selMem(); ok {
			return m, m.memEditCmd(r.ID)
		}
	}
	return m, nil
}

// memDeleteCmd removes the fact through the memory package, the single writer of memory files
// (file and index row, under its lock); the TUI never touches the file itself.
func (m *Model) memDeleteCmd(id string) tea.Cmd {
	mm := m.opts.Memory
	return func() tea.Msg {
		if err := mm.Delete(id); err != nil {
			return memDoneMsg{err: err}
		}
		return memDoneMsg{note: "deleted " + id}
	}
}

// memEditCmd opens the fact file in $VISUAL / $EDITOR and, when the editor exits, hands the
// edited bytes to the memory package (AcceptEdit: the same path the supervisor's hand-edit
// tick uses), which validates them, records verified_by: user and re-grounds the fact. The
// TUI itself writes nothing.
func (m *Model) memEditCmd(id string) tea.Cmd {
	mm := m.opts.Memory
	path := filepath.Join(mm.Dir, id+".md")
	editor := m.opts.Editor
	if editor == nil {
		editor = defaultEditor
	}
	return tea.ExecProcess(editor(path), func(err error) tea.Msg { return acceptEdited(mm, id, path, err) })
}

// acceptEdited is what happens when the editor exits: on an editor error nothing else does;
// otherwise the bytes on disk go to AcceptEdit.
func acceptEdited(mm *memory.Memory, id, path string, editorErr error) memDoneMsg {
	if editorErr != nil {
		return memDoneMsg{err: fmt.Errorf("editor: %w", editorErr)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return memDoneMsg{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, _, aerr := mm.AcceptEdit(ctx, id, data)
	if aerr != nil {
		return memDoneMsg{err: fmt.Errorf("%s is invalid: %v (the file is left as you saved it)", id, aerr)}
	}
	return memDoneMsg{note: fmt.Sprintf("%s saved: verified by %s", f.ID, f.VerifiedBy)}
}

func defaultEditor(path string) *exec.Cmd {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	parts := strings.Fields(ed)
	return exec.Command(parts[0], append(parts[1:], path)...)
}

func (m *Model) onMemDone(msg memDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.setStatus(msg.err.Error(), true)
	} else {
		m.setStatus(msg.note, false)
	}
	m.mem.detail = false
	return m.memLoadCmd()
}

// memDetailMsg carries the current blobs of the selected fact's cited paths.
type memDetailMsg struct {
	id    string
	blobs map[string]string
	err   error
}

func (m *Model) memDetailCmd() tea.Cmd {
	r, ok := m.selMem()
	if !ok {
		return nil
	}
	mm := m.opts.Memory
	return func() tea.Msg {
		f, err := mm.Get(r.ID)
		if err != nil {
			return memDetailMsg{id: r.ID, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		b, err := mm.CurrentBlobs(ctx, f)
		return memDetailMsg{id: r.ID, blobs: b, err: err}
	}
}

func (m *Model) onMemDetail(msg memDetailMsg) {
	if m.mem.detail {
		m.mem.lines = m.memDetailLinesWith(msg)
	}
}

func (m *Model) memDetailLines() []string { return m.memDetailLinesWith(memDetailMsg{}) }

// memDetailLinesWith renders the selected fact: its file, and for each cited path the blob
// recorded against the one on integration now.
func (m *Model) memDetailLinesWith(d memDetailMsg) []string {
	r, ok := m.selMem()
	if !ok {
		return nil
	}
	var out []string
	if m.opts.Memory != nil {
		if raw, err := os.ReadFile(filepath.Join(m.opts.Memory.Dir, r.ID+".md")); err == nil {
			out = append(out, styleBold.Render(r.ID+".md"))
			out = append(out, splitLines(strings.TrimRight(string(raw), "\n"))...)
		}
	}
	out = append(out, "")
	if f, err := m.opts.Memory.Get(r.ID); err == nil {
		out = append(out, styleBold.Render("Cited files: recorded blob vs now on integration"))
		for _, p := range f.Paths {
			now := d.blobs[p.Path]
			switch {
			case d.id != r.ID:
				out = append(out, fmt.Sprintf("  %s   recorded %s   checking…", p.Path, shortBlob(p.Blob)))
			case now == "":
				out = append(out, styleErr.Render(fmt.Sprintf("  %s   recorded %s   MISSING: the path no longer exists", p.Path, shortBlob(p.Blob))))
			case now != p.Blob:
				out = append(out, styleAmber.Render(fmt.Sprintf("  %s   recorded %s   now %s   CHANGED", p.Path, shortBlob(p.Blob), shortBlob(now))))
			default:
				out = append(out, fmt.Sprintf("  %s   recorded %s   now %s   same", p.Path, shortBlob(p.Blob), shortBlob(now)))
			}
		}
		if len(f.Paths) == 0 {
			out = append(out, styleDim.Render("  (cites no file: it cannot be grounded)"))
		}
	} else {
		out = append(out, styleErr.Render("This file does not parse: "+r.Text), styleDim.Render("It is kept out of packs and left on disk. Press e to fix it."))
	}
	return out
}

func shortBlob(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func memGlyph(state string) string {
	switch state {
	case memory.StateFresh:
		return "✓"
	case memory.StateStale:
		return "⚠"
	case memory.StateInvalid:
		return "✗"
	}
	return "?"
}

func memStyle(state, s string) string {
	switch state {
	case memory.StateStale:
		return styleAmber.Render(s)
	case memory.StateInvalid:
		return styleErr.Render(s)
	case memory.StateFresh:
		return edgeGreen.Render(s)
	}
	return styleDim.Render(s)
}

// renderMemoryRows lays the list out in w columns; stale rows are amber with a ⚠ and the
// reason, invalid rows red with a ✗, so they are never mistaken for healthy ones.
func renderMemoryRows(rows []memRow, cur, w, h int) []string {
	var out []string
	for i, r := range rows {
		mark := "  "
		if i == cur {
			mark = "▸ "
		}
		path := r.Path
		if path == "" {
			path = "(no file)"
		} else if r.More > 0 {
			path += fmt.Sprintf(" +%d", r.More)
		}
		head := fmt.Sprintf("%s%s %-10s %-10s %-16s %-28s ", mark, memGlyph(r.State), r.State, r.ID, r.Kind, clipTo(path, 28))
		text := r.Text
		if r.Reason != "" {
			text = r.Reason + " — " + text
		}
		line := ansi.Truncate(head+text, w, "…")
		line = memStyle(r.State, line)
		if i == cur {
			line = styleSelected.Render(ansi.Strip(line))
		}
		out = append(out, line)
	}
	if len(out) > h && h > 0 { // keep the cursor row in view
		start := clamp(cur-h/2, 0, len(out)-h)
		out = out[start : start+h]
	}
	return out
}

func clipTo(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n+1:])
	}
	return s
}

func (m *Model) memorySummary() string {
	c := map[string]int{}
	for _, v := range m.mem.views {
		c[v.State]++
	}
	c[memory.StateInvalid] += len(m.mem.invalid)
	total := len(m.mem.views) + len(m.mem.invalid)
	s := fmt.Sprintf("%d facts: %d fresh, %d stale, %d unverified, %d invalid", total, c[memory.StateFresh], c[memory.StateStale], c[memory.StateUnverified], c[memory.StateInvalid])
	if f := memFilters[m.mem.filter]; f != "" {
		s += "   filter: " + f
	}
	return s
}

func (m *Model) memoryView() string {
	title := styleTitle.Render("Team memory")
	var body []string
	switch {
	case m.mem.err != "":
		body = []string{styleErr.Render("memory: " + m.mem.err)}
	case !m.mem.loaded:
		body = []string{styleDim.Render("loading…")}
	case m.mem.detail:
		body = m.mem.lines
	default:
		rows := m.memRows()
		if len(rows) == 0 {
			msg := "No facts yet. Agents propose facts with remember; the principal verifies them at approve."
			if memFilters[m.mem.filter] != "" {
				msg = "No " + memFilters[m.mem.filter] + " facts. Press f for another filter."
			}
			body = []string{styleDim.Render(msg)}
		} else {
			body = renderMemoryRows(rows, m.mem.cur, m.w, m.h-5)
		}
	}
	head := title
	if m.mem.loaded && !m.mem.detail {
		head += styleDim.Render("  " + m.memorySummary())
	}
	return fit(head+"\n\n"+strings.Join(body, "\n"), m.w, m.h-1) + "\n" + m.footerOverlay()
}

func (m *Model) memoryFooter() string {
	switch {
	case m.mem.confirm != "":
		return styleAmber.Render("Delete " + m.mem.confirm + "? It removes the file; y = delete, any other key = keep")
	case m.mem.detail:
		return styleDim.Render("e edit · d delete · esc back to the list")
	}
	return styleDim.Render("↑↓ move · f filter · enter detail · e edit · d delete (asks first) · esc back")
}

// ---- per-task memory: what a task was given, and what it cost ----

type delivery struct {
	FactIDs []string
	Tokens  int
	Agent   string
}

// deliveries reads the memory_used events of task: oldest first.
func deliveries(evs []store.Event, taskID string) []delivery {
	var out []delivery
	for _, e := range evs {
		if e.Kind != "memory_used" {
			continue
		}
		var d struct {
			TaskID  string   `json:"task_id"`
			Agent   string   `json:"agent"`
			FactIDs []string `json:"fact_ids"`
			Tokens  int      `json:"tokens"`
		}
		if json.Unmarshal([]byte(e.Data), &d) != nil || d.TaskID != taskID {
			continue
		}
		out = append(out, delivery{FactIDs: d.FactIDs, Tokens: d.Tokens, Agent: d.Agent})
	}
	return out
}

// memoryUsage is the "Memory" section of a task's detail: the facts of its last delivery with
// their text, and a stats line (facts delivered, pack tokens, and the task's tokens against the
// average of tasks that never got a pack, "n/a" without a baseline).
//
// all is every memory_used event (to know which tasks had a pack), tokens the per-task token
// totals, text looks a fact's text up by id.
func memoryUsage(taskID string, all []store.Event, tokens map[string]int64, text func(id string) (string, bool)) string {
	ds := deliveries(all, taskID)
	if len(ds) == 0 {
		return styleDim.Render("No memory pack was delivered for this task.") + "\n"
	}
	last := ds[len(ds)-1]
	var b strings.Builder
	b.WriteString(styleBold.Render(fmt.Sprintf("Memory used (last delivery, %d fact%s)", len(last.FactIDs), memPlural(len(last.FactIDs)))) + "\n")
	for _, id := range last.FactIDs {
		if t, ok := text(id); ok {
			fmt.Fprintf(&b, "  %s  %s\n", id, t)
		} else {
			fmt.Fprintf(&b, "  %s  %s\n", id, styleDim.Render("(no longer on disk)"))
		}
	}
	if len(last.FactIDs) == 0 {
		b.WriteString(styleDim.Render("  (the pack held only the project map)") + "\n")
	}
	b.WriteString("  " + memoryStats(taskID, ds, all, tokens) + "\n")
	return b.String()
}

func memPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// memoryStats: "facts delivered: 5 over 2 deliveries · pack tokens: 840 · task tokens 412k vs
// 530k average without a pack (-22%)", or "... vs n/a (no tasks without a pack to compare)".
func memoryStats(taskID string, ds []delivery, all []store.Event, tokens map[string]int64) string {
	distinct := map[string]bool{}
	pack := 0
	for _, d := range ds {
		pack += d.Tokens
		for _, id := range d.FactIDs {
			distinct[id] = true
		}
	}
	s := fmt.Sprintf("facts delivered: %d over %d deliver%s · pack tokens: %d", len(distinct), len(ds), map[bool]string{true: "y", false: "ies"}[len(ds) == 1], pack)
	withPack := map[string]bool{}
	for _, e := range all {
		if e.Kind != "memory_used" {
			continue
		}
		var d struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(e.Data), &d) == nil && d.TaskID != "" {
			withPack[d.TaskID] = true
		}
	}
	var sum, n int64
	for id, t := range tokens {
		if !withPack[id] && t > 0 {
			sum += t
			n++
		}
	}
	mine := tokens[taskID]
	switch {
	case mine <= 0:
		return s + " · task tokens: n/a (none recorded for this task)"
	case n == 0:
		return s + fmt.Sprintf(" · task tokens: %s vs n/a (no task without a pack to compare)", humanTokens(mine))
	}
	avg := sum / n
	delta := float64(mine-avg) / float64(avg) * 100
	return s + fmt.Sprintf(" · task tokens: %s vs %s average of %d task%s without a pack (%+.0f%%)", humanTokens(mine), humanTokens(avg), n, memPlural(int(n)), delta)
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// taskMemory is the Memory section of the task detail (empty when the TUI has no store).
func taskMemory(o Options, taskID string) string {
	if o.Store == nil {
		return ""
	}
	evs, _ := o.Store.EventsAfter(0, "memory_used", 5000)
	tokens, _ := o.Store.TaskTokens()
	text := func(id string) (string, bool) {
		if o.Memory == nil {
			return "", false
		}
		f, err := o.Memory.Get(id)
		if err != nil {
			return "", false
		}
		return f.Text, true
	}
	return memoryUsage(taskID, evs, tokens, text)
}
