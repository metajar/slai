package supervisor

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Team health: detection, bounded recovery and escalate-once for the failure
// modes the plain wake/nudge/stuck logic cannot see (spec team-health.md).
//
// Every escalation goes through escalate: it opens a store.Escalation (KV, so a
// restarted Supervisor does not re-fire), emits one event and at most one
// message to the orchestrator. clearResolved closes an escalation when the
// agent shows real progress, which re-arms it.
var (
	RateLimitProbe     = 10 * time.Minute // re-check a rate-limited agent whose reset time is unknown
	RateLimitGrace     = 30 * time.Second // wake this long after the parsed reset time
	ModalPatience      = 2 * time.Minute  // a human-less permission/trust modal may block an agent owing work this long
	ModalRepeat        = 10 * time.Minute // while it stays blocked, remind the orchestrator and user this often
	UnreadAfter        = 10 * time.Minute // unread mail older than this on an idle agent is stuck
	UnreadRestartAfter = 60 * time.Second // between the unread reset-and-retype and the restart
	OrchSilentAfter    = 10 * time.Minute // orchestrator dead/silent with open tasks this long is down
)

// Escalation kinds (also the event kinds; see AC 19).
const (
	EscRateLimited    = "rate_limited"
	EscRateLimitedAll = "rate_limited_all"
	EscModalStuck     = "modal_stuck"
	EscReviveGaveUp   = "revive_gave_up"
	EscOrchDown       = "orch_down"
	EscUnreadStuck    = "unread_stuck"
	EscStuckGaveUp    = "stuck_gave_up"
	EscStalled        = "stalled"
)

const (
	kvRateLimit = "ratelimit." // + agent: "<wake at unix>"
	kvUnread    = "unread."    // + agent: "<stage>|<at unix>"
	kvRestarted = "restarted." // + agent: "1" until its next wake line says it was restarted
	kvModalAt   = "modal."     // + agent: unix time of the last modal notice
	kvTypeSkip  = "typeskip."  // + agent: "1" while typing is being withheld because a prompt is showing
)

type escalation struct {
	agent, kind, text string
	user              bool   // also tell the user (TUI banner / doctor)
	orchMsg           string // sent to the orchestrator once, unless the agent is the orchestrator
	taskID            string
	data              map[string]any
}

// escalate opens the escalation and tells whoever it names, exactly once until
// clearResolved (or ClearEscalation) closes it. It reports whether this call fired.
func (s *Supervisor) escalate(e escalation) bool {
	raised, err := s.Store.RaiseEscalation(store.Escalation{
		Agent: e.agent, Kind: e.kind, Text: e.text, Since: s.now().Unix(), User: e.user,
	})
	if err != nil {
		s.logf("escalate %s/%s: %v", e.kind, e.agent, err)
		return false
	}
	if !raised {
		return false
	}
	data := map[string]any{"text": e.text, "user": e.user, "escalation": true} // escalation: the notify hook keys on it
	for k, v := range e.data {
		data[k] = v
	}
	s.event(e.agent, e.kind, data)
	if e.orchMsg != "" && e.agent != s.orchestrator() {
		s.notify(s.orchestrator(), e.taskID, e.orchMsg)
	}
	return true
}

// holdOff: never act on an agent that is paused, stopped on purpose, focused in
// the TUI, or has someone attached to its session (see userPresent).
func (s *Supervisor) holdOff(ctx context.Context, a store.Agent, paused map[string]bool) bool {
	if paused[a.ID] || s.kvGet(kvStopped+a.ID) != "" {
		return true
	}
	return s.userPresent(ctx, a)
}

// safeToType reports whether text plus Enter can be typed into the agent's pane
// without answering a permission/trust prompt (hooks can lag the pane, and Enter
// on "❯ 1. Yes" approves). It records one type_skipped_modal event per episode.
func (s *Supervisor) safeToType(ctx context.Context, a store.Agent) bool {
	pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
	if err != nil {
		return true
	}
	return s.paneSafe(a, pane)
}

func (s *Supervisor) paneSafe(a store.Agent, pane string) bool {
	excerpt := s.modalExcerpt(a, pane)
	if excerpt == "" {
		_ = s.Store.DeleteKV(kvTypeSkip + a.ID)
		return true
	}
	if s.kvGet(kvTypeSkip+a.ID) == "" {
		_ = s.Store.SetKV(kvTypeSkip+a.ID, "1")
		s.event(a.ID, "type_skipped_modal", map[string]any{"text": excerpt})
		s.paneDecision(a, "type_blocked", pane)
	}
	return false
}

