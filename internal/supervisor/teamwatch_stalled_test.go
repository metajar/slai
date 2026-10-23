package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func eventCount(s *Supervisor, kind string) int {
	evs, _ := s.Store.EventsAfter(0, kind, 1000)
	return len(evs)
}

func stalledEnv(t *testing.T) (*Supervisor, *time.Time, store.Task) {
	t.Helper()
	s, now := driverEnv(t)
	task, err := s.Store.CreateTask(store.Task{Title: "Fix backfill", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"orch", "eng-1"} {
		_ = s.Store.SetAgentStatus(a, "idle")
	}
	return s, now, task
}

func TestTeamStalledNoticeOnceThenAtMostHourly(t *testing.T) {
	s, now, task := stalledEnv(t)
	ctx := context.Background()
	s.teamTick(ctx)
	if eventCount(s, EscTeamStalled) != 0 {
		t.Fatal("notice before the team had been quiet long enough")
	}
	_ = s.Store.SetKV(kvWakeSkip+"eng-1", "attached|1")
	*now = now.Add(TeamStalledAfter + time.Minute)
	s.teamTick(ctx)
	if got := eventCount(s, EscTeamStalled); got != 1 {
		t.Fatalf("stalled events = %d, want 1", got)
	}
	evs, _ := s.Store.EventsAfter(0, EscTeamStalled, 10)
	for _, want := range []string{task.ID, "eng-1", "no agent is working", "attached", "nothing has moved for"} {
		if !strings.Contains(evs[0].Data, want) {
			t.Errorf("notice %s lacks %q", evs[0].Data, want)
		}
	}
	if !strings.Contains(evs[0].Data, `"user":true`) || !strings.Contains(evs[0].Data, `"escalation":true`) {
		t.Errorf("notice must be a user escalation (dashboard + notify): %s", evs[0].Data)
	}
	for _, d := range []time.Duration{time.Minute, 20 * time.Minute, 38 * time.Minute} { // up to 1h after the first notice
		*now = now.Add(d)
		s.teamTick(ctx)
	}
	if got := eventCount(s, EscTeamStalled); got != 1 {
		t.Fatalf("repeated within the hour: %d events", got)
	}
	*now = now.Add(2 * time.Minute) // now over an hour since the first notice
	s.teamTick(ctx)
	if got := eventCount(s, EscTeamStalled); got != 2 {
		t.Fatalf("after an hour: %d events, want 2", got)
	}
}

func TestTeamStalledNoticeIsNotRaisedWhileSomeoneWorksAndClearsWhenItMoves(t *testing.T) {
	s, now, _ := stalledEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "working") // busy: not stalled however old the board is
	*now = now.Add(TeamStalledAfter + time.Hour)
	s.teamTick(ctx)
	if eventCount(s, EscTeamStalled) != 0 {
		t.Fatal("notice while an agent was working")
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Minute)
	s.teamTick(ctx)
	if eventCount(s, EscTeamStalled) != 1 {
		t.Fatal("expected the notice once nobody works")
	}
	// the agent starts working again: the escalation closes, so a later stall is a new notice
	_ = s.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(time.Minute)
	s.teamTick(ctx)
	if escs, _ := s.Store.OpenEscalations(); len(escs) != 0 {
		for _, e := range escs {
			if e.Kind == EscTeamStalled {
				t.Fatalf("stalled escalation still open after the board moved: %+v", e)
			}
		}
	}
}

func TestRecordManualNudge(t *testing.T) {
	s, now, _ := stalledEnv(t)
	if s.RecordManualNudge("eng-1") {
		t.Fatal("counted a nudge to an agent that has only just gone idle")
	}
	*now = now.Add(NudgeIdleAfter + time.Second)
	if !s.RecordManualNudge("eng-1") || eventCount(s, "manual_nudge") != 1 {
		t.Fatal("a send to a silent idle agent that owes a task must count")
	}
	_ = s.Store.SetAgentStatus("eng-1", "waiting_on_user")
	*now = now.Add(time.Hour)
	if s.RecordManualNudge("eng-1") {
		t.Fatal("answering an ask_user is not a nudge")
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Hour)
	_ = s.Store.SetAgentPaused("eng-1", true)
	if s.RecordManualNudge("eng-1") {
		t.Fatal("a send to a paused agent is not a nudge")
	}
	_ = s.Store.SetAgentPaused("eng-1", false)
	if s.RecordManualNudge("orch") { // owes nothing
		t.Fatal("counted a nudge to an agent that owes nothing")
	}
}
