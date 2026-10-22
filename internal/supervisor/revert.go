package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// `slai revert T-xxxx` takes one merged task back out of the integration branch with
// a git revert of its merge commit. The integration checkout is the supervisor's, so
// the revert runs in the same serialised pass as merges (mergeTick holds mergeMu): the
// CLI only records a revert_requested event and waits for the outcome event.

const (
	KVRevertCursor = "revert.cursor" // last processed revert_requested event id

	EvRevertRequested = "revert_requested"
	EvReverted        = "reverted"
	EvRevertRefused   = "revert_refused"
	EvRevertFailed    = "revert_failed"
)

// RevertRequest is the data JSON of a revert_requested event.
type RevertRequest struct {
	TaskID string `json:"task_id"`
	Force  bool   `json:"force"`
	By     string `json:"by"`
}

// RevertPlan says what reverting a task would do.
type RevertPlan struct {
	Task        store.Task
	MergeSHA    string   // the merge commit that will be reverted
	Later       []string // merged tasks that depend on it (block the revert without Force)
	OpenAfter   []string // unmerged tasks that depend on it (warning only)
	Integration string
}

// RefusedError is a revert slai will not do, with the reason in plain words.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// PlanRevert checks everything that can be checked without changing anything.
func (s *Supervisor) PlanRevert(ctx context.Context, taskID string, force bool) (RevertPlan, error) {
	var p RevertPlan
	task, err := s.Store.GetTask(taskID)
	if err != nil {
		return p, &RefusedError{fmt.Sprintf("no task %s", taskID)}
	}
	p.Task = task
	p.Integration = s.Team.Project.IntegrationBranch
	switch task.Status {
	case store.StatusMerged:
	case store.StatusReverted:
		return p, &RefusedError{fmt.Sprintf("%s was already reverted", taskID)}
	default:
		return p, &RefusedError{fmt.Sprintf("%s is %s, not merged: there is nothing on %s to revert", taskID, task.Status, p.Integration)}
	}
	wt := s.IntegrationWorktree()
	if err := s.Git.AddWorktree(ctx, wt, p.Integration, s.Team.Project.MainBranch); err != nil {
		return p, fmt.Errorf("integration checkout: %w", err)
	}
	repo := &git.Repo{Dir: wt}
	if dirty, err := repo.IsDirty(ctx); err != nil {
		return p, err
	} else if dirty {
		return p, &RefusedError{"the integration checkout (" + wt + ") has uncommitted changes; slai will not revert on top of them. Look at `git -C " + wt + " status`, clean it up, and try again"}
	}
	p.MergeSHA = repo.MergeCommit(ctx, p.Integration, taskID)
	if p.MergeSHA == "" {
		return p, &RefusedError{fmt.Sprintf("could not find the merge commit of %s on %s", taskID, p.Integration)}
	}
	all, err := s.Store.ListTasks("")
	if err != nil {
		return p, err
	}
	for _, t := range all {
		for _, d := range t.DependsOn {
			if d != taskID {
				continue
			}
			switch t.Status {
			case store.StatusMerged:
				p.Later = append(p.Later, t.ID)
			case store.StatusCancelled, store.StatusReverted, store.StatusDone:
			default:
				p.OpenAfter = append(p.OpenAfter, t.ID)
			}
		}
	}
	if len(p.Later) > 0 && !force {
		return p, &RefusedError{fmt.Sprintf("%s already merged and depend on %s: reverting it would leave them without code they need. Revert those first, or pass --force to revert %s anyway",
			strings.Join(p.Later, ", "), taskID, taskID)}
	}
	return p, nil
}

// revertTick performs the queued reverts. The caller holds mergeMu.
func (s *Supervisor) revertTick(ctx context.Context) {
	cur := int64(atoi(s.kvGet(KVRevertCursor)))
	evs, err := s.Store.EventsAfter(cur, EvRevertRequested, 10)
	if err != nil {
		s.logf("revert: %v", err)
		return
	}
	for _, ev := range evs {
		if ctx.Err() != nil {
			return
		}
		s.revertOne(ctx, ev)
		_ = s.Store.SetKV(KVRevertCursor, strconv.FormatInt(ev.ID, 10))
	}
}

func (s *Supervisor) revertOne(ctx context.Context, ev store.Event) {
	var req RevertRequest
	if json.Unmarshal([]byte(ev.Data), &req) != nil || req.TaskID == "" {
		s.event("", EvRevertFailed, map[string]any{"request": ev.ID, "error": "bad revert_requested data"})
		return
	}
	outcome := func(kind, text string, extra map[string]any) {
		d := map[string]any{"task_id": req.TaskID, "request": ev.ID, "text": text}
		for k, v := range extra {
			d[k] = v
		}
		s.event(req.By, kind, d)
	}
	plan, err := s.PlanRevert(ctx, req.TaskID, req.Force)
	var ref *RefusedError
	switch {
	case errors.As(err, &ref):
		outcome(EvRevertRefused, ref.Reason, nil)
		return
	case err != nil:
		outcome(EvRevertFailed, err.Error(), nil)
		return
	}
	title := strings.TrimSpace(plan.Task.Title)
	repo := &git.Repo{Dir: s.IntegrationWorktree()}
	if err := repo.RevertMerge(ctx, plan.MergeSHA, fmt.Sprintf("Revert [%s] %s", plan.Task.ID, title)); err != nil {
		outcome(EvRevertFailed, fmt.Sprintf("git could not revert the merge of %s (nothing was changed): %v", plan.Task.ID, err), nil)
		return
	}
	if err := s.Store.MarkReverted(plan.Task.ID); err != nil {
		outcome(EvRevertFailed, fmt.Sprintf("reverted on %s but could not mark the task: %v", plan.Integration, err), nil)
		return
	}
	msg := fmt.Sprintf("[%s] %q was REVERTED off %s (revert of merge %s). Its code is no longer in the integration branch; the task is marked reverted.", plan.Task.ID, title, plan.Integration, plan.MergeSHA)
	if len(plan.Later) > 0 {
		msg += " Forced: " + strings.Join(plan.Later, ", ") + " already merged and depend on it and may no longer build."
	}
	s.notifyOutcome(plan.Task, req.By, msg)
	outcome(EvReverted, msg, map[string]any{"merge": plan.MergeSHA})
}
