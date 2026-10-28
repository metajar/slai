package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// switchEnv: eng-1's worktree is on task A's branch; task C (eng-2) is what B waits on.
type switchEnv struct {
	s          *Supervisor
	wt         string
	a, b, c    store.Task
	branchOfFn func(store.Task) string
}

func newSwitchEnv(t *testing.T) *switchEnv {
	t.Helper()
	s := unitEnv(t, team(5))
	sh(t, s.Dir, "git", "branch", "slai/integration")
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	for _, id := range []string{"eng-1", "eng-2"} {
		if err := s.Store.UpsertAgent(store.Agent{ID: id, Role: "engineer"}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(title, assignee, status string) store.Task {
		tk, err := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		br := TaskBranch(assignee, tk.ID)
		_ = s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: br})
		tk.Branch = br
		sh(t, s.Dir, "git", "branch", br, "slai/integration")
		return tk
	}
	e := &switchEnv{s: s, wt: wt}
	e.a = mk("A", "eng-1", store.StatusInProgress)
	e.c = mk("C", "eng-2", store.StatusInProgress)
	e.b = mk("B", "eng-1", store.StatusBlocked)
	if err := s.Store.SetTaskDeps(e.b.ID, []string{e.c.ID}); err != nil {
		t.Fatal(err)
	}
	sh(t, s.Dir, "git", "worktree", "add", wt, e.a.Branch)
	_ = s.Store.SetAgentTask("eng-1", e.a.ID)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	return e
}

func (e *switchEnv) branch(t *testing.T) string {
	return strings.TrimSpace(sh(t, e.wt, "git", "rev-parse", "--abbrev-ref", "HEAD"))
}

func (e *switchEnv) current(t *testing.T) string {
	a, err := e.s.Store.AgentByID("eng-1")
	if err != nil {
		t.Fatal(err)
	}
	return a.CurrentTask
}

func (e *switchEnv) assignB(t *testing.T) {
	t.Helper()
	id, err := e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: e.b.ID, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !e.s.prepareAssignments(context.Background(), store.Agent{ID: "eng-1", Worktree: e.wt}, []store.Message{{ID: id, Kind: "assignment", TaskID: e.b.ID, To: "eng-1"}}) {
		t.Fatal("prepare failed")
	}
}

