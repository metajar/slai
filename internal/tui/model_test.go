package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// fakePane records pane I/O instead of calling tmux.
type fakePane struct {
	mu       sync.Mutex
	seqs     [][]tmux.Key
	pastes   []string
	lines    []string // SendKeys text
	resizes  [][2]int
	killed   []string
	snapBody string
	snapW    int
	snapH    int
	cursor   [3]int // x, y, visible
}

func (f *fakePane) SendSeq(_ context.Context, s string, k []tmux.Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seqs = append(f.seqs, append([]tmux.Key{}, k...))
	return nil
}
func (f *fakePane) Paste(_ context.Context, s, t string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pastes = append(f.pastes, t)
	return nil
}
func (f *fakePane) SendKeys(_ context.Context, s, t string, enter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf("%s|%s|%v", s, t, enter))
	return nil
}
func (f *fakePane) Snapshot(context.Context, string) (tmux.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tmux.Snapshot{Body: f.snapBody, W: f.snapW, H: f.snapH, CursorX: f.cursor[0], CursorY: f.cursor[1], CursorVisible: f.cursor[2] == 1}, nil
}
func (f *fakePane) ResizeWindow(_ context.Context, s string, w, h int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{w, h})
	return nil
}
func (f *fakePane) WindowSizeLatest(context.Context, string) error { return nil }
func (f *fakePane) BindDetachKey(context.Context, string) error    { return nil }
func (f *fakePane) KillSession(_ context.Context, n string) error {
	f.killed = append(f.killed, n)
	return nil
}

// flat renders all keys sent so far as one string, e.g. "hello Enter".
func (f *fakePane) flat() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for _, s := range f.seqs {
		for _, k := range s {
			if k.Literal {
				parts = append(parts, "'"+k.S+"'")
			} else {
				parts = append(parts, k.S)
			}
		}
	}
	return strings.Join(parts, " ")
}

func paneModel(t *testing.T) (*Model, *fakePane, *store.Store) {
	t.Helper()
	m, s := newModel(t)
	fp := &fakePane{snapBody: "line one\nline two\n", snapW: 70, snapH: 20}
	m.io = fp
	m.snd = newSender(fp)
	m.opts.SessionName = func(id string) string { return "sess_" + id }
	return m, fp, s
}

