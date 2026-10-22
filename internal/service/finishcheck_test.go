package service

import (
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// mergedSpecTask makes a spec'd task that went through review with the given verdicts
// and is merged. answers == nil means the reviewer answered nothing.
func mergedSpecTask(t *testing.T, s *Service, answers []ACVerdictArg) string {
	t.Helper()
	id := specTask(t, s, "")
	// give the task a spec_path without a file: the verdict check does not read the file
	if err := s.Store.UpdateTaskFields(id, store.Task{SpecPath: ".slai/specs/none.md"}); err != nil {
		t.Fatal(err)
	}
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims[:2]}))
	must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, ACVerdicts: answers}))
	if err := s.Store.UpdateTaskStatus(id, store.StatusMerged); err != nil {
		t.Fatal(err)
	}
	return id
}

var passBoth = []ACVerdictArg{{AC: "AC 1", Pass: true}, {AC: "AC 2", Pass: true}}

// AC 1: a merged spec'd task with a failing latest verdict blocks finish, naming task and AC.
func TestFinishBlockedByFailingVerdict(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := mergedSpecTask(t, s, []ACVerdictArg{{AC: "AC 1", Pass: true}, {AC: "AC 2", Pass: false, Note: "no test"}})
	_, err := s.Finish(ctx, "orch", FinishArgs{Summary: "all done"})
	wantErr(t, err, id)
	wantErr(t, err, "AC 2")
	wantErr(t, err, "marked fail")
	if _, ok, _ := s.Store.GetKV("run.done"); ok {
		t.Fatal("run marked done despite the block")
	}
	// the explicit, recorded override
	must(s.Finish(ctx, "orch", FinishArgs{Summary: "user accepted AC 2 gap", AcceptUnmetACs: true}))
	if eventCount(s, "finish_accepted_unmet") != 1 {
		t.Fatal("the override was not recorded")
	}
}

// AC 2: an unanswered claim blocks finish.
func TestFinishBlockedByUnansweredClaim(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := mergedSpecTask(t, s, []ACVerdictArg{{AC: "AC 1", Pass: true}}) // AC 2 never answered
	_, err := s.Finish(ctx, "orch", FinishArgs{Summary: "all done"})
	wantErr(t, err, id)
	wantErr(t, err, "AC 2")
	wantErr(t, err, "never answered")
}

// AC 3: all pass -> finish succeeds and reports how many merged tasks had no spec.
func TestFinishSucceedsWhenAllVerdictsPassAndCountsMissingSpecs(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	mergedSpecTask(t, s, passBoth)
	r := j(must(s.Finish(ctx, "orch", FinishArgs{Summary: "all done"})))
	if r["status"] != "finished" || r["merged_without_spec"] != float64(0) {
		t.Fatalf("finish = %v", r)
	}

	s2 := newSvc(t, true)
	mergedSpecTask(t, s2, passBoth)
	plain := specTask(t, s2, "")
	must(s2.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: plain, Summary: "done"}))
	if err := s2.Store.UpdateTaskStatus(plain, store.StatusMerged); err != nil {
		t.Fatal(err)
	}
	r = j(must(s2.Finish(ctx, "orch", FinishArgs{Summary: "all done"})))
	if r["merged_without_spec"] != float64(1) || r["merged"] != float64(2) {
		t.Fatalf("finish = %v, want 1 merged task without a spec out of 2", r)
	}
}

// AC 4: cancelled tasks and tasks without a spec_path never block.
func TestFinishIgnoresCancelledAndSpeclessTasks(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	// a cancelled spec'd task whose verdicts would fail
	bad := mergedSpecTask(t, s, []ACVerdictArg{{AC: "AC 1", Pass: false}, {AC: "AC 2", Pass: false}})
	if err := s.Store.MarkReverted(bad); err != nil { // no longer merged: not counted
		t.Fatal(err)
	}
	plain := specTask(t, s, "")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: plain, Summary: "done"}))
	if err := s.Store.UpdateTaskStatus(plain, store.StatusMerged); err != nil {
		t.Fatal(err)
	}
	cancelled := createTask(t, s)
	must(s.CancelTask(ctx, "orch", CancelTaskArgs{TaskID: cancelled, Reason: "not needed"}))
	if r := j(must(s.Finish(ctx, "orch", FinishArgs{Summary: "all done"}))); r["status"] != "finished" {
		t.Fatalf("finish = %v", r)
	}
}