func (e *switchEnv) mergeC(t *testing.T) {
	t.Helper()
	for _, st := range []string{store.StatusInReview, store.StatusMerged} {
		if err := e.s.Store.UpdateTaskStatus(e.c.ID, st); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *switchEnv) dirty(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.wt, "wip.txt"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *switchEnv) noStash(t *testing.T) {
	t.Helper()
	if out := strings.TrimSpace(sh(t, e.wt, "git", "stash", "list")); out != "" {
		t.Fatalf("something was stashed: %s", out)
	}
}

func (e *switchEnv) tick() { e.s.worktreeTick(context.Background()) }

// AC 1: a blocked assignment never stashes, checks out, or moves current_task.
func TestBlockedAssignmentLeavesWorktreeAlone(t *testing.T) {
	e := newSwitchEnv(t)
	e.dirty(t)
	e.assignB(t)
	if got, err := e.s.Store.ClaimInbox("eng-1"); err != nil || len(got) != 1 || got[0].TaskID != e.b.ID {
		t.Fatalf("queued task notice must be readable: %+v, %v", got, err)
	}
	if got := e.branch(t); got != e.a.Branch {
		t.Fatalf("worktree moved to %s", got)
	}
	if _, err := os.Stat(filepath.Join(e.wt, "wip.txt")); err != nil {
		t.Fatalf("uncommitted work was removed: %v", err)
	}
	e.noStash(t)
	if got := e.current(t); got != e.a.ID {
		t.Fatalf("current_task = %s", got)
	}
	e.mergeC(t) // B is now in_progress too, but the board keeps showing the task on the checked-out branch
	ags, _ := e.s.Store.GetAgents()
	for _, ag := range ags {
		if ag.ID == "eng-1" && ag.CurrentTask != e.a.ID {
			t.Fatalf("board current_task = %s, want %s", ag.CurrentTask, e.a.ID)
		}
	}
}

// AC 2: deps merge while the current task is in review: switch and update current_task.
func TestUnblockedTaskSwitchesWhenCurrentInReview(t *testing.T) {
	e := newSwitchEnv(t)
	e.assignB(t)
	if err := e.s.Store.UpdateTaskStatus(e.a.ID, store.StatusInReview); err != nil {
		t.Fatal(err)
	}
	e.mergeC(t)
	e.tick()
	if got := e.branch(t); got != e.b.Branch {
		t.Fatalf("worktree on %s, want %s", got, e.b.Branch)
	}
	if got := e.current(t); got != e.b.ID {
		t.Fatalf("current_task = %s", got)
	}
	msgs, _ := e.s.Store.UndeliveredFor("eng-1")
	found := false
	for _, m := range msgs {
		found = found || (m.TaskID == e.b.ID && strings.Contains(m.Body, "switched your worktree"))
	}
	if !found {
		t.Fatalf("no switch note: %+v", msgs)
	}
}

// AC 3: deps merge mid-task: no switch until the current task enters review.
func TestUnblockedTaskWaitsForCurrentTaskToReachReview(t *testing.T) {
	e := newSwitchEnv(t)
	e.assignB(t)
	e.dirty(t)
	e.mergeC(t)
	e.tick()
	if e.branch(t) != e.a.Branch || e.current(t) != e.a.ID {
		t.Fatalf("switched mid-task: %s / %s", e.branch(t), e.current(t))
	}
	e.noStash(t)
	_ = os.Remove(filepath.Join(e.wt, "wip.txt"))
	if err := e.s.Store.UpdateTaskStatus(e.a.ID, store.StatusInReview); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if e.branch(t) != e.b.Branch || e.current(t) != e.b.ID {
		t.Fatalf("not switched after review: %s / %s", e.branch(t), e.current(t))
	}
}

func TestStartableAssignmentStaysHeldBehindActiveTaskUntilSwitch(t *testing.T) {
	e := newSwitchEnv(t)
	n := newTask(t, e, store.StatusAssigned)
	id, err := e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: n.ID, Body: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if !e.s.prepareAssignments(context.Background(), store.Agent{ID: "eng-1", Worktree: e.wt}, []store.Message{{ID: id, Kind: "assignment", TaskID: n.ID, To: "eng-1"}}) {
		t.Fatal("preparing a queued task should not fail")
	}
	if got, err := e.s.Store.ClaimInbox("eng-1"); err != nil || len(got) != 0 {
		t.Fatalf("assignment visible on the previous task's branch: %+v, %v", got, err)
	}
	if e.branch(t) != e.a.Branch {
		t.Fatalf("switched away from active task: %s", e.branch(t))
	}
	if err := e.s.Store.UpdateTaskStatus(e.a.ID, store.StatusInReview); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if e.branch(t) != n.Branch {
		t.Fatalf("queued task not switched: %s", e.branch(t))
	}
	if got, err := e.s.Store.ClaimInbox("eng-1"); err != nil || len(got) != 2 || got[0].ID != id || got[1].Kind != "note" {
		t.Fatalf("assignment and switch notice not released after switch: %+v, %v", got, err)
	}
}

// AC 4: changes requested on a task whose branch is not checked out.
func TestChangesRequestedSwitchesBackOnlyWhenClean(t *testing.T) {
	setup := func(t *testing.T) *switchEnv {
		e := newSwitchEnv(t)
		e.assignB(t)
		for _, st := range []string{store.StatusInReview} {
			_ = e.s.Store.UpdateTaskStatus(e.a.ID, st)
		}
		e.mergeC(t)
		e.tick() // now on B
		if e.branch(t) != e.b.Branch {
			t.Fatalf("setup: on %s", e.branch(t))
		}
		if err := e.s.Store.UpdateTaskStatus(e.a.ID, store.StatusChangesRequested); err != nil {
			t.Fatal(err)
		}
		return e
	}
	t.Run("clean", func(t *testing.T) {
		e := setup(t)
		e.tick()
		if e.branch(t) != e.a.Branch || e.current(t) != e.a.ID {
			t.Fatalf("not switched back: %s / %s", e.branch(t), e.current(t))
		}
	})
	t.Run("dirty", func(t *testing.T) {
		e := setup(t)
		e.dirty(t)
		e.tick()
		e.tick() // the note is sent once
		if e.branch(t) != e.b.Branch || e.current(t) != e.b.ID {
			t.Fatalf("switched with dirty tree: %s / %s", e.branch(t), e.current(t))
		}
		e.noStash(t)
		msgs, _ := e.s.Store.UndeliveredFor("eng-1")
		n := 0
		for _, m := range msgs {
			if m.TaskID == e.a.ID && strings.Contains(m.Body, e.b.Branch) && strings.Contains(m.Body, e.a.Branch) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("want one note naming both branches, got %d: %+v", n, msgs)
		}
	})
}

// T-0091: with several startable queued tasks, the highest priority wins, then the lowest id.
func TestQueuedTaskChoiceByPriorityThenID(t *testing.T) {
	run := func(t *testing.T, prioLow, prioHigh int, wantHigh bool) {
		e := newSwitchEnv(t)
		mk := func(title string, prio int) store.Task {
			tk, err := e.s.Store.CreateTask(store.Task{Title: title, Assignee: "eng-1", Status: store.StatusInProgress})
			if err != nil {
				t.Fatal(err)
			}
			br := TaskBranch("eng-1", tk.ID)
			_ = e.s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: br})
			_ = e.s.Store.SetTaskPriority(tk.ID, prio)
			sh(t, e.s.Dir, "git", "branch", br, "slai/integration")
			tk.Branch = br
			return tk
		}
		lowID := mk("low id", prioLow)
		highID := mk("high id", prioHigh)
		if err := e.s.Store.UpdateTaskStatus(e.a.ID, store.StatusInReview); err != nil {
			t.Fatal(err)
		}
		e.tick()
		want := lowID
		if wantHigh {
			want = highID
		}
		if e.branch(t) != want.Branch || e.current(t) != want.ID {
			t.Fatalf("on %s / %s, want %s", e.branch(t), e.current(t), want.ID)
		}
	}
	t.Run("higher priority beats lower id", func(t *testing.T) { run(t, 0, 5, true) })
	t.Run("equal priority: lower id wins", func(t *testing.T) { run(t, 3, 3, false) })
}

