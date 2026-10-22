package store

import "testing"

func TestTaskTokensSumsPerTaskAndSkipsUnattributed(t *testing.T) {
	s, _ := open(t)
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng-1"})
	if err := s.UpsertAgent(Agent{ID: "eng-1", Role: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s.AddUsage("eng-1", 5, 5, 0.1) // no current task yet: unattributed
	if err := s.SetAgentTask("eng-1", a.ID); err != nil {
		t.Fatal(err)
	}
	s.AddUsage("eng-1", 100, 50, 0.2)
	s.AddUsage("eng-1", 10, 0, 0.1)
	got, err := s.TaskTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[a.ID] != 160 {
		t.Fatalf("TaskTokens = %v, want {%s: 160}", got, a.ID)
	}
}
