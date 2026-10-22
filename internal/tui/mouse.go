package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// wheelStep is how many lines (list rows / history lines) one wheel notch moves.
const wheelStep = 3

// multiClickWindow is the max gap between presses on the same cell for a
// double / triple click.
const multiClickWindow = 400 * time.Millisecond

// cell is a position inside the preview's inner rectangle (0,0 = top left).
type cell struct{ X, Y int }

func (c cell) before(o cell) bool { return c.Y < o.Y || (c.Y == o.Y && c.X < o.X) }

// mouseGeom is the screen geometry mouse events are interpreted against.
type mouseGeom struct {
	ListOn bool
	ListW  int // list pane width, including borders (starts at x=0)
	PrevOn bool
	PrevX0 int // preview pane left edge (screen x)
	PrevW  int // preview pane width: all content, the preview has no side bars
	Top    int // screen row of the body's first line
	BodyH  int // body height, including borders

	// State of the selected agent's pane, which decides what the mouse means.
	Focused   bool // keys / clicks go to the agent
	AltScreen bool // the app is on the alternate screen (no scrollback)
	MouseAny  bool // the app asked for mouse reports
}

func (g mouseGeom) inList(x int) bool { return g.ListOn && x < g.ListW }

// inPreview: anywhere in the preview box, borders included.
func (g mouseGeom) inPreview(x, y int) bool {
	return g.PrevOn && x >= g.PrevX0 && x < g.PrevX0+g.PrevW && y >= g.Top && y < g.Top+g.BodyH
}

// inner is the content rectangle size: the full width, and the height minus the top and
// bottom rules.
func (g mouseGeom) inner() (w, h int) { return max(g.PrevW, 1), max(g.BodyH-2, 1) }

// innerCell maps a screen point to a preview cell if it is inside the inner area.
func (g mouseGeom) innerCell(x, y int) (cell, bool) {
	w, h := g.inner()
	c := cell{x - g.PrevX0, y - g.Top - 1}
	return c, g.PrevOn && c.X >= 0 && c.X < w && c.Y >= 0 && c.Y < h
}

// clampCell clamps a screen point into the inner area (drags never leak out).
func (g mouseGeom) clampCell(x, y int) cell {
	w, h := g.inner()
	return cell{clamp(x-g.PrevX0, 0, w-1), clamp(y-g.Top-1, 0, h-1)}
}

type mouseKind int

const (
	mkNone mouseKind = iota
	mkListWheel
	mkPreviewWheel
	mkListClick    // X, Y = screen coordinates
	mkPreviewFocus // click (no drag) on the preview
	mkSelBegin     // press inside the preview: clear the old selection
	mkSelUpdate    // drag: A..B is the selection so far
	mkSelCopy      // drag finished: copy A..B
	mkSelWord      // double click at A
	mkSelLine      // triple click at A
	mkFwdWheel     // forward a wheel report to the app at pane cell A (Delta<0 = up)
	mkFwdClick     // forward press+release of button Btn at pane cell A
	mkScrollKeys   // alt screen without mouse reporting: send Up/Down keys (Delta<0 = up)
)

// mouseAct is what a mouse event means.
type mouseAct struct {
	Kind  mouseKind
	Delta int // wheel: negative = up
	Btn   int // mkFwdClick: SGR button code (0 left, 1 middle, 2 right)
	X, Y  int
	A, B  cell
}

// mouseState is the press/drag/click-count memory between events.
type mouseState struct {
	pressed  bool
	dragging bool
	anchor   cell
	head     cell
	clicks   int
	lastAt   time.Time
	lastCell cell
}

