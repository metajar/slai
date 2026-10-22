package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// AC2: with zero tasks the hint names the configured orchestrator; a task removes it.
func TestFirstRunHintNamesOrchestratorUntilATaskExists(t *testing.T) {
	m, _ := newModel(t)
	orch := m.orchestratorID()
	if orch == "" {
		t.Fatal("no orchestrator in the test team")
	}
	if strings.Contains(screen(m), "Type your goal") {
		t.Fatal("hint shown although a task exists")
	}
	// fresh board: no tasks
	m2 := New(Options{Project: "demo", Dir: t.TempDir(), Team: testTeam(), Store: openStore(t)})
	for _, a := range testTeam().Agents {
		_ = m2.opts.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role, Model: a.Model, Status: "idle"})
	}
	m2.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m2.Update(m2.refreshCmd()())
	v := screen(m2)
	if !strings.Contains(v, "Type your goal to "+orch) {
		t.Fatalf("hint missing or names the wrong agent (want %q):\n%s", orch, v)
	}
	_, _ = m2.opts.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: orch, Branch: "b"})
	m2.Update(m2.refreshCmd()())
	if strings.Contains(screen(m2), "Type your goal") {
		t.Fatal("hint must disappear once a task exists")
	}
}

// AC3: with no agent under the cursor (a group header), the quick prompt addresses the orchestrator; so does `o`.
func TestQuickPromptWithoutAgentTargetsOrchestrator(t *testing.T) {
	m, _ := newModel(t)
	orch := m.orchestratorID()
	for i, it := range m.items {
		if it.isHeader() {
			m.cur = i
			break
		}
	}
	m.Update(key(" "))
	if m.mode != mQuick || m.quickTo != orch {
		t.Fatalf("space on a header: mode=%v quickTo=%q want %q", m.mode, m.quickTo, orch)
	}
	m.Update(key("esc"))
	m.cursorTo("eng-1")
	m.Update(key("o"))
	if m.mode != mQuick || m.quickTo != orch {
		t.Fatalf("o: mode=%v quickTo=%q want %q", m.mode, m.quickTo, orch)
	}
}
