package supervisor

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fullTeam() *config.Team {
	t := config.Preset("squad", "p", "demo", "main")
	t.ApplyDefaults()
	return t
}

// Lane enforcement: golden launch config (settings.json + command) per role.
func TestGuardGoldenLaunchConfigPerRole(t *testing.T) {
	s := unitEnv(t, fullTeam())
	s.SlaiBin = "slai"
	seen := map[string]bool{}
	for _, a := range s.Team.Agents {
		if seen[a.Role] {
			continue
		}
		seen[a.Role] = true
		spec, err := s.SpecFor(a)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.HarnessOf(a).WriteRunFiles(spec); err != nil {
			t.Fatal(err)
		}
		settings, _ := os.ReadFile(filepath.Join(s.Dir, ".slai", "run", a.ID, "settings.json"))
		got := map[string]string{
			a.Role + ".settings.json": string(settings),
			a.Role + ".command.txt":   s.HarnessOf(a).Command(spec) + "\n",
		}
		for name, body := range got {
			body = strings.ReplaceAll(body, s.Dir, "<PROJ>")
			body = sessionIDRe.ReplaceAllString(body, "<SESSION>")
			path := filepath.Join("testdata", "golden", name)
			if *update {
				os.MkdirAll(filepath.Dir(path), 0o755)
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if string(want) != body {
				t.Errorf("%s differs from golden (run with -update to accept):\n%s", name, body)
			}
		}
	}
	if len(seen) != 5 {
		t.Fatalf("roles covered: %v", seen)
	}
}

// PRD 8.1: no default or preset ever produces a bypass flag.
func TestGuardNoBypassPermissionsByDefault(t *testing.T) {
	if config.DefaultPermissionMode == "bypassPermissions" {
		t.Fatal("default permission mode bypasses prompts")
	}
	for _, name := range config.PresetNames {
		tm := config.Preset(name, "p", "d", "main")
		tm.ApplyDefaults()
		if tm.Defaults.PermissionMode == "bypassPermissions" {
			t.Errorf("preset %s defaults to bypassPermissions", name)
		}
		s := unitEnv(t, tm)
		for _, a := range tm.Agents {
			spec, err := s.SpecFor(a)
			if err != nil {
				t.Fatal(err)
			}
			cmd := s.HarnessOf(a).Command(spec)
			for _, bad := range []string{"bypassPermissions", "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions"} {
				if strings.Contains(cmd, bad) {
					t.Errorf("%s/%s command contains %s: %s", name, a.ID, bad, cmd)
				}
			}
		}
	}
}

// Destructive commands: every role denies push / reset --hard; non-engineers deny rm -rf.
func TestGuardDestructiveDeny(t *testing.T) {
	s := unitEnv(t, fullTeam())
	for _, a := range s.Team.Agents {
		spec, _ := s.SpecFor(a)
		has := func(r string) bool {
			for _, d := range spec.Deny {
				if d == r {
					return true
				}
			}
			return false
		}
		if !has("Bash(git push:*)") || !has("Bash(git reset --hard:*)") {
			t.Errorf("%s missing push/reset deny: %v", a.ID, spec.Deny)
		}
		if a.Role != config.RoleEngineer && !has("Bash(rm -rf:*)") {
			t.Errorf("%s missing rm -rf deny", a.ID)
		}
	}
}

// max_turns_per_task: assignment wake-up resets turns/current_task (SetAgentTask).
func TestGuardAssignmentResetsTurns(t *testing.T) {
	s := unitEnv(t, team(5))
	s.Store.UpsertAgent(store.Agent{ID: "orch", Role: config.RoleOrchestrator})
	for i := 0; i < 7; i++ {
		s.Store.BumpTurns("orch")
	}
	msgs := []store.Message{{ID: 1, Kind: "assignment", TaskID: "T-0009", To: "orch"}}
	if !s.prepareAssignments(context.Background(), store.Agent{ID: "orch"}, msgs) {
		t.Fatal("prepare failed")
	}
	a, _ := s.Store.AgentByID("orch")
	if a.Turns != 0 || a.CurrentTask != "T-0009" {
		t.Fatalf("%+v", a)
	}
}

// Branch collisions: merges are serialized by a mutex in the supervisor.
func TestGuardMergeMutexHeld(t *testing.T) {
	s := unitEnv(t, team(5))
	s.rt.mergeMu.Lock()
	done := make(chan struct{})
	go func() { s.mergeTick(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("mergeTick ran while the merge mutex was held")
	case <-time.After(200 * time.Millisecond):
	}
	s.rt.mergeMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mergeTick never ran")
	}
}

func TestGuardConcurrentMergeTicksMergeOnce(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	var tasks []store.Task
	for i, a := range []string{"eng-1", "eng-2"} {
		tk, _ := s.Store.CreateTask(store.Task{Title: "t", Assignee: a, Status: store.StatusInReview})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(a, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		commitOnTask(t, s, tk, []string{"a.txt", "b.txt"}[i], "x\n")
		requestMerge(s, tk.ID)
		tasks = append(tasks, tk)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.mergeTick(ctx) }()
	}
	wg.Wait()
	for _, tk := range tasks {
		if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
			t.Errorf("%s: %s", tk.ID, g.Status)
		}
	}
	if hasEvent(s, "merge_error") || hasEvent(s, "merge_conflict") {
		t.Fatal("unexpected merge failure")
	}
	log := sh(t, s.Dir, "git", "log", "--merges", "--oneline", "slai/integration")
	if n := len(strings.Split(strings.TrimSpace(log), "\n")); n != 2 {
		t.Fatalf("want 2 merge commits, got %d:\n%s", n, log)
	}
}

