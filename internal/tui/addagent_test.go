package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

// agentFormModel is a model whose project dir holds a real squad team.yaml.
func agentFormModel(t *testing.T) (*Model, string) {
	t.Helper()
	m, _ := newModel(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := teamPath(dir)
	if err := config.Write(path, config.Preset("squad", "demo", "", "main")); err != nil {
		t.Fatal(err)
	}
	m.opts.Dir = dir
	return m, path
}

func typeStr(m *Model, s string) {
	for _, r := range s {
		m.Update(key(string(r)))
	}
}

func readTeam(t *testing.T, path string) *config.Team {
	t.Helper()
	tm, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestNOpensNewAgentFormWithSuggestedDefaults(t *testing.T) {
	m, _ := agentFormModel(t)
	m.Update(key("n"))
	if m.mode != mAddAgent || m.form == nil {
		t.Fatalf("n did not open the form: mode=%v", m.mode)
	}
	v := screen(m)
	for _, want := range []string{"New agent", "‹ engineer ›", "eng-3", "sonnet", "ctrl+s"} {
		if !strings.Contains(v, want) {
			t.Errorf("form lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Persona") {
		t.Error("Persona must only show for focus_group")
	}
}

func TestAddAgentEscWritesNothing(t *testing.T) {
	m, path := agentFormModel(t)
	before, _ := os.ReadFile(path)
	m.Update(key("n"))
	typeStr(m, "x")
	m.Update(key("esc"))
	after, _ := os.ReadFile(path)
	if m.mode != mList || m.form != nil || string(before) != string(after) {
		t.Fatalf("esc: mode=%v, file changed=%v", m.mode, string(before) != string(after))
	}
}

func TestAddAgentSaveWritesTeamYaml(t *testing.T) {
	m, path := agentFormModel(t)
	old := readTeam(t, path).Agents
	m.Update(key("n"))
	m.Update(key("tab")) // Harness
	m.Update(key("tab")) // ID
	m.Update(key("tab")) // Focus
	typeStr(m, "infra")
	m.Update(key("ctrl+s"))
	if m.mode != mList {
		t.Fatalf("form still open: %q", m.form.err)
	}
	got := readTeam(t, path).Agents
	if len(got) != len(old)+1 {
		t.Fatalf("%d agents, want %d", len(got), len(old)+1)
	}
	for i := range old {
		if got[i] != old[i] {
			t.Errorf("agent %d changed: %+v -> %+v", i, old[i], got[i])
		}
	}
	want := config.Agent{ID: "eng-3", Role: config.RoleEngineer, Model: "sonnet", Focus: "infra", PermissionMode: "auto"}
	if got[len(got)-1] != want {
		t.Errorf("new agent = %+v, want %+v", got[len(got)-1], want)
	}
	if !strings.Contains(m.status, "eng-3 added to team.yaml") {
		t.Errorf("status = %q", m.status)
	}
}

func TestAddAgentDuplicateIDShowsErrorAndWritesNothing(t *testing.T) {
	m, path := agentFormModel(t)
	before, _ := os.ReadFile(path)
	m.Update(key("n"))
	m.Update(key("tab")) // Harness
	m.Update(key("tab")) // ID field
	for range "eng-3" {
		m.Update(key("backspace"))
	}
	typeStr(m, "eng-1")
	if !strings.Contains(screen(m), `duplicate id "eng-1"`) {
		t.Fatalf("live validation missing:\n%s", screen(m))
	}
	m.Update(key("ctrl+s"))
	after, _ := os.ReadFile(path)
	if m.mode != mAddAgent || string(before) != string(after) {
		t.Fatalf("a duplicate id must keep the form open and write nothing (mode=%v)", m.mode)
	}
}

func TestAddAgentFocusGroupShowsPersonaAndNoAutoMode(t *testing.T) {
	m, path := agentFormModel(t)
	m.Update(key("n"))
	// cycle the role to focus_group (engineer is last-but-one; one step right)
	m.Update(key("right"))
	v := screen(m)
	if !strings.Contains(v, "‹ focus_group ›") || !strings.Contains(v, "Persona") || !strings.Contains(v, "haiku") || !strings.Contains(v, "fg-1") {
		t.Fatalf("focus_group form:\n%s", v)
	}
	m.Update(key("ctrl+s"))
	got := readTeam(t, path).Agents
	last := got[len(got)-1]
	if last.ID != "fg-1" || last.Role != config.RoleFocusGroup || last.PermissionMode != "" {
		t.Fatalf("saved %+v", last)
	}
}

func TestAddAgentSaveKeepsEditsMadeMeanwhile(t *testing.T) {
	m, path := agentFormModel(t)
	m.Update(key("n"))
	// someone adds eng-3 to the file while the form is open
	tm := readTeam(t, path)
	if err := config.AddAgent(tm, config.Agent{ID: "eng-3", Role: config.RoleEngineer, Model: "haiku"}); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(path, tm); err != nil {
		t.Fatal(err)
	}
	m.Update(key("ctrl+s")) // the form still says eng-3: now a duplicate
	if m.mode != mAddAgent || !strings.Contains(m.form.err, "duplicate") {
		t.Fatalf("expected a duplicate error, mode=%v err=%q", m.mode, m.form.err)
	}
	if got := readTeam(t, path).Agents; got[len(got)-1].Model != "haiku" {
		t.Fatal("the other edit was clobbered")
	}
}

// The status must not promise an automatic start unless a reconcile actually ran.
func TestAddAgentStatusDoesNotPromiseAStartWithoutReconcile(t *testing.T) {
	m, _ := agentFormModel(t)
	m.Update(key("n"))
	m.Update(key("ctrl+s"))
	if strings.Contains(m.status, "will start") || !strings.Contains(m.status, "slai init --edit") {
		t.Fatalf("status = %q", m.status)
	}

	// a failing callback is no promise either
	m, _ = agentFormModel(t)
	m.opts.TeamChanged = func(*config.Team) error { return os.ErrClosed }
	m.Update(key("n"))
	m.Update(key("ctrl+s"))
	if strings.Contains(m.status, "will start") {
		t.Fatalf("status = %q", m.status)
	}

	// a callback that reconciled may promise it
	m, _ = agentFormModel(t)
	var got *config.Team
	m.opts.TeamChanged = func(t *config.Team) error { got = t; return nil }
	m.Update(key("n"))
	m.Update(key("ctrl+s"))
	if got == nil || got.Agents[len(got.Agents)-1].ID != "eng-3" || !strings.Contains(m.status, "supervisor will start it") {
		t.Fatalf("status = %q, team = %v", m.status, got)
	}
}

func fakeBinaries(t *testing.T, have ...string) {
	t.Helper()
	old := lookPath
	lookPath = func(name string) (string, error) {
		for _, h := range have {
			if h == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { lookPath = old })
}

// AC 18: the form has a Harness field (default claude); the Model hint and
// validation follow it; a missing binary shows an inline error and blocks Save.
func TestAddAgentHarnessFieldFollowsTheHarness(t *testing.T) {
	fakeBinaries(t, "opencode", "pi")
	m, path := agentFormModel(t)
	m.Update(key("n"))
	v := screen(m)
	if !strings.Contains(v, "‹ claude ›") || !strings.Contains(v, "alias or claude-*") {
		t.Fatalf("default harness is claude with the claude hint:\n%s", v)
	}
	m.Update(key("tab")) // Harness
	m.Update(key("right"))
	v = screen(m)
	if !strings.Contains(v, "‹ opencode ›") || !strings.Contains(v, "provider/model, e.g. zai/glm-5.3") {
		t.Fatalf("opencode hint missing:\n%s", v)
	}
	// the claude default model is gone; an empty model is invalid for opencode
	if !strings.Contains(m.form.err, "provider/model") {
		t.Fatalf("empty model must be rejected live, err=%q", m.form.err)
	}
	m.Update(key("ctrl+s"))
	if m.mode != mAddAgent {
		t.Fatal("an invalid model must not save")
	}
	m.Update(key("tab")) // ID
	m.Update(key("tab")) // Focus
	m.Update(key("tab")) // Model
	typeStr(m, "zhipuai-coding-plan/glm-5.3")
	if m.form.err != "" {
		t.Fatalf("valid provider/model rejected: %q", m.form.err)
	}
	m.Update(key("ctrl+s"))
	if m.mode != mList {
		t.Fatalf("save failed: %q", m.form.err)
	}
	got := readTeam(t, path).Agents
	last := got[len(got)-1]
	if last.Harness != config.HarnessOpenCode || last.Model != "zhipuai-coding-plan/glm-5.3" || last.ID != "eng-3" {
		t.Fatalf("saved %+v", last)
	}
	// the claude agents keep an empty harness (their lines in team.yaml do not change)
	for _, a := range got[:len(got)-1] {
		if a.Harness != "" {
			t.Errorf("%s gained harness %q", a.ID, a.Harness)
		}
	}
}

func TestAddAgentMissingHarnessBinaryBlocksSubmit(t *testing.T) {
	fakeBinaries(t, "opencode") // pi missing
	m, path := agentFormModel(t)
	before, _ := os.ReadFile(path)
	m.Update(key("n"))
	m.Update(key("tab"))
	m.Update(key("right")) // opencode
	m.Update(key("right")) // pi
	if v := screen(m); !strings.Contains(v, "‹ pi ›") || !strings.Contains(v, "pi not found on PATH") {
		t.Fatalf("inline error missing:\n%s", v)
	}
	if strings.Contains(screen(m), "Permission") {
		t.Error("pi has no permission layer: the field must be hidden")
	}
	m.Update(key("tab"))
	m.Update(key("tab"))
	m.Update(key("tab")) // ID, Focus, Model
	typeStr(m, "zai/glm-5.3")
	m.Update(key("ctrl+s"))
	after, _ := os.ReadFile(path)
	if m.mode != mAddAgent || string(before) != string(after) || !strings.Contains(m.form.err, "pi not found") {
		t.Fatalf("a missing binary must block submit: mode=%v err=%q", m.mode, m.form.err)
	}
}