func (s *Supervisor) sessionLive(ctx context.Context, a store.Agent) bool {
	live, err := s.Tmux.HasSession(ctx, a.TmuxSession)
	return err == nil && live
}

// progressAfter reports whether the agent made a tool call or fired a hook after t.
func (s *Supervisor) progressAfter(id string, t int64) bool {
	if at, err := s.Store.LastHookEventAt(id); err == nil && at > t {
		return true
	}
	return int64(atoi(s.kvGet(kvLastSeen+id))) > t
}

// healthTick runs every health check once.
func (s *Supervisor) healthTick(ctx context.Context) {
	if s.Paused() {
		return
	}
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return
	}
	paused, _ := s.Store.PausedAgents()
	s.clearResolved(ctx, agents, tasks)
	for _, a := range agents {
		cfg, ok := s.agentCfg(a.ID)
		if !ok {
			continue
		}
		if s.holdOff(ctx, a, paused) {
			if !paused[a.ID] && s.kvGet(kvStopped+a.ID) == "" {
				s.presenceWatch(ctx, a) // a user is there: never type, but do not be silent about it
			}
			continue
		}
		s.guardAgent(ctx, a, cfg)
		s.landingAgent(ctx, a)
		s.applySnapshotLimit(a)
		a, _ = s.Store.AgentByID(a.ID)
		s.rateLimitAgent(ctx, a, agents)
		s.modalAgent(ctx, a, cfg, tasks)
		s.hooksAgent(ctx, a)
		if s.Driver {
			s.unreadAgent(ctx, a, cfg, tasks)
			s.mcpAgent(ctx, a, cfg, tasks)
		}
	}
	if s.Driver {
		s.orchWatch(ctx, agents, tasks, paused)
		s.stalledWatch(ctx, agents, paused)
	}
}

// ReviveFreshShown is how long the "orchestrator restarted without its context" item stays open.
const ReviveFreshShown = 10 * time.Minute

// clearResolved closes every escalation whose cause has gone away.
func (s *Supervisor) clearResolved(ctx context.Context, agents []store.Agent, tasks []store.Task) {
	escs, err := s.Store.OpenEscalations()
	if err != nil {
		return
	}
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	for _, e := range escs {
		a, ok := byID[e.Agent]
		lv := func() health.Liveness { return s.livenessOf(ctx, a) }
		done := false
		switch e.Kind {
		case EscRateLimitedAll:
			done = !s.allRateLimited(ctx, agents)
		case EscRateLimited:
			done = !ok || (lv() != health.RateLimited && s.progressAfter(a.ID, e.Since))
			if done {
				_ = s.Store.DeleteKV(kvRateLimit + e.Agent)
			}
		case EscModalStuck:
			done = !ok || lv() != health.Blocked
		case EscReviveGaveUp:
			done = !ok || lv() != health.Dead
		case EscOrchDown, EscStuckGaveUp:
			done = !ok || (lv() != health.Dead && s.progressAfter(a.ID, e.Since))
		case EscUnreadStuck:
			done = !ok || len(s.oldUnread(a.ID)) == 0
		case EscAttachedBlocking:
			done = s.presenceCleared(ctx, a, ok)
		case EscHooksFailing:
			at, _ := s.Store.LastHookEventAt(e.Agent)
			done = !ok || lv() == health.Dead || at > e.Since || (at != 0 && s.now().Sub(time.Unix(at, 0)) < HooksSilentAfter)
		case hooks.EscReviveFresh:
			// stays up long enough for the user to see it, then goes once the agent is working again
			done = !ok || (s.now().Sub(time.Unix(e.Since, 0)) > ReviveFreshShown && s.progressAfter(a.ID, e.Since))
		case EscMCPCrash:
			done = !ok || (lv() != health.Dead && s.progressAfter(a.ID, e.Since))
		case EscGuardMissing:
			done = !ok || lv() == health.Dead || s.guardReady(a)
			if done {
				s.guardRecovered(e.Agent)
			}
		case EscAgentUpgradeFailed:
			done = !ok || lv() == health.Dead || !service.StaleBuild(s.Store, a.ID, s.build())
		case EscStalled:
			cfg, cok := s.agentCfg(e.Agent)
			done = !ok || !cok || signature(s.owedAll(cfg, tasks)) != e.Text
		}
		if done {
			_ = s.Store.ClearEscalation(e.Agent, e.Kind)
		}
	}
}

// ---- rate limits (FM-4) ----

func (s *Supervisor) allRateLimited(ctx context.Context, agents []store.Agent) bool {
	limited := 0
	for _, a := range agents {
		switch s.livenessOf(ctx, a) {
		case health.RateLimited:
			limited++
		case health.Dead:
		default:
			return false
		}
	}
	return limited > 0
}

