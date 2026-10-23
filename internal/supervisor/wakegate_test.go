package supervisor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// breakWorktree makes every worktree switch for eng-1 fail until the returned func restores it.
func breakWorktree(t *testing.T, s *Supervisor) (restore func()) {
	t.Helper()
	wt := s.taskWorktree(store.Agent{ID: "eng-1"})
	if err := os.Rename(wt, wt+".off"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Rename(wt+".off", wt); err != nil {
			t.Fatal(err)
		}
	}
}

func openEscUser(t *testing.T, s *Supervisor, kind, agent string) bool {
	t.Helper()
	e, ok := openEsc(t, s, kind, agent)
	return ok && e.User
}

// H-1: a failed worktree switch holds back only that assignment. The agent's other mail is
// delivered, the switch is retried, the user is told after 5 minutes, and the assignment goes
// through once the worktree works again.
func TestFailedSwitchStillDeliversOtherMail(t *testing.T) {
	s, now := driverEnv(t)
	s.Driver = false
	ctx := context.Background()
	restore := breakWorktree(t, s)
	task, _ := s.Store.CreateTask(store.Task{Title: "D", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusAssigned})
	asg, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: task.ID, Body: "do D"})
	note, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "other mail"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")

	s.Tick(ctx)
	if wakesOf(s, "eng-1") != 1 {
		t.Fatalf("wakes = %d, want 1 (the note must wake the agent although the switch failed)", wakesOf(s, "eng-1"))
	}
	got, err := s.Store.ClaimInbox("eng-1")
	if err != nil || len(got) != 1 || got[0].ID != note {
		t.Fatalf("get_inbox handed out %+v (err %v), want only the note %d", got, err, note)
	}
	if !s.Store.InboxHeld(asg) {
		t.Fatal("the failed assignment is not held back")
	}
	if openEscUser(t, s, EscSwitchFailed, "eng-1") {
		t.Fatal("user told before 5 minutes")
	}

	// the retry happens every tick: nothing deliverable is left, so the reason is recorded
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Minute)
	s.wakeTick(ctx)
	if r, _ := WakeSkipReason(s.Store, "eng-1"); r != SkipPrepFailed {
		t.Fatalf("skip reason = %q, want %q", r, SkipPrepFailed)
	}
	*now = now.Add(SwitchEscalateAfter)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.wakeTick(ctx)
	if !openEscUser(t, s, EscSwitchFailed, "eng-1") {
		t.Fatal("no user-visible escalation after 5 minutes of a failing switch")
	}

	restore()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Minute)
	s.wakeTick(ctx)
	if s.Store.InboxHeld(asg) {
		t.Fatal("assignment still held after the switch worked")
	}
	if _, ok := openEsc(t, s, EscSwitchFailed, "eng-1"); ok {
		t.Fatal("escalation not cleared after the switch worked")
	}
	if wakesOf(s, "eng-1") != 2 {
		t.Fatalf("wakes = %d, want the assignment to wake the agent", wakesOf(s, "eng-1"))
	}
}

