package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/store"
)

func completeModel() *Model {
	m := New(Options{})
	m.agents = []store.Agent{{ID: "orch"}, {ID: "pm"}, {ID: "principal"}, {ID: "eng-1"}, {ID: "eng-2"}}
	m.tasks = []store.Task{
		{ID: "T-0001", Status: "merged", Branch: "slai/eng-1/T-0001", UpdatedAt: 10},
		{ID: "T-0002", Status: "merged", Branch: "slai/eng-2/T-0002", UpdatedAt: 20},
		{ID: "T-0071", Status: "in_progress", Branch: "slai/eng-2/T-0071"},
		{ID: "T-0072", Status: "assigned", Branch: "slai/eng-2/T-0072"},
		{ID: "T-0070", Status: "in_review", Branch: "slai/eng-1/T-0070"},
	}
	m.tasks = append(m.tasks,
		store.Task{ID: "T-0003", Status: store.StatusReverted, UpdatedAt: 30},
		store.Task{ID: "T-0004", Status: store.StatusDone, UpdatedAt: 40})
	m.mode, m.quickTo = mQuick, "orch"
	m.quick.SetValue("")
	m.quick.Focus()
	return m
}

func press(m *Model, ks ...tea.KeyMsg) {
	for _, k := range ks {
		m.handleKey(k)
	}
}

var shiftTab = tea.KeyMsg{Type: tea.KeyShiftTab}

func hintText(m *Model) string { return ansi.Strip(m.completionHint()) }

// AC 1
func TestTabCompletesOpenTaskIdsAndCycles(t *testing.T) {
	m := completeModel()
	press(m, key("T-00"), key("tab"))
	if got := m.quick.Value(); got != "T-0070" {
		t.Fatalf("first Tab = %q, want the first open task", got)
	}
	press(m, key("tab"))
	if got := m.quick.Value(); got != "T-0071" {
		t.Fatalf("second Tab = %q", got)
	}
	press(m, shiftTab)
	if got := m.quick.Value(); got != "T-0070" {
		t.Fatalf("Shift+Tab = %q, want back to T-0070", got)
	}
	press(m, shiftTab) // wraps backwards; closed tasks (incl. reverted/done) come after the open ones
	if got := m.quick.Value(); got != "T-0001" {
		t.Fatalf("wrapped Shift+Tab = %q", got)
	}
	if got := m.comp.cands; strings.Join(got, " ") != "T-0070 T-0071 T-0072 T-0004 T-0003 T-0002 T-0001" {
		t.Fatalf("candidate order = %v", got)
	}
	if h := hintText(m); !strings.Contains(h, "T-0001") || !strings.Contains(h, "T-0003") {
		t.Errorf("hint = %q", h)
	}
}

func TestTabOnlyReplacesTheWordUnderTheCursor(t *testing.T) {
	m := completeModel()
	press(m, key("please look at t-0072 now"))
	for range 4 { // move before " now"
		press(m, key("left"))
	}
	press(m, key("tab"))
	if got := m.quick.Value(); got != "please look at T-0072 now" {
		t.Fatalf("value = %q", got)
	}
	// bare digits complete to task ids too
	m2 := completeModel()
	press(m2, key("see 72"), key("tab"))
	if got := m2.quick.Value(); got != "see T-0072" {
		t.Fatalf("digits: %q", got)
	}
}

// AC 2
func TestTabCompletesBranchesAndAgents(t *testing.T) {
	m := completeModel()
	press(m, key("slai/eng"), key("tab"))
	if got := m.quick.Value(); !strings.HasPrefix(got, "slai/eng-") {
		t.Fatalf("branch = %q", got)
	}
	if got := m.quick.Value(); got != "slai/eng-2/T-0071" && got != "slai/eng-1/T-0070" {
		t.Errorf("not an open task branch: %q", got)
	}
	m = completeModel()
	press(m, key("pri"), key("tab"))
	if got := m.quick.Value(); got != "principal" {
		t.Fatalf("agent = %q", got)
	}
	m = completeModel()
	press(m, key("ask @eng"), key("tab"), key("tab"))
	if got := m.quick.Value(); got != "ask eng-2" {
		t.Fatalf("@ drops the @: %q", got)
	}
}

// AC 3
func TestTabWithNoCandidatesLeavesTextAndSaysSo(t *testing.T) {
	m := completeModel()
	press(m, key("zzz"), key("tab"))
	if got := m.quick.Value(); got != "zzz" {
		t.Fatalf("text changed to %q", got)
	}
	if h := hintText(m); h != "no matches" {
		t.Fatalf("hint = %q", h)
	}
	if !strings.Contains(ansi.Strip(m.View()), "no matches") {
		t.Error("the box does not show the hint")
	}
	press(m, key("x")) // typing clears the hint
	if hintText(m) != "" {
		t.Error("hint outlived the next key")
	}
}

// AC 4
func TestEscDuringCompletionRestoresTheTypedWord(t *testing.T) {
	m := completeModel()
	press(m, key("look at T-00"), key("tab"), key("tab"))
	press(m, key("esc"))
	if got := m.quick.Value(); got != "look at T-00" {
		t.Fatalf("value = %q", got)
	}
	if m.mode != mQuick {
		t.Fatal("first Esc closed the box instead of cancelling the completion")
	}
	if hintText(m) != "" {
		t.Error("hint stayed after Esc")
	}
	press(m, key("esc"))
	if m.mode == mQuick {
		t.Fatal("second Esc should close the box")
	}
}

func TestHintShowsAtMostFiveAndHighlightsTheSelected(t *testing.T) {
	m := completeModel()
	m.tasks = nil
	for i := 1; i <= 9; i++ {
		m.tasks = append(m.tasks, store.Task{ID: "T-000" + string(rune('0'+i)), Status: "assigned"})
	}
	press(m, key("T-"), key("tab"), key("tab"), key("tab"), key("tab"), key("tab"), key("tab"))
	h := hintText(m)
	if n := strings.Count(h, "T-000"); n != 5 {
		t.Fatalf("hint shows %d candidates, want 5: %q", n, h)
	}
	if !strings.Contains(h, "T-0007") || !strings.Contains(m.completionHint(), styleSelected.Render("T-0007")) {
		t.Errorf("selected not highlighted/visible: %q", h)
	}
}

func TestEnterAfterCompletionSendsTheCompletedText(t *testing.T) {
	m := completeModel()
	press(m, key("re T-0072"), key("tab"))
	if m.comp.active == false {
		t.Fatal("no completion")
	}
	press(m, key("x"))
	if m.comp.active {
		t.Error("typing should end the completion")
	}
}

// AC 5
func TestTabOutsideTheSendBoxIsUnchanged(t *testing.T) {
	m := completeModel()
	m.mode = mList
	m.quick.Blur()
	m.quick.SetValue("T-00")
	sel := m.sel
	press(m, key("tab"))
	if m.comp.active || m.mode != mList || m.sel != sel || m.quick.Value() != "T-00" {
		t.Errorf("Tab in the list changed state: comp=%v mode=%v sel=%s", m.comp, m.mode, m.sel)
	}
	// focus mode still forwards Tab to the pane
	if ks, _, ok := translateKey(tea.KeyMsg{Type: tea.KeyTab}); !ok || len(ks) != 1 {
		t.Errorf("focus mode no longer forwards Tab: %v %v", ks, ok)
	}
	// the hold prompt is a name, not a message: Tab does nothing there
	h := completeModel()
	h.quickHold = true
	press(h, key("pri"), key("tab"))
	if h.quick.Value() != "pri" {
		t.Errorf("hold prompt completed: %q", h.quick.Value())
	}
}