// mouseDispatch is the pure core of mouse handling: it maps one event to an
// action and the next state. It performs no I/O and touches no model.
func mouseDispatch(ev tea.MouseMsg, g mouseGeom, st mouseState, now time.Time) (mouseState, mouseAct) {
	switch ev.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		if ev.Action != tea.MouseActionPress {
			return st, mouseAct{}
		}
		d := wheelStep
		if ev.Button == tea.MouseButtonWheelUp {
			d = -wheelStep
		}
		switch {
		case g.inList(ev.X):
			return st, mouseAct{Kind: mkListWheel, Delta: d}
		case g.inPreview(ev.X, ev.Y):
			switch {
			case g.MouseAny: // the app scrolls itself: give it the report
				return st, mouseAct{Kind: mkFwdWheel, Delta: d, A: g.clampCell(ev.X, ev.Y)}
			case g.AltScreen: // nothing in tmux to scroll: act like a pager
				return st, mouseAct{Kind: mkScrollKeys, Delta: d}
			}
			return st, mouseAct{Kind: mkPreviewWheel, Delta: d}
		}
		return st, mouseAct{}
	}

	switch ev.Action {
	case tea.MouseActionPress:
		if ev.Button == tea.MouseButtonMiddle || ev.Button == tea.MouseButtonRight {
			c, ok := g.innerCell(ev.X, ev.Y)
			if ok && g.Focused && g.MouseAny {
				btn := 1
				if ev.Button == tea.MouseButtonRight {
					btn = 2
				}
				return st, mouseAct{Kind: mkFwdClick, Btn: btn, A: c}
			}
			return st, mouseAct{}
		}
		if ev.Button != tea.MouseButtonLeft {
			return st, mouseAct{}
		}
		st.pressed, st.dragging = false, false
		if g.inList(ev.X) {
			st.clicks = 0
			return st, mouseAct{Kind: mkListClick, X: ev.X, Y: ev.Y}
		}
		if !g.inPreview(ev.X, ev.Y) {
			return st, mouseAct{}
		}
		c, ok := g.innerCell(ev.X, ev.Y)
		if !ok { // border
			st.clicks = 0
			return st, mouseAct{Kind: mkPreviewFocus}
		}
		if st.clicks > 0 && now.Sub(st.lastAt) <= multiClickWindow && st.lastCell == c {
			st.clicks++
		} else {
			st.clicks = 1
		}
		st.lastAt, st.lastCell = now, c
		st.pressed, st.anchor, st.head = true, c, c
		return st, mouseAct{Kind: mkSelBegin, A: c}

	case tea.MouseActionMotion:
		if !st.pressed {
			return st, mouseAct{}
		}
		c := g.clampCell(ev.X, ev.Y)
		if c == st.head {
			return st, mouseAct{}
		}
		st.head = c
		st.dragging = st.dragging || c != st.anchor
		if !st.dragging {
			return st, mouseAct{}
		}
		return st, mouseAct{Kind: mkSelUpdate, A: st.anchor, B: c}

	case tea.MouseActionRelease:
		if !st.pressed {
			return st, mouseAct{}
		}
		c := g.clampCell(ev.X, ev.Y)
		st.pressed = false
		if st.dragging || c != st.anchor {
			st.dragging, st.clicks = false, 0
			return st, mouseAct{Kind: mkSelCopy, A: st.anchor, B: c}
		}
		if g.MouseAny { // the app owns clicks (double click = two clicks)
			if g.Focused {
				return st, mouseAct{Kind: mkFwdClick, Btn: 0, A: c}
			}
			return st, mouseAct{Kind: mkPreviewFocus}
		}
		switch {
		case st.clicks >= 3:
			st.clicks = 0
			return st, mouseAct{Kind: mkSelLine, A: c}
		case st.clicks == 2:
			return st, mouseAct{Kind: mkSelWord, A: c}
		}
		return st, mouseAct{Kind: mkPreviewFocus}
	}
	return st, mouseAct{}
}

// ---- scroll math ----

// scrollBy moves an offset (lines back from the bottom) by delta, clamped to
// [0, hist].
func scrollBy(off, delta, hist int) int { return clamp(off+delta, 0, max(hist, 0)) }

// followHistory keeps the same absolute position when `appended` new lines
// were pushed into the scrollback while the view is scrolled back.
func followHistory(off, appended, hist int) int {
	if off <= 0 || appended <= 0 {
		return clamp(off, 0, max(hist, 0))
	}
	return clamp(off+appended, 0, max(hist, 0))
}

// ---- selection text ----

// cluster is one displayed glyph (base rune + combining marks) and the cells it covers.
type cluster struct {
	x, w int
	s    string
}

// clusters splits plain text into glyphs with their cell extents.
func clusters(s string) []cluster {
	var out []cluster
	x := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if w == 0 && len(out) > 0 { // combining mark / ZWJ etc.: attach to the previous glyph
			out[len(out)-1].s += string(r)
			continue
		}
		out = append(out, cluster{x: x, w: w, s: string(r)})
		x += w
	}
	return out
}

// normSel orders a and b in reading order.
func normSel(a, b cell) (cell, cell) {
	if b.before(a) {
		return b, a
	}
	return a, b
}

// rowSpan is the half-open cell range [c0,c1) selected on row y for a stream
// selection a..b (inclusive of b's cell). ok is false when the row is outside.
func rowSpan(a, b cell, y, w int) (c0, c1 int, ok bool) {
	a, b = normSel(a, b)
	if y < a.Y || y > b.Y {
		return 0, 0, false
	}
	c0, c1 = 0, w
	if y == a.Y {
		c0 = a.X
	}
	if y == b.Y {
		c1 = b.X + 1
	}
	return clamp(c0, 0, w), clamp(c1, 0, w), true
}

func sliceCells(plain string, c0, c1 int) string {
	var sb strings.Builder
	for _, cl := range clusters(plain) {
		if cl.x < c1 && cl.x+max(cl.w, 1) > c0 {
			sb.WriteString(cl.s)
		}
	}
	return sb.String()
}

