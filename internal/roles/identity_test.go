package roles

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func squad() *config.Team {
	t := config.Preset("squad", "acme", "demo", "main")
	t.Agents = []config.Agent{
		{ID: "orch", Role: config.RoleOrchestrator, Model: "opus"},
		{ID: "pm", Role: config.RolePM, Model: "sonnet"},
		{ID: "principal", Role: config.RolePrincipal, Model: "opus"},
		{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"},
		{ID: "fg-newbie", Role: config.RoleFocusGroup, Model: "sonnet"},
	}
	return t
}

// .slai/agents.json agrees with team.yaml and the role one-liners.
func TestAgentsFileMatchesTeamAndRoles(t *testing.T) {
	team := squad()
	var doc struct {
		Order  []string            `json:"order"`
		Agents map[string]Identity `json:"agents"`
	}
	if err := json.Unmarshal(AgentsFile(team), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Order) != len(team.Agents) || len(doc.Agents) != len(team.Agents) {
		t.Fatalf("doc = %+v", doc)
	}
	wantReports := map[string]string{"orch": "user", "pm": "orch", "principal": "orch", "eng-1": "principal", "fg-newbie": "pm"}
	for i, a := range team.Agents {
		got := doc.Agents[a.ID]
		if doc.Order[i] != a.ID || got.ID != a.ID || got.Role != a.Role {
			t.Errorf("%s: %+v", a.ID, got)
		}
		if got.Responsibilities == "" || got.Responsibilities != Responsibility(a.Role) {
			t.Errorf("%s responsibilities = %q", a.ID, got.Responsibilities)
		}
		if got.Model != team.ModelFor(a) || got.Harness != string(a.HarnessKind()) {
			t.Errorf("%s model/harness = %s/%s", a.ID, got.Model, got.Harness)
		}
		if got.ReportsTo != wantReports[a.ID] {
			t.Errorf("%s reports_to = %q, want %q", a.ID, got.ReportsTo, wantReports[a.ID])
		}
		if len(got.Teammates) != len(team.Agents)-1 {
			t.Errorf("%s teammates = %v", a.ID, got.Teammates)
		}
		for _, m := range got.Teammates {
			if m.ID == a.ID {
				t.Errorf("%s lists itself as a teammate", a.ID)
			}
		}
	}
}

func TestIdentityLineNamesIDAndRole(t *testing.T) {
	team := squad()
	for _, a := range team.Agents {
		line := IdentityLineFor(team, a.ID)
		if !strings.HasPrefix(line, "You are "+a.ID+", the "+a.Role+" of this team (see `whoami`). ") {
			t.Errorf("%s: %q", a.ID, line)
		}
		if strings.Contains(line, "never write code") != (a.Role == config.RoleOrchestrator) {
			t.Errorf("%s: orchestrator-only sentence wrong in %q", a.ID, line)
		}
	}
	if IdentityLineFor(team, "nobody") != "" {
		t.Error("unknown agent got an identity line")
	}
}

func TestEveryRoleHasWhoamiAndAResponsibility(t *testing.T) {
	for _, r := range AllRoles {
		if Responsibility(r) == "" {
			t.Errorf("%s has no responsibility", r)
		}
		found := false
		for _, n := range ToolNames(r) {
			found = found || n == "whoami"
		}
		if !found {
			t.Errorf("%s cannot call whoami", r)
		}
	}
}
