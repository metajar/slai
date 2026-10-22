package supervisor

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// spendEnv is a unit supervisor on a fake clock whose store stamps usage with it, with
// eng-1 working T-0001 (assigned to it) and eng-2 working T-0002.
func spendEnv(t *testing.T, caps config.SpendCaps) (*Supervisor, *time.Time, string, string) {
	t.Helper()
	tm := team(5)
	tm.Defaults.Caps = caps
	s := unitEnv(t, tm)
	now := time.Unix(2_000_000, 0)
	s.Now = func() time.Time { return now }
	s.Store.SetClock(s.Now)
	var ids []string
	for _, id := range []string{"eng-1", "eng-2"} {
		if err := s.Store.UpsertAgent(store.Agent{ID: id, Role: "engineer", Status: "idle"}); err != nil {
			t.Fatal(err)
		}
		tk, err := s.Store.CreateTask(store.Task{Title: id, Assignee: id, Status: store.StatusInProgress})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Store.SetAgentTask(id, tk.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	return s, &now, ids[0], ids[1]
}

// AC1: 80% of per_task_usd warns the assignee and the orchestrator once, with a
// spend_warn event; 100% gates only that task's assignee (reason "task cap") and asks once.
func TestTaskCapWarnsOnceThenGatesOnlyTheAssignee(t *testing.T) {
	s, _, t1, _ := spendEnv(t, config.SpendCaps{PerTaskUSD: 10})
	s.Store.AddUsage("eng-1", 1, 1, 7.9)
	if g := s.checkSpend(); g.blocked("eng-1") || countEvents(s, "spend_warn") != 0 {
		t.Fatal("79% must not warn")
	}
	// spend by another agent on the same task counts toward it
	s.Store.SetAgentTask("eng-2", t1)
	s.Store.AddUsage("eng-2", 1, 1, 0.2) // 8.1
	s.Store.SetAgentTask("eng-2", "T-0002")
	for i := 0; i < 3; i++ {
		if g := s.checkSpend(); g.blocked("eng-1") {
			t.Fatal("80% must not gate")
		}
	}
	if n := countEvents(s, "spend_warn"); n != 1 {
		t.Fatalf("spend_warn events = %d, want exactly 1", n)
	}
	for _, to := range []string{"eng-1", "orch"} {
		if n := unreadFrom(t, s, to, t1); n != 1 {
			t.Fatalf("%s got %d warnings for %s, want 1", to, n, t1)
		}
	}
	s.Store.AddUsage("eng-1", 1, 1, 2) // 10.1
	g := s.checkSpend()
	if !g.blocked("eng-1") || g.blocked("eng-2") || g.blocked("orch") || g.run || g.daily {
		t.Fatalf("only eng-1 should be gated: %+v", g)
	}
	s.checkSpend()
	if countEvents(s, "spend_cap") != 1 || countEvents(s, "ask_user") != 1 {
		t.Fatalf("want one spend_cap and one ask_user, got %d / %d", countEvents(s, "spend_cap"), countEvents(s, "ask_user"))
	}
	// the board shows why: an agent that owes work is gated, reason "task cap"
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "x"})
	late := s.now().Add(30 * time.Minute) // silent long enough that it would read as stale
	h := service.AgentHealthGated(s.Store, "eng-1", true, late, service.GateConfigOf(s.Team))
	if h.Liveness != "gated" || h.Reason != "task cap" {
		t.Fatalf("board: %+v", h)
	}
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "x"})
	if h := service.AgentHealthGated(s.Store, "eng-2", true, late, service.GateConfigOf(s.Team)); h.Liveness == "gated" {
		t.Fatalf("eng-2 must not be gated: %+v", h)
	}
	// a finished task no longer gates its assignee
	if err := s.Store.UpdateTaskStatus(t1, store.StatusCancelled); err != nil {
		t.Fatal(err)
	}
	if g := s.checkSpend(); g.blocked("eng-1") {
		t.Fatal("cancelled task still gates")
	}
}

func unreadFrom(t *testing.T, s *Supervisor, to, task string) int {
	t.Helper()
	msgs, err := s.Store.PendingWake(0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.To == to && m.From == "slai" && m.TaskID == task {
			n++
		}
	}
	return n
}

