package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// mergeFixture: eng-1's T1 is in review and approved; eng-2's T2 depends on it and has
// already started (its branch is checked out in its worktree) while it waits.
func mergeFixture(t *testing.T, t2File, t2Content string) (s *Supervisor, t1, t2 store.Task) {
	t.Helper()
	s = tmuxEnv(t, team(5))
	mk := func(assignee, title, status string) store.Task {
		tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: status})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		return tk
	}
	t1 = mk("eng-1", "first", store.StatusInReview)
	t2 = mk("eng-2", "second", store.StatusBlocked)
	if err := s.Store.SetTaskDeps(t2.ID, []string{t1.ID}); err != nil {
		t.Fatal(err)
	}
	commitOnTask(t, s, t1, "f.txt", "one\n")
	commitOnTask(t, s, t2, t2File, t2Content) // checks T2's branch out in eng-2's worktree
	return s, t1, t2
}

func notesFor(t *testing.T, s *Supervisor, to, taskID string) []store.Message {
	t.Helper()
	ms, err := s.Store.UndeliveredFor(to)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Message
	for _, m := range ms {
		if m.From == "slai" && m.TaskID == taskID {
			out = append(out, m)
		}
	}
	return out
}

func head(t *testing.T, dir string) string {
	return strings.TrimSpace(sh(t, dir, "git", "rev-parse", "HEAD"))
}

// AC 1: the merge of a dependency releases the dependent with ONE note, and its branch
// is rebased onto integration when that is clean.
func TestMergeRebasesTheDependentsBranchWhenClean(t *testing.T) {
	s, t1, t2 := mergeFixture(t, "g.txt", "two\n")
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-2")
	before := head(t, wt)
	requestMerge(s, t1.ID)
	s.Tick(context.Background())

	if g, _ := s.Store.GetTask(t1.ID); g.Status != store.StatusMerged {
		t.Fatalf("t1 = %+v", g)
	}
	if g, _ := s.Store.GetTask(t2.ID); g.Status != store.StatusInProgress {
		t.Fatalf("t2 should be released to in_progress, is %s", g.Status)
	}
	notes := notesFor(t, s, "eng-2", t2.ID)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "dependency "+t1.ID+" merged; you can proceed") ||
		!strings.Contains(notes[0].Body, "slai rebased your branch "+t2.Branch) || strings.Contains(notes[0].Body, "First bring your branch up to date") {
		t.Fatalf("go-ahead notes = %+v", notes)
	}
	if head(t, wt) == before {
		t.Fatal("the branch was not rebased")
	}
	if got := sh(t, wt, "git", "merge-base", "--is-ancestor", "slai/integration", "HEAD"); got != "" {
		t.Fatalf("integration is not an ancestor after the rebase: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "f.txt")); string(b) != "one\n" {
		t.Fatalf("dependency's file missing from the rebased branch: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "g.txt")); string(b) != "two\n" {
		t.Fatalf("the dependent's own work was lost: %q", b)
	}
	if !hasEvent(s, "dependent_rebased") {
		t.Fatal("no dependent_rebased event")
	}
	// a later tick adds nothing
	s.Tick(context.Background())
	if n := len(notesFor(t, s, "eng-2", t2.ID)); n != 1 {
		t.Fatalf("notes after another tick = %d", n)
	}
}

func TestMergeLeavesAConflictingDependentAloneAndSaysSo(t *testing.T) {
	s, t1, t2 := mergeFixture(t, "f.txt", "conflicting\n")
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-2")
	before := head(t, wt)
	requestMerge(s, t1.ID)
	s.Tick(context.Background())

	notes := notesFor(t, s, "eng-2", t2.ID)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "conflicts in f.txt, so nothing was changed") || !strings.Contains(notes[0].Body, "git rebase slai/integration") {
		t.Fatalf("notes = %+v", notes)
	}
	if head(t, wt) != before {
		t.Fatal("a failed rebase changed the branch")
	}
	if st := sh(t, wt, "git", "status"); strings.Contains(st, "rebase in progress") || strings.Contains(st, "Unmerged") {
		t.Fatalf("rebase left half-done:\n%s", st)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "f.txt")); string(b) != "conflicting\n" {
		t.Fatalf("worktree file changed: %q", b)
	}
	if g, _ := s.Store.GetTask(t2.ID); g.Status != store.StatusInProgress {
		t.Fatalf("t2 should still be released: %s", g.Status)
	}
}

func TestMergeNeverRebasesOverUncommittedWork(t *testing.T) {
	s, t1, t2 := mergeFixture(t, "g.txt", "two\n")
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-2")
	before := head(t, wt)
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	requestMerge(s, t1.ID)
	s.Tick(context.Background())
	notes := notesFor(t, s, "eng-2", t2.ID)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "did not rebase") || !strings.Contains(notes[0].Body, "git rebase slai/integration") {
		t.Fatalf("notes = %+v", notes)
	}
	if head(t, wt) != before {
		t.Fatal("rebased a dirty worktree")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "wip.txt")); string(b) != "half done\n" {
		t.Fatal("uncommitted work was disturbed")
	}
}

