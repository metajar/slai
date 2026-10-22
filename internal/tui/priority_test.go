package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/oorbt/slai/internal/store"
)

func TestBoardRowsSortByPriorityThenID(t *testing.T) {
	tasks := []store.Task{
		{ID: "T-0001", Status: "assigned"},
		{ID: "T-0002", Status: "assigned", Priority: 2},
		{ID: "T-0003", Status: "assigned", Priority: -1},
		{ID: "T-0004", Status: "assigned", Priority: 2},
		{ID: "T-0005", Status: "in_review", Priority: -5}, // groups still come first
	}
	var got []string
	for _, r := range boardRows(tasks) {
		got = append(got, r.ID)
	}
	if want := "T-0005 T-0002 T-0004 T-0001 T-0003"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	// all equal: the old id order, untouched
	same := []store.Task{{ID: "T-0003", Status: "assigned"}, {ID: "T-0001", Status: "assigned"}, {ID: "T-0002", Status: "assigned"}}
	got = nil
	for _, r := range boardRows(same) {
		got = append(got, r.ID)
	}
	if strings.Join(got, " ") != "T-0001 T-0002 T-0003" {
		t.Fatalf("equal priorities reordered: %v", got)
	}
}

// Golden: the Tasks view shows a priority column on every row.
func TestTasksViewShowsPriorityColumn(t *testing.T) {
	tasks := []store.Task{
		{ID: "T-0001", Status: "in_progress", Assignee: "eng-1", Title: "Plain"},
		{ID: "T-0002", Status: "in_progress", Assignee: "eng-2", Title: "Urgent", Priority: 3},
		{ID: "T-0003", Status: "in_progress", Assignee: "eng-1", Title: "Later", Priority: -1},
	}
	var got []string
	for _, l := range renderTasks(tasks, 0, 80, 10) {
		got = append(got, strings.TrimRight(ansi.Strip(l), " "))
	}
	want := []string{
		"IN PROGRESS (3)",
		"  T-0002 in_progress       p3   eng-2      Urgent",
		"  T-0001 in_progress       p0   eng-1      Plain",
		"  T-0003 in_progress       p-1  eng-1      Later",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func priorityModel(t *testing.T) (*Model, *store.Store, *[]string) {
	m, s := newModel(t)
	for _, title := range []string{"Second", "Third"} {
		_, _ = s.CreateTask(store.Task{Title: title, Assignee: "eng-1", CreatedBy: "orch", Branch: "b"})
	}
	var calls []string
	m.opts.SetPriority = func(id, spec string) (int, error) { // stands in for the supervisor
		calls = append(calls, id+" "+spec)
		return 1, nil
	}
	m.Update(m.refreshCmd()())
	m.Update(key("b"))
	if m.ov != ovTasks {
		t.Fatal("tasks view not open")
	}
	return m, s, &calls
}

func TestTasksViewPriorityKeysGoThroughTheCallback(t *testing.T) {
	m, s, calls := priorityModel(t)
	cur, _ := m.currentTask()
	for key_, spec := range map[string]string{"+": "up", "-": "down", "T": "top"} {
		*calls = nil
		_, cmd := m.Update(key(key_))
		if cmd == nil {
			t.Fatalf("%s: no command", key_)
		}
		msg := cmd()
		am, ok := msg.(actionMsg)
		if !ok || am.err != nil || am.followTask != cur.ID {
			t.Fatalf("%s: msg = %#v", key_, msg)
		}
		if len(*calls) != 1 || (*calls)[0] != cur.ID+" "+spec {
			t.Fatalf("%s: calls = %v", key_, *calls)
		}
	}
	// the TUI itself never wrote the store: the stub did nothing, so nothing changed
	if got, _ := s.GetTask(cur.ID); got.Priority != 0 {
		t.Fatalf("the TUI wrote priority %d to the store", got.Priority)
	}
}

func TestTasksViewCursorFollowsTheMovedTask(t *testing.T) {
	m, s, _ := priorityModel(t)
	m.tasksCur = 0
	first, _ := m.currentTask()
	_ = s.SetTaskPriority("T-0003", 5) // what the supervisor would have written
	m.Update(actionMsg{text: "ok", followTask: "T-0003"})
	m.Update(m.refreshCmd()())
	if cur, _ := m.currentTask(); cur.ID != "T-0003" || first.ID == "T-0003" {
		t.Fatalf("cursor on %s (was %s), want T-0003", cur.ID, first.ID)
	}
	if rows := boardRows(m.tasks); rows[0].ID != "T-0003" {
		t.Fatalf("T-0003 not first: %v", rows)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "p5") {
		t.Fatalf("view lacks the new priority:\n%s", v)
	}
}

func TestTasksViewPriorityWithoutSupervisorSaysSo(t *testing.T) {
	m, _, _ := priorityModel(t)
	m.opts.SetPriority = nil
	_, cmd := m.Update(key("+"))
	if cmd != nil || !m.statusE || !strings.Contains(m.status, "priority") {
		t.Fatalf("status=%q err=%v", m.status, m.statusE)
	}
}
