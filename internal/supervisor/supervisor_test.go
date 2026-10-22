package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

var fakeBinDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeclaude")
	if err != nil {
		panic(err)
	}
	fakeBinDir = dir
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "claude"), "github.com/oorbt/slai/testdata/fakeclaude")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakeclaude: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func team(maxConc int) *config.Team {
	return &config.Team{
		Version:  1,
		Project:  config.Project{Name: "p", MainBranch: "main", IntegrationBranch: "slai/integration"},
		Defaults: config.Defaults{PermissionMode: "manual", MaxTurnsPerTask: 40, MaxConcurrentAgents: maxConc},
		Models:   config.DefaultModels(),
		Agents: []config.Agent{
			{ID: "orch", Role: config.RoleOrchestrator, Model: "sonnet"},
			{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"},
			{ID: "eng-2", Role: config.RoleEngineer, Model: "sonnet"},
		},
	}
}

// unitEnv: store + git repo, no tmux sessions.
func unitEnv(t *testing.T, tm *config.Team) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	sh(t, dir, "git", "init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".slai/\n"), 0o644)
	sh(t, dir, "git", "add", ".")
	sh(t, dir, "git", "commit", "-m", "init")
	if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Supervisor{Dir: dir, Team: tm, Store: st, Git: &git.Repo{Dir: dir}, Tmux: tmux.New("unused"),
		Log: t.Logf}
}

func tmuxEnv(t *testing.T, tm *config.Team) *Supervisor {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	s := unitEnv(t, tm)
	s.Tmux = tmux.New(fmt.Sprintf("slaisup-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = s.Tmux.KillServer(context.Background()) })
	if _, err := s.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func status(t *testing.T, s *Supervisor, id string) string {
	t.Helper()
	as, _ := s.Store.GetAgents()
	for _, a := range as {
		if a.ID == id {
			return a.Status
		}
	}
	t.Fatalf("no agent %s", id)
	return ""
}

func pane(t *testing.T, s *Supervisor, id string) string {
	out, _ := s.Tmux.CapturePane(context.Background(), s.Session(id), false, 100)
	return out
}

func waitPane(t *testing.T, s *Supervisor, id, want string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if strings.Contains(pane(t, s, id), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pane %s never showed %q:\n%s", id, want, pane(t, s, id))
}

func hasEvent(s *Supervisor, kind string) bool {
	evs, _ := s.Store.RecentEvents(2000)
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestWakeOnlyWhenIdle(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	s.Store.SetAgentStatus("eng-1", "working")
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "a"})
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "b"})
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "c"})
	s.Tick(ctx)
	waitPane(t, s, "eng-2", "[slai] You have 2 new items. Call get_inbox.")
	if strings.Contains(pane(t, s, "eng-1"), "[slai]") {
		t.Fatal("working agent was woken")
	}
	if got := status(t, s, "eng-2"); got != "working" {
		t.Fatalf("eng-2 status %s", got)
	}
	m, _ := s.Store.UndeliveredFor("eng-2")
	if len(m) != 2 || m[0].WokenAt == 0 {
		t.Fatalf("woken not marked: %+v", m)
	}
	// Not re-woken within 60s even if idle again.
	base := strings.Count(pane(t, s, "eng-2"), "[slai]")
	s.Store.SetAgentStatus("eng-2", "idle")
	s.Tick(ctx)
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(pane(t, s, "eng-2"), "[slai]"); n != base {
		t.Fatalf("re-woken too early: %d vs %d", n, base)
	}
	// ... but re-woken after 60s.
	s.Now = func() time.Time { return time.Now().Add(2 * WakeRetry) }
	s.Store.AddEvent("eng-2", "hook:stop", "{}") // the agent ended its turn without reading
	s.Store.SetAgentStatus("eng-2", "idle")
	s.Tick(ctx)
	time.Sleep(500 * time.Millisecond)
	if n := strings.Count(pane(t, s, "eng-2"), "[slai]"); n <= base {
		t.Fatalf("expected re-wake, got %d (base %d)", n, base)
	}
}

func TestPauseBlocksWake(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "a"})
	if err := s.Pause(); err != nil || !s.Paused() {
		t.Fatal("pause not persisted")
	}
	s.Tick(ctx)
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(pane(t, s, "eng-1"), "[slai]") {
		t.Fatal("woken while paused")
	}
	s.Resume()
	if s.Paused() {
		t.Fatal("still paused")
	}
	s.Tick(ctx)
	waitPane(t, s, "eng-1", "[slai]")
	// per-agent pause
	s.Store.SetAgentStatus("eng-2", "idle")
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "a"})
	s.PauseAgent("eng-2")
	s.Tick(ctx)
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(pane(t, s, "eng-2"), "[slai]") {
		t.Fatal("paused agent woken")
	}
}

