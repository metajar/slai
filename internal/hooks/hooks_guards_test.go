package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func lane(t *testing.T, role string) (Lane, string) {
	t.Helper()
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(proj, ".slai", "worktrees", "eng-1")
	os.MkdirAll(wt, 0o755)
	os.MkdirAll(filepath.Join(proj, ".slai", "specs"), 0o755)
	cwd := proj
	if role == config.RoleEngineer {
		cwd = wt
	}
	return Lane{Role: role, ProjectDir: proj, Worktree: wt, Cwd: cwd}, proj
}

func in(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }

func TestLaneEditGuards(t *testing.T) {
	type c struct {
		role, tool, path string // path relative to project root
		deny             bool
	}
	for _, tc := range []c{
		{config.RoleOrchestrator, "Write", "main.go", true},
		{config.RoleOrchestrator, "Edit", ".slai/specs/T-1.md", true},
		{config.RoleFocusGroup, "MultiEdit", "main.go", true},
		{config.RoleFocusGroup, "NotebookEdit", "a.ipynb", true},
		{config.RolePM, "Write", ".slai/specs/T-0001.md", false},
		{config.RolePM, "Edit", ".slai/specs/sub/x.md", false},
		{config.RolePM, "Write", "main.go", true},
		{config.RolePM, "Write", ".slai/specs/../decisions.md", true},
		{config.RolePM, "Write", ".slai/decisions.md", true},
		{config.RolePrincipal, "Edit", ".slai/decisions.md", false},
		{config.RolePrincipal, "Write", ".slai/specs/T-1.md", true},
		{config.RolePrincipal, "Write", "main.go", true},
		{config.RoleEngineer, "Write", ".slai/worktrees/eng-1/pkg/a.go", false},
		{config.RoleEngineer, "Edit", ".slai/worktrees/eng-1/../eng-2/a.go", true},
		{config.RoleEngineer, "Write", "main.go", true},
	} {
		l, proj := lane(t, tc.role)
		key := "file_path"
		if tc.tool == "NotebookEdit" {
			key = "notebook_path"
		}
		d := CheckLane(l, tc.tool, in(map[string]any{key: filepath.Join(proj, tc.path)}))
		if (d != nil) != tc.deny {
			t.Errorf("%s %s %s: deny=%v want %v", tc.role, tc.tool, tc.path, d != nil, tc.deny)
		}
	}
	// relative path resolves against cwd (engineer in worktree)
	l, _ := lane(t, config.RoleEngineer)
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": "pkg/a.go"})); d != nil {
		t.Errorf("relative in-worktree path denied: %v", d)
	}
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": "../../../x.go"})); d == nil {
		t.Error("relative escape allowed")
	}
	// symlink escape from the worktree into the rest of the project is denied
	os.Symlink(l.ProjectDir, filepath.Join(l.Worktree, "link"))
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": filepath.Join(l.Worktree, "link", "x.go")})); d == nil {
		t.Error("symlink escape allowed")
	}
	// scratch space outside the project is fine for engineers (temp files, fixtures)...
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": "/tmp/slai-scratch-ok.txt"})); d != nil {
		t.Errorf("engineer scratch write in /tmp denied: %v", d)
	}
	// ...but a scratch path that is really inside the project is not
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": filepath.Join(l.ProjectDir, "main.go")})); d == nil {
		t.Error("engineer wrote to the main checkout")
	}
	// non-edit tools untouched
	if d := CheckLane(l, "Read", in(map[string]any{"file_path": "/etc/passwd"})); d != nil {
		t.Error("Read should not be lane-guarded")
	}
}

