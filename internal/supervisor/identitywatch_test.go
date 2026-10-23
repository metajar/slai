package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// C7: the orchestrator's identity line rides on every Nth board check (default 10).
func TestOrchestratorGetsIdentityLineOnTheTenthBoardCheck(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	if _, err := s.Store.CreateTask(store.Task{Title: "feature", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	var checks []string
	for i := 0; i < 20 && len(checks) < 10; i++ { // the first tick only records the board
		_ = s.Store.SetAgentStatus("orch", "idle")
		*now = now.Add(DigestEvery + OrchIdleAfter + 2e9)
		_ = s.Store.SetKV(kvIgnored+"orch", "0") // an orchestrator that answers every check
		s.driveTick(ctx)
		for _, m := range mustUndelivered(t, s, "orch") {
			if strings.Contains(m.Body, "Board check") {
				checks = append(checks, m.Body)
			}
			_ = s.Store.MarkDelivered(m.ID)
		}
	}
	if len(checks) != 10 {
		t.Fatalf("got %d board checks, want 10", len(checks))
	}
	for i, b := range checks {
		has := strings.HasPrefix(b, "You are orch, the orchestrator")
		if has != (i == 9) {
			t.Errorf("board check %d identity line = %v, want %v:\n%.120s", i+1, has, i == 9, b)
		}
	}
	if !strings.Contains(checks[9], "You never write code") {
		t.Errorf("orchestrator line lacks its role reminder: %.200s", checks[9])
	}
	if countEvents(s, "identity_reground") != 1 {
		t.Errorf("identity_reground events = %d, want 1", countEvents(s, "identity_reground"))
	}
}

func TestIdentityEveryBoardChecksIsConfigurable(t *testing.T) {
	s, _ := driverEnv(t)
	s.Team.Defaults.IdentityEveryBoardChecks = 2
	if got := s.withOrchIdentity("orch", "a"); got != "a" {
		t.Fatalf("1st check = %q", got)
	}
	if got := s.withOrchIdentity("orch", "b"); !strings.HasPrefix(got, "You are orch") {
		t.Fatalf("2nd check = %q", got)
	}
	s.Team.Defaults.IdentityEveryBoardChecks = -1
	for i := 0; i < 5; i++ {
		if got := s.withOrchIdentity("orch", "c"); got != "c" {
			t.Fatalf("disabled but got %q", got)
		}
	}
}
