package supervisor

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// healthEnv: a running team with the driver on and a controllable clock.
func healthEnv(t *testing.T) (*Supervisor, *time.Time) {
	t.Helper()
	s, now := driverEnv(t)
	return s, now
}

func countEvents(s *Supervisor, kind string) int {
	evs, _ := s.Store.RecentEvents(500)
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func openEsc(t *testing.T, s *Supervisor, kind, agent string) (store.Escalation, bool) {
	t.Helper()
	es, err := s.Store.OpenEscalations()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Kind == kind && e.Agent == agent {
			return e, true
		}
	}
	return store.Escalation{}, false
}

// show prints text into the agent's pane without submitting it.
func show(t *testing.T, s *Supervisor, id string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if err := s.Tmux.SendKeys(context.Background(), s.Session(id), l, false); err != nil {
			t.Fatal(err)
		}
		waitPane(t, s, id, l)
	}
}

func slaiMsgs(t *testing.T, s *Supervisor, to string) []store.Message {
	return pendingFrom(t, s, to, "slai")
}

// A second Supervisor on the same store stands in for a supervisor restart (AC 15).
func restarted(s *Supervisor, now *time.Time) *Supervisor {
	return &Supervisor{Dir: s.Dir, Team: s.Team, Store: s.Store, Git: s.Git, Tmux: s.Tmux, Driver: true,
		Now: func() time.Time { return *now }, Log: s.Log}
}

func TestRateLimitedAgentIsLeftAloneAndWokenAfterReset(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	reset := now.Add(time.Hour).Truncate(time.Minute).Add(time.Minute)
	show(t, s, "eng-1", "5-hour limit reached ∙ resets "+reset.Format("3:04pm"))
	_ = s.Store.SetAgentStatus("eng-1", "rate_limited")

	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscRateLimited, "eng-1")
	if !ok || e.User || !strings.Contains(e.Text, reset.Format("15:04")) {
		t.Fatalf("rate-limit escalation = %+v, %v", e, ok)
	}
	if got := slaiMsgs(t, s, "orch"); len(got) != 1 || !strings.Contains(got[0].Body, "eng-1") {
		t.Fatalf("orch notices = %+v", got)
	}
	// the nudge / stuck / revive paths ignore it, however long it sits
	*now = now.Add(45 * time.Minute) // before the reset
	for i := 0; i < 3; i++ {
		s.driveTick(ctx)
		s.healthTick(ctx)
	}
	if len(slaiMsgs(t, s, "eng-1")) != 0 || hasEvent(s, "stuck_detected") || hasEvent(s, "auto_revive") {
		t.Fatal("acted on a rate-limited agent")
	}
	if len(slaiMsgs(t, s, "orch")) != 1 || countEvents(s, EscRateLimited) != 1 {
		t.Fatal("rate-limit escalation repeated")
	}
	if strings.Contains(pane(t, s, "eng-1"), "rate limit should have reset") {
		t.Fatal("woken before the reset")
	}
	*now = reset.Add(RateLimitGrace + time.Second) // past reset + grace
	s.healthTick(ctx)
	waitPane(t, s, "eng-1", "rate limit should have reset")
	if st := status(t, s, "eng-1"); st != "working" {
		t.Fatalf("status after wake = %s", st)
	}
	// real progress re-arms the escalation
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	*now = now.Add(time.Second)
	_ = s.Store.SetKV(kvLastSeen+"eng-1", itoa64(now.Unix()))
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscRateLimited, "eng-1"); ok {
		t.Fatal("escalation not cleared by progress")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func TestRateLimitedUnknownResetProbes(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	show(t, s, "eng-1", "Claude usage limit reached. Please try again later.")
	_ = s.Store.SetAgentStatus("eng-1", "rate_limited")
	s.healthTick(ctx)
	*now = now.Add(RateLimitProbe - time.Second)
	s.healthTick(ctx)
	if strings.Contains(pane(t, s, "eng-1"), "rate limit should have reset") {
		t.Fatal("probed early")
	}
	*now = now.Add(2 * time.Second)
	s.healthTick(ctx)
	waitPane(t, s, "eng-1", "rate limit should have reset")
}

func TestAllAgentsRateLimitedTellsUserOnce(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	for _, id := range []string{"orch", "eng-1", "eng-2"} {
		_ = s.Store.SetAgentStatus(id, "rate_limited")
	}
	s.healthTick(ctx)
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscRateLimitedAll, "team")
	if !ok || !e.User || countEvents(s, EscRateLimitedAll) != 1 {
		t.Fatalf("all-limited escalation = %+v %v", e, ok)
	}
}

func TestModalStuckEscalatesOnceAndNeverApproves(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	show(t, s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
	_ = s.Store.SetAgentStatus("eng-1", "waiting")
	before := pane(t, s, "eng-1")

	*now = now.Add(ModalPatience - time.Minute)
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscModalStuck, "eng-1"); ok {
		t.Fatal("escalated before ModalPatience")
	}
	*now = now.Add(2 * time.Minute)
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscModalStuck, "eng-1")
	if !ok || !e.User || !strings.Contains(e.Text, "permission prompt") || strings.Contains(e.Text, "Do you want") || strings.Contains(e.Text, "❯") {
		t.Fatalf("modal escalation = %+v %v", e, ok)
	}
	s.healthTick(ctx)
	s.healthTick(ctx)
	if countEvents(s, EscModalStuck) != 1 || len(slaiMsgs(t, s, "orch")) != 1 {
		t.Fatal("modal escalation repeated")
	}
	if pane(t, s, "eng-1") != before {
		t.Fatal("supervisor typed into a permission prompt")
	}
	// a status change clears it and re-arms
	_ = s.Store.SetAgentStatus("eng-1", "working")
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscModalStuck, "eng-1"); ok {
		t.Fatal("not cleared")
	}
	_ = s.Store.SetAgentStatus("eng-1", "waiting")
	*now = now.Add(ModalPatience + time.Second)
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscModalStuck, "eng-1"); !ok {
		t.Fatal("did not re-arm")
	}
}

func TestModalRepeatsEveryTenMinutesAndIgnoresIdleAgents(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	show(t, s, "eng-1", "Do you want to proceed?")
	_ = s.Store.SetAgentStatus("eng-1", "waiting")
	*now = now.Add(ModalPatience + time.Second)
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscModalStuck, "eng-1"); ok {
		t.Fatal("an agent owing nothing and with no unread mail is not worth a user escalation")
	}
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	s.healthTick(ctx)
	if countEvents(s, EscModalStuck) != 1 {
		t.Fatal("first notice missing")
	}
	*now = now.Add(ModalRepeat - time.Minute)
	s.healthTick(ctx)
	if countEvents(s, EscModalStuck) != 1 {
		t.Fatal("repeated too soon")
	}
	*now = now.Add(2 * time.Minute)
	s.healthTick(ctx)
	s.healthTick(ctx)
	notes := 0
	for _, m := range slaiMsgs(t, s, "orch") {
		if strings.Contains(m.Body, "stuck on a prompt") {
			notes++
		}
	}
	if countEvents(s, EscModalStuck) != 2 || notes != 2 {
		t.Fatalf("repeat: %d events, %d orch notes", countEvents(s, EscModalStuck), notes)
	}
}

