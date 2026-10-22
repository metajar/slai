package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// Tasks go to agents that can act on them, and every kind of task has a way to finish.
func TestAssigneeMustBeAbleToWorkTheTask(t *testing.T) {
	s := newSvc(t, true)
	for _, who := range []string{"orch", "prin", "fg"} {
		_, err := s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: who})
		wantErr(t, err, "cannot work a task")
	}
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	_, err := s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Assignee: "prin"})
	wantErr(t, err, "cannot work a task")
}

func TestPMSpecTaskCompletesWithReportDone(t *testing.T) {
	s := newSvc(t, true)
	r := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "spec", Description: "write it", Assignee: "pm"})))
	id := r["task_id"].(string)
	if r["branch"] != "" && r["branch"] != nil {
		t.Errorf("a spec task has no branch, got %v", r["branch"])
	}
	if g := j(must(s.GetTask(ctx, "pm", GetTaskArgs{TaskID: id}))); g["status"] != "in_progress" {
		t.Fatalf("pm get_task should start the task: %v", g)
	}
	must(s.Report(ctx, "pm", ReportArgs{TaskID: id, Status: "done", Summary: "spec at .slai/specs/x.md"}))
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusDone {
		t.Fatalf("pm task should be done, is %s", got.Status)
	}
	if store.CanTransition(store.StatusDone, store.StatusInProgress) {
		t.Fatal("done must be terminal")
	}
	// reporting done without ever calling get_task still closes it
	id2 := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "spec2", Description: "d", Assignee: "pm"})))["task_id"].(string)
	must(s.Report(ctx, "pm", ReportArgs{TaskID: id2, Status: "done", Summary: "ok"}))
	if got := must(s.Store.GetTask(id2)); got.Status != store.StatusDone {
		t.Fatalf("got %s", got.Status)
	}
}

func TestEngineerCannotCloseTaskWithoutReview(t *testing.T) {
	s := newSvc(t, true)
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	_, err := s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "done", Summary: "all done"})
	wantErr(t, err, "request_review")
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusInProgress {
		t.Fatalf("status changed to %s", got.Status)
	}
	// blocked is always allowed
	must(s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "blocked", Summary: "need X"}))
}

func TestBlockedTaskCanBeUnblockedByOrchestrator(t *testing.T) {
	s := newSvc(t, true)
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	must(s.Report(ctx, "eng-1", ReportArgs{TaskID: id, Status: "blocked", Summary: "need the API key name"}))
	must(s.GetInbox(ctx, "eng-1"))

	_, err := s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Status: "merged"})
	wantErr(t, err, "can only be set")
	must(s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Status: "in_progress", Description: "Use API_KEY."}))
	if got := must(s.Store.GetTask(id)); got.Status != store.StatusInProgress {
		t.Fatalf("not unblocked: %s", got.Status)
	}
	in := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	if len(in) != 1 || !strings.Contains(in[0].(map[string]any)["body"].(string), "unblocked") {
		t.Fatalf("assignee not told: %v", in)
	}
	// not blocked any more: the same call is refused
	_, err = s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Status: "in_progress"})
	wantErr(t, err, "can only be set")
}

func TestReassignGivesNewEngineerItsOwnBranch(t *testing.T) {
	s := newSvc(t, true)
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "t", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	if len(s.Team.Agents) < 1 {
		t.Skip()
	}
	other := ""
	for _, a := range s.Team.Agents {
		if a.Role == "engineer" && a.ID != "eng-1" {
			other = a.ID
		}
	}
	if other == "" {
		t.Skip("fixture has a single engineer")
	}
	must(s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, Assignee: other}))
	if got := must(s.Store.GetTask(id)); got.Branch != "slai/"+other+"/"+id {
		t.Fatalf("branch %q was not reset for %s", got.Branch, other)
	}
}

func TestFinishRefusesWhileTasksOpenAndReopensOnNewWork(t *testing.T) {
	s := newSvc(t, true)
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "feature", Description: "d", Assignee: "eng-1"})))["task_id"].(string)
	_, err := s.Finish(ctx, "orch", FinishArgs{Summary: "done!"})
	wantErr(t, err, "still open")
	_, err = s.Finish(ctx, "orch", FinishArgs{Summary: " "})
	wantErr(t, err, "summary is required")
	must(s.CancelTask(ctx, "orch", CancelTaskArgs{TaskID: id, Reason: "changed plan"}))
	must(s.Finish(ctx, "orch", FinishArgs{Summary: "Built X on slai/integration. Run: go run .  Not verified: windows."}))
	if v, ok, _ := s.Store.GetKV("run.done"); !ok || !strings.Contains(v, "Built X") {
		t.Fatalf("run.done not recorded: %q %v", v, ok)
	}
	if err := s.Authorize("eng-1", "finish"); err == nil {
		t.Fatal("only the orchestrator may finish")
	}
	must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "follow-up", Description: "d", Assignee: "eng-1"}))
	if _, ok, _ := s.Store.GetKV("run.done"); ok {
		t.Fatal("new work must reopen a finished run")
	}
}