// rateLimitAgent handles an agent whose pane showed a usage cap: tell the
// orchestrator once, leave it alone (nudge, stuck and revive all skip it), and
// wake it after the parsed reset time (else probe every RateLimitProbe).
func (s *Supervisor) rateLimitAgent(ctx context.Context, a store.Agent, agents []store.Agent) {
	if s.livenessOf(ctx, a) != health.RateLimited {
		return
	}
	now := s.now()
	v := s.kvGet(kvRateLimit + a.ID)
	if v == "" {
		pane, _ := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
		wakeAt, text := now.Add(RateLimitProbe), "rate limited; reset time unknown, will probe every "+RateLimitProbe.String()
		if reset, ok := hooks.ParseRateLimitReset(pane, now); ok {
			wakeAt, text = reset.Add(RateLimitGrace), "rate limited until "+reset.Format("15:04")
		} else if until, ok := s.snapshotLimit(now); ok {
			wakeAt, text = until.Add(RateLimitGrace), "rate limited until "+until.Format("15:04")
		}
		_ = s.Store.SetKV(kvRateLimit+a.ID, strconv.FormatInt(wakeAt.Unix(), 10))
		s.escalate(escalation{agent: a.ID, kind: EscRateLimited, text: text, taskID: a.CurrentTask,
			data:    map[string]any{"wake_at": wakeAt.Unix()},
			orchMsg: fmt.Sprintf("%s is %s. slai will not nudge or restart it and wakes it automatically after the reset. Reassign its work with update_task if it is urgent.", a.ID, text)})
		if s.allRateLimited(ctx, agents) {
			s.escalate(escalation{agent: "team", kind: EscRateLimitedAll, user: true,
				text: "every agent is rate limited; nothing will progress until the limit resets"})
		}
		return
	}
	if now.Unix() < int64(atoi(v)) {
		return
	}
	if until, ok := s.snapshotLimit(now); ok { // the account is still capped: not yet
		_ = s.Store.SetKV(kvRateLimit+a.ID, strconv.FormatInt(until.Add(RateLimitGrace).Unix(), 10))
		return
	}
	if !s.safeToType(ctx, a) {
		return
	}
	line := "[slai] Your rate limit should have reset. Continue where you left off: call get_inbox, then get_task."
	if ident := roles.IdentityLineFor(s.Team, a.ID); ident != "" {
		line = ident + " " + line
	}
	if err := s.typeLine(ctx, a, line); err != nil {
		s.logf("rate-limit wake %s: %v", a.ID, err)
		return
	}
	// If the cap is still on, the pane detector flips it back and we probe again later.
	_, _ = s.Store.CompareAndSetStatus(a.ID, "working", hooks.StatusRateLimited)
	_ = s.Store.SetKV(kvRateLimit+a.ID, strconv.FormatInt(now.Add(RateLimitProbe).Unix(), 10))
	s.event(a.ID, "rate_limit_wake", map[string]any{})
}

// SnapshotStale is how old the statusline's rate-limit snapshot may be and still
// count; a missing or older one means "unknown", which never sets rate_limited.
const SnapshotStale = 10 * time.Minute

// snapshotLimit reports whether the account-wide rate-limit snapshot (written by
// `slai statusline`) shows a window at 100% or more that has not reset yet, and
// when the last such window resets. All agents share the account, so this is a
// second signal beside the pane text.
func (s *Supervisor) snapshotLimit(now time.Time) (until time.Time, limited bool) {
	snap, ok, err := ratelimit.Load(s.Store)
	if err != nil || !ok || snap.At == 0 || now.Sub(time.Unix(snap.At, 0)) > SnapshotStale {
		return time.Time{}, false
	}
	consider := func(pct float64, resets int64) {
		if pct >= 100 && resets > now.Unix() && time.Unix(resets, 0).After(until) {
			until, limited = time.Unix(resets, 0), true
		}
	}
	if w := snap.FiveHour; w != nil {
		consider(w.UsedPercent, w.ResetsAt)
	}
	if w := snap.SevenDay; w != nil {
		consider(w.UsedPercent, w.ResetsAt)
	}
	if w := snap.SpendLimit; w != nil {
		consider(w.UsedPercent, w.ResetsAt)
	}
	return until, limited
}

// applySnapshotLimit marks an agent rate_limited when the account is capped even
// though its pane shows no limit text (the agent may be mid-turn or idle). Agents
// parked on a prompt or the user, and dead ones, are left alone.
func (s *Supervisor) applySnapshotLimit(a store.Agent) {
	switch a.Status {
	case hooks.StatusRateLimited, "dead", hooks.StatusWaiting, hooks.StatusWaitingOnUser:
		return
	}
	if _, limited := s.snapshotLimit(s.now()); limited {
		_, _ = s.Store.CompareAndSetStatus(a.ID, hooks.StatusRateLimited, a.Status)
	}
}

