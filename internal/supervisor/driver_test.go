package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// driverEnv is a running team with the driver on and a controllable clock.
func driverEnv(t *testing.T) (*Supervisor, *time.Time) {
	t.Helper()
	s := tmuxEnv(t, team(5))
	s.Driver = true
	now := time.Now()
	s.Now = func() time.Time { return now }
	return s, &now
}

func pendingFrom(t *testing.T, s *Supervisor, to, from string) []store.Message {
	t.Helper()
	ms, err := s.Store.UndeliveredFor(to)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Message
	for _, m := range ms {
		if m.From == from {
			out = append(out, m)
		}
	}
	return out
}

func TestDriverRemindsIdleOwnerThenEscalates(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "Fix backfill", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-1", "idle")

	s.driveTick(ctx)
	if len(pendingFrom(t, s, "eng-1", "slai")) != 0 {
		t.Fatal("reminded before the agent had been idle long enough")
	}
	*now = now.Add(NudgeIdleAfter + time.Second)
	s.driveTick(ctx)
	got := pendingFrom(t, s, "eng-1", "slai")
	if len(got) != 1 || !strings.Contains(got[0].Body, task.ID) || !strings.Contains(got[0].Body, "request_review") {
		t.Fatalf("expected one actionable reminder, got %+v", got)
	}
	s.driveTick(ctx) // same instant: no storm
	if len(pendingFrom(t, s, "eng-1", "slai")) != 1 {
		t.Fatal("reminder repeated without waiting NudgeEvery")
	}

	// the reminder is delivered and ignored; reminders continue, then escalate
	for i := 0; i < MaxNudges; i++ {
		ms, _ := s.Store.UndeliveredFor("eng-1")
		for _, m := range ms {
			_ = s.Store.MarkDelivered(m.ID)
		}
		*now = now.Add(NudgeEvery + time.Second)
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		*now = now.Add(NudgeIdleAfter + time.Second)
		s.driveTick(ctx)
	}
	esc := pendingFrom(t, s, "orch", "slai")
	if len(esc) == 0 || !strings.Contains(esc[len(esc)-1].Body, "has not made progress") || !hasEvent(s, "stalled") {
		t.Fatalf("expected an escalation to the orchestrator, got %+v", esc)
	}
}

func TestDriverResetsWhenTaskProgresses(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusAssigned})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(NudgeIdleAfter + time.Second)
	s.driveTick(ctx)
	if n := s.kvGet(kvNudge + "eng-1"); !strings.Contains(n, "|1|") {
		t.Fatalf("nudge count = %q", n)
	}
	for _, m := range mustUndelivered(t, s, "eng-1") {
		_ = s.Store.MarkDelivered(m.ID)
	}
	// the engineer moves the task along: the count starts over
	_ = s.Store.UpdateTaskStatus(task.ID, store.StatusInProgress)
	*now = now.Add(NudgeEvery + time.Second)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(NudgeIdleAfter + time.Second)
	s.driveTick(ctx)
	if n := s.kvGet(kvNudge + "eng-1"); !strings.Contains(n, "|1|") {
		t.Fatalf("count should restart after progress, got %q", n)
	}
}

