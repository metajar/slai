package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// boardEnv is a driver test whose store clock follows the fake clock, so task and agent
// timestamps move together with it.
func boardEnv(t *testing.T) (*Supervisor, *time.Time) {
	s, now := driverEnv(t)
	s.Store.SetClock(func() time.Time { return *now })
	return s, now
}

func advance(s *Supervisor, now *time.Time, d time.Duration) {
	*now = now.Add(d)
	_ = s.Store.SetAgentStatus("orch", "idle") // the orchestrator has been idle a while
	*now = now.Add(OrchIdleAfter + time.Second)
}

func boardChecks(t *testing.T, s *Supervisor) []string {
	t.Helper()
	var out []string
	for _, m := range mustUndelivered(t, s, "orch") {
		if strings.Contains(m.Body, "Board check") {
			out = append(out, m.Body)
		}
	}
	return out
}

// H-3: a busy owner's task is not "Need a decision" however old its row is, the lines say "no
// board change", never "idle", and carry the owner's liveness.
func TestBoardCheckSplitsBusyFromNeedsDecision(t *testing.T) {
	s, now := boardEnv(t)
	ctx := context.Background()
	busyTask, _ := s.Store.CreateTask(store.Task{Title: "long build", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	quiet, _ := s.Store.CreateTask(store.Task{Title: "silent work", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	advance(s, now, 0)
	s.driveTick(ctx)                 // first sight of the board
	*now = now.Add(30 * time.Minute) // both task rows are 30 minutes old
	_ = s.Store.SetAgentStatus("eng-1", "working")
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}") // eng-1 is mid-turn and active right now
	advance(s, now, DigestEvery+time.Minute)
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	_ = s.Store.SetAgentStatus("eng-1", "working")
	s.driveTick(ctx)
	checks := boardChecks(t, s)
	if len(checks) != 1 {
		t.Fatalf("board checks = %d, want 1", len(checks))
	}
	body := checks[0]
	i, j := strings.Index(body, "Need a decision"), strings.Index(body, "In progress, no action needed")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("digest sections:\n%s", body)
	}
	need, fine := body[i:j], body[j:]
	if !strings.Contains(need, quiet.ID) || strings.Contains(need, busyTask.ID) {
		t.Errorf("'Need a decision' should list %s and not the busy %s:\n%s", quiet.ID, busyTask.ID, need)
	}
	if !strings.Contains(fine, busyTask.ID) || !strings.Contains(fine, "eng-1 busy") {
		t.Errorf("busy task should sit under 'no action needed' with its owner's liveness:\n%s", fine)
	}
	if strings.Contains(body, ", idle ") || !strings.Contains(body, "no board change for") {
		t.Errorf("task ages must read 'no board change for N', not 'idle':\n%s", body)
	}
	if !strings.Contains(need, "NO ACTIVITY") {
		t.Errorf("a task quiet for 15+ min should be marked:\n%s", need)
	}
}

// H-3: when every open task's owner is busy and healthy there is nothing to decide, so no
// digest is sent (it would only cost the orchestrator a turn).
func TestNoBoardCheckWhenEveryOwnerIsBusy(t *testing.T) {
	s, now := boardEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "a", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_, _ = s.Store.CreateTask(store.Task{Title: "b", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	s.driveTick(ctx)
	for i := 0; i < 4; i++ {
		advance(s, now, DigestEvery+time.Minute)
		for _, id := range []string{"eng-1", "eng-2"} {
			_ = s.Store.SetAgentStatus(id, "working")
			_ = s.Store.AddEvent(id, "hook:pretooluse", "{}")
		}
		s.driveTick(ctx)
	}
	if got := boardChecks(t, s); len(got) != 0 {
		t.Fatalf("digest sent although every owner is busy:\n%v", got)
	}
}

// B3: a stalled task whose owner is not busy must get a recorded decision; without one the
// user is told after DecisionWithin; any qualifying orchestrator action clears it.
func TestIgnoredBoardCheckEscalatesToTheUser(t *testing.T) {
	for _, act := range []string{"", "update_task", "cancel_task", "create_task", "send_message", "ask_user"} {
		t.Run("decision="+act, func(t *testing.T) {
			s, now := boardEnv(t)
			ctx := context.Background()
			task, _ := s.Store.CreateTask(store.Task{Title: "silent work", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
			_ = s.Store.SetAgentStatus("eng-2", "idle")
			advance(s, now, 0)
			s.driveTick(ctx) // first sight of the board
			advance(s, now, StalledTaskAfter+time.Minute)
			s.driveTick(ctx)
			if len(boardChecks(t, s)) != 1 {
				t.Fatalf("no board check:\n%v", boardChecks(t, s))
			}
			if !strings.Contains(boardChecks(t, s)[0], "recorded decision") {
				t.Errorf("the board check should say a decision is expected:\n%s", boardChecks(t, s)[0])
			}
			switch act {
			case "update_task", "cancel_task":
				_ = s.Store.AddEvent("orch", act, `{"task_id":"`+task.ID+`"}`)
			case "create_task":
				_ = s.Store.AddEvent("orch", act, `{"title":"split of `+task.ID+`","assignee":"eng-1"}`)
			case "send_message":
				_ = s.Store.AddEvent("orch", act, `{"to":"eng-2","body":"status?"}`)
			case "ask_user":
				_ = s.Store.AddEvent("orch", act, `{"question":"which way?"}`)
			}
			*now = now.Add(DecisionWithin + time.Second)
			s.driveTick(ctx)
			_, escalated := openEsc(t, s, EscNoDecision, "orch")
			if act == "" {
				if !openEscUser(t, s, EscNoDecision, "orch") {
					t.Fatal("no attention item for an ignored board check")
				}
				// it closes when the task moves on
				_ = s.Store.UpdateTaskStatus(task.ID, store.StatusInReview)
				s.driveTick(ctx)
				if _, still := openEsc(t, s, EscNoDecision, "orch"); still {
					t.Fatal("attention item not cleared once the task moved")
				}
				return
			}
			if escalated {
				t.Fatalf("user told although the orchestrator did %s", act)
			}
			if !hasEvent(s, "board_decision") {
				t.Fatal("decision not recorded")
			}
		})
	}
}

// A decision about another task or another owner does not count.
func TestUnrelatedOrchestratorActionIsNotADecision(t *testing.T) {
	s, now := boardEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "silent work", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	advance(s, now, 0)
	s.driveTick(ctx)
	advance(s, now, StalledTaskAfter+time.Minute)
	s.driveTick(ctx)
	_ = s.Store.AddEvent("orch", "update_task", `{"task_id":"T-9999"}`)
	_ = s.Store.AddEvent("orch", "send_message", `{"to":"eng-1","body":"hi"}`)
	*now = now.Add(DecisionWithin + time.Second)
	s.driveTick(ctx)
	if !openEscUser(t, s, EscNoDecision, "orch") {
		t.Fatalf("unrelated actions counted as a decision on %s", task.ID)
	}
}
