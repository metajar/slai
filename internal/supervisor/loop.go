package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
)

// Tunables (PRD 3.3, 3.4, 8).
const (
	DefaultInterval   = time.Second
	WakeRetry         = 60 * time.Second  // re-wake an idle agent whose earlier nudge went unanswered
	FallbackStale     = 30 * time.Second  // hook data older than this triggers pane polling
	FallbackPoll      = 2 * time.Second   // pane poll period per agent
	reviewChargeGrace = 10 * time.Second  // idle this long: the last turn's usage row is written
	KVPaused          = "paused"          // global pause flag ("1" when paused)
	KVPendingStart    = "startup.pending" // agent ids held back until Claude trusts the folder
	KVMergeCursor     = "merge.cursor"    // last processed merge_requested event id
	KVLoopReset       = "loop.reset_at"   // unix; loop-guard window never starts before this
)

type runtime struct {
	mu        sync.Mutex // guards the maps below
	lastPoll  map[string]time.Time
	wakeKeys  map[string]bool      // one-shot reports already made (wake failures, switch notes)
	panes     map[string]paneSeen  // stuck detector: last pane capture per agent
	hookPanes map[string]hookPane  // hooks_failing detector: pane change counts per agent
	lastBG    map[string]time.Time // background-task footer poll: last read per agent
	mergeMu   sync.Mutex           // serializes merges (PRD 8)
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
		return
	}
	log.Printf(format, a...)
}

func (s *Supervisor) agentCfg(id string) (config.Agent, bool) {
	for _, a := range s.Team.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return config.Agent{}, false
}

func (s *Supervisor) orchestrator() string {
	for _, a := range s.Team.Agents {
		if a.Role == config.RoleOrchestrator {
			return a.ID
		}
	}
	return ""
}

func (s *Supervisor) event(agent, kind string, data any) {
	b, _ := json.Marshal(data)
	if err := s.Store.AddEvent(agent, kind, string(b)); err != nil {
		s.logf("event %s: %v", kind, err)
	}
}

// Run ticks until ctx is cancelled. Returns nil on cancellation.
func (s *Supervisor) Run(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	_ = s.Store.SetSupervisorHeartbeat(s.now().Unix()) // before hashing the binary: `up` and doctor look right away
	s.recordBuild()
	for {
		s.Tick(ctx)
		s.reexecIfUpgraded() // between ticks: no step is in flight
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs one pass: liveness/fallback status, guards, merges, wake-ups.
// Every stage is isolated: a panic in one is logged and recorded as an event but
// can neither skip the other stages nor kill the supervisor, because a dead
// supervisor means nobody delivers work to any agent.
func (s *Supervisor) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	_ = s.Store.SetSupervisorHeartbeat(s.now().Unix())
	s.beginTick()
	s.runSteps([]step{
		{"release-pending", func() { s.releasePending(ctx) }},
		{"team", func() { s.teamTick(ctx) }},
		{"release-holds", s.releaseHolds},
		{"drive", func() { s.driveTick(ctx) }},
		{"health", func() { s.healthTick(ctx) }},
		{"upgrade", func() { s.upgradeTick(ctx) }},
		{"status", func() { s.statusTick(ctx) }},
		{"background", func() { s.backgroundTick(ctx) }},
		{"loop-guard", func() { s.checkLoops() }},
		{"merge", func() { s.mergeTick(ctx) }},
		{"memory", func() { s.memoryTick(ctx) }},
		{"blocked-reports", func() { s.blockedReportsTick(ctx) }},
		{"redirect", func() { s.redirectTick(ctx) }},
		{"worktree", func() { s.worktreeTick(ctx) }},
		{"auto-reset", func() { s.autoResetTick(ctx) }},
		{"reset", func() { s.resetTick(ctx) }},
		{"wake", func() { s.wakeTick(ctx) }},
		{"notify", func() { s.notifyTick(ctx) }},
	})
}

type step struct {
	name string
	run  func()
}

func (s *Supervisor) runSteps(steps []step) {
	for _, st := range steps {
		// Beat between steps too, so a long merge does not read as a dead
		// supervisor while a genuinely hung step still goes stale.
		_ = s.Store.SetSupervisorHeartbeat(s.now().Unix())
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.logf("supervisor: %s panicked: %v\n%s", st.name, r, debug.Stack())
					s.event("", "supervisor_panic", map[string]any{"step": st.name, "panic": fmt.Sprint(r)})
				}
			}()
			st.run()
		}()
	}
}

// ---- global / per-agent pause ----

// Pause stops wake-ups globally (persisted in kv).
func (s *Supervisor) Pause() error { return s.Store.SetKV(KVPaused, "1") }

// Resume re-enables wake-ups.
func (s *Supervisor) Resume() error {
	_ = s.Store.DeleteKV(KVTeamHold) // a resume of any kind ends a team hold too
	return s.Store.SetKV(KVPaused, "0")
}