func TestLaneBashGuards(t *testing.T) {
	type c struct {
		role, cmd string
		deny      bool
	}
	for _, tc := range []c{
		{config.RoleEngineer, "rm -rf build", false},
		{config.RoleEngineer, "rm -rf ./a ./b", false},
		{config.RoleEngineer, "rm -rf /", true},
		{config.RoleEngineer, "rm -rf ..", true},
		{config.RoleEngineer, "rm -rf .", true},
		{config.RoleEngineer, "rm -fr ~/x", true},
		{config.RoleEngineer, "rm -rf $HOME", true},
		{config.RoleEngineer, "go test ./... && rm -rf /tmp/x", true},
		{config.RoleEngineer, "rm -r --force ../../other", true},
		{config.RoleEngineer, "echo hi > out.txt", false},
		{config.RoleEngineer, "echo hi > /etc/hosts", true},
		{config.RoleEngineer, "echo hi >> ../../main.go", true},
		{config.RoleEngineer, "go test ./... 2>&1 | tee /tmp/log", false},
		{config.RoleEngineer, `echo x > "$TMPDIR/out.json"`, false},
		{config.RoleEngineer, "echo x > $HOME/.bashrc", true},
		{config.RoleEngineer, "echo x > ~/.bashrc", true},
		{config.RoleEngineer, "echo x > $S/fake.mjs", false},
		{config.RoleEngineer, "rm -rf $X/build", true}, // rm -r stays strict
		{config.RoleEngineer, "rm -rf /tmp/other", true},
		{config.RolePM, "echo x > /tmp/log", true},
		{config.RoleEngineer, "go build ./...", false},
		{config.RoleOrchestrator, "rm -rf build", true},
		{config.RolePM, "rm -rf .slai/specs", true},
		{config.RolePM, "echo spec > .slai/specs/T-1.md", false},
		{config.RolePM, "echo x > main.go", true},
		{config.RolePM, "cat .slai/specs/T-1.md", false},
		{config.RolePrincipal, "echo '- decision' >> .slai/decisions.md", false},
		{config.RolePrincipal, "sed -i s/a/b/ main.go", true},
		{config.RolePrincipal, "touch x.go", true},
		{config.RoleFocusGroup, "mv a b", true},
		{config.RoleFocusGroup, "ls -la && git status", false},
		{config.RoleFocusGroup, "go test ./... > /dev/null", false},
	} {
		l, _ := lane(t, tc.role)
		d := CheckLane(l, "Bash", in(map[string]any{"command": tc.cmd}))
		if (d != nil) != tc.deny {
			t.Errorf("%s %q: deny=%v want %v", tc.role, tc.cmd, d != nil, tc.deny)
		}
	}
}

func TestDecisionHookJSONSchema(t *testing.T) {
	var m map[string]map[string]string
	if err := json.Unmarshal(Decision{Deny: true, Reason: "nope"}.HookJSON(), &m); err != nil {
		t.Fatal(err)
	}
	h := m["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || h["permissionDecisionReason"] != "nope" {
		t.Fatalf("%v", h)
	}
}

func TestHandleLaneGuardDeniesAndLogs(t *testing.T) {
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	st, _ := store.Open(filepath.Join(proj, "s.db"))
	t.Cleanup(func() { st.Close() })
	st.UpsertAgent(store.Agent{ID: "pm", Role: config.RolePM, Status: "idle"})
	p := Payload{ToolName: "Write", Cwd: proj, ToolInput: in(map[string]any{"file_path": filepath.Join(proj, "main.go")})}
	r, err := Handle(st, "pm", "PreToolUse", p, Options{ProjectDir: proj})
	if err != nil || r.Decision == nil || !r.Decision.Deny || !strings.Contains(r.Decision.Reason, "product_manager") {
		t.Fatalf("%+v %v", r, err)
	}
	if evs, _ := st.EventsByKind("pm", "guard:lane", 5); len(evs) != 1 {
		t.Fatal("no guard:lane event")
	}
	p.ToolInput = in(map[string]any{"file_path": filepath.Join(proj, ".slai", "specs", "T-1.md")})
	if r, _ := Handle(st, "pm", "PreToolUse", p, Options{ProjectDir: proj}); r.Decision != nil {
		t.Fatal("in-lane write denied")
	}
	// guard disabled without ProjectDir; unknown agent fails open
	p.ToolInput = in(map[string]any{"file_path": filepath.Join(proj, "main.go")})
	if r, _ := Handle(st, "pm", "PreToolUse", p, Options{}); r.Decision != nil {
		t.Fatal("guard active without ProjectDir")
	}
	if r, err := Handle(st, "ghost", "PreToolUse", p, Options{ProjectDir: proj}); err == nil || r.Decision != nil {
		t.Fatal("unknown agent must error without a decision")
	}
}

// Guard: turns/current_task reset by SetAgentTask (assignment) so max_turns
// is per task; the guard fires again for the next task.
func TestMaxTurnsResetsPerAssignedTask(t *testing.T) {
	st := newStore(t, "idle", "T-0001")
	fire := func() (n int) {
		for i := 0; i < 5; i++ {
			if r, _ := Handle(st, "e1", "userpromptsubmit", Payload{}, Options{MaxTurnsPerTask: 3}); r.Guard != nil {
				n++
			}
		}
		return
	}
	if fire() != 1 {
		t.Fatal("first task guard")
	}
	if err := st.SetAgentTask("e1", "T-0002"); err != nil {
		t.Fatal(err)
	}
	if ag, _ := st.AgentByID("e1"); ag.Turns != 0 || ag.CurrentTask != "T-0002" {
		t.Fatalf("%+v", ag)
	}
	if fire() != 1 {
		t.Fatal("guard must fire again for the next task")
	}
}