func TestIgnoredDigestsReachUser(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("orch", "idle")
	for i := 0; i < IgnoredDigests+2; i++ {
		for _, m := range mustUndelivered(t, s, "orch") {
			_ = s.Store.MarkDelivered(m.ID) // delivered, but the orchestrator does nothing
		}
		*now = now.Add(DigestEvery + OrchIdleAfter + time.Second)
		_ = s.Store.SetAgentStatus("orch", "idle")
		*now = now.Add(OrchIdleAfter + time.Second)
		s.driveTick(ctx)
		if i < 2 {
			if _, ok := openEsc(t, s, EscOrchDown, "orch"); ok {
				t.Fatalf("escalated after only %d digests", i+1)
			}
		}
	}
	e, ok := openEsc(t, s, EscOrchDown, "orch")
	if !ok || !e.User || countEvents(s, EscOrchDown) != 1 {
		t.Fatalf("orch_down = %+v %v (%d events)", e, ok, countEvents(s, EscOrchDown))
	}
}

func TestModalWithPendingAskUserIsNotStuck(t *testing.T) {
	s, now := healthEnv(t)
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	show(t, s, "eng-1", "Do you want to proceed?")
	_ = s.Store.AddEvent("eng-1", "ask_user", `{"question":"which db?"}`)
	_ = s.Store.SetAgentStatus("eng-1", "waiting_on_user")
	*now = now.Add(ModalPatience + time.Minute)
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscModalStuck, "eng-1"); ok {
		t.Fatal("a pending ask_user is not a stuck modal")
	}
}

func TestReviveGaveUpOnceAndSurvivesSupervisorRestart(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Tmux.KillSession(ctx, s.Session("eng-1"))
	_ = s.Store.SetAgentStatus("eng-1", "dead")
	var recent []string
	for i := 0; i < ReviveMax; i++ {
		recent = append(recent, itoa64(now.Unix()-int64(i)))
	}
	_ = s.Store.SetKV(kvRevive+"eng-1", strings.Join(recent, ","))

	s.driveTick(ctx)
	s.driveTick(ctx)
	s2 := restarted(s, now)
	s2.driveTick(ctx)
	s2.driveTick(ctx)
	e, ok := openEsc(t, s, EscReviveGaveUp, "eng-1")
	if !ok || !e.User {
		t.Fatalf("escalation = %+v %v", e, ok)
	}
	if countEvents(s, EscReviveGaveUp) != 1 || len(slaiMsgs(t, s, "orch")) != 1 {
		t.Fatalf("gave up %d times, %d orch msgs", countEvents(s, EscReviveGaveUp), len(slaiMsgs(t, s, "orch")))
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); live {
		t.Fatal("revived past the budget")
	}
	// the user starting it again clears the state
	cfg, _ := s.agentCfg("eng-1")
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscReviveGaveUp, "eng-1"); ok {
		t.Fatal("not cleared after restart")
	}
}

func TestRevivedAgentWakeLineSaysRestartedWithUnreadCount(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "a"})
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "b"})
	_ = s.Tmux.KillSession(ctx, s.Session("eng-1"))
	_ = s.Store.SetAgentStatus("eng-1", "dead")
	s.driveTick(ctx)
	if !hasEvent(s, "auto_revive") {
		t.Fatal("not revived")
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Second)
	s.wakeTick(ctx)
	waitPane(t, s, "eng-1", "You were restarted")
	if !strings.Contains(pane(t, s, "eng-1"), "You have 2 new items") {
		t.Fatalf("wake line lacks the unread count:\n%s", pane(t, s, "eng-1"))
	}
	if s.kvGet(kvRestarted+"eng-1") != "" {
		t.Fatal("restarted marker not consumed")
	}
}

