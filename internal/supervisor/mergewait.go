package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
)

// Less waiting on merges (improvements.md item 7). Engineers used to report(blocked)
// "waiting on T-x to merge" and needed the orchestrator to ping them. Now:
//   - when a dependency enters review its dependents' assignees are told which branch
//     it is on (store.noteDependentsOfReview);
//   - when it merges, a released dependent's branch is rebased onto the integration
//     branch if that is clean, and the one go-ahead note says what happened;
//   - a report(blocked) that names an already-merged task is answered by the supervisor
//     within a tick and the task goes back to in_progress;
//   - the orchestrator digest lists tasks waiting on a merge apart from stuck ones.

// dependentNotes rebases, where it can do so safely, the branch of every task the merge
// of dep releases, and returns what to tell each assignee (dependent id -> sentence).
// It runs after the merge commit exists and before the dependency's status flips, so the
// go-ahead note is a single note that already carries the outcome. Nothing it does can
// lose work: it only rebases a task branch that is checked out in its assignee's own
// worktree with no uncommitted changes, and an unclean rebase is aborted, leaving the
// branch exactly as it was.
func (s *Supervisor) dependentNotes(ctx context.Context, dep store.Task) map[string]string {
	ds, err := s.Store.PendingReleases(dep.ID)
	if err != nil || len(ds) == 0 {
		return nil
	}
	integration := s.Team.Project.IntegrationBranch
	notes := map[string]string{}
	for _, d := range ds {
		if d.Assignee == "" {
			continue
		}
		branch := d.Branch
		if branch == "" {
			branch = TaskBranch(d.Assignee, d.ID)
		}
		repo := &git.Repo{Dir: filepath.Join(s.Dir, ".slai", "worktrees", d.Assignee)}
		if cur, err := repo.CurrentBranch(ctx); err != nil || cur != branch {
			continue // not started on this branch: the generic advice (rebase when you start) applies
		}
		upToDate, err := repo.RebaseOnto(ctx, integration)
		var ce *git.ConflictError
		switch {
		case err == nil && upToDate:
			notes[d.ID] = fmt.Sprintf("Your branch %s already contains %s: nothing to rebase.", branch, integration)
			s.event(d.Assignee, "dependent_rebased", map[string]any{"task_id": d.ID, "dependency": dep.ID, "result": "up_to_date"})
		case err == nil:
			notes[d.ID] = fmt.Sprintf("slai rebased your branch %s onto %s for you (it was clean): nothing to do, continue.", branch, integration)
			s.event(d.Assignee, "dependent_rebased", map[string]any{"task_id": d.ID, "dependency": dep.ID, "result": "rebased"})
		case errors.As(err, &ce):
			notes[d.ID] = fmt.Sprintf("slai tried to rebase %s onto %s but it conflicts in %s, so nothing was changed. Run `git rebase %s` in your worktree and resolve it.",
				branch, integration, strings.Join(ce.Files, ", "), integration)
			s.event(d.Assignee, "dependent_rebased", map[string]any{"task_id": d.ID, "dependency": dep.ID, "result": "conflict", "files": ce.Files})
		default:
			notes[d.ID] = fmt.Sprintf("slai did not rebase %s (%s). Run `git rebase %s` yourself after committing or stashing your changes.", branch, firstErrLine(err), integration)
			s.event(d.Assignee, "dependent_rebased", map[string]any{"task_id": d.ID, "dependency": dep.ID, "result": "skipped", "error": err.Error()})
		}
	}
	return notes
}

func firstErrLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

var taskIDRe = regexp.MustCompile(`\bT-\d+\b`)

// blockedReportsTick answers a report(blocked) that names a task which has already
// merged: the engineer is waiting on something that is no longer in the way (the
// go-ahead note for declared dependencies covers only depends_on). The task goes back
// to in_progress and the engineer gets one note with the merge commit. It runs every
// tick, so the answer comes within one.
func (s *Supervisor) blockedReportsTick(ctx context.Context) {
	blocked, err := s.Store.ListTasks(store.StatusBlocked)
	if err != nil || len(blocked) == 0 {
		return
	}
	repo := &git.Repo{Dir: s.IntegrationWorktree()}
	for _, t := range blocked {
		if t.Assignee == "" {
			continue
		}
		evs, err := s.Store.TaskEvents(t.ID, 20)
		if err != nil {
			continue
		}
		var summary string
		for i := len(evs) - 1; i >= 0; i-- { // newest report wins
			if evs[i].Kind != "report" || evs[i].Agent != t.Assignee {
				continue
			}
			var d struct{ Status, Summary string }
			if json.Unmarshal([]byte(evs[i].Data), &d) == nil && d.Status == "blocked" {
				summary = d.Summary
			}
			break
		}
		if summary == "" {
			continue
		}
		for _, id := range taskIDRe.FindAllString(summary, -1) {
			if id == t.ID {
				continue
			}
			other, err := s.Store.GetTask(id)
			if err != nil || other.Status != store.StatusMerged {
				continue
			}
			key := "blockedans." + t.ID + "." + id
			if s.kvGet(key) != "" {
				continue
			}
			_ = s.Store.SetKV(key, "1")
			sha := repo.MergeCommit(ctx, s.Team.Project.IntegrationBranch, id)
			at := "into " + s.Team.Project.IntegrationBranch
			if sha != "" {
				at = "at " + sha
			}
			if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInProgress); err != nil {
				s.logf("blocked answer %s: %v", t.ID, err)
				continue
			}
			s.notify(t.Assignee, t.ID, fmt.Sprintf("[%s] %s merged %s; rebase and continue (git rebase %s). Your task is back in progress.",
				t.ID, id, at, s.Team.Project.IntegrationBranch))
			s.event(t.Assignee, "blocked_answered", map[string]any{"task_id": t.ID, "merged": id, "sha": sha})
			break
		}
	}
}

// waitingOnMerge lists the open tasks that are held only by dependencies that have not
// merged yet ("T-0004 waits for T-0003 [in_review]"), and the others.
func waitingOnMerge(tasks []store.Task) (waiting, rest []store.Task, reasons map[string]string) {
	byID := map[string]store.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	reasons = map[string]string{}
	for _, t := range tasks {
		if !openTask(t) {
			continue
		}
		var holding []string
		if t.Status == store.StatusAssigned || t.Status == store.StatusBlocked {
			for _, d := range t.DependsOn {
				if dt, ok := byID[d]; ok && !store.DepSatisfied(dt.Status) {
					holding = append(holding, fmt.Sprintf("%s [%s]", d, dt.Status))
				}
			}
		}
		if len(holding) > 0 {
			waiting = append(waiting, t)
			reasons[t.ID] = strings.Join(holding, ", ")
		} else {
			rest = append(rest, t)
		}
	}
	return waiting, rest, reasons
}
