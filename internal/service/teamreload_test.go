package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
)

// A long-lived MCP server (the orchestrator's) must see agents added to team.yaml
// without a restart, and must survive a bad edit.
func TestServiceReloadsTeamYaml(t *testing.T) {
	s := newSvc(t, true)
	p := filepath.Join(s.Dir, ".slai", "team.yaml")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	base := *s.Team
	base.Models = config.DefaultModels()
	base.Version = 1
	base.Agents = nil
	for _, a := range s.Team.Agents {
		a.Model = "sonnet"
		if a.Role == config.RoleFocusGroup {
			a.Persona = "newbie"
		}
		base.Agents = append(base.Agents, a)
	}
	base.ApplyDefaults()
	if err := config.Write(p, &base); err != nil {
		t.Fatal(err)
	}
	ids := func() map[string]bool {
		out := map[string]bool{}
		for _, a := range j(must(s.GetBoard(ctx, "orch", StatusFilter{})))["agents"].([]any) {
			out[a.(map[string]any)["id"].(string)] = true
		}
		return out
	}
	if ids()["eng-2"] {
		t.Fatal("eng-2 before it was added")
	}
	tm := base
	tm.Agents = append(append([]config.Agent{}, base.Agents...), config.Agent{ID: "eng-2", Role: config.RoleEngineer, Model: "sonnet"})
	if err := config.Write(p, &tm); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, mt, mt)
	if !ids()["eng-2"] {
		t.Fatal("added agent not on the board")
	}
	// an invalid edit keeps the last good team
	_ = os.WriteFile(p, []byte("agents: [broken: \n"), 0o644)
	mt = mt.Add(time.Minute)
	_ = os.Chtimes(p, mt, mt)
	if !ids()["eng-2"] || !ids()["orch"] {
		t.Fatal("an invalid edit dropped the team")
	}
}
