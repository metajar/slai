package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/tmux"
)

var tgeom = mouseGeom{ListOn: true, ListW: 20, PrevOn: true, PrevX0: 20, PrevW: 40, Top: 1, BodyH: 12}

func mev(x, y int, a tea.MouseAction, b tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: a, Button: b}
}

func TestMouseDispatchTable(t *testing.T) {
	t0 := time.Unix(1000, 0)
	press := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionPress, tea.MouseButtonLeft) }
	move := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionMotion, tea.MouseButtonLeft) }
	rel := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionRelease, tea.MouseButtonLeft) }
	type step struct {
		ev   tea.MouseMsg
		dt   time.Duration
		act  mouseKind
		a, b cell
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"wheel up list", []step{{ev: mev(5, 4, tea.MouseActionPress, tea.MouseButtonWheelUp), act: mkListWheel}}},
		{"wheel down preview", []step{{ev: mev(30, 4, tea.MouseActionPress, tea.MouseButtonWheelDown), act: mkPreviewWheel}}},
		{"wheel preview border", []step{{ev: mev(20, 4, tea.MouseActionPress, tea.MouseButtonWheelDown), act: mkPreviewWheel}}},
		{"wheel outside", []step{{ev: mev(30, 30, tea.MouseActionPress, tea.MouseButtonWheelDown), act: mkNone}}},
		{"list click", []step{{ev: press(3, 3), act: mkListClick}}},
		{"preview border click focuses", []step{{ev: press(21, 1), act: mkPreviewFocus}}},
		{"click = press+release same cell", []step{
			{ev: press(25, 5), act: mkSelBegin},
			{ev: rel(25, 5), dt: 10 * time.Millisecond, act: mkPreviewFocus}}},
		{"drag selects and copies", []step{
			{ev: press(25, 5), act: mkSelBegin},
			{ev: move(27, 5), dt: time.Millisecond, act: mkSelUpdate, a: cell{5, 3}, b: cell{7, 3}},
			{ev: rel(27, 5), dt: time.Millisecond, act: mkSelCopy, a: cell{5, 3}, b: cell{7, 3}}}},
		{"drag left of pane clamps to column 0", []step{
			{ev: press(25, 5), act: mkSelBegin},
			{ev: move(2, 5), act: mkSelUpdate, a: cell{5, 3}, b: cell{0, 3}},
			{ev: rel(2, 5), act: mkSelCopy, a: cell{5, 3}, b: cell{0, 3}}}},
		{"drag past bottom right clamps inside the pane", []step{
			{ev: press(25, 5), act: mkSelBegin},
			{ev: move(200, 99), act: mkSelUpdate, a: cell{5, 3}, b: cell{39, 9}},
			{ev: rel(200, 99), act: mkSelCopy, a: cell{5, 3}, b: cell{39, 9}}}},
		{"drag above the pane clamps to row 0", []step{
			{ev: press(25, 5), act: mkSelBegin},
			{ev: move(25, 0), act: mkSelUpdate, a: cell{5, 3}, b: cell{5, 0}}}},
		{"motion without press ignored", []step{{ev: move(25, 5), act: mkNone}}},
		{"double click", []step{
			{ev: press(25, 5), act: mkSelBegin}, {ev: rel(25, 5), act: mkPreviewFocus},
			{ev: press(25, 5), dt: 100 * time.Millisecond, act: mkSelBegin},
			{ev: rel(25, 5), act: mkSelWord, a: cell{5, 3}}}},
		{"triple click", []step{
			{ev: press(25, 5), act: mkSelBegin}, {ev: rel(25, 5), act: mkPreviewFocus},
			{ev: press(25, 5), dt: 100 * time.Millisecond, act: mkSelBegin}, {ev: rel(25, 5), act: mkSelWord, a: cell{5, 3}},
			{ev: press(25, 5), dt: 100 * time.Millisecond, act: mkSelBegin},
			{ev: rel(25, 5), act: mkSelLine, a: cell{5, 3}}}},
		{"slow second click is a fresh click", []step{
			{ev: press(25, 5), act: mkSelBegin}, {ev: rel(25, 5), act: mkPreviewFocus},
			{ev: press(25, 5), dt: time.Second, act: mkSelBegin}, {ev: rel(25, 5), act: mkPreviewFocus}}},
		{"second click on another cell is fresh", []step{
			{ev: press(25, 5), act: mkSelBegin}, {ev: rel(25, 5), act: mkPreviewFocus},
			{ev: press(26, 5), dt: 50 * time.Millisecond, act: mkSelBegin}, {ev: rel(26, 5), act: mkPreviewFocus}}},
		{"right button ignored", []step{{ev: mev(25, 5, tea.MouseActionPress, tea.MouseButtonRight), act: mkNone}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var st mouseState
			now := t0
			for i, s := range c.steps {
				now = now.Add(s.dt)
				var act mouseAct
				st, act = mouseDispatch(s.ev, tgeom, st, now)
				if act.Kind != s.act {
					t.Fatalf("step %d: kind = %d, want %d (%+v)", i, act.Kind, s.act, act)
				}
				if s.act == mkSelUpdate || s.act == mkSelCopy {
					if act.A != s.a || act.B != s.b {
						t.Fatalf("step %d: range = %v..%v, want %v..%v", i, act.A, act.B, s.a, s.b)
					}
				}
				if (s.act == mkSelWord || s.act == mkSelLine) && act.A != s.a {
					t.Fatalf("step %d: at %v, want %v", i, act.A, s.a)
				}
			}
		})
	}
	// wheel direction: up is negative
	_, a := mouseDispatch(mev(30, 4, tea.MouseActionPress, tea.MouseButtonWheelUp), tgeom, mouseState{}, t0)
	if a.Delta != -wheelStep {
		t.Fatalf("wheel up delta = %d", a.Delta)
	}
}