func mustUndelivered(t *testing.T, s *Supervisor, id string) []store.Message {
	ms, err := s.Store.UndeliveredFor(id)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestDriverDoesNotNudgeBusyWaitingOrHeldAgents(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	*now = now.Add(time.Hour)
	for _, st := range []string{"working", "waiting", "waiting_on_user", "dead"} {
		_ = s.Store.SetAgentStatus("eng-1", st)
		s.driveTick(ctx)
		if len(pendingFrom(t, s, "eng-1", "slai")) != 0 {
			t.Fatalf("nudged an agent in state %q", st)
		}
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetAgentPaused("eng-1", true)
	s.driveTick(ctx)
	if len(pendingFrom(t, s, "eng-1", "slai")) != 0 {
		t.Fatal("nudged a held agent")
	}
}

func TestDriverOrchestratorDigestAndSettled(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	t1, _ := s.Store.CreateTask(store.Task{Title: "feature", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("orch", "idle")
	_ = s.Store.SetAgentStatus("eng-1", "working") // the board is moving: leave orch alone
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	*now = now.Add(DigestEvery + time.Minute)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	if len(pendingFrom(t, s, "orch", "slai")) != 0 {
		t.Fatal("digest sent while an engineer was actively working")
	}
	// nobody is working and the board hasn't moved: digest
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(DigestEvery + time.Minute)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	got := pendingFrom(t, s, "orch", "slai")
	if len(got) == 0 || !strings.Contains(got[len(got)-1].Body, t1.ID) || !strings.Contains(got[len(got)-1].Body, "Board check") {
		t.Fatalf("expected a board digest naming %s, got %+v", t1.ID, got)
	}
	// closing the last task produces exactly one 'verify and tell the user' notice
	for _, m := range mustUndelivered(t, s, "orch") {
		_ = s.Store.MarkDelivered(m.ID)
	}
	_ = s.Store.UpdateTaskStatus(t1.ID, store.StatusInReview)
	_ = s.Store.UpdateTaskStatus(t1.ID, store.StatusMerged)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	done := pendingFrom(t, s, "orch", "slai")
	if len(done) != 1 || !strings.Contains(done[0].Body, "final summary") {
		t.Fatalf("expected one settled notice, got %+v", done)
	}
	_ = s.Store.MarkDelivered(done[0].ID)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(time.Hour)
	s.driveTick(ctx)
	if len(pendingFrom(t, s, "orch", "slai")) != 0 {
		t.Fatal("settled notice repeated")
	}
}

func TestAutoReviveRespectsDeliberateStop(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})

	// a crash: the session disappears while work is owed -> revived automatically
	_ = s.Tmux.KillSession(ctx, s.Session("eng-2"))
	_ = s.Store.SetAgentStatus("eng-2", "dead")
	s.driveTick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-2")); !live || !hasEvent(s, "auto_revive") {
		t.Fatal("crashed agent with owed work was not revived")
	}

	// the user stops it on purpose -> it stays stopped
	if err := s.StopAgent(ctx, "eng-2"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	s.driveTick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-2")); live {
		t.Fatal("deliberately stopped agent was revived")
	}
	// ...until the user starts it again
	cfg, _ := s.agentCfg("eng-2")
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if s.kvGet(kvStopped+"eng-2") != "" {
		t.Fatal("start did not clear the stop flag")
	}
}

func TestTickStagesAreIsolatedFromPanics(t *testing.T) {
	s := unitEnv(t, team(5))
	var ran []string
	s.runSteps([]step{
		{"a", func() { ran = append(ran, "a") }},
		{"boom", func() { panic("kaboom") }},
		{"c", func() { ran = append(ran, "c") }},
	})
	if strings.Join(ran, ",") != "a,c" {
		t.Fatalf("a panic skipped other stages: %v", ran)
	}
	if !hasEvent(s, "supervisor_panic") {
		t.Fatal("panic not recorded")
	}
}

// An agent that never answers the same unread items is poked a bounded number of
// times, then the orchestrator is told; one that was parked on ask_user for a long
// time receives other work too.
func TestWakesAreBoundedAndAskUserHasPatience(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	s.Driver = false
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	for i := 0; i < MaxUnansweredWakes+3; i++ {
		_ = s.Store.SetAgentStatus("eng-1", "idle")                  // the agent ends a turn without claiming
		_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}") // the wake landed...
		_ = s.Store.AddEvent("eng-1", "hook:stop", "{}")             // ...and the turn ended without claiming
		*now = now.Add(GiveUpAfter/MaxUnansweredWakes + time.Second)
		s.Tick(ctx)
	}
	*now = now.Add(GiveUpAfter)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.Tick(ctx)
	n := 0
	evs, _ := s.Store.RecentEvents(500)
	for _, e := range evs {
		if e.Agent == "eng-1" && e.Kind == "wake" {
			n++
		}
	}
	// MaxUnansweredWakes, then no more until the backoff (WakeRearmEvery) has passed: the last
	// Tick above is GiveUpAfter later, so it re-arms exactly once.
	if n != MaxUnansweredWakes+1 {
		t.Fatalf("woke %d times, want %d plus one re-armed wake", n, MaxUnansweredWakes)
	}
	if !hasEvent(s, "wake_gave_up") {
		t.Fatal("giving up was not recorded")
	}
	told := false
	for _, m := range mustUndelivered(t, s, "orch") {
		if strings.Contains(m.Body, "ignored") {
			told = true
		}
	}
	if !told {
		t.Fatal("orchestrator was not told eng-1 is stuck")
	}

	*now = time.Now() // back to the store's clock
	// ask_user patience: a fresh message reaches an agent parked on a human for 15+ minutes
	_ = s.Store.SetAgentStatus("eng-2", "waiting_on_user")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Kind: "note", Body: "heads up"})
	s.Tick(ctx)
	for _, m := range mustUndelivered(t, s, "eng-2") {
		if m.WokenAt != 0 {
			t.Fatal("woken before the patience window")
		}
	}
	*now = now.Add(AskUserPatience + time.Minute)
	s.Tick(ctx)
	woken := false
	for _, m := range mustUndelivered(t, s, "eng-2") {
		if m.WokenAt != 0 {
			woken = true
		}
	}
	if !woken {
		t.Fatal("agent parked on ask_user for too long never received other work")
	}
}

func wakesOf(s *Supervisor, agent string) int {
	n := 0
	evs, _ := s.Store.RecentEvents(500)
	for _, e := range evs {
		if e.Agent == agent && e.Kind == "wake" {
			n++
		}
	}
	return n
}

func toldStuck(t *testing.T, s *Supervisor) bool {
	for _, m := range mustUndelivered(t, s, "orch") {
		if strings.Contains(m.Body, "ignored") {
			return true
		}
	}
	return false
}

// A busy agent (turn never ends, or a user attached to its pane) is not re-woken and
// never reported as stuck, however long that lasts.
func TestBusyAgentIsNotWokenRepeatedlyNorReportedStuck(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	s.Driver = false
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	s.Tick(ctx) // first wake
	for i := 0; i < 4; i++ {
		_ = s.Store.SetAgentStatus("eng-1", "idle") // e.g. a Notification hook flipped it, but no Stop
		*now = now.Add(WakeRetry + time.Second)
		s.Tick(ctx)
	}
	if n := wakesOf(s, "eng-1"); n != 1 {
		t.Fatalf("busy agent woken %d times within the hold, want 1", n)
	}
	for i := 0; i < 60; i++ { // an hour more without a single Stop
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		*now = now.Add(WakeRetry)
		s.Tick(ctx)
	}
	if toldStuck(t, s) || hasEvent(s, "wake_gave_up") {
		t.Fatal("busy agent reported stuck")
	}
}

// Attempts that are skipped (the user is attached/focused) never count as wakes.
func TestSkippedWakesDoNotCount(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	s.Driver = false
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	_ = s.Store.TouchFocus("eng-1", *now)
	for i := 0; i < 3*MaxUnansweredWakes; i++ {
		s.Tick(ctx)
	}
	if n := wakesOf(s, "eng-1"); n != 0 {
		t.Fatalf("woke a focused agent %d times", n)
	}
	if v := s.kvGet(kvWakeTries + "eng-1"); v != "" {
		t.Fatalf("skipped attempts were counted: %q", v)
	}
}
