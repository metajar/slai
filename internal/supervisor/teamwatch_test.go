package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// writeTeam writes t as team.yaml and makes sure its mtime moves.
func writeTeam(t *testing.T, s *Supervisor, tm *config.Team) {
	t.Helper()
	p := s.teamPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(p, tm); err != nil {
		t.Fatal(err)
	}
	bump(t, p)
}

var mtimeTick time.Duration

func bump(t *testing.T, p string) {
	t.Helper()
	mtimeTick += time.Minute
	mt := time.Now().Add(mtimeTick)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func cloneTeam(t *testing.T, tm *config.Team) *config.Team {
	t.Helper()
	b, err := config.Marshal(tm)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hotEnv(t *testing.T) (*Supervisor, *time.Time) {
	t.Helper()
	s, now := driverEnv(t)
	writeTeam(t, s, s.Team)
	s.teamTick(context.Background()) // baseline
	return s, now
}

func sessionUp(t *testing.T, s *Supervisor, id string) bool {
	t.Helper()
	live, err := s.Tmux.HasSession(context.Background(), s.Session(id))
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func TestAddedAgentIsStartedAndAnnouncedOnce(t *testing.T) {
	s, _ := hotEnv(t)
	ctx := context.Background()
	if hasEvent(s, "agent_added") {
		t.Fatal("the starting team was announced")
	}
	tm := cloneTeam(t, s.Team)
	tm.Agents = append(tm.Agents, config.Agent{ID: "eng-3", Role: config.RoleEngineer, Model: "sonnet", Focus: "billing"})
	writeTeam(t, s, tm)
	before, _ := s.Store.GetAgents()
	for i := 0; i < 3; i++ {
		s.teamTick(ctx)
	}
	if !sessionUp(t, s, "eng-3") {
		t.Fatal("eng-3 was not started")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".slai", "worktrees", "eng-3")); err != nil {
		t.Fatalf("no worktree for the new engineer: %v", err)
	}
	sh(t, filepath.Join(s.Dir, ".slai", "worktrees", "eng-3"), "git", "merge-base", "--is-ancestor", "slai/integration", "HEAD")
	if countEvents(s, "agent_added") != 1 {
		t.Fatalf("agent_added events = %d", countEvents(s, "agent_added"))
	}
	msgs := slaiMsgs(t, s, "orch")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "New teammate eng-3 (engineer, focus: billing) joined. Assign it with create_task.") {
		t.Fatalf("orch messages = %+v", msgs)
	}
	if hasEvent(s, "agent_restarted") || len(before) != 3 {
		t.Fatal("existing agents were touched")
	}
	for _, id := range []string{"orch", "eng-1", "eng-2"} {
		if !sessionUp(t, s, id) {
			t.Fatalf("%s lost its session", id)
		}
	}
	if len(s.Team.Agents) != 4 {
		t.Fatal("team not swapped")
	}
}

func TestInvalidTeamKeepsOldTeamAndEscalatesOnce(t *testing.T) {
	s, _ := hotEnv(t)
	ctx := context.Background()
	p := s.teamPath()
	if err := os.WriteFile(p, []byte("agents: [this is: not valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bump(t, p)
	for i := 0; i < 4; i++ {
		s.teamTick(ctx)
	}
	e, ok := openEsc(t, s, EscTeamInvalid, "team")
	if !ok || !e.User || countEvents(s, EscTeamInvalid) != 1 {
		t.Fatalf("escalation %+v ok=%v events=%d", e, ok, countEvents(s, EscTeamInvalid))
	}
	if len(s.Team.Agents) != 3 || !sessionUp(t, s, "eng-1") {
		t.Fatal("the old team was not kept")
	}
	// an edit that parses but fails validation is equally rejected
	bad := cloneTeam(t, s.Team)
	bad.Agents = append(bad.Agents, bad.Agents[1]) // duplicate id
	b, _ := config.Marshal(bad)
	_ = os.WriteFile(p, b, 0o644)
	bump(t, p)
	s.teamTick(ctx)
	if len(s.Team.Agents) != 3 || countEvents(s, EscTeamInvalid) != 1 {
		t.Fatal("duplicate-id team accepted or re-escalated")
	}
	// fixing the file clears it
	writeTeam(t, s, s.Team)
	s.teamTick(ctx)
	if _, ok := openEsc(t, s, EscTeamInvalid, "team"); ok {
		t.Fatal("escalation not cleared once the file is valid")
	}
}

func TestRemovingABusyAgentDoesNotKillIt(t *testing.T) {
	s, now := hotEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-2", "working")
	tm := cloneTeam(t, s.Team)
	tm.Agents = tm.Agents[:2] // drop eng-2
	writeTeam(t, s, tm)
	for i := 0; i < 3; i++ {
		s.teamTick(ctx)
		*now = now.Add(TeamRetry + time.Second)
	}
	if !sessionUp(t, s, "eng-2") {
		t.Fatal("busy agent was killed")
	}
	e, ok := openEsc(t, s, EscTeamRemovalBlocked, "eng-2")
	if !ok || !strings.Contains(e.Text, "left running") || countEvents(s, EscTeamRemovalBlocked) != 1 {
		t.Fatalf("escalation %+v ok=%v", e, ok)
	}
	// idle but still owing the task: still not stopped
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	*now = now.Add(TeamRetry + time.Second)
	s.teamTick(ctx)
	if !sessionUp(t, s, "eng-2") {
		t.Fatal("agent with an open task was stopped")
	}
	// done with everything: now it goes
	tasks, _ := s.Store.ListTasks("")
	for _, tk := range tasks {
		if err := s.Store.UpdateTaskStatus(tk.ID, store.StatusCancelled); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(TeamRetry + time.Second)
	s.teamTick(ctx)
	if sessionUp(t, s, "eng-2") {
		t.Fatal("idle agent with nothing assigned was not stopped")
	}
	if _, ok := openEsc(t, s, EscTeamRemovalBlocked, "eng-2"); ok {
		t.Fatal("escalation not cleared after the removal")
	}
	if !sessionUp(t, s, "eng-1") || !sessionUp(t, s, "orch") {
		t.Fatal("the rest of the team was touched")
	}
}

func TestChangedAgentConfigRestartsOnlyWhenIdle(t *testing.T) {
	s, now := hotEnv(t)
	ctx := context.Background()
	tm := cloneTeam(t, s.Team)
	tm.Agents[1].Model = "opus"
	_ = s.Store.SetAgentStatus("eng-1", "working")
	writeTeam(t, s, tm)
	s.teamTick(ctx)
	if hasEvent(s, "agent_restarted") {
		t.Fatal("restarted a working agent")
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(TeamRetry + time.Second)
	s.teamTick(ctx)
	if countEvents(s, "agent_restarted") != 1 {
		t.Fatalf("agent_restarted = %d once idle", countEvents(s, "agent_restarted"))
	}
	*now = now.Add(TeamRetry + time.Second)
	s.teamTick(ctx)
	if countEvents(s, "agent_restarted") != 1 {
		t.Fatal("restarted again")
	}
}
