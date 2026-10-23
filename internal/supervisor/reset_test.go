package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func queueReset(t *testing.T, s *Supervisor, id, trigger, requester string) service.ResetReq {
	t.Helper()
	req, queued, err := service.QueueReset(s.Store, id, service.ResetReq{Trigger: trigger, Requester: requester, Reason: "test"})
	if err != nil || !queued {
		t.Fatalf("queue: %v %v", queued, err)
	}
	return req
}

func sessionID(t *testing.T, s *Supervisor, id string) string {
	t.Helper()
	b, err := os.ReadFile(harness.SessionIDPath(s.Dir, id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// startHook plays the SessionStart hook of the restarted session.
func startHook(t *testing.T, s *Supervisor, id string) {
	t.Helper()
	opt := hooks.Options{IdentityLine: func(a string) string { return roles.IdentityLineFor(s.Team, a) }}
	if _, err := hooks.Handle(s.Store, id, "SessionStart", hooks.Payload{Source: "startup", SessionID: "x"}, opt); err != nil {
		t.Fatal(err)
	}
}

func eventsOf(s *Supervisor, kind string) []store.Event {
	var out []store.Event
	evs, _ := s.Store.RecentEvents(2000)
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A1/A5/A9/C5: an idle agent is restarted in a fresh session, re-grounded with the identity
// line as the first thing typed, and counts as cleared only once it has called whoami.
func TestResetRestartsFreshAndIsClearedOnWhoami(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	before := sessionID(t, s, "eng-1")
	if before == "" {
		t.Fatal("no session id before the reset")
	}
	s.Store.AddUsage("eng-1", 90000, 1000, 0.5)
	req := queueReset(t, s, "eng-1", service.TriggerUser, "")
	s.resetTick(ctx)
	if after := sessionID(t, s, "eng-1"); after == "" || after == before {
		t.Fatalf("session id %q -> %q: not a fresh session", before, after)
	}
	cur, ok := service.LoadReset(s.Store, "eng-1")
	if !ok || cur.ResetAt == 0 {
		t.Fatalf("request after restart: %+v %v", cur, ok)
	}
	if _, done := service.LoadResetRes(s.Store, "eng-1"); done {
		t.Fatal("reported cleared before the agent called whoami")
	}
	startHook(t, s, "eng-1")
	ev := eventsOf(s, "context_reset")
	if len(ev) != 1 || !strings.Contains(ev[0].Data, `"trigger":"user"`) || !strings.Contains(ev[0].Data, `"tokens_before":90000`) {
		t.Fatalf("context_reset events: %+v", ev)
	}
	if len(eventsOf(s, "revive_fresh")) != 0 {
		t.Fatal("a deliberate reset was recorded as revive_fresh")
	}
	s.wakeTick(ctx)
	waitPane(t, s, "eng-1", "You are eng-1, the engineer of this team")
	s.resetTick(ctx) // still no whoami
	if _, done := service.LoadResetRes(s.Store, "eng-1"); done {
		t.Fatal("cleared without whoami")
	}
	s.Store.SetKV(service.KVWhoami+"eng-1", strconv.FormatInt(time.Now().Unix()+1, 10))
	s.resetTick(ctx)
	res, done := service.LoadResetRes(s.Store, "eng-1")
	if !done || res.State != "cleared" || res.Req != req.At {
		t.Fatalf("result: %+v %v", res, done)
	}
	if _, pending := service.LoadReset(s.Store, "eng-1"); pending {
		t.Fatal("request still pending after cleared")
	}
}

// A2: a busy agent is never interrupted; the reset waits and runs when it is idle.
func TestResetDefersWhileBusy(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	before := sessionID(t, s, "eng-1")
	s.Store.SetAgentStatus("eng-1", "working")
	queueReset(t, s, "eng-1", service.TriggerUser, "")
	s.resetTick(ctx)
	req, _ := service.LoadReset(s.Store, "eng-1")
	if !req.Deferred || req.ResetAt != 0 || sessionID(t, s, "eng-1") != before {
		t.Fatalf("busy agent was not deferred: %+v", req)
	}
	s.Store.SetAgentStatus("eng-1", "idle")
	s.resetTick(ctx)
	if req, _ = service.LoadReset(s.Store, "eng-1"); req.ResetAt == 0 || sessionID(t, s, "eng-1") == before {
		t.Fatalf("idle agent was not restarted: %+v", req)
	}
}

// A wake-up in flight (woken, unread) also counts as busy.
func TestResetDefersWhileWakeInFlight(t *testing.T) {
	s := tmuxEnv(t, team(5))
	id, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "x"})
	s.Store.MarkWoken(id)
	queueReset(t, s, "eng-1", service.TriggerUser, "")
	s.resetTick(context.Background())
	if req, _ := service.LoadReset(s.Store, "eng-1"); !req.Deferred || req.ResetAt != 0 {
		t.Fatalf("%+v", req)
	}
}

// taskOnBranch gives eng-1 an in-progress task whose branch has (or lacks) a commit and
// whose worktree is (or is not) dirty.
func taskOnBranch(t *testing.T, s *Supervisor, commit, dirty bool) store.Task {
	t.Helper()
	branch := "slai/eng-1/T-0001" // the first task of a fresh store
	task, err := s.Store.CreateTask(store.Task{Title: "work", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress, Branch: branch})
	if err != nil || task.ID != "T-0001" {
		t.Fatalf("task: %+v %v", task, err)
	}
	wt := filepath.Join(s.Dir, ".slai", "worktrees", "eng-1")
	sh(t, wt, "git", "checkout", "-b", branch)
	if commit {
		os.WriteFile(filepath.Join(wt, "done.txt"), []byte("x\n"), 0o644)
		sh(t, wt, "git", "add", "done.txt")
		sh(t, wt, "git", "commit", "-m", "work")
	}
	if dirty {
		os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("x\n"), 0o644)
	}
	return task
}

// A3: an engineer holding work that exists only in its conversation is not reset.
func TestResetRefusedForUnrecordedWork(t *testing.T) {
	for _, tc := range []struct {
		name          string
		commit, dirty bool
		want          string
	}{
		{"uncommitted changes", true, true, "uncommitted changes"},
		{"no commit on the branch", false, false, "no commit on its branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmuxEnv(t, team(5))
			before := sessionID(t, s, "eng-1")
			taskOnBranch(t, s, tc.commit, tc.dirty)
			req := queueReset(t, s, "eng-1", service.TriggerOrch, "orch")
			s.resetTick(context.Background())
			res, ok := service.LoadResetRes(s.Store, "eng-1")
			if !ok || res.State != "skipped" || !strings.Contains(res.Detail, tc.want) || res.Req != req.At {
				t.Fatalf("result: %+v %v", res, ok)
			}
			if sessionID(t, s, "eng-1") != before {
				t.Fatal("a refused reset restarted the agent")
			}
			if _, pending := service.LoadReset(s.Store, "eng-1"); pending {
				t.Fatal("refused request still pending")
			}
			msgs, _ := s.Store.UndeliveredFor("orch") // the requester is told
			if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "skipped") {
				t.Fatalf("requester not told: %+v", msgs)
			}
		})
	}
}