// extractSelection returns the plain text of the stream selection a..b over
// rows (ANSI is stripped; trailing spaces are trimmed per line; lines joined
// with \n; trailing empty lines dropped).
func extractSelection(rows []string, a, b cell, w int) string {
	a, b = normSel(a, b)
	var lines []string
	for y := max(a.Y, 0); y <= b.Y && y < len(rows); y++ {
		c0, c1, ok := rowSpan(a, b, y, w)
		if !ok {
			continue
		}
		lines = append(lines, strings.TrimRight(sliceCells(ansi.Strip(rows[y]), c0, c1), " \t"))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(stripGutters(lines), "\n")
}

// stripGutters removes a box-drawing "│ " gutter on the left and a " │" on the right
// when every non-empty selected line carries it (Claude Code draws its input box and
// dialogs that way). One line without the gutter leaves the text exactly as selected.
func stripGutters(lines []string) []string {
	left, right, any := true, true, false
	for _, l := range lines {
		if l == "" {
			continue
		}
		any = true
		left = left && strings.HasPrefix(l, "│")
		right = right && strings.HasSuffix(l, "│")
	}
	if !any || (!left && !right) {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if l != "" {
			if left {
				l = strings.TrimPrefix(strings.TrimPrefix(l, "│"), " ")
			}
			if right {
				l = strings.TrimRight(strings.TrimSuffix(l, "│"), " ")
			}
		}
		out[i] = l
	}
	for len(out) > 0 && out[len(out)-1] == "" { // an empty box row is empty once stripped
		out = out[:len(out)-1]
	}
	return out
}

// wordBounds finds the word under cell column x in a plain row: the maximal
// run of non-space glyphs (so paths and URLs select whole). ok is false on a space.
func wordBounds(plain string, x int) (c0, c1 int, ok bool) {
	cls := clusters(plain)
	at := -1
	for i, cl := range cls {
		if x >= cl.x && x < cl.x+max(cl.w, 1) {
			at = i
		}
	}
	isSep := func(s string) bool { return strings.TrimFunc(s, unicode.IsSpace) == "" }
	if at < 0 || isSep(cls[at].s) {
		return 0, 0, false
	}
	lo, hi := at, at
	for lo > 0 && !isSep(cls[lo-1].s) {
		lo--
	}
	for hi+1 < len(cls) && !isSep(cls[hi+1].s) {
		hi++
	}
	return cls[lo].x, cls[hi].x + max(cls[hi].w, 1) - 1, true
}

// highlightRow paints cells [c0,c1) of an ANSI row in reverse video while
// leaving the rest of the row's colours intact.
func highlightRow(line string, c0, c1, w int) string {
	if c1 <= c0 {
		return line
	}
	line = padTo(line, w)
	mid := ansi.Strip(ansi.Cut(line, c0, c1))
	return ansi.Truncate(line, c0, "") + "\x1b[0m\x1b[7m" + mid + "\x1b[0m" + ansi.TruncateLeft(line, c1, "")
}

// highlightRows applies the selection a..b to rows.
func highlightRows(rows []string, a, b cell, w int) []string {
	out := append([]string(nil), rows...)
	for y := range out {
		if c0, c1, ok := rowSpan(a, b, y, w); ok {
			out[y] = highlightRow(out[y], c0, c1, w)
		}
	}
	return out
}

// ---- forwarding mouse reports to the app ----

// sgrReport is an SGR (1006) mouse report; col and row are 1-based.
func sgrReport(code, col, row int, press bool) string {
	final := "M"
	if !press {
		final = "m"
	}
	return fmt.Sprintf("\x1b[<%d;%d;%d%s", code, col, row, final)
}

// legacyReport is the X10 / normal-mode report. Coordinates beyond 95 would
// need bytes >= 128, which send-keys -l cannot carry, so those are skipped.
func legacyReport(code, col, row int, press bool) string {
	if col > 95 || row > 95 {
		return ""
	}
	if !press {
		code = 3 | (code &^ 3)
	}
	return string([]byte{0x1b, '[', 'M', byte(32 + code), byte(32 + col), byte(32 + row)})
}

// wheelReports builds n wheel reports (up: button 64, down: 65) for the app's
// encoding at 0-based pane cell c.
func wheelReports(up bool, c cell, sgr bool, n int) string {
	code := 65
	if up {
		code = 64
	}
	one := sgrReport(code, c.X+1, c.Y+1, true)
	if !sgr {
		one = legacyReport(code, c.X+1, c.Y+1, true)
	}
	return strings.Repeat(one, n)
}

// clickReports builds the press + release pair for button btn at cell c.
func clickReports(btn int, c cell, sgr bool) string {
	if sgr {
		return sgrReport(btn, c.X+1, c.Y+1, true) + sgrReport(btn, c.X+1, c.Y+1, false)
	}
	return legacyReport(btn, c.X+1, c.Y+1, true) + legacyReport(btn, c.X+1, c.Y+1, false)
}