// Paused reports the global pause flag.
func (s *Supervisor) Paused() bool {
	v, ok, err := s.Store.GetKV(KVPaused)
	return err == nil && ok && v == "1"
}

// PauseAgent pauses wake-ups for one agent.
func (s *Supervisor) PauseAgent(id string) error { return s.Store.SetAgentPaused(id, true) }

// ResumeAgent resumes one agent and restarts the loop-guard window.
func (s *Supervisor) ResumeAgent(id string) error {
	if err := s.Store.SetAgentPaused(id, false); err != nil {
		return err
	}
	_ = s.Store.DeleteKV(KVHoldPrefix + id)
	return s.Store.SetKV(KVLoopReset, fmt.Sprint(s.now().Unix()))
}

// SetPriority changes a task's priority (a number, up, down or top) and records who
// asked. It is the one write path for `slai priority` and the TUI's Tasks view.
func (s *Supervisor) SetPriority(taskID, spec string) (int, error) {
	p, err := s.Store.ApplyPriority(taskID, spec)
	if err != nil {
		return 0, err
	}
	s.event("user", "priority", map[string]any{"task_id": taskID, "priority": p})
	return p, nil
}

// SendUser queues a kind=user message from "user" to an agent.
func (s *Supervisor) SendUser(to, body string) (int64, error) { return s.SendUserReply(to, body, 0) }

// SendUserReply is SendUser with reply_to set (0 = none): `slai reply` passes the ask_user event id.
func (s *Supervisor) SendUserReply(to, body string, replyTo int64) (int64, error) {
	if _, ok := s.agentCfg(to); !ok {
		return 0, fmt.Errorf("unknown agent %q", to)
	}
	return s.Store.AddMessage(store.Message{From: "user", To: to, Kind: "user", ReplyTo: replyTo, Body: body})
}

// ---- status: liveness + pane fallback ----

func (s *Supervisor) statusTick(ctx context.Context) {
	agents, err := s.Store.GetAgents()
	if err != nil {
		s.logf("status: %v", err)
		return
	}
	now := s.now()
	s.rt.mu.Lock()
	if s.rt.lastPoll == nil {
		s.rt.lastPoll = map[string]time.Time{}
	}
	s.rt.mu.Unlock()
	for _, a := range agents {
		live, err := s.Tmux.HasSession(ctx, a.TmuxSession)
		if err != nil {
			continue
		}
		if !live {
			if a.Status != "dead" {
				_ = s.Store.SetAgentStatus(a.ID, "dead")
			}
			continue
		}
		if a.Status == "dead" {
			_ = s.Store.SetAgentStatus(a.ID, "idle")
			continue
		}
		// Hooks are the primary signal: the pane is only read when no hook event, not just
		// no status change, is fresher than FallbackStale.
		signal := a.StatusAt
		if at, err := s.Store.LastHookEventAt(a.ID); err == nil && at > signal {
			signal = at
		}
		if a.Status == "waiting_on_user" || now.Sub(time.Unix(signal, 0)) <= FallbackStale {
			continue
		}
		if _, capped := s.snapshotLimit(now); capped && a.Status == hooks.StatusRateLimited {
			continue // the account-wide snapshot says capped; the pane may well look idle
		}
		s.rt.mu.Lock()
		last := s.rt.lastPoll[a.ID]
		due := now.Sub(last) >= FallbackPoll
		if due {
			s.rt.lastPoll[a.ID] = now
		}
		s.rt.mu.Unlock()
		if !due {
			continue
		}
		pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
		if err != nil {
			continue
		}
		st, ok := s.detectPane(a, pane)
		if !ok || st == a.Status || (st == "idle" && a.Status == "finished") {
			continue
		}
		_ = s.Store.SetAgentStatusKeepAge(a.ID, st)
	}
}

// ---- wake-ups (PRD 3.4) ----

func idleish(st string) bool { return st == "idle" || st == "finished" }

