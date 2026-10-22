package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// memoryPointer is the one sentence a wake line carries about the team's memory:
//
//	memory: N facts for this task, see get_task.memory or get_memory
//
// It never carries a fact. The pack itself reaches the agent through get_task and
// get_memory (the same string for every agent harness), and the memory_used event is
// recorded there, when a pack is actually returned; typing this sentence delivers nothing.
// N is the number of facts in the pack the task would get. A task that lists files and has
// no fact about them gets no sentence (there is nothing to point at); a task with no files
// always does, since its pack leads with the project map.
func (s *Supervisor) memoryPointer(ctx context.Context, taskID string) string {
	if taskID == "" {
		return ""
	}
	t, err := s.Store.GetTask(taskID)
	if err != nil {
		return ""
	}
	var files []string
	_ = json.Unmarshal([]byte(t.Files), &files)
	m := memory.New(filepath.Join(s.Dir, ".slai", "memory"), s.Store, s.Git, s.Team.Project.IntegrationBranch)
	p, err := m.BuildPack(ctx, memory.TeamPackOptions(s.Team, s.Dir, files, ""))
	if err != nil {
		return ""
	}
	n := len(p.FactIDs)
	if n == 0 && len(files) > 0 {
		return ""
	}
	noun := "facts"
	if n == 1 {
		noun = "fact"
	}
	return fmt.Sprintf("memory: %d %s for this task, see get_task.memory or get_memory", n, noun)
}

// wakeMemoryTask is the task a wake line should point at: the task of the oldest
// assignment among the items being delivered, else (for a restart or upgrade wake) the
// agent's current task, else its oldest open one.
func (s *Supervisor) wakeMemoryTask(a store.Agent, msgs []store.Message, afterRestart bool) string {
	best := ""
	var bestID int64
	for _, m := range msgs {
		if m.Kind == "assignment" && m.TaskID != "" && (best == "" || m.ID < bestID) {
			best, bestID = m.TaskID, m.ID
		}
	}
	if best != "" || !afterRestart {
		return best
	}
	if a.CurrentTask != "" {
		if t, err := s.Store.GetTask(a.CurrentTask); err == nil && !terminalStatus(t.Status) {
			return t.ID
		}
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return ""
	}
	for _, t := range tasks {
		if t.Assignee == a.ID && !terminalStatus(t.Status) {
			return t.ID
		}
	}
	return ""
}

func terminalStatus(st string) bool {
	switch st {
	case store.StatusMerged, store.StatusCancelled, store.StatusDone, store.StatusReverted:
		return true
	}
	return false
}