func TestExtractSelection(t *testing.T) {
	rows := []string{
		"\x1b[31mhello world\x1b[0m   ",
		"second line    ",
		"third          ",
	}
	const w = 15
	cases := []struct {
		name string
		a, b cell
		want string
	}{
		{"single line", cell{6, 0}, cell{10, 0}, "world"},
		{"reversed", cell{10, 0}, cell{6, 0}, "world"},
		{"multi line stream", cell{6, 0}, cell{5, 2}, "world\nsecond line\nthird"},
		{"to end of first line only trims", cell{0, 0}, cell{14, 0}, "hello world"},
		{"blank tail dropped", cell{0, 1}, cell{14, 2}, "second line\nthird"},
		{"rows past the end ignored", cell{0, 2}, cell{3, 9}, "third"},
		{"last row cut at cursor", cell{0, 1}, cell{3, 2}, "second line\nthir"},
	}
	for _, c := range cases {
		if got := extractSelection(rows, c.a, c.b, w); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// wide runes occupy two cells; a selection touching either half takes the glyph
	wide := []string{"a世界b  "}
	if got := extractSelection(wide, cell{1, 0}, cell{1, 0}, 7); got != "世" {
		t.Errorf("first half: %q", got)
	}
	if got := extractSelection(wide, cell{2, 0}, cell{3, 0}, 7); got != "世界" {
		t.Errorf("second half + next: %q", got)
	}
	if got := extractSelection(wide, cell{0, 0}, cell{5, 0}, 7); got != "a世界b" {
		t.Errorf("all: %q", got)
	}
	// combining marks stay attached to their base
	comb := []string{"café ok  "}
	if got := extractSelection(comb, cell{0, 0}, cell{3, 0}, 9); got != "café" {
		t.Errorf("combining: %q", got)
	}
	if got := extractSelection(comb, cell{5, 0}, cell{6, 0}, 9); got != "ok" {
		t.Errorf("after combining: %q", got)
	}
}

func TestWordBounds(t *testing.T) {
	row := "run  ./cmd/slai --flag 世界x  "
	c0, c1, ok := wordBounds(row, 7)
	if !ok || sliceCells(row, c0, c1+1) != "./cmd/slai" {
		t.Fatalf("path word: %d %d %v", c0, c1, ok)
	}
	if _, _, ok := wordBounds(row, 3); ok {
		t.Fatal("space is not a word")
	}
	c0, c1, ok = wordBounds(row, 24) // inside the wide run
	if !ok || sliceCells(row, c0, c1+1) != "世界x" {
		t.Fatalf("wide word: %q", sliceCells(row, c0, c1+1))
	}
}

func TestHighlightKeepsOtherColours(t *testing.T) {
	rows := []string{"\x1b[31mred text\x1b[0m", "\x1b[32mgreen\x1b[0m"}
	out := highlightRows(rows, cell{4, 0}, cell{6, 0}, 10)
	if !strings.Contains(out[0], "\x1b[7mtex\x1b[0m") {
		t.Fatalf("no reverse video: %q", out[0])
	}
	if out[1] != rows[1] {
		t.Fatalf("untouched row changed: %q", out[1])
	}
	if got := ansi.Strip(out[0]); got != "red text  " {
		t.Fatalf("text altered: %q", got)
	}
}

func TestScrollMath(t *testing.T) {
	cases := []struct{ off, d, hist, want int }{
		{0, 3, 100, 3}, {0, -3, 100, 0}, {98, 3, 100, 100}, {5, 3, 0, 0}, {2, -3, 100, 0}, {10, 3, 100, 13},
	}
	for _, c := range cases {
		if got := scrollBy(c.off, c.d, c.hist); got != c.want {
			t.Errorf("scrollBy(%d,%d,%d) = %d want %d", c.off, c.d, c.hist, got, c.want)
		}
	}
	if followHistory(0, 5, 100) != 0 {
		t.Error("live view must not follow")
	}
	if followHistory(10, 5, 100) != 15 || followHistory(10, 5, 12) != 12 || followHistory(10, 0, 100) != 10 {
		t.Error("followHistory")
	}
	if n := scrollNote(120); !strings.Contains(n, "↑ 120 lines back") || !strings.Contains(scrollNote(1), "1 line back") {
		t.Errorf("note %q", n)
	}
}

func TestOSC52(t *testing.T) {
	if got := osc52("hi", false); got != "\x1b]52;c;aGk=\x07" {
		t.Fatalf("%q", got)
	}
	want := "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\"
	if got := osc52("hi", true); got != want {
		t.Fatalf("tmux wrap %q want %q", got, want)
	}
}

func fakeClip(env map[string]string, have map[string]bool, failing map[string]bool) (*Clipboard, *bytes.Buffer, *[]string) {
	var out bytes.Buffer
	var ran []string
	return &Clipboard{
		LookPath: func(n string) (string, error) {
			if have[n] {
				return "/bin/" + n, nil
			}
			return "", errors.New("not found")
		},
		Run: func(_ context.Context, name string, args []string, stdin string) error {
			ran = append(ran, name+" "+strings.Join(args, " ")+"|"+stdin)
			if failing[name] {
				return errors.New("boom")
			}
			return nil
		},
		Getenv: func(k string) string { return env[k] },
		Out:    &out,
		GOOS:   "linux",
	}, &out, &ran
}

func TestClipboardFallbacks(t *testing.T) {
	// X11 with xclip: helper used AND OSC 52 emitted
	c, out, ran := fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true, "xsel": true}, nil)
	how, err := c.Copy("abc")
	if err != nil || how != "xclip+osc52" || len(*ran) != 1 || (*ran)[0] != "xclip -selection clipboard|abc" || out.String() != osc52("abc", false) {
		t.Fatalf("xclip: %q %v %v %q", how, err, *ran, out.String())
	}
	// wl-copy preferred on wayland
	c, _, ran = fakeClip(map[string]string{"WAYLAND_DISPLAY": "w", "DISPLAY": ":0"}, map[string]bool{"wl-copy": true, "xclip": true}, nil)
	if how, _ := c.Copy("x"); how != "wl-copy+osc52" || len(*ran) != 1 {
		t.Fatalf("wl-copy: %q %v", how, *ran)
	}
	// failing first helper falls through to the next
	c, _, ran = fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true, "xsel": true}, map[string]bool{"xclip": true})
	if how, _ := c.Copy("x"); how != "xsel+osc52" || len(*ran) != 2 {
		t.Fatalf("xsel fallback: %q %v", how, *ran)
	}
	// SSH / headless: no display -> no helper runs, OSC 52 only (tmux-wrapped under $TMUX)
	c, out, ran = fakeClip(map[string]string{"TMUX": "/tmp/x,1,0"}, map[string]bool{"xclip": true}, nil)
	how, err = c.Copy("héllo")
	if err != nil || how != "osc52" || len(*ran) != 0 || out.String() != osc52("héllo", true) {
		t.Fatalf("headless: %q %v %v %q", how, err, *ran, out.String())
	}
	// macOS
	c, _, _ = fakeClip(nil, map[string]bool{"pbcopy": true}, nil)
	c.GOOS = "darwin"
	if how, _ := c.Copy("x"); how != "pbcopy+osc52" {
		t.Fatalf("pbcopy: %q", how)
	}
	// nothing works
	c, _, _ = fakeClip(nil, nil, nil)
	c.Out = nil
	if _, err := c.Copy("x"); err == nil {
		t.Fatal("expected failure")
	}
	// oversized text skips OSC 52 but a helper still works
	c, out, _ = fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true}, nil)
	if how, err := c.Copy(strings.Repeat("a", 100000)); err != nil || how != "xclip" || out.Len() != 0 {
		t.Fatalf("big: %q %v", how, err)
	}
}

