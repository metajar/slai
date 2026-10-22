package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// Raw tool events, as OpenCode and Pi send them, through the neutral adapter and the
// lane guard: canonical tool name, then allow/deny for in-lane and out-of-lane targets.
func TestOpenCodeAndPiToolVocabularyIsJudgedByTheLaneGuard(t *testing.T) {
	proj := t.TempDir()
	wt := filepath.Join(proj, ".slai", "worktrees", "g1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	st := neutralStore(t)
	_ = st.UpsertAgent(store.Agent{ID: "g1", Role: "engineer", Worktree: wt})
	_ = st.UpsertAgent(store.Agent{ID: "boss", Role: "orchestrator"})
	in, out := filepath.Join(wt, "a.go"), filepath.Join(proj, "main.go")
	patch := func(files ...string) string {
		s := "*** Begin Patch\n"
		for _, f := range files {
			s += "*** Update File: " + f + "\n@@\n-a\n+b\n"
		}
		return s + "*** End Patch"
	}
	js := func(kv ...any) string {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	type tc struct {
		harness, tool string
		input         string
		agent         string
		canonical     string // tool name the guard saw ("" = unchanged)
		deny          bool
		reason        string // substring of the deny reason
	}
	cases := []tc{
		// OpenCode (tool ids and argument names from opencode 1.18)
		{"opencode", "write", js("filePath", in, "content", "x"), "g1", "Write", false, ""},
		{"opencode", "write", js("filePath", out, "content", "x"), "g1", "Write", true, "may not write"},
		{"opencode", "edit", js("filePath", in, "oldString", "a", "newString", "b"), "g1", "Edit", false, ""},
		{"opencode", "edit", js("filePath", out, "oldString", "a", "newString", "b"), "g1", "Edit", true, "may not write"},
		{"opencode", "multiedit", js("filePath", out, "edits", []any{}), "g1", "MultiEdit", true, "may not write"},
		{"opencode", "patch", js("patchText", patch(in)), "g1", "", false, ""},
		{"opencode", "patch", js("patchText", patch(in, out)), "g1", "", true, "may not patch"},
		{"opencode", "apply_patch", js("patchText", patch(out)), "g1", "", true, "may not patch"},
		{"opencode", "apply_patch", js("patchText", "no files here"), "g1", "", true, "did not name any file"},
		{"opencode", "bash", js("command", "go test ./..."), "g1", "Bash", false, ""},
		{"opencode", "bash", js("command", "echo x > "+out), "g1", "Bash", true, "lane guard"},
		{"opencode", "read", js("filePath", "/etc/hostname"), "g1", "", false, ""},
		{"opencode", "grep", js("pattern", "x", "path", "/etc"), "g1", "", false, ""},
		{"opencode", "glob", js("pattern", "*.go", "path", out), "g1", "", false, ""},
		{"opencode", "list", js("path", "/etc"), "g1", "", false, ""},
		{"opencode", "webfetch", js("url", "https://example.com/a/b"), "g1", "", false, ""},
		{"opencode", "todowrite", js("todos", []any{}), "g1", "", false, ""},
		{"opencode", "task", js("description", "d", "prompt", "p", "subagent_type", "general"), "g1", "", false, ""},
		{"opencode", "slai_get_inbox", js(), "g1", "", false, ""},
		// Pi (built-ins: read, bash, edit, write, grep, find, ls)
		{"pi", "write", js("path", in, "content", "x"), "g1", "Write", false, ""},
		{"pi", "write", js("path", "/etc/cron.d/x", "content", "x"), "g1", "Write", true, "may not write"},
		{"pi", "edit", js("path", out, "oldText", "a", "newText", "b"), "g1", "Edit", true, "may not write"},
		{"pi", "edit", js("path", in, "oldText", "a", "newText", "b"), "g1", "Edit", false, ""},
		{"pi", "bash", js("command", "rm -rf "+out), "g1", "Bash", true, "lane guard"},
		{"pi", "read", js("path", "/etc/hostname"), "g1", "", false, ""},
		{"pi", "grep", js("pattern", "x", "path", "/etc"), "g1", "", false, ""},
		{"pi", "find", js("pattern", "*.go", "path", "/etc"), "g1", "", false, ""},
		{"pi", "ls", js("path", "/etc"), "g1", "", false, ""},
		// fail closed: a tool the guard has never heard of
		{"opencode", "frobnicate", js("path", out), "g1", "", true, "unknown tool frobnicate"},
		{"pi", "deploy", js("target", "/etc/x"), "g1", "", true, "unknown tool deploy"},
		{"pi", "deploy", js("destination", []any{in, out}), "g1", "", true, "unknown tool deploy"},
		{"opencode", "frobnicate", js("path", in), "g1", "", false, ""},
		{"opencode", "frobnicate", js("query", "who"), "g1", "", false, ""},
		{"opencode", "frobnicate", js("file_type", "go", "dir_name", "src"), "g1", "", false, ""}, // not path-looking values
		// a read-only role has no lane to write in at all
		{"opencode", "write", js("filePath", in), "boss", "Write", true, "read-only"},
		{"opencode", "frobnicate", js("path", in), "boss", "", true, "unknown tool"},
		{"pi", "read", js("path", in), "boss", "", false, ""},
	}
	for _, c := range cases {
		name := c.harness + "/" + c.tool + "/" + c.agent + "/" + c.input
		if len(name) > 90 {
			name = name[:90]
		}
		t.Run(name, func(t *testing.T) {
			ev, p, neutral, err := Adapt("tool", Payload{Event: "tool", Tool: c.tool, ToolInput: json.RawMessage(c.input), Cwd: wt})
			if err != nil || !neutral || ev != "pretooluse" {
				t.Fatalf("Adapt: %v %v %s", err, neutral, ev)
			}
			if c.canonical != "" && p.ToolName != c.canonical {
				t.Fatalf("canonical tool = %q, want %q", p.ToolName, c.canonical)
			}
			res, err := Handle(st, c.agent, ev, p, Options{ProjectDir: proj})
			if err != nil {
				t.Fatal(err)
			}
			denied := res.Decision != nil && res.Decision.Deny
			if denied != c.deny {
				t.Fatalf("deny = %v, want %v (%+v)", denied, c.deny, res.Decision)
			}
			if c.deny && !strings.Contains(res.Decision.Reason, c.reason) {
				t.Fatalf("reason %q lacks %q", res.Decision.Reason, c.reason)
			}
		})
	}
}