// max_concurrent_agents: idle agents beyond the cap are not woken.
func TestGuardMaxConcurrentWakes(t *testing.T) {
	s := tmuxEnv(t, team(1))
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "a"})
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "b"})
	s.Tick(context.Background())
	woken := 0
	for _, id := range []string{"eng-1", "eng-2"} {
		if status(t, s, id) == "working" {
			woken++
		}
	}
	if woken != 1 {
		t.Fatalf("woken=%d with cap 1", woken)
	}
}

// ---- init --edit reconcile ----

func TestPlanReconcile(t *testing.T) {
	order := []string{"a", "b", "c", "d"}
	desired := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
	stored := map[string]string{"a": "1", "b": "old", "c": ""}
	live := map[string]bool{"a": true, "b": true, "c": true, "x": true}
	p := PlanReconcile(order, desired, stored, live, []string{"a", "b", "c", "x"})
	want := Plan{Start: []string{"d"}, Restart: []string{"b"}, Stop: []string{"x"}, Adopt: []string{"c"}}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("%+v want %+v", p, want)
	}
	if !PlanReconcile([]string{"a"}, map[string]string{"a": "1"}, map[string]string{"a": "1"}, map[string]bool{"a": true}, []string{"a"}).Empty() {
		t.Fatal("unchanged team should be an empty plan")
	}
}

