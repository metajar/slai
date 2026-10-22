package config

import (
	"strings"
	"testing"
)

// Files written before the harness field existed load unchanged, default to claude
// and write back without a harness key.
func TestLegacyTeamFileLoadsAndRoundTripsWithoutHarness(t *testing.T) {
	tm, err := Load("testdata/legacy_team.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range tm.Agents {
		if a.Harness != "" || a.HarnessKind() != HarnessClaude {
			t.Errorf("%s: harness %q kind %q", a.ID, a.Harness, a.HarnessKind())
		}
	}
	b, err := Marshal(tm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "harness:") {
		t.Errorf("a claude agent must not write a harness key:\n%s", b)
	}
	again, err := Parse(b)
	if err != nil || len(again.Agents) != len(tm.Agents) {
		t.Fatalf("round trip: %v", err)
	}
}

func teamWith(a Agent) *Team {
	t := Preset("solo", "p", "d", "main")
	t.Agents = append(t.Agents, a)
	return t
}

func TestHarnessValidation(t *testing.T) {
	cases := []struct {
		name string
		a    Agent
		want string // substring of the error, "" = valid
	}{
		{"default claude alias", Agent{ID: "x1", Role: RoleEngineer, Model: "sonnet"}, ""},
		{"explicit claude", Agent{ID: "x1", Role: RoleEngineer, Model: "claude-opus-4", Harness: "claude"}, ""},
		{"opencode glm", Agent{ID: "x1", Role: RoleEngineer, Model: "zai/glm-5.3", Harness: "opencode"}, ""},
		{"pi glm", Agent{ID: "x1", Role: RoleEngineer, Model: "zai/glm-5.3", Harness: "pi"}, ""},
		{"unknown harness", Agent{ID: "x1", Role: RoleEngineer, Model: "sonnet", Harness: "codex"}, `(x1): unknown harness "codex" (want one of claude, opencode, pi)`},
		{"opencode needs provider/model", Agent{ID: "x1", Role: RoleEngineer, Model: "glm-5.3", Harness: "opencode"}, `must be provider/model for harness opencode`},
		{"pi empty provider", Agent{ID: "x1", Role: RoleEngineer, Model: "/glm", Harness: "pi"}, `must be provider/model`},
		{"pi empty model", Agent{ID: "x1", Role: RoleEngineer, Model: "zai/", Harness: "pi"}, `must be provider/model`},
		{"claude alias rule not applied to opencode", Agent{ID: "x1", Role: RoleEngineer, Model: "sonnet", Harness: "opencode"}, `must be provider/model`},
		{"claude prefix rule still applies to claude", Agent{ID: "x1", Role: RoleEngineer, Model: "zai/glm-5.3"}, `neither a key in models`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := teamWith(c.a).Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestModelForPassesNonClaudeModelsThrough(t *testing.T) {
	tm := Preset("solo", "p", "d", "main")
	if got := tm.ModelFor(Agent{Model: "fable"}); got != "claude-fable-5-1" {
		t.Errorf("claude alias = %q", got)
	}
	if got := tm.ModelFor(Agent{Model: "zai/glm-5.3", Harness: "pi"}); got != "zai/glm-5.3" {
		t.Errorf("pi model = %q", got)
	}
	if got := tm.ModelFor(Agent{Model: "opus", Harness: "opencode"}); got != "opus" {
		t.Errorf("non-claude must not be run through the alias table: %q", got)
	}
}

func TestHarnessRoundTrips(t *testing.T) {
	tm := teamWith(Agent{ID: "glm-1", Role: RoleEngineer, Model: "zai/glm-5.3", Harness: "pi"})
	b, err := Marshal(tm)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "harness: pi") {
		t.Fatalf("harness not written:\n%s", b)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Agents[len(got.Agents)-1]; a.Harness != "pi" || a.Model != "zai/glm-5.3" {
		t.Fatalf("round trip lost it: %+v", a)
	}
}