// scrollPane is a fake tmux pane with a long history: line N of `total`.
type scrollPane struct {
	fakePane
	total int // lines of output ever produced
	h, w  int
}

func (p *scrollPane) hist() int { return max(p.total-p.h, 0) }

func (p *scrollPane) SnapshotAt(_ context.Context, _ string, off int) (tmux.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	off = min(max(off, 0), p.hist())
	var sb strings.Builder
	last := p.total - off // exclusive
	for i := last - p.h; i < last; i++ {
		fmt.Fprintf(&sb, "line %03d\n", i+1)
	}
	return tmux.Snapshot{Body: sb.String(), W: p.w, H: p.h, HistorySize: p.hist(), Offset: off}, nil
}

// pump runs a cmd (and the messages it yields) like the bubbletea runtime would.
func pump(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			pump(m, c)
		}
	default:
		_, next := m.Update(msg)
		pump(m, next)
	}
}

func scrollModel(t *testing.T) (*Model, *scrollPane, *bytes.Buffer) {
	t.Helper()
	m, _, _ := paneModel(t)
	pw, ph := m.previewSize()
	sp := &scrollPane{total: 300, h: ph, w: pw}
	sp.snapW, sp.snapH = pw, ph
	m.io = sp
	m.snd = newSender(sp)
	var out bytes.Buffer
	c, o, _ := fakeClip(nil, nil, nil)
	_ = o
	c.Out = &out
	m.clip = c
	m.previewBusy = false
	pump(m, m.previewKick())
	return m, sp, &out
}

