package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/service"
)

func (p *project) kv(key string) string {
	st := p.db()
	defer st.Close()
	v, _, _ := st.GetKV(key)
	return v
}

// TestSelfHealUpgrade: installing a new slai binary over the running one heals the
// whole team: the supervisor re-execs (same pid, no re-fired escalations) and the
// idle agents are restarted one at a time, orchestrator last, each coming back with
// an MCP server on the new build.
func TestSelfHealUpgrade(t *testing.T) {
	if skipWhy != "" {
		t.Skip(skipWhy)
	}
	// A private copy of the binaries: replacing the shared ones would upgrade every other test.
	priv := t.TempDir()
	for _, b := range []string{"slai", "claude"} {
		data, err := os.ReadFile(filepath.Join(binDir, b))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(priv, b), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := newProject(t, healOpts(map[string]string{
		"PATH":           priv + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKECLAUDE_MCP": "1",
	}))
	up := exec.Command(filepath.Join(priv, "slai"), "up")
	up.Dir, up.Env = p.dir, p.env
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("slai up: %v\n%s", err, out)
	}
	ids := []string{"orch", "principal", "eng-1", "eng-2"}
	oldBuild := ""
	p.waitFor(60*time.Second, "supervisor and every agent's MCP server on the first build", func() bool {
		oldBuild = p.kv(service.KVSupervisorBuild)
		if oldBuild == "" {
			return false
		}
		for _, id := range ids {
			if p.kv(service.KVMCPBuildPrefix+id) != oldBuild {
				return false
			}
		}
		return true
	})
	pid := p.supervisorPid()
	if n := len(p.events("agent_upgraded", "")); n != 0 {
		t.Fatalf("%d upgrade restarts before any upgrade", n)
	}

	// "Install" a different build: written beside and renamed over, like install(1) does.
	build := exec.Command("go", "build", "-ldflags", "-X github.com/oorbt/slai/cmd.version=upgrade-test", "-o", filepath.Join(priv, "slai.new"), ".")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(priv, "slai.new"), filepath.Join(priv, "slai")); err != nil {
		t.Fatal(err)
	}

	p.waitFor(healTimeout, "supervisor_upgraded", func() bool { return len(p.events("supervisor_upgraded", "")) > 0 })
	newBuild := p.kv(service.KVSupervisorBuild)
	if newBuild == "" || newBuild == oldBuild {
		t.Fatalf("supervisor.build %q after the upgrade (was %q)", newBuild, oldBuild)
	}
	if got := p.supervisorPid(); got != pid {
		t.Errorf("supervisor pid %d -> %d: exec should keep the process", pid, got)
	}
	p.waitFor(healTimeout, "every agent back on the new build", func() bool {
		for _, id := range ids {
			if p.kv(service.KVMCPBuildPrefix+id) != newBuild {
				return false
			}
		}
		return true
	})

	// each agent was restarted exactly once, orchestrator last
	last := int64(0)
	for _, id := range ids[1:] {
		evs := p.events("agent_upgraded", id)
		if len(evs) != 1 {
			t.Errorf("%s: %d agent_upgraded events, want 1", id, len(evs))
			continue
		}
		last = max(last, evs[0].ID)
	}
	if evs := p.events("agent_upgraded", "orch"); len(evs) != 1 || evs[0].ID < last {
		t.Errorf("orch must be restarted last: %+v (others up to event %d)", evs, last)
	}
	if n := len(p.events("supervisor_upgraded", "")); n != 1 {
		t.Errorf("supervisor_upgraded events = %d, want 1", n)
	}
	if len(p.openEscalations()) != 0 {
		t.Errorf("escalations opened by an upgrade: %+v", p.openEscalations())
	}
	p.waitFor(30*time.Second, "orchestrator told it was restarted by the upgrade", func() bool {
		for _, r := range p.fakeRecords("input", "orch") {
			if l, _ := r["line"].(string); strings.Contains(l, "slai was upgraded; you were restarted") {
				return true
			}
		}
		return false
	})

	// the board agrees: current build, nobody stale
	out, code := p.run("orch", "tool", "get_board", "{}")
	if code != 0 {
		t.Fatalf("get_board: exit %d\n%s", code, out)
	}
	var board struct {
		Build  string
		Agents []struct {
			ID         string
			StaleBuild bool `json:"stale_build"`
		}
	}
	if err := json.Unmarshal([]byte(out), &board); err != nil {
		t.Fatalf("board json: %v\n%s", err, out)
	}
	if board.Build != newBuild {
		t.Errorf("board build %q, want %q", board.Build, newBuild)
	}
	for _, a := range board.Agents {
		if a.StaleBuild {
			t.Errorf("%s still reported stale_build", a.ID)
		}
	}
}