func TestConcurrencyCap(t *testing.T) {
	s := tmuxEnv(t, team(1))
	s.Store.SetAgentStatus("eng-1", "working")
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "x"})
	s.Tick(context.Background())
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(pane(t, s, "eng-2"), "[slai]") {
		t.Fatal("woken beyond cap")
	}
	s.Store.SetAgentStatus("eng-1", "idle")
	s.Tick(context.Background())
	waitPane(t, s, "eng-2", "[slai]")
}

func TestAssignmentPreparesBranch(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "do it", Assignee: "eng-1", CreatedBy: "orch"})
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: task.ID, Body: "go"})
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	// leftover uncommitted work must not block the assignment: it is stashed,
	// the engineer is told, and the wake goes through
	os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644)
	s.Tick(ctx)
	waitPane(t, s, "eng-1", "[slai]")
	if !hasEvent(s, "worktree_stashed") {
		t.Fatal("dirty worktree should be stashed with a worktree_stashed event")
	}
	if _, err := os.Stat(filepath.Join(wt, "scratch.txt")); err == nil {
		t.Fatal("scratch.txt should have been stashed away")
	}
	if got := sh(t, wt, "git", "stash", "list"); !strings.Contains(got, "auto-stash before "+task.ID) {
		t.Fatalf("stash not recoverable: %q", got)
	}
	if hasEvent(s, "wake_error") {
		t.Fatal("stashing must not produce a wake_error")
	}
	want := "slai/eng-1/" + task.ID
	if got := strings.TrimSpace(sh(t, wt, "git", "rev-parse", "--abbrev-ref", "HEAD")); got != want {
		t.Fatalf("branch %q want %q", got, want)
	}
	if got, _ := s.Store.GetTask(task.ID); got.Branch != want {
		t.Fatalf("task branch not stored: %q", got.Branch)
	}
}

func commitOnTask(t *testing.T, s *Supervisor, task store.Task, file, content string) {
	t.Helper()
	wt := filepath.Join(s.Dir, ".slai", "worktrees", task.Assignee)
	sh(t, wt, "git", "checkout", "-B", task.Branch, "slai/integration")
	os.WriteFile(filepath.Join(wt, file), []byte(content), 0o644)
	sh(t, wt, "git", "add", ".")
	sh(t, wt, "git", "commit", "-m", "work "+task.ID)
}

func requestMerge(s *Supervisor, id string) {
	s.Store.AddEvent("principal", "merge_requested", fmt.Sprintf(`{"task_id":%q,"notes":"lgtm"}`, id))
}

func TestMergeSuccessAndConflict(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	mk := func(assignee, title string) store.Task {
		tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: store.StatusInReview})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		return tk
	}
	t1 := mk("eng-1", "first")
	t2 := mk("eng-2", "second")
	commitOnTask(t, s, t1, "f.txt", "one\n")
	commitOnTask(t, s, t2, "f.txt", "two\n")
	requestMerge(s, t1.ID)
	requestMerge(s, t2.ID)
	s.Tick(ctx)

	g1, _ := s.Store.GetTask(t1.ID)
	if g1.Status != store.StatusMerged || !hasEvent(s, "merged") {
		t.Fatalf("t1 not merged: %+v", g1)
	}
	log := sh(t, s.Dir, "git", "log", "--oneline", "--merges", "slai/integration")
	if !strings.Contains(log, "["+t1.ID+"] first") {
		t.Fatalf("merge commit missing:\n%s", log)
	}
	// user's main checkout untouched
	if b := strings.TrimSpace(sh(t, s.Dir, "git", "rev-parse", "--abbrev-ref", "HEAD")); b != "main" {
		t.Fatalf("main checkout moved to %s", b)
	}
	g2, _ := s.Store.GetTask(t2.ID)
	if g2.Status != store.StatusInProgress || !hasEvent(s, "merge_conflict") {
		t.Fatalf("t2 should be in_progress after conflict: %+v", g2)
	}
	msgs, _ := s.Store.UndeliveredFor("eng-2")
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Body, "f.txt") {
		t.Fatalf("conflict message missing: %+v", msgs)
	}
	if dirty, _ := (&git.Repo{Dir: s.IntegrationWorktree()}).IsDirty(ctx); dirty {
		t.Fatal("integration worktree left dirty after abort")
	}
	// cursor persisted: another tick must not reprocess
	before, _ := s.Store.UndeliveredFor("eng-2")
	s.Tick(ctx)
	after, _ := s.Store.UndeliveredFor("eng-2")
	if len(before) != len(after) {
		t.Fatal("merge reprocessed")
	}
	if v, _, _ := s.Store.GetKV(KVMergeCursor); v == "" {
		t.Fatal("cursor not stored")
	}
}