func wheelAt(m *Model, inPreview bool, up bool) tea.Cmd {
	g := m.geom()
	x := 3
	if inPreview {
		x = g.PrevX0 + 5
	}
	b := tea.MouseButtonWheelDown
	if up {
		b = tea.MouseButtonWheelUp
	}
	_, c := m.Update(mev(x, g.Top+3, tea.MouseActionPress, b))
	return c
}

func TestWheelScrollsPreviewHistory(t *testing.T) {
	m, sp, _ := scrollModel(t)
	if !strings.Contains(ansi.Strip(m.View()), "line 300") {
		t.Fatalf("not at bottom:\n%s", ansi.Strip(m.View()))
	}
	pump(m, wheelAt(m, true, true))
	if m.scroll["orch"] != 3 {
		t.Fatalf("offset = %d", m.scroll["orch"])
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "↑ 3 lines back") || strings.Contains(v, "line 300") || !strings.Contains(v, "line 297") {
		t.Fatalf("view after wheel up:\n%s", v)
	}
	pump(m, wheelAt(m, true, true))
	pump(m, wheelAt(m, true, false))
	if m.scroll["orch"] != 3 {
		t.Fatalf("offset after up,up,down = %d", m.scroll["orch"])
	}
	for i := 0; i < 5; i++ {
		pump(m, wheelAt(m, true, false))
	}
	if m.scroll["orch"] != 0 || strings.Contains(ansi.Strip(m.View()), "lines back") {
		t.Fatalf("clamp at bottom failed: %d", m.scroll["orch"])
	}
	for i := 0; i < 200; i++ {
		pump(m, wheelAt(m, true, true))
	}
	if m.scroll["orch"] != sp.hist() {
		t.Fatalf("clamp at top: %d want %d", m.scroll["orch"], sp.hist())
	}
	if !strings.Contains(ansi.Strip(m.View()), "line 001") {
		t.Fatal("oldest line not visible at top")
	}
	// the wheel is never forwarded to the agent, in list or focus mode
	pump(m, m.enterFocus())
	pump(m, wheelAt(m, true, true))
	if m.mode != mFocus {
		t.Fatal("wheel left focus mode")
	}
	m.snd.wait()
	if got := sp.flat(); got != "" {
		t.Fatalf("wheel reached the agent: %q", got)
	}
	// wheel over the list still scrolls the list, not the preview
	before := m.scroll["orch"]
	wheelAt(m, false, false)
	if m.scroll["orch"] != before {
		t.Fatal("list wheel changed preview offset")
	}
}

