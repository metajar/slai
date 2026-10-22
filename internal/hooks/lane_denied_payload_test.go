package hooks

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// A lane_denied event names who was denied, which tool, and the path (or, for a shell
// command, the command) it aimed at, so the TUI and `slai status` can show it.
func TestLaneDeniedEventCarriesAgentToolAndTarget(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	proj := t.TempDir()
	wt := filepath.Join(proj, ".slai", "worktrees", "eng-1")
	_ = st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer", Worktree: wt, Status: "idle"})
	cases := []struct{ tool, input, target string }{
		{"Write", `{"file_path":"/etc/passwd","content":"x"}`, "/etc/passwd"},
		{"Bash", `{"command":"echo x > /etc/hosts"}`, "echo x > /etc/hosts"},
	}
	for _, c := range cases {
		p := Payload{ToolName: c.tool, ToolInput: json.RawMessage(c.input), Cwd: wt}
		res, err := Handle(st, "eng-1", "pretooluse", p, Options{ProjectDir: proj})
		if err != nil || res.Decision == nil || !res.Decision.Deny {
			t.Fatalf("%s: err=%v decision=%+v", c.tool, err, res.Decision)
		}
		evs, _ := st.EventsByKind("eng-1", "lane_denied", 1)
		var d map[string]any
		if len(evs) != 1 || json.Unmarshal([]byte(evs[0].Data), &d) != nil {
			t.Fatalf("%s: events = %+v", c.tool, evs)
		}
		if d["agent"] != "eng-1" || d["tool"] != c.tool || d["path"] != c.target || d["reason"] == "" {
			t.Fatalf("%s: payload = %v", c.tool, d)
		}
	}
}