func TestLoopGuard(t *testing.T) {
	s := unitEnv(t, team(5))
	for _, a := range s.Team.Agents {
		s.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	for i := 0; i < LoopMessageThreshold; i++ {
		from, to := "eng-1", "eng-2"
		if i%2 == 1 {
			from, to = to, from
		}
		s.Store.AddMessage(store.Message{From: from, To: to, Body: "x"})
	}
	if got := s.checkLoops(); len(got) != 0 {
		t.Fatalf("at threshold must not trip: %v", got)
	}
	s.Store.AddMessage(store.Message{From: "eng-1", To: "eng-2", Body: "x"})
	if got := s.checkLoops(); len(got) != 1 {
		t.Fatalf("should trip: %v", got)
	}
	p, _ := s.Store.PausedAgents()
	if !p["eng-1"] || !p["eng-2"] || p["orch"] || !hasEvent(s, "ask_user") {
		t.Fatalf("pause/event wrong: %v", p)
	}
	// messages involving user/slai never count
	for i := 0; i < 30; i++ {
		s.Store.AddMessage(store.Message{From: "user", To: "orch", Body: "x"})
	}
	s.ResumeAgent("eng-1")
	s.ResumeAgent("eng-2")
	if got := s.checkLoops(); len(got) != 0 {
		t.Fatalf("resume should reset window: %v", got)
	}
}

func TestSpendCaps(t *testing.T) {
	tm := team(5)
	tm.Defaults.Caps = config.SpendCaps{PerAgentUSD: 10, DailyUSD: 20}
	s := unitEnv(t, tm)
	count := func(kind string) int {
		evs, _ := s.Store.RecentEvents(2000)
		n := 0
		for _, e := range evs {
			if e.Kind == kind {
				n++
			}
		}
		return n
	}
	if g := s.checkSpend(); g.daily || len(g.agents) != 0 {
		t.Fatal("no spend, no gate")
	}
	s.Store.AddUsage("eng-1", 1, 1, 7.9)
	if g := s.checkSpend(); g.blocked("eng-1") || count("spend_warn") != 0 {
		t.Fatal("79% must not warn")
	}
	s.Store.AddUsage("eng-1", 1, 1, 0.2) // 8.1 = 81%
	if g := s.checkSpend(); g.blocked("eng-1") || count("spend_warn") != 1 {
		t.Fatalf("81%% should warn once, got %d", count("spend_warn"))
	}
	s.checkSpend()
	if count("spend_warn") != 1 {
		t.Fatal("warn must be deduped")
	}
	s.Store.AddUsage("eng-1", 1, 1, 2) // 10.1
	g := s.checkSpend()
	if !g.blocked("eng-1") || g.blocked("eng-2") || g.daily || count("spend_cap") != 1 || count("ask_user") != 1 {
		t.Fatalf("agent cap: %+v caps=%d", g, count("spend_cap"))
	}
	s.Store.AddUsage("eng-2", 1, 1, 9.9) // total 20.0 = daily cap
	g = s.checkSpend()
	if !g.daily || !g.blocked("orch") {
		t.Fatal("daily cap should block everyone")
	}
}

func TestSpendCapBlocksWake(t *testing.T) {
	tm := team(5)
	tm.Defaults.Caps = config.SpendCaps{PerAgentUSD: 1}
	s := tmuxEnv(t, tm)
	s.Store.AddUsage("eng-1", 1, 1, 5)
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "a"})
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-2", Body: "a"})
	s.Tick(context.Background())
	waitPane(t, s, "eng-2", "[slai]")
	if strings.Contains(pane(t, s, "eng-1"), "[slai]") {
		t.Fatal("capped agent woken")
	}
}