func TestStalledReachesUserOnlyIfOrchIgnoresIt(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	cfg, _ := s.agentCfg("eng-1")
	s.escalateStalled(cfg, []store.Task{task})
	e, ok := openEsc(t, s, EscStalled, "eng-1")
	if !ok || e.User {
		t.Fatalf("stalled = %+v %v", e, ok)
	}
	*now = now.Add(time.Hour) // an unread note never starts the clock
	s.healthTick(ctx)
	if e, _ := openEsc(t, s, EscStalled, "eng-1"); e.User {
		t.Fatal("promoted before the orchestrator could have seen the note")
	}
	deliverAll(t, s, "orch")
	seen := time.Now()
	*now = seen.Add(OrchIdleAfter - 2*time.Second) // the clock runs from delivery
	s.healthTick(ctx)
	if e, _ := openEsc(t, s, EscStalled, "eng-1"); e.User {
		t.Fatal("promoted too early")
	}
	*now = seen.Add(OrchIdleAfter + 2*time.Second)
	s.healthTick(ctx)
	s.healthTick(ctx)
	if e, _ := openEsc(t, s, EscStalled, "eng-1"); !e.User {
		t.Fatal("ignored stalled escalation never reached the user")
	}
	if n := countEvents(s, EscStalled); n != 2 { // the orch notice, then the user escalation
		t.Fatalf("stalled events = %d", n)
	}

	// an orchestrator that acted (sent a message) keeps it at orch level
	s2, now2 := healthEnv(t)
	task2, _ := s2.Store.CreateTask(store.Task{Title: "y", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	s2.escalateStalled(cfg, []store.Task{task2})
	deliverAll(t, s2, "orch")
	*now2 = now2.Add(time.Second)
	_ = s2.Store.SetKV(kvLastSeen+"orch", itoa64(now2.Unix()+1))
	*now2 = now2.Add(OrchIdleAfter + time.Minute)
	s2.healthTick(ctx)
	if e, _ := openEsc(t, s2, EscStalled, "eng-1"); e.User {
		t.Fatal("promoted although the orchestrator acted")
	}
}

func deliverAll(t *testing.T, s *Supervisor, to string) {
	t.Helper()
	for _, m := range mustUndelivered(t, s, to) {
		if err := s.Store.MarkDelivered(m.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// wokenMail queues a note to id and marks its wake as typed.
func wokenMail(t *testing.T, s *Supervisor, id string) int64 {
	t.Helper()
	mid, err := s.Store.AddMessage(store.Message{From: "pm", To: id, Kind: "note", Body: "please read"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MarkWoken(mid)
	return mid
}

func TestBusyOrchestratorIsNotPromotedBeforeItReadsTheNote(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	cfg, _ := s.agentCfg("eng-1")
	_ = s.Store.SetAgentStatus("orch", "working")
	s.escalateStalled(cfg, []store.Task{task})
	*now = now.Add(5 * time.Minute) // the orchestrator is mid-turn the whole time
	s.healthTick(ctx)
	if e, _ := openEsc(t, s, EscStalled, "eng-1"); e.User {
		t.Fatal("promoted while the orchestrator was still in its turn")
	}
	deliverAll(t, s, "orch") // it reads the note...
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "what is the status?"})
	*now = time.Now().Add(OrchIdleAfter + 10*time.Second) // ...and replies
	s.healthTick(ctx)
	if e, _ := openEsc(t, s, EscStalled, "eng-1"); e.User {
		t.Fatal("promoted although the orchestrator replied")
	}
}

func TestNeverWokenMailIsNotUnreadStuck(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "queued behind the concurrency cap"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(15 * time.Minute)
	s.healthTick(ctx)
	if hasEvent(s, "unread_nudge") {
		t.Fatal("nudged an agent whose mail was never woken")
	}
	if es, _ := s.Store.OpenEscalations(); len(es) != 0 {
		t.Fatalf("escalated: %+v", es)
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live {
		t.Fatal("restarted a healthy idle agent")
	}
}

func TestNothingIsTypedOverAPermissionPrompt(t *testing.T) {
	oldD, oldP := tmux.EnterDelay, StuckKeyPause
	tmux.EnterDelay, StuckKeyPause = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tmux.EnterDelay, StuckKeyPause = oldD, oldP })
	s, now := healthEnv(t)
	ctx := context.Background()
	// status says idle, but the pane (which hooks can lag) shows a permission prompt
	show(t, s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
	before := pane(t, s, "eng-1")
	wokenMail(t, s, "eng-1")
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.wakeTick(ctx) // the ordinary wake
	*now = now.Add(UnreadAfter + time.Second)
	s.healthTick(ctx) // unread reset-and-retype
	_ = s.Store.SetAgentStatus("eng-1", "rate_limited")
	s.healthTick(ctx)
	*now = now.Add(RateLimitProbe + time.Second)
	s.healthTick(ctx) // rate-limit wake
	time.Sleep(300 * time.Millisecond)
	if got := pane(t, s, "eng-1"); got != before {
		t.Fatalf("keys were sent over the prompt:\n%s", got)
	}
	if countEvents(s, "type_skipped_modal") != 1 {
		t.Fatalf("type_skipped_modal events = %d, want exactly 1", countEvents(s, "type_skipped_modal"))
	}
}

func TestUnreadStuckRetypesThenRestartsWithOneNotice(t *testing.T) {
	oldD, oldP := tmux.EnterDelay, StuckKeyPause
	tmux.EnterDelay, StuckKeyPause = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tmux.EnterDelay, StuckKeyPause = oldD, oldP })
	s, now := healthEnv(t)
	ctx := context.Background()
	wokenMail(t, s, "eng-1")
	_ = s.Store.SetAgentStatus("eng-1", "idle")

	s.healthTick(ctx)
	if hasEvent(s, "unread_nudge") {
		t.Fatal("acted on fresh mail")
	}
	*now = now.Add(UnreadAfter + time.Second)
	s.healthTick(ctx)
	if countEvents(s, "unread_nudge") != 1 {
		t.Fatal("no reset-and-retype")
	}
	waitPane(t, s, "eng-1", "You have 1 new items")
	s.healthTick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live {
		t.Fatal("restarted too soon")
	}
	*now = now.Add(UnreadRestartAfter + time.Second)
	s.healthTick(ctx)
	s.healthTick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); live {
		t.Fatal("still alive after the restart step")
	}
	if _, ok := openEsc(t, s, EscUnreadStuck, "eng-1"); !ok || countEvents(s, EscUnreadStuck) != 1 || len(slaiMsgs(t, s, "orch")) != 1 {
		t.Fatalf("expected exactly one unread_stuck notice, got %d events %d msgs", countEvents(s, EscUnreadStuck), len(slaiMsgs(t, s, "orch")))
	}
}

func TestOrchestratorDownIsRestartedWithBriefAndUserIsTold(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "build it", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Tmux.KillSession(ctx, s.Session("orch"))
	_ = s.Store.SetAgentStatus("orch", "dead")

	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscOrchDown, "orch"); ok {
		t.Fatal("fired before OrchSilentAfter")
	}
	*now = now.Add(OrchSilentAfter + time.Second)
	s.healthTick(ctx)
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscOrchDown, "orch")
	if !ok || !e.User || countEvents(s, EscOrchDown) != 1 {
		t.Fatalf("orch_down = %+v %v", e, ok)
	}
	brief := slaiMsgs(t, s, "orch")
	if len(brief) != 1 || !strings.Contains(brief[0].Body, "Resume brief") || !strings.Contains(brief[0].Body, "build it") {
		t.Fatalf("resume brief = %+v", brief)
	}
	s.driveTick(ctx) // autoRevive now has unread mail to justify the restart
	if live, _ := s.Tmux.HasSession(ctx, s.Session("orch")); !live {
		t.Fatal("orchestrator not restarted")
	}
}

func TestSilentOrchestratorIsRestarted(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("orch", "idle")
	wokenMail(t, s, "orch") // woken, never read
	_ = s.Store.SetKV(kvBoardAt, signature([]store.Task{task})+"|"+itoa64(now.Unix()))
	*now = now.Add(OrchSilentAfter + time.Minute) // fake orch never responds
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscOrchDown, "orch"); !ok {
		t.Fatal("silent orchestrator not flagged")
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("orch")); live {
		t.Fatal("silent orchestrator not restarted")
	}
	if len(pendingFrom(t, s, "orch", "slai")) != 1 {
		t.Fatal("expected the resume brief")
	}
}

func TestQuietOrchestratorWithBusyEngineerIsHealthy(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("orch", "idle")
	_ = s.Store.SetAgentStatus("eng-1", "working")
	wokenMail(t, s, "orch")
	_ = s.Store.SetKV(kvBoardAt, signature([]store.Task{task})+"|"+itoa64(now.Unix()))
	*now = now.Add(15 * time.Minute)
	_ = s.Store.SetKV(kvLastSeen+"eng-1", itoa64(now.Unix())) // the engineer is alive
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscOrchDown, "orch"); ok {
		t.Fatal("restarted an idle orchestrator whose engineer is mid-task")
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("orch")); !live {
		t.Fatal("killed a healthy orchestrator")
	}
	// a stale stored board signature means the board changed since: not silent either
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetKV(kvBoardAt, "T-9999:assigned:x|1")
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscOrchDown, "orch"); ok {
		t.Fatal("a changed board signature must read as board movement")
	}
}

