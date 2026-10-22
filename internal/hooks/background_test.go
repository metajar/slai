package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudePane(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "panes", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The footer of Claude Code 2.1.306 counts background shells and monitors while the
// agent works on after its turn ended; only the footer is believed.
func TestBackgroundFooterFromRealCaptures(t *testing.T) {
	r := ClaudeRules()
	cases := []struct{ file, want string }{
		{"bg-shell-footer.txt", "1 shell"},                               // captured
		{"bg-tasks-footer.txt", "1 shell, 1 monitor"},                    // captured
		{"bg-tasks-footer-plural-synthetic.txt", "2 shells, 3 monitors"}, // synthetic: plurals were not captured
		{"bg-summary-says-running-footer-clear.txt", ""},                 // the summary line outlives the task
		{"bg-team-agents-only.txt", ""},                                  // "← 11 agents" is the team
		{"bg-quoted-in-scrollback.txt", ""},                              // the words above the footer are history
		{"bg-zero-monitors.txt", ""},
		{"subagents-working-turn-2.1.307.txt", "2 subagents"}, // captured: main turn mid-work
		{"subagents-idle-turn-2.1.307.txt", "1 subagent"},     // captured: main turn over, subagent still running
		{"idle-prompt.txt", ""},
		{"working-spinner.txt", ""},
		{"permission-dialog.txt", ""},
	}
	for _, c := range cases {
		if got := BackgroundWith(r, claudePane(t, c.file)); got != c.want {
			t.Errorf("%s: %q, want %q", c.file, got, c.want)
		}
	}
	for footer, want := range map[string]string{
		"  ⏵⏵ auto mode on (shift+tab to cycle) · 2 monitors · ← 11 agents": "2 monitors",
		"  ⏸ manual mode on · 3 shells, 1 monitor · ← 11 agents":            "3 shells, 1 monitor",
	} {
		if got := BackgroundWith(r, "❯ \n"+footer); got != want {
			t.Errorf("%q: %q, want %q", footer, got, want)
		}
	}
	if BackgroundWith(r, "") != "" {
		t.Error("empty pane")
	}
}

// A harness without such a footer has no Background rule and never reports one.
func TestHarnessWithoutABackgroundFooterReportsNone(t *testing.T) {
	r := ClaudeRules()
	r.Background = nil
	if got := BackgroundWith(r, claudePane(t, "bg-tasks-footer.txt")); got != "" {
		t.Fatalf("got %q with no Background rule", got)
	}
}

// T-0109: the subagents line counts only in the pane's bottom lines and only with its
// " · /team" suffix; the footer's "← 12 agents" is the team; the shell footer still adds up.
func TestSubagentsLine(t *testing.T) {
	r := ClaudeRules()
	rule := "────────────────────────────────────────\n❯ \n────────────────────────────────────────\n  5h 1% · wk 2% · $3 today\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← 12 agents"
	cases := map[string]struct {
		pane string
		n    int
		bg   string
	}{
		"live":          {"✻ Thinking… (3s)\n● 3 agents working · /team\n\n" + rule, 3, "3 subagents"},
		"singular":      {"● 1 agent working · /team\n\n" + rule, 1, "1 subagent"},
		"team only":     {rule, 0, ""},
		"zero":          {"● 0 agents working · /team\n" + rule, 0, ""},
		"prose":         {"● 2 agents working on it, I think\n" + rule, 0, ""},
		"quoted":        {"  `● 2 agents working · /team`\n" + rule, 0, ""},
		"scrolled away": {"● 2 agents working · /team\n" + strings.Repeat("output line\n", 12) + rule, 0, ""},
		"with shell":    {"● 2 agents working · /team\n──\n❯ \n──\n  ⏵⏵ auto mode on (shift+tab to cycle) · 1 shell · ← 12 agents", 2, "1 shell, 2 subagents"},
	}
	for name, c := range cases {
		if got := SubagentsWith(r, c.pane); got != c.n {
			t.Errorf("%s: SubagentsWith = %d, want %d", name, got, c.n)
		}
		if got := BackgroundWith(r, c.pane); got != c.bg {
			t.Errorf("%s: BackgroundWith = %q, want %q", name, got, c.bg)
		}
	}
	r.Subagents = nil
	if SubagentsWith(r, cases["live"].pane) != 0 {
		t.Error("a harness without the line reports subagents")
	}
	a := "● Read(x)\n● 2 agents working · /team\n" + rule
	b := "● Read(x)\n● 3 agents working · /team\n" + rule
	if StablePane(a) != StablePane(b) {
		t.Error("the subagents count changes the stable pane")
	}
}
