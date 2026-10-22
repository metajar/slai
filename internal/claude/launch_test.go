package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandAndRunFiles(t *testing.T) {
	d := t.TempDir()
	s := Spec{AgentID: "eng-1", Model: "sonnet", PermissionMode: "manual", ProjectDir: d, WorkDir: d,
		SystemPrompt: "you are eng-1", Deny: []string{"Bash(git push:*)"}}
	if err := s.WriteRunFiles(); err != nil {
		t.Fatal(err)
	}
	cmd := s.Command()
	for _, want := range []string{"claude --model 'sonnet'", "--permission-mode 'manual'", "--append-system-prompt", "--settings"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q: %s", want, cmd)
		}
	}
	if !strings.HasSuffix(cmd, filepath.Join("run", "eng-1", "mcp.json")+"'") {
		t.Errorf("--mcp-config should be last: %s", cmd)
	}
	if strings.Contains(cmd, "--continue") || strings.Contains(cmd, "--resume") {
		t.Errorf("never --continue (it resumes whichever conversation in the directory is newest, crossing agents): %s", cmd)
	}
	if strings.Contains(cmd, "--session-id") {
		t.Errorf("an agent without a session id must not pass one: %s", cmd)
	}
	s.SessionID = "11111111-2222-4333-8444-555555555555"
	if fresh := s.Command(); !strings.Contains(fresh, "--session-id '"+s.SessionID+"'") || strings.Contains(fresh, "--resume") || strings.Contains(fresh, " || ") {
		t.Errorf("first launch should start its own session: %s", fresh)
	}
	s.Resume = true
	rev := s.Command()
	parts := strings.Split(rev, " || ")
	if len(parts) != 3 {
		t.Fatalf("revive wants resume, then same-id start, then a fresh id: %s", rev)
	}
	if !strings.Contains(parts[0], "--resume '"+s.SessionID+"'") ||
		!strings.Contains(parts[1], "--session-id '"+s.SessionID+"'") ||
		!strings.Contains(parts[2], "--session-id '") || strings.Contains(parts[2], s.SessionID) {
		t.Errorf("revive fallbacks wrong: %s", rev)
	}
	if strings.Contains(rev, "--continue") {
		t.Errorf("revive must never use --continue: %s", rev)
	}
	b, _ := os.ReadFile(filepath.Join(RunDir(d, "eng-1"), "settings.json"))
	var st struct {
		Hooks      map[string]json.RawMessage
		StatusLine struct{ Type, Command string }
	}
	if err := json.Unmarshal(b, &st); err != nil || len(st.Hooks) != len(HookEvents) {
		t.Fatalf("settings hooks: %v %s", err, b)
	}
	if st.StatusLine.Type != "command" || !strings.HasSuffix(st.StatusLine.Command, " statusline --agent eng-1") {
		t.Fatalf("statusLine: %+v", st.StatusLine)
	}
}

func TestSessionIDsArePerAgentAndStable(t *testing.T) {
	d := t.TempDir()
	a1, existed, err := LoadOrCreateSessionID(d, "orch")
	if err != nil || existed || len(a1) != 36 {
		t.Fatalf("first create: %q existed=%v err=%v", a1, existed, err)
	}
	a2, existed, _ := LoadOrCreateSessionID(d, "orch")
	if a2 != a1 || !existed {
		t.Fatalf("not stable: %q vs %q existed=%v", a2, a1, existed)
	}
	b, _, _ := LoadOrCreateSessionID(d, "pm")
	if b == a1 {
		t.Fatal("two agents share a session id")
	}
	if NewSessionID() == NewSessionID() {
		t.Fatal("session ids must be unique")
	}
	if v := NewSessionID(); v[14] != '4' {
		t.Fatalf("not a v4 uuid: %s", v)
	}
}

// Identity is pinned in every generated file, never left to the inherited process env.
func TestRunFilesPinAgentIdentity(t *testing.T) {
	d := t.TempDir()
	s := Spec{AgentID: "orch", ProjectDir: d, WorkDir: d, SlaiBin: "slai"}
	if err := s.WriteRunFiles(); err != nil {
		t.Fatal(err)
	}
	read := func(name string) map[string]any {
		b, err := os.ReadFile(filepath.Join(RunDir(d, "orch"), name))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	wantEnv := map[string]any{"SLAI_AGENT_ID": "orch", "SLAI_PROJECT": d}
	srv := read("mcp.json")["mcpServers"].(map[string]any)["slai"].(map[string]any)
	if got := fmt.Sprint(srv["args"]); got != "[mcp --agent orch --project "+d+"]" {
		t.Errorf("mcp args = %s", got)
	}
	if !reflect.DeepEqual(srv["env"], wantEnv) {
		t.Errorf("mcp env = %v", srv["env"])
	}
	set := read("settings.json")
	if !reflect.DeepEqual(set["env"], wantEnv) {
		t.Errorf("settings env = %v", set["env"])
	}
	for ev, v := range set["hooks"].(map[string]any) {
		cmd := v.([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
		if !strings.HasSuffix(cmd, " --agent orch") || !strings.Contains(cmd, "hook "+strings.ToLower(ev)) {
			t.Errorf("hook %s command = %q", ev, cmd)
		}
	}
}