func TestStuckGiveUpAlsoTellsUserOnce(t *testing.T) {
	s, _ := healthEnv(t)
	as, _ := s.Store.GetAgents()
	var a store.Agent
	for _, x := range as {
		if x.ID == "eng-1" {
			a = x
		}
	}
	cfg, _ := s.agentCfg("eng-1")
	s.stuckGiveUp(a, cfg, 0, "kill failed")
	s.stuckGiveUp(a, cfg, 0, "kill failed")
	e, ok := openEsc(t, s, EscStuckGaveUp, "eng-1")
	if !ok || !e.User || countEvents(s, EscStuckGaveUp) != 1 || len(slaiMsgs(t, s, "orch")) != 1 {
		t.Fatalf("stuck_gave_up = %+v %v", e, ok)
	}
	ocfg, _ := s.agentCfg("orch")
	s.stuckGiveUp(store.Agent{ID: "orch"}, ocfg, 0, "kill failed")
	if _, ok := openEsc(t, s, EscStuckGaveUp, "orch"); !ok {
		t.Fatal("orchestrator give-up must still reach the user")
	}
}

// AC 16: none of the new paths act on a paused, stopped or TUI-focused agent.
func TestHealthPathsSkipHeldAgents(t *testing.T) {
	holds := map[string]func(s *Supervisor, id string){
		"paused":  func(s *Supervisor, id string) { _ = s.Store.SetAgentPaused(id, true) },
		"stopped": func(s *Supervisor, id string) { _ = s.Store.SetKV(kvStopped+id, "1") },
		"focused": func(s *Supervisor, id string) { _ = s.Store.TouchFocus(id, s.now()) },
	}
	setups := map[string]func(t *testing.T, s *Supervisor, id string){
		"rate_limit": func(t *testing.T, s *Supervisor, id string) {
			show(t, s, id, "Claude usage limit reached")
			_ = s.Store.SetAgentStatus(id, "rate_limited")
		},
		"modal": func(t *testing.T, s *Supervisor, id string) {
			_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: id, CreatedBy: "orch", Status: store.StatusInProgress})
			show(t, s, id, "Do you want to proceed?")
			_ = s.Store.SetAgentStatus(id, "waiting")
		},
		"unread": func(t *testing.T, s *Supervisor, id string) {
			wokenMail(t, s, id)
			_ = s.Store.SetAgentStatus(id, "idle")
		},
		"orch_silent": func(t *testing.T, s *Supervisor, id string) {
			task, _ := s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
			_ = s.Store.SetAgentStatus(id, "idle")
			wokenMail(t, s, id)
			_ = s.Store.SetKV(kvBoardAt, signature([]store.Task{task})+"|1")
		},
	}
	for hn, hold := range holds {
		for sn, setup := range setups {
			t.Run(hn+"/"+sn, func(t *testing.T) {
				s, now := healthEnv(t)
				id := "eng-1"
				if sn == "orch_silent" {
					id = "orch"
				}
				setup(t, s, id)
				hold(s, id)
				*now = now.Add(time.Hour)
				for i := 0; i < 3; i++ {
					hold(s, id)
					s.healthTick(context.Background())
					*now = now.Add(2 * time.Minute)
				}
				if es, _ := s.Store.OpenEscalations(); len(es) != 0 {
					t.Fatalf("escalated for a held agent: %+v", es)
				}
				for _, k := range []string{"unread_nudge", "rate_limit_wake"} {
					if hasEvent(s, k) {
						t.Fatalf("acted: %s", k)
					}
				}
				if live, _ := s.Tmux.HasSession(context.Background(), s.Session(id)); !live {
					t.Fatal("killed a held agent")
				}
			})
		}
	}
}

func TestHeartbeatRefreshedBetweenSteps(t *testing.T) {
	s := unitEnv(t, team(5))
	now := time.Unix(1_000_000, 0)
	s.Now = func() time.Time { return now }
	var seen []int64
	s.runSteps([]step{
		{"a", func() { now = now.Add(5 * time.Second) }},
		{"b", func() {
			at, _, _ := s.Store.SupervisorHeartbeat()
			seen = append(seen, at)
			now = now.Add(5 * time.Second)
		}},
		{"c", func() { at, _, _ := s.Store.SupervisorHeartbeat(); seen = append(seen, at) }},
	})
	if len(seen) != 2 || seen[0] != 1_000_005 || seen[1] != 1_000_010 {
		t.Fatalf("heartbeat before steps b and c = %v", seen)
	}
}

func TestOwedRepliesEnterTheReminderLadder(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	ask, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ask", Body: "which db do we use?"})
	ping, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ping", Body: "ping"})
	for _, m := range mustUndelivered(t, s, "eng-1") {
		_ = s.Store.MarkDelivered(m.ID) // claimed, never answered
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")

	*now = now.Add(OwedReplyAfter - time.Minute)
	s.driveTick(ctx)
	if len(slaiMsgs(t, s, "eng-1")) != 0 {
		t.Fatal("reminded before the reply was 5 minutes overdue")
	}
	*now = now.Add(2*time.Minute + NudgeIdleAfter)
	s.driveTick(ctx)
	got := slaiMsgs(t, s, "eng-1")
	if len(got) != 1 || !strings.Contains(got[0].Body, "reply(message_id") || !strings.Contains(got[0].Body, "which db") || !strings.Contains(got[0].Body, "pong") {
		t.Fatalf("expected one reminder naming both owed replies, got %+v", got)
	}
	// answering both clears the obligation
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "reply", ReplyTo: ask, Body: "sqlite"})
	_ = s.Store.SetKV("pong."+strconv.FormatInt(ping, 10), `{"at":1}`)
	deliverAll(t, s, "eng-1")
	*now = now.Add(NudgeEvery + time.Second)
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(NudgeIdleAfter + time.Second)
	s.driveTick(ctx)
	if len(slaiMsgs(t, s, "eng-1")) != 0 {
		t.Fatal("reminded although both were answered")
	}
}

func TestPingBypassesConcurrencyCap(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	s.Team.Defaults.MaxConcurrentAgents = 1
	_ = s.Store.SetAgentStatus("eng-2", "working")
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"})
	s.wakeTick(ctx)
	if hasEvent(s, "wake") {
		t.Fatal("ordinary wake ignored the concurrency cap")
	}
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ping", Body: "ping"})
	s.wakeTick(ctx)
	if !hasEvent(s, "wake") {
		t.Fatal("a ping must be delivered despite the cap")
	}
}

