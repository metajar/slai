package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func TestExeWatchDetectsReplacementOnlyAfterItSettles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "slai")
	write := func(body string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now()
	write("v1", t0)
	w := newExeWatch(p)
	now := t0
	if w.Check(now) != "" {
		t.Fatal("unchanged binary reported as replaced")
	}
	// touched, same bytes: not an upgrade
	write("v1", t0.Add(time.Minute))
	now = now.Add(time.Second)
	w.Check(now)
	now = now.Add(ExeSettle)
	if w.Check(now) != "" {
		t.Fatal("touch reported as replaced")
	}
	// new bytes: not trusted until they stop changing
	write("v2-longer", t0.Add(2*time.Minute))
	now = now.Add(time.Second)
	if w.Check(now) != "" {
		t.Fatal("replacement trusted before it settled")
	}
	write("v2-longer!", t0.Add(3*time.Minute)) // still being written
	now = now.Add(ExeSettle - time.Second)
	if w.Check(now) != "" {
		t.Fatal("settle window did not restart when the file changed again")
	}
	now = now.Add(ExeSettle)
	if h := w.Check(now); h == "" || h == w.base {
		t.Fatalf("settled replacement not reported: %q", h)
	}
	// a vanished binary (mid-rename) is not an error and not a verdict
	_ = os.Remove(p)
	if w.Check(now.Add(time.Hour)) == "" {
		t.Log("vanished file keeps the last verdict")
	}
}

func upgradeEnv(t *testing.T) (*Supervisor, *time.Time, string) {
	t.Helper()
	s, now := driverEnv(t)
	p := filepath.Join(t.TempDir(), "slai")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.ExePath = p
	s.ExecArgs = []string{"slai", "supervise"}
	s.Build = "v1"
	return s, now, p
}

func replaceExe(t *testing.T, p string, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, mt, mt)
}

func TestSupervisorReexecsOnceOnANewBinary(t *testing.T) {
	s, now, p := upgradeEnv(t)
	var calls int
	var gotPath string
	var gotArgs []string
	s.Exec = func(path string, args, env []string) error {
		calls++
		gotPath, gotArgs = path, args
		return nil // pretend it replaced the process and carried on
	}
	s.recordBuild()
	for i := 0; i < 3; i++ {
		s.reexecIfUpgraded()
		*now = now.Add(time.Second)
	}
	if calls != 0 {
		t.Fatal("exec before any change")
	}
	replaceExe(t, p, "v2 is different")
	s.reexecIfUpgraded()
	if calls != 0 {
		t.Fatal("exec before the binary settled")
	}
	*now = now.Add(ExeSettle + time.Second)
	for i := 0; i < 4; i++ {
		s.reexecIfUpgraded()
		*now = now.Add(time.Second)
	}
	if calls != 1 || gotPath != p || strings.Join(gotArgs, " ") != "slai supervise" {
		t.Fatalf("exec calls=%d path=%q args=%v", calls, gotPath, gotArgs)
	}
	if _, ok := openEsc(t, s, EscUpgradeFailed, "team"); ok {
		t.Fatal("escalated although exec succeeded")
	}
}

func TestFailedReexecKeepsRunningAndEscalatesOnce(t *testing.T) {
	s, now, p := upgradeEnv(t)
	calls := 0
	s.Exec = func(string, []string, []string) error { calls++; return errors.New("exec format error") }
	s.reexecIfUpgraded()
	replaceExe(t, p, "v2 is different")
	s.reexecIfUpgraded()
	*now = now.Add(ExeSettle + time.Second)
	for i := 0; i < 3; i++ {
		s.reexecIfUpgraded()
	}
	e, ok := openEsc(t, s, EscUpgradeFailed, "team")
	if calls != 1 || !ok || !e.User || !strings.Contains(e.Text, "exec format error") || countEvents(s, EscUpgradeFailed) != 1 {
		t.Fatalf("calls=%d esc=%+v ok=%v", calls, e, ok)
	}
}

