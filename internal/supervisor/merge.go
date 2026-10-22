package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// A merge that fails for a reason other than a conflict (git hiccup, locked index,
// branch checked out elsewhere) is retried, not dropped: the principal's approval
// must not be lost to a transient error.
const (
	MergeRetries    = 4
	MergeRetryEvery = 20 * time.Second
	kvMergeRetry    = "merge.retry." // + event id: "<count>|<last unix>"
)

// MergeRequest is the data JSON of a "merge_requested" event.
type MergeRequest struct {
	TaskID string `json:"task_id"`
	Notes  string `json:"notes"`
	// SHA is the commit that was reviewed and approved; the merge takes exactly it.
	SHA string `json:"sha"`
}

// IntegrationWorktree is the dedicated checkout of the integration branch.
func (s *Supervisor) IntegrationWorktree() string {
	return filepath.Join(s.Dir, ".slai", "worktrees", "_integration")
}

func (s *Supervisor) mergeTick(ctx context.Context) {
	s.rt.mergeMu.Lock()
	defer s.rt.mergeMu.Unlock()
	cur := int64(0)
	if v, ok, _ := s.Store.GetKV(KVMergeCursor); ok {
		cur, _ = strconv.ParseInt(v, 10, 64)
	}
	evs, err := s.Store.EventsAfter(cur, "merge_requested", 20)
	if err != nil {
		s.logf("merge: %v", err)
		return
	}
	for _, ev := range evs {
		if ctx.Err() != nil {
			return
		}
		if !s.mergeOne(ctx, ev) {
			return // transient failure: keep the approval, keep order, retry on a later tick
		}
		if err := s.Store.SetKV(KVMergeCursor, strconv.FormatInt(ev.ID, 10)); err != nil {
			s.logf("merge cursor: %v", err)
			return
		}
	}
	s.revertTick(ctx) // reverts share the merge lock and run after the merges queued before them
}

// notifyOutcome tells everyone involved in a merge: the assignee, the approver
// (the agent whose approval queued the merge) and the orchestrator, once each.
func (s *Supervisor) notifyOutcome(task store.Task, approver, body string, skip ...string) {
	seen := map[string]bool{"slai": true}
	for _, k := range skip {
		seen[k] = true
	}
	for _, to := range []string{task.Assignee, approver, s.orchestrator()} {
		if to == "" || seen[to] {
			continue
		}
		seen[to] = true
		s.notify(to, task.ID, body)
	}
}

func (s *Supervisor) notify(to, taskID, body string) {
	if to == "" {
		return
	}
	_, _ = s.Store.AddMessage(store.Message{From: "slai", To: to, Kind: "note", TaskID: taskID, Body: body})
}

