package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func requestMergeAt(s *Supervisor, id, sha string) {
	s.Store.AddEvent("principal", "merge_requested", fmt.Sprintf(`{"task_id":%q,"notes":"lgtm","sha":%q}`, id, sha))
}

func pinFixture(t *testing.T) (*Supervisor, store.Task) {
	t.Helper()
	s := tmuxEnv(t, team(5))
	tk, _ := s.Store.CreateTask(store.Task{Title: "pinned", Assignee: "eng-1", Status: store.StatusInReview})
	s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch("eng-1", tk.ID)})
	tk, _ = s.Store.GetTask(tk.ID)
	commitOnTask(t, s, tk, "f.txt", "one\n")
	return s, tk
}

func addCommit(t *testing.T, s *Supervisor, file string) string {
	t.Helper()
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	os.WriteFile(filepath.Join(wt, file), []byte("late\n"), 0o644)
	sh(t, wt, "git", "add", ".")
	sh(t, wt, "git", "commit", "-m", "after approval "+file)
	return strings.TrimSpace(sh(t, wt, "git", "rev-parse", "HEAD"))
}

func countBody(ms []store.Message, sub string) (n int) {
	for _, m := range ms {
		if strings.Contains(m.Body, sub) {
			n++
		}
	}
	return
}

// Approve, then a new commit: nothing merges, the task stays in review, and the
// engineer and the principal are each told once.
func TestMergeRefusesABranchThatMovedAfterApproval(t *testing.T) {
	s, tk := pinFixture(t)
	ctx := context.Background()
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	approved := strings.TrimSpace(sh(t, wt, "git", "rev-parse", "HEAD"))
	requestMergeAt(s, tk.ID, approved)
	tip := addCommit(t, s, "unreviewed.txt")
	integrationBefore := strings.TrimSpace(sh(t, s.Dir, "git", "rev-parse", "slai/integration"))

	s.Tick(ctx)
	s.Tick(ctx) // the approval is consumed: a second pass must not repeat anything

	if got := strings.TrimSpace(sh(t, s.Dir, "git", "rev-parse", "slai/integration")); got != integrationBefore {
		t.Fatalf("integration moved to %s: an unreviewed commit was merged", got)
	}
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusInReview {
		t.Fatalf("task status = %s, want in_review", g.Status)
	}
	eng, _ := s.Store.UndeliveredFor("eng-1")
	if n := countBody(eng, "new commits after approval"); n != 1 || countBody(eng, "request_review again") != 1 {
		t.Fatalf("engineer notes: %+v", eng)
	}
	if n := countBody(eng, approved[:8]) + countBody(eng, tip[:8]); n < 1 {
		t.Fatalf("the note should name the commits: %+v", eng)
	}
	prin, _ := s.Store.UndeliveredFor("principal")
	if n := countBody(prin, "NOT merged"); n != 1 || countBody(prin, "review it again") != 1 {
		t.Fatalf("principal notes: %+v", prin)
	}
	if !hasEvent(s, "merge_refused") || hasEvent(s, "merged") {
		t.Fatal("events: want merge_refused and no merged")
	}

	// the engineer re-requests, the principal approves the new head: it merges, all of it
	requestMergeAt(s, tk.ID, tip)
	s.Tick(ctx)
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
		t.Fatalf("after the fresh approval: %s", g.Status)
	}
	for _, f := range []string{"f.txt", "unreviewed.txt"} {
		if _, err := os.Stat(filepath.Join(s.IntegrationWorktree(), f)); err != nil {
			t.Errorf("%s missing from integration after the re-approved merge", f)
		}
	}
}

// Approve with no new commit: the merge takes exactly the approved commit.
func TestMergeTakesExactlyTheApprovedCommit(t *testing.T) {
	s, tk := pinFixture(t)
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	approved := strings.TrimSpace(sh(t, wt, "git", "rev-parse", "HEAD"))
	requestMergeAt(s, tk.ID, approved)
	s.Tick(context.Background())
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
		t.Fatalf("status = %s", g.Status)
	}
	if second := strings.TrimSpace(sh(t, s.Dir, "git", "rev-parse", "slai/integration^2")); second != approved {
		t.Fatalf("merge commit's second parent = %s, want the approved %s", second, approved)
	}
	if hasEvent(s, "merge_refused") {
		t.Fatal("refused a branch that had not moved")
	}
}

// An approval from before pinning (no sha) still merges the branch as it always did.
func TestMergeWithoutAPinnedShaStillWorks(t *testing.T) {
	s, tk := pinFixture(t)
	requestMerge(s, tk.ID)
	s.Tick(context.Background())
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
		t.Fatalf("status = %s", g.Status)
	}
}