// ---- human-less modal prompts (FM-3) ----

// modalAgent flags an agent parked on a permission/trust/login prompt that no
// ask_user explains while it owes work or has unread mail: one user escalation
// and one orchestrator note, repeated every ModalRepeat while it stays blocked.
// slai never answers the prompt: only the user can.
func (s *Supervisor) modalAgent(ctx context.Context, a store.Agent, cfg config.Agent, tasks []store.Task) {
	if s.livenessOf(ctx, a) != health.Blocked {
		return
	}
	now := s.now()
	if now.Sub(time.Unix(a.StatusAt, 0)) < ModalPatience {
		return
	}
	if len(owedBy(cfg, tasks)) == 0 {
		if n, err := s.Store.UnreadCount(a.ID); err != nil || n == 0 {
			return // blocked, but nothing is waiting on it
		}
	}
	if evs, err := s.Store.EventsByKind(a.ID, "ask_user", 1); err == nil && len(evs) > 0 && evs[0].At >= a.StatusAt-2 {
		return // a real question for the user is pending
	}
	pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
	if err != nil {
		return
	}
	excerpt := s.modalExcerpt(a, pane)
	if excerpt == "" {
		return
	}
	text := "needs you: " + excerpt
	orchMsg := func(prefix string) string {
		return fmt.Sprintf("%s%s has been stuck on a prompt only the user can answer: %s. The user has been told; slai will not answer it. Consider reassigning its work meanwhile.", prefix, a.ID, excerpt)
	}
	if s.escalate(escalation{agent: a.ID, kind: EscModalStuck, user: true, taskID: a.CurrentTask, text: text, orchMsg: orchMsg("")}) {
		_ = s.Store.SetKV(kvModalAt+a.ID, strconv.FormatInt(now.Unix(), 10))
		return
	}
	if last := int64(atoi(s.kvGet(kvModalAt + a.ID))); last != 0 && now.Sub(time.Unix(last, 0)) >= ModalRepeat {
		// a repeat is a clear + raise, so the once-per-state rule still holds between repeats
		_ = s.Store.ClearEscalation(a.ID, EscModalStuck)
		if s.escalate(escalation{agent: a.ID, kind: EscModalStuck, user: true, taskID: a.CurrentTask, text: text,
			data: map[string]any{"repeat": true}, orchMsg: orchMsg("Still blocked: ")}) {
			_ = s.Store.SetKV(kvModalAt+a.ID, strconv.FormatInt(now.Unix(), 10))
		}
	}
}

// ---- unread mail that never gets read (FM-6) ----

// oldUnread lists undelivered messages to id whose wake line was typed at least
// UnreadAfter ago, plus mail that waited that long because a user was attached or
// focused (presenceWatch marks it): that mail was skipped on purpose, and once the user
// has gone it is as overdue as a wake that went unanswered. Mail that was never woken for
// any other reason (concurrency cap, spend gate, pause) is the wake loop's business, not
// evidence of a stuck agent.
func (s *Supervisor) oldUnread(id string) []store.Message {
	msgs, err := s.Store.UndeliveredFor(id)
	if err != nil {
		return nil
	}
	cut := s.now().Add(-UnreadAfter).Unix()
	skipped := int64(atoi(s.kvGet(kvPresenceSkip + id)))
	var out []store.Message
	for _, m := range msgs {
		switch {
		case m.WokenAt != 0 && m.WokenAt <= cut:
			out = append(out, m)
		case m.WokenAt == 0 && skipped != 0 && skipped <= cut && m.CreatedAt <= cut:
			out = append(out, m)
		}
	}
	return out
}