// mergeOne handles one approval. It returns false when the request should be
// retried later (the cursor then stays put).
func (s *Supervisor) mergeOne(ctx context.Context, ev store.Event) bool {
	var req MergeRequest
	if err := json.Unmarshal([]byte(ev.Data), &req); err != nil || req.TaskID == "" {
		s.event("", "merge_error", map[string]any{"event_id": ev.ID, "error": "bad merge_requested data"})
		return true
	}
	task, err := s.Store.GetTask(req.TaskID)
	if err != nil {
		s.event("", "merge_error", map[string]any{"task_id": req.TaskID, "error": err.Error()})
		return true
	}
	switch task.Status {
	case store.StatusMerged:
		return true
	case store.StatusCancelled, store.StatusDone:
		s.event(task.Assignee, "merge_skipped", map[string]any{"task_id": task.ID, "status": task.Status})
		return true // approved, then cancelled: never merge cancelled work
	}
	branch := task.Branch
	if branch == "" {
		branch = TaskBranch(task.Assignee, task.ID)
	}
	// Merge exactly what was approved. A branch that gained commits after the approval
	// has changes nobody reviewed: refuse, keep it in review, and ask for a new review.
	toMerge := branch
	if req.SHA != "" {
		tip, err := s.Git.RevParse(ctx, branch)
		if err == nil && tip != req.SHA {
			s.refuseMovedBranch(task, ev, branch, req.SHA, tip)
			return true
		}
		toMerge = req.SHA
	}
	// fail records a transient error; after MergeRetries the task goes back to its
	// engineer and everyone involved is told, so nothing is silently stuck in review.
	fail := func(err error) bool {
		n, last := 0, int64(0)
		if v := s.kvGet(kvMergeRetry + strconv.FormatInt(ev.ID, 10)); v != "" {
			f := strings.SplitN(v, "|", 2)
			n = atoi(f[0])
			if len(f) == 2 {
				last, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
		now := s.now()
		if last != 0 && now.Sub(time.Unix(last, 0)) < MergeRetryEvery {
			return false // not yet: still backing off
		}
		n++
		_ = s.Store.SetKV(kvMergeRetry+strconv.FormatInt(ev.ID, 10), fmt.Sprintf("%d|%d", n, now.Unix()))
		s.event(task.Assignee, "merge_error", map[string]any{"task_id": task.ID, "error": err.Error(), "attempt": n})
		if n < MergeRetries {
			return false
		}
		_ = s.Store.UpdateTaskStatus(task.ID, store.StatusInProgress)
		msg := fmt.Sprintf("[%s] could not be merged into %s after %d attempts: %v. It is back in progress. Re-run request_review once the engineer has checked their branch (%s).", task.ID, s.Team.Project.IntegrationBranch, n, err, branch)
		s.notifyOutcome(task, ev.Agent, msg)
		for _, a := range s.Team.Agents {
			if a.Role == config.RolePrincipal && a.ID != ev.Agent && a.ID != task.Assignee && a.ID != s.orchestrator() {
				s.notify(a.ID, task.ID, msg)
			}
		}
		return true
	}
	wt := s.IntegrationWorktree()
	if err := s.Git.AddWorktree(ctx, wt, s.Team.Project.IntegrationBranch, s.Team.Project.MainBranch); err != nil {
		return fail(err)
	}
	repo := &git.Repo{Dir: wt}
	if err := repo.ResetClean(ctx); err != nil { // slai owns this checkout: start every merge from a clean tree
		return fail(err)
	}
	title := strings.TrimSpace(task.Title)
	// "Merge [T-1] title": the task's own commits are usually "[T-1] ..." too, and an
	// identical subject on the merge commit reads as the work landing twice.
	mergeMsg := fmt.Sprintf("Merge [%s] %s", task.ID, title)
	if s.Team.Project.TestCommand != "" {
		err = s.gatedMerge(ctx, repo, toMerge, mergeMsg) // tests run on the result before integration moves
	} else {
		err = repo.MergeNoFF(ctx, toMerge, mergeMsg)
	}
	var ce *git.ConflictError
	var ge *GateError
	switch {
	case errors.As(err, &ge):
		s.refuseFailedGate(task, ev, branch, ge)
	case err == nil:
		notes := s.dependentNotes(ctx, task) // rebase what this merge releases, if clean
		if err := s.Store.UpdateTaskStatusNotes(task.ID, store.StatusMerged, notes); err != nil {
			return fail(err)
		}
		msg := fmt.Sprintf("[%s] %q was merged into %s.", task.ID, title, s.Team.Project.IntegrationBranch)
		s.notifyOutcome(task, ev.Agent, msg)
		s.event(task.Assignee, "merged", map[string]any{"task_id": task.ID, "branch": branch})
	case errors.As(err, &ce):
		_ = s.Store.UpdateTaskStatus(task.ID, store.StatusInProgress) // illegal from other states: ignored
		s.notify(task.Assignee, task.ID, fmt.Sprintf(
			"[%s] merge into %s conflicted in: %s. Merge %s into your branch in your worktree, resolve, commit, then report again.",
			task.ID, s.Team.Project.IntegrationBranch, strings.Join(ce.Files, ", "), s.Team.Project.IntegrationBranch))
		s.notifyOutcome(task, ev.Agent, fmt.Sprintf("[%s] merge into %s conflicted in: %s; sent back to %s.",
			task.ID, s.Team.Project.IntegrationBranch, strings.Join(ce.Files, ", "), task.Assignee), task.Assignee) // the assignee got the detailed note above
		s.event(task.Assignee, "merge_conflict", map[string]any{"task_id": task.ID, "branch": branch, "files": ce.Files})
	default:
		return fail(err)
	}
	return true
}

// refuseMovedBranch handles an approval whose branch gained commits afterwards. The
// task stays in review (it is moved back there if it was somewhere else), the engineer
// is told to request review again, and the principals are told a re-review is coming.
// The approval event is consumed, so this happens once; a new request_review pins the
// new head and a fresh approval merges it.
func (s *Supervisor) refuseMovedBranch(task store.Task, ev store.Event, branch, approved, tip string) {
	short := func(x string) string {
		if len(x) > 8 {
			return x[:8]
		}
		return x
	}
	if task.Status != store.StatusInReview {
		_ = s.Store.UpdateTaskStatus(task.ID, store.StatusInReview)
	}
	s.event(task.Assignee, "merge_refused", map[string]any{"task_id": task.ID, "branch": branch, "approved": approved, "tip": tip,
		"cause": "the branch gained commits after the approval"})
	s.notify(task.Assignee, task.ID, fmt.Sprintf(
		"[%s] new commits after approval: the principal approved %s but %s is now at %s, so nothing was merged. Call request_review again so the new commits are reviewed.",
		task.ID, short(approved), branch, short(tip)))
	seen := map[string]bool{task.Assignee: true}
	tell := func(to string) {
		if to == "" || seen[to] {
			return
		}
		seen[to] = true
		s.notify(to, task.ID, fmt.Sprintf("[%s] %s had commits added after you approved %s (branch now at %s). It was NOT merged; expect a new review request from %s and review it again.",
			task.ID, branch, short(approved), short(tip), task.Assignee))
	}
	tell(ev.Agent)
	for _, a := range s.Team.Agents {
		if a.Role == config.RolePrincipal {
			tell(a.ID)
		}
	}
}
