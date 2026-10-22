package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

func neutralProject(t *testing.T, role string) (proj string, status func() string) {
	t.Helper()
	proj = t.TempDir()
	_ = os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	db := filepath.Join(proj, ".slai", "state.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAgent(store.Agent{ID: "g1", Role: role})
	_ = st.UpsertAgent(store.Agent{ID: "other", Role: "engineer"})
	st.Close()
	return proj, func() string {
		st, _ := store.Open(db)
		defer st.Close()
		a, _ := st.AgentByID("g1")
		return a.Status
	}
}

func runHookCmd(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	c := hookCmd()
	var out bytes.Buffer
	c.SetIn(strings.NewReader(stdin))
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	c.SetArgs(args)
	if err := c.Execute(); err != nil {
		t.Fatalf("hook must not fail: %v", err)
	}
	return out.String()
}

// AC 7: the flags win over the ambient env, which here names another agent and project.
func TestHookFlagsIgnoreAmbientIdentity(t *testing.T) {
	proj, status := neutralProject(t, "engineer")
	t.Setenv("SLAI_AGENT_ID", "other")
	t.Setenv("SLAI_PROJECT", t.TempDir()) // a different, empty project
	runHookCmd(t, `{"event":"prompt"}`, "prompt", "--agent", "g1", "--project", proj)
	if s := status(); s != "working" {
		t.Fatalf("g1 = %s: the hook acted on the ambient identity", s)
	}
	st, _ := store.Open(filepath.Join(proj, ".slai", "state.db"))
	defer st.Close()
	if o, _ := st.AgentByID("other"); o.Status == "working" {
		t.Fatal("the ambient agent was updated")
	}
}

func TestNeutralPayloadThroughTheCommand(t *testing.T) {
	proj, status := neutralProject(t, "engineer")
	t.Setenv("SLAI_AGENT_ID", "")
	t.Setenv("SLAI_PROJECT", "")
	args := []string{"--agent", "g1", "--project", proj}
	steps := []struct{ ev, body, want string }{
		{"session_start", `{"session_id":"ses_42"}`, "idle"},
		{"prompt", `{}`, "working"},
		{"tool", `{"tool":"bash"}`, "working"},
		{"tool_end", `{"tool":"bash"}`, "working"},
		{"waiting", `{}`, "waiting"},
		{"prompt", `{}`, "working"},
		{"idle", `{}`, "idle"},
		{"prompt", `{}`, "working"},
		{"stop", `{"event":"stop","usage":{"prompt":500,"output":20,"cost":0.002}}`, "finished"},
	}
	for _, s := range steps {
		out := runHookCmd(t, s.body, append([]string{s.ev}, args...)...)
		want := ""
		if s.ev == "tool" {
			want = `{"decision":"allow"}` // the lane guard always answers explicitly
		}
		if strings.TrimSpace(out) != want {
			t.Fatalf("%s: stdout %q, want %q", s.ev, out, want)
		}
		if got := status(); got != s.want {
			t.Fatalf("after %s: status %s, want %s", s.ev, got, s.want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(proj, ".slai", "run", "g1", "session-id")); string(b) != "ses_42\n" {
		t.Fatalf("session id not stored: %q", b)
	}
	st, _ := store.Open(filepath.Join(proj, ".slai", "state.db"))
	defer st.Close()
	if sp, _ := st.AgentSpendToday("g1"); sp < 0.0019 || sp > 0.0021 {
		t.Fatalf("usage not recorded: %v", sp)
	}
	// the lane guard answers in the neutral format
	out := runHookCmd(t, `{"event":"tool","tool":"Write","tool_input":{"file_path":"/etc/passwd"},"cwd":"/etc"}`, append([]string{"tool"}, args...)...)
	var dec map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &dec); err != nil || dec["decision"] != "deny" || dec["reason"] == "" {
		t.Fatalf("lane denial = %q (%v)", out, err)
	}
	if strings.Contains(out, "hookSpecificOutput") {
		t.Fatal("neutral caller got Claude's format")
	}
}

// Claude's own payloads are unchanged: same arg, hook_event_name, Claude reply format.
func TestClaudePayloadStillClaudeFormat(t *testing.T) {
	proj, status := neutralProject(t, "engineer")
	t.Setenv("SLAI_AGENT_ID", "g1")
	t.Setenv("SLAI_PROJECT", proj)
	runHookCmd(t, `{"hook_event_name":"UserPromptSubmit"}`, "userpromptsubmit")
	if status() != "working" {
		t.Fatal("claude prompt hook broken")
	}
	out := runHookCmd(t, `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/etc/passwd"},"cwd":"/etc"}`, "pretooluse")
	if !strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("claude denial lost its format: %q", out)
	}
}

func TestMCPCommandHasProjectFlag(t *testing.T) {
	c := mcpCmd()
	if c.Flags().Lookup("agent") == nil || c.Flags().Lookup("project") == nil {
		t.Fatal("slai mcp needs --agent and --project")
	}
}
