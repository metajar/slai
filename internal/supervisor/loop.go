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
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
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
	if err != nil || len(pending) == 0 {
		return
	}
	byAgent := map[string][]store.Message{}
	for _, m := range pending {
		byAgent[m.To] = append(byAgent[m.To], m)
	}
	paused, _ := s.Store.PausedAgents()
	gate := s.checkSpend()
	working := 0
	for _, a := range agents {
		if a.Status == "working" {
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
		msgs := byAgent[a.ID]
		if len(msgs) == 0 || paused[a.ID] || gate.blocked(a.ID) {
			continue
		}
		answer := a.Status == "waiting_on_user" && hasKind(msgs, "user")
		// An agent parked on ask_user must not hold the team's other messages hostage
		// forever: after AskUserPatience it receives them too (and proceeds on defaults).
		if a.Status == "waiting_on_user" && s.idleFor(a) > AskUserPatience {
			answer = true
		}
		if !idleish(a.Status) && !answer {
			continue
		}
		if s.wakeHeld(ctx, a, msgs) {
			continue
		}
		if working >= s.Team.Defaults.MaxConcurrentAgents && !hasKind(msgs, "ping") { // a liveness ping must get through
			continue
		}
		if live, err := s.Tmux.HasSession(ctx, a.TmuxSession); err != nil || !live {
			continue
		}
		if s.Store.Focused(a.ID, s.now()) {
			continue // user is typing into this agent in the TUI
		}
		if n, err := s.Tmux.ClientCount(ctx, a.TmuxSession); err != nil || n > 0 {
			continue // user is attached; never type over them
		}
		msgs = s.refreshPending(a.ID, msgs)
		if !s.prepareAssignments(ctx, a, msgs) {
			continue
		}
		if !s.safeToType(ctx, a) {
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
// ignored MaxUnansweredWakes wakes over at least GiveUpAfter.
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
			s.notify(s.orchestrator(), msgs[0].TaskID, fmt.Sprintf("%s has ignored %d wake-ups over %d minutes for the same unread items (starting with message %d) and is idle. It may be stuck; check it or reassign its work.", a.ID, w.n, (now-w.firstAt)/60, w.id))
		}
		return true
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
	var fresh []store.Message
	for _, m := range all {
		if m.To == agent {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) < len(msgs) {
		return msgs // never wake for less than was already decided
	}
	return fresh
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
// assignments. Returns false (and records a wake_error event once) on failure.
//
// An assignment that is queued does not touch the worktree or current_task: the task
// still waits on dependencies, or the engineer is mid-way through another task. The
// note still reaches them; worktreeTick switches once the task is startable and the
// current one has gone to review.
func (s *Supervisor) prepareAssignments(ctx context.Context, a store.Agent, msgs []store.Message) bool {
	cfg, isCfg := s.agentCfg(a.ID)
	engineer := isCfg && cfg.Role == config.RoleEngineer
	cur := a.CurrentTask
	if fresh, err := s.Store.AgentByID(a.ID); err == nil {
		cur = fresh.CurrentTask
	}
	for _, m := range msgs {
		if m.Kind != "assignment" || m.TaskID == "" || m.WokenAt != 0 {
			continue
		}
		task, err := s.Store.GetTask(m.TaskID)
		if err != nil {
			_ = s.Store.SetAgentTask(a.ID, m.TaskID)
			continue
		}
		if !engineer {
			_ = s.Store.SetAgentTask(a.ID, m.TaskID)
			continue
		}
		if waiting, _ := s.Store.WaitingOn(task.ID); len(waiting) > 0 || task.Status == store.StatusBlocked {
			continue // queued behind its dependencies
		}
		if cur != "" && cur != task.ID {
			if ct, err := s.Store.GetTask(cur); err == nil && busyStatus(ct.Status) {
				continue // mid-way through another task: worktreeTick switches later
			}
		}
		if err := s.switchWorktree(ctx, a, task); err != nil {
			s.reportSwitchError(a, task, m.ID, err)
			return false
		}
		cur = task.ID
	}
	return true
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