// unreadAgent: an alive, idle agent that has not read mail for UnreadAfter gets
// one reset-and-retype of the wake line, then (UnreadRestartAfter later, still
// unread) the stuck-restart path, with one notice to the orchestrator (the user,
// if the orchestrator is the one not reading).
func (s *Supervisor) unreadAgent(ctx context.Context, a store.Agent, cfg config.Agent, tasks []store.Task) {
	old := s.oldUnread(a.ID)
	if len(old) == 0 {
		_ = s.Store.DeleteKV(kvUnread + a.ID)
		return
	}
	if !idleish(a.Status) || !s.sessionLive(ctx, a) {
		return
	}
	now := s.now()
	f := strings.Split(s.kvGet(kvUnread+a.ID), "|")
	stage, at := 0, int64(0)
	if len(f) == 2 {
		stage = atoi(f[0])
		at, _ = strconv.ParseInt(f[1], 10, 64)
	}
	save := func(st int) {
		_ = s.Store.SetKV(kvUnread+a.ID, fmt.Sprintf("%d|%d", st, now.Unix()))
	}
	switch {
	case stage == 0:
		if !s.safeToType(ctx, a) {
			return
		}
		line := fmt.Sprintf("[slai] You have %d new items. Call get_inbox.", len(old))
		err := s.retype(ctx, a, line)
		s.event(a.ID, "unread_nudge", map[string]any{"unread": len(old), "error": errStr(err)})
		save(1)
	case stage == 1 && now.Sub(time.Unix(at, 0)) >= UnreadRestartAfter:
		text := fmt.Sprintf("%d message(s) unread for over %s", len(old), UnreadAfter)
		s.escalate(escalation{agent: a.ID, kind: EscUnreadStuck, user: cfg.Role == config.RoleOrchestrator,
			text: text, taskID: old[0].TaskID,
			orchMsg: fmt.Sprintf("%s has not read its inbox (%s) even after slai re-sent the wake-up. slai is restarting its session; check on it if that does not help.", a.ID, text)})
		save(2)
		s.stuckRestart(ctx, a, cfg, tasks, s.lastActivity(a), fmt.Sprintf("%s unread for over %s; the wake-up was re-typed %s ago and still not read",
			plural(len(old), "message"), UnreadAfter, UnreadRestartAfter))
	}
}

// ---- orchestrator watch (FM-7) ----

// orchWatch restarts a dead or silent orchestrator that has open tasks, with a
// resume brief, and tells the user directly (never through the orchestrator).
func (s *Supervisor) orchWatch(ctx context.Context, agents []store.Agent, tasks []store.Task, paused map[string]bool) {
	oid := s.orchestrator()
	var o store.Agent
	for _, a := range agents {
		if a.ID == oid {
			o = a
		}
	}
	if o.ID == "" || s.holdOff(ctx, o, paused) {
		return
	}
	now := s.now()
	var open []string
	for _, t := range tasks {
		if openTask(t) {
			open = append(open, "- "+taskLine(t, now))
		}
	}
	if len(open) == 0 {
		return
	}
	threshold := OrchSilentAfter
	var reason string
	switch lv := s.livenessOf(ctx, o); {
	case lv == health.Dead:
		if now.Sub(time.Unix(o.StatusAt, 0)) >= threshold {
			reason = "the orchestrator is down"
		}
	case lv == health.Blocked:
		// parked on the user: not the orchestrator's fault
	default:
		// Quiet is not silent: an idle orchestrator waiting on busy engineers is
		// healthy. It is silent when the board has not changed for the threshold,
		// nobody else is working, and it was woken for mail it never read.
		quiet := now.Sub(time.Unix(s.lastActivity(o), 0)) >= threshold
		boardStill := false
		if f := strings.SplitN(s.kvGet(kvBoardAt), "|", 2); len(f) == 2 && f[0] == signature(tasks) {
			ts, _ := strconv.ParseInt(f[1], 10, 64)
			boardStill = now.Sub(time.Unix(ts, 0)) >= threshold
		}
		othersWorking := false
		for _, a := range agents {
			if a.ID != o.ID && a.Status == "working" {
				othersWorking = true
			}
		}
		if quiet && boardStill && !othersWorking && len(s.oldUnread(o.ID)) > 0 {
			reason = "the orchestrator has been silent for " + threshold.String() + " with tasks open, the board unchanged and mail unread"
		}
	}
	if reason == "" {
		return
	}
	unread, _ := s.Store.UndeliveredFor(o.ID)
	if !s.escalate(escalation{agent: o.ID, kind: EscOrchDown, user: true, text: "ORCH DOWN: " + reason,
		data: map[string]any{"open": len(open), "unread": len(unread)}}) {
		return
	}
	_, _ = s.Store.AddMessage(store.Message{From: "slai", To: o.ID, Kind: "note", Body: fmt.Sprintf(
		"slai restarted you (%s). You lost your previous context. Resume brief: %d unread message(s) wait in your inbox (call get_inbox), and %d task(s) are open:\n%s\nCall get_board, then decide the next action for each task now.",
		reason, len(unread)+1, len(open), strings.Join(open, "\n"))})
	if s.livenessOf(ctx, o) != health.Dead && s.sessionLive(ctx, o) {
		if err := s.killSession(ctx, o.ID, o.TmuxSession, "orchestrator restarted: "+reason); err != nil {
			s.logf("orch restart: %v", err)
			return
		}
		s.restartEvent(o.ID, "orch_restart", "orchestrator restarted: "+reason, map[string]any{"open": len(open), "unread": len(unread)})
		_ = s.Store.SetAgentStatus(o.ID, "dead") // autoRevive takes it from here
	}
}