func (s *Supervisor) wakeTick(ctx context.Context) {
	if s.Paused() {
		return
	}
	agents, err := s.Store.GetAgents()
	if err != nil {
		s.logf("wake: %v", err)
		return
	}
	// Fallback for a review charge the Stop hook never ended (see store.KVReviewEndCharge):
	// an agent idle for a while has had its last turn's usage recorded.
	for _, a := range agents {
		if idleish(a.Status) && s.idleFor(a) > reviewChargeGrace {
			s.Store.FinishReviewCharge(a.ID)
		}
	}
	pending, err := s.Store.PendingWake(s.now().Add(-WakeRetry).Unix())
	if err != nil {
		return
	}
	if len(pending) == 0 {
		s.clearStaleSkips(agents, nil)
		return
	}
	byAgent := map[string][]store.Message{}
	for _, m := range pending {
		byAgent[m.To] = append(byAgent[m.To], m)
	}
	s.clearStaleSkips(agents, byAgent)
	paused, _ := s.Store.PausedAgents()
	gate := s.checkSpend()
	// A "working" status only holds a slot while the agent shows signs of life: one whose
	// hooks and tool calls went quiet (a phantom) must not starve the rest of the team.
	working := 0
	for _, a := range agents {
		if a.Status == "working" && s.now().Sub(time.Unix(s.lastActivity(a), 0)) <= health.StaleAfter {
			working++
		}
	}
	// When max_concurrent_agents is the limit, the agent owed the highest-priority task
	// gets the free slot first. Stable, so equal priorities keep team order.
	prio := s.Store.TaskPriorities()
	owed := func(id string) int {
		top := math.MinInt
		for _, m := range byAgent[id] {
			top = max(top, prio[m.TaskID]) // an item with no task counts as 0
		}
		if top == math.MinInt { // nothing owed: it is skipped below, any value will do
			return 0
		}
		return top
	}
	sort.SliceStable(agents, func(i, j int) bool { return owed(agents[i].ID) > owed(agents[j].ID) })
	for _, a := range agents {
		all := byAgent[a.ID]
		if len(all) == 0 {
			continue
		}
		// Assignments whose worktree switch failed are held back from get_inbox (see
		// prepareAssignments); they must not count toward this agent's wake state or line.
		msgs := s.withoutHeld(all)
		if paused[a.ID] || gate.blocked(a.ID) {
			s.skipWake(a.ID, SkipHeld)
			continue
		}
		answer := a.Status == "waiting_on_user" && hasKind(msgs, "user")
		// An agent parked on ask_user must not hold the team's other messages hostage
		// forever: after AskUserPatience it receives them too (and proceeds on defaults).
		if a.Status == "waiting_on_user" && s.idleFor(a) > AskUserPatience {
			answer = true
		}
		if !idleish(a.Status) && !answer {
			s.skipWake(a.ID, SkipBusy)
			continue
		}
		if len(msgs) > 0 && s.wakeHeld(ctx, a, msgs) {
			s.skipWake(a.ID, SkipHeld)
			continue
		}
		if working >= s.Team.Defaults.MaxConcurrentAgents && !hasKind(msgs, "ping") { // a liveness ping must get through
			s.skipWake(a.ID, SkipSlot)
			continue
		}
		if live, err := s.Tmux.HasSession(ctx, a.TmuxSession); err != nil || !live {
			continue
		}
		if s.Store.Focused(a.ID, s.now()) {
			s.skipWake(a.ID, SkipFocus)
			continue // user is typing into this agent in the TUI
		}
		if n, err := s.Tmux.ClientCount(ctx, a.TmuxSession); err != nil || n > 0 {
			s.skipWake(a.ID, SkipAttached)
			continue // user is attached; never type over them
		}
		all = s.refreshPending(a.ID, all)
		held := s.prepareAssignmentsHeld(ctx, a, all)
		msgs = withoutIDs(s.withoutHeld(all), held)
		if len(msgs) == 0 {
			if len(held) > 0 {
				s.skipWake(a.ID, SkipPrepFailed)
			}
			continue // nothing deliverable yet; the switch is retried on the next tick
		}
		if !s.safeToType(ctx, a) {
			s.skipWake(a.ID, SkipModal)
			continue
		}
		line := fmt.Sprintf("[slai] You have %d new items. Call get_inbox.", len(msgs))
		restarted := s.kvGet(kvRestarted+a.ID) != ""
		upgraded := s.kvGet(kvUpgraded+a.ID) != ""
		if upgraded {
			line = upgradeWakeLine(len(msgs))
		} else if restarted {
			line = fmt.Sprintf("[slai] You were restarted and lost your previous context. You have %d new items. Call get_inbox, then get_task.", len(msgs))
		}
		// after any gap in its context the line opens with who the agent is
		pending := s.kvGet(hooks.KVIdentityPending+a.ID) != ""
		if restarted || upgraded || pending {
			if ident := roles.IdentityLineFor(s.Team, a.ID); ident != "" {
				line = ident + " " + line
			}
		}
		// assignments and post-restart/upgrade wakes point at the task's memory: one sentence,
		// no fact text (the pack is fetched through get_task / get_memory)
		if task := s.wakeMemoryTask(a, msgs, restarted || upgraded); task != "" {
			if note := s.memoryPointer(ctx, task); note != "" {
				line += " " + note
			}
		}
		if err := s.typeLine(ctx, a, line); err != nil {
			s.logf("wake %s: %v", a.ID, err)
			continue
		}
		if restarted {
			_ = s.Store.DeleteKV(kvRestarted + a.ID)
		}
		if pending {
			_ = s.Store.DeleteKV(hooks.KVIdentityPending + a.ID)
		}
		if upgraded {
			_ = s.Store.DeleteKV(kvUpgraded + a.ID)
		}
		s.clearSkip(a.ID)
		s.wakeSent(a, msgs)
		s.noteWakeTyped(a, line)
		ids := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			_ = s.Store.MarkWoken(m.ID)
			ids = append(ids, m.ID)
		}
		// Optimistic; hooks overwrite this as soon as the agent reacts.
		_, _ = s.Store.CompareAndSetStatus(a.ID, "working", a.Status)
		working++
		s.event(a.ID, "wake", map[string]any{"count": len(msgs), "message_ids": ids})
	}
}