func TestTranslateKeyTable(t *testing.T) {
	r := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	alt := func(k tea.KeyMsg) tea.KeyMsg { k.Alt = true; return k }
	typ := func(k tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: k} }
	cases := []struct {
		name string
		in   tea.KeyMsg
		want string // flat form: 'lit' or Name
	}{
		{"rune", r("a"), "'a'"},
		{"unicode", r("é"), "'é'"},
		{"semicolon", r(";"), "';'"},
		{"dash", r("-"), "'-'"},
		{"space", tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, "' '"},
		{"multi rune", r("abc"), "'abc'"},
		{"enter", typ(tea.KeyEnter), "Enter"},
		{"tab", typ(tea.KeyTab), "Tab"},
		{"btab", typ(tea.KeyShiftTab), "BTab"},
		{"bspace", typ(tea.KeyBackspace), "BSpace"},
		{"escape", typ(tea.KeyEsc), "Escape"},
		{"up", typ(tea.KeyUp), "Up"},
		{"down", typ(tea.KeyDown), "Down"},
		{"left", typ(tea.KeyLeft), "Left"},
		{"right", typ(tea.KeyRight), "Right"},
		{"home", typ(tea.KeyHome), "Home"},
		{"end", typ(tea.KeyEnd), "End"},
		{"pgup", typ(tea.KeyPgUp), "PgUp"},
		{"pgdn", typ(tea.KeyPgDown), "PgDn"},
		{"delete", typ(tea.KeyDelete), "DC"},
		{"insert", typ(tea.KeyInsert), "IC"},
		{"f1", typ(tea.KeyF1), "F1"},
		{"f5", typ(tea.KeyF5), "F5"},
		{"f12", typ(tea.KeyF12), "F12"},
		{"ctrl+a", typ(tea.KeyCtrlA), "C-a"},
		{"ctrl+c", typ(tea.KeyCtrlC), "C-c"},
		{"ctrl+d", typ(tea.KeyCtrlD), "C-d"},
		{"ctrl+z", typ(tea.KeyCtrlZ), "C-z"},
		{"ctrl+l", typ(tea.KeyCtrlL), "C-l"},
		{"ctrl+up", typ(tea.KeyCtrlUp), "C-Up"},
		{"shift+left", typ(tea.KeyShiftLeft), "S-Left"},
		{"alt+x", alt(r("x")), "M-x"},
		{"alt+enter", alt(typ(tea.KeyEnter)), "M-Enter"},
		{"alt+bspace", alt(typ(tea.KeyBackspace)), "M-BSpace"},
		{"alt+up", alt(typ(tea.KeyUp)), "M-Up"},
		{"alt+ctrl+a", alt(typ(tea.KeyCtrlA)), "M-C-a"},
	}
	for _, c := range cases {
		keys, paste, ok := translateKey(c.in)
		if !ok || paste != "" {
			t.Errorf("%s: ok=%v paste=%q", c.name, ok, paste)
			continue
		}
		var parts []string
		for _, k := range keys {
			if k.Literal {
				parts = append(parts, "'"+k.S+"'")
			} else {
				parts = append(parts, k.S)
			}
		}
		if got := strings.Join(parts, " "); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// bracketed paste
	_, paste, ok := translateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\nb"), Paste: true})
	if !ok || paste != "a\nb" {
		t.Errorf("paste: %q %v", paste, ok)
	}
	if !returnKey(typ(tea.KeyCtrlQ)) || !returnKey(typ(tea.KeyCtrlBackslash)) || returnKey(typ(tea.KeyEsc)) || returnKey(typ(tea.KeyCtrlC)) {
		t.Error("return keys")
	}
}

func TestListSelectionAndFocus(t *testing.T) {
	m, fp, _ := paneModel(t)
	if m.sel != "orch" {
		t.Fatalf("default selection = %s", m.sel)
	}
	// down: orch -> pm header? (cursor walks headers and agents)
	for i := 0; i < 20 && m.sel != "eng-2"; i++ {
		m.Update(key("down"))
	}
	if m.sel != "eng-2" {
		t.Fatalf("sel = %s", m.sel)
	}
	m.Update(key("g")) // jump to next waiting
	// eng-2 is the only waiting agent and already selected
	if !strings.Contains(m.status, "nobody else") {
		t.Fatalf("status = %q", m.status)
	}
	m.Update(key("up"))
	m.Update(key("g"))
	if m.sel != "eng-2" {
		t.Fatalf("g -> %s", m.sel)
	}

	// enter focuses; typing is forwarded in order; ctrl+q returns
	_, _ = m.Update(key("enter"))
	if m.mode != mFocus {
		t.Fatalf("not focused: status=%q cur=%d item=%+v", m.status, m.cur, m.items[m.cur])
	}
	v := m.View()
	for _, w := range []string{"FOCUSED: eng-2", "ctrl+q"} {
		if !strings.Contains(ansi.Strip(v), w) {
			t.Errorf("focus view missing %q", w)
		}
	}
	for _, r := range "hi" {
		m.Update(key(string(r)))
	}
	m.Update(key("enter"))
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(key("q")) // q is just a letter in focus mode
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m.snd.wait()
	if got := fp.flat(); got != "'h' 'i' Enter Escape 'q' C-c" {
		t.Fatalf("forwarded = %q", got)
	}
	if m.mode != mFocus {
		t.Fatal("ctrl+c/esc must not leave focus")
	}
	// paste goes through Paste
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l1\nl2"), Paste: true})
	m.snd.wait()
	if len(fp.pastes) != 1 || fp.pastes[0] != "l1\nl2" {
		t.Fatalf("pastes = %q", fp.pastes)
	}
	m.Update(key("ctrl+q"))
	if m.mode != mList {
		t.Fatal("ctrl+q should return to list")
	}
	// in list mode keys are NOT forwarded
	m.Update(key("x"))
	m.snd.wait()
	if strings.Contains(fp.flat(), "'x'") {
		t.Fatal("list-mode key leaked to the pane")
	}
}

