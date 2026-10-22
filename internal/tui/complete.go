package tui

import (
	"sort"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/oorbt/slai/internal/store"
)

// maxHintCands is how many candidates the one-line completion hint shows.
const maxHintCands = 5

// completion is the Tab-completion state of the quick-send box. It lives only while
// the user cycles; any other key ends it and keeps whatever text is in the box.
type completion struct {
	active     bool
	none       bool     // Tab found nothing: the hint says "no matches"
	start, end int      // rune span of the word being completed (end moves as candidates differ)
	orig       string   // the word as typed, restored by Esc
	cands      []string // candidates in cycling order
	idx        int
	stepped    bool // a candidate has been inserted (the first Tab inserts the first one)
}

// wordAt returns the rune span of the word that ends at the cursor.
func wordAt(rs []rune, cur int) (int, int) {
	cur = max(0, min(cur, len(rs)))
	s := cur
	for s > 0 && !unicode.IsSpace(rs[s-1]) {
		s--
	}
	return s, cur
}

func isOpenTask(t store.Task) bool {
	switch t.Status {
	case store.StatusMerged, store.StatusCancelled, store.StatusDone, store.StatusReverted:
		return false
	}
	return true
}

// orderedTasks is the board's tasks with open ones first (by id), then the closed ones,
// most recently updated first. It only reads the model's cached tasks.
func orderedTasks(ts []store.Task) []store.Task {
	out := append([]store.Task(nil), ts...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := isOpenTask(out[i]), isOpenTask(out[j])
		if oi != oj {
			return oi
		}
		if oi {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// candidates lists what the word could complete to, and the text to insert for each.
func (m *Model) candidates(word string) []string {
	lw := strings.ToLower(word)
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	agent := func(prefix string) {
		for _, a := range m.agents {
			if strings.HasPrefix(strings.ToLower(a.ID), prefix) {
				add(a.ID)
			}
		}
	}
	switch {
	case lw == "":
	case strings.HasPrefix(lw, "@"):
		agent(lw[1:])
	case strings.HasPrefix(lw, "slai/"):
		for _, t := range orderedTasks(m.tasks) {
			if t.Branch != "" && strings.HasPrefix(strings.ToLower(t.Branch), lw) {
				add(t.Branch)
			}
		}
	case strings.HasPrefix(lw, "t-") || allDigits(lw):
		num := strings.TrimPrefix(lw, "t-")
		for _, t := range orderedTasks(m.tasks) {
			id := strings.ToLower(t.ID)
			if strings.HasPrefix(lw, "t-") && strings.HasPrefix(id, lw) ||
				allDigits(lw) && strings.Contains(strings.TrimPrefix(id, "t-"), num) {
				add(t.ID)
			}
		}
	default:
		agent(lw)
	}
	return out
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// completeKey handles Tab / Shift+Tab / Esc for the quick-send box. handled is false
// for every other key (which also ends a running completion).
func (m *Model) completeKey(k tea.KeyMsg) (handled bool) {
	if m.quickHold {
		return false
	}
	switch k.Type {
	case tea.KeyTab, tea.KeyShiftTab:
		if !m.comp.active {
			m.startCompletion()
		}
		if m.comp.active && len(m.comp.cands) > 0 {
			n := len(m.comp.cands)
			if m.comp.stepped {
				d := 1
				if k.Type == tea.KeyShiftTab {
					d = n - 1
				}
				m.comp.idx = (m.comp.idx + d) % n
			} else if k.Type == tea.KeyShiftTab {
				m.comp.idx = n - 1
			}
			m.comp.stepped = true
			m.applyCompletion()
		}
		return true
	case tea.KeyEsc:
		if !m.comp.active {
			return false
		}
		if len(m.comp.cands) > 0 {
			m.replaceWord(m.comp.orig)
		}
		m.comp = completion{}
		return true
	}
	m.comp = completion{}
	return false
}

func (m *Model) startCompletion() {
	rs := []rune(m.quick.Value())
	s, e := wordAt(rs, m.quick.Position())
	word := string(rs[s:e])
	c := completion{active: true, start: s, end: e, orig: word, cands: m.candidates(word)}
	c.none = len(c.cands) == 0
	m.comp = c
}

func (m *Model) applyCompletion() {
	m.replaceWord(m.comp.cands[m.comp.idx])
}

// replaceWord swaps the span being completed for text and parks the cursor after it.
func (m *Model) replaceWord(text string) {
	rs := []rune(m.quick.Value())
	s, e := min(m.comp.start, len(rs)), min(m.comp.end, len(rs))
	nr := append(append(append([]rune(nil), rs[:s]...), []rune(text)...), rs[e:]...)
	m.quick.SetValue(string(nr))
	m.comp.end = s + len([]rune(text))
	m.quick.SetCursor(m.comp.end)
}

// completionHint is the one line under the send box: up to five candidates around the
// selected one (highlighted), or "no matches".
func (m *Model) completionHint() string {
	c := m.comp
	if !c.active {
		return ""
	}
	if len(c.cands) == 0 {
		return styleDim.Render("no matches")
	}
	lo := 0
	if len(c.cands) > maxHintCands {
		lo = max(0, min(c.idx-maxHintCands/2, len(c.cands)-maxHintCands))
	}
	hi := min(lo+maxHintCands, len(c.cands))
	var parts []string
	for i := lo; i < hi; i++ {
		if i == c.idx && c.stepped {
			parts = append(parts, styleSelected.Render(c.cands[i]))
		} else {
			parts = append(parts, styleDim.Render(c.cands[i]))
		}
	}
	s := strings.Join(parts, "  ")
	if lo > 0 {
		s = styleDim.Render("… ") + s
	}
	if hi < len(c.cands) {
		s += styleDim.Render(" …")
	}
	return s
}
