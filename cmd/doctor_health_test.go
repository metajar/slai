package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

func healthStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{"orch", "eng-1"} {
		if err := st.UpsertAgent(store.Agent{ID: id, Role: "engineer", Status: "idle", TmuxSession: "s-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

var ageRe = regexp.MustCompile(`last-activity=\S+( ago)?`)

func runHealth(t *testing.T, st *store.Store, dir string, now time.Time) (string, int) {
	t.Helper()
	old := buildID
	buildID = func() string { return "cur" }
	defer func() { buildID = old }()
	agents, _ := st.GetAgents()
	var buf bytes.Buffer
	bad := teamHealth(&buf, st, dir, agents, map[string]bool{"s-orch": true}, now)
	return ageRe.ReplaceAllString(buf.String(), "last-activity=X"), bad
}

func TestTeamHealthSupervisorStates(t *testing.T) {
	st := healthStore(t)
	now := time.Unix(1_000_000, 0)
	_ = st.SetKV("supervisor.heartbeat", "999000")

	// Stopped project (no pidfile) with a stale heartbeat: info only.
	dir := t.TempDir()
	out, bad := runHealth(t, st, dir, now)
	if bad != 0 || !strings.Contains(out, "- supervisor not running (stopped)") {
		t.Fatalf("stopped: bad=%d\n%s", bad, out)
	}

	// Pidfile present, lock free: crashed.
	if err := os.MkdirAll(filepath.Dir(supervisor.PidPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supervisor.PidPath(dir), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, bad = runHealth(t, st, dir, now)
	if bad != 1 || !strings.Contains(out, "✗ supervisor not running (heartbeat 1000s old)") {
		t.Fatalf("crashed: bad=%d\n%s", bad, out)
	}

	// Lock held: stale heartbeat is a hang, fresh is fine.
	_ = os.Remove(supervisor.PidPath(dir))
	lk, err := supervisor.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	if out, bad = runHealth(t, st, dir, now); bad != 1 || !strings.Contains(out, "✗ supervisor not running") {
		t.Fatalf("hung: bad=%d\n%s", bad, out)
	}
	_ = st.SetKV("supervisor.heartbeat", "999999")
	_ = st.SetKV("supervisor.build", "cur")
	if out, bad = runHealth(t, st, dir, now); bad != 0 || !strings.Contains(out, "✓ supervisor running (pid ") {
		t.Fatalf("healthy: bad=%d\n%s", bad, out)
	}
}

var supRunning = regexp.MustCompile(`^  ✓ supervisor running \(pid \d+, build cur, heartbeat 1s old\)$`)

func TestTeamHealthGolden(t *testing.T) {
	st := healthStore(t)
	now := time.Unix(1_000_000, 0)
	dir := t.TempDir()
	lk, err := supervisor.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	_ = st.SetKV("supervisor.heartbeat", "999999")
	_ = st.SetKV("supervisor.build", "cur")
	_ = st.SetKV("esc.rate_limited_all.team", `{"agent":"team","kind":"rate_limited_all","text":"all limited","since":4,"user":true}`)
	_ = st.SetKV("esc.revive_gave_up.eng-1", `{"agent":"eng-1","kind":"revive_gave_up","text":"3 tries","since":1,"user":true}`)
	_ = st.SetKV("esc.stalled.orch", `{"agent":"orch","kind":"stalled","text":"x","since":2}`)
	out, bad := runHealth(t, st, dir, now)
	if bad != 1 {
		t.Fatalf("bad=%d (revive_gave_up must fail)\n%s", bad, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "team health:" || !supRunning.MatchString(lines[1]) || len(lines) != 5 {
		t.Fatalf("golden header:\n%s", out)
	}
	if lines[4] != "  team escalation: rate_limited_all all limited" {
		t.Fatalf("team escalation line: %q", lines[4])
	}
	re := regexp.MustCompile(`^  (\S+)\s+(alive|idle|busy|blocked|stale|dead|rate_limited)\s+reason=\S.* last-activity=X escalation=(\S+)$`)
	want := map[string]string{"eng-1": "revive_gave_up", "orch": "stalled"}
	for _, l := range lines[2:4] {
		m := re.FindStringSubmatch(l)
		if m == nil || want[m[1]] != m[3] {
			t.Fatalf("bad line %q\n%s", l, out)
		}
	}
	_ = st.SetKV("esc.orch_down.orch", `{"agent":"orch","kind":"orch_down","since":3,"user":true}`)
	if _, bad = runHealth(t, st, dir, now); bad != 2 {
		t.Fatalf("orch_down must fail, bad=%d", bad)
	}
}

func TestTeamHealthStoppedBySlaiDownIsInfo(t *testing.T) {
	st := healthStore(t)
	now := time.Unix(1_000_000, 0)
	_ = st.SetKV("supervisor.heartbeat", "999000") // stale
	dir := t.TempDir()
	// the pidfile is gone after a clean exit; the marker says it was on purpose
	supervisor.MarkStopped(dir)
	out, bad := runHealth(t, st, dir, now)
	if bad != 0 || !strings.Contains(out, "  - supervisor: stopped (slai down)\n") || strings.Contains(out, "not running") {
		t.Fatalf("stopped by slai down: bad=%d\n%s", bad, out)
	}
	// even a leftover pidfile (lock free) is not a failure while the marker stands
	_ = os.WriteFile(supervisor.PidPath(dir), []byte("1\n"), 0o644)
	if out, bad = runHealth(t, st, dir, now); bad != 0 || !strings.Contains(out, "stopped (slai down)") {
		t.Fatalf("marker + pidfile: bad=%d\n%s", bad, out)
	}
	// without the marker the same pidfile is a crash
	_ = os.Remove(supervisor.StoppedPath(dir))
	if out, bad = runHealth(t, st, dir, now); bad == 0 || !strings.Contains(out, "✗ supervisor not running") {
		t.Fatalf("no marker: bad=%d\n%s", bad, out)
	}
}

// The supervisor line merges pid, heartbeat and build into one verdict.
func TestTeamHealthSupervisorBuild(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	older := func(pid int, was string) string {
		return fmt.Sprintf("  ⚠ supervisor is running an older slai build (pid %d, build %s, installed cur); it re-execs itself on the next tick, or run `slai down && slai up`\n", pid, was)
	}
	cases := []struct {
		name      string
		hb, build string // KV values ("" = unset)
		want      string // exact supervisor line, "%d" = our pid
		bad       int
	}{
		{"same build", "999999", "cur", "  ✓ supervisor running (pid %d, build cur, heartbeat 1s old)\n", 0},
		{"older build", "999999", "v0", older(0, "v0"), 0},
		{"pre-upgrade: no heartbeat no build", "", "", older(0, "unknown"), 0},
		{"no heartbeat, build reported", "", "cur", older(0, "cur"), 0},
		{"older build and hung", "999000", "v0", "  ✗ supervisor not running (heartbeat 1000s old)\n", 1},
	}
	for _, c := range cases {
		st := healthStore(t)
		dir := t.TempDir()
		lk, err := supervisor.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		if c.hb != "" {
			_ = st.SetKV("supervisor.heartbeat", c.hb)
		}
		if c.build != "" {
			_ = st.SetKV("supervisor.build", c.build)
		}
		out, bad := runHealth(t, st, dir, now)
		lk.Release()
		want := c.want
		if strings.Contains(want, "%d") {
			want = fmt.Sprintf(want, os.Getpid())
		} else {
			want = strings.Replace(want, "(pid 0,", fmt.Sprintf("(pid %d,", os.Getpid()), 1)
		}
		if bad != c.bad || strings.Split(out, "\n")[1]+"\n" != want {
			t.Errorf("%s: bad=%d want %d\n got: %q\nwant: %q", c.name, bad, c.bad, strings.Split(out, "\n")[1], strings.TrimSpace(want))
		}
	}
}

func TestTeamHealthAgentOldBuild(t *testing.T) {
	st := healthStore(t)
	now := time.Unix(1_000_000, 0)
	dir := t.TempDir()
	_ = st.SetKV("mcp.build.eng-1", "v0")
	_ = st.SetKV("mcp.build.orch", "cur")
	out, _ := runHealth(t, st, dir, now)
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "  eng-1") && !strings.HasSuffix(l, " old build"):
			t.Errorf("eng-1 line lacks 'old build': %q", l)
		case strings.HasPrefix(l, "  orch") && strings.Contains(l, "old build"):
			t.Errorf("orch is current: %q", l)
		}
	}
}