func TestMCPDeadRestartsInsteadOfOnlyNotifying(t *testing.T) {
	for _, dead := range []bool{true, false} {
		s, now := healthEnv(t)
		ctx := context.Background()
		_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "read me"})
		_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}") // hooks are firing
		first := now.Add(-GiveUpAfter - time.Minute).Unix()
		if !dead {
			_ = s.Store.SetKV(kvLastSeen+"eng-1", strconv.FormatInt(now.Unix(), 10)) // it did call slai tools
		}
		msgs := mustUndelivered(t, s, "eng-1")
		s.saveWake("eng-1", wakeState{id: msgs[0].ID, n: MaxUnansweredWakes, firstAt: first, lastAt: first})
		_ = s.Store.SetAgentStatus("eng-1", "idle")
		s.wakeTick(ctx)
		live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1"))
		if dead && (live || !hasEvent(s, "mcp_dead")) {
			t.Fatalf("dead MCP not restarted (live=%v)", live)
		}
		if !dead && (!live || hasEvent(s, "mcp_dead") || !hasEvent(s, "wake_gave_up")) {
			t.Fatalf("agent that does call tools was restarted (live=%v)", live)
		}
	}
}

func TestStuckNudgeSkippedWhileToolCallInFlight(t *testing.T) {
	s, now := stuckEnv(t)
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}") // a long build started, no Stop since
	stuckTicks(s, now)
	if hasEvent(s, "stuck_detected") {
		t.Fatal("nudged an agent inside a tool call")
	}
	_ = s.Store.AddEvent("eng-1", "hook:stop", "{}") // the turn ended
	*now = now.Add(StuckAfter + time.Second)
	stuckTicks(s, now)
	if !hasEvent(s, "stuck_detected") {
		t.Fatal("not nudged once the tool call was over")
	}
}

// fgEnv is healthEnv with a focus-group persona on the team.
func fgEnv(t *testing.T) (*Supervisor, *time.Time) {
	t.Helper()
	tm := team(5)
	tm.Agents = append(tm.Agents, config.Agent{ID: "fg-newbie", Role: config.RoleFocusGroup, Model: "sonnet"})
	s := tmuxEnv(t, tm)
	s.Driver = true
	now := time.Now()
	s.Now = func() time.Time { return now }
	return s, &now
}

func TestUnansweredAskToPersonaGetsReplyReminder(t *testing.T) {
	s, now := fgEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "fg-newbie", Kind: "ask", Body: "is the setup flow clear?"})
	deliverAll(t, s, "fg-newbie")
	_ = s.Store.SetAgentStatus("fg-newbie", "idle")
	*now = now.Add(OwedReplyAfter + NudgeIdleAfter + time.Second)
	s.driveTick(ctx)
	got := slaiMsgs(t, s, "fg-newbie")
	if len(got) != 1 || !strings.Contains(got[0].Body, "reply(message_id") || !strings.Contains(got[0].Body, "setup flow") {
		t.Fatalf("persona reminder = %+v", got)
	}
}

func TestUnansweredAskToOrchestratorEscalatesToUserNotToItself(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "ask", Body: "which db do we use?"})
	deliverAll(t, s, "orch")
	*now = now.Add(OwedReplyAfter + time.Second)
	for i := 0; i <= MaxNudges+1; i++ {
		deliverAll(t, s, "orch")
		_ = s.Store.SetAgentStatus("orch", "idle")
		*now = now.Add(NudgeEvery + NudgeIdleAfter + time.Second)
		s.driveTick(ctx)
		if i == 0 {
			reminders := 0
			for _, m := range slaiMsgs(t, s, "orch") {
				if strings.Contains(m.Body, "which db") && strings.Contains(m.Body, "reply(message_id") {
					reminders++
				}
			}
			if reminders != 1 {
				t.Fatalf("orchestrator not reminded of its unanswered ask (%d)", reminders)
			}
		}
	}
	e, ok := openEsc(t, s, EscStalled, "orch")
	if !ok || !e.User {
		t.Fatalf("stalled escalation for orch = %+v %v, want user-level", e, ok)
	}
	for _, m := range slaiMsgs(t, s, "orch") {
		if strings.Contains(m.Body, "has not made progress") {
			t.Fatal("told the orchestrator about itself")
		}
	}
}

func TestWakeThatNeverLandsIsRevertedAndRetypedOnce(t *testing.T) {
	oldD, oldP := tmux.EnterDelay, StuckKeyPause
	tmux.EnterDelay, StuckKeyPause = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tmux.EnterDelay, StuckKeyPause = oldD, oldP })
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "go"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.wakeTick(ctx)
	if st := status(t, s, "eng-1"); st != "working" {
		t.Fatalf("optimistic status = %s", st)
	}
	*now = now.Add(WakeLandWindow - time.Second)
	s.healthTick(ctx)
	if hasEvent(s, "wake_retyped") || status(t, s, "eng-1") != "working" {
		t.Fatal("judged the wake before the window passed")
	}
	*now = now.Add(2 * time.Second)
	s.healthTick(ctx) // no UserPromptSubmit: revert the status and retype once
	if !hasEvent(s, "wake_retyped") {
		t.Fatal("no clear-and-retype")
	}
	waitPane(t, s, "eng-1", "echo: [slai] You have 1 new items")
	*now = now.Add(WakeLandWindow + time.Second)
	s.healthTick(ctx) // still nothing: give up on this wake, leave the rest to the wake loop
	if countEvents(s, "wake_retyped") != 1 || !hasEvent(s, "wake_not_landed") {
		t.Fatalf("expected exactly one retype then wake_not_landed")
	}
	if st := status(t, s, "eng-1"); st != "idle" {
		t.Fatalf("status after a wake that never landed = %s, want idle", st)
	}
}

func TestWakeThatLandsIsLeftAlone(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "go"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	s.wakeTick(ctx)
	_ = s.Store.AddEvent("eng-1", "hook:userpromptsubmit", "{}")
	*now = now.Add(WakeLandWindow * 3)
	s.healthTick(ctx)
	if hasEvent(s, "wake_retyped") || hasEvent(s, "wake_not_landed") || status(t, s, "eng-1") != "working" {
		t.Fatal("a wake that landed was second-guessed")
	}
	if s.kvGet(kvLanding+"eng-1") != "" {
		t.Fatal("landing record not cleared")
	}
}

func TestHooksFailingWhenPaneChangesButNoHooks(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(HooksSilentAfter + time.Minute) // five-plus minutes with no hook event
	_ = hooks.RecordErr(s.Store, hooks.KVHookErr+"eng-1", "settings.json not readable", *now)
	showLines(t, s, "eng-1", "✻ Thinking… (12s · esc to interrupt)") // Claude is visibly running
	for i := 0; i <= hooksPaneChanges; i++ {
		s.healthTick(ctx)
		show(t, s, "eng-1", "output line "+strconv.Itoa(i))
	}
	s.healthTick(ctx)
	s.healthTick(ctx)
	e, ok := openEsc(t, s, EscHooksFailing, "eng-1")
	if !ok || e.User || !strings.Contains(e.Text, "settings.json") || countEvents(s, EscHooksFailing) != 1 {
		t.Fatalf("hooks_failing = %+v %v (%d events)", e, ok, countEvents(s, EscHooksFailing))
	}
	if len(slaiMsgs(t, s, "orch")) != 1 {
		t.Fatal("expected one orch note")
	}
	// a hook arriving clears it
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	*now = time.Now()
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("not cleared when hooks resumed")
	}
}