// ---- stalled escalation reaches the user (FM-5) ----

// stalledWatch promotes a stalled escalation to the user when the orchestrator
// has neither called a tool nor sent a message since it was raised.
func (s *Supervisor) stalledWatch(ctx context.Context, agents []store.Agent, paused map[string]bool) {
	escs, err := s.Store.OpenEscalations()
	if err != nil {
		return
	}
	now := s.now()
	oid := s.orchestrator()
	for _, e := range escs {
		if e.Kind != EscStalled || e.User {
			continue
		}
		// The clock starts when the orchestrator could have seen the note (it
		// claimed its inbox), not when the note was queued: a busy orchestrator
		// only reads it after its current turn.
		seen := s.stalledNoteSeenAt(oid, e)
		if seen == 0 || now.Sub(time.Unix(seen, 0)) < OrchIdleAfter {
			continue
		}
		held := false
		for _, a := range agents {
			if a.ID == e.Agent || a.ID == oid {
				held = held || s.holdOff(ctx, a, paused)
			}
			// the stalled agent turned out to be busy on a background task (or gated /
			// rate limited): there is nothing to tell the user about
			if a.ID == e.Agent {
				switch s.livenessOf(ctx, a) {
				case health.Busy, health.Gated, health.RateLimited:
					held = true
				}
			}
		}
		if held || s.progressAfter(oid, seen) || s.sentSince(oid, seen) {
			continue
		}
		_ = s.Store.ClearEscalation(e.Agent, e.Kind)
		raised, err := s.Store.RaiseEscalation(store.Escalation{Agent: e.Agent, Kind: e.Kind, Text: e.Text, Since: e.Since, User: true})
		if err == nil && raised {
			s.event(e.Agent, EscStalled, map[string]any{"user": true, "text": "stalled: " + e.Text})
		}
	}
}

// stalledNoteSeenAt is when the orchestrator claimed the stalled note for e (0 if
// it has not yet).
func (s *Supervisor) stalledNoteSeenAt(orch string, e store.Escalation) int64 {
	msgs, err := s.Store.RecentMessages(500)
	if err != nil {
		return 0
	}
	var seen int64
	for _, m := range msgs {
		if m.From == "slai" && m.To == orch && m.CreatedAt >= e.Since-1 && strings.Contains(m.Body, e.Agent+" (") && strings.Contains(m.Body, "has not made progress") {
			if m.DeliveredAt == 0 {
				return 0
			}
			seen = max(seen, m.DeliveredAt)
		}
	}
	return seen
}

func (s *Supervisor) sentSince(from string, t int64) bool {
	msgs, err := s.Store.RecentMessages(500)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.From == from && m.CreatedAt >= t {
			return true
		}
	}
	return false
}

// ---- wake-landed verification ----

var WakeLandWindow = 15 * time.Second // a typed wake must produce a UserPromptSubmit hook within this

const (
	HooksSilentAfter = 5 * time.Minute  // pane keeps changing but no hook this long = hooks_failing
	hooksPaneChanges = 3                // distinct pane changes that count as "keeps changing"
	MCPStampWindow   = 60 * time.Second // an mcp__slai__ call must stamp last-seen within this
	MCPRepeatWindow  = 30 * time.Minute // a second MCP failure inside this escalates
)

const (
	EscHooksFailing = "hooks_failing"
	EscMCPCrash     = "mcp_crash"

	kvMCPFail = "mcp.fail."     // + agent: unix time of its latest consecutive MCP failure (cleared by a good call)
	kvLanding = "wake.landing." // + agent: "<sent at>|<previous status>|<last prompt-submit event id>|<retried 0/1>|<line>"
)

type landing struct {
	sentAt, baseID int64
	prev           string
	retried        bool
	line           string
}

func (s *Supervisor) loadLanding(id string) (landing, bool) {
	f := strings.SplitN(s.kvGet(kvLanding+id), "|", 5)
	if len(f) != 5 {
		return landing{}, false
	}
	l := landing{prev: f[1], retried: f[3] == "1", line: f[4]}
	l.sentAt, _ = strconv.ParseInt(f[0], 10, 64)
	l.baseID, _ = strconv.ParseInt(f[2], 10, 64)
	return l, true
}

func (s *Supervisor) saveLanding(id string, l landing) {
	r := 0
	if l.retried {
		r = 1
	}
	_ = s.Store.SetKV(kvLanding+id, fmt.Sprintf("%d|%s|%d|%d|%s", l.sentAt, l.prev, l.baseID, r, l.line))
}

