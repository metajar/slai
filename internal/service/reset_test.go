package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// A10: clear_context queues the same request `slai clear` does, and the guards hold.
func TestClearContextQueuesAndGuards(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	out := j(must(s.ClearContext(ctx, "orch", ClearContextArgs{Agent: "eng-1", Reason: "bloated"})))
	if out["status"] != "queued" {
		t.Fatalf("queue: %v", out)
	}
	req, ok := LoadReset(s.Store, "eng-1")
	if !ok || req.Trigger != TriggerOrch || req.Requester != "orch" || req.Reason != "bloated" {
		t.Fatalf("request: %+v %v", req, ok)
	}
	if again := j(must(s.ClearContext(ctx, "orch", ClearContextArgs{Agent: "eng-1"}))); again["status"] != "already_pending" {
		t.Fatalf("second ask: %v", again)
	}
	for _, tc := range []struct{ agent, want string }{
		{"orch", "only the user"}, // the orchestrator: CLI only (and it is also itself)
		{"nobody", "unknown agent"},
	} {
		_, err := s.ClearContext(ctx, "orch", ClearContextArgs{Agent: tc.agent})
		wantErr(t, err, tc.want)
	}
	// the principal while a review is open
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
	_, err := s.ClearContext(ctx, "orch", ClearContextArgs{Agent: "prin"})
	wantErr(t, err, "review is open")
	must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id}))
	if _, err := s.Store.GetTask(id); err != nil {
		t.Fatal(err)
	}
	s.Store.UpdateTaskStatus(id, store.StatusMerged)
	if q := j(must(s.ClearContext(ctx, "orch", ClearContextArgs{Agent: "prin"}))); q["status"] != "queued" {
		t.Fatalf("principal with no open review: %v", q)
	}
}

func TestClearContextIsOnlyForTheOrchestrator(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	for _, who := range []string{"pm", "prin", "eng-1", "fg"} {
		wantErr(t, s.Authorize(who, "clear_context"), "not allowed")
	}
	if err := s.Authorize("orch", "clear_context"); err != nil {
		t.Fatal(err)
	}
}

// An agent may not clear itself even where the role matrix would let it ask.
func TestClearGuardRefusesSelfAndNonUserOrchestrator(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	for _, tc := range []struct {
		trigger, requester, target, want string
	}{
		{TriggerOrch, "eng-1", "eng-1", "own context"},
		{TriggerAuto, "", "orch", "only the user"},
		{TriggerOrch, "pm", "orch", "only the user"},
	} {
		if got := ClearGuard(s.team(), s.Store, tc.trigger, tc.requester, tc.target); !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q", tc, got)
		}
	}
	if got := ClearGuard(s.team(), s.Store, TriggerUser, "", "orch"); got != "" {
		t.Errorf("the user may reset the orchestrator: %q", got)
	}
}
