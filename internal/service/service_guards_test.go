package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// PRD 8 "Review ping-pong": the 3rd request_changes (and only the 3rd) blocks
// the task; orchestrator gets a notice and an ask_user event reaches the user.
func TestGuardReviewPingPongThreshold(t *testing.T) {
	if MaxReviewRounds != 3 {
		t.Fatalf("PRD says 3 rounds, got %d", MaxReviewRounds)
	}
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	s.GetInbox(ctx, "orch")
	for round := 1; round <= 2; round++ {
		must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"}))
		must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "c"}))
		if tk, _ := s.Store.GetTask(id); tk.Status == store.StatusBlocked {
			t.Fatalf("blocked too early at round %d", round)
		}
		must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	}
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"}))
	must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "still wrong"}))
	if tk, _ := s.Store.GetTask(id); tk.Status != store.StatusBlocked {
		t.Fatalf("want blocked, got %s", tk.Status)
	}
	oin := j(must(s.GetInbox(ctx, "orch")))["items"].([]any)
	if len(oin) != 1 || !strings.Contains(oin[0].(map[string]any)["body"].(string), "blocked") {
		t.Fatalf("orchestrator notice: %v", oin)
	}
	evs, _ := s.Store.EventsByKind("prin", "ask_user", 5)
	if len(evs) != 1 {
		t.Fatalf("ask_user events: %d", len(evs))
	}
}

// Lane enforcement at the MCP layer: only the principal may approve /
// request changes; engineers cannot approve their own work.
func TestGuardOnlyPrincipalApproves(t *testing.T) {
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"}))
	for _, caller := range []string{"eng-1", "orch", "pm", "fg"} {
		if _, err := s.Call(ctx, caller, "approve", []byte(`{"task_id":"`+id+`"}`)); err == nil {
			t.Errorf("%s was allowed to approve", caller)
		}
	}
}
