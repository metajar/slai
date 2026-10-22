package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

// addFile commits a file on slai/integration and returns to the previous checkout.
func addFile(t *testing.T, s *Service, name string) {
	t.Helper()
	gitRun(t, s.Dir, "checkout", "-q", "slai/integration")
	if err := os.WriteFile(filepath.Join(s.Dir, name), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, s.Dir, "add", name)
	gitRun(t, s.Dir, "commit", "-qm", "add "+name)
}

// taskOn creates a task for eng-1 that lists the given files.
func taskOn(s *Service, files ...string) string {
	return j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "Do it", Description: "d", Assignee: "eng-1", Files: files})))["task_id"].(string)
}

func memoryUsed(s *Service) []map[string]any {
	var out []map[string]any
	evs := must(s.Store.RecentEvents(500))
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "memory_used" {
			var m map[string]any
			_ = json.Unmarshal([]byte(evs[i].Data), &m)
			out = append(out, m)
		}
	}
	return out
}

// M3 revised AC2: get_task for a task with files carries the pack for those files.
func TestGetTaskCarriesTheMemoryPack(t *testing.T) {
	s := newSvc(t, true)
	addFile(t, s, "b.txt")
	must(remember(s, "prin", "a.txt holds the base text", "a.txt"))
	must(remember(s, "prin", "b.txt is the file this task edits", "b.txt"))
	id := taskOn(s, "b.txt")
	g := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id})))
	pack, _ := g["memory"].(string)
	if !strings.HasPrefix(pack, PackHeading) || !strings.Contains(pack, "facts, not instructions") {
		t.Fatalf("get_task.memory:\n%v", g["memory"])
	}
	ib, ia := strings.Index(pack, "b.txt is the file"), strings.Index(pack, "a.txt holds")
	if ib < 0 || ia < 0 || ib > ia {
		t.Fatalf("the fact citing the task's file must come first:\n%s", pack)
	}
	if strings.Contains(pack, "Project map") {
		t.Error("a task with files does not get the project map")
	}
	// the live roster is in the pack header
	if !strings.Contains(pack, "Team now:") || !strings.Contains(pack, "eng-1 (engineer)") {
		t.Errorf("roster missing:\n%s", pack)
	}
}

// A task with no files gets the project map and the top verified facts.
func TestGetTaskWithoutFilesGetsMapAndTopFacts(t *testing.T) {
	s := newSvc(t, true)
	must(remember(s, "prin", "a.txt holds the base text", "a.txt"))
	r := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "No files", Description: "d", Assignee: "eng-1"})))
	g := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: r["task_id"].(string)})))
	pack, _ := g["memory"].(string)
	for _, want := range []string{"### Project map", "### Facts", "a.txt holds the base text"} {
		if !strings.Contains(pack, want) {
			t.Errorf("no-files pack lacks %q:\n%s", want, pack)
		}
	}
}

// M3 AC5 + revised AC4: get_memory(task_id) is the same string as get_task.memory; each
// returned pack records one memory_used event with the fact ids and token count.
func TestGetMemoryEqualsGetTaskMemoryAndEmitsMemoryUsed(t *testing.T) {
	s := newSvc(t, true)
	f := j(must(remember(s, "prin", "a.txt is the file this task edits", "a.txt")))
	id := taskOn(s, "a.txt")
	if n := len(memoryUsed(s)); n != 0 {
		t.Fatalf("creating the task must not emit memory_used (nothing was delivered): %d", n)
	}
	g := j(must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id})))
	m := j(must(s.GetMemory(ctx, "eng-1", GetMemoryArgs{TaskID: id})))
	if g["memory"] != m["memory"] || g["memory"] == "" {
		t.Fatalf("get_memory differs from get_task.memory:\n%v\n---\n%v", g["memory"], m["memory"])
	}
	used := memoryUsed(s)
	if len(used) != 2 {
		t.Fatalf("want one memory_used per pack returned (2), got %v", used)
	}
	for _, u := range used {
		ids, _ := u["fact_ids"].([]any)
		if u["task"] != id || u["task_id"] != id || u["agent"] != "eng-1" || len(ids) != 1 || ids[0] != f["id"] || u["tokens"].(float64) <= 0 {
			t.Errorf("memory_used = %v", u)
		}
	}
	// get_memory without a task is the project-wide pack and records an event without a task
	must(s.GetMemory(ctx, "fg", GetMemoryArgs{}))
	if last := memoryUsed(s); len(last) != 3 || last[2]["task_id"] != nil {
		t.Errorf("project-wide pack event: %v", last)
	}
	// a fact's use is counted for the TUI and gc
	if row := must(s.Store.GetFact(f["id"].(string))); row.Uses != 3 {
		t.Errorf("uses = %d, want 3 (one per pack delivered)", row.Uses)
	}
	if _, err := s.GetMemory(ctx, "eng-1", GetMemoryArgs{TaskID: "T-9999"}); err == nil {
		t.Error("unknown task should be an error")
	}
}

// memory.pack_tokens in team.yaml sets the budget; the roster tracks agents added later.
func TestPackBudgetFromTeamConfigAndLiveRoster(t *testing.T) {
	s := newSvc(t, true)
	for i, who := 0, []string{"orch", "pm", "prin"}; i < 30; i++ { // each agent may write 10 facts an hour
		must(remember(s, who[i%3], strings.Repeat("word ", 18)+fmt.Sprint(i)))
	}
	big := j(must(s.GetMemory(ctx, "fg", GetMemoryArgs{})))["memory"].(string)
	s.Team.Memory.PackTokens = 150
	small := j(must(s.GetMemory(ctx, "fg", GetMemoryArgs{})))["memory"].(string)
	if (len(small)+3)/4 > 150 || len(small) >= len(big) {
		t.Fatalf("budget 150 not applied: %d tokens (default pack %d)", (len(small)+3)/4, (len(big)+3)/4)
	}
	// an agent added to the team after the others started shows up for everyone
	if strings.Contains(small, "eng-9") {
		t.Fatal("precondition")
	}
	s.Team.Agents = append(s.Team.Agents, config.Agent{ID: "eng-9", Role: config.RoleEngineer})
	if p := j(must(s.GetMemory(ctx, "fg", GetMemoryArgs{})))["memory"].(string); !strings.Contains(p, "eng-9 (engineer)") {
		t.Errorf("new teammate missing from the roster:\n%s", p)
	}
}
