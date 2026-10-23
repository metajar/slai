package hooks

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/oorbt/slai/internal/config"
)

// The principal may test and write scratch files inside the review checkout, but still may not
// write anywhere else or move a checkout outside it.
func TestPrincipalReviewCheckoutLane(t *testing.T) {
	proj := t.TempDir()
	review := filepath.Join(proj, ".slai", "worktrees", "_review")
	eng := filepath.Join(proj, ".slai", "worktrees", "eng-1")
	type c struct {
		name, cwd, cmd string
		deny           bool
	}
	for _, tc := range []c{
		{"go test in the review checkout", review, "go test ./... && go vet ./... && go build ./...", false},
		{"go test -o redirect inside", review, "go test ./... > " + filepath.Join(review, "out.txt"), false},
		{"read-only git anywhere", proj, "git log --oneline -3 && git diff HEAD~1 && git show HEAD", false},
		{"git checkout in the review checkout", review, "git checkout --detach abc123", false},
		{"git -C review checkout", proj, "git -C " + review + " checkout --detach abc123", false},
		{"cd review then git checkout", proj, "cd " + review + " && git checkout abc123", false},
		{"git checkout in the project checkout", proj, "git checkout main", true},
		{"git checkout in an engineer worktree", eng, "git checkout slai/eng-1/T-0001", true},
		{"git -C engineer worktree", review, "git -C " + eng + " reset --hard HEAD~1", true},
		{"git switch / restore / clean / stash outside", proj, "git restore . ", true},
		{"git clean outside", eng, "git clean -fd", true},
		{"git stash outside", proj, "git stash", true},
		{"redirect outside", proj, "echo x > notes.txt", true},
		{"mkdir in an engineer worktree", eng, "mkdir -p " + filepath.Join(eng, "x"), true},
	} {
		l := Lane{Role: config.RolePrincipal, ProjectDir: proj, Cwd: tc.cwd}
		d := CheckLane(l, "Bash", json.RawMessage(mustJSON(t, map[string]string{"command": tc.cmd})))
		if got := d != nil && d.Deny; got != tc.deny {
			t.Errorf("%s: denied=%v want %v (%v)", tc.name, got, tc.deny, d)
		}
	}
	// the Write tool follows the same lane
	for path, deny := range map[string]bool{
		filepath.Join(review, "fixture.txt"):      false,
		filepath.Join(proj, "main.go"):            true,
		filepath.Join(eng, "main.go"):             true,
		filepath.Join(proj, ".slai/decisions.md"): false,
	} {
		d := CheckLane(Lane{Role: config.RolePrincipal, ProjectDir: proj}, "Write", json.RawMessage(mustJSON(t, map[string]string{"file_path": path})))
		if got := d != nil && d.Deny; got != deny {
			t.Errorf("Write %s: denied=%v want %v", path, got, deny)
		}
	}
	// other roles gain nothing from the review checkout
	for _, role := range []string{config.RoleEngineer, config.RolePM, config.RoleOrchestrator} {
		d := CheckLane(Lane{Role: role, ProjectDir: proj, Worktree: eng}, "Write", json.RawMessage(mustJSON(t, map[string]string{"file_path": filepath.Join(review, "x.go")})))
		if d == nil || !d.Deny {
			t.Errorf("%s may write in the review checkout", role)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
