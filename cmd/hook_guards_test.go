package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// The hook prints a PreToolUse deny decision for out-of-lane writes and
// nothing otherwise; it always exits 0.
func TestHookLaneGuardStdout(t *testing.T) {
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	st, _ := store.Open(filepath.Join(proj, ".slai", "state.db"))
	st.UpsertAgent(store.Agent{ID: "pm", Role: config.RolePM})
	st.Close()
	t.Setenv("SLAI_AGENT_ID", "pm")
	t.Setenv("SLAI_PROJECT", proj)
	run := func(stdin string) string {
		c := hookCmd()
		var out bytes.Buffer
		c.SetIn(strings.NewReader(stdin))
		c.SetOut(&out)
		c.SetArgs([]string{"pretooluse"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	payload := func(path string) string {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": proj,
			"tool_input": map[string]any{"file_path": path, "content": "x"}})
		return string(b)
	}
	out := run(payload(filepath.Join(proj, "main.go")))
	var d struct {
		H struct{ HookEventName, PermissionDecision, PermissionDecisionReason string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.H.PermissionDecision != "deny" || d.H.HookEventName != "PreToolUse" || d.H.PermissionDecisionReason == "" {
		t.Fatalf("stdout %q (%v)", out, err)
	}
	if out := run(payload(filepath.Join(proj, ".slai", "specs", "T-1.md"))); out != "" {
		t.Fatalf("in-lane write should print nothing: %q", out)
	}
	if out := run("garbage"); out != "" {
		t.Fatalf("bad payload printed %q", out)
	}
}

func TestApplyEditFlags(t *testing.T) {
	tm := config.Preset("squad", "p", "", "main")
	err := applyEditFlags(tm, initFlags{setModels: []string{"eng-1=haiku", "pm=claude-x"}, permissionMode: "plan"})
	if err != nil || tm.Agents[3].Model != "haiku" || tm.Defaults.PermissionMode != "plan" {
		t.Fatalf("%v %+v", err, tm.Agents[3])
	}
	for _, bad := range []string{"nope", "ghost=opus", "=x"} {
		if applyEditFlags(tm, initFlags{setModels: []string{bad}}) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckModelsWarnsAndContinues(t *testing.T) {
	old := modelProbe
	defer func() { modelProbe = old }()
	var probed []string
	modelProbe = func(_ context.Context, m string) error {
		probed = append(probed, m)
		if m == "claude-fable-5-1" {
			return context.DeadlineExceeded
		}
		return nil
	}
	tm := config.Preset("squad", "p", "", "main")
	var buf bytes.Buffer
	bad := checkModels(context.Background(), tm, &buf)
	if len(bad) != 1 || bad[0] != "claude-fable-5-1" || !strings.Contains(buf.String(), "warning") {
		t.Fatalf("%v %q", bad, buf.String())
	}
	if len(probed) != 4 { // opus sonnet fable haiku, deduped
		t.Fatalf("probed %v", probed)
	}
}

func TestDoctorParsing(t *testing.T) {
	if claudeVersionWarning("2.1.304 (Claude Code)") != "" {
		t.Error("known-good version warned")
	}
	for _, v := range []string{"1.9.0", "3.0.0", "garbage"} {
		if claudeVersionWarning(v) == "" {
			t.Errorf("%s should warn", v)
		}
	}
	help := "  --permission-mode <mode>   Permission mode\n      (choices: \"acceptEdits\", \"auto\",\n   \"manual\", \"plan\")\n  --other"
	ch := permissionChoices(help)
	if len(ch) != 4 || ch[2] != "manual" {
		t.Fatalf("%v", ch)
	}
	miss := missingModes(ch)
	if len(miss) != 2 { // bypassPermissions, dontAsk
		t.Fatalf("%v", miss)
	}
	if len(missingModes(config.PermissionModes)) != 0 {
		t.Error("full list should match")
	}
}

func TestDoctorProjectChecksFix(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	tm := config.Preset("solo", "p", "", "main")
	os.MkdirAll(filepath.Join(dir, ".slai", "run"), 0o755)
	if err := config.Write(filepath.Join(dir, ".slai", "team.yaml"), tm); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLAI_TMUX_SOCKET", "slai-doctor-test-none")
	pid := filepath.Join(dir, ".slai", "run", "supervisor.pid")
	os.WriteFile(pid, []byte("999999\n"), 0o644)
	var buf bytes.Buffer
	projectChecks(context.Background(), dir, false, &buf)
	if !strings.Contains(buf.String(), "stale supervisor pidfile") || !strings.Contains(buf.String(), "worktree for eng-1 missing") {
		t.Fatalf("%s", buf.String())
	}
	if _, err := os.Stat(pid); err != nil {
		t.Fatal("pidfile removed without --fix")
	}
	buf.Reset()
	projectChecks(context.Background(), dir, true, &buf)
	if _, err := os.Stat(pid); err == nil {
		t.Fatal("--fix did not remove stale pidfile")
	}
}
