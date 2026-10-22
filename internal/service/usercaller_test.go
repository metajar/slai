package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// The person at the keyboard (web UI) may create, unblock and cancel tasks, as "user", and
// nothing that reviews or merges.
func TestUserCallerMayManageTasksButNotReview(t *testing.T) {
	s := newSvc(t, false)
	res, err := s.CreateTask(ctx, UserCaller, CreateTaskArgs{Title: "from the web", Assignee: "eng-1"})
	if err != nil {
		t.Fatalf("user create_task: %v", err)
	}
	id := res.(map[string]any)["task_id"].(string)
	if tk, _ := s.Store.GetTask(id); tk.CreatedBy != UserCaller {
		t.Errorf("created_by = %q", tk.CreatedBy)
	}
	if _, err := s.CancelTask(ctx, UserCaller, CancelTaskArgs{TaskID: id, Reason: "not needed"}); err != nil {
		t.Fatalf("user cancel_task: %v", err)
	}
	if tk, _ := s.Store.GetTask(id); tk.Status != store.StatusCancelled {
		t.Errorf("status = %s", tk.Status)
	}
	for _, tool := range []string{"approve", "request_changes", "review_diff", "report", "request_review", "finish"} {
		err := s.Authorize(UserCaller, tool)
		if err == nil || !strings.Contains(err.Error(), "not available to the user") {
			t.Errorf("%s: user must be refused, got %v", tool, err)
		}
	}
}