func TestIdlePaneWithMovingStatuslineIsNotHooksFailing(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Hour)
	show(t, s, "eng-1", "? for shortcuts")
	for i := 0; i < 2*hooksPaneChanges; i++ {
		s.healthTick(ctx)
		show(t, s, "eng-1", "5h "+strconv.Itoa(10+i)+"% wk 3% $ 4.2") // other agents' usage moves the statusline
	}
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("an idle prompt whose statusline moves is not a hooks failure")
	}
}

func TestHooksNotFailingWhenPaneIsStatic(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(time.Hour)
	for i := 0; i < 6; i++ {
		s.healthTick(ctx)
	}
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("an idle agent with a static pane is not a hooks failure")
	}
}

// A slai call that was interrupted before it reached the server leaves a pending
// stamp; an agent that kept working (later hook events) must not be restarted.
func TestMCPPendingIgnoredWhenAgentMovedOn(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "working")
	_ = s.Store.SetKV(hooks.KVMCPPending+"eng-1", strconv.FormatInt(now.Unix()-5, 10))
	_ = s.Store.AddEvent("eng-1", "hook:posttooluse", "{}") // it carried on after the call
	*now = now.Add(MCPStampWindow + time.Minute)
	s.healthTick(ctx)
	if hasEvent(s, "mcp_restart") {
		t.Fatal("restarted an agent that kept working")
	}
	if _, ok, _ := s.Store.GetKV(hooks.KVMCPPending + "eng-1"); ok {
		t.Fatal("pending call not forgotten")
	}
	if _, ok := hooks.LoadErr(s.Store, hooks.KVMCPErr+"eng-1"); ok {
		t.Fatal("recorded an mcp error for an agent that moved on")
	}
}

func TestMCPCrashRestartsOnceThenEscalates(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	pend := func() {
		_ = s.Store.SetKV(hooks.KVMCPPending+"eng-1", strconv.FormatInt(now.Unix(), 10))
	}
	pend()
	*now = now.Add(MCPStampWindow - time.Second)
	s.healthTick(ctx)
	if hasEvent(s, "mcp_restart") {
		t.Fatal("judged before the window")
	}
	*now = now.Add(2 * time.Second)
	s.healthTick(ctx)
	if !hasEvent(s, "mcp_restart") {
		t.Fatal("no mcp_restart event")
	}
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); live {
		t.Fatal("session not restarted")
	}
	if _, ok := openEsc(t, s, EscMCPCrash, "eng-1"); ok {
		t.Fatal("escalated on the first failure")
	}
	// it comes back, fails the same way again inside the window: now the user hears
	cfg, _ := s.agentCfg("eng-1")
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	*now = now.Add(5 * time.Minute)
	pend()
	*now = now.Add(MCPStampWindow + time.Second)
	s.healthTick(ctx)
	s.healthTick(ctx)
	if e, ok := openEsc(t, s, EscMCPCrash, "eng-1"); !ok || !e.User || countEvents(s, EscMCPCrash) != 1 {
		t.Fatalf("repeat MCP failure: %+v %v", e, ok)
	}
	if r, ok := hooks.LoadErr(s.Store, hooks.KVMCPErr+"eng-1"); !ok || r.Count != 2 || r.At == 0 || r.Last == "" {
		t.Fatalf("mcp.err record = %+v %v, want cumulative count 2", r, ok)
	}
	// a call that reaches the service clears the pending stamp and the consecutive-failure count
	pend()
	*now = now.Add(time.Second)
	_ = s.Store.SetKV(kvLastSeen+"eng-1", strconv.FormatInt(now.Unix(), 10))
	s.healthTick(ctx)
	if s.kvGet(hooks.KVMCPPending+"eng-1") != "" || s.kvGet(kvMCPFail+"eng-1") != "" {
		t.Fatal("successful call did not clear MCP state")
	}
}