func TestScrolledKeysAndHold(t *testing.T) {
	m, sp, _ := scrollModel(t)
	pump(m, m.enterFocus())
	pump(m, wheelAt(m, true, true))
	_, ph := m.previewSize()
	// PgUp / PgDn / Home / End move the view instead of reaching the agent
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	pump(m, c)
	if m.scroll["orch"] != 3+ph/2 {
		t.Fatalf("pgup offset %d", m.scroll["orch"])
	}
	_, c = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	pump(m, c)
	if m.scroll["orch"] != sp.hist() {
		t.Fatalf("home offset %d", m.scroll["orch"])
	}
	_, c = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	pump(m, c)
	if m.scroll["orch"] != sp.hist()-ph/2 {
		t.Fatalf("pgdn offset %d", m.scroll["orch"])
	}
	// hold position while output arrives
	before := m.scroll["orch"]
	top := firstRow(m)
	sp.total += 7
	pump(m, m.previewKick())
	if m.scroll["orch"] != before+7 || firstRow(m) != top {
		t.Fatalf("view drifted: offset %d -> %d, first row %q -> %q", before, m.scroll["orch"], top, firstRow(m))
	}
	_, c = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	pump(m, c)
	if m.scroll["orch"] != 0 {
		t.Fatalf("end offset %d", m.scroll["orch"])
	}
	m.snd.wait()
	if got := sp.flat(); got != "" {
		t.Fatalf("paging keys leaked to agent: %q", got)
	}
	// not scrolled: paging keys are forwarded as before
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m.snd.wait()
	if got := sp.flat(); got != "PgUp" {
		t.Fatalf("forwarded %q", got)
	}
	// typing while scrolled snaps back to the bottom and is forwarded
	pump(m, wheelAt(m, true, true))
	if m.scroll["orch"] == 0 {
		t.Fatal("not scrolled")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.scroll["orch"] != 0 {
		t.Fatal("typing did not return to bottom")
	}
	m.snd.wait()
	if got := sp.flat(); got != "PgUp 'x'" {
		t.Fatalf("forwarded %q", got)
	}
}

func firstRow(m *Model) string {
	rows := m.plainRows()
	if len(rows) == 0 {
		return ""
	}
	return strings.TrimSpace(rows[0])
}

func TestDragSelectCopiesOnlyPaneText(t *testing.T) {
	m, _, out := scrollModel(t)
	var copied []string
	c, o, _ := fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true}, nil)
	c.Run = func(_ context.Context, _ string, _ []string, stdin string) error {
		copied = append(copied, stdin)
		return nil
	}
	c.Out = out
	_ = o
	m.clip = c
	g := m.geom()
	// drag from row 2 col 4 to row 4 col 9 (inner coords), pointer wanders left over the list
	x0, y0 := g.PrevX0+4, g.Top+1+2
	m.Update(mev(x0, y0, tea.MouseActionPress, tea.MouseButtonLeft))
	m.Update(mev(g.PrevX0+9, g.Top+1+4, tea.MouseActionMotion, tea.MouseButtonLeft))
	if !m.hl.active {
		t.Fatal("no highlight during drag")
	}
	if v := m.View(); !strings.Contains(v, "\x1b[7m") {
		t.Fatal("no reverse video in view")
	}
	_, cmd := m.Update(mev(1, g.Top+1+4, tea.MouseActionRelease, tea.MouseButtonLeft)) // released over the list
	if m.mode != mList {
		t.Fatal("drag must not focus")
	}
	pump(m, cmd)
	if len(copied) != 1 {
		t.Fatalf("copied %v", copied)
	}
	got := copied[0]
	// the release happened over the list: the end clamps to column 0 of the last row
	rows := m.plainRows()
	exp := strings.TrimRight(rows[2][4:], " ") + "\n" + strings.TrimRight(rows[3], " ") + "\n" + strings.TrimRight(rows[4][:1], " ")
	if got != exp {
		t.Fatalf("copied %q want %q", got, exp)
	}
	for _, bad := range []string{"AGENTS", "orch", "│", "╭"} {
		if strings.Contains(got, bad) {
			t.Fatalf("copied text leaked %q: %q", bad, got)
		}
	}
	if !strings.HasPrefix(out.String(), "\x1b]52;c;") {
		t.Fatalf("no OSC 52: %q", out.String())
	}
	if m.status != "copied 3 lines" {
		t.Fatalf("status %q", m.status)
	}
	// Esc clears the selection and nothing else; the next Esc is a normal key
	m.mode = mFocus
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.hl.active {
		t.Fatal("esc did not clear selection")
	}
	m.snd.wait()
	if got := m.io.(*scrollPane).flat(); got != "" {
		t.Fatalf("esc forwarded while clearing selection: %q", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.snd.wait()
	if got := m.io.(*scrollPane).flat(); got != "Escape" {
		t.Fatalf("second esc: %q", got)
	}
	// any other key clears the selection and is still forwarded
	m.hl = selection{true, cell{0, 0}, cell{3, 0}}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	m.snd.wait()
	if m.hl.active || m.io.(*scrollPane).flat() != "Escape 'z'" {
		t.Fatalf("key with selection: %v %q", m.hl.active, m.io.(*scrollPane).flat())
	}
}

func TestDoubleTripleClickCopy(t *testing.T) {
	m, _, _ := scrollModel(t)
	var copied []string
	c, _, _ := fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true}, nil)
	c.Run = func(_ context.Context, _ string, _ []string, stdin string) error {
		copied = append(copied, stdin)
		return nil
	}
	m.clip = c
	g := m.geom()
	x, y := g.PrevX0+6, g.Top+1+1 // on "001"-ish digits of "line 002"
	click := func() {
		_, _ = m.Update(mev(x, y, tea.MouseActionPress, tea.MouseButtonLeft))
		_, cmd := m.Update(mev(x, y, tea.MouseActionRelease, tea.MouseButtonLeft))
		pump(m, cmd)
	}
	click()
	if m.mode != mFocus || len(copied) != 0 {
		t.Fatalf("single click: mode %v copied %v", m.mode, copied)
	}
	click()
	if len(copied) != 1 || copied[0] != strings.Fields(m.plainRows()[1])[1] {
		t.Fatalf("double click copied %q", copied)
	}
	click()
	if len(copied) != 2 || copied[1] != strings.TrimSpace(m.plainRows()[1]) {
		t.Fatalf("triple click copied %q", copied)
	}
}

