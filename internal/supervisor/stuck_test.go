package supervisor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

const wakeLine = "[slai] You have 1 new items. Call get_inbox."

// stuckEnv: eng-1 is "working" by status, its pane shows the wake line typed but
// never submitted, and no hook has fired since. The clock is controllable.
func stuckEnv(t *testing.T) (*Supervisor, *time.Time) {
	t.Helper()
	oldD, oldP := tmux.EnterDelay, StuckKeyPause
	tmux.EnterDelay, StuckKeyPause = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tmux.EnterDelay, StuckKeyPause = oldD, oldP })
	s, now := driverEnv(t)
	_ = s.Store.SetAgentStatus("eng-1", "working")
	if err := s.Tmux.SendKeys(context.Background(), s.Session("eng-1"), wakeLine, false); err != nil {
		t.Fatal(err)
	}
	waitPane(t, s, "eng-1", wakeLine)
	return s, now
}

// stuckTicks advances the clock past StuckAfter and runs the two polls the
// detector needs to see an unchanged pane.
func stuckTicks(s *Supervisor, now *time.Time) {
	ctx := context.Background()
	*now = now.Add(StuckAfter + time.Second)
	s.driveTick(ctx)
	*now = now.Add(StuckPaneGap + time.Second)
	s.driveTick(ctx)
}

// restartTicks waits out StuckRestartAfter, then polls twice so the detector sees
// the pane unchanged again.
func restartTicks(s *Supervisor, now *time.Time) {
	ctx := context.Background()
	*now = now.Add(StuckRestartAfter + time.Second)
	s.driveTick(ctx)
	*now = now.Add(StuckPaneGap + time.Second)
	s.driveTick(ctx)
}

func TestStuckNudgeResubmitsUnsentWake(t *testing.T) {
	s, now := stuckEnv(t)
	stuckTicks(s, now)
	if !hasEvent(s, "stuck_detected") || !hasEvent(s, "stuck_nudge") {
		t.Fatal("expected stuck_detected and stuck_nudge events")
	}
	waitPane(t, s, "eng-1", "echo: "+wakeLine) // cleared and submitted exactly once
	if strings.Count(pane(t, s, "eng-1"), "echo: [slai]") != 1 {
		t.Fatalf("wake line not submitted exactly once:\n%s", pane(t, s, "eng-1"))
	}
	if len(pendingFrom(t, s, "orch", "slai")) != 0 {
		t.Fatal("orchestrator bothered after a successful nudge")
	}
}

func TestStuckNotDetectedWhileAlive(t *testing.T) {
	s, now := stuckEnv(t)
	ctx := context.Background()
	// recent hook activity: not silent
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	*now = now.Add(StuckAfter - 30*time.Second)
	s.driveTick(ctx)
	*now = now.Add(StuckPaneGap + time.Second)
	s.driveTick(ctx)
	if hasEvent(s, "stuck_detected") {
		t.Fatal("flagged an agent that had recent hook activity")
	}
	// silent for long, but the pane keeps changing (a live turn streaming output)
	*now = now.Add(StuckAfter + time.Second)
	for i := 0; i < 3; i++ {
		_ = s.Tmux.SendKeys(ctx, s.Session("eng-1"), fmt.Sprintf("x%d", i), false)
		time.Sleep(150 * time.Millisecond)
		*now = now.Add(StuckPaneGap + time.Second)
		s.driveTick(ctx)
	}
	if hasEvent(s, "stuck_detected") {
		t.Fatal("flagged an agent whose pane was still changing")
	}
}

