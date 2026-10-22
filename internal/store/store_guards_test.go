package store

import (
	"path/filepath"
	"testing"
	"time"
)

// max_turns_per_task relies on SetAgentTask resetting the per-task counter.
func TestGuardSetAgentTaskResetsTurns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.UpsertAgent(Agent{ID: "e1", Role: "engineer"})
	s.SetAgentTask("e1", "T-0001")
	for i := 0; i < 5; i++ {
		s.BumpTurns("e1")
	}
	if a, _ := s.AgentByID("e1"); a.Turns != 5 || a.CurrentTask != "T-0001" {
		t.Fatalf("%+v", a)
	}
	if err := s.SetAgentTask("e1", "T-0002"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.AgentByID("e1"); a.Turns != 0 || a.CurrentTask != "T-0002" {
		t.Fatalf("turns not reset: %+v", a)
	}
}

// Spend caps read from usage: per-agent and daily sums are day-scoped.
func TestGuardSpendQueries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddUsage("a", 10, 10, 1.5)
	s.AddUsage("b", 10, 10, 0.5)
	s.AddUsage("a", 10, 10, 1.0)
	if v, _ := s.TodaySpend(); v != 3.0 {
		t.Fatalf("today %v", v)
	}
	per, _ := s.SpendByAgentToday()
	if per["a"] != 2.5 || per["b"] != 0.5 {
		t.Fatalf("%v", per)
	}
}

// MonthSpend counts from the first of the local month, not a rolling 30 days.
func TestMonthSpendBoundary(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := func(y int, m time.Month, d, h int) {
		s.now = func() time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.Local) }
	}
	at(2026, time.September, 30, 23)
	s.AddUsage("a", 1, 1, 5.0)
	at(2026, time.October, 1, 0)
	s.AddUsage("a", 1, 1, 2.0)
	at(2026, time.October, 15, 12)
	s.AddUsage("b", 1, 1, 1.0)
	if v, _ := s.MonthSpend(); v != 3.0 {
		t.Fatalf("october %v", v)
	}
}
