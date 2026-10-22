package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func testTeam() *config.Team {
	return &config.Team{
		Project: config.Project{Name: "demo"},
		Agents: []config.Agent{
			{ID: "orch", Role: "orchestrator", Model: "opus"},
			{ID: "pm", Role: "product_manager", Model: "sonnet"},
			{ID: "eng-1", Role: "engineer", Model: "sonnet"},
			{ID: "eng-2", Role: "engineer", Model: "sonnet"},
		},
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "ctrl+q":
		return tea.KeyMsg{Type: tea.KeyCtrlQ}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestOrderAndGroups(t *testing.T) {
	stored := []store.Agent{
		{ID: "orch", Status: "idle"}, {ID: "pm", Status: "finished"},
		{ID: "eng-1", Status: "working"}, {ID: "eng-2", Status: "waiting_on_user"},
	}
	got := orderAgents(testTeam(), stored)
	if got[0].ID != "orch" || got[3].ID != "eng-2" {
		t.Fatalf("team order should be preserved: %+v", got)
	}
	for st, g := range map[string]string{"idle": "●", "working": "●", "waiting": "▲", "waiting_on_user": "▲", "finished": "◆", "review": "◆", "dead": "○"} {
		if glyph(st) != g {
			t.Errorf("glyph(%s)=%s", st, glyph(st))
		}
	}
	items := buildItems(got, nil)
	var seq []string
	for _, it := range items {
		if it.isHeader() {
			seq = append(seq, "["+it.title+"]")
		} else {
			seq = append(seq, it.agent)
		}
	}
	// waiting agent sorts first inside its group
	want := "[Orchestrator] orch [Planning] pm [Engineers] eng-2 eng-1"
	if strings.Join(seq, " ") != want {
		t.Fatalf("items = %v, want %s", seq, want)
	}
	if !items[4].isHeader() || items[4].needs != 1 {
		t.Fatalf("engineers header should count 1 needing you: %+v", items[4])
	}
	// folding hides the agents but keeps the header
	folded := buildItems(got, map[string]bool{"engineers": true})
	if len(folded) != 5 || !folded[4].folded {
		t.Fatalf("fold: %+v", folded)
	}
	lines := renderList(items, 1, 30, got, nil, nil, true)
	out := ansi.Strip(joinLines(lines))
	for _, w := range []string{"Orchestrator", "▾ Engineers (2)", "▲ 1 needs you", "needs you", "opus", "sonnet", "idle"} {
		if !strings.Contains(out, w) {
			t.Errorf("list missing %q in:\n%s", w, out)
		}
	}
	// missing from store => dead
	if orderAgents(testTeam(), nil)[0].Status != "dead" {
		t.Fatal("expected dead")
	}
}

func joinLines(ls []listLine) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.text)
	}
	return strings.Join(s, "\n")
}

func TestActivityLine(t *testing.T) {
	tasks := []store.Task{{ID: "T-0004", Title: "Add webhook"}}
	if got := agentActivity(store.Agent{ID: "e", CurrentTask: "T-0004", Status: "working"}, tasks, nil); got != "T-0004 Add webhook" {
		t.Fatal(got)
	}
	ev := []store.Event{{Agent: "e", Kind: "hook:pretooluse", Data: `{"tool":"Bash"}`}}
	if got := agentActivity(store.Agent{ID: "e", Status: "working"}, nil, ev); got != "last: Bash" {
		t.Fatal(got)
	}
	if got := agentActivity(store.Agent{ID: "e", Status: "idle"}, nil, nil); got != "idle" {
		t.Fatal(got)
	}
	if shortModel("claude-fable-5-1") != "fable" || shortModel("Opus") != "opus" {
		t.Fatal("shortModel")
	}
}

func TestHeaderCounts(t *testing.T) {
	a := []store.Agent{{Status: "working"}, {Status: "working"}, {Status: "waiting"}, {Status: "idle"}, {Status: "dead"}}
	got := headerRight(a, 3.42, true)
	for _, s := range []string{"5 agents", "2 working", "1 needs you", "1 dead", "~$3.42 today", "PAUSED"} {
		if !strings.Contains(got, s) {
			t.Errorf("%q missing in %q", s, got)
		}
	}
}

func TestBellOnce(t *testing.T) {
	ag := []store.Agent{{ID: "a", Status: "waiting"}, {ID: "b", Status: "idle"}}
	n, ring := newlyWaiting(map[string]bool{}, ag)
	if len(ring) != 1 || ring[0] != "a" {
		t.Fatal(ring)
	}
	_, ring = newlyWaiting(n, ag)
	if len(ring) != 0 {
		t.Fatal("rang twice")
	}
	n, _ = newlyWaiting(n, []store.Agent{{ID: "a", Status: "idle"}})
	_, ring = newlyWaiting(n, ag)
	if len(ring) != 1 {
		t.Fatal("should ring again after leaving waiting")
	}
}

func TestBoardGroupingAndLog(t *testing.T) {
	tasks := []store.Task{
		{ID: "T-0001", Status: "merged", Title: "m"},
		{ID: "T-0004", Status: "in_progress", Assignee: "eng-1", Title: "Stripe webhook"},
		{ID: "T-0003", Status: "in_review", Assignee: "eng-2", Title: "Invoice list UI", ReviewRounds: 1},
	}
	rows := boardRows(tasks)
	if rows[0].ID != "T-0003" || rows[1].ID != "T-0004" || rows[2].ID != "T-0001" {
		t.Fatalf("%+v", rows)
	}
	if l := boardLine(rows[0]); !strings.Contains(l, "r1") || !strings.Contains(l, "in_review") {
		t.Fatal(l)
	}
	e := store.Event{Agent: "eng-1", Kind: "request_review", Data: `{"to":"principal","task_id":"T-0004"}`}
	if got := formatEvent(e); got != "eng-1 → principal: request_review T-0004" {
		t.Fatal(got)
	}
	if got := formatEvent(store.Event{Agent: "pm", Kind: "status", Data: "{}"}); got != "pm: status" {
		t.Fatal(got)
	}
}

func TestFitPreview(t *testing.T) {
	body := "a\n\x1b[31mredredred\x1b[0m\nlast\n\n\n"
	l := fitPreview(body, 5, 2)
	if len(l) != 2 || !strings.Contains(l[0], "\x1b[31m") || strings.Contains(l[0], "redredred") {
		t.Fatalf("%q", l)
	}
}

func TestBoardLineShowsWaitsOn(t *testing.T) {
	dep := store.Task{ID: "T-0001", Status: store.StatusInProgress}
	done := store.Task{ID: "T-0002", Status: store.StatusMerged}
	w := store.Task{ID: "T-0003", Title: "later", Status: store.StatusBlocked, DependsOn: []string{"T-0001", "T-0002"}}
	lines := renderTasks([]store.Task{dep, done, w}, 0, 120, 20)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "waits on T-0001") || strings.Contains(joined, "waits on T-0001, T-0002") {
		t.Fatalf("only unmerged deps should show:\n%s", joined)
	}
}