func TestMouseToggleKey(t *testing.T) {
	m, _, _ := paneModel(t)
	_, c := m.Update(key("m"))
	if m.mouseOn || c == nil {
		t.Fatal("m should release mouse capture")
	}
	if _, ok := c().(interface{}); !ok {
		t.Fatal("no msg")
	}
	if !strings.Contains(ansi.Strip(m.View()), "mouse capture OFF") {
		t.Fatalf("no footer hint:\n%s", ansi.Strip(m.View()))
	}
	_, c = m.Update(key("m"))
	if !m.mouseOn || c == nil {
		t.Fatal("m should re-enable capture")
	}
}

func TestMouseDispatchPaneModes(t *testing.T) {
	t0 := time.Unix(2000, 0)
	app := tgeom
	app.MouseAny, app.AltScreen, app.Focused = true, true, true
	appUnfocused := app
	appUnfocused.Focused = false
	pager := tgeom
	pager.AltScreen = true
	norm := tgeom
	norm.Focused = true
	one := func(g mouseGeom, evs ...tea.MouseMsg) mouseAct {
		var st mouseState
		var a mouseAct
		for i, e := range evs {
			st, a = mouseDispatch(e, g, st, t0.Add(time.Duration(i)*time.Millisecond))
		}
		return a
	}
	press := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionPress, tea.MouseButtonLeft) }
	move := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionMotion, tea.MouseButtonLeft) }
	rel := func(x, y int) tea.MouseMsg { return mev(x, y, tea.MouseActionRelease, tea.MouseButtonLeft) }
	wheel := func(x, y int, b tea.MouseButton) tea.MouseMsg { return mev(x, y, tea.MouseActionPress, b) }

	// wheel: mode selection by pane flags
	if a := one(app, wheel(30, 4, tea.MouseButtonWheelUp)); a.Kind != mkFwdWheel || a.Delta >= 0 || a.A != (cell{10, 2}) {
		t.Fatalf("app wheel: %+v", a)
	}
	if a := one(appUnfocused, wheel(30, 4, tea.MouseButtonWheelDown)); a.Kind != mkFwdWheel || a.Delta <= 0 {
		t.Fatalf("app wheel in list mode: %+v", a)
	}
	if a := one(pager, wheel(30, 4, tea.MouseButtonWheelDown)); a.Kind != mkScrollKeys || a.Delta <= 0 {
		t.Fatalf("alt screen without mouse: %+v", a)
	}
	if a := one(norm, wheel(30, 4, tea.MouseButtonWheelDown)); a.Kind != mkPreviewWheel {
		t.Fatalf("normal screen: %+v", a)
	}
	// wheel over the list is always the list
	if a := one(app, wheel(3, 4, tea.MouseButtonWheelDown)); a.Kind != mkListWheel {
		t.Fatalf("list wheel: %+v", a)
	}
	// coordinates at the corners of the inner area (inner is 40 x 10 at x 20.., y 2.. — the preview has no side bars)
	corners := []struct {
		x, y         int
		wantC, wantR int // 1-based
	}{{20, 2, 1, 1}, {59, 2, 40, 1}, {20, 11, 1, 10}, {59, 11, 40, 10}, {20, 1, 1, 1}, {59, 12, 40, 10}}
	for _, c := range corners {
		a := one(app, wheel(c.x, c.y, tea.MouseButtonWheelUp))
		if a.Kind != mkFwdWheel || a.A.X+1 != c.wantC || a.A.Y+1 != c.wantR {
			t.Errorf("corner (%d,%d): %+v want col=%d row=%d", c.x, c.y, a, c.wantC, c.wantR)
		}
	}

	// clicks
	if a := one(app, press(25, 5), rel(25, 5)); a.Kind != mkFwdClick || a.Btn != 0 || a.A != (cell{5, 3}) {
		t.Fatalf("focused click with app mouse: %+v", a)
	}
	if a := one(appUnfocused, press(25, 5), rel(25, 5)); a.Kind != mkPreviewFocus {
		t.Fatalf("unfocused click only focuses: %+v", a)
	}
	// a double click is two forwarded clicks while the app owns the mouse
	if a := one(app, press(25, 5), rel(25, 5), press(25, 5), rel(25, 5)); a.Kind != mkFwdClick {
		t.Fatalf("double click: %+v", a)
	}
	// without app mouse a double click selects a word
	if a := one(norm, press(25, 5), rel(25, 5), press(25, 5), rel(25, 5)); a.Kind != mkSelWord {
		t.Fatalf("double click without app mouse: %+v", a)
	}
	// a drag is ours (selection), never forwarded
	if a := one(app, press(25, 5), move(30, 5), rel(30, 5)); a.Kind != mkSelCopy {
		t.Fatalf("drag: %+v", a)
	}
	if a := one(app, press(25, 5), move(30, 5)); a.Kind != mkSelUpdate {
		t.Fatalf("drag motion: %+v", a)
	}
	// right / middle: forwarded as 2 / 1 when focused with app mouse, else ignored
	if a := one(app, mev(25, 5, tea.MouseActionPress, tea.MouseButtonRight)); a.Kind != mkFwdClick || a.Btn != 2 {
		t.Fatalf("right: %+v", a)
	}
	if a := one(app, mev(25, 5, tea.MouseActionPress, tea.MouseButtonMiddle)); a.Kind != mkFwdClick || a.Btn != 1 {
		t.Fatalf("middle: %+v", a)
	}
	if a := one(appUnfocused, mev(25, 5, tea.MouseActionPress, tea.MouseButtonRight)); a.Kind != mkNone {
		t.Fatalf("unfocused right: %+v", a)
	}
	if a := one(norm, mev(25, 5, tea.MouseActionPress, tea.MouseButtonRight)); a.Kind != mkNone {
		t.Fatalf("right without app mouse: %+v", a)
	}
}

