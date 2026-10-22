package supervisor

import (
	"context"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func wokenAgents(s *Supervisor) map[string]bool {
	out := map[string]bool{}
	evs, _ := s.Store.RecentEvents(500)
	for _, e := range evs {
		if e.Kind == "wake" {
			out[e.Agent] = true
		}
	}
	return out
}

// One free slot, two agents owed work: the one owed the higher-priority task is woken.
// With no priorities set, team order decides, exactly as before.
func TestWakePrefersHigherPriorityUnderSlotLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p1, p2 int // priorities of eng-1's and eng-2's tasks
		want   string
	}{{"default order", 0, 0, "eng-1"}, {"priority wins", 0, 1, "eng-2"}, {"lowered task yields", -1, 0, "eng-2"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := healthEnv(t)
			ctx := context.Background()
			s.Team.Defaults.MaxConcurrentAgents = 1
			for _, id := range []string{"eng-1", "eng-2"} {
				tk, _ := s.Store.CreateTask(store.Task{Title: id, Assignee: id, CreatedBy: "orch", Status: store.StatusAssigned})
				_, _ = s.Store.AddMessage(store.Message{From: "orch", To: id, Kind: "assignment", TaskID: tk.ID, Body: "go"})
				_ = s.Store.SetAgentStatus(id, "idle")
				p := tc.p1
				if id == "eng-2" {
					p = tc.p2
				}
				if p != 0 {
					_ = s.Store.SetTaskPriority(tk.ID, p)
				}
			}
			s.wakeTick(ctx)
			got := wokenAgents(s)
			if len(got) != 1 || !got[tc.want] {
				t.Fatalf("woken = %v, want only %s", got, tc.want)
			}
		})
	}
}