func TestConfigHashSensitivity(t *testing.T) {
	dir := t.TempDir()
	tm := fullTeam()
	a := config.Agent{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet", Focus: "api"}
	base := ConfigHash(tm, a, dir)
	if ConfigHash(tm, a, dir) != base {
		t.Fatal("unstable")
	}
	mut := func(f func(*config.Team, *config.Agent)) string {
		t2, a2 := fullTeam(), a
		f(t2, &a2)
		return ConfigHash(t2, a2, dir)
	}
	for name, h := range map[string]string{
		"model":      mut(func(_ *config.Team, a *config.Agent) { a.Model = "haiku" }),
		"focus":      mut(func(_ *config.Team, a *config.Agent) { a.Focus = "ui" }),
		"persona":    mut(func(_ *config.Team, a *config.Agent) { a.Persona = "x" }),
		"role":       mut(func(_ *config.Team, a *config.Agent) { a.Role = config.RoleFocusGroup }),
		"permission": mut(func(t *config.Team, _ *config.Agent) { t.Defaults.PermissionMode = "plan" }),
		"agent mode": mut(func(_ *config.Team, a *config.Agent) { a.PermissionMode = "auto" }),
		"alias":      mut(func(t *config.Team, _ *config.Agent) { t.Models["sonnet"] = "claude-sonnet-x" }),
	} {
		if h == base {
			t.Errorf("%s change did not alter hash", name)
		}
	}
	// unrelated changes must not
	if mut(func(t *config.Team, _ *config.Agent) { t.Defaults.MaxTurnsPerTask = 7; t.Project.Description = "z" }) != base {
		t.Error("limits/description should not restart agents")
	}
	os.MkdirAll(filepath.Join(dir, ".slai", "prompts"), 0o755)
	os.WriteFile(filepath.Join(dir, ".slai", "prompts", "engineer.md"), []byte("custom"), 0o644)
	if ConfigHash(tm, a, dir) == base {
		t.Error("prompt override did not alter hash")
	}
}

func TestReconcileRestartsOnlyChanged(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	for _, id := range []string{"orch", "eng-1", "eng-2"} {
		if v, ok, _ := s.Store.GetKV(CfgKey(id)); !ok || v == "" {
			t.Fatalf("no config hash stored for %s", id)
		}
	}
	oldOrch, _, _ := s.Store.GetKV(CfgKey("orch"))
	if p, err := s.Reconcile(ctx); err != nil || !p.Empty() {
		t.Fatalf("no-op reconcile: %+v %v", p, err)
	}
	// edit: eng-1 model changes, eng-2 removed, eng-3 added
	s.Team.Agents = []config.Agent{
		s.Team.Agents[0],
		{ID: "eng-1", Role: config.RoleEngineer, Model: "haiku"},
		{ID: "eng-3", Role: config.RoleEngineer, Model: "sonnet"},
	}
	p, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Restart, []string{"eng-1"}) || !reflect.DeepEqual(p.Stop, []string{"eng-2"}) || !reflect.DeepEqual(p.Start, []string{"eng-3"}) {
		t.Fatalf("%+v", p)
	}
	for id, want := range map[string]bool{"orch": true, "eng-1": true, "eng-2": false, "eng-3": true} {
		if live, _ := s.Tmux.HasSession(ctx, s.Session(id)); live != want {
			t.Errorf("%s live=%v want %v", id, live, want)
		}
	}
	if v, _, _ := s.Store.GetKV(CfgKey("orch")); v != oldOrch {
		t.Error("orch hash changed")
	}
	if v, _, _ := s.Store.GetKV(CfgKey("eng-1")); v != ConfigHash(s.Team, s.Team.Agents[1], s.Dir) {
		t.Error("eng-1 hash not refreshed")
	}
	if _, err := s.Store.AgentByID("eng-2"); err == nil {
		t.Error("eng-2 row not deleted")
	}
	if !hasEvent(s, "agent_restarted") {
		t.Error("no agent_restarted event")
	}
}

func TestTrustGateStaggersStartup(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	s := unitEnv(t, team(5))
	s.GateOnTrust = true
	s.Tmux = tmux.New(fmt.Sprintf("slaigate-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = s.Tmux.KillServer(context.Background()) })
	ctx := context.Background()

	started, err := s.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != "orch" {
		t.Fatalf("untrusted folder: only the orchestrator may start, got %v", started)
	}
	s.Tick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); live {
		t.Fatal("eng-1 started before trust was recorded")
	}

	// the user answers "trust this folder?": Claude records it for the project
	rec := fmt.Sprintf(`{"projects":{%q:{"hasTrustDialogAccepted":true}}}`, s.Dir)
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx)
	for _, a := range s.Team.Agents {
		if live, _ := s.Tmux.HasSession(ctx, s.Session(a.ID)); !live {
			t.Errorf("%s not started after trust was recorded", a.ID)
		}
	}
	if v, _, _ := s.Store.GetKV(KVPendingStart); v != "" {
		t.Errorf("pending list not cleared: %q", v)
	}
}

func TestTeardownKillsEverythingAgentsStarted(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	// a detached process an agent left behind (a dev server, a Claude background session)
	stray := exec.Command("sleep", "60")
	stray.Env = append(os.Environ(), "SLAI_PROJECT="+s.Dir)
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	go stray.Wait()
	innocent := exec.Command("sleep", "60")
	innocent.Env = append(os.Environ(), "SLAI_PROJECT=/some/other/project")
	if err := innocent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = innocent.Process.Kill(); _, _ = innocent.Process.Wait() })

	rep, err := s.Teardown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sessions) != len(s.Team.Agents) {
		t.Errorf("killed %d sessions, want %d: %v", len(rep.Sessions), len(s.Team.Agents), rep.Sessions)
	}
	if rep.Killed < 1 || len(rep.Survivors) != 0 {
		t.Errorf("killed=%d survivors=%v", rep.Killed, rep.Survivors)
	}
	if left := s.Sessions(ctx); len(left) != 0 {
		t.Errorf("sessions left: %v", left)
	}
	if syscall.Kill(stray.Process.Pid, 0) == nil {
		t.Error("stray agent process survived Teardown")
	}
	if syscall.Kill(innocent.Process.Pid, 0) != nil {
		t.Error("Teardown killed a process from another project")
	}
}

var sessionIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)