func TestFallbackAndDead(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	s.Detect = func(string) (string, bool) { return "waiting", true }
	s.Tick(ctx)
	if got := status(t, s, "eng-1"); got != "idle" {
		t.Fatalf("fresh hook data must not be overridden, got %s", got)
	}
	s.Now = func() time.Time { return time.Now().Add(time.Minute) }
	s.Tick(ctx)
	if got := status(t, s, "eng-1"); got != "waiting" {
		t.Fatalf("fallback not applied: %s", got)
	}
	s.Tmux.KillSession(ctx, s.Session("eng-2"))
	s.Tick(ctx)
	if got := status(t, s, "eng-2"); got != "dead" {
		t.Fatalf("dead not detected: %s", got)
	}
}

func TestClientCountZero(t *testing.T) {
	s := tmuxEnv(t, team(5))
	n, err := s.Tmux.ClientCount(context.Background(), s.Session("orch"))
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Running(dir); ok {
		t.Fatal("nothing running")
	}
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := Running(dir); !ok || pid != os.Getpid() {
		t.Fatalf("running=%v pid=%d", ok, pid)
	}
	if _, err := Acquire(dir); err == nil {
		t.Fatal("second acquire must fail")
	}
	l.Release()
	if _, ok := Running(dir); ok {
		t.Fatal("released")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	s := unitEnv(t, team(5))
	s.Interval = 10 * time.Millisecond
	s.Tmux = tmux.New("slaiunused")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// A stale .started marker (conversation gone) makes the revive try --continue,
// which real claude rejects with "No conversation found to continue"; the
// `|| fresh` fallback must still leave a live session.
func TestReviveStaleMarkerFallsBackToFresh(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	state := t.TempDir()
	t.Setenv("FAKECLAUDE_NO_CONVERSATION_ON_CONTINUE", "1")
	t.Setenv("FAKECLAUDE_STATE", state)
	s := tmuxEnv(t, team(3))
	ctx := context.Background()
	for _, id := range []string{"orch", "eng-1"} {
		waitPane(t, s, id, "> ready")
	}
	if err := s.Down(ctx); err != nil {
		t.Fatal(err)
	}
	// the conversations vanish, the markers and session ids stay
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if started, err := s.Up(ctx); err != nil || len(started) != 3 {
		t.Fatalf("revive: %v %v", started, err)
	}
	for _, a := range s.Team.Agents {
		waitPane(t, s, a.ID, "No conversation found with session ID")
		waitPane(t, s, a.ID, "> ready")
		if live, _ := s.Tmux.HasSession(ctx, s.Session(a.ID)); !live {
			t.Errorf("%s has no live session after revive", a.ID)
		}
	}
	// and a revive that does have a conversation resumes it
	if err := s.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Up(ctx); err != nil {
		t.Fatal(err)
	}
	waitPane(t, s, "orch", "Continuing previous conversation")
}

// The bug that crossed agents: orch, pm and principal share the project directory,
// and `claude --continue` resumes the newest conversation in a directory. Every agent
// must resume its OWN session, and a marker left by an older build (no session id)
// must start fresh rather than guess.
func TestAgentsSharingADirectoryResumeTheirOwnSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	t.Setenv("FAKECLAUDE_NO_CONVERSATION_ON_CONTINUE", "1")
	t.Setenv("FAKECLAUDE_STATE", t.TempDir())
	s := tmuxEnv(t, team(3))
	ctx := context.Background()
	ids := map[string]string{}
	for _, a := range s.Team.Agents {
		id, existed, err := claude.LoadOrCreateSessionID(s.Dir, a.ID)
		if err != nil || !existed {
			t.Fatalf("%s: session id missing after first start (%v, existed=%v)", a.ID, err, existed)
		}
		for other, oid := range ids {
			if oid == id {
				t.Fatalf("%s and %s share session id %s", a.ID, other, id)
			}
		}
		ids[a.ID] = id
		waitPane(t, s, a.ID, "> ready")
	}
	if err := s.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Up(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Team.Agents {
		waitPane(t, s, a.ID, "Continuing previous conversation")
		if got, _, _ := claude.LoadOrCreateSessionID(s.Dir, a.ID); got != ids[a.ID] {
			t.Errorf("%s session id changed across revive: %s -> %s", a.ID, ids[a.ID], got)
		}
	}
	// a marker from an older build (it used --continue) with no session id: start fresh
	if err := s.Down(ctx); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(claude.SessionIDPath(s.Dir, "orch"))
	if _, err := s.Up(ctx); err != nil {
		t.Fatal(err)
	}
	waitPane(t, s, "orch", "> ready")
	if strings.Contains(pane(t, s, "orch"), "Continuing previous conversation") {
		t.Fatal("orch resumed a conversation it cannot prove is its own")
	}
	if got, _, _ := claude.LoadOrCreateSessionID(s.Dir, "orch"); got == ids["orch"] {
		t.Fatal("orch should have been given a new session id")
	}
}

// While the user is typing into an agent from the TUI (focus heartbeat), the
// supervisor must not inject a wake-up line into the same pane.
func TestNoWakeWhileUserFocused(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "a"})
	if err := s.Store.TouchFocus("eng-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx)
	time.Sleep(400 * time.Millisecond)
	if strings.Contains(pane(t, s, "eng-1"), "[slai]") {
		t.Fatal("woke an agent the user is focused on")
	}
	// heartbeat expires (or is cleared): the wake goes out
	_ = s.Store.ClearFocus("eng-1")
	s.Tick(ctx)
	waitPane(t, s, "eng-1", "[slai] You have 1 new items")
}

// Normal orchestration traffic (assignments, reports, review) between a busy
// orchestrator and an engineer must never trip the loop guard.
func TestLoopGuardIgnoresTaskTraffic(t *testing.T) {
	s := unitEnv(t, team(5))
	for _, a := range s.Team.Agents {
		s.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	for i := 0; i < 40; i++ {
		kind, from, to := "assignment", "orch", "eng-1"
		if i%2 == 1 {
			kind, from, to = "note", "eng-1", "orch"
		}
		s.Store.AddMessage(store.Message{From: from, To: to, Kind: kind, TaskID: fmt.Sprintf("T-%04d", i), Body: "work"})
	}
	if got := s.checkLoops(); len(got) != 0 {
		t.Fatalf("task traffic tripped the guard: %v", got)
	}
}

// A guard hold is a cool-down: it always ends on its own, so the team can
// never be left paused waiting for a human.
func TestLoopHoldReleasesByItself(t *testing.T) {
	s := unitEnv(t, team(5))
	for _, a := range s.Team.Agents {
		s.Store.UpsertAgent(store.Agent{ID: a.ID, Role: a.Role})
	}
	now := time.Now()
	s.Now = func() time.Time { return now }
	for i := 0; i <= LoopMessageThreshold; i++ {
		from, to := "eng-1", "eng-2"
		if i%2 == 1 {
			from, to = to, from
		}
		s.Store.AddMessage(store.Message{From: from, To: to, Kind: "note", Body: "chat"})
	}
	if got := s.checkLoops(); len(got) != 1 {
		t.Fatalf("should trip: %v", got)
	}
	if p, _ := s.Store.PausedAgents(); !p["eng-1"] || !p["eng-2"] {
		t.Fatalf("pair not held: %v", p)
	}
	s.releaseHolds()
	if p, _ := s.Store.PausedAgents(); !p["eng-1"] || !p["eng-2"] {
		t.Fatal("hold released too early")
	}
	now = now.Add(LoopHold + time.Second)
	s.releaseHolds()
	if p, _ := s.Store.PausedAgents(); len(p) != 0 {
		t.Fatalf("hold did not release by itself: %v", p)
	}
	if got := s.checkLoops(); len(got) != 0 {
		t.Fatalf("released pair must not instantly re-trip: %v", got)
	}
	// a manual pause (no hold key) is never auto-released
	s.Store.SetAgentPaused("orch", true)
	now = now.Add(time.Hour)
	s.releaseHolds()
	if p, _ := s.Store.PausedAgents(); !p["orch"] {
		t.Fatal("manual pause was auto-released")
	}
}

func mkReviewTask(t *testing.T, s *Supervisor, assignee, title string) store.Task {
	t.Helper()
	tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: store.StatusInReview})
	s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
	tk, _ = s.Store.GetTask(tk.ID)
	return tk
}