func TestMCPCheckSkipsPendingPermissionPrompts(t *testing.T) {
	for _, st := range []string{"waiting", "waiting_on_user"} {
		s, now := healthEnv(t)
		ctx := context.Background()
		_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
		_ = s.Store.SetAgentStatus("eng-1", st) // the tool-permission prompt is still open
		_ = s.Store.SetKV(hooks.KVMCPPending+"eng-1", strconv.FormatInt(now.Unix(), 10))
		*now = now.Add(MCPStampWindow * 5)
		s.healthTick(ctx)
		if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live || hasEvent(s, "mcp_restart") {
			t.Fatalf("restarted an agent parked on a prompt (status %s)", st)
		}
	}
	// same for a pane that shows a modal while hooks still say idle
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	show(t, s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	_ = s.Store.SetKV(hooks.KVMCPPending+"eng-1", strconv.FormatInt(now.Unix(), 10))
	*now = now.Add(MCPStampWindow * 5)
	s.healthTick(ctx)
	if live, _ := s.Tmux.HasSession(ctx, s.Session("eng-1")); !live || hasEvent(s, "mcp_restart") {
		t.Fatal("restarted an agent with a permission prompt on screen")
	}
}

func TestDigestCeilingStopsUntilBoardChanges(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	_ = s.Store.SetAgentStatus("orch", "idle")
	round := func() {
		deliverAll(t, s, "orch")
		*now = now.Add(DigestEvery + OrchIdleAfter + time.Second)
		_ = s.Store.SetAgentStatus("orch", "idle")
		*now = now.Add(OrchIdleAfter + time.Second)
		s.driveTick(ctx)
	}
	for i := 0; i < IgnoredDigests+2; i++ {
		round()
	}
	if _, ok := openEsc(t, s, EscOrchDown, "orch"); !ok {
		t.Fatal("ceiling never reached")
	}
	digests := countEvents(s, "digest")
	for i := 0; i < 4; i++ {
		round()
	}
	if countEvents(s, "digest") != digests {
		t.Fatalf("digests kept coming after the ceiling: %d -> %d", digests, countEvents(s, "digest"))
	}
	_, _ = s.Store.CreateTask(store.Task{Title: "new", Assignee: "eng-2", CreatedBy: "orch", Status: store.StatusInProgress})
	round()
	round()
	if countEvents(s, "digest") <= digests {
		t.Fatal("digests did not resume after the board changed")
	}
}

func saveSnapshot(t *testing.T, s *Supervisor, at time.Time, fiveHour, sevenDay float64, resets time.Time) {
	t.Helper()
	snap := ratelimit.Snapshot{At: at.Unix(),
		FiveHour: &ratelimit.Window{UsedPercent: fiveHour, ResetsAt: resets.Unix()},
		SevenDay: &ratelimit.Window{UsedPercent: sevenDay, ResetsAt: resets.Add(48 * time.Hour).Unix()}}
	if err := ratelimit.Save(s.Store, snap); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitSnapshotIsASecondSignal(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_ = s.Store.SetAgentStatus("eng-1", "idle") // its pane shows no limit text at all
	reset := now.Add(time.Hour)

	saveSnapshot(t, s, *now, 99.9, 10, reset) // under 100%: nothing
	s.healthTick(ctx)
	if st := status(t, s, "eng-1"); st != "idle" {
		t.Fatalf("status at 99.9%% = %s", st)
	}
	saveSnapshot(t, s, now.Add(-SnapshotStale-time.Minute), 100, 10, reset) // stale: unknown
	s.healthTick(ctx)
	if st := status(t, s, "eng-1"); st != "idle" {
		t.Fatalf("a stale snapshot set %s", st)
	}
	ratelimit.Save(s.Store, ratelimit.Snapshot{At: now.Unix()}) // nothing known
	s.healthTick(ctx)
	if st := status(t, s, "eng-1"); st != "idle" {
		t.Fatalf("an empty snapshot set %s", st)
	}
	saveSnapshot(t, s, *now, 100, 10, now.Add(-time.Minute)) // already reset
	s.healthTick(ctx)
	if st := status(t, s, "eng-1"); st != "idle" {
		t.Fatalf("an expired window set %s", st)
	}

	saveSnapshot(t, s, *now, 100, 10, reset) // capped until reset
	s.healthTick(ctx)
	if st := status(t, s, "eng-1"); st != "rate_limited" {
		t.Fatalf("status with the account capped = %s", st)
	}
	e, ok := openEsc(t, s, EscRateLimited, "eng-1")
	if !ok || !strings.Contains(e.Text, reset.Format("15:04")) {
		t.Fatalf("escalation = %+v %v", e, ok)
	}
	// the pane fallback must not flip it back to idle while the snapshot says capped
	_ = s.Store.SetAgentStatus("eng-1", "rate_limited")
	*now = now.Add(FallbackStale + time.Second)
	saveSnapshot(t, s, *now, 100, 10, reset)
	s.statusTick(ctx)
	if st := status(t, s, "eng-1"); st != "rate_limited" {
		t.Fatalf("pane fallback overrode the snapshot: %s", st)
	}
	// still capped shortly before the reset: no wake
	*now = reset.Add(-time.Minute)
	saveSnapshot(t, s, *now, 100, 10, reset)
	s.healthTick(ctx)
	if strings.Contains(pane(t, s, "eng-1"), "rate limit should have reset") {
		t.Fatal("woken before the reset")
	}
	// past the reset (+grace) the snapshot is stale/expired: woken
	*now = reset.Add(RateLimitGrace + time.Second)
	s.healthTick(ctx)
	waitPane(t, s, "eng-1", "rate limit should have reset")
}

// The modal_stuck note names the prompt without repeating it, so showing the note
// in the orchestrator's own pane does not make that pane look like a prompt (which
// used to silence the one agent that coordinates recovery).
func TestModalNoteDoesNotPoisonOrchestratorPane(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	show(t, s, "eng-1", "Bash command", "rm -rf build", "Do you want to proceed?", "❯ 1. Yes", "2. No")
	_ = s.Store.SetAgentStatus("eng-1", "waiting")
	*now = now.Add(ModalPatience + time.Second)
	s.healthTick(ctx)
	notes := slaiMsgs(t, s, "orch")
	if len(notes) != 1 {
		t.Fatalf("expected one orch note, got %d", len(notes))
	}
	for _, bad := range []string{"Do you want", "❯", "1. Yes", "2. No"} {
		if strings.Contains(notes[0].Body, bad) || strings.Contains(openEscText(t, s), bad) {
			t.Fatalf("note or escalation repeats %q:\n%s", bad, notes[0].Body)
		}
	}
	// the orchestrator reads it: its pane now shows the note's text
	show(t, s, "orch", notes[0].Body)
	if ex := hooks.ModalExcerpt(pane(t, s, "orch")); ex != "" {
		t.Fatalf("orch pane reads as a modal: %q", ex)
	}
	_ = s.Store.SetAgentStatus("orch", "idle")
	if !s.safeToType(ctx, mustAgent(t, s, "orch")) || hasEvent(s, "type_skipped_modal") {
		t.Fatal("typing to the orchestrator was withheld because of the note it was sent")
	}
	*now = now.Add(time.Hour)
	s.healthTick(ctx)
	if _, ok := openEsc(t, s, EscModalStuck, "orch"); ok {
		t.Fatal("modal_stuck was raised for the orchestrator itself")
	}
}

func openEscText(t *testing.T, s *Supervisor) string {
	t.Helper()
	es, _ := s.Store.OpenEscalations()
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Text + "\n")
	}
	return b.String()
}

func mustAgent(t *testing.T, s *Supervisor, id string) store.Agent {
	t.Helper()
	a, err := s.Store.AgentByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// An idle pane whose old scrollback quotes a full prompt (raw question, selector
// and options) still accepts a wake: only a live prompt blocks typing.
func TestIdlePaneWithQuotedPromptInScrollbackStillAcceptsWake(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	line := func(text string) {
		if err := s.Tmux.SendKeys(ctx, s.Session("eng-1"), text, true); err != nil {
			t.Fatal(err)
		}
	}
	line("earlier someone pasted: Do you want to proceed? ❯ 1. Yes 2. No")
	for i := 0; i < 20; i++ { // ordinary output since, pushing the quote out of the live region
		line("later output " + strconv.Itoa(i))
	}
	waitPane(t, s, "eng-1", "later output 19")
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "go"})
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	if !s.safeToType(ctx, mustAgent(t, s, "eng-1")) {
		t.Fatal("a quoted prompt in old scrollback blocked typing")
	}
	s.wakeTick(ctx)
	if !hasEvent(s, "wake") || hasEvent(s, "type_skipped_modal") {
		t.Fatal("the wake was withheld because of a prompt quoted in scrollback")
	}
}