func TestStuckRestartAfterFailedNudge(t *testing.T) {
	s, now := stuckEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	stuckTicks(s, now)
	if hasEvent(s, "stuck_restart") {
		t.Fatal("restarted before giving the nudge StuckRestartAfter")
	}
	restartTicks(s, now)
	if !hasEvent(s, "stuck_restart") {
		t.Fatal("expected stuck_restart")
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); live {
		t.Fatal("session still alive after restart step")
	}
	s.driveTick(ctx) // autoRevive starts a clean session
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live || !hasEvent(s, "auto_revive") {
		t.Fatal("agent was not revived")
	}
	if len(pendingFrom(t, s, "orch", "slai")) != 0 {
		t.Fatal("orchestrator bothered although the restart worked")
	}
}

func TestStuckReviveMaxExhaustedTellsOrchestrator(t *testing.T) {
	s, now := stuckEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	var recent []string
	for i := 0; i < ReviveMax; i++ {
		recent = append(recent, strconv.FormatInt(now.Unix(), 10))
	}
	_ = s.Store.SetKV(kvRevive+"eng-1", strings.Join(recent, ","))
	stuckTicks(s, now)
	restartTicks(s, now)
	if !hasEvent(s, "stuck_failed") || hasEvent(s, "stuck_restart") {
		t.Fatal("expected stuck_failed and no restart")
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live {
		t.Fatal("session killed despite ReviveMax")
	}
	msgs := pendingFrom(t, s, "orch", "slai")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "eng-1") || !strings.Contains(msgs[0].Body, "could not be restarted") {
		t.Fatalf("expected one orchestrator message, got %+v", msgs)
	}
	*now = now.Add(time.Hour) // still silent: no repeat spam
	s.driveTick(ctx)
	if len(pendingFrom(t, s, "orch", "slai")) != 1 {
		t.Fatal("orchestrator told more than once")
	}
}

func TestStuckStoppedAgentLeftAlone(t *testing.T) {
	s, now := stuckEnv(t)
	_ = s.Store.SetKV(kvStopped+"eng-1", "1")
	stuckTicks(s, now)
	if hasEvent(s, "stuck_detected") {
		t.Fatal("touched an agent the user stopped on purpose")
	}
}

func TestStuckNoOpenWorkJustClearsStatus(t *testing.T) {
	s, now := stuckEnv(t)
	stuckTicks(s, now)
	restartTicks(s, now)
	if !hasEvent(s, "stuck_cleared") || hasEvent(s, "stuck_restart") {
		t.Fatal("expected stuck_cleared without a restart")
	}
	if got := status(t, s, "eng-1"); got != "idle" {
		t.Fatalf("status = %s, want idle", got)
	}
}

func TestStuckRecoversWhenHooksResume(t *testing.T) {
	s, now := stuckEnv(t)
	ctx := context.Background()
	stuckTicks(s, now)
	time.Sleep(1100 * time.Millisecond) // events have 1s resolution
	_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}")
	*now = now.Add(time.Second)
	s.driveTick(ctx)
	if !hasEvent(s, "stuck_recovered") {
		t.Fatal("expected stuck_recovered")
	}
	if v := s.kvGet(kvStuck + "eng-1"); v != "" {
		t.Fatalf("stuck state not cleared: %q", v)
	}
}

// A non-Claude harness reports tool_end (hook:posttooluse): a turn that stalls
// after its last tool call finished is stuck, not "inside a tool call".
func TestToolInFlightEndsAtPostToolUse(t *testing.T) {
	s, now := driverEnv(t)
	flight := func() bool { return s.toolInFlight("eng-1", *now) }
	if flight() {
		t.Fatal("no tool event, nothing in flight")
	}
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	if !flight() {
		t.Fatal("a started tool call must count as in flight")
	}
	_ = s.Store.AddEvent("eng-1", "hook:posttooluse", "{}")
	if flight() {
		t.Fatal("the call finished (PostToolUse after PreToolUse): not in flight")
	}
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	if !flight() {
		t.Fatal("a newer PreToolUse starts a new call")
	}
	_ = s.Store.AddEvent("eng-1", "hook:stop", "{}")
	if flight() {
		t.Fatal("Stop ends the turn")
	}
}
