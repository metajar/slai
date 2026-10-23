package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/usage"
)

func autoTeam(auto bool) *config.Team {
	t := team(5)
	t.Defaults.ContextReset = config.ContextReset{Auto: auto, ThresholdTokens: 100000}
	return t
}

func bigContext(t *testing.T, s *Supervisor, id string, tokens int64) {
	t.Helper()
	if err := usage.SetContext(s.Store, id, tokens); err != nil {
		t.Fatal(err)
	}
}

func autoQueued(s *Supervisor, id string) (service.ResetReq, bool) {
	return service.LoadReset(s.Store, id)
}

// A7: off by default, nothing happens however big the context is.
func TestAutoResetOffByDefault(t *testing.T) {
	s := tmuxEnv(t, autoTeam(false))
	bigContext(t, s, "eng-1", 900000)
	s.autoResetTick(context.Background())
	if _, ok := autoQueued(s, "eng-1"); ok {
		t.Fatal("auto reset ran with auto off")
	}
	if len(eventsOf(s, "auto_reset_queued")) != 0 {
		t.Fatal("event recorded with auto off")
	}
}

// A7 + A8 (30 min): on, over the threshold, idle and owing nothing: reset once, not again
// until AutoResetEvery has passed.
func TestAutoResetOnceThenNotForThirtyMinutes(t *testing.T) {
	s := tmuxEnv(t, autoTeam(true))
	ctx := context.Background()
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	// not over the threshold (or exactly at it): nothing
	for _, n := range []int64{50000, 100000} {
		bigContext(t, s, "eng-1", n)
		s.autoResetTick(ctx)
		if _, ok := autoQueued(s, "eng-1"); ok {
			t.Fatalf("queued at %d tokens", n)
		}
	}
	bigContext(t, s, "eng-1", 100001)
	s.autoResetTick(ctx)
	req, ok := autoQueued(s, "eng-1")
	if !ok || req.Trigger != service.TriggerAuto || req.Requester != "" || req.Reason == "" {
		t.Fatalf("request: %+v %v", req, ok)
	}
	if _, queued := autoQueued(s, "eng-2"); queued {
		t.Fatal("eng-2 has a small context but was queued")
	}
	if len(eventsOf(s, "auto_reset_queued")) != 1 {
		t.Fatalf("events: %d", len(eventsOf(s, "auto_reset_queued")))
	}
	before := sessionID(t, s, "eng-1")
	s.resetTick(ctx) // the engine carries it out as any reset
	if sessionID(t, s, "eng-1") == before {
		t.Fatal("the reset engine did not restart the agent")
	}
	startHook(t, s, "eng-1")
	if ev := eventsOf(s, "context_reset"); len(ev) != 1 || !containsAll(ev[0].Data, `"trigger":"auto"`) {
		t.Fatalf("context_reset: %+v", ev)
	}
	s.Store.SetKV(service.KVWhoami+"eng-1", itoa64(clock.Unix()+1))
	s.resetTick(ctx) // cleared
	// still over the threshold (the new session has not taken a turn yet): the guard holds
	s.Store.SetAgentStatus("eng-1", "idle")
	s.Store.MarkDelivered(allUnread(s, "eng-1")...)
	clock = clock.Add(AutoResetEvery - time.Minute)
	s.autoResetTick(ctx)
	if _, ok := autoQueued(s, "eng-1"); ok {
		t.Fatal("reset again inside the 30 minute guard")
	}
	clock = clock.Add(2 * time.Minute)
	s.autoResetTick(ctx)
	if _, ok := autoQueued(s, "eng-1"); !ok {
		t.Fatal("not reset again after the guard expired")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func allUnread(s *Supervisor, id string) []int64 {
	var ids []int64
	m, _ := s.Store.UndeliveredFor(id)
	for _, x := range m {
		ids = append(ids, x.ID)
	}
	return ids
}

// A8: each guard on its own keeps an over-threshold, otherwise eligible agent from being reset.
func TestAutoResetGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Supervisor)
	}{
		{"busy", func(t *testing.T, s *Supervisor) { s.Store.SetAgentStatus("eng-1", "working") }},
		{"owes a task", func(t *testing.T, s *Supervisor) {
			s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
		}},
		{"unread inbox", func(t *testing.T, s *Supervisor) {
			s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "hi"})
		}},
		{"paused agent", func(t *testing.T, s *Supervisor) { s.Store.SetAgentPaused("eng-1", true) }},
		{"team paused", func(t *testing.T, s *Supervisor) { s.Pause() }},
		{"team hold", func(t *testing.T, s *Supervisor) { s.HoldTeam("overnight") }},
		{"stopped on purpose", func(t *testing.T, s *Supervisor) { s.Store.SetKV(kvStopped+"eng-1", "1") }},
		{"spend capped", func(t *testing.T, s *Supervisor) {
			s.Team.Defaults.Caps.PerAgentUSD = 1
			s.Store.AddUsage("eng-1", 10, 10, 5)
		}},
		{"performing a review", func(t *testing.T, s *Supervisor) {
			tk, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInReview})
			s.Store.SetAgentTask("eng-1", tk.ID) // review_diff points the reviewer at the task
		}},
		{"a reset already pending", func(t *testing.T, s *Supervisor) {
			queueReset(t, s, "eng-1", service.TriggerUser, "")
		}},
		{"reset recently", func(t *testing.T, s *Supervisor) {
			s.Store.SetKV(kvAutoResetAt+"eng-1", itoa64(time.Now().Unix()))
		}},
		{"no live session", func(t *testing.T, s *Supervisor) {
			s.Tmux.KillSession(context.Background(), s.Session("eng-1"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmuxEnv(t, autoTeam(true))
			bigContext(t, s, "eng-1", 500000)
			tc.setup(t, s)
			pending := func() int {
				_, ok := service.LoadReset(s.Store, "eng-1")
				return len(eventsOf(s, "auto_reset_queued")) + map[bool]int{true: 1}[ok]
			}
			base := pending()
			s.autoResetTick(context.Background())
			if got := pending(); got != base {
				t.Fatalf("auto reset fired despite the guard (%d -> %d)", base, got)
			}
		})
	}
}

// The orchestrator is never reset automatically (A11: the CLI only).
func TestAutoResetNeverTouchesTheOrchestrator(t *testing.T) {
	s := tmuxEnv(t, autoTeam(true))
	bigContext(t, s, "orch", 900000)
	s.autoResetTick(context.Background())
	if _, ok := service.LoadReset(s.Store, "orch"); ok {
		t.Fatal("orchestrator queued for an automatic reset")
	}
}