// A principal without a reply tool answered asks with send_message (live bug,
// T-0034): that note to the asker, sent after the ask, must clear the obligation.
func TestSendMessageToTheAskerClearsAnOwedReply(t *testing.T) {
	s, now := healthEnv(t)
	cfg, _ := s.agentCfg("eng-1")
	early, _ := s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "note", Body: "sent before the ask"})
	_ = early
	ask, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ask", Body: "which db do we use?"})
	*now = now.Add(OwedReplyAfter + time.Minute)
	if got := s.owedMessages(cfg); len(got) != 1 || got[0].ID != "msg-"+strconv.FormatInt(ask, 10) {
		t.Fatalf("an unanswered ask must be owed, got %+v", got)
	}
	// notes that are not an answer: to someone else, to "all", or an ask back
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "eng-2", Kind: "note", Body: "unrelated"})
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "all", Kind: "note", Body: "broadcast"})
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "ask", Body: "counter-question"})
	if got := s.owedMessages(cfg); len(got) != 1 {
		t.Fatalf("only a direct answer to the asker clears it, got %+v", got)
	}
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "note", Body: "sqlite"})
	if got := s.owedMessages(cfg); len(got) != 0 {
		t.Fatalf("send_message to the asker after the ask must clear it, got %+v", got)
	}
	// a pong-less ping is not cleared by a note
	ping, _ := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ping", Body: "ping"})
	*now = now.Add(OwedReplyAfter + time.Minute)
	_, _ = s.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "note", Body: "hi"})
	if got := s.owedMessages(cfg); len(got) != 1 || got[0].ID != "msg-"+strconv.FormatInt(ping, 10) {
		t.Fatalf("a ping needs a pong, got %+v", got)
	}
}

func TestOwedReplySurvivesUnrelatedMessageFlood(t *testing.T) {
	s, now := healthEnv(t)
	cfg, _ := s.agentCfg("eng-1")
	ask, err := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ask", Body: "still waiting?"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 301; i++ {
		if _, err := s.Store.AddMessage(store.Message{From: "slai", To: "pm", Kind: "note", Body: "unrelated"}); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(OwedReplyAfter + time.Minute)
	if got := s.owedMessages(cfg); len(got) != 1 || got[0].ID != "msg-"+strconv.FormatInt(ask, 10) {
		t.Fatalf("old unanswered ask lost behind unrelated traffic: %+v", got)
	}
}

// Every reminder names only tools its recipient has.
func TestReminderNamesOnlyToolsTheRoleHas(t *testing.T) {
	var union []string
	seen := map[string]bool{}
	for _, role := range roles.AllRoles {
		for _, n := range roles.ToolNames(role) {
			if !seen[n] {
				seen[n] = true
				union = append(union, n)
			}
		}
	}
	for _, role := range roles.AllRoles {
		own := map[string]bool{}
		for _, n := range roles.ToolNames(role) {
			own[n] = true
		}
		for _, onlyMsgs := range []bool{true, false} {
			text := reminderAsk(role, onlyMsgs)
			for _, tool := range union {
				if own[tool] {
					continue
				}
				if regexp.MustCompile(`\b` + tool + `\(|\bcall ` + tool + `\b|\b` + tool + `, `).MatchString(text) {
					t.Errorf("%s reminder (onlyMsgs=%v) names %s, which the role lacks: %q", role, onlyMsgs, tool, text)
				}
			}
		}
	}
}

// seedHookWatch makes the next healthTick the one that decides: the watch has seen
// hooksPaneChanges-1 changes and last hashed prev (as hooksAgent hashes a pane).
func seedHookWatch(s *Supervisor, now *time.Time, prev string) {
	_ = s.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(HooksSilentAfter + time.Minute)
	sum := sha1.Sum([]byte(hooks.StablePane(prev)))
	s.rt.mu.Lock()
	s.rt.hookPanes = map[string]hookPane{"eng-1": {hash: hex.EncodeToString(sum[:]), changes: hooksPaneChanges - 1}}
	s.rt.mu.Unlock()
}

// AC1: only the spinner's timer, tokens and glyph differ from the last poll.
func TestHooksNotFailingWhenOnlySpinnerTicks(t *testing.T) {
	s, now := healthEnv(t)
	show(t, s, "eng-1", "✻ Deciphering… (3s · ↓ 5.1k tokens)")
	seedHookWatch(s, now, strings.ReplaceAll(pane(t, s, "eng-1"), "✻ Deciphering… (3s · ↓ 5.1k", "· Deciphering… (2s · ↓ 4.9k"))
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("spinner timer ticks are not pane changes")
	}
}

// AC2: the ctrl+b hint means a tool is in flight; the pane changing is not a hooks failure.
func TestHooksNotFailingWhileToolRunsWithBackgroundHint(t *testing.T) {
	s, now := healthEnv(t)
	show(t, s, "eng-1", "(ctrl+b ctrl+b (twice) to run in background)")
	seedHookWatch(s, now, "something else entirely")
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("a tool in flight fires no hooks")
	}
}

// AC3: new tool-call lines with no hook event still escalate, once.
func TestHooksFailingWhenNewToolLinesAppearBesideSpinner(t *testing.T) {
	s, now := healthEnv(t)
	s.Detect = func(string) (string, bool) { return "working", true }
	_ = s.Store.SetAgentStatus("eng-1", "working")
	*now = now.Add(HooksSilentAfter + time.Minute)
	for i := 0; i <= hooksPaneChanges; i++ {
		showLines(t, s, "eng-1", "● Read(f"+strconv.Itoa(i)+".go)", "✻ Deciphering… ("+strconv.Itoa(i)+"s · ↓ 5 tokens)")
		s.healthTick(context.Background())
	}
	s.healthTick(context.Background())
	if countEvents(s, EscHooksFailing) != 1 {
		t.Fatalf("hooks_failing events = %d, want 1", countEvents(s, EscHooksFailing))
	}
}

// T-0109 AC1: while subagents run (`● N agents working · /team`) the parent fires no hooks and
// its pane keeps changing; that is not a hooks failure.
func TestHooksNotFailingWhileSubagentsRun(t *testing.T) {
	s, now := healthEnv(t)
	s.Detect = func(string) (string, bool) { return "working", true }
	show(t, s, "eng-1", "● 2 agents working · /team")
	seedHookWatch(s, now, "something else entirely")
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); ok {
		t.Fatal("subagents in flight fire no hooks on the parent")
	}
	if hp, ok := s.rt.hookPanes["eng-1"]; ok {
		t.Fatalf("the watch was not reset: %+v", hp)
	}
}

// T-0109 AC2: the team footer `← 12 agents` is not subagents; the same pane change escalates.
func TestTeamFooterIsNotSubagents(t *testing.T) {
	s, now := healthEnv(t)
	s.Detect = func(string) (string, bool) { return "working", true }
	show(t, s, "eng-1", "  ⏵⏵ auto mode on (shift+tab to cycle) · ← 12 agents")
	seedHookWatch(s, now, "something else entirely")
	s.healthTick(context.Background())
	if _, ok := openEsc(t, s, EscHooksFailing, "eng-1"); !ok {
		t.Fatal("a team footer must not silence hooks_failing")
	}
}