// Why mail is waiting (H-1). Every branch of the wake tick that leaves an agent's mail
// unwoken records kv wake.skip.<agent> = "<reason>|<since>"; a wake clears it. `slai status`
// and `slai doctor` (H-4) show it.
const (
	kvWakeSkip     = "wake.skip."  // + agent
	SkipHeld       = "held"        // paused, spend-capped, loop-guard hold, or too many unanswered wakes
	SkipSlot       = "slot"        // max_concurrent_agents is full
	SkipFocus      = "focus"       // the user is typing into the agent in the TUI
	SkipAttached   = "attached"    // a tmux client is attached
	SkipModal      = "modal"       // a permission or trust prompt is on screen
	SkipPrepFailed = "prep_failed" // the worktree switch for its assignment failed
	SkipBusy       = "busy"        // mid-turn: mail waits for its Stop hook
)

// skipWake records why agent's mail is not being woken now. The since stamp survives repeated
// ticks with the same reason.
func (s *Supervisor) skipWake(agent, reason string) {
	if cur := strings.SplitN(s.kvGet(kvWakeSkip+agent), "|", 2); cur[0] == reason {
		return
	}
	_ = s.Store.SetKV(kvWakeSkip+agent, fmt.Sprintf("%s|%d", reason, s.now().Unix()))
}

func (s *Supervisor) clearSkip(agent string) {
	if s.kvGet(kvWakeSkip+agent) != "" {
		_ = s.Store.DeleteKV(kvWakeSkip + agent)
	}
}

// clearStaleSkips drops the reason of every agent that has nothing pending any more.
func (s *Supervisor) clearStaleSkips(agents []store.Agent, pending map[string][]store.Message) {
	for _, a := range agents {
		if len(pending[a.ID]) == 0 {
			s.clearSkip(a.ID)
		}
	}
}

// WakeSkipReason returns the recorded reason and since-time (unix) agent's mail is waiting,
// or "" when it is not.
func WakeSkipReason(st *store.Store, agent string) (reason string, since int64) {
	v, _, _ := st.GetKV(kvWakeSkip + agent)
	f := strings.SplitN(v, "|", 2)
	if f[0] == "" {
		return "", 0
	}
	if len(f) == 2 {
		since, _ = strconv.ParseInt(f[1], 10, 64)
	}
	return f[0], since
}

// Wake limits: a wake line costs the agent a billable turn, so an agent that keeps
// ignoring the same unread items is not poked forever.
//
// A wake counts as ignored only when it was actually typed (wakeSent, not merely
// considered) and the agent ended a turn (a Stop hook) after the previous wake while
// the same items stayed unread. A wake that lands while the agent is still in a turn
// (or queued behind one) is not an answer yet, so it neither counts nor is repeated
// until the agent stops or WakeBusyHold passes.
const (
	MaxUnansweredWakes = 5
	GiveUpAfter        = 10 * time.Minute // span of counted wakes before "may be stuck"
	WakeBusyHold       = 5 * time.Minute  // do not re-wake an agent that has not ended a turn since
	WakeRearmEvery     = 10 * time.Minute // after MaxUnansweredWakes: one more wake this often, never a permanent hold
	AskUserPatience    = 15 * time.Minute
	kvWakeTries        = "wake.tries." // + agent: "<oldest pending id>|<counted wakes>|<first at>|<last at>|<last stop event id>|<alerted>"
)

type wakeState struct {
	id, firstAt, lastAt, lastStop int64
	n                             int
	alerted                       bool
}

func oldestID(msgs []store.Message) int64 {
	first := msgs[0].ID
	for _, m := range msgs {
		if m.ID < first {
			first = m.ID
		}
	}
	return first
}

// loadWake returns the stored state for the agent, reset when the oldest unread
// message is a different one.
func (s *Supervisor) loadWake(agent string, first int64) wakeState {
	var w wakeState
	if f := strings.Split(s.kvGet(kvWakeTries+agent), "|"); len(f) == 6 {
		w.id, w.n = int64(atoi(f[0])), atoi(f[1])
		w.firstAt, w.lastAt, w.lastStop = int64(atoi(f[2])), int64(atoi(f[3])), int64(atoi(f[4]))
		w.alerted = f[5] == "1"
	}
	if w.id != first {
		w = wakeState{id: first}
	}
	return w
}

