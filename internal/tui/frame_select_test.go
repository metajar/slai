package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const frameGlyphs = "│╭╮╰╯"

// previewScreenColumns returns, for every screen row of the preview body, the plain
// text of the preview's columns [x0, x0+w).
func previewScreenRows(m *Model) []string {
	g := m.geom()
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	var out []string
	for y := g.Top; y < g.Top+g.BodyH && y < len(lines); y++ {
		out = append(out, ansi.Strip(ansi.Cut(lines[y], g.PrevX0, g.PrevX0+g.PrevW)))
	}
	return out
}

// The built-in selection works on the pane's own rows, so the TUI frame can never be
// in what it copies: not for a full-pane drag, not for drags that start or end on the
// very edge cells, not in focus mode.
func TestBuiltInSelectionNeverYieldsFrameGlyphs(t *testing.T) {
	for _, focused := range []bool{false, true} {
		m, _, _ := scrollModel(t)
		if focused {
			m.Update(key("enter"))
			if m.mode != mFocus {
				t.Fatal("not focused")
			}
		}
		pw, ph := m.previewSize()
		rows := m.plainRows()
		for _, r := range rows {
			if strings.ContainsAny(r, frameGlyphs) {
				t.Fatalf("focused=%v: pane rows carry frame glyphs: %q", focused, r)
			}
		}
		drags := [][2]cell{
			{{0, 0}, {pw - 1, ph - 1}},
			{{0, 0}, {0, 0}},
			{{pw - 1, 0}, {pw - 1, ph - 1}},
			{{pw - 1, ph - 1}, {0, 0}},
			{{-1, -1}, {pw, ph}}, // a drag that began on the border column / row
		}
		for _, d := range drags {
			if got := extractSelection(rows, d[0], d[1], pw); strings.ContainsAny(got, frameGlyphs) {
				t.Fatalf("focused=%v drag %v copied frame glyphs: %q", focused, d, got)
			}
		}
		// the same through the real mouse path, starting on the border column
		g := m.geom()
		var copied string
		m.clip.Run = func(_ context.Context, _ string, _ []string, stdin string) error { copied = stdin; return nil }
		m.Update(mev(g.PrevX0, g.Top, tea.MouseActionPress, tea.MouseButtonLeft))
		_, cmd := m.Update(mev(g.PrevX0+g.PrevW-1, g.Top+g.BodyH-1, tea.MouseActionRelease, tea.MouseButtonLeft))
		pump(m, cmd)
		if strings.ContainsAny(copied, frameGlyphs) {
			t.Fatalf("focused=%v mouse drag from the border copied %q", focused, copied)
		}
	}
}

// Terminal-native selection (mouse capture off, shift+drag) copies whole screen
// columns, so the preview must not draw a vertical bar at either side.
func TestPreviewSideColumnsHaveNoVerticalBar(t *testing.T) {
	for _, focused := range []bool{false, true} {
		m, _, _ := scrollModel(t)
		if focused {
			m.Update(key("enter"))
		}
		rows := previewScreenRows(m)
		if len(rows) < 5 {
			t.Fatalf("only %d preview rows", len(rows))
		}
		for i, r := range rows[1 : len(rows)-1] {
			if strings.ContainsAny(r, "│") {
				t.Fatalf("focused=%v row %d has a vertical bar: %q", focused, i+1, r)
			}
		}
	}
}

func TestExtractSelectionStripsBoxGutterOnlyWhenEveryLineHasIt(t *testing.T) {
	boxed := []string{"╭────────╮", "│ hello  │", "│ world  │", "│        │", "╰────────╯"}
	if got := extractSelection(boxed[1:4], cell{0, 0}, cell{9, 2}, 10); got != "hello\nworld" {
		t.Errorf("gutters not stripped: %q", got)
	}
	// the selection includes a line without the gutter: nothing is touched
	if got := extractSelection(boxed, cell{0, 0}, cell{9, 4}, 10); !strings.Contains(got, "│ hello") {
		t.Errorf("mixed selection must stay verbatim: %q", got)
	}
	// a stream selection that starts inside the box and stops inside it carries no full gutter
	if got := extractSelection(boxed[1:3], cell{2, 0}, cell{5, 1}, 10); got != "hello  │\n│ worl" {
		t.Errorf("inner selection: %q", got)
	}
	// only the left gutter present (pane text cut at the right edge)
	if got := extractSelection([]string{"│ a b", "│ c"}, cell{0, 0}, cell{9, 1}, 10); got != "a b\nc" {
		t.Errorf("left gutter only: %q", got)
	}
}
