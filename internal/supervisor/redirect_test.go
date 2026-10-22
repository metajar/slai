package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func redirectAgent(t *testing.T, s *Supervisor, status string) store.Agent {
	t.Helper()
	_ = s.Store.SetAgentStatus("eng-1", status)
	return mustAgent(t, s, "eng-1")
}

func TestRedirectInterruptsOnceThenWakeDeliversTheCorrection(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	a := redirectAgent(t, s, "working")
	id, err := RequestRedirect(s.Store, a, "  use the other API  ")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.Store.GetMessage(id)
	if m.Kind != "note" || m.From != "user" || m.To != "eng-1" || m.Body != "Course correction from user: use the other API" {
		t.Fatalf("message = %+v", m)
	}
	s.redirectTick(ctx)
	if countEvents(s, "redirect") != 1 || status(t, s, "eng-1") != "idle" {
		t.Fatalf("redirect events=%d status=%s", countEvents(s, "redirect"), status(t, s, "eng-1"))
	}
	s.redirectTick(ctx) // the request is consumed: no second Escape
	if countEvents(s, "redirect") != 1 {
		t.Fatal("Escape sent twice for one request")
	}
	*now = now.Add(time.Second)
	s.wakeTick(ctx)
	waitPane(t, s, "eng-1", "You have 1 new items")
}

func TestRedirectNeverFiresOverAPermissionDialog(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	show(t, s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
	a := redirectAgent(t, s, "working")
	id, _ := RequestRedirect(s.Store, a, "stop")
	s.redirectTick(ctx)
	if countEvents(s, "redirect") != 0 || countEvents(s, "redirect_skipped") != 1 {
		t.Fatalf("redirect=%d skipped=%d", countEvents(s, "redirect"), countEvents(s, "redirect_skipped"))
	}
	if d := lastEventData(t, s, "eng-1", "redirect_skipped"); !strings.Contains(d["reason"].(string), "permission dialog") {
		t.Fatalf("reason = %v", d["reason"])
	}
	if status(t, s, "eng-1") != "working" {
		t.Fatal("status changed although nothing was sent")
	}
	if m, _ := s.Store.GetMessage(id); m.DeliveredAt != 0 {
		t.Fatal("the correction must stay queued")
	}
}

func TestRedirectSkippedWhenPausedOrDead(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	a := redirectAgent(t, s, "working")
	_, _ = RequestRedirect(s.Store, a, "x")
	_ = s.Pause()
	s.redirectTick(ctx)
	_ = s.Resume()
	_, _ = RequestRedirect(s.Store, a, "y")
	_ = s.Tmux.KillSession(ctx, s.Session("eng-1"))
	s.redirectTick(ctx)
	if countEvents(s, "redirect") != 0 || countEvents(s, "redirect_skipped") != 2 {
		t.Fatalf("redirect=%d skipped=%d", countEvents(s, "redirect"), countEvents(s, "redirect_skipped"))
	}
}

func TestSetPriorityAppliesAndRecordsWhoAsked(t *testing.T) {
	s, _ := healthEnv(t)
	t1, _ := s.Store.CreateTask(store.Task{Title: "a", Assignee: "eng-1", CreatedBy: "orch"})
	t2, _ := s.Store.CreateTask(store.Task{Title: "b", Assignee: "eng-1", CreatedBy: "orch"})
	if p, err := s.SetPriority(t2.ID, "top"); err != nil || p != 1 {
		t.Fatalf("top = %d, %v", p, err)
	}
	if p, _ := s.SetPriority(t1.ID, "up"); p != 1 {
		t.Fatalf("up = %d", p)
	}
	if _, err := s.SetPriority("T-9999", "up"); err == nil {
		t.Fatal("unknown task accepted")
	}
	if got, _ := s.Store.GetTask(t2.ID); got.Priority != 1 || countEvents(s, "priority") != 2 {
		t.Fatalf("priority=%d events=%d", got.Priority, countEvents(s, "priority"))
	}
}