func TestMouseReportEncoding(t *testing.T) {
	if got := wheelReports(true, cell{11, 6}, true, 2); got != "\x1b[<64;12;7M\x1b[<64;12;7M" {
		t.Fatalf("%q", got)
	}
	if got := wheelReports(false, cell{0, 0}, true, 1); got != "\x1b[<65;1;1M" {
		t.Fatalf("%q", got)
	}
	if got := clickReports(0, cell{2, 3}, true); got != "\x1b[<0;3;4M\x1b[<0;3;4m" {
		t.Fatalf("%q", got)
	}
	if got := clickReports(2, cell{2, 3}, true); got != "\x1b[<2;3;4M\x1b[<2;3;4m" {
		t.Fatalf("%q", got)
	}
	// legacy X10: ESC [ M, button+32, col+32, row+32; release is button 3
	if got := wheelReports(true, cell{0, 0}, false, 1); got != "\x1b[M`!!" {
		t.Fatalf("%q", got)
	}
	if got := clickReports(0, cell{0, 0}, false); got != "\x1b[M !!\x1b[M#!!" {
		t.Fatalf("%q", got)
	}
	if got := wheelReports(true, cell{150, 0}, false, 1); got != "" {
		t.Fatalf("legacy beyond 95 cols must be skipped: %q", got)
	}
}

// appPane is a scrollPane whose snapshot reports the given pane mode.
type appPane struct {
	scrollPane
	alt, mouse, sgr bool
}

func (p *appPane) SnapshotAt(ctx context.Context, s string, off int) (tmux.Snapshot, error) {
	snap, err := p.scrollPane.SnapshotAt(ctx, s, 0)
	snap.HistorySize, snap.Offset = 0, 0
	snap.AltScreen, snap.MouseAny, snap.MouseSGR = p.alt, p.mouse, p.sgr
	return snap, err
}

