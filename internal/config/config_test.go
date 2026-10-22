package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPresetsValidate(t *testing.T) {
	for _, n := range append([]string{""}, PresetNames...) {
		tm := Preset(n, "app", "desc", "main")
		if tm == nil {
			t.Fatalf("%q: nil", n)
		}
		if err := tm.Validate(); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	if Preset("bogus", "a", "b", "main") != nil {
		t.Error("unknown preset should be nil")
	}
}

func TestPresetShape(t *testing.T) {
	count := func(tm *Team, role string) (n int) {
		for _, a := range tm.Agents {
			if a.Role == role {
				n++
			}
		}
		return
	}
	solo := Preset("solo", "a", "b", "main")
	if count(solo, RoleEngineer) != 1 || count(solo, RolePM) != 0 || solo.Defaults.RequireReview {
		t.Errorf("solo wrong: %+v", solo)
	}
	sq := Preset("squad", "a", "b", "main")
	if count(sq, RoleEngineer) != 2 || count(sq, RoleFocusGroup) != 3 || count(sq, RolePrincipal) != 1 {
		t.Errorf("squad wrong")
	}
	full := Preset("full", "a", "b", "main")
	if count(full, RoleEngineer) != 4 {
		t.Errorf("full wrong")
	}
}

func mod(f func(*Team)) *Team {
	t := Preset("squad", "app", "d", "main")
	f(t)
	return t
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		t    *Team
		want string
	}{
		{"version", mod(func(t *Team) { t.Version = 2 }), "version must be 1"},
		{"no orch", mod(func(t *Team) { t.Agents = t.Agents[1:] }), "exactly one orchestrator"},
		{"two orch", mod(func(t *Team) { t.Agents = append(t.Agents, Agent{ID: "orch2", Role: RoleOrchestrator, Model: "opus"}) }), "exactly one orchestrator"},
		{"two principal", mod(func(t *Team) { t.Agents = append(t.Agents, Agent{ID: "pr2", Role: RolePrincipal, Model: "fable"}) }), "at most one principal"},
		{"two pm", mod(func(t *Team) { t.Agents = append(t.Agents, Agent{ID: "pm2", Role: RolePM, Model: "sonnet"}) }), "at most one product_manager"},
		{"review needs principal", mod(func(t *Team) {
			var out []Agent
			for _, a := range t.Agents {
				if a.Role != RolePrincipal {
					out = append(out, a)
				}
			}
			t.Agents = out
		}), "require_review"},
		{"bad id upper", mod(func(t *Team) { t.Agents[1].ID = "PM" }), "must match"},
		{"bad id short", mod(func(t *Team) { t.Agents[1].ID = "p" }), "must match"},
		{"bad id long", mod(func(t *Team) { t.Agents[1].ID = "a" + strings.Repeat("b", 24) }), "must match"},
		{"bad id digit start", mod(func(t *Team) { t.Agents[1].ID = "1pm" }), "must match"},
		{"dup id", mod(func(t *Team) { t.Agents[1].ID = "orch" }), "duplicate id"},
		{"bad model", mod(func(t *Team) { t.Agents[0].Model = "gpt" }), "model \"gpt\""},
		{"bad role", mod(func(t *Team) { t.Agents[3].Role = "wizard" }), "unknown role"},
		{"bad perm", mod(func(t *Team) { t.Defaults.PermissionMode = "default" }), "permission_mode"},
		{"turns", mod(func(t *Team) { t.Defaults.MaxTurnsPerTask = -1 }), "max_turns_per_task"},
		{"empty name", mod(func(t *Team) { t.Project.Name = "" }), "project.name"},
	}
	for _, c := range cases {
		err := c.t.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestValidateRawModelOK(t *testing.T) {
	tm := mod(func(t *Team) { t.Agents[0].Model = "claude-opus-9" })
	if err := tm.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := tm.ResolveModel("claude-opus-9"); got != "claude-opus-9" {
		t.Error(got)
	}
}

func TestValidateAllErrorsAtOnce(t *testing.T) {
	tm := mod(func(t *Team) {
		t.Version = 0
		t.Agents[0].Model = "nope"
		t.Agents[1].ID = "BAD"
		t.Defaults.PermissionMode = "x"
	})
	msg := tm.Validate().Error()
	for _, w := range []string{"version", "nope", "BAD", "permission_mode"} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in %s", w, msg)
		}
	}
}

func TestDefaultsAndResolve(t *testing.T) {
	tm, err := Parse([]byte(`
version: 1
project: {name: x, description: d, main_branch: main}
defaults: {require_review: false}
agents:
  - {id: orch, role: orchestrator, model: fable}
`))
	if err != nil {
		t.Fatal(err)
	}
	if tm.Project.IntegrationBranch != "slai/integration" || tm.Defaults.PermissionMode != "manual" ||
		tm.Defaults.MaxTurnsPerTask != 40 || tm.Defaults.MaxConcurrentAgents != 5 {
		t.Errorf("defaults: %+v", tm)
	}
	if tm.ResolveModel("fable") != "claude-fable-5-1" || tm.ResolveModel("opus") != "opus" || tm.ResolveModel("claude-x") != "claude-x" {
		t.Error("resolve")
	}
}