// lastPromptID is the id of the agent's newest UserPromptSubmit event (0 if none).
func (s *Supervisor) lastPromptID(id string) int64 {
	evs, err := s.Store.EventsByKind(id, "hook:userpromptsubmit", 1)
	if err != nil || len(evs) == 0 {
		return 0
	}
	return evs[0].ID
}

// noteWakeTyped records that a wake line was just typed, so landingAgent can
// check that the agent actually took it.
func (s *Supervisor) noteWakeTyped(a store.Agent, line string) {
	if !s.harnessOf(a.ID).Features().PromptSubmitEvent {
		return // no prompt-submitted event to confirm it with
	}
	s.saveLanding(a.ID, landing{sentAt: s.now().Unix(), prev: a.Status, baseID: s.lastPromptID(a.ID), line: line})
}

// landingAgent verifies a typed wake: the optimistic "working" status is only
// believed if a UserPromptSubmit hook follows within WakeLandWindow. If not, the
// status is reverted, the input box cleared and the line retyped once; a second
// miss reverts again and leaves it to the wake loop's bounded retries (the wake
// already counted as unanswered).
func (s *Supervisor) landingAgent(ctx context.Context, a store.Agent) {
	l, ok := s.loadLanding(a.ID)
	if !ok {
		return
	}
	if s.lastPromptID(a.ID) > l.baseID {
		_ = s.Store.DeleteKV(kvLanding + a.ID) // it landed
		return
	}
	now := s.now()
	if now.Sub(time.Unix(l.sentAt, 0)) < WakeLandWindow {
		return
	}
	if a.Status == "working" {
		_, _ = s.Store.CompareAndSetStatus(a.ID, l.prev, "working")
	}
	if l.retried || !s.sessionLive(ctx, a) {
		s.event(a.ID, "wake_not_landed", map[string]any{"retried": l.retried})
		_ = s.Store.DeleteKV(kvLanding + a.ID)
		return
	}
	if !s.safeToType(ctx, a) {
		return
	}
	err := s.retype(ctx, a, l.line)
	s.event(a.ID, "wake_retyped", map[string]any{"error": errStr(err)})
	if err == nil {
		_, _ = s.Store.CompareAndSetStatus(a.ID, "working", l.prev)
	}
	l.retried, l.sentAt = true, now.Unix()
	s.saveLanding(a.ID, l)
}

// ---- hooks that stopped arriving ----

// hooksAgent raises hooks_failing when the pane keeps changing but no hook event
// has arrived for HooksSilentAfter: Claude is running but slai is blind to it
// (broken settings, `slai` not on PATH, the hook erroring).
func (s *Supervisor) hooksAgent(ctx context.Context, a store.Agent) {
	if a.Status == "dead" || a.Status == hooks.StatusRateLimited {
		return
	}
	now := s.now()
	last, _ := s.Store.LastHookEventAt(a.ID)
	if now.Sub(time.Unix(max(last, a.StatusAt), 0)) < HooksSilentAfter && last != 0 {
		s.clearHookWatch(a.ID)
		return
	}
	if last == 0 && now.Sub(time.Unix(a.StatusAt, 0)) < HooksSilentAfter {
		return
	}
	pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
	if err != nil {
		return
	}
	// Only a pane that reads "working" (spinner / esc to interrupt) counts: an idle
	// prompt's pane also changes (the statusline shows team-wide usage figures).
	if st, ok := s.detectPane(a, pane); !ok || st != hooks.StatusWorking {
		s.clearHookWatch(a.ID)
		return
	}
	// A tool call in flight, or subagents running for the session, legitimately fire no
	// hooks, however long they run.
	rules := s.harnessOf(a.ID).PaneRules()
	if hooks.ToolRunning(pane) || hooks.SubagentsWith(rules, pane) > 0 {
		s.clearHookWatch(a.ID)
		return
	}
	// Waiting on a background shell or monitor (the footer says so) fires no hooks either.
	if hooks.BackgroundWith(rules, pane) != "" && !s.isBlockedPane(a, pane) {
		s.clearHookWatch(a.ID)
		return
	}
	sum := sha1.Sum([]byte(hooks.StablePane(pane)))
	h := hex.EncodeToString(sum[:])
	s.rt.mu.Lock()
	if s.rt.hookPanes == nil {
		s.rt.hookPanes = map[string]hookPane{}
	}
	hp := s.rt.hookPanes[a.ID]
	if hp.hash != "" && hp.hash != h {
		hp.changes++
	}
	hp.hash = h
	s.rt.hookPanes[a.ID] = hp
	changes := hp.changes
	s.rt.mu.Unlock()
	if changes < hooksPaneChanges {
		return
	}
	text := fmt.Sprintf("hooks are not arriving (none for %s while its pane keeps changing)", HooksSilentAfter)
	if r, ok := hooks.LoadErr(s.Store, hooks.KVHookErr+a.ID); ok {
		text += "; last hook error: " + firstLine(r.Last, 120)
	}
	where := "`slai doctor`"
	if _, err := os.Stat(filepath.Join(s.Dir, ".slai", "run", a.ID, "hook.log")); err == nil {
		where += fmt.Sprintf(" and .slai/run/%s/hook.log", a.ID) // written only when a hook errors
	}
	s.escalate(escalation{agent: a.ID, kind: EscHooksFailing, text: text,
		orchMsg: fmt.Sprintf("%s: %s. slai cannot see its status or tool use, so liveness and nudges for it are unreliable. Check %s.", a.ID, text, where)})
}

