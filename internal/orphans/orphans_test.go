package orphans

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/reaper"
	"github.com/oorbt/slai/internal/tmux"
)

var ctx = context.Background()

func testClient(t *testing.T) *tmux.Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	c := tmux.New(fmt.Sprintf("slai-orph-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9))
	t.Cleanup(func() { _ = c.KillServer(ctx) })
	return c
}

// team starts n sessions of a team rooted at project, as `slai up` does.
func team(t *testing.T, c *tmux.Client, project string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := tmux.SessionName(project, fmt.Sprintf("a%d", i))
		cwd := t.TempDir()
		if err := c.NewSession(ctx, name, cwd, map[string]string{"SLAI_PROJECT": project}, "sleep 600"); err != nil {
			t.Fatal(err)
		}
	}
}

func find(teams []Team, project string) (Team, bool) {
	for _, tm := range teams {
		if tm.Project == project {
			return tm, true
		}
	}
	return Team{}, false
}

func finder(c *tmux.Client, current ...string) Finder {
	return Finder{Clients: []*tmux.Client{c}, Current: current,
		Running:   func(string) bool { return false },
		ListProcs: func() (map[string][]reaper.Proc, error) { return nil, nil }}
}

func TestListsForeignTeamsAndFlagsMissingProjectAsOrphan(t *testing.T) {
	c := testClient(t)
	gone := filepath.Join(t.TempDir(), "gone")
	team(t, c, gone, 3)
	teams, err := finder(c, t.TempDir()).Teams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := find(teams, gone)
	if !ok || len(got.Sessions) != 3 || !got.Orphan || got.LastActivity.IsZero() || !strings.HasPrefix(got.Prefix, "slai_") {
		t.Fatalf("team = %+v ok=%v", got, ok)
	}
	if !strings.Contains(got.Reason, "gone") {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestCurrentProjectIsNeverListed(t *testing.T) {
	c := testClient(t)
	cur := t.TempDir()
	team(t, c, cur, 2)
	other := filepath.Join(t.TempDir(), "other")
	team(t, c, other, 1)
	// run from inside an agent's worktree: still the same project
	teams, _ := finder(c, ProjectRoots(filepath.Join(cur, ".slai", "worktrees", "eng-1"))...).Teams(ctx)
	if _, ok := find(teams, cur); ok {
		t.Fatalf("current project listed: %+v", teams)
	}
	if _, ok := find(teams, other); !ok {
		t.Fatalf("foreign team missing: %+v", teams)
	}
	// even with the supervisor down and no activity, the current team is not an orphan
	f := finder(c, cur)
	f.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	teams, _ = f.Teams(ctx)
	if _, ok := find(teams, cur); ok {
		t.Fatal("current project listed as orphan after 48h")
	}
}

func TestOrphanNeedsFreeLockAndInactivity(t *testing.T) {
	c := testClient(t)
	proj := t.TempDir()
	team(t, c, proj, 2)
	f := finder(c, t.TempDir())

	teams, _ := f.Teams(ctx)
	if got, ok := find(teams, proj); !ok || got.Orphan {
		t.Fatalf("freshly active team flagged: %+v", got)
	}
	f.Now = func() time.Time { return time.Now().Add(13 * time.Hour) }
	teams, _ = f.Teams(ctx)
	if got, _ := find(teams, proj); !got.Orphan || !strings.Contains(got.Reason, "inactive") {
		t.Fatalf("inactive team not flagged: %+v", got)
	}
	f.Running = func(string) bool { return true } // its supervisor is alive
	teams, _ = f.Teams(ctx)
	if got, _ := find(teams, proj); got.Orphan {
		t.Fatalf("team with a running supervisor flagged: %+v", got)
	}
	// state.db activity counts: a recent write keeps an idle-looking team alive
	f.Running = func(string) bool { return false }
	_ = os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	db := filepath.Join(proj, ".slai", "state.db")
	_ = os.WriteFile(db, []byte("x"), 0o644)
	f.Now = func() time.Time { return time.Now().Add(13*time.Hour - time.Minute).Add(-0) }
	_ = os.Chtimes(db, time.Now().Add(12*time.Hour), time.Now().Add(12*time.Hour))
	teams, _ = f.Teams(ctx)
	if got, _ := find(teams, proj); got.Orphan {
		t.Fatalf("recent state.db write ignored: %+v", got)
	}
}

func TestProcessesWithoutSessionsFormATeam(t *testing.T) {
	c := testClient(t)
	gone := filepath.Join(t.TempDir(), "minion")
	f := finder(c, t.TempDir())
	f.ListProcs = func() (map[string][]reaper.Proc, error) {
		return map[string][]reaper.Proc{gone: {{PID: 1, Cmd: "slai mcp"}}, f.Current[0]: {{PID: 2, Cmd: "slai mcp"}}}, nil
	}
	teams, _ := f.Teams(ctx)
	got, ok := find(teams, gone)
	if !ok || len(got.Sessions) != 0 || len(got.Procs) != 1 || !got.Orphan {
		t.Fatalf("process-only team = %+v ok=%v", got, ok)
	}
	if _, ok := find(teams, f.Current[0]); ok {
		t.Fatal("current project's processes listed")
	}
}

func TestKillStopsOnlyOrphanSessions(t *testing.T) {
	c := testClient(t)
	cur := t.TempDir()
	live := t.TempDir() // foreign but healthy
	gone := filepath.Join(t.TempDir(), "gone")
	team(t, c, cur, 2)
	team(t, c, live, 2)
	team(t, c, gone, 3)
	f := finder(c, cur)
	teams, _ := f.Teams(ctx)
	for _, tm := range teams {
		if tm.Orphan != (tm.Project == gone) {
			t.Fatalf("unexpected orphan flag: %+v", tm)
		}
	}
	healthy, _ := find(teams, live)
	if _, _, err := f.Kill(ctx, healthy); err == nil {
		t.Fatal("Kill accepted a team that is not an orphan")
	}
	orph, _ := find(teams, gone)
	ns, _, err := f.Kill(ctx, orph)
	if err != nil || ns != 3 {
		t.Fatalf("kill = %d, %v", ns, err)
	}
	left, _ := c.ListSessions(ctx, "slai_")
	if len(left) != 4 {
		t.Fatalf("sessions left = %v, want the current (2) and healthy (2) teams", left)
	}
	for _, n := range left {
		if strings.HasPrefix(n, orph.Prefix) {
			t.Fatalf("orphan session survived: %s", n)
		}
	}
	// a team that is the current project is refused even if forced
	forged := Team{Prefix: tmux.SessionName(cur, ""), Orphan: true}
	if _, _, err := f.Kill(ctx, forged); err == nil {
		t.Fatal("Kill accepted the current project's prefix")
	}
	// a supervisor that came back since listing stops the kill
	f.Running = func(string) bool { return true }
	if _, _, err := f.Kill(ctx, Team{Prefix: "slai_000000_", Project: live, Orphan: true}); err == nil {
		t.Fatal("Kill ignored a running supervisor")
	}
}
