package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// mergedTask creates a task for eng-1, commits file on its branch and merges it.
func mergedTask(t *testing.T, s *Supervisor, title, file string, deps ...string) store.Task {
	t.Helper()
	tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: "eng-1", Status: store.StatusInReview})
	s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch("eng-1", tk.ID)})
	if len(deps) > 0 {
		if err := s.Store.SetTaskDeps(tk.ID, deps); err != nil {
			t.Fatal(err)
		}
	}
	tk, _ = s.Store.GetTask(tk.ID)
	commitOnTask(t, s, tk, file, title+"\n")
	requestMerge(s, tk.ID)
	s.Tick(context.Background())
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
		t.Fatalf("%s did not merge: %s", tk.ID, g.Status)
	}
	return tk
}

func integrationFiles(t *testing.T, s *Supervisor) string {
	return sh(t, s.Dir, "git", "ls-tree", "-r", "--name-only", "slai/integration")
}

func requestRevert(s *Supervisor, id string, force bool) int64 {
	f := "false"
	if force {
		f = "true"
	}
	s.Store.AddEvent("user", EvRevertRequested, `{"task_id":"`+id+`","force":`+f+`,"by":"user"}`)
	return s.Store.LastEventID()
}

func outcomeFor(s *Supervisor, req int64, kind string) (string, bool) {
	evs, _ := s.Store.EventsAfter(req, kind, 20)
	for _, e := range evs {
		if strings.Contains(e.Data, `"request":`+strconv.FormatInt(req, 10)) {
			return e.Data, true
		}
	}
	return "", false
}

// Revert one of two merged tasks: only its change goes, the other stays, the task is
// marked reverted, orch and the engineer are told, and `slai commit` would carry nothing of it.
func TestRevertTakesOneTaskOutAndLeavesTheOthers(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	a := mergedTask(t, s, "alpha", "a.txt")
	b := mergedTask(t, s, "beta", "b.txt")
	if f := integrationFiles(t, s); !strings.Contains(f, "a.txt") || !strings.Contains(f, "b.txt") {
		t.Fatalf("fixture: %s", f)
	}
	plan, err := s.PlanRevert(ctx, a.ID, false)
	if err != nil || plan.MergeSHA == "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	before := sh(t, s.Dir, "git", "rev-parse", "slai/integration")
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusMerged {
		t.Fatal("planning changed the task")
	}
	if sh(t, s.Dir, "git", "rev-parse", "slai/integration") != before {
		t.Fatal("planning moved the branch")
	}

	req := requestRevert(s, a.ID, false)
	s.Tick(ctx)
	if _, ok := outcomeFor(s, req, EvReverted); !ok {
		evs, _ := s.Store.RecentEvents(10)
		t.Fatalf("no reverted outcome; events: %+v", evs)
	}
	if f := integrationFiles(t, s); strings.Contains(f, "a.txt") || !strings.Contains(f, "b.txt") {
		t.Fatalf("integration files after revert: %s", f)
	}
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusReverted {
		t.Fatalf("status = %s, want reverted", g.Status)
	}
	if g, _ := s.Store.GetTask(b.ID); g.Status != store.StatusMerged {
		t.Fatal("the other task was touched")
	}
	eng, _ := s.Store.UndeliveredFor("eng-1")
	orch, _ := s.Store.UndeliveredFor("orch")
	if countBody(eng, "REVERTED") != 1 || countBody(orch, "REVERTED") != 1 {
		t.Fatalf("notifications: eng %+v orch %+v", eng, orch)
	}
	// slai commit would add integration onto main: the reverted task's file is not in the result
	if d := sh(t, s.Dir, "git", "diff", "--name-only", "main...slai/integration"); strings.Contains(d, "a.txt") || !strings.Contains(d, "b.txt") {
		t.Fatalf("what slai commit would carry: %q", d)
	}
	if _, err := s.PlanRevert(ctx, a.ID, false); err == nil || !strings.Contains(err.Error(), "already reverted") {
		t.Fatalf("second revert: %v", err)
	}
}

func TestRevertRefusesWhenLaterMergedTasksDependOnIt(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	a := mergedTask(t, s, "alpha", "a.txt")
	b := mergedTask(t, s, "beta", "b.txt", a.ID)

	var ref *RefusedError
	if _, err := s.PlanRevert(ctx, a.ID, false); !errors.As(err, &ref) || !strings.Contains(ref.Reason, b.ID) || !strings.Contains(ref.Reason, "--force") {
		t.Fatalf("err = %v, want refusal naming %s and --force", err, b.ID)
	}
	req := requestRevert(s, a.ID, false)
	s.Tick(ctx)
	if _, ok := outcomeFor(s, req, EvRevertRefused); !ok {
		t.Fatal("no refusal outcome")
	}
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusMerged || !strings.Contains(integrationFiles(t, s), "a.txt") {
		t.Fatal("a refused revert changed something")
	}
	req = requestRevert(s, a.ID, true)
	s.Tick(ctx)
	if _, ok := outcomeFor(s, req, EvReverted); !ok || strings.Contains(integrationFiles(t, s), "a.txt") {
		t.Fatal("--force did not revert")
	}
}

func TestRevertRefusesADirtyIntegrationCheckoutAndUnmergedTasks(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	a := mergedTask(t, s, "alpha", "a.txt")
	os.WriteFile(filepath.Join(s.IntegrationWorktree(), "stray.txt"), []byte("x"), 0o644)
	var ref *RefusedError
	if _, err := s.PlanRevert(ctx, a.ID, false); !errors.As(err, &ref) || !strings.Contains(ref.Reason, "uncommitted") {
		t.Fatalf("dirty checkout: %v", err)
	}
	os.Remove(filepath.Join(s.IntegrationWorktree(), "stray.txt"))
	open, _ := s.Store.CreateTask(store.Task{Title: "open", Assignee: "eng-1", Status: store.StatusInProgress})
	if _, err := s.PlanRevert(ctx, open.ID, false); !errors.As(err, &ref) || !strings.Contains(ref.Reason, "not merged") {
		t.Fatalf("unmerged task: %v", err)
	}
	if _, err := s.PlanRevert(ctx, "T-9999", false); err == nil {
		t.Fatal("unknown task accepted")
	}
}