// A dirty or half-merged integration worktree must not cost an approved task its merge.
func TestMergeRecoversFromDirtyIntegrationWorktree(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	tk := mkReviewTask(t, s, "eng-1", "feature")
	commitOnTask(t, s, tk, "g.txt", "x\n")
	// something left the slai-owned integration checkout dirty (tracked edit + stray file)
	wt := s.IntegrationWorktree()
	os.WriteFile(filepath.Join(wt, "README.md"), []byte("scribble\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "stray.txt"), []byte("x"), 0o644)
	requestMerge(s, tk.ID)
	s.Tick(ctx)
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusMerged {
		t.Fatalf("approved task not merged after a dirty integration tree: %s", g.Status)
	}
}

// A transient failure keeps the approval and retries; repeated failure sends the task
// back to its engineer and tells everyone, instead of leaving it in review forever.
func TestMergeRetriesThenReturnsTaskAndNotifies(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	now := time.Now()
	s.Now = func() time.Time { return now }
	tk := mkReviewTask(t, s, "eng-1", "doomed")
	// the task branch does not exist, so every attempt fails with a non-conflict error
	requestMerge(s, tk.ID)
	for i := 0; i < MergeRetries; i++ {
		s.Tick(ctx)
		if g, _ := s.Store.GetTask(tk.ID); i < MergeRetries-1 && g.Status != store.StatusInReview {
			t.Fatalf("attempt %d: task left review too early (%s)", i+1, g.Status)
		}
		if cur := s.kvGet(KVMergeCursor); i < MergeRetries-1 && cur != "" {
			t.Fatalf("attempt %d: cursor advanced past an unmerged approval", i+1)
		}
		now = now.Add(MergeRetryEvery + time.Second)
	}
	g, _ := s.Store.GetTask(tk.ID)
	if g.Status != store.StatusInProgress {
		t.Fatalf("task should return to in_progress after %d failures, is %s", MergeRetries, g.Status)
	}
	for _, who := range []string{"eng-1", "orch"} {
		var told bool
		ms, _ := s.Store.UndeliveredFor(who)
		for _, m := range ms {
			if strings.Contains(m.Body, "could not be merged") {
				told = true
			}
		}
		if !told {
			t.Errorf("%s was not told the merge failed", who)
		}
	}
}

func TestMergeSkipsApprovedThenCancelledTask(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	tk := mkReviewTask(t, s, "eng-1", "dropped")
	commitOnTask(t, s, tk, "h.txt", "x\n")
	requestMerge(s, tk.ID)
	if err := s.Store.UpdateTaskStatus(tk.ID, store.StatusCancelled); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx)
	if g, _ := s.Store.GetTask(tk.ID); g.Status != store.StatusCancelled {
		t.Fatalf("cancelled task changed to %s", g.Status)
	}
	if out := sh(t, s.Dir, "git", "log", "--oneline", "slai/integration"); strings.Contains(out, "dropped") {
		t.Fatal("cancelled work was merged")
	}
}

// Each task lands on integration once: the merge commit's subject differs from the
// task's own commit subjects, a branch cut after the first merge adds only its own
// commit, and the approver hears the outcome.
func TestSequentialMergesNoDuplicateSubjectsAndApproverNotified(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	mk := func(assignee, title string) store.Task {
		tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: assignee, Status: store.StatusInReview})
		s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch(assignee, tk.ID)})
		tk, _ = s.Store.GetTask(tk.ID)
		return tk
	}
	commit := func(task store.Task, file string) {
		wt := filepath.Join(s.Dir, ".slai", "worktrees", task.Assignee)
		sh(t, wt, "git", "checkout", "-B", task.Branch, "slai/integration")
		os.WriteFile(filepath.Join(wt, file), []byte("x\n"), 0o644)
		sh(t, wt, "git", "add", ".")
		sh(t, wt, "git", "commit", "-m", "["+task.ID+"] "+task.Title)
	}
	t1 := mk("eng-1", "first")
	commit(t1, "a.txt")
	requestMerge(s, t1.ID)
	s.Tick(ctx)
	t2 := mk("eng-1", "second") // branch created after the first task merged
	commit(t2, "b.txt")
	requestMerge(s, t2.ID)
	s.Tick(ctx)
	for _, id := range []string{t1.ID, t2.ID} {
		if g, _ := s.Store.GetTask(id); g.Status != store.StatusMerged {
			t.Fatalf("%s not merged: %+v", id, g)
		}
	}
	seen := map[string]int{}
	for _, subj := range strings.Split(strings.TrimSpace(sh(t, s.Dir, "git", "log", "--format=%s", "main..slai/integration")), "\n") {
		seen[subj]++
	}
	for subj, n := range seen {
		if n > 1 {
			t.Errorf("subject %q appears %d times on integration", subj, n)
		}
	}
	if n := strings.Count(sh(t, s.Dir, "git", "log", "--format=%s", "slai/integration"), "["+t1.ID+"] first"); n != 2 {
		t.Errorf("want task commit + merge commit once each, got %d lines", n)
	}
	got := 0
	for _, m := range mustUndelivered(t, s, "principal") {
		if strings.Contains(m.Body, "was merged into") {
			got++
		}
	}
	if got != 2 {
		t.Errorf("approver got %d merge outcome notes, want 2", got)
	}
}