func TestCustomModelAliasKept(t *testing.T) {
	tm := Preset("solo", "a", "b", "main")
	tm.Models["fable"] = "claude-fable-9"
	tm.ApplyDefaults()
	if tm.ResolveModel("fable") != "claude-fable-9" {
		t.Error("override clobbered")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	for _, y := range []string{
		"version: 1\nbogus: 1\n",
		"version: 1\nproject: {name: x, mian_branch: m}\n",
		"version: 1\nagents:\n  - {id: orch, role: orchestrator, model: opus, typo: 1}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("expected error for %q", y)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, n := range PresetNames {
		orig := Preset(n, "app", "multi\nline desc", "main")
		p := filepath.Join(t.TempDir(), "team.yaml")
		if err := Write(p, orig); err != nil {
			t.Fatal(err)
		}
		got, err := Load(p)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if !reflect.DeepEqual(orig, got) {
			t.Errorf("%s: round trip mismatch\n%+v\n%+v", n, orig, got)
		}
	}
}

func TestWriteRejectsInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "team.yaml")
	if err := Write(p, &Team{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("file should not exist")
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load("/nonexistent/team.yaml"); err == nil {
		t.Error("expected error")
	}
}

func TestWorkingRolesDefaultToAuto(t *testing.T) {
	for _, name := range PresetNames {
		team := Preset(name, "p", "", "main")
		for _, a := range team.Agents {
			got := team.PermissionModeFor(a)
			want := team.Defaults.PermissionMode
			if a.Role != RoleFocusGroup {
				want = "auto"
			}
			if got != want {
				t.Errorf("%s/%s: permission mode %q, want %q", name, a.ID, got, want)
			}
		}
	}
	team := Preset("squad", "p", "", "main")
	team.Agents[0].PermissionMode = "bogus"
	if err := team.Validate(); err == nil || !strings.Contains(err.Error(), "permission_mode") {
		t.Errorf("invalid per-agent permission_mode accepted: %v", err)
	}
}

func TestNotifyValidation(t *testing.T) {
	tm := Preset("", "app", "desc", "main")
	if err := tm.Validate(); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	tm.Notify = Notify{Command: "true", Events: []string{"awaiting_user", "bogus"}}
	err := tm.Validate()
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) || !strings.Contains(err.Error(), "run_finished") {
		t.Fatalf("err = %v, want unknown event naming the allowed list", err)
	}
	tm.Notify = Notify{Command: "true", Events: []string{"escalation", "run_finished"}}
	if err := tm.Validate(); err != nil {
		t.Fatalf("valid notify rejected: %v", err)
	}
	out, _ := yaml.Marshal(Preset("", "app", "desc", "main"))
	if strings.Contains(string(out), "notify") {
		t.Fatal("team.yaml without notify must serialize unchanged")
	}
}

func TestTestCommandOptionalAndRoundTrips(t *testing.T) {
	tm := Preset("", "app", "desc", "main")
	out, _ := yaml.Marshal(tm)
	if strings.Contains(string(out), "test_command") || strings.Contains(string(out), "test_timeout") {
		t.Fatal("team.yaml without a test gate must serialize unchanged")
	}
	tm.Project.TestCommand, tm.Project.TestTimeoutSeconds = "go test ./...", 120
	if err := tm.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := yaml.Marshal(tm)
	var back Team
	if err := yaml.Unmarshal(b, &back); err != nil || back.Project.TestCommand != "go test ./..." || back.Project.TestTimeoutSeconds != 120 {
		t.Fatalf("round trip: %+v %v", back.Project, err)
	}
	tm.Project.TestTimeoutSeconds = -1
	if tm.Validate() == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestTaskAndRunCapsValidateAndRoundTrip(t *testing.T) {
	tm := Preset("", "app", "desc", "main")
	if out, _ := yaml.Marshal(tm); strings.Contains(string(out), "per_task_usd") || strings.Contains(string(out), "per_run_usd") {
		t.Fatal("unset caps must not appear in team.yaml")
	}
	tm.Defaults.Caps.PerTaskUSD, tm.Defaults.Caps.PerRunUSD = 4, 40
	if err := tm.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := yaml.Marshal(tm)
	var back Team
	if err := yaml.Unmarshal(b, &back); err != nil || back.Defaults.Caps.PerTaskUSD != 4 || back.Defaults.Caps.PerRunUSD != 40 {
		t.Fatalf("round trip: %+v %v", back.Defaults.Caps, err)
	}
	for _, c := range []SpendCaps{{PerTaskUSD: -1}, {PerRunUSD: -1}} {
		tm.Defaults.Caps = c
		if tm.Validate() == nil {
			t.Fatalf("negative cap accepted: %+v", c)
		}
	}
}

func TestMemoryPackTokensValidateAndRoundTrip(t *testing.T) {
	tm := Preset("", "app", "desc", "main")
	if out, _ := yaml.Marshal(tm); strings.Contains(string(out), "pack_tokens") {
		t.Fatal("an unset memory section must not appear in team.yaml")
	}
	if tm.Memory.PackBudget() != 1500 {
		t.Fatalf("default budget %d", tm.Memory.PackBudget())
	}
	tm.Memory.PackTokens = 800
	if err := tm.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := yaml.Marshal(tm)
	var back Team
	if err := yaml.Unmarshal(b, &back); err != nil || back.Memory.PackBudget() != 800 {
		t.Fatalf("round trip: %+v %v", back.Memory, err)
	}
	tm.Memory.PackTokens = -1
	if tm.Validate() == nil {
		t.Fatal("negative pack_tokens accepted")
	}
}