// A3 otherwise + A4 + A5: committed, clean work: slai asks the agent to record progress,
// clears it once the note is read and it is idle, everything outside the conversation
// survives, and the agent is told to pick its task up again without a human message.
func TestResetKeepsWhatShouldSurviveAndResumesTheTask(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	task := taskOnBranch(t, s, true, false)
	other, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Body: "unread mail"})
	s.Store.AddUsage("eng-1", 1000, 100, 0.25)
	spendBefore, _ := s.Store.UsageByAgent(0)
	queueReset(t, s, "eng-1", service.TriggerUser, "")
	s.resetTick(ctx)
	req, _ := service.LoadReset(s.Store, "eng-1")
	if req.NoteID == 0 || req.ResetAt != 0 {
		t.Fatalf("expected the record-progress note first: %+v", req)
	}
	s.resetTick(ctx) // the note is unread: not yet
	if cur, _ := service.LoadReset(s.Store, "eng-1"); cur.ResetAt != 0 {
		t.Fatal("cleared before the agent read the note")
	}
	s.Store.MarkDelivered(req.NoteID)
	s.Store.MarkWoken(other) // unread mail stays unread, but its wake-up is not "in flight" once old:
	s.Now = func() time.Time { return time.Now().Add(2 * WakeRetry) }
	s.resetTick(ctx)
	if cur, _ := service.LoadReset(s.Store, "eng-1"); cur.ResetAt == 0 {
		t.Fatalf("not restarted after the note was read: %+v", cur)
	}
	// A4: what survives
	if m, _ := s.Store.UndeliveredFor("eng-1"); len(m) == 0 || m[0].ID != other {
		t.Fatalf("unread inbox lost: %+v", m)
	}
	if got, err := s.Store.GetTask(task.ID); err != nil || got.Assignee != "eng-1" || got.Branch == "" {
		t.Fatalf("task: %+v %v", got, err)
	}
	if !s.Git.BranchExists(ctx, task.Branch) {
		t.Fatal("task branch gone")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".slai", "worktrees", "eng-1", "done.txt")); err != nil {
		t.Fatalf("worktree content lost: %v", err)
	}
	if spendAfter, _ := s.Store.UsageByAgent(0); spendAfter["eng-1"] != spendBefore["eng-1"] {
		t.Fatalf("spend changed: %v -> %v", spendBefore, spendAfter)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, ".slai", "run", "eng-1", "system.md")); err != nil || len(b) == 0 {
		t.Fatalf("role prompt lost: %v", err)
	}
	// A5: no human input: the hook re-grounds, the next wake types identity then get_task
	startHook(t, s, "eng-1")
	s.wakeTick(ctx)
	waitPane(t, s, "eng-1", "You are eng-1, the engineer of this team")
	waitPane(t, s, "eng-1", "Call get_inbox, then get_task.")
}