func (s *Supervisor) saveWake(agent string, w wakeState) {
	al := 0
	if w.alerted {
		al = 1
	}
	_ = s.Store.SetKV(kvWakeTries+agent, fmt.Sprintf("%d|%d|%d|%d|%d|%d", w.id, w.n, w.firstAt, w.lastAt, w.lastStop, al))
}

// lastStopID is the id of the agent's latest Stop hook event (0 if none).
func (s *Supervisor) lastStopID(agent string) int64 {
	evs, err := s.Store.EventsByKind(agent, "hook:stop", 1)
	if err != nil || len(evs) == 0 {
		return 0
	}
	return evs[0].ID
}

// wakeHeld reports whether a wake must not be sent now. It never changes the
// wake count; it only emits the "may be stuck" alert, once, when the agent has
// ignored MaxUnansweredWakes wakes over at least GiveUpAfter. A held-out agent is not
// locked out for good: it is woken again every WakeRearmEvery, and the count is dropped
// as soon as the agent makes a slai tool call after the last wake (it is alive and reading).
func (s *Supervisor) wakeHeld(ctx context.Context, a store.Agent, msgs []store.Message) bool {
	w := s.loadWake(a.ID, oldestID(msgs))
	if w.n == 0 {
		return false
	}
	now := s.now().Unix()
	if w.n >= MaxUnansweredWakes {
		if !w.alerted && now-w.firstAt >= int64(GiveUpAfter/time.Second) {
			w.alerted = true
			s.saveWake(a.ID, w)
			s.event(a.ID, "wake_gave_up", map[string]any{"message_id": w.id, "wakes": w.n})
			if s.mcpDead(a, w) {
				s.restartForMCP(ctx, a, w)
				return true
			}
			text := fmt.Sprintf("%s has ignored %d wake-ups over %d minutes for the same unread items (starting with message %d) and is idle. It may be stuck; check it or reassign its work.", a.ID, w.n, (now-w.firstAt)/60, w.id)
			if a.ID == s.orchestrator() {
				// nobody above it to tell: the user hears about it directly
				s.escalate(escalation{agent: a.ID, kind: EscWakeIgnored, user: true, text: text})
			} else {
				s.notify(s.orchestrator(), msgs[0].TaskID, text)
			}
		}
		if int64(atoi(s.kvGet(kvLastSeen+a.ID))) > w.lastAt {
			// it has made a slai tool call since the last wake: alive and reading, not locked out
			s.saveWake(a.ID, wakeState{id: w.id})
			_ = s.Store.ClearEscalation(a.ID, EscWakeIgnored)
			return false
		}
		return now-w.lastAt < int64(WakeRearmEvery/time.Second) // backoff: one wake per WakeRearmEvery
	}
	// Still in (or queued behind) the turn the last wake landed in.
	return s.lastStopID(a.ID) == w.lastStop && now-w.lastAt < int64(WakeBusyHold/time.Second)
}

// mcpDead recognises a dead slai MCP connection: the agent's hooks are firing (the
// session and Claude are alive and turning) yet it has made no slai tool call since
// the wakes began, so it cannot be reading its inbox.
func (s *Supervisor) mcpDead(a store.Agent, w wakeState) bool {
	hook, err := s.Store.LastHookEventAt(a.ID)
	if err != nil || hook == 0 || s.now().Sub(time.Unix(hook, 0)) > StuckAfter {
		return false
	}
	return int64(atoi(s.kvGet(kvLastSeen+a.ID))) < w.firstAt
}

// restartForMCP restarts the session so Claude reconnects its MCP servers, and
// tells the orchestrator why.
func (s *Supervisor) restartForMCP(ctx context.Context, a store.Agent, w wakeState) {
	cfg, ok := s.agentCfg(a.ID)
	if !ok {
		return
	}
	tasks, _ := s.Store.ListTasks("")
	cause := fmt.Sprintf("its slai MCP connection looks dead: %d wake-ups ignored while its hooks kept firing and it made no slai tool call", w.n)
	s.event(a.ID, "mcp_dead", map[string]any{"message_id": w.id, "wakes": w.n, "cause": cause})
	s.notify(s.orchestrator(), "", fmt.Sprintf("%s ignored %d wake-ups although it is active: its slai MCP connection looks dead (no slai tool call since the wakes began). slai is restarting its session so it reconnects.", a.ID, w.n))
	s.stuckRestart(ctx, a, cfg, tasks, s.lastActivity(a), cause)
}

