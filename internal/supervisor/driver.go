package supervisor

import (
	"context"
	"fmt"
	"github.com/oorbt/slai/internal/health"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
)

// The driver is what makes a team autonomous. The wake loop only reacts to unread
// messages; the driver notices the quiet failure modes where work is owed but
// nothing is moving (an agent ended its turn with a task still open, a message
// was claimed but never acted on, a session died, the orchestrator is waiting on
// nobody) and re-engages the right agent, escalating when reminders don't help.
const (
	NudgeIdleAfter = 90 * time.Second // an owner must be idle this long before a reminder
	NudgeEvery     = 3 * time.Minute  // between reminders to the same agent
	MaxNudges      = 4                // reminders before escalating to the orchestrator
	OrchIdleAfter  = 60 * time.Second // orchestrator idle time before a board digest
	DigestEvery    = 4 * time.Minute  // between board digests while nothing is moving
	ReviveMax      = 3                // automatic revives per ReviveWindow
	ReviveWindow   = 10 * time.Minute
	IgnoredDigests = 3 // digests the orchestrator can ignore before the user is told
)

const (
	kvNudge   = "drive.nudge."   // + agent: "<signature>|<count>|<last unix>"
	kvDigest  = "drive.digest."  // + orchestrator: "<last unix>"
	kvSettled = "drive.settled." // + orchestrator: merged-task count already announced
	kvRevive  = "drive.revive."  // + agent: comma-separated unix times
	kvStopped = "stopped."       // + agent: "1" when the user stopped it on purpose
	kvIgnored = "drive.ignored." // + orchestrator: digests sent since it last did anything
	kvCapSig  = "drive.capsig."  // + orchestrator: board signature when the digest ceiling was hit
	kvBoardAt = "drive.boardat"  // "<board signature>|<unix it last changed>"
)

func openTask(t store.Task) bool {
	return t.Status != store.StatusMerged && t.Status != store.StatusCancelled && t.Status != store.StatusDone && t.Status != store.StatusReverted
}

// owedBy lists tasks an agent is expected to be working on right now.
func owedBy(a config.Agent, tasks []store.Task) []store.Task {
	var out []store.Task
	for _, t := range tasks {
		switch t.Status {
		case store.StatusAssigned, store.StatusInProgress, store.StatusChangesRequested:
			if t.Assignee == a.ID {
				out = append(out, t)
			}
		case store.StatusInReview:
			if a.Role == config.RolePrincipal {
				out = append(out, t)
			}
		}
	}
	return out
}

// Owed replies (AC 21): an ask nobody replied to, or a ping nobody ponged, that
// is older than OwedReplyAfter counts as work the recipient owes, so it joins the
// reminder/escalation ladder. They are modeled as pseudo-tasks (id "msg-<n>") so
// the ladder's signature, wording and escalation reuse the task path; they age out
// after OwedReplyMax so ancient history is not nagged forever.
const (
	OwedReplyAfter = 5 * time.Minute
	OwedReplyMax   = time.Hour
	StatusAwaiting = "awaiting_reply"
)

