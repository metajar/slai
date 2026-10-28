package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// With no priorities set the board and the inbox keep their order; a priority moves a
// task (and the items about it) first, and the board JSON carries the field.
func TestPriorityOrdersBoardAndInbox(t *testing.T) {
	curT = t
	s := newSvc(t, false)
	a := createTask(t, s)
	b := createTask(t, s)
	ids := func() []string {
		var out []string
		for _, tv := range j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["tasks"].([]any) {
			out = append(out, tv.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids(); got[0] != a || got[1] != b {
		t.Fatalf("default order changed: %v", got)
	}
	if _, err := s.Store.ApplyPriority(b, "top"); err != nil {
		t.Fatal(err)
	}
	if got := ids(); got[0] != b {
		t.Fatalf("priority task not first on the board: %v", got)
	}
	raw, _ := json.Marshal(must(s.GetBoard(ctx, "orch", StatusFilter{})))
	if !json.Valid(raw) || !strings.Contains(string(raw), `"priority":1`) || !strings.Contains(string(raw), `"priority":0`) {
		t.Fatalf("board JSON lacks priority: %s", raw)
	}
	// The supervisor prepares/releases the engineer's assignment messages before
	// they become claimable. Simulate that handoff in this service-only test.
	pending, err := s.Store.UndeliveredFor("eng-1")
	if err != nil || len(pending) != 2 {
		t.Fatalf("queued assignments: %+v, %v", pending, err)
	}
	for _, m := range pending {
		if err := s.Store.ReleaseInbox(m.ID); err != nil {
			t.Fatal(err)
		}
	}
	items := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	if first := items[0].(map[string]any)["task_id"]; first != b {
		t.Fatalf("inbox lists %v first, want the high-priority task %s", first, b)
	}
}
