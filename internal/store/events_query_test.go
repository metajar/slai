package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func openT(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestQueryEventsFilters(t *testing.T) {
	s := openT(t)
	s.AddEvent("eng-1", "request_review", `{"task_id":"T-0001","summary":"100% sure_thing"}`)
	s.AddEvent("eng-2", "report", `{"task":"T-0002"}`)
	s.AddEvent("", "wake", `{"count":1}`)
	s.AddEvent("eng-1", "hook:stop", `{}`)
	s.AddEvent("eng-1", "usage:mark", `not json`)
	count := func(f EventFilter) int {
		_, n, err := s.QueryEvents(f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(EventFilter{}); got != 3 {
		t.Errorf("noise hidden by default: %d, want 3", got)
	}
	if got := count(EventFilter{Noise: true}); got != 5 {
		t.Errorf("with noise %d, want 5", got)
	}
	if got := count(EventFilter{HasAgent: true, Agent: ""}); got != 1 {
		t.Errorf("supervisor events %d", got)
	}
	if count(EventFilter{HasAgent: true, Agent: "eng-1"}) != 1 || count(EventFilter{Kind: "report"}) != 1 {
		t.Error("agent / kind filters")
	}
	if count(EventFilter{Task: "T-0001"}) != 1 || count(EventFilter{Task: "T-0002"}) != 1 || count(EventFilter{Task: "T-9"}) != 0 {
		t.Error("task filter matches data.task_id and data.task")
	}
	if count(EventFilter{Text: "100% sure_"}) != 1 || count(EventFilter{Text: "%"}) != 1 || count(EventFilter{Text: "_"}) != 1 || count(EventFilter{Text: "SURE"}) != 1 {
		t.Error("text search is literal and case-insensitive")
	}
	// a malformed data blob does not break the task filter
	if count(EventFilter{Task: "T-0001", Noise: true}) != 1 {
		t.Error("non-JSON data broke the task filter")
	}
}

func TestQueryEventsPagesNewestFirst(t *testing.T) {
	s := openT(t)
	for i := 0; i < 250; i++ {
		s.AddEvent("a", "k", fmt.Sprintf(`{"n":%d}`, i))
	}
	evs, total, err := s.QueryEvents(EventFilter{Limit: 100})
	if err != nil || total != 250 || len(evs) != 100 || evs[0].Data != `{"n":249}` {
		t.Fatalf("page 1: %v %d %d", err, total, len(evs))
	}
	evs, _, _ = s.QueryEvents(EventFilter{Limit: 100, Offset: 200})
	if len(evs) != 50 || evs[49].Data != `{"n":0}` {
		t.Fatalf("last page: %d", len(evs))
	}
	if ev, _, _ := s.QueryEvents(EventFilter{}); len(ev) != 100 {
		t.Errorf("default limit %d", len(ev))
	}
	as, _ := s.EventAgents()
	ks, _ := s.EventKinds()
	if len(as) != 1 || as[0] != "a" || len(ks) != 1 || ks[0] != "k" {
		t.Errorf("menus %v %v", as, ks)
	}
}

func TestUsageByTask(t *testing.T) {
	s := openT(t)
	tk, _ := s.CreateTask(Task{Title: "t", Assignee: "eng-1", Status: StatusInProgress})
	s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer"})
	s.SetAgentTask("eng-1", tk.ID)
	s.AddUsage("eng-1", 100, 50, 1.25)
	s.AddUsage("eng-1", 200, 25, 0.75)
	s.SetAgentTask("eng-1", "")
	s.AddUsage("eng-1", 1, 1, 9) // no task: not charged to one
	got, err := s.UsageByTask(0)
	if err != nil || len(got) != 1 || got[tk.ID] != (TaskUsage{CostUSD: 2, Tokens: 375, Rows: 2}) {
		t.Fatalf("%v %v", got, err)
	}
	if later, _ := s.UsageByTask(1 << 40); len(later) != 0 {
		t.Errorf("since filter: %v", later)
	}
}