// H-1: every branch that leaves mail unwoken records why; a wake or an empty queue clears it.
func TestSkipReasonsAreRecordedAndCleared(t *testing.T) {
	s, now := driverEnv(t)
	s.Driver = false
	ctx := context.Background()
	reason := func(agent string) string { r, _ := WakeSkipReason(s.Store, agent); return r }
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"})

	_ = s.Store.SetAgentStatus("eng-1", "working")
	s.wakeTick(ctx)
	if reason("eng-1") != SkipBusy {
		t.Fatalf("busy: reason = %q", reason("eng-1"))
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetAgentPaused("eng-1", true)
	s.wakeTick(ctx)
	if reason("eng-1") != SkipHeld {
		t.Fatalf("held: reason = %q", reason("eng-1"))
	}
	_ = s.Store.SetAgentPaused("eng-1", false)
	_ = s.Store.TouchFocus("eng-1", *now)
	s.wakeTick(ctx)
	if reason("eng-1") != SkipFocus {
		t.Fatalf("focus: reason = %q", reason("eng-1"))
	}
	_ = s.Store.ClearFocus("eng-1")
	s.Team.Defaults.MaxConcurrentAgents = 0
	s.wakeTick(ctx)
	if reason("eng-1") != SkipSlot {
		t.Fatalf("slot: reason = %q", reason("eng-1"))
	}
	if r := strings.SplitN(s.kvGet(kvWakeSkip+"eng-1"), "|", 2); len(r) != 2 || r[1] == "" {
		t.Fatalf("reason has no since stamp: %q", s.kvGet(kvWakeSkip+"eng-1"))
	}
	s.Team.Defaults.MaxConcurrentAgents = 5
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.wakeTick(ctx)
	if wakesOf(s, "eng-1") != 1 || reason("eng-1") != "" {
		t.Fatalf("after the wake: wakes=%d reason=%q", wakesOf(s, "eng-1"), reason("eng-1"))
	}
	// a stale reason goes when nothing is pending any more
	_ = s.Store.SetKV(kvWakeSkip+"eng-2", "busy|1")
	s.wakeTick(ctx)
	if reason("eng-2") != "" {
		t.Fatalf("stale reason kept: %q", reason("eng-2"))
	}
}

// H-1: a "working" agent whose hooks and tool calls went quiet does not take a concurrency slot.
func TestPhantomWorkingAgentDoesNotTakeASlot(t *testing.T) {
	s, now := driverEnv(t)
	s.Driver = false
	ctx := context.Background()
	s.Team.Defaults.MaxConcurrentAgents = 1
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetAgentStatus("eng-2", "working") // just started a turn: a real slot
	s.wakeTick(ctx)
	if wakesOf(s, "eng-1") != 0 || func() string { r, _ := WakeSkipReason(s.Store, "eng-1"); return r }() != SkipSlot {
		t.Fatalf("a live working agent should hold the only slot (wakes=%d)", wakesOf(s, "eng-1"))
	}
	*now = now.Add(11 * time.Minute) // eng-2 has shown no hook or tool activity for longer than StaleAfter
	s.wakeTick(ctx)
	if wakesOf(s, "eng-1") != 1 {
		t.Fatal("a phantom 'working' agent starved eng-1")
	}
}

// H-1: after MaxUnansweredWakes the agent is woken again every WakeRearmEvery instead of never;
// a slai tool call after the last wake clears the lock-out at once.
func TestUnansweredWakesRearmAfterBackoff(t *testing.T) {
	s, now := driverEnv(t)
	s.Driver = false
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "please look"})
	ignore := func() {
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}")
		_ = s.Store.AddEvent("eng-1", "hook:stop", "{}")
	}
	for i := 0; i < MaxUnansweredWakes; i++ {
		ignore()
		*now = now.Add(GiveUpAfter/MaxUnansweredWakes + time.Second)
		s.Tick(ctx)
	}
	if wakesOf(s, "eng-1") != MaxUnansweredWakes {
		t.Fatalf("wakes = %d, want %d", wakesOf(s, "eng-1"), MaxUnansweredWakes)
	}
	ignore()
	*now = now.Add(WakeRearmEvery / 2)
	s.Tick(ctx)
	if wakesOf(s, "eng-1") != MaxUnansweredWakes {
		t.Fatal("woken again before the backoff elapsed")
	}
	ignore()
	*now = now.Add(WakeRearmEvery/2 + time.Second)
	s.Tick(ctx)
	if wakesOf(s, "eng-1") != MaxUnansweredWakes+1 {
		t.Fatalf("not re-armed after %s: wakes = %d", WakeRearmEvery, wakesOf(s, "eng-1"))
	}
	// a slai tool call after the last wake proves it is alive and reading
	for i := 0; i < 3; i++ { // use up the re-armed budget again: held until the next backoff
		ignore()
	}
	_ = s.Store.SetKV(kvLastSeen+"eng-1", itoa64(now.Unix()+1))
	ignore()
	*now = now.Add(2 * time.Second)
	s.Tick(ctx)
	if wakesOf(s, "eng-1") != MaxUnansweredWakes+2 {
		t.Fatalf("a live agent stayed locked out: wakes = %d", wakesOf(s, "eng-1"))
	}
}

// H-1: when the agent that stops answering is the orchestrator, the user is told directly.
func TestStuckOrchestratorEscalatesToTheUser(t *testing.T) {
	s, now := driverEnv(t)
	s.Driver = false
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "slai", To: "orch", Kind: "note", Body: "decide"})
	for i := 0; i < MaxUnansweredWakes+2; i++ {
		_ = s.Store.SetAgentStatus("orch", "idle")
		_ = s.Store.AddEvent("orch", "hook:userpromptsubmit", "{}")
		_ = s.Store.AddEvent("orch", "hook:stop", "{}")
		*now = now.Add(GiveUpAfter/MaxUnansweredWakes + time.Second)
		s.Tick(ctx)
	}
	if !openEscUser(t, s, EscWakeIgnored, "orch") {
		t.Fatal("no user-visible escalation for an orchestrator that ignores its wake-ups")
	}
	for _, m := range mustUndelivered(t, s, "orch") {
		if strings.Contains(m.Body, "ignored") {
			t.Fatalf("the orchestrator was told about itself: %q", m.Body)
		}
	}
}

// H-1: the message-loop guard keeps its 90 s hold but only asks the user from the third trip.
func TestLoopGuardAsksTheUserOnlyOnTheThirdTrip(t *testing.T) {
	s := unitEnv(t, team(5))
	for _, a := range s.Team.Agents {
		_ = s.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.Store.SetClock(func() time.Time { return clock })
	trip := func() {
		t.Helper()
		clock = clock.Add(2 * time.Second) // ResumeAgent resets the window to "strictly after now"
		for i := 0; i <= LoopMessageThreshold; i++ {
			_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "eng-2", Body: "x"})
		}
		if got := s.checkLoops(); len(got) != 1 {
			t.Fatalf("should trip: %v", got)
		}
		if p, _ := s.Store.PausedAgents(); !p["eng-1"] || !p["eng-2"] {
			t.Fatalf("not held: %v", p)
		}
		s.ResumeAgent("eng-1")
		s.ResumeAgent("eng-2")
		clock = clock.Add(2 * time.Second)
	}
	asks := func() int { return countEvents(s, "ask_user") }
	trip()
	trip()
	if asks() != 0 || countEvents(s, "loop_hold") != 2 {
		t.Fatalf("after 2 trips: ask_user=%d loop_hold=%d", asks(), countEvents(s, "loop_hold"))
	}
	trip()
	if asks() != 1 {
		t.Fatalf("after 3 trips ask_user = %d, want 1", asks())
	}
}
