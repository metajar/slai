package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

func TestLaneHeredocsAndScratch(t *testing.T) {
	scratch := filepath.Join("/tmp", fmt.Sprintf("claude-%d", os.Getuid()), "proj", "sess", "scratchpad")
	t.Setenv("SLAI_TEST_SCRATCH", scratch)
	t.Setenv("SLAI_TEST_HOME_UNSET_PROBE", "")

	type c struct {
		role, cmd string
		deny      bool
	}
	for _, tc := range []c{
		// heredoc variants: input, body is data
		{config.RolePrincipal, "python3 -I - <<'EOF'\nn=2\nprint(n > 1)\nEOF", false},
		{config.RolePrincipal, "python3 - <<EOF\nn=2\nEOF", false},
		{config.RolePrincipal, "python3 - <<\"EOF\"\nx > y.txt\nEOF", false},
		{config.RolePrincipal, "cat <<-EOF\n\tn=2 > README.md\n\tEOF\n", false},
		{config.RolePrincipal, "cat <<'A' <<'B'\nx > f\nA\ny > g\nB", false},
		{config.RolePrincipal, "python3 - <<'EOF' | head\nn=2\nEOF", false},
		{config.RolePrincipal, "python3 - <<'EOF'\nn=2\nEOF\nls", false},
		// here-strings
		{config.RolePrincipal, "python3 -c 'pass' <<< 'n=2 > x'", false},
		{config.RolePrincipal, "cat <<< hello", false},
		{config.RolePrincipal, "grep a < input.txt", false},
		// a real redirect next to a heredoc is still checked
		{config.RolePrincipal, "cat <<EOF > README.md\nhi\nEOF", true},
		{config.RolePrincipal, "cat <<EOF\nhi\nEOF\necho x > README.md", true},
		{config.RolePrincipal, "cat <<EOF >> .slai/decisions.md\nhi\nEOF", false},
		{config.RolePrincipal, "echo x > README.md", true},
		{config.RolePM, "echo x > README.md", true},
		{config.RoleEngineer, "cat <<EOF > /etc/hosts\nhi\nEOF", true},
		{config.RoleEngineer, "cat <<EOF > out.txt\nhi\nEOF", false},

		// Claude scratchpad for every role
		{config.RolePrincipal, "mkdir -p " + scratch, false},
		{config.RoleFocusGroup, "touch " + scratch + "/probe.sh", false},
		{config.RolePM, "echo x > " + scratch + "/a.txt", false},
		{config.RolePrincipal, "mkdir -p /tmp/not-claude", true},
		{config.RolePrincipal, "mkdir -p /tmp/claude-1/x", os.Getuid() != 1},
		{config.RolePrincipal, "echo x > /tmp/x", true},

		// $VAR targets
		{config.RolePrincipal, `S=` + scratch + `; mkdir -p "$S"`, false},
		{config.RolePrincipal, `S=` + scratch + ` && echo x > "$S/p.py"`, false},
		{config.RolePrincipal, `mkdir -p "$SLAI_TEST_SCRATCH"`, false},
		{config.RolePrincipal, `mkdir -p "${SLAI_TEST_SCRATCH}/sub"`, false},
		{config.RolePrincipal, `S=/etc; mkdir -p "$S"`, true},
		{config.RolePrincipal, `mkdir -p "$SLAI_TEST_UNSET"`, true},
		{config.RolePrincipal, `S=$(mktemp -d); mkdir -p "$S"`, true},
		{config.RoleEngineer, `echo x > $SLAI_TEST_UNSET/f`, false},
		{config.RoleEngineer, `S=/etc; echo x > $S/f`, true},
		{config.RoleEngineer, `rm -rf $SLAI_TEST_UNSET/build`, true},
	} {
		l, _ := lane(t, tc.role)
		d := CheckLane(l, "Bash", in(map[string]any{"command": tc.cmd}))
		if (d != nil) != tc.deny {
			t.Errorf("%s %q: deny=%v want %v (%v)", tc.role, tc.cmd, d != nil, tc.deny, d)
		}
	}

	for _, role := range []string{config.RolePM, config.RolePrincipal, config.RoleOrchestrator, config.RoleFocusGroup, config.RoleEngineer} {
		l, _ := lane(t, role)
		if d := CheckLane(l, "Write", in(map[string]any{"file_path": scratch + "/probe.py"})); d != nil {
			t.Errorf("%s Write to scratchpad denied: %v", role, d.Reason)
		}
	}
	l, _ := lane(t, config.RolePrincipal)
	if d := CheckLane(l, "Write", in(map[string]any{"file_path": filepath.Join(l.ProjectDir, "README.md")})); d == nil {
		t.Error("principal Write to README.md allowed")
	}
}

// T-0100: a focus-group persona may read a project PNG but write nothing.
func TestFocusGroupLaneReadsButNeverWrites(t *testing.T) {
	proj := t.TempDir()
	l := LaneFor(config.RoleFocusGroup, "fg", "", proj, proj)
	png := filepath.Join(proj, "docs", "mock.png")
	cases := []struct {
		name, tool, input string
		deny              bool
	}{
		{"read png", "Read", `{"file_path":"` + png + `"}`, false},
		{"glob", "Glob", `{"pattern":"**/*.png"}`, false},
		{"grep", "Grep", `{"pattern":"x"}`, false},
		{"write", "Write", `{"file_path":"` + png + `"}`, true},
		{"edit", "Edit", `{"file_path":"` + png + `"}`, true},
		{"bash redirect", "Bash", `{"command":"echo x > ` + png + `"}`, true},
		{"bash cp", "Bash", `{"command":"cp /etc/hostname ` + png + `"}`, true},
	}
	for _, c := range cases {
		d := CheckLane(l, c.tool, json.RawMessage(c.input))
		if (d != nil && d.Deny) != c.deny {
			t.Errorf("%s: deny = %v, want %v (%+v)", c.name, d != nil, c.deny, d)
		}
	}
}
