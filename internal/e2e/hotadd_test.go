package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
)

// TestTeamHotAdd: an engineer appended to team.yaml while the team runs gets a
// session and a worktree, the orchestrator is told once, the rest of the team is
// left alone, and get_board lists the newcomer.
func TestTeamHotAdd(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	p.waitFor(30*time.Second, "supervisor ticking", func() bool {
		out, _ := p.run("", "status", "--json")
		return strings.Contains(out, "orch")
	})
	time.Sleep(2 * time.Second) // let the supervisor baseline the team file
	startsBefore := map[string]int{}
	for _, id := range []string{"orch", "principal", "eng-1", "eng-2"} {
		startsBefore[id] = len(p.fakeStarts(id))
	}

	teamPath := filepath.Join(p.dir, ".slai", "team.yaml")
	team, err := config.Load(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	team.Agents = append(team.Agents, config.Agent{ID: "eng-9", Role: config.RoleEngineer, Model: "sonnet", Focus: "billing"})
	if err := config.Write(teamPath, team); err != nil {
		t.Fatal(err)
	}

	p.waitFor(healTimeout, "eng-9 started", func() bool { return p.agentStatus("eng-9") != "" && len(p.fakeStarts("eng-9")) == 1 })
	if out, err := exec.Command("tmux", "-L", p.sock, "has-session", "-t", "="+p.session("eng-9")).CombinedOutput(); err != nil {
		t.Fatalf("no tmux session for eng-9: %v %s", err, out)
	}
	wt := filepath.Join(p.dir, ".slai", "worktrees", "eng-9")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("no worktree for eng-9: %v", err)
	}
	if out, err := exec.Command("git", "-C", wt, "merge-base", "--is-ancestor", "slai/integration", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("eng-9's worktree is not based on the integration branch: %v %s", err, out)
	}
	p.waitFor(30*time.Second, "agent_added", func() bool { return len(p.events("agent_added", "eng-9")) == 1 })
	p.waitFor(30*time.Second, "orchestrator told", func() bool {
		return len(p.messagesTo("orch", "New teammate eng-9 (engineer, focus: billing) joined")) == 1
	})
	time.Sleep(3 * time.Second)
	if n := len(p.events("agent_added", "eng-9")); n != 1 {
		t.Errorf("agent_added events = %d, want 1", n)
	}
	if n := len(p.messagesTo("orch", "New teammate eng-9")); n != 1 {
		t.Errorf("orchestrator messages = %d, want 1", n)
	}
	for id, n := range startsBefore {
		if got := len(p.fakeStarts(id)); got != n {
			t.Errorf("%s was restarted by the hot add (%d -> %d starts)", id, n, got)
		}
	}

	out, code := p.run("orch", "tool", "get_board", "{}")
	if code != 0 {
		t.Fatalf("get_board exit %d\n%s", code, out)
	}
	var board struct{ Agents []struct{ ID, Role string } }
	if err := json.Unmarshal([]byte(out), &board); err != nil {
		t.Fatalf("board json: %v\n%s", err, out)
	}
	found := false
	for _, a := range board.Agents {
		found = found || (a.ID == "eng-9" && a.Role == config.RoleEngineer)
	}
	if !found {
		t.Errorf("get_board does not list eng-9: %s", out)
	}
}