// T-0112: a restart wipes nothing the worktree switch depends on, and an unrestored (empty)
// current_task is not "no current task".
func newTask(t *testing.T, e *switchEnv, status string) store.Task {
	t.Helper()
	tk, err := e.s.Store.CreateTask(store.Task{Title: "N", Assignee: "eng-1", Status: status})
	if err != nil {
		t.Fatal(err)
	}
	br := TaskBranch("eng-1", tk.ID)
	_ = e.s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: br})
	sh(t, e.s.Dir, "git", "branch", br, "slai/integration")
	tk.Branch = br
	return tk
}

func (e *switchEnv) finishA(t *testing.T) {
	t.Helper()
	for _, st := range []string{store.StatusInReview, store.StatusMerged} {
		if err := e.s.Store.UpdateTaskStatus(e.a.ID, st); err != nil {
			t.Fatal(err)
		}
	}
}

func notesTo(e *switchEnv, agent string) []store.Message {
	msgs, _ := e.s.Store.UndeliveredFor(agent)
	return msgs
}

// AC 1: right after a restart the stored current_task is empty. A task just assigned (the
// assignment message not yet processed) is then the engineer's derived current task, so the
// reconcile pass leaves the worktree alone: only the assignment path switches it.
func TestNoSwitchWhileCurrentTaskIsUnrestored(t *testing.T) {
	e := newSwitchEnv(t)
	e.finishA(t)
	_ = e.s.Store.SetAgentTask("eng-1", "") // `slai up` reset the row
	n := newTask(t, e, store.StatusAssigned)
	e.tick()
	if e.branch(t) != e.a.Branch {
		t.Fatalf("reconcile switched the worktree to %s while current_task was unrestored", e.branch(t))
	}
	if len(notesTo(e, "eng-1")) != 0 {
		t.Fatalf("a switch note was sent: %+v", notesTo(e, "eng-1"))
	}
	// the assignment path still does its job
	id, _ := e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: n.ID, Body: "x"})
	if !e.s.prepareAssignments(context.Background(), store.Agent{ID: "eng-1", Worktree: e.wt}, []store.Message{{ID: id, Kind: "assignment", TaskID: n.ID, To: "eng-1"}}) {
		t.Fatal("prepare failed")
	}
	if e.branch(t) != n.Branch || e.current(t) != n.ID {
		t.Fatalf("assignment did not switch: %s / %s", e.branch(t), e.current(t))
	}
}

