package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// The hold is in the database, so a restarted supervisor (and a down + up) still holds.
func TestTeamHoldSurvivesSupervisorRestart(t *testing.T) {
	s, now := healthEnv(t)
	if err := s.HoldTeam("overnight"); err != nil {
		t.Fatal(err)
	}
	s2 := restarted(s, now)
	h, ok := s2.CurrentHold()
	if !ok || h.Name != "overnight" || h.Since == 0 || !s2.Paused() {
		t.Fatalf("after restart: %+v %v paused=%v", h, ok, s2.Paused())
	}
	// holding again renames but keeps the start
	*now = now.Add(time.Hour)
	_ = s2.HoldTeam("renamed")
	if h2, _ := s2.CurrentHold(); h2.Name != "renamed" || h2.Since != h.Since {
		t.Fatalf("re-hold: %+v, want same since %d", h2, h.Since)
	}
}

// Held: no wake, no reminder, nothing restarted, sessions stay up. Resumed: the
// pending wake goes out and the reminders catch up.
func TestTeamHoldSuppressesDeliveryUntilResume(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "owed"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	if err := s.HoldTeam("lunch"); err != nil {
		t.Fatal(err)
	}
	slaiBefore := len(slaiMsgs(t, s, "eng-1")) + len(slaiMsgs(t, s, "orch"))
	for i := 0; i < 3; i++ {
		*now = now.Add(NudgeIdleAfter + time.Minute)
		s.wakeTick(ctx)
		s.driveTick(ctx)
		s.healthTick(ctx)
		s.upgradeTick(ctx)
	}
	if hasEvent(s, "wake") {
		t.Fatal("woke an agent during a team hold")
	}
	if n := len(slaiMsgs(t, s, "eng-1")) + len(slaiMsgs(t, s, "orch")); n != slaiBefore {
		t.Fatalf("slai sent %d new reminder(s) during the hold", n-slaiBefore)
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live {
		t.Fatal("a session was killed during the hold")
	}
	if _, ok, err := s.ReleaseHold(); !ok || err != nil {
		t.Fatalf("release: %v %v", ok, err)
	}
	if _, ok := s.CurrentHold(); ok || s.Paused() {
		t.Fatal("hold not lifted")
	}
	s.wakeTick(ctx)
	if !hasEvent(s, "wake") {
		t.Fatal("the owed wake was not delivered after resume")
	}
	if !hasEvent(s, "team_hold") || !hasEvent(s, "team_resume") {
		t.Fatal("hold/resume not recorded as events")
	}
}

// A plain pause/resume (TUI `p`) is unaffected, and resuming it leaves no hold behind.
func TestPlainPauseIsNotATeamHold(t *testing.T) {
	s, _ := healthEnv(t)
	_ = s.Pause()
	if _, ok := s.CurrentHold(); ok {
		t.Fatal("a plain pause reads as a team hold")
	}
	if _, was, _ := s.ReleaseHold(); was {
		t.Fatal("ReleaseHold lifted a plain pause")
	}
	if !s.Paused() {
		t.Fatal("plain pause must be untouched by ReleaseHold")
	}
	_ = s.HoldTeam("x")
	_ = s.Resume() // the TUI's resume ends a hold too
	if _, ok := s.CurrentHold(); ok {
		t.Fatal("Resume left the hold record")
	}
}