func TestFocusResizesWindowToPreview(t *testing.T) {
	m, fp, _ := paneModel(t)
	pw, ph := m.previewSize()
	cmd := m.resizeCmd("orch", pw, ph)
	cmd()
	if len(fp.resizes) != 1 || fp.resizes[0] != [2]int{pw, ph} {
		t.Fatalf("resizes = %v want %dx%d", fp.resizes, pw, ph)
	}
	// preview content is exactly pw x ph
	m.snapFor, m.snap = "orch", tmux.Snapshot{Body: strings.Repeat("x", 500) + "\nshort\n", W: pw, H: ph}
	m.sel = "orch"
	rows := previewRows(m.snap.Body, pw, ph)
	if len(rows) != ph {
		t.Fatalf("rows %d != %d", len(rows), ph)
	}
	for i, r := range rows {
		if w := ansi.StringWidth(r); w != pw {
			t.Fatalf("row %d width %d != %d", i, w, pw)
		}
	}
	// mismatching snapshot triggers a resize command
	_, c := m.Update(previewMsg{agent: "orch", snap: tmux.Snapshot{W: 10, H: 5}})
	if c == nil {
		t.Fatal("expected self-heal resize")
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	npw, nph := m.previewSize()
	if npw == pw || nph == ph {
		t.Fatalf("size should change: %d,%d", npw, nph)
	}
	// the whole view stays within the terminal
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != 40 {
		t.Fatalf("view has %d lines, want 40", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 140 {
			t.Fatalf("line %d width %d > 140", i, w)
		}
	}
}

func TestDrawCursor(t *testing.T) {
	rows := previewRows("\x1b[31mred\x1b[0m text\n", 12, 2)
	out := drawCursor(rows, 1, 0, 12)
	if !strings.Contains(out[0], "\x1b[0;7me\x1b[0m") {
		t.Fatalf("cursor cell: %q", out[0])
	}
	if ansi.Strip(out[0]) != ansi.Strip(rows[0]) {
		t.Fatalf("cursor changed text: %q vs %q", ansi.Strip(out[0]), ansi.Strip(rows[0]))
	}
	if ansi.StringWidth(out[0]) != 12 {
		t.Fatalf("width %d", ansi.StringWidth(out[0]))
	}
	// out of range is a no-op
	_ = drawCursor(rows, 99, 99, 12)
}

func TestQuickPrompt(t *testing.T) {
	m, fp, s := paneModel(t)
	// eng-2 is waiting (permission), mark it waiting_on_user
	_ = s.SetAgentStatus("eng-2", "waiting_on_user")
	m.Update(m.refreshCmd()())
	m.Update(key("g"))
	if m.sel != "eng-2" {
		t.Fatalf("sel = %s", m.sel)
	}
	m.Update(key("space"))
	if m.mode != mQuick {
		t.Fatal("quick prompt not open")
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "to eng-2:") {
		t.Fatalf("popup missing:\n%s", v)
	}
	for _, r := range "yes do it" {
		m.Update(key(string(r)))
	}
	_, cmd := m.Update(key("enter"))
	if m.mode != mList || cmd == nil {
		t.Fatal("enter should close + send")
	}
	msg := cmd()
	if am, ok := msg.(actionMsg); !ok || am.err != nil || !strings.Contains(am.text, "queued") {
		t.Fatalf("%+v", msg)
	}
	if len(fp.lines) != 0 {
		t.Fatalf("quick prompt bypassed supervisor safety checks: %v", fp.lines)
	}
	if m.mode == mFocus {
		t.Fatal("quick prompt must not focus")
	}
	if un, _ := s.UndeliveredFor("eng-2"); len(un) != 1 || un[0].Kind != "user" || un[0].Body != "yes do it" {
		t.Fatalf("quick prompt should queue a user message: %+v", un)
	}
	// esc cancels
	m.Update(key("space"))
	m.Update(key("z"))
	m.Update(key("esc"))
	if m.mode != mList || len(fp.lines) != 0 {
		t.Fatal("esc should cancel")
	}
}

func TestQuickPromptWithoutStoreDoesNotType(t *testing.T) {
	m, fp, _ := paneModel(t)
	m.opts.Store = nil
	msg := m.sendCmd("eng-2", "hello", false)()
	if am, ok := msg.(actionMsg); !ok || am.err == nil {
		t.Fatalf("missing store should fail rather than send unrecorded input: %+v", msg)
	}
	if len(fp.lines) != 0 {
		t.Fatalf("typed without persisting: %v", fp.lines)
	}
}

func TestOverlays(t *testing.T) {
	m, _, _ := paneModel(t)
	m.Update(key("b"))
	if m.ov != ovTasks || !strings.Contains(ansi.Strip(m.View()), "Do it") {
		t.Fatalf("tasks overlay: %v\n%s", m.ov, m.View())
	}
	if strings.Contains(ansi.Strip(m.View()), "AGENTS") {
		t.Fatal("tasks view should be full screen")
	}
	// enter -> detail; esc returns to tasks; esc returns to list
	_, cmd := m.Update(key("enter"))
	m.Update(cmd())
	if m.ov != ovDetail || !strings.Contains(m.vp.View(), "T-0001") {
		t.Fatalf("detail: %v", m.ov)
	}
	m.Update(key("esc"))
	if m.ov != ovTasks {
		t.Fatalf("esc from detail -> %v", m.ov)
	}
	m.Update(key("d")) // not in review
	if !m.statusE {
		t.Fatal("d on non-review task should error")
	}
	m.Update(key("esc"))
	if m.ov != ovNone {
		t.Fatal("esc closes tasks")
	}
	m.Update(key("l"))
	if m.ov != ovLog || !strings.Contains(ansi.Strip(m.View()), "eng-1 → principal: request_review T-0001") {
		t.Fatalf("log overlay:\n%s", m.View())
	}
	m.Update(key("esc"))
	if m.ov != ovNone || !strings.Contains(ansi.Strip(m.View()), "AGENTS") {
		t.Fatal("esc closes log")
	}
	m.Update(key("?"))
	if m.ov != ovHelp || !strings.Contains(m.View(), "ctrl+q") {
		t.Fatal("help")
	}
	m.Update(key("x"))
	if m.ov != ovNone || m.mode != mList {
		t.Fatal("any key closes help")
	}
	// t on selected agent's task
	m.Update(key("down"))
	_, cmd = m.Update(key("t"))
	if cmd == nil && !m.statusE {
		t.Fatal("t gave neither a detail nor an error")
	}
}

func TestMainViewLayout(t *testing.T) {
	m, _, _ := paneModel(t)
	v := ansi.Strip(m.View())
	for _, want := range []string{"slai", "demo", "4 agents", "AGENTS", "Orchestrator", "Engineers", "needs you", "enter/→", "quick prompt"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "BOARD") || strings.Contains(v, "to orch") {
		t.Error("old board/prompt bar must be gone")
	}
	for _, sz := range [][2]int{{50, 12}, {20, 6}, {200, 60}, {30, 8}, {59, 20}, {60, 20}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		out := m.View()
		if sz[0] >= 20 && sz[1] >= 6 {
			if n := len(strings.Split(out, "\n")); n != sz[1] {
				t.Errorf("%v: %d lines", sz, n)
			}
		}
	}
	// narrow terminal: focus shows only the preview
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	m.Update(key("enter"))
	if m.mode != mFocus || strings.Contains(ansi.Strip(m.View()), "AGENTS") {
		t.Fatal("narrow focus")
	}
}

func TestEmptyStateHint(t *testing.T) {
	m := New(Options{Project: "p", Team: testTeam(), Pane: &fakePane{snapBody: "banner\n", snapW: 70, snapH: 20}})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	pw, ph := m.previewSize()
	m.Update(previewMsg{agent: "orch", snap: tmux.Snapshot{Body: "banner\n", W: pw, H: ph}})
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "press enter to talk to the orchestrator") {
		t.Fatalf("no hint:\n%s", v)
	}
}

func TestFoldAndMouse(t *testing.T) {
	m, _, _ := paneModel(t)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	// move cursor to the Engineers header and fold with left/enter
	for i := 0; i < 20 && !(m.items[m.cur].isHeader() && m.items[m.cur].group == "engineers"); i++ {
		m.Update(key("down"))
	}
	m.Update(key("enter"))
	if !m.folded["engineers"] {
		t.Fatal("enter on header should fold")
	}
	m.Update(key("right"))
	if m.folded["engineers"] {
		t.Fatal("right on header should unfold")
	}
	// click on the first agent row selects it; click on preview focuses
	l := m.layout()
	m.Update(key("g")) // somewhere else
	y := l.top + 1 + 1 // first agent line (after Orchestrator header)
	m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.sel != "orch" {
		t.Fatalf("click selected %s", m.sel)
	}
	m.Update(tea.MouseMsg{X: l.lw + 5, Y: l.top + 3, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m.Update(tea.MouseMsg{X: l.lw + 5, Y: l.top + 3, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if m.mode != mFocus {
		t.Fatal("click on preview should focus")
	}
	m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.mode != mList {
		t.Fatal("click on list leaves focus")
	}
}

func TestConfirmStopAndRestart(t *testing.T) {
	var stopped, restarted []string
	m, _, _ := paneModel(t)
	m.opts.Stop = func(_ context.Context, id string) error { stopped = append(stopped, id); return nil }
	m.opts.Restart = func(_ context.Context, id string) error { restarted = append(restarted, id); return nil }
	m.Update(key("x"))
	if m.mode != mConfirm || !strings.Contains(ansi.Strip(m.View()), "Stop orch?") {
		t.Fatal("x should ask")
	}
	m.Update(key("n"))
	if m.mode != mList || len(stopped) != 0 {
		t.Fatal("n cancels")
	}
	m.Update(key("x"))
	_, cmd := m.Update(key("y"))
	cmd()
	if len(stopped) != 1 || stopped[0] != "orch" {
		t.Fatalf("stopped %v", stopped)
	}
	m.Update(key("R"))
	_, cmd = m.Update(key("y"))
	cmd()
	if len(restarted) != 1 {
		t.Fatalf("restarted %v", restarted)
	}
}

func TestReviveAndPause(t *testing.T) {
	var revived string
	paused := false
	m := New(Options{Project: "p", Team: testTeam(),
		Revive:    func(_ context.Context, id string) error { revived = id; return nil },
		SetPaused: func(b bool) error { paused = b; return nil },
		IsPaused:  func() bool { return paused }})
	// all agents "dead" (not in store): enter refuses to focus, v revives
	m.Update(key("enter"))
	if m.mode != mList || !m.statusE {
		t.Fatal("dead agent must not focus")
	}
	_, cmd := m.Update(key("v"))
	cmd()
	if revived != "orch" {
		t.Fatal(revived)
	}
	_, cmd = m.Update(key("p"))
	cmd()
	if !paused {
		t.Fatal("pause")
	}
	_ = m.View()
}

func TestWarnBannerAndSupervisorNote(t *testing.T) {
	m := New(Options{Project: "p", Team: testTeam(), Supervisor: "daemon"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.Update(refreshMsg{warn: "Claude Code will ask “trust this folder?” in each agent — focus an agent and answer it once."})
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "trust this folder") || !strings.Contains(v, "supervisor: daemon") {
		t.Fatalf("banner/header:\n%s", v)
	}
	m.Update(refreshMsg{})
	if strings.Contains(ansi.Strip(m.View()), "trust this folder") {
		t.Fatal("banner should clear")
	}
}

func TestQuit(t *testing.T) {
	m, _, _ := paneModel(t)
	if _, cmd := m.Update(key("q")); cmd == nil {
		t.Fatal("q should quit")
	}
}

var _ = store.StatusMerged

func newModel(t *testing.T) (*Model, *store.Store) {
	s := openStore(t)
	for _, a := range testTeam().Agents {
		_ = s.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, Status: "idle"})
	}
	_ = s.SetAgentStatus("eng-2", "waiting")
	_ = s.SetAgentStatus("eng-1", "finished")
	_, _ = s.CreateTask(store.Task{Title: "Do it", Assignee: "eng-1", CreatedBy: "orch", Branch: "b"})
	_ = s.AddEvent("eng-1", "request_review", `{"to":"principal","task_id":"T-0001"}`)
	m := New(Options{Project: "demo", Dir: t.TempDir(), Team: testTeam(), Store: s})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m.Update(m.refreshCmd()())
	return m, s
}

func TestFocusClearsFinishedDot(t *testing.T) {
	m, fp, s := paneModel(t)
	_ = fp
	m.sel = "eng-1"
	m.cursorTo("eng-1")
	m.mode = mFocus
	_ = s.SetAgentStatus("eng-1", "finished")
	_, cmd := m.Update(m.refreshCmd()())
	if a, _ := m.selAgent(); a.Status != "idle" {
		t.Fatalf("status = %s", a.Status)
	}
	if cmd == nil {
		t.Fatal("expected a clear command")
	}
	cmd()
	if as, _ := s.GetAgents(); func() string {
		for _, a := range as {
			if a.ID == "eng-1" {
				return a.Status
			}
		}
		return ""
	}() != "idle" {
		t.Fatal("store still says finished")
	}
}

func TestResumeKeyClearsHeldAgent(t *testing.T) {
	m, s := newModel(t)
	var resumed string
	m.opts.ResumeAgent = func(id string) error { resumed = id; return s.SetAgentPaused(id, false) }
	_ = s.SetAgentPaused("eng-1", true)
	m.Update(m.refreshCmd()())
	if !m.heldAgents["eng-1"] {
		t.Fatal("held agent not tracked")
	}
	m.setSel("eng-1")
	var shown bool
	for _, l := range renderList(m.items, m.cur, 40, m.agents, m.tasks, m.events, true) {
		if strings.Contains(ansi.Strip(l.text), "paused") {
			shown = true
		}
	}
	if !shown {
		t.Error("paused marker not rendered")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	if cmd == nil {
		t.Fatal("u should resume a held agent")
	}
	cmd()
	if resumed != "eng-1" {
		t.Fatalf("resumed %q", resumed)
	}
	if p, _ := s.PausedAgents(); p["eng-1"] {
		t.Error("still paused in store")
	}
}

func TestPreviewFooterNotesMachineTrafficForNonOrch(t *testing.T) {
	m := New(Options{Project: "p", Team: testTeam(), Pane: &fakePane{snapBody: "x\n", snapW: 70, snapH: 20}})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	pw, ph := m.previewSize()
	note := "driven by orch; you rarely need to type here"
	show := func(agent string) string {
		m.sel = agent
		m.Update(previewMsg{agent: agent, snap: tmux.Snapshot{Body: "line\n", W: pw, H: ph}})
		return ansi.Strip(m.View())
	}
	if v := show("eng-1"); !strings.Contains(v, note) {
		t.Fatalf("no machine-traffic note on a non-orch agent:\n%s", v)
	}
	if v := show("orch"); strings.Contains(v, note) {
		t.Fatalf("orchestrator preview must not carry the note:\n%s", v)
	}
}
