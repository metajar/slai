package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/orphans"
	"github.com/oorbt/slai/internal/reaper"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tmux"
)

func orphanEnv(t *testing.T) (cur, gone string, c *tmux.Client) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("slai-cmdorph-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	t.Setenv("SLAI_TMUX_SOCKET", sock)
	t.Setenv("SLAI_PROJECT", "")
	c = tmux.New(sock)
	t.Cleanup(func() { _ = c.KillServer(context.Background()) })
	cur = t.TempDir()
	gone = filepath.Join(t.TempDir(), "minion")
	for _, p := range []string{cur, gone} {
		for i := 0; i < 2; i++ {
			if err := c.NewSession(context.Background(), tmux.SessionName(p, fmt.Sprintf("a%d", i)), t.TempDir(),
				map[string]string{"SLAI_PROJECT": p}, "sleep 600"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return cur, gone, c
}

// hermetic is a finder that sees only the test's tmux server and no real processes,
// so a test can never stop a real team.
func hermetic(c *tmux.Client, cur string) orphans.Finder {
	return orphans.Finder{Clients: []*tmux.Client{c}, Current: orphans.ProjectRoots(cur),
		ListProcs: func() (map[string][]reaper.Proc, error) { return nil, nil }}
}

func TestDoctorListsOtherTeamsAndFlagsOrphans(t *testing.T) {
	t.Setenv("SLAI_NO_ORPHAN_SCAN", "")
	cur, gone, _ := orphanEnv(t)
	var buf bytes.Buffer
	bad := doctorOtherTeams(context.Background(), cur, false, &buf)
	out := buf.String()
	if bad < 1 || !strings.Contains(out, "Other slai teams:") || !strings.Contains(out, "ORPHAN "+gone) || !strings.Contains(out, "2 session(s)") {
		t.Fatalf("bad=%d\n%s", bad, out)
	}
	if strings.Contains(out, cur) {
		t.Fatalf("the current project is listed:\n%s", out)
	}
	// run from inside an agent's worktree: still the same current project
	buf.Reset()
	doctorOtherTeams(context.Background(), filepath.Join(cur, ".slai", "worktrees", "eng-1"), false, &buf)
	if strings.Contains(buf.String(), cur) {
		t.Fatalf("worktree cwd listed its own team:\n%s", buf.String())
	}
}

func TestDownOrphansDryRunKillsNothing(t *testing.T) {
	cur, gone, c := orphanEnv(t)
	var buf bytes.Buffer
	if err := stopOrphans(context.Background(), hermetic(c, cur), true, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, gone) || !strings.Contains(out, "dry run: nothing stopped") || !strings.Contains(out, "session "+tmux.SessionName(gone, "a0")) {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if left, _ := c.ListSessions(context.Background(), "slai_"); len(left) != 4 {
		t.Fatalf("dry run stopped sessions: %v", left)
	}
}

func TestDownOrphansStopsOnlyOrphans(t *testing.T) {
	cur, gone, c := orphanEnv(t)
	var buf bytes.Buffer
	if err := stopOrphans(context.Background(), hermetic(c, cur), false, &buf); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	left, _ := c.ListSessions(context.Background(), "slai_")
	if len(left) != 2 {
		t.Fatalf("left = %v, want only the current team", left)
	}
	for _, n := range left {
		if !strings.HasPrefix(n, tmux.SessionName(cur, "")) {
			t.Fatalf("a non-current session survived or current died: %v (gone=%s)", left, gone)
		}
	}
}

func TestDoctorAllShowsHealthyTeamsOnlyWithFlag(t *testing.T) {
	t.Setenv("SLAI_NO_ORPHAN_SCAN", "")
	cur, gone, _ := orphanEnv(t)
	l, err := supervisor.Acquire(gone) // gone now has a live supervisor: healthy, not an orphan
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	var def, all bytes.Buffer
	if bad := doctorOtherTeams(context.Background(), cur, false, &def); bad != 0 {
		t.Fatalf("healthy team counted as a problem:\n%s", def.String())
	}
	if strings.Contains(def.String(), gone) || !strings.Contains(def.String(), "slai doctor --all") {
		t.Fatalf("default output lists a healthy team or omits the --all hint:\n%s", def.String())
	}
	doctorOtherTeams(context.Background(), cur, true, &all)
	if !strings.Contains(all.String(), "Other slai teams:") || !strings.Contains(all.String(), gone) {
		t.Fatalf("--all output:\n%s", all.String())
	}
}
