package store

import (
	"strings"
	"testing"
)

func mkTask(t *testing.T, s *Store, assignee, status string) string {
	t.Helper()
	tk, err := s.CreateTask(Task{Title: "t", Assignee: assignee, CreatedBy: "orch", Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

// goAheads are the "you can proceed" notes: what a dependency's merge sends.
func goAheads(t *testing.T, s *Store, agent string) []string {
	t.Helper()
	var out []string
	for _, b := range inboxBodies(t, s, agent) {
		if strings.Contains(b, "you can proceed") {
			out = append(out, b)
		}
	}
	return out
}

func inboxBodies(t *testing.T, s *Store, agent string) []string {
	t.Helper()
	ms, err := s.UndeliveredFor(agent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		out = append(out, m.Body)
	}
	return out
}

// An existing database from before task_deps (migration 0003) opens, migrates,
// keeps its tasks, and then supports dependencies.
func TestOldSchemaMigratesToTaskDeps(t *testing.T) {
	s, p := open(t)
	id := mkTask(t, s, "eng-1", "")
	if _, err := s.db.Exec(`DROP TABLE facts_index; DROP TABLE task_deps; ALTER TABLE tasks DROP COLUMN priority; DROP INDEX idx_usage_task; ALTER TABLE usage DROP COLUMN task_id; DELETE FROM schema_migrations WHERE version>=3`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen old schema: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.SchemaVersion(); v < 3 {
		t.Fatalf("schema version %d", v)
	}
	if _, err := s2.GetTask(id); err != nil {
		t.Fatal(err)
	}
	dep := mkTask(t, s2, "eng-2", "")
	if err := s2.SetTaskDeps(id, []string{dep}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.GetTask(id); len(got.DependsOn) != 1 || got.DependsOn[0] != dep {
		t.Fatalf("deps: %v", got.DependsOn)
	}
}

func TestMergeUnblocksDependentExactlyOnce(t *testing.T) {
	s, _ := open(t)
	a := mkTask(t, s, "eng-1", "")
	b := mkTask(t, s, "eng-2", "")
	waiter := mkTask(t, s, "eng-2", StatusBlocked)
	assigned := mkTask(t, s, "eng-1", "")
	if err := s.SetTaskDeps(waiter, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDeps(assigned, []string{a}); err != nil {
		t.Fatal(err)
	}
	merge := func(id string) {
		t.Helper()
		for _, to := range []string{StatusInProgress, StatusInReview, StatusMerged} {
			if err := s.UpdateTaskStatus(id, to); err != nil {
				t.Fatal(err)
			}
		}
	}
	merge(a)
	// entering review also told the dependents which branch to read (once each), apart from the go-ahead
	if got := inboxBodies(t, s, "eng-2"); len(got) != 1 || !strings.Contains(got[0], "dependency "+a+" is now in review") {
		t.Fatalf("eng-2 review notices: %v", got)
	}
	// waiter still waits on b: not released. assigned (only dep a) is notified, stays assigned.
	if w, _ := s.GetTask(waiter); w.Status != StatusBlocked {
		t.Fatalf("waiter released early: %s", w.Status)
	}
	if got := goAheads(t, s, "eng-2"); len(got) != 0 {
		t.Fatalf("eng-2 notified early: %v", got)
	}
	if got := goAheads(t, s, "eng-1"); len(got) != 1 || !strings.Contains(got[0], "["+assigned+"] dependency "+a+" merged; you can proceed") || !strings.Contains(got[0], "git rebase slai/integration") {
		t.Fatalf("eng-1 inbox: %v", got)
	}
	if w, _ := s.WaitingOn(waiter); len(w) != 1 || w[0] != b {
		t.Fatalf("waiting_on: %v", w)
	}
	merge(b)
	if w, _ := s.GetTask(waiter); w.Status != StatusInProgress {
		t.Fatalf("waiter should be in_progress, is %s", w.Status)
	}
	if got := goAheads(t, s, "eng-2"); len(got) != 1 || !strings.Contains(got[0], "dependency "+b+" merged") {
		t.Fatalf("eng-2 inbox: %v", got)
	}
	// terminal states cannot repeat, and an unrelated cancel does not re-notify
	if err := s.UpdateTaskStatus(a, StatusMerged); err == nil {
		t.Fatal("merged must be terminal")
	}
	other := mkTask(t, s, "eng-1", "")
	_ = s.UpdateTaskStatus(other, StatusCancelled)
	if got := goAheads(t, s, "eng-2"); len(got) != 1 {
		t.Fatalf("duplicate notice: %v", got)
	}
}

func TestCancelledDependencySatisfies(t *testing.T) {
	s, _ := open(t)
	dep := mkTask(t, s, "eng-1", "")
	w := mkTask(t, s, "eng-2", StatusBlocked)
	_ = s.SetTaskDeps(w, []string{dep})
	if err := s.UpdateTaskStatus(dep, StatusCancelled); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTask(w); got.Status != StatusInProgress {
		t.Fatalf("status %s", got.Status)
	}
	if b := inboxBodies(t, s, "eng-2"); len(b) != 1 || !strings.Contains(b[0], "cancelled") {
		t.Fatalf("inbox %v", b)
	}
}

func TestSetTaskDepsValidation(t *testing.T) {
	s, _ := open(t)
	a, b := mkTask(t, s, "x", ""), mkTask(t, s, "x", "")
	if err := s.SetTaskDeps(a, []string{a}); err == nil {
		t.Fatal("self dep")
	}
	if err := s.SetTaskDeps(a, []string{"T-9999"}); err == nil {
		t.Fatal("unknown dep")
	}
	if err := s.SetTaskDeps(a, []string{b}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskDeps(b, []string{a}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle not rejected: %v", err)
	}
}

// AC 1 regression: a dependent with two dependencies is released, and told, once -
// by the last of them.
func TestDependentWithTwoDependenciesGetsOneGoAheadNote(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	a, b := mkTask(t, s, "eng-1", StatusInReview), mkTask(t, s, "eng-1", StatusInReview)
	c := mkTask(t, s, "eng-2", StatusBlocked)
	if err := s.SetTaskDeps(c, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(a, StatusMerged); err != nil {
		t.Fatal(err)
	}
	for _, body := range inboxBodies(t, s, "eng-2") {
		if strings.Contains(body, "you can proceed") {
			t.Fatalf("released while %s is still open: %s", b, body)
		}
	}
	if err := s.UpdateTaskStatusNotes(b, StatusMerged, map[string]string{c: "slai rebased your branch for you."}); err != nil {
		t.Fatal(err)
	}
	var go1 []string
	for _, body := range inboxBodies(t, s, "eng-2") {
		if strings.Contains(body, "you can proceed") {
			go1 = append(go1, body)
		}
	}
	if len(go1) != 1 || !strings.Contains(go1[0], "slai rebased your branch for you.") || strings.Contains(go1[0], "First bring your branch up to date") {
		t.Fatalf("go-ahead notes = %v", go1)
	}
	if tk, _ := s.GetTask(c); tk.Status != StatusInProgress {
		t.Fatalf("dependent status = %s", tk.Status)
	}
}

func TestPendingReleasesListsOnlyWhatThisMergeWouldRelease(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	a, b := mkTask(t, s, "eng-1", StatusInReview), mkTask(t, s, "eng-1", StatusInProgress)
	onlyA, both, started := mkTask(t, s, "eng-2", StatusBlocked), mkTask(t, s, "eng-2", StatusAssigned), mkTask(t, s, "eng-2", StatusInProgress)
	_ = s.SetTaskDeps(onlyA, []string{a})
	_ = s.SetTaskDeps(both, []string{a, b})
	_ = s.SetTaskDeps(started, []string{a})
	got, err := s.PendingReleases(a)
	if err != nil || len(got) != 1 || got[0].ID != onlyA || got[0].Assignee != "eng-2" {
		t.Fatalf("PendingReleases(%s) = %+v %v, want only %s", a, got, err, onlyA)
	}
}

// AC 2: a dependency entering review tells each held dependent's assignee, once, which
// branch to read.
func TestDependencyEnteringReviewNotesTheDependentsOnce(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	dep := mkTask(t, s, "eng-1", StatusInProgress)
	_ = s.UpdateTaskFields(dep, Task{Branch: "slai/eng-1/" + dep})
	held, started := mkTask(t, s, "eng-2", StatusAssigned), mkTask(t, s, "eng-2", StatusInProgress)
	_ = s.SetTaskDeps(held, []string{dep})
	_ = s.SetTaskDeps(started, []string{dep})
	count := func() (n int, body string) {
		for _, b := range inboxBodies(t, s, "eng-2") {
			if strings.Contains(b, "dependency "+dep+" is now in review") {
				n, body = n+1, b
			}
		}
		return
	}
	if err := s.UpdateTaskStatus(dep, StatusInReview); err != nil {
		t.Fatal(err)
	}
	n, body := count()
	if n != 1 || !strings.Contains(body, "["+held+"]") || !strings.Contains(body, "branch slai/eng-1/"+dep) || !strings.Contains(body, "git diff slai/integration...slai/eng-1/"+dep) {
		t.Fatalf("notes = %d: %s", n, body)
	}
	// review bounces back and forth: still just the one note
	for _, to := range []string{StatusChangesRequested, StatusInProgress, StatusInReview} {
		if err := s.UpdateTaskStatus(dep, to); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := count(); n != 1 {
		t.Fatalf("notes after review round trips = %d, want 1", n)
	}
	// and it is not the go-ahead: the dependent stays held
	if tk, _ := s.GetTask(held); tk.Status != StatusAssigned {
		t.Fatalf("held task moved to %s", tk.Status)
	}
}

// A reverted dependency counts like a cancelled one: the work was withdrawn, so a task
// depending on it is no longer held back (the orchestrator is told by the revert notice).
func TestRevertedDependencyNoLongerHoldsDependents(t *testing.T) {
	s, _ := open(t)
	dep := mkTask(t, s, "eng-1", "")
	waiter := mkTask(t, s, "eng-2", "")
	if err := s.SetTaskDeps(waiter, []string{dep}); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.WaitingOn(waiter); len(w) != 1 {
		t.Fatalf("should wait on the open dependency: %v", w)
	}
	if err := s.UpdateTaskStatus(dep, StatusInProgress); err != nil {
		t.Fatal(err)
	}
	_ = s.UpdateTaskStatus(dep, StatusInReview)
	if err := s.UpdateTaskStatus(dep, StatusMerged); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReverted(dep); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.WaitingOn(waiter); len(w) != 0 {
		t.Fatalf("still waiting on a reverted dependency: %v", w)
	}
	if !DepSatisfied(StatusReverted) {
		t.Fatal("DepSatisfied(reverted) = false")
	}
	if err := s.MarkReverted(dep); err == nil {
		t.Fatal("reverting twice must fail")
	}
}