func (s *Supervisor) clearHookWatch(id string) {
	s.rt.mu.Lock()
	delete(s.rt.hookPanes, id)
	s.rt.mu.Unlock()
}

type hookPane struct {
	hash    string
	changes int
}

// ---- per-agent MCP crash ----

// mcpAgent: the PreToolUse hook saw the agent start an mcp__slai__ call, but no
// service call stamped its last-seen within MCPStampWindow, so its slai MCP
// server is not answering. The session is restarted once (Claude reconnects its
// MCP servers on start); a repeat inside MCPRepeatWindow escalates to the user.
func (s *Supervisor) mcpAgent(ctx context.Context, a store.Agent, cfg config.Agent, tasks []store.Task) {
	pend := int64(atoi(s.kvGet(hooks.KVMCPPending + a.ID)))
	if pend == 0 {
		return
	}
	if int64(atoi(s.kvGet(kvLastSeen+a.ID))) >= pend {
		_ = s.Store.DeleteKV(hooks.KVMCPPending + a.ID) // the call reached the service
		_ = s.Store.DeleteKV(kvMCPFail + a.ID)          // the call reached the service; mcp.err stays for doctor
		return
	}
	now := s.now()
	if now.Sub(time.Unix(pend, 0)) < MCPStampWindow || a.Status == "dead" {
		return
	}
	// False-positive guard (T-0051). The stamp is taken when the call ARRIVES at the
	// MCP server, so a slow tool (ping waits up to two minutes) does not trip this.
	// What does: the PreToolUse hook fires, then the call never reaches the server
	// because the user interrupted the turn, the call was denied, or the server was
	// still starting. In every such case the agent carries on, so a hook event newer
	// than the pending call means it moved past it: a hung MCP server blocks the turn
	// and produces no further hooks. Forget the pending call instead of restarting a
	// working agent.
	if at, err := s.Store.LastHookEventAt(a.ID); err == nil && at > pend {
		_ = s.Store.DeleteKV(hooks.KVMCPPending + a.ID)
		return
	}
	// A tool call can sit unanswered because the user has not approved it (manual
	// permission mode): that is a prompt, not a dead connection.
	if a.Status == hooks.StatusWaiting || a.Status == hooks.StatusWaitingOnUser {
		return
	}
	if pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0); err == nil && s.modalExcerpt(a, pane) != "" {
		return
	}
	_ = s.Store.DeleteKV(hooks.KVMCPPending + a.ID)
	prev := int64(atoi(s.kvGet(kvMCPFail + a.ID)))
	_ = s.Store.SetKV(kvMCPFail+a.ID, strconv.FormatInt(now.Unix(), 10))
	_ = hooks.RecordErr(s.Store, hooks.KVMCPErr+a.ID, fmt.Sprintf("no slai tool call stamp %s after an mcp__slai__ call started", MCPStampWindow), now)
	if prev != 0 && now.Sub(time.Unix(prev, 0)) < MCPRepeatWindow {
		s.escalate(escalation{agent: a.ID, kind: EscMCPCrash, user: true,
			text:    "slai MCP server keeps failing for this agent (restart did not help)",
			orchMsg: fmt.Sprintf("%s's slai MCP connection failed again after a restart; it cannot call slai tools. The user has been told. Reassign its work if it is urgent.", a.ID)})
		return
	}
	cause := fmt.Sprintf("a slai tool call started %ds ago and never reached the service: its MCP server crashed or lost its connection", int(now.Unix()-pend))
	s.event(a.ID, "mcp_restart", map[string]any{"pending_s": int(now.Unix() - pend), "cause": cause})
	s.stuckRestart(ctx, a, cfg, tasks, s.lastActivity(a), cause)
}