// C5: no whoami in time: the identity line is sent again once, then the user is told.
func TestResetRegroundResendsThenGivesUp(t *testing.T) {
	oldR, oldG := ResetResend, ResetGiveUp
	ResetResend, ResetGiveUp = time.Minute, time.Minute
	t.Cleanup(func() { ResetResend, ResetGiveUp = oldR, oldG })
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	queueReset(t, s, "eng-2", service.TriggerOrch, "orch")
	s.resetTick(ctx)
	startHook(t, s, "eng-2")
	s.Store.MarkDelivered(func() []int64 { // the hook's own note: the agent "read" it
		var ids []int64
		m, _ := s.Store.UndeliveredFor("eng-2")
		for _, x := range m {
			ids = append(ids, x.ID)
		}
		return ids
	}()...)
	notes := func() int {
		m, _ := s.Store.UndeliveredFor("eng-2")
		return len(m)
	}
	s.resetTick(ctx)
	if notes() != 0 {
		t.Fatal("re-sent too early")
	}
	clock = clock.Add(ResetResend + time.Second)
	s.resetTick(ctx)
	if notes() != 1 || len(eventsOf(s, "context_reset_reground_resent")) != 1 {
		t.Fatalf("identity line not re-sent once: %d notes", notes())
	}
	if v, _, _ := s.Store.GetKV(hooks.KVIdentityPending + "eng-2"); v == "" {
		t.Fatal("re-send did not mark the wake line")
	}
	s.resetTick(ctx) // still waiting: no third send
	if len(eventsOf(s, "context_reset_reground_resent")) != 1 {
		t.Fatal("re-sent twice")
	}
	clock = clock.Add(ResetGiveUp + time.Second)
	s.resetTick(ctx)
	res, ok := service.LoadResetRes(s.Store, "eng-2")
	if !ok || res.State != "failed" || !strings.Contains(res.Detail, "did not re-ground after reset") {
		t.Fatalf("result: %+v %v", res, ok)
	}
	escs, _ := s.Store.OpenEscalations()
	found := false
	for _, e := range escs {
		found = found || (e.Agent == "eng-2" && e.Kind == EscRegroundFailed && e.User)
	}
	if !found {
		t.Fatalf("user not told it needs them: %+v", escs)
	}
	if m, _ := s.Store.UndeliveredFor("orch"); len(m) == 0 || !strings.Contains(m[len(m)-1].Body, "failed") {
		t.Fatalf("requester not told: %+v", m)
	}
}

// A11: resetting the orchestrator hands it the board in its wake-up.
func TestResetOfTheOrchestratorCarriesTheBoard(t *testing.T) {
	s := tmuxEnv(t, team(5))
	s.Store.CreateTask(store.Task{Title: "ship the thing", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	queueReset(t, s, "orch", service.TriggerUser, "")
	s.resetTick(context.Background())
	var board string
	m, _ := s.Store.UndeliveredFor("orch")
	for _, x := range m {
		if strings.Contains(x.Body, "Your context was reset") {
			board = x.Body
		}
	}
	if !strings.Contains(board, "T-0001") || !strings.Contains(board, "eng-2") || !strings.Contains(board, "ship the thing") || !strings.Contains(board, "in_progress") {
		t.Fatalf("board summary: %q", board)
	}
}

// Guards hold in the engine too: only the user resets the orchestrator; an agent never itself.
func TestResetGuardsRefuseInTheEngine(t *testing.T) {
	s := tmuxEnv(t, team(5))
	before := sessionID(t, s, "orch")
	queueReset(t, s, "orch", service.TriggerOrch, "eng-1")
	s.resetTick(context.Background())
	if res, ok := service.LoadResetRes(s.Store, "orch"); !ok || res.State != "skipped" || !strings.Contains(res.Detail, "only the user") {
		t.Fatalf("%+v %v", res, ok)
	}
	queueReset(t, s, "eng-1", service.TriggerOrch, "eng-1")
	s.resetTick(context.Background())
	if res, _ := service.LoadResetRes(s.Store, "eng-1"); res.State != "skipped" || !strings.Contains(res.Detail, "own context") {
		t.Fatalf("%+v", res)
	}
	if sessionID(t, s, "orch") != before {
		t.Fatal("orchestrator restarted")
	}
}