func appModel(t *testing.T, alt, mouse, sgr bool) (*Model, *appPane) {
	t.Helper()
	m, _, _ := paneModel(t)
	pw, ph := m.previewSize()
	ap := &appPane{alt: alt, mouse: mouse, sgr: sgr}
	ap.total, ap.h, ap.w = 100, ph, pw
	m.io = ap
	m.snd = newSender(ap)
	pump(m, m.previewKick())
	return m, ap
}

func TestForwardingToAppWithMouse(t *testing.T) {
	m, ap := appModel(t, true, true, true)
	g := m.geom()
	if !g.MouseAny || !g.AltScreen {
		t.Fatalf("geom flags not taken from snapshot: %+v", g)
	}
	// wheel in LIST mode reaches the app as SGR reports at translated coords; no history offset
	_, c := m.Update(mev(g.PrevX0+11, g.Top+1+6, tea.MouseActionPress, tea.MouseButtonWheelUp))
	pump(m, c)
	m.snd.wait()
	want := strings.Repeat("\x1b[<64;12;7M", wheelReportsPerNotch)
	if got := ap.flat(); got != "'"+want+"'" {
		t.Fatalf("forwarded %q want %q", got, want)
	}
	if m.scroll["orch"] != 0 || strings.Contains(ansi.Strip(m.View()), "lines back") {
		t.Fatal("history path used for an alt-screen app")
	}
	// unfocused click only focuses, nothing forwarded
	before := ap.flat()
	m.Update(mev(g.PrevX0+5, g.Top+3, tea.MouseActionPress, tea.MouseButtonLeft))
	_, c = m.Update(mev(g.PrevX0+5, g.Top+3, tea.MouseActionRelease, tea.MouseButtonLeft))
	pump(m, c)
	m.snd.wait()
	if m.mode != mFocus || ap.flat() != before {
		t.Fatalf("unfocused click: mode %v flat %q", m.mode, ap.flat())
	}
	if !strings.Contains(ansi.Strip(m.View()), "mouse: app") {
		t.Fatal("footer lacks the app-mouse hint")
	}
	// focused click is forwarded as press+release
	m.Update(mev(g.PrevX0+2, g.Top+1+3, tea.MouseActionPress, tea.MouseButtonLeft))
	_, c = m.Update(mev(g.PrevX0+2, g.Top+1+3, tea.MouseActionRelease, tea.MouseButtonLeft))
	pump(m, c)
	m.snd.wait()
	if got := ap.flat(); got != before+" '\x1b[<0;3;4M\x1b[<0;3;4m'" {
		t.Fatalf("click forwarded %q", got)
	}
	// a drag is selection + copy, not forwarded
	var copied []string
	cb, _, _ := fakeClip(map[string]string{"DISPLAY": ":0"}, map[string]bool{"xclip": true}, nil)
	cb.Run = func(_ context.Context, _ string, _ []string, in string) error {
		copied = append(copied, in)
		return nil
	}
	m.clip = cb
	n := len(ap.flat())
	m.Update(mev(g.PrevX0+1, g.Top+1+1, tea.MouseActionPress, tea.MouseButtonLeft))
	m.Update(mev(g.PrevX0+6, g.Top+1+2, tea.MouseActionMotion, tea.MouseButtonLeft))
	_, c = m.Update(mev(g.PrevX0+6, g.Top+1+2, tea.MouseActionRelease, tea.MouseButtonLeft))
	pump(m, c)
	m.snd.wait()
	if len(ap.flat()) != n || len(copied) != 1 {
		t.Fatalf("drag forwarded or not copied: %q %v", ap.flat(), copied)
	}
}

func TestAltScreenWithoutMouseSendsArrows(t *testing.T) {
	m, ap := appModel(t, true, false, false)
	g := m.geom()
	_, c := m.Update(mev(g.PrevX0+5, g.Top+3, tea.MouseActionPress, tea.MouseButtonWheelUp))
	pump(m, c)
	_, c = m.Update(mev(g.PrevX0+5, g.Top+3, tea.MouseActionPress, tea.MouseButtonWheelDown))
	pump(m, c)
	m.snd.wait()
	if got := ap.flat(); got != "Up Up Up Down Down Down" {
		t.Fatalf("%q", got)
	}
}

func TestLegacyEncodingFallback(t *testing.T) {
	m, ap := appModel(t, true, true, false)
	g := m.geom()
	_, c := m.Update(mev(g.PrevX0, g.Top+1, tea.MouseActionPress, tea.MouseButtonWheelDown))
	pump(m, c)
	m.snd.wait()
	if got := ap.flat(); got != "'"+strings.Repeat("\x1b[Ma!!", wheelReportsPerNotch)+"'" {
		t.Fatalf("%q", got)
	}
}