// AC 15 for upgrades: the new process starts from the persisted state, so
// nothing already escalated fires again, and it records the new build.
func TestUpgradedSupervisorDoesNotRefireEscalations(t *testing.T) {
	s, now := healthEnv(t)
	s.Build = "v1"
	s.recordBuild()
	ctx := context.Background()
	show(t, s, "eng-1", "5-hour limit reached ∙ resets 11:59pm")
	_ = s.Store.SetAgentStatus("eng-1", "rate_limited")
	s.healthTick(ctx)
	if countEvents(s, EscRateLimited) != 1 {
		t.Fatal("setup: no escalation")
	}
	orchBefore := len(slaiMsgs(t, s, "orch"))

	s2 := restarted(s, now)
	s2.Build = "v2"
	s2.recordBuild()
	s2.healthTick(ctx)
	s2.healthTick(ctx)
	if countEvents(s2, EscRateLimited) != 1 || len(slaiMsgs(t, s, "orch")) != orchBefore {
		t.Fatal("escalation re-fired after the supervisor was upgraded")
	}
	if countEvents(s2, "supervisor_upgraded") != 1 || s2.kvGet(service.KVSupervisorBuild) != "v2" {
		t.Fatalf("upgrade not recorded: events=%d build=%q", countEvents(s2, "supervisor_upgraded"), s2.kvGet(service.KVSupervisorBuild))
	}
	s3 := restarted(s2, now)
	s3.Build = "v2"
	s3.recordBuild()
	if countEvents(s3, "supervisor_upgraded") != 1 {
		t.Fatal("a restart on the same build reported an upgrade")
	}
}

func staleAll(s *Supervisor) {
	for _, a := range s.Team.Agents {
		_ = s.Store.SetKV(service.KVMCPBuildPrefix+a.ID, "v1")
	}
}

func upgraded(s *Supervisor, id string) bool {
	evs, _ := s.Store.EventsByKind(id, "agent_upgraded", 5)
	return len(evs) > 0
}

func TestUpgradeRestartsOnlyAgentsThatAreSafeToRestart(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s *Supervisor, now *time.Time)
		want  bool
	}{
		{"idle", func(t *testing.T, s *Supervisor, now *time.Time) {}, true},
		{"finished", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.Store.SetAgentStatus("eng-1", "finished") }, true},
		{"working", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.Store.SetAgentStatus("eng-1", "working") }, false},
		{"waiting (blocked)", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.Store.SetAgentStatus("eng-1", "waiting") }, false},
		{"waiting on user", func(t *testing.T, s *Supervisor, now *time.Time) {
			_ = s.Store.SetAgentStatus("eng-1", "waiting_on_user")
		}, false},
		{"rate limited", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.Store.SetAgentStatus("eng-1", "rate_limited") }, false},
		{"paused", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.PauseAgent("eng-1") }, false},
		{"stopped on purpose", func(t *testing.T, s *Supervisor, now *time.Time) { _ = s.Store.SetKV(kvStopped+"eng-1", "1") }, false},
		{"focused in the TUI", func(t *testing.T, s *Supervisor, now *time.Time) {
			_ = s.Store.SetKV(store.FocusKey("eng-1"), itoa64(now.Unix()))
		}, false},
		{"unread mail", func(t *testing.T, s *Supervisor, now *time.Time) {
			_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"})
		}, false},
		{"tool in flight", func(t *testing.T, s *Supervisor, now *time.Time) {
			_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
		}, false},
		{"permission prompt showing", func(t *testing.T, s *Supervisor, now *time.Time) {
			show(t, s, "eng-1", "Bash command", "  rm -rf build", "Do you want to proceed?", "❯ 1. Yes", "  2. No")
		}, false},
		{"restart disabled in config", func(t *testing.T, s *Supervisor, now *time.Time) {
			off := false
			s.Team.Defaults.UpgradeRestartAgents = &off
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, now, _ := upgradeEnv(t)
			s.Build = "v2"
			s.recordBuild()
			staleAll(s)
			_ = s.Store.SetAgentStatus("orch", "working") // keep the orchestrator out of the way
			_ = s.Store.SetAgentStatus("eng-2", "working")
			_ = s.Store.SetAgentStatus("eng-1", "idle")
			c.setup(t, s, now)
			s.upgradeTick(context.Background())
			if got := upgraded(s, "eng-1"); got != c.want {
				t.Fatalf("eng-1 restarted = %v, want %v", got, c.want)
			}
			for _, id := range []string{"orch", "eng-2"} {
				if upgraded(s, id) {
					t.Fatalf("%s is working and must not be restarted", id)
				}
			}
		})
	}
}