func (s *Supervisor) owedMessages(a config.Agent) []store.Task {
	now := s.now()
	msgs, err := s.Store.OwedMessages(a.ID, now.Add(-OwedReplyMax).Unix(), now.Add(-OwedReplyAfter).Unix())
	if err != nil {
		return nil
	}
	var out []store.Task
	for _, m := range msgs {
		// Preserve the exact duration bounds even though message timestamps have
		// only second precision.
		age := now.Sub(time.Unix(m.CreatedAt, 0))
		if age < OwedReplyAfter || age > OwedReplyMax {
			continue
		}
		verb := "reply to the ask from " + m.From + ": " + firstLine(m.Body, 60)
		if m.Kind == "ping" {
			verb = "pong the liveness ping from " + m.From
		}
		out = append(out, store.Task{ID: fmt.Sprintf("msg-%d", m.ID), Title: verb + fmt.Sprintf(" (message_id=%d)", m.ID),
			Assignee: a.ID, Status: StatusAwaiting, UpdatedAt: m.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// owedAll is everything the agent owes: its tasks and its unanswered asks/pings.
func (s *Supervisor) owedAll(a config.Agent, tasks []store.Task) []store.Task {
	return append(owedBy(a, tasks), s.owedMessages(a)...)
}

// realTaskID is the task id for message TaskID fields ("" for a pseudo-task).
func onlyMessages(owed []store.Task) bool {
	for _, t := range owed {
		if t.Status != StatusAwaiting {
			return false
		}
	}
	return len(owed) > 0
}

func realTaskID(t store.Task) string {
	if strings.HasPrefix(t.ID, "msg-") {
		return ""
	}
	return t.ID
}

// taskLine describes a task for a nudge or digest. The age is how long the task row has not
// changed, not whether its owner is working: "no board change", never "idle".
func taskLine(t store.Task, now time.Time) string { return taskLineLive(t, now, "") }

// taskLineLive is taskLine with the owner's liveness (health.Classify) next to its name.
func taskLineLive(t store.Task, now time.Time, live health.Liveness) string {
	age := ""
	if t.UpdatedAt > 0 {
		age = ", no board change for " + now.Sub(time.Unix(t.UpdatedAt, 0)).Round(time.Minute).String()
	}
	who := t.Assignee
	if live != "" {
		who += " " + live.String()
	}
	return fmt.Sprintf("%s [%s] %s (%s%s)", t.ID, t.Status, t.Title, who, age)
}

func signature(ts []store.Task) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.ID+":"+t.Status+":"+t.Assignee)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// driveTick runs the driver once. It is a no-op unless Supervisor.Driver is set.
func (s *Supervisor) driveTick(ctx context.Context) {
	if !s.Driver || s.Paused() {
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
	held, _ := s.Store.PausedAgents()
	gate := s.checkSpend()
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	s.stuckTick(ctx, agents, tasks)
	s.autoRevive(ctx, agents, tasks)
	s.decisionTick(tasks)
	for _, cfg := range s.Team.Agents {
		a, ok := byID[cfg.ID]
		if !ok || held[a.ID] || gate.blocked(a.ID) || !s.reachable(ctx, a) {
			continue
		}
		switch cfg.Role {
		case config.RoleOrchestrator:
			s.driveOrchestrator(ctx, a, agents, tasks)
			if len(s.owedMessages(cfg)) > 0 { // asks the PM or others never got an answer to
				s.nudgeOwner(cfg, a, tasks)
			}
		case config.RoleFocusGroup:
			// personas own no tasks, but they owe replies to the asks sent to them
			s.nudgeOwner(cfg, a, tasks)
		default:
			s.nudgeOwner(cfg, a, tasks)
		}
	}
}

// reachable: the agent is idle (not working or waiting on a human), its session is
// alive, nobody is typing into it, and it has nothing unread that the wake loop
// will already deliver.
func (s *Supervisor) reachable(ctx context.Context, a store.Agent) bool {
	if !idleish(a.Status) {
		return false
	}
	// Reminders are for an agent that is idle with work owed. One the classifier calls
	// busy (waiting on a background task or monitor), gated, rate limited or blocked is
	// not ignoring anyone: leave it alone.
	switch s.livenessOf(ctx, a) {
	case health.Busy, health.Gated, health.RateLimited, health.Blocked, health.Dead:
		return false
	}
	if live, err := s.Tmux.HasSession(ctx, a.TmuxSession); err != nil || !live {
		return false
	}
	if s.userPresent(ctx, a) { // focused in the TUI or a client attached
		return false
	}
	if msgs, err := s.Store.UndeliveredFor(a.ID); err != nil || len(msgs) > 0 {
		return false
	}
	return true
}

func (s *Supervisor) idleFor(a store.Agent) time.Duration {
	if a.StatusAt == 0 {
		return 0
	}
	return s.now().Sub(time.Unix(a.StatusAt, 0))
}

// nudgeOwner reminds an idle agent of tasks it still owes, and tells the
// orchestrator when reminders are not getting anywhere.
func (s *Supervisor) nudgeOwner(cfg config.Agent, a store.Agent, tasks []store.Task) {
	owed := s.owedAll(cfg, tasks)
	if len(owed) == 0 || s.idleFor(a) < NudgeIdleAfter {
		return
	}
	sig := signature(owed)
	var (
		oldSig string
		count  int
		last   int64
	)
	if v, ok, _ := s.Store.GetKV(kvNudge + a.ID); ok {
		f := strings.SplitN(v, "|", 3)
		if len(f) == 3 {
			oldSig, count = f[0], atoi(f[1])
			last, _ = strconv.ParseInt(f[2], 10, 64)
		}
	}
	if oldSig != sig { // progress (or new work): start counting again
		count, last = 0, 0
	}
	now := s.now()
	if last != 0 && now.Sub(time.Unix(last, 0)) < NudgeEvery {
		return
	}
	count++
	_ = s.Store.SetKV(kvNudge+a.ID, fmt.Sprintf("%s|%d|%d", sig, count, now.Unix()))

	if count%(MaxNudges+1) == 0 {
		s.escalateStalled(cfg, owed)
		return
	}
	var lines []string
	for _, t := range owed {
		lines = append(lines, "- "+taskLine(t, now))
	}
	ask := reminderAsk(cfg.Role, onlyMessages(owed))
	body := fmt.Sprintf("Reminder from slai (%d of %d): you still have open work and have been idle.\n%s\n%s\nDon't wait to be asked again; act on this now.",
		count, MaxNudges, strings.Join(lines, "\n"), ask)
	_, _ = s.Store.AddMessage(store.Message{From: "slai", To: a.ID, Kind: "note", TaskID: realTaskID(owed[0]), Body: body})
	s.event(a.ID, "nudge", map[string]any{"count": count, "tasks": sig})
}

func (s *Supervisor) escalateStalled(cfg config.Agent, owed []store.Task) {
	now := s.now()
	var lines []string
	for _, t := range owed {
		lines = append(lines, "- "+taskLine(t, now))
	}
	if cfg.Role == config.RoleOrchestrator {
		// nobody above it to tell: the user hears about its unanswered asks directly
		s.escalate(escalation{agent: cfg.ID, kind: EscStalled, user: true, text: signature(owed),
			data: map[string]any{"tasks": signature(owed)}})
		return
	}
	s.event(cfg.ID, "stalled", map[string]any{"tasks": signature(owed)})
	_, _ = s.Store.RaiseEscalation(store.Escalation{Agent: cfg.ID, Kind: EscStalled, Text: signature(owed), Since: now.Unix()})
	for _, o := range s.Team.Agents {
		if o.Role != config.RoleOrchestrator {
			continue
		}
		_, _ = s.Store.AddMessage(store.Message{From: "slai", To: o.ID, Kind: "note", TaskID: realTaskID(owed[0]),
			Body: fmt.Sprintf("%s (%s) has not made progress after %d reminders:\n%s\nDecide now: message them with specifics, reassign with update_task, cancel_task, or ask_user if a human decision is needed. Do not just wait.",
				cfg.ID, cfg.Role, MaxNudges, strings.Join(lines, "\n"))})
	}
}

// driveOrchestrator keeps the hub engaged: while tasks are open and nothing is
// moving it gets a periodic board digest, and when the last task closes it is
// told to verify the result and report to the user.
func (s *Supervisor) driveOrchestrator(ctx context.Context, o store.Agent, agents []store.Agent, tasks []store.Task) {
	if s.idleFor(o) < OrchIdleAfter {
		return
	}
	now := s.now()
	var open, merged int
	var lines []string
	for _, t := range tasks {
		if t.Status == store.StatusMerged {
			merged++
		}
		if openTask(t) {
			open++
			lines = append(lines, "- "+taskLine(t, now))
		}
	}
	if open == 0 {
		if s.kvGet("run.done") != "" {
			return // the orchestrator already declared the request finished
		}
		done := atoi(s.kvGet(kvSettled + o.ID))
		if merged > done {
			_ = s.Store.SetKV(kvSettled+o.ID, strconv.Itoa(merged))
			_, _ = s.Store.AddMessage(store.Message{From: "slai", To: o.ID, Kind: "note",
				Body: fmt.Sprintf("Every task on the board is now merged or cancelled (%d merged). Before you tell the user it is done: check the result against their original request (read the merged files; have an engineer run the tests/build if you haven't seen that). If anything is missing, create follow-up tasks. If it is complete, call the `finish` tool with a short final summary (what was built, where it lives (branch slai/integration), how to run it, what you did not verify) and then say the same to the user, ending with: to see what they would get they run `slai commit --dry-run` (or `git diff <their branch>...slai/integration`), and to put it on their current branch they run `slai commit`, which previews the same plan and asks before applying (or `slai commit -b NAME` for a new branch). Never merge or commit into the user's branches yourself.", merged)})
			s.event(o.ID, "settled", map[string]any{"merged": merged})
		}
		return
	}
	// someone is actively working: the board is moving, leave the orchestrator alone
	// unless it has been stuck for a long time.
	sig := signature(tasks)
	changedAt := now
	if v := s.kvGet(kvBoardAt); v != "" {
		if f := strings.SplitN(v, "|", 2); len(f) == 2 && f[0] == sig {
			if ts, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				changedAt = time.Unix(ts, 0)
			}
		}
	}
	_ = s.Store.SetKV(kvBoardAt, fmt.Sprintf("%s|%d", sig, changedAt.Unix()))
	busy := false
	for _, a := range agents {
		if a.ID != o.ID && (a.Status == "working") {
			busy = true
		}
	}
	stuckFor := now.Sub(changedAt)
	if busy && stuckFor < 10*time.Minute {
		return
	}
	if stuckFor < DigestEvery {
		return
	}
	// Digest ceiling: once IgnoredDigests have gone unanswered the user has been
	// told; more digests only burn turns. Resume when the board changes.
	if atoi(s.kvGet(kvIgnored+o.ID)) >= IgnoredDigests {
		if s.kvGet(kvCapSig+o.ID) == sig {
			return
		}
		_ = s.Store.SetKV(kvIgnored+o.ID, "0")
	}
	if last, _ := strconv.ParseInt(s.kvGet(kvDigest+o.ID), 10, 64); last != 0 && now.Sub(time.Unix(last, 0)) < DigestEvery {
		return
	}
	// Sort the open tasks by what the orchestrator can do about them. A task whose owner is
	// busy and healthy needs nothing; one waiting on a merge or on review is queued; the rest
	// need a decision. With nothing to decide there is no digest (it would cost a billable turn).
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	waiting, rest, why := waitingOnMerge(tasks)
	var need, busyOK, inReview, stalled []store.Task
	live := map[string]health.Liveness{}
	for _, t := range rest {
		l := s.ownerLiveness(ctx, byID, t.Assignee)
		live[t.ID] = l
		switch {
		case t.Status == store.StatusInReview:
			inReview = append(inReview, t)
		case healthyBusy(l):
			busyOK = append(busyOK, t)
		default:
			need = append(need, t)
			if s.stalledTask(t, byID) {
				stalled = append(stalled, t)
			}
		}
	}
	if len(need) == 0 {
		return
	}
	s.noteDigest(o, strconv.FormatInt(now.Unix(), 10), sig)
	lines = []string{fmt.Sprintf("Need a decision (%d; owner not busy):", len(need))}
	for _, t := range need {
		mark := ""
		if s.stalledTask(t, byID) {
			mark = " - NO ACTIVITY for " + s.quietFor(t, byID).Round(time.Minute).String()
		}
		lines = append(lines, "- "+taskLineLive(t, now, live[t.ID])+mark)
	}
	if len(busyOK) > 0 {
		lines = append(lines, fmt.Sprintf("In progress, no action needed (%d; owner busy):", len(busyOK)))
		for _, t := range busyOK {
			lines = append(lines, "- "+taskLineLive(t, now, live[t.ID]))
		}
	}
	if len(inReview) > 0 {
		lines = append(lines, fmt.Sprintf("In review, waiting on the principal (%d; no action needed):", len(inReview)))
		for _, t := range inReview {
			lines = append(lines, "- "+taskLineLive(t, now, live[t.ID]))
		}
	}
	if len(waiting) > 0 {
		// Tasks held only by an unmerged dependency are queued, not stuck: they start
		// by themselves (with a go-ahead note) when it merges.
		lines = append(lines, fmt.Sprintf("Waiting on a merge (%d; queued, not stuck: each gets a go-ahead note when its dependency merges, so push the dependency through review instead of pinging them):", len(waiting)))
		for _, t := range waiting {
			lines = append(lines, fmt.Sprintf("- %s: waits for %s", taskLine(t, now), why[t.ID]))
		}
	}
	ask := "For each task under \"Need a decision\" decide the next action now: unblock it (answer the question, send specifics to its owner), reassign it, split it, cancel it, or ask_user if a human decision is needed. Don't end your turn just waiting; and don't wait on the focus group or anyone else for more than a few minutes. Proceed with sensible defaults and say what you assumed."
	if len(stalled) > 0 {
		ask += fmt.Sprintf(" Tasks marked NO ACTIVITY need a recorded decision: slai counts an update_task, cancel_task or create_task on the task, a send_message to its owner, or an ask_user, and tells the user if there is none within %d minutes.", int(DecisionWithin/time.Minute))
	}
	body := fmt.Sprintf("Board check from slai: no board change for %s; %d open task(s), %d need a decision:\n%s\n%s",
		stuckFor.Round(time.Minute), open, len(need), strings.Join(lines, "\n"), ask)
	_, _ = s.Store.AddMessage(store.Message{From: "slai", To: o.ID, Kind: "note", Body: s.withOrchIdentity(o.ID, body)})
	s.expectDecisions(stalled)
	s.event(o.ID, "digest", map[string]any{"open": open})
}

// noteDigest records a digest sent to the orchestrator. If it did nothing since
// the previous one the ignored count grows, and at IgnoredDigests the user is
// told directly (the orchestrator cannot be the one to report itself).
func (s *Supervisor) noteDigest(o store.Agent, nowStr, boardSig string) {
	prev, _ := strconv.ParseInt(s.kvGet(kvDigest+o.ID), 10, 64)
	ignored := atoi(s.kvGet(kvIgnored + o.ID))
	switch {
	case prev == 0 || s.progressAfter(o.ID, prev):
		ignored = 0
	default:
		ignored++
	}
	_ = s.Store.SetKV(kvDigest+o.ID, nowStr)
	_ = s.Store.SetKV(kvIgnored+o.ID, strconv.Itoa(ignored))
	if ignored >= IgnoredDigests {
		_ = s.Store.SetKV(kvCapSig+o.ID, boardSig)
		s.escalate(escalation{agent: o.ID, kind: EscOrchDown, user: true,
			text: fmt.Sprintf("ORCH STUCK: ignored %d board digests in a row", ignored)})
	}
}

// autoRevive restarts a dead agent that still has work (or unread messages),
// unless the user stopped it on purpose or it keeps dying.
func (s *Supervisor) autoRevive(ctx context.Context, agents []store.Agent, tasks []store.Task) {
	now := s.now()
	for _, a := range agents {
		if s.livenessOf(ctx, a) != health.Dead || s.kvGet(kvStopped+a.ID) != "" {
			continue
		}
		cfg, ok := s.agentCfg(a.ID)
		if !ok {
			continue
		}
		msgs, _ := s.Store.UndeliveredFor(a.ID)
		if len(owedBy(cfg, tasks)) == 0 && len(msgs) == 0 {
			continue
		}
		var recent []string
		for _, f := range strings.Split(s.kvGet(kvRevive+a.ID), ",") {
			if ts, err := strconv.ParseInt(f, 10, 64); err == nil && now.Sub(time.Unix(ts, 0)) < ReviveWindow {
				recent = append(recent, f)
			}
		}
		if len(recent) >= ReviveMax {
			// keeps dying: stop retrying and let it show as dead, but say so once
			s.escalate(escalation{agent: a.ID, kind: EscReviveGaveUp, user: true, taskID: a.CurrentTask,
				text:    fmt.Sprintf("dead (revive gave up after %d)", ReviveMax),
				orchMsg: fmt.Sprintf("%s keeps dying: slai restarted it %d times in %s and has stopped. The user has been told. Reassign its work with update_task if it is urgent.", a.ID, ReviveMax, ReviveWindow)})
			continue
		}
		started, err := s.StartAgent(ctx, cfg)
		if err != nil {
			s.logf("auto-revive %s: %v", a.ID, err)
			continue
		}
		_ = s.Store.SetKV(kvRevive+a.ID, strings.Join(append(recent, strconv.FormatInt(now.Unix(), 10)), ","))
		if started {
			_ = s.Store.SetKV(kvRestarted+a.ID, "1")
			_ = s.Store.SetKV(hooks.KVRevived+a.ID, strconv.FormatInt(now.Unix(), 10))
			if len(msgs) == 0 {
				// nothing unread would wake it; the restarted agent still needs telling what happened
				_, _ = s.Store.AddMessage(store.Message{From: "slai", To: a.ID, Kind: "note",
					Body: "slai restarted your session after it died. Call get_task to see what you still owe and carry on."})
			}
		}
		s.restartEvent(a.ID, "auto_revive", fmt.Sprintf("its session died (%s) while it owed %s and had %s unread",
			deadHow(a), plural(len(owedBy(cfg, tasks)), "task"), plural(len(msgs), "message")),
			map[string]any{"owed": len(owedBy(cfg, tasks)), "unread": len(msgs)})
	}
}

// StopAgent stops one agent on purpose; the driver will not revive it until the
// user starts it again.
func (s *Supervisor) StopAgent(ctx context.Context, id string) error {
	cause := "stopped on request (slai stop / the TUI); it stays down until started again"
	if err := s.killSession(ctx, id, s.Session(id), cause); err != nil {
		return err
	}
	s.restartEvent(id, "agent_stopped", cause, nil)
	_ = s.Store.SetKV(kvStopped+id, "1")
	return s.Store.SetAgentStatus(id, "dead")
}

// deadHow says how long ago a dead agent was last seen alive, for revive causes.
func deadHow(a store.Agent) string {
	if a.StatusAt == 0 {
		return "no activity recorded"
	}
	return "last status change " + time.Since(time.Unix(a.StatusAt, 0)).Round(time.Second).String() + " ago"
}

func (s *Supervisor) kvGet(key string) string {
	v, _, _ := s.Store.GetKV(key)
	return v
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// reminderAsk is the closing instruction of a reminder. It names only tools the
// role actually has (roles.ToolNames), so it never sends an agent to a missing tool.
func reminderAsk(role string, onlyMsgs bool) string {
	switch {
	case onlyMsgs:
		return "Answer now: reply(message_id, body) to an ask; for a liveness ping run `echo $SLAI_AGENT_ID` and call pong(message_id, agent_id)."
	case role == config.RolePrincipal:
		return "These are waiting for your review. For each: call review_diff, then approve or request_changes with specific comments."
	case role == config.RolePM:
		return "Finish the work now: write the spec to the absolute path <project>/.slai/specs/<task-id>.md, then call report(done). If something blocks you, call report(blocked) saying exactly what."
	case role == config.RoleFocusGroup: // personas only answer asks
		return "Answer now: call get_inbox and reply(message_id, body) to the ask."
	case role == config.RoleOrchestrator:
		return "Continue now: check get_board and your inbox, then create_task, update_task or send_message as needed, or call finish when the request is complete."
	default:
		return "Continue now: call get_task, do the work in your worktree, commit it, then call request_review. If you are blocked, call report(blocked) saying exactly what you need. If you already finished, call request_review."
	}
}

// kvBoardChecks + orchestrator counts the board checks sent to it, for withOrchIdentity.
const kvBoardChecks = "drive.boardchecks."

// withOrchIdentity opens every Nth board check (defaults.identity_every_board_checks, default
// 10) with the orchestrator's identity line: forgetting who it is costs the most there (C7).
func (s *Supervisor) withOrchIdentity(orch, body string) string {
	every := s.Team.Defaults.IdentityEvery()
	if every == 0 {
		return body
	}
	n := atoi(s.kvGet(kvBoardChecks+orch)) + 1
	_ = s.Store.SetKV(kvBoardChecks+orch, strconv.Itoa(n))
	if n%every != 0 {
		return body
	}
	line := roles.IdentityLineFor(s.Team, orch)
	if line == "" {
		return body
	}
	s.event(orch, "identity_reground", map[string]any{"board_checks": n})
	return line + " " + body
}
