package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func TestCreateTaskDependsOn(t *testing.T) {
	s := newSvc(t, true)
	a := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "a", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	r := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "b", Description: "d", Assignee: "pm", DependsOn: []string{a}})))
	b := r["task_id"].(string)
	if w, _ := r["waiting_on"].([]any); len(w) != 1 || w[0] != a {
		t.Fatalf("waiting_on: %v", r["waiting_on"])
	}
	if got := must(s.Store.GetTask(b)); got.Status != store.StatusBlocked {
		t.Fatalf("task with unmerged deps must be blocked (assigned counts as owed work), is %s", got.Status)
	}
	in := j(must(s.GetInbox(ctx, "pm")))["items"].([]any)
	if len(in) != 1 || !strings.Contains(in[0].(map[string]any)["body"].(string), "blocked on "+a) {
		t.Fatalf("assignment should say it waits: %v", in)
	}
	board := j(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	var found bool
	for _, tv := range board["tasks"].([]any) {
		m := tv.(map[string]any)
		if m["id"] == b {
			found = true
			if d, _ := m["depends_on"].([]any); len(d) != 1 || d[0] != a {
				t.Fatalf("depends_on: %v", m)
			}
			if w, _ := m["waiting_on"].([]any); len(w) != 1 || w[0] != a {
				t.Fatalf("waiting_on: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("task missing from board")
	}
	_, err := s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "c", Description: "d", Assignee: "eng-1", DependsOn: []string{"T-9999"}})
	wantErr(t, err, "T-9999")
	// update_task replaces deps; a cycle is rejected
	_, err = s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: a, DependsOn: []string{b}})
	wantErr(t, err, "cycle")
}

func TestRequestReviewFromBlocked(t *testing.T) {
	s := newSvc(t, true)
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	must(s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "blocked", Summary: "waiting"}))
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusBlocked {
		t.Fatalf("setup: %s", got.Status)
	}
	// no commits: refused, and the task stays blocked
	_, err := s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"})
	if err == nil {
		t.Fatal("expected refusal without commits")
	}
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusBlocked {
		t.Fatalf("refused review must not change status: %s", got.Status)
	}
	commitOnBranch(t, s, "slai/eng-1/"+id)
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "s"})))
	if r["status"] != store.StatusInReview {
		t.Fatalf("got %v", r)
	}
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusInReview {
		t.Fatalf("status %s", got.Status)
	}
}

func mergeTask(t *testing.T, s *Service, id string) {
	t.Helper()
	for _, to := range []string{store.StatusInProgress, store.StatusInReview, store.StatusMerged} {
		if err := s.Store.UpdateTaskStatus(id, to); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDependentUnblocksAfterLastDepMerges(t *testing.T) {
	s := newSvc(t, true)
	a := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "a", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	b := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "b", Description: "d", Assignee: "pm", DependsOn: []string{a}})))["task_id"].(string)
	must(s.GetInbox(ctx, "pm")) // drain the assignment
	mergeTask(t, s, a)
	if got := must(s.Store.GetTask(b)); got.Status != store.StatusInProgress {
		t.Fatalf("status %s", got.Status)
	}
	in := j(must(s.GetInbox(ctx, "pm")))["items"].([]any)
	// first the review-start notice (read-only prep), then the go-ahead
	if len(in) != 2 || !strings.Contains(in[0].(map[string]any)["body"].(string), "["+b+"] dependency "+a+" is now in review") ||
		!strings.Contains(in[1].(map[string]any)["body"].(string), "["+b+"] dependency "+a+" merged; you can proceed") ||
		!strings.Contains(in[1].(map[string]any)["body"].(string), "bring your branch up to date with slai/integration") {
		t.Fatalf("inbox: %v", in)
	}
	// update_task adding a pending dep blocks a running task and says so
	c := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "c", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: c}))
	d := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "d", Description: "d", Assignee: "pm"})))["task_id"].(string)
	must(s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: c, DependsOn: []string{d}}))
	if got := must(s.Store.GetTask(c)); got.Status != store.StatusBlocked {
		t.Fatalf("update with pending dep: %s", got.Status)
	}
}

func TestRequestReviewRefusedWhileWaiting(t *testing.T) {
	s := newSvc(t, true)
	a := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "a", Description: "d", Assignee: "pm"})))["task_id"].(string)
	b := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "b", Description: "d", Assignee: "eng-1", DependsOn: []string{a}})))["task_id"].(string)
	commitOnBranch(t, s, "slai/eng-1/"+b)
	_, err := s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: b, Summary: "s"})
	wantErr(t, err, "task "+b+" waits on "+a+"; request review after it merges and you have rebased")
	if got := must(s.Store.GetTask(b)); got.Status != store.StatusBlocked {
		t.Fatalf("must stay blocked, is %s", got.Status)
	}
	mergeTask(t, s, a)
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: b, Summary: "s"})))
	if r["status"] != store.StatusInReview {
		t.Fatalf("got %v", r)
	}
}
