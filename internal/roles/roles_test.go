package roles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func team() *config.Team {
	return &config.Team{
		Project: config.Project{Name: "demo", Description: "A todo app"},
		Agents: []config.Agent{
			{ID: "orch", Role: config.RoleOrchestrator},
			{ID: "pm", Role: config.RolePM},
			{ID: "pe", Role: config.RolePrincipal},
			{ID: "eng1", Role: config.RoleEngineer, Focus: "backend"},
			{ID: "fg1", Role: config.RoleFocusGroup, Persona: "A busy nurse"},
		},
	}
}

func TestRenderAllRoles(t *testing.T) {
	tm := team()
	for _, a := range tm.Agents {
		out, err := Render(tm, a, "")
		if err != nil {
			t.Fatalf("%s: %v", a.Role, err)
		}
		for _, want := range []string{a.ID, "A todo app", "get_inbox"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q", a.Role, want)
			}
		}
		if strings.Contains(out, "- "+a.ID+" (") {
			t.Errorf("%s lists itself as teammate", a.ID)
		}
		if a.ID != "eng1" && !strings.Contains(out, "eng1 (engineer): backend") {
			t.Errorf("%s: teammates missing:\n%s", a.ID, out)
		}
	}
	out, _ := Render(tm, tm.Agents[4], "")
	if !strings.Contains(out, "busy nurse") {
		t.Error("persona missing")
	}
	out, _ = Render(tm, tm.Agents[3], "")
	if !strings.Contains(out, "backend") {
		t.Error("focus missing")
	}
}

func TestRenderOverrideAndUnknown(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".slai", "prompts"), 0o755)
	os.WriteFile(filepath.Join(dir, ".slai", "prompts", "engineer.md"), []byte("Custom for {{.Agent.ID}} on {{.Project.Name}}"), 0o644)
	tm := team()
	out, err := Render(tm, tm.Agents[3], dir)
	if err != nil || out != "Custom for eng1 on demo" {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = Render(tm, tm.Agents[1], dir) // no override for pm
	if !strings.Contains(out, ".slai/specs/") {
		t.Error("pm should use embedded")
	}
	if _, err := Render(tm, config.Agent{ID: "x", Role: "nope"}, ""); err == nil {
		t.Error("want error")
	}
}

func TestAllowedTools(t *testing.T) {
	want := map[string]int{config.RoleOrchestrator: 15, config.RolePM: 13, config.RolePrincipal: 13, config.RoleEngineer: 12, config.RoleFocusGroup: 7}
	for r, n := range want {
		got := AllowedTools(r)
		if len(got) != n {
			t.Errorf("%s: %d tools", r, len(got))
		}
		for _, g := range got {
			if !strings.HasPrefix(g, "mcp__slai__") {
				t.Errorf("bad name %s", g)
			}
		}
	}
	if !slices.Contains(AllowedTools(config.RolePrincipal), "mcp__slai__approve") ||
		slices.Contains(AllowedTools(config.RoleEngineer), "mcp__slai__approve") {
		t.Error("approve matrix wrong")
	}
	if AllowedTools("bogus") != nil {
		t.Error("unknown role should have none")
	}
}

func TestClaudeToolRules(t *testing.T) {
	for _, r := range AllRoles {
		_, deny := ClaudeToolRules(r, "/wt")
		for _, d := range []string{"Bash(git push:*)", "Bash(git reset --hard:*)"} {
			if !slices.Contains(deny, d) {
				t.Errorf("%s missing deny %s", r, d)
			}
		}
	}
	for _, r := range []string{config.RoleOrchestrator, config.RoleFocusGroup} {
		allow, deny := ClaudeToolRules(r, "")
		if !slices.Contains(deny, "Edit") || !slices.Contains(deny, "Write") {
			t.Errorf("%s should deny edits", r)
		}
		for _, a := range allow {
			if strings.HasPrefix(a, "Edit") || strings.HasPrefix(a, "Write") {
				t.Errorf("%s allows %s", r, a)
			}
		}
	}
	allow, deny := ClaudeToolRules(config.RolePM, "")
	if !slices.Contains(allow, "Edit(.slai/specs/**)") || slices.Contains(deny, "Write") {
		t.Errorf("pm: %v %v", allow, deny)
	}
	allow, _ = ClaudeToolRules(config.RolePrincipal, "")
	if !slices.Contains(allow, "Edit(.slai/decisions.md)") {
		t.Errorf("pe: %v", allow)
	}
	allow, deny = ClaudeToolRules(config.RoleEngineer, "/wt/eng1")
	if !slices.Contains(allow, "Edit(//wt/eng1/**)") || slices.Contains(deny, "Edit") {
		t.Errorf("eng: %v %v", allow, deny)
	}
}