func TestUpgradeIsRollingOrchLastAndOneAtATime(t *testing.T) {
	s, now, _ := upgradeEnv(t)
	s.Build = "v2"
	s.recordBuild()
	staleAll(s)
	ctx := context.Background()
	for _, id := range []string{"orch", "eng-1", "eng-2"} {
		_ = s.Store.SetAgentStatus(id, "idle")
	}
	_ = s.Store.SetAgentStatus("eng-2", "working") // busy: done when it next goes idle

	s.upgradeTick(ctx)
	if !upgraded(s, "eng-1") || upgraded(s, "orch") || upgraded(s, "eng-2") {
		t.Fatal("first tick should restart exactly eng-1")
	}
	// eng-1 has not come back on the new build: nobody else is restarted meanwhile
	_ = s.Store.SetAgentStatus("eng-2", "idle")
	s.upgradeTick(ctx)
	if upgraded(s, "eng-2") || upgraded(s, "orch") {
		t.Fatal("a second agent was restarted before the first came back")
	}
	_ = s.Store.SetKV(service.KVMCPBuildPrefix+"eng-1", "v2") // eng-1's new MCP server reports in
	s.upgradeTick(ctx)
	if !upgraded(s, "eng-2") || upgraded(s, "orch") {
		t.Fatal("eng-2 should follow eng-1, orch still last")
	}
	// orch waits for everyone: eng-2 not yet back
	s.upgradeTick(ctx)
	if upgraded(s, "orch") {
		t.Fatal("orch restarted before the others finished")
	}
	_ = s.Store.SetKV(service.KVMCPBuildPrefix+"eng-2", "v2")
	_ = s.Store.SetAgentStatus("orch", "working")
	s.upgradeTick(ctx)
	if upgraded(s, "orch") {
		t.Fatal("busy orch restarted")
	}
	_ = s.Store.SetAgentStatus("orch", "idle")
	s.upgradeTick(ctx)
	if !upgraded(s, "orch") {
		t.Fatal("idle orch not restarted last")
	}
	msgs := slaiMsgs(t, s, "orch")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "upgraded") {
		t.Fatalf("orch note = %+v", msgs)
	}
	*now = now.Add(time.Second)
	if len(slaiMsgs(t, s, "eng-1")) != 0 {
		t.Fatal("an engineer owing nothing was woken for no reason")
	}
}

func TestRestartedAgentWakeSaysSlaiWasUpgraded(t *testing.T) {
	s, _, _ := upgradeEnv(t)
	s.Build = "v2"
	s.recordBuild()
	staleAll(s)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("orch", "idle")
	_ = s.Store.SetAgentStatus("eng-1", "working")
	_ = s.Store.SetAgentStatus("eng-2", "working")
	// the orchestrator is the only one left once the others are current
	_ = s.Store.SetKV(service.KVMCPBuildPrefix+"eng-1", "v2")
	_ = s.Store.SetKV(service.KVMCPBuildPrefix+"eng-2", "v2")
	s.upgradeTick(ctx)
	if !upgraded(s, "orch") {
		t.Fatal("orch not restarted")
	}
	s.wakeTick(ctx)
	waitPane(t, s, "orch", "slai was upgraded; you were restarted")
	waitPane(t, s, "orch", "Call get_inbox")
}

func TestStaleBuildAndBoard(t *testing.T) {
	s := unitEnv(t, team(5))
	_ = s.Store.SetKV(service.KVSupervisorBuild, "v2")
	if service.StaleBuild(s.Store, "eng-1", "v2") {
		t.Fatal("a never-started agent is not stale")
	}
	_ = s.Store.SetKV("lastseen.eng-1", "100") // served calls but never recorded a build: old code
	_ = s.Store.SetKV("lastseen.eng-2", "100")
	if service.StaleBuild(s.Store, "eng-1", "v2") {
		t.Fatal("stale before the supervisor looked")
	}
	service.MarkLegacyMCP(s.Store, []string{"eng-1", "orch"})
	if !service.StaleBuild(s.Store, "eng-1", "v2") || service.StaleBuild(s.Store, "orch", "v2") || service.StaleBuild(s.Store, "eng-2", "v2") {
		t.Fatal("only the agent that already served calls is a legacy MCP server")
	}
	_ = s.Store.SetKV(service.KVMCPBuildPrefix+"eng-1", "v2")
	if service.StaleBuild(s.Store, "eng-1", "v2") {
		t.Fatal("current build reads stale")
	}
}