// wakeSent records a wake that was actually typed into the agent's pane.
func (s *Supervisor) wakeSent(a store.Agent, msgs []store.Message) {
	w := s.loadWake(a.ID, oldestID(msgs))
	now, stop := s.now().Unix(), s.lastStopID(a.ID)
	if w.n == 0 || stop != w.lastStop {
		if w.n == 0 {
			w.firstAt = now
		}
		w.n++
	}
	w.lastAt, w.lastStop = now, stop
	s.saveWake(a.ID, w)
}

func hasKind(ms []store.Message, k string) bool {
	for _, m := range ms {
		if m.Kind == k {
			return true
		}
	}
	return false
}

// refreshPending re-reads what is owed to agent just before it is woken. The snapshot a
// wake pass starts from can be seconds old on a loaded machine (every tmux call is slow); an
// assignment created since then would otherwise be read by the agent's get_inbox before
// prepareAssignments has put its worktree on the task branch, and the agent would commit on
// the previous task's branch.
func (s *Supervisor) refreshPending(agent string, msgs []store.Message) []store.Message {
	all, err := s.Store.PendingWake(s.now().Add(-WakeRetry).Unix())
	if err != nil {
		return msgs
	}
	// Merge by id, not length: an older item may have been claimed while a new
	// assignment arrived, leaving a shorter fresh list that still needs preparing.
	out := append([]store.Message(nil), msgs...)
	seen := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		seen[m.ID] = true
	}
	for _, m := range all {
		if m.To == agent && !seen[m.ID] {
			out = append(out, m)
			seen[m.ID] = true
		}
	}
	return out
}

// TaskBranch is the conventional branch name for a task (PRD 3.5).
func TaskBranch(agent, taskID string) string { return fmt.Sprintf("slai/%s/%s", agent, taskID) }

// busyStatus: the engineer is mid-way through this task (the branch checked out in
// their worktree holds, or is about to hold, uncommitted work for it).
func busyStatus(st string) bool {
	return st == store.StatusAssigned || st == store.StatusInProgress || st == store.StatusChangesRequested
}

// taskWorktree is the engineer's worktree path.
func (s *Supervisor) taskWorktree(a store.Agent) string {
	if a.Worktree != "" {
		return a.Worktree
	}
	return filepath.Join(s.Dir, ".slai", "worktrees", a.ID)
}

func taskBranchOf(a store.Agent, t store.Task) string {
	if t.Branch != "" {
		return t.Branch
	}
	return TaskBranch(a.ID, t.ID)
}

// onceKey reports true the first time key is seen (for one-shot events and notes).
func (s *Supervisor) onceKey(key string) bool {
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	if s.rt.wakeKeys == nil {
		s.rt.wakeKeys = map[string]bool{}
	}
	seen := s.rt.wakeKeys[key]
	s.rt.wakeKeys[key] = true
	return !seen
}

// switchWorktree puts the engineer's worktree on task's branch and makes it their
// current_task (the board shows the task whose branch is checked out).
//
// The auto-stash below is ONLY for leftovers of a finished task. Callers must have
// established that the engineer's current task is not in progress: uncommitted work
// of a task the engineer is still doing must never be stashed.
func (s *Supervisor) switchWorktree(ctx context.Context, a store.Agent, task store.Task) error {
	branch := taskBranchOf(a, task)
	wt := s.taskWorktree(a)
	repo := &git.Repo{Dir: wt}
	// Leftover uncommitted files must not block new work forever: stash them
	// (recoverable with `git stash list` / `git stash pop`) and tell the engineer.
	if stashed, serr := repo.StashAll(ctx, "slai: auto-stash before "+task.ID); serr == nil && stashed {
		s.event(a.ID, "worktree_stashed", map[string]any{"task_id": task.ID, "worktree": wt})
		_, _ = s.Store.AddMessage(store.Message{From: "slai", To: a.ID, Kind: "note", TaskID: task.ID,
			Body: "Your worktree had uncommitted changes from earlier work, so slai stashed them (git stash list) before switching to " + branch + ". Run `git stash pop` only if you need them back."})
	}
	if _, err := repo.PrepareTaskBranch(ctx, branch, s.Team.Project.IntegrationBranch); err != nil {
		return err
	}
	// Refs are shared by every worktree, so the branch must now be visible from the main
	// checkout too (the service reads it there). If it is not, the switch did not happen
	// and nothing is reported.
	if !(&git.Repo{Dir: s.Dir}).BranchExists(ctx, branch) {
		return fmt.Errorf("branch %s is not visible in the main repository after the switch", branch)
	}
	if task.Branch == "" {
		_ = s.Store.UpdateTaskFields(task.ID, store.Task{Branch: branch})
	}
	_ = s.Store.SetAgentTask(a.ID, task.ID)
	return nil
}