func TestRenderPathsBranchesAndParams(t *testing.T) {
	tm := team()
	tm.Project.IntegrationBranch = "slai/integration"
	tm.Project.MainBranch = "trunk"
	render := func(i int) string {
		out, err := Render(tm, tm.Agents[i], "/abs/project")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	eng, prin, fg := render(3), render(2), render(4)
	for name, out := range map[string]string{"eng": eng, "prin": prin, "fg": fg} {
		for _, want := range []string{"/abs/project/.slai/specs/", "/abs/project/.slai/decisions.md", "select:mcp__slai__<name>", "`get_inbox` is how you read"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
	}
	for name, out := range map[string]string{"eng": eng, "prin": prin} {
		if !strings.Contains(out, "`slai/integration`") || !strings.Contains(out, "`trunk`") {
			t.Errorf("%s: branch names missing", name)
		}
	}
	if !strings.Contains(fg, "`message_id`") {
		t.Error("fg: message_id parameter not named")
	}
	// defaults when the team config leaves the branches unset
	tm.Project.IntegrationBranch, tm.Project.MainBranch = "", ""
	if out := render(3); !strings.Contains(out, "`slai/integration`") || !strings.Contains(out, "`main`") {
		t.Error("branch defaults missing")
	}
}

func TestFocusGroupFinishesWithReply(t *testing.T) {
	tm := team()
	fg, _ := Render(tm, tm.Agents[4], "/abs/project")
	if strings.Contains(fg, "report") {
		t.Errorf("focus group prompt mentions report, which it cannot call:\n%s", fg)
	}
	if !strings.Contains(fg, "`reply` is your finish action") {
		t.Error("focus group prompt does not name reply as the finish action")
	}
	eng, _ := Render(tm, tm.Agents[3], "/abs/project")
	if !strings.Contains(eng, "call `report`") {
		t.Error("engineer prompt lost the report instruction")
	}
}

func TestOrchestratorNotesCarryNoObligation(t *testing.T) {
	tm := team()
	out, err := Render(tm, tm.Agents[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "never a plain note. Notes carry no obligation and nothing chases them.") {
		t.Error("orchestrator prompt missing the task-or-ask rule")
	}
}

func TestOrchestratorPromptMentionsDependsOn(t *testing.T) {
	tm := team()
	out, err := Render(tm, tm.Agents[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "`depends_on`") {
		t.Error("orchestrator prompt should tell it to use depends_on")
	}
}

// The spec-to-review handoff: binding decisions and open questions in the spec,
// ACs answered one by one in the review request and in the review.
func TestSpecHandoffPhrases(t *testing.T) {
	tm := team()
	want := map[string][]string{
		"pm": {"**Binding decisions**", "**Open questions**", "each with an owner", "each with an owner and a default",
			"dated addendum", "tell the principal", "Define shared vocabulary", "never restate it"},
		"eng1": {"lists each acceptance criterion", "marked met or not met", "name the test per AC in the claim note"},
		"pe": {"answers each acceptance criterion pass or fail", "record the decision in `.slai/decisions.md` AND send the product manager",
			"dated addendum", "tests assert behaviour, not copied output", "a test must fail without the change"},
	}
	for _, a := range tm.Agents {
		phrases, ok := want[a.ID]
		if !ok {
			continue
		}
		out, err := Render(tm, a, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range phrases {
			if !strings.Contains(out, p) {
				t.Errorf("%s prompt lacks %q", a.ID, p)
			}
		}
	}
}

// Any agent can be asked (service.Ask accepts every team member), so every role
// needs `reply` to answer; a role without it cannot clear an owed-reply reminder.
func TestEveryRoleCanReply(t *testing.T) {
	for _, role := range AllRoles {
		found := false
		for _, n := range ToolNames(role) {
			found = found || n == "reply"
		}
		if !found {
			t.Errorf("role %s has no reply tool but can receive an ask: %v", role, ToolNames(role))
		}
	}
}

// T-0100: focus-group personas read project files and images, and nothing that writes.
func TestFocusGroupReadOnlyFileAccess(t *testing.T) {
	allow, deny := ClaudeToolRules(config.RoleFocusGroup, "")
	want := append(AllowedTools(config.RoleFocusGroup), "Read", "Glob", "Grep")
	if !slices.Equal(allow, want) {
		t.Fatalf("allow = %v, want %v", allow, want)
	}
	for _, a := range allow {
		for _, w := range []string{"Edit", "Write", "NotebookEdit", "MultiEdit", "Bash"} {
			if strings.HasPrefix(a, w) {
				t.Errorf("focus group allows %s", a)
			}
		}
	}
	for _, d := range []string{"Edit", "Write", "NotebookEdit"} {
		if !slices.Contains(deny, d) {
			t.Errorf("focus group no longer denies %s", d)
		}
	}
	for _, r := range AllRoles {
		if r == config.RoleFocusGroup {
			continue
		}
		a, _ := ClaudeToolRules(r, "/wt")
		for _, x := range []string{"Read", "Glob", "Grep"} {
			if slices.Contains(a, x) {
				t.Errorf("%s settings gained %s", r, x)
			}
		}
	}
	out, err := Render(&config.Team{Project: config.Project{Name: "p"}}, config.Agent{ID: "fg", Role: config.RoleFocusGroup}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "may read files and images") || !strings.Contains(out, "never modify anything") {
		t.Errorf("template does not state the read-only rule:\n%s", out)
	}
}