func TestMergeLeavesADependentThatHasNotStartedWithTheGenericAdvice(t *testing.T) {
	s := tmuxEnv(t, team(5))
	mk := func(assignee, title, status string) store.Task {
		tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: status})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		return tk
	}
	t1 := mk("eng-1", "first", store.StatusInReview)
	t2 := mk("eng-2", "second", store.StatusAssigned)
	_ = s.Store.SetTaskDeps(t2.ID, []string{t1.ID})
	commitOnTask(t, s, t1, "f.txt", "one\n")
	requestMerge(s, t1.ID)
	s.Tick(context.Background())
	notes := notesFor(t, s, "eng-2", t2.ID)
	// (the idle engineer's worktree is also switched to the released task: a second, separate note)
	generic := 0
	for _, n := range notes {
		if strings.Contains(n.Body, "First bring your branch up to date with slai/integration") {
			generic++
		}
	}
	if generic != 1 || hasEvent(s, "dependent_rebased") {
		t.Fatalf("notes = %+v", notes)
	}
}

// AC 3: report(blocked) naming a merged task is answered within a tick.
func TestBlockedReportNamingAMergedTaskIsAnsweredAutomatically(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	mk := func(assignee, title, status string) store.Task {
		tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: status})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		return tk
	}
	t1 := mk("eng-1", "first", store.StatusInReview)
	t3 := mk("eng-2", "third", store.StatusInProgress)
	t4 := mk("eng-2", "unrelated", store.StatusInProgress)
	commitOnTask(t, s, t1, "f.txt", "one\n")
	requestMerge(s, t1.ID)
	s.Tick(ctx)
	sha := strings.TrimSpace(sh(t, s.Dir, "git", "log", "-1", "--format=%h", "--merges", "slai/integration"))

	report := func(task store.Task, summary string) {
		_ = s.Store.UpdateTaskStatus(task.ID, store.StatusBlocked)
		_ = s.Store.AddEvent("eng-2", "report", fmt.Sprintf(`{"status":"blocked","summary":%q,"task_id":%q}`, summary, task.ID))
	}
	report(t3, "blocked: waiting on "+t1.ID+" to merge before I can build on it")
	report(t4, "blocked: need a decision on the API shape, see "+t4.ID+" and the open question")
	s.Tick(ctx)

	if g, _ := s.Store.GetTask(t3.ID); g.Status != store.StatusInProgress {
		t.Fatalf("t3 should be back in progress: %s", g.Status)
	}
	notes := notesFor(t, s, "eng-2", t3.ID)
	want := fmt.Sprintf("%s merged at %s; rebase and continue", t1.ID, sha)
	found := false
	for _, n := range notes {
		found = found || strings.Contains(n.Body, want)
	}
	if !found || sha == "" {
		t.Fatalf("no %q note (sha %q): %+v", want, sha, notes)
	}
	if g, _ := s.Store.GetTask(t4.ID); g.Status != store.StatusBlocked {
		t.Fatalf("a blocked report that names no merged task was answered: %s", g.Status)
	}
	// answered once
	_ = s.Store.UpdateTaskStatus(t3.ID, store.StatusBlocked)
	s.Tick(ctx)
	before := len(notesFor(t, s, "eng-2", t3.ID))
	s.Tick(ctx)
	if len(notesFor(t, s, "eng-2", t3.ID)) != before {
		t.Fatal("answered the same report twice")
	}
}

// AC 4: the digest keeps tasks that are merely queued behind a merge apart from stuck ones.
func TestDigestListsWaitingOnMergeSeparately(t *testing.T) {
	s, now := driverEnv(t)
	ctx := context.Background()
	dep, _ := s.Store.CreateTask(store.Task{Title: "foundation", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInReview})
	queued, _ := s.Store.CreateTask(store.Task{Title: "builds on it", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusAssigned})
	_ = s.Store.SetTaskDeps(queued.ID, []string{dep.ID})
	stuck, _ := s.Store.CreateTask(store.Task{Title: "silent work", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	for _, id := range []string{"orch", "eng-1", "eng-2"} {
		_ = s.Store.SetAgentStatus(id, "idle")
	}
	*now = now.Add(DigestEvery + time.Minute)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	*now = now.Add(DigestEvery + time.Minute)
	_ = s.Store.SetAgentStatus("orch", "idle")
	*now = now.Add(OrchIdleAfter + time.Second)
	s.driveTick(ctx)
	var body string
	for _, m := range pendingFrom(t, s, "orch", "slai") {
		if strings.Contains(m.Body, "Board check") {
			body = m.Body
		}
	}
	if body == "" {
		t.Fatal("no digest")
	}
	i, j := strings.Index(body, "Need a decision"), strings.Index(body, "Waiting on a merge")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("digest sections:\n%s", body)
	}
	need, wait := body[i:j], body[j:]
	if !strings.Contains(need, stuck.ID) || strings.Contains(need, queued.ID) {
		t.Errorf("'need a decision' should list %s and not %s:\n%s", stuck.ID, queued.ID, need)
	}
	if !strings.Contains(wait, queued.ID) || !strings.Contains(wait, "waits for "+dep.ID+" [in_review]") {
		t.Errorf("'waiting on a merge' should list %s waiting for %s:\n%s", queued.ID, dep.ID, wait)
	}
}