// With the stored current_task intact (the previous task, finished), the same new task is
// switched to by the reconcile pass and the note names the previous task.
func TestSwitchNoteNamesThePreviousTask(t *testing.T) {
	e := newSwitchEnv(t)
	e.finishA(t)
	n := newTask(t, e, store.StatusAssigned)
	e.tick()
	if e.branch(t) != n.Branch {
		t.Fatalf("not switched: %s", e.branch(t))
	}
	var note string
	for _, m := range notesTo(e, "eng-1") {
		if m.TaskID == n.ID {
			note = m.Body
		}
	}
	if !strings.Contains(note, "(previous task "+e.a.ID+")") {
		t.Fatalf("note = %q", note)
	}
}

// AC 2: the note never carries an empty "(previous task )".
func TestSwitchNoteNeverHasAnEmptyPreviousTask(t *testing.T) {
	for _, prev := range []string{"", "T-0001"} {
		got := switchNote("T-0004", "slai/eng-2/T-0002", "slai/eng-2/T-0004", prev)
		if strings.Contains(got, "(previous task )") || strings.Contains(got, "()") {
			t.Errorf("prev %q: %q", prev, got)
		}
		if (prev != "") != strings.Contains(got, "previous task") {
			t.Errorf("prev %q: %q", prev, got)
		}
		if !strings.HasSuffix(got, "Start on it.") {
			t.Errorf("%q", got)
		}
	}
}

func TestRefreshPendingKeepsLateAssignmentWhenEarlierMailWasClaimed(t *testing.T) {
	e := newSwitchEnv(t)
	e.finishA(t)
	oldID, _ := e.s.Store.AddMessage(store.Message{From: "slai", To: "eng-1", Kind: "note", Body: "earlier"})
	snapshot := []store.Message{{ID: oldID, To: "eng-1", Kind: "note"}}
	if _, err := e.s.Store.ClaimInbox("eng-1"); err != nil {
		t.Fatal(err)
	}
	n := newTask(t, e, store.StatusAssigned)
	aid, _ := e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: n.ID, Body: "x"})
	got := e.s.refreshPending("eng-1", snapshot)
	if len(got) != 2 || got[1].ID != aid {
		t.Fatalf("late assignment lost when earlier mail was claimed: %+v", got)
	}
}

// T-0112: a wake pass's snapshot can be stale on a loaded machine. An assignment that arrived
// after it is picked up just before the agent is woken, so its worktree is switched before the
// agent's get_inbox can show it the task.
func TestRefreshPendingPicksUpAnAssignmentThatArrivedAfterTheSnapshot(t *testing.T) {
	e := newSwitchEnv(t)
	e.finishA(t)
	oldID, _ := e.s.Store.AddMessage(store.Message{From: "slai", To: "eng-1", Kind: "note", TaskID: e.a.ID, Body: "merged"})
	snapshot := []store.Message{{ID: oldID, To: "eng-1", Kind: "note", TaskID: e.a.ID}}
	n := newTask(t, e, store.StatusAssigned)
	aid, _ := e.s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: n.ID, Body: "x"})
	if !e.s.Store.InboxHeld(aid) {
		t.Fatal("assignment inserted during wake refresh must be held until prepared")
	}

	got := e.s.refreshPending("eng-1", snapshot)
	if len(got) != 2 || got[1].ID != aid {
		t.Fatalf("refreshed = %+v, want the note and the assignment", got)
	}
	if !e.s.prepareAssignments(context.Background(), store.Agent{ID: "eng-1", Worktree: e.wt}, got) {
		t.Fatal("prepare failed")
	}
	if e.branch(t) != n.Branch {
		t.Fatalf("worktree on %s before the agent could read the assignment, want %s", e.branch(t), n.Branch)
	}
	// never fewer than the pass already decided on
	if got := e.s.refreshPending("eng-1", append(snapshot, store.Message{ID: 9998, To: "eng-1"}, store.Message{ID: 9999, To: "eng-1"})); len(got) != 4 || got[3].ID != aid {
		t.Errorf("refresh dropped the snapshot or late assignment: %+v", got)
	}
}