// prepareAssignments puts an engineer's worktree on the task branch for fresh
// assignments. It returns false (and records a wake_error event once) when a switch failed.
//
// An assignment that is queued does not touch the worktree or current_task: the task
// still waits on dependencies, or the engineer is mid-way through another task. The
// note still reaches them; worktreeTick switches once the task is startable and the
// current one has gone to review.
func (s *Supervisor) prepareAssignments(ctx context.Context, a store.Agent, msgs []store.Message) bool {
	return len(s.prepareAssignmentsHeld(ctx, a, msgs)) == 0
}

// SwitchEscalateAfter is how long a worktree switch may keep failing before the user is told.
const SwitchEscalateAfter = 5 * time.Minute

// EscSwitchFailed is the user-visible escalation for an assignment that could not be handed over.
const EscSwitchFailed = "switch_failed"

// EscWakeIgnored is raised for the user when the orchestrator itself ignores its wake-ups.
const EscWakeIgnored = "wake_ignored"

const kvPrepFail = "wake.prepfail." // + agent: unix time its worktree switch first failed

// prepareAssignmentsHeld is prepareAssignments returning the ids of the assignments that could
// not be handed over: the one whose switch failed and every later fresh assignment. Those are
// held back from the agent's get_inbox (it would start them on the wrong branch) and retried
// on the next tick; the agent's other mail is not affected. After SwitchEscalateAfter of
// failing the user gets an escalation.
func (s *Supervisor) prepareAssignmentsHeld(ctx context.Context, a store.Agent, msgs []store.Message) (held []int64) {
	cfg, isCfg := s.agentCfg(a.ID)
	engineer := isCfg && cfg.Role == config.RoleEngineer
	cur := a.CurrentTask
	if fresh, err := s.Store.AgentByID(a.ID); err == nil {
		cur = fresh.CurrentTask
	}
	var failedTask store.Task
	var failedErr error
	for _, m := range msgs {
		if m.Kind != "assignment" || m.TaskID == "" || m.WokenAt != 0 {
			continue
		}
		if failedErr != nil { // after a failed switch nothing later can be handed over either
			_ = s.Store.HoldInbox(m.ID)
			held = append(held, m.ID)
			continue
		}
		task, err := s.Store.GetTask(m.TaskID)
		if err != nil {
			_ = s.Store.SetAgentTask(a.ID, m.TaskID)
			_ = s.Store.ReleaseInbox(m.ID)
			continue
		}
		if !engineer {
			_ = s.Store.SetAgentTask(a.ID, m.TaskID)
			_ = s.Store.ReleaseInbox(m.ID)
			continue
		}
		if waiting, _ := s.Store.WaitingOn(task.ID); len(waiting) > 0 || task.Status == store.StatusBlocked {
			_ = s.Store.ReleaseInbox(m.ID)
			continue // queued notice: do not start until dependencies clear
		}
		if cur != "" && cur != task.ID {
			if ct, err := s.Store.GetTask(cur); err == nil && busyStatus(ct.Status) {
				_ = s.Store.ReleaseInbox(m.ID)
				continue // queued notice: worktreeTick switches later
			}
		}
		if err := s.switchWorktree(ctx, a, task); err != nil {
			s.reportSwitchError(a, task, m.ID, err)
			failedTask, failedErr = task, err
			_ = s.Store.HoldInbox(m.ID)
			held = append(held, m.ID)
			continue
		}
		_ = s.Store.ReleaseInbox(m.ID)
		cur = task.ID
	}
	if failedErr == nil {
		if s.kvGet(kvPrepFail+a.ID) != "" {
			_ = s.Store.DeleteKV(kvPrepFail + a.ID)
			_ = s.Store.ClearEscalation(a.ID, EscSwitchFailed)
		}
		return nil
	}
	since := int64(atoi(s.kvGet(kvPrepFail + a.ID)))
	if since == 0 {
		since = s.now().Unix()
		_ = s.Store.SetKV(kvPrepFail+a.ID, strconv.FormatInt(since, 10))
	}
	if s.now().Unix()-since >= int64(SwitchEscalateAfter/time.Second) {
		s.escalate(escalation{agent: a.ID, kind: EscSwitchFailed, user: true, taskID: failedTask.ID,
			text: fmt.Sprintf("slai has failed for %d min to put %s's worktree on %s (%v); that assignment is held back and retried every tick, the rest of its mail is delivered.", (s.now().Unix()-since)/60, a.ID, failedTask.ID, failedErr)})
	}
	return held
}

