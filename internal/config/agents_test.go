package config

import (
	"os"
	"strings"
	"testing"
)

func TestSuggestID(t *testing.T) {
	squad := Preset("squad", "p", "", "main") // orch pm principal eng-1 eng-2 fg-newbie fg-power fg-skeptic
	gap := Preset("squad", "p", "", "main")
	gap.Agents = append(gap.Agents[:3], gap.Agents[4:]...) // drop eng-1
	solo := Preset("solo", "p", "", "main")
	cases := []struct {
		name string
		team *Team
		role string
		want string
	}{
		{"next engineer", squad, RoleEngineer, "eng-3"},
		{"engineer fills a gap", gap, RoleEngineer, "eng-1"},
		{"first focus group number", squad, RoleFocusGroup, "fg-1"},
		{"singleton role free", solo, RolePM, "pm"},
		{"singleton role taken", squad, RolePM, "pm-2"},
		{"principal taken", squad, RolePrincipal, "principal-2"},
		{"orchestrator taken", squad, RoleOrchestrator, "orch-2"},
		{"unknown role", squad, "weird", "agent"},
	}
	for _, c := range cases {
		if got := SuggestID(c.team, c.role); got != c.want {
			t.Errorf("%s: SuggestID(%s) = %q, want %q", c.name, c.role, got, c.want)
		}
	}
}

func TestAddAgent(t *testing.T) {
	cases := []struct {
		name    string
		agent   Agent
		wantErr string // "" = ok
	}{
		{"valid engineer", Agent{ID: "eng-3", Role: RoleEngineer, Model: "sonnet", Focus: "infra"}, ""},
		{"raw model string", Agent{ID: "eng-3", Role: RoleEngineer, Model: "claude-sonnet-5-5"}, ""},
		{"duplicate id", Agent{ID: "eng-1", Role: RoleEngineer, Model: "sonnet"}, `duplicate id "eng-1"`},
		{"bad id", Agent{ID: "Eng 3", Role: RoleEngineer, Model: "sonnet"}, "must match"},
		{"empty id", Agent{ID: "", Role: RoleEngineer, Model: "sonnet"}, "must match"},
		{"unknown role", Agent{ID: "x-1", Role: "janitor", Model: "sonnet"}, "unknown role"},
		{"unknown model", Agent{ID: "eng-3", Role: RoleEngineer, Model: "gpt"}, "model"},
		{"bad permission mode", Agent{ID: "eng-3", Role: RoleEngineer, Model: "sonnet", PermissionMode: "yolo"}, "permission_mode"},
		{"second orchestrator", Agent{ID: "orch-2", Role: RoleOrchestrator, Model: "opus"}, "exactly one orchestrator"},
		{"second pm", Agent{ID: "pm-2", Role: RolePM, Model: "sonnet"}, "at most one product_manager"},
	}
	for _, c := range cases {
		tm := Preset("squad", "p", "", "main")
		n := len(tm.Agents)
		err := AddAgent(tm, c.agent)
		if c.wantErr == "" {
			if err != nil || len(tm.Agents) != n+1 || tm.Agents[n] != c.agent {
				t.Errorf("%s: err=%v agents=%d", c.name, err, len(tm.Agents))
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err=%v, want it to contain %q", c.name, err, c.wantErr)
		}
		if len(tm.Agents) != n {
			t.Errorf("%s: team changed on error (%d agents)", c.name, len(tm.Agents))
		}
	}
}

// A team that was already invalid must not block (or be blamed on) a good agent.
func TestAddAgentIgnoresPreexistingProblems(t *testing.T) {
	tm := Preset("squad", "p", "", "main")
	tm.Defaults.MaxTurnsPerTask = 0
	if err := AddAgent(tm, Agent{ID: "eng-3", Role: RoleEngineer, Model: "sonnet"}); err != nil {
		t.Fatalf("pre-existing problem blamed on the new agent: %v", err)
	}
}

func TestWriteIsAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/team.yaml"
	tm := Preset("solo", "p", "", "main")
	if err := Write(path, tm); err != nil {
		t.Fatal(err)
	}
	if err := AddAgent(tm, Agent{ID: "eng-2", Role: RoleEngineer, Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, tm); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || len(got.Agents) != 3 {
		t.Fatalf("load: %v %v", err, got)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

func TestHarnessBinaryAndHint(t *testing.T) {
	for kind, want := range map[string]string{"": "claude", HarnessClaude: "claude", HarnessOpenCode: "opencode", HarnessPi: "pi"} {
		if got := HarnessBinary(kind); got != want {
			t.Errorf("HarnessBinary(%q) = %q, want %q", kind, got, want)
		}
	}
	if !strings.Contains(ModelHint(HarnessPi), "provider/model") || strings.Contains(ModelHint(HarnessClaude), "provider/model") {
		t.Error("model hint must follow the harness")
	}
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	found := func(s string) (string, error) { return "/bin/" + s, nil }
	if err := CheckHarnessBinary(HarnessPi, missing); err == nil || !strings.Contains(err.Error(), "pi not found") {
		t.Errorf("missing binary: %v", err)
	}
	if CheckHarnessBinary(HarnessOpenCode, found) != nil {
		t.Error("present binary must pass")
	}
}