// AC2: run spend crossing per_run_usd gates the whole team (wake_gate.spend_capped) with
// one ask_user; finish moves the baseline so the next run starts at zero.
func TestRunCapPausesTeamAndFinishResetsBaseline(t *testing.T) {
	s, now, _, _ := spendEnv(t, config.SpendCaps{PerRunUSD: 10})
	s.Store.SetKV(service.KVRunStart, strconv.FormatInt(now.Unix(), 10))
	*now = now.Add(time.Minute)
	s.Store.AddUsage("eng-1", 1, 1, 6)
	s.Store.AddUsage("eng-2", 1, 1, 4.5)
	g := s.checkSpend()
	if !g.run || !g.blocked("orch") || !g.blocked("eng-2") {
		t.Fatalf("run cap should gate everyone: %+v", g)
	}
	s.checkSpend()
	if countEvents(s, "spend_cap") != 1 || countEvents(s, "ask_user") != 1 {
		t.Fatalf("want one spend_cap and one ask_user, got %d / %d", countEvents(s, "spend_cap"), countEvents(s, "ask_user"))
	}
	w := service.ReadWakeGate(s.Store, service.GateConfigOf(s.Team), nil, s.now())
	if !w.SpendCapped {
		t.Fatalf("wake_gate.spend_capped not set: %+v", w)
	}
	// finish resets the baseline (it needs the open tasks closed first)
	for _, id := range []string{"T-0001", "T-0002"} {
		if err := s.Store.UpdateTaskStatus(id, store.StatusCancelled); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Minute)
	svc := &service.Service{Store: s.Store, Team: s.Team, Dir: s.Dir, Now: s.Now}
	if _, err := svc.Finish(context.Background(), "orch", service.FinishArgs{Summary: "all done"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if g := s.checkSpend(); g.run || g.blocked("orch") {
		t.Fatalf("finish should reset the run baseline: %+v", g)
	}
	s.Store.AddUsage("eng-1", 1, 1, 8.5) // 85% of the new run
	if g := s.checkSpend(); g.run || countEvents(s, "spend_warn") != 1 {
		t.Fatalf("new run spend should warn only: run=%v warns=%d", g.run, countEvents(s, "spend_warn"))
	}
}

// AC3: raising the cap in team.yaml lifts the gate on the next tick.
func TestRaisingTaskAndRunCapsLiftsTheGate(t *testing.T) {
	s, now, _, _ := spendEnv(t, config.SpendCaps{PerTaskUSD: 5, PerRunUSD: 8})
	s.Store.SetKV(service.KVRunStart, strconv.FormatInt(now.Unix(), 10))
	*now = now.Add(time.Minute)
	s.Store.AddUsage("eng-1", 1, 1, 6)
	s.Store.AddUsage("eng-2", 1, 1, 3)
	g := s.checkSpend()
	if !g.run || !g.blocked("eng-1") {
		t.Fatalf("setup: %+v", g)
	}
	s.Team.Defaults.Caps.PerRunUSD = 20 // hot reload swaps the team in place
	g = s.checkSpend()
	if g.run || g.blocked("orch") || !g.blocked("eng-1") {
		t.Fatalf("raising the run cap should free everyone but the task-capped eng-1: %+v", g)
	}
	s.Team.Defaults.Caps.PerTaskUSD = 50
	if g = s.checkSpend(); g.blocked("eng-1") {
		t.Fatalf("raising the task cap should free eng-1: %+v", g)
	}
	// crossing the new, higher cap asks again
	s.Store.AddUsage("eng-1", 1, 1, 50)
	if g = s.checkSpend(); !g.blocked("eng-1") || countEvents(s, "ask_user") != 4 { // run 8, task 5, then task 50 and run 20
		t.Fatalf("new cap: blocked=%v asks=%d", g.blocked("eng-1"), countEvents(s, "ask_user"))
	}
	// and the same for the readers of the gate (board): config is read each call
	cfg := service.GateConfigOf(s.Team)
	if !service.ReadAgentGate(s.Store, cfg, "eng-1", s.now()).TaskCapped {
		t.Fatal("gate source disagrees")
	}
	s.Team.Defaults.Caps.PerTaskUSD = 500
	if service.ReadAgentGate(s.Store, service.GateConfigOf(s.Team), "eng-1", s.now()).TaskCapped {
		t.Fatal("gate source still capped after raising the cap")
	}
}

// Spend that belongs to no task counts toward the run cap only.
func TestUnattributedSpendCountsTowardRunOnly(t *testing.T) {
	s, now, _, _ := spendEnv(t, config.SpendCaps{PerTaskUSD: 1, PerRunUSD: 5})
	s.Store.SetKV(service.KVRunStart, strconv.FormatInt(now.Unix(), 10))
	*now = now.Add(time.Minute)
	s.Store.SetAgentTask("eng-1", "")
	s.Store.AddUsage("eng-1", 1, 1, 6)
	g := s.checkSpend()
	if !g.run || countEvents(s, "spend_cap") != 1 {
		t.Fatalf("run cap should trip: %+v", g)
	}
	ts, _ := s.Store.OpenTaskSpend()
	if len(ts) != 0 {
		t.Fatalf("no task should be charged: %v", ts)
	}
}

// AC4: with neither new cap set, nothing is evaluated or recorded.
func TestUnsetNewCapsChangeNothing(t *testing.T) {
	s, _, _, _ := spendEnv(t, config.SpendCaps{PerAgentUSD: 100})
	s.Store.AddUsage("eng-1", 1, 1, 50)
	g := s.checkSpend()
	if g.run || g.blocked("eng-1") || countEvents(s, "spend_cap")+countEvents(s, "spend_warn") != 0 {
		t.Fatalf("%+v", g)
	}
}

// slai up starts the run: it records the baseline (here when none is set), and a later up
// with nothing new started leaves it where it was.
func TestUpRecordsRunStart(t *testing.T) {
	s := tmuxEnv(t, team(5)) // already ran Up, which started the sessions
	if s.kvGet(service.KVRunStart) == "" {
		t.Fatal("the up that started the sessions did not record a run start")
	}
	_ = s.Store.DeleteKV(service.KVRunStart)
	now := time.Unix(3_000_000, 0)
	s.Now = func() time.Time { return now }
	if _, err := s.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.kvGet(service.KVRunStart); got != "3000000" {
		t.Fatalf("run start = %q", got)
	}
	now = now.Add(time.Hour)
	if _, err := s.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.kvGet(service.KVRunStart); got != "3000000" {
		t.Fatalf("a no-op up moved the run start to %q", got)
	}
}