// withoutHeld drops the messages held back from get_inbox.
func (s *Supervisor) withoutHeld(msgs []store.Message) []store.Message {
	var out []store.Message
	for _, m := range msgs {
		if !s.Store.InboxHeld(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// withoutIDs drops the messages with the given ids.
func withoutIDs(msgs []store.Message, ids []int64) []store.Message {
	if len(ids) == 0 {
		return msgs
	}
	skip := map[int64]bool{}
	for _, id := range ids {
		skip[id] = true
	}
	var out []store.Message
	for _, m := range msgs {
		if !skip[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

func (s *Supervisor) reportSwitchError(a store.Agent, task store.Task, msgID int64, err error) {
	if s.onceKey(fmt.Sprintf("wakeerr:%d:%s", msgID, task.ID)) {
		s.event(a.ID, "wake_error", map[string]any{"task_id": task.ID, "message_id": msgID, "error": err.Error()})
		s.notify(s.orchestrator(), task.ID, fmt.Sprintf("slai could not hand %s to %s: %v. Fix it (or reassign with update_task).", task.ID, a.ID, err))
	}
}

// worktreeTick keeps each idle engineer's worktree on the right task branch:
//   - a task that came back with changes_requested is switched back to, if the
//     worktree is clean (dirty: nothing is stashed; the engineer is told which two
//     branches are in play);
//   - a queued task that is now startable (dependencies merged) is switched to once
//     the engineer's current task is no longer in progress (in review, merged, ...).
func (s *Supervisor) worktreeTick(ctx context.Context) {
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return
	}
	for _, a := range agents {
		if cfg, ok := s.agentCfg(a.ID); !ok || cfg.Role != config.RoleEngineer || !idleish(a.Status) {
			continue
		}
		s.reconcileWorktree(ctx, a, tasks)
	}
}

// switchNote tells the engineer their worktree moved. It names the previous task only when
// there is one: an empty "(previous task )" is noise.
func switchNote(task, from, to, prev string) string {
	msg := fmt.Sprintf("Task %s is startable now: slai switched your worktree from %s to %s", task, from, to)
	if prev != "" {
		msg += " (previous task " + prev + ")"
	}
	return msg + ". Start on it."
}

func (s *Supervisor) reconcileWorktree(ctx context.Context, a store.Agent, tasks []store.Task) {
	// GetAgents derives current_task from the tasks; the stored one is what slai last switched
	// to. A stored value that is empty (a restart that has not restored it yet) is not
	// "no current task": keep the derived one, so a task the engineer is already on, or is
	// about to be woken for, is never treated as a queued task to switch to.
	if raw, err := s.Store.AgentByID(a.ID); err == nil && raw.CurrentTask != "" {
		a.CurrentTask = raw.CurrentTask
	}
	repo := &git.Repo{Dir: s.taskWorktree(a)}
	here, err := repo.CurrentBranch(ctx)
	if err != nil {
		return
	}
	var cur *store.Task
	var mine []store.Task
	for _, t := range tasks {
		if t.Assignee != a.ID {
			continue
		}
		mine = append(mine, t)
		if t.ID == a.CurrentTask {
			t := t
			cur = &t
		}
	}
	// Highest priority first, then lowest id: the order get_board and the wake-ups use.
	sort.SliceStable(mine, func(i, j int) bool {
		if mine[i].Priority != mine[j].Priority {
			return mine[i].Priority > mine[j].Priority
		}
		return mine[i].ID < mine[j].ID
	})
	// request_changes on a task whose branch is not checked out.
	for _, t := range mine {
		if t.Status != store.StatusChangesRequested || taskBranchOf(a, t) == here {
			continue
		}
		dirty, derr := repo.IsDirty(ctx)
		if derr != nil {
			return
		}
		if dirty {
			if s.onceKey(fmt.Sprintf("cr-dirty:%s:%d", t.ID, t.ReviewRounds)) {
				s.notify(a.ID, t.ID, fmt.Sprintf("Review of %s asked for changes, but your worktree is on %s with uncommitted changes, so slai did not switch (and did not stash). Commit or discard them, then `git checkout %s` to address the review.", t.ID, here, taskBranchOf(a, t)))
			}
			return
		}
		if err := s.switchWorktree(ctx, a, t); err != nil {
			s.reportSwitchError(a, t, 0, err)
			return
		}
		s.notify(a.ID, t.ID, fmt.Sprintf("Review of %s asked for changes: slai switched your worktree from %s back to %s.", t.ID, here, taskBranchOf(a, t)))
		return
	}
	if cur != nil && busyStatus(cur.Status) {
		return // still on it: nothing to switch to yet
	}
	for _, t := range mine {
		if t.ID == a.CurrentTask || (t.Status != store.StatusAssigned && t.Status != store.StatusInProgress) {
			continue
		}
		if waiting, _ := s.Store.WaitingOn(t.ID); len(waiting) > 0 {
			continue
		}
		if taskBranchOf(a, t) == here {
			_ = s.Store.SetAgentTask(a.ID, t.ID) // already on it: only the board was behind; never stash
			return
		}
		prev := a.CurrentTask
		if err := s.switchWorktree(ctx, a, t); err != nil {
			s.reportSwitchError(a, t, 0, err)
			return
		}
		s.notify(a.ID, t.ID, switchNote(t.ID, here, taskBranchOf(a, t), prev))
		return
	}
}
