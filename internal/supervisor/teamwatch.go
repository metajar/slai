package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// Hot reload: a running supervisor notices edits to .slai/team.yaml and brings the
// live team in line, without ever cutting off an agent that is doing something.
const (
	EscTeamInvalid        = "team_config_invalid"  // team.yaml no longer parses/validates; the old team stays
	EscTeamRemovalBlocked = "team_removal_blocked" // an agent removed from team.yaml is busy, so it was left running

	// EscTeamStalled: tasks are open, nobody is working and nothing has moved for TeamStalledAfter.
	// The user is told once per episode, and at most once per TeamStalledRepeat while it lasts.
	EscTeamStalled = "team_stalled"

	kvAnnounced   = "team.announced."   // + agent: "1" once the orchestrator was told about it
	kvStalledAt   = "team.stalled.at"   // unix the user was last told the team is stalled
	kvStalledScan = "team.stalled.scan" // unix of the last stall check (it runs at most every StalledScanEvery)
)

// Team stall notice (spec B5).
var (
	TeamStalledAfter  = 20 * time.Minute
	TeamStalledRepeat = time.Hour
	StalledScanEvery  = 30 * time.Second
)

// TeamRetry paces re-attempts of a reload that had to leave something undone.
var TeamRetry = 2 * time.Second

type teamWatch struct {
	seen    bool
	size    int64
	mtime   time.Time
	pending bool // part of the last change is not applied yet (busy agent)
	lastTry time.Time
	lastErr string
}

func (s *Supervisor) teamPath() string { return filepath.Join(s.Dir, ".slai", "team.yaml") }

// teamTick runs every tick: one stat when nothing changed.
func (s *Supervisor) teamTick(ctx context.Context) {
	if _, held := s.CurrentHold(); held {
		return // no session is started, stopped or restarted during a team hold; the edit is picked up after `slai resume`
	}
	s.teamStalledTick()
	tw := &s.tw
	if !tw.seen {
		for _, a := range s.Team.Agents { // the team we started with needs no welcome
			if s.kvGet(kvAnnounced+a.ID) == "" {
				_ = s.Store.SetKV(kvAnnounced+a.ID, "1")
			}
		}
	}
	fi, err := os.Stat(s.teamPath())
	if err != nil {
		return
	}
	if !tw.seen || fi.Size() != tw.size || !fi.ModTime().Equal(tw.mtime) {
		tw.seen, tw.size, tw.mtime = true, fi.Size(), fi.ModTime()
		s.loadTeamFile()
	}
	if tw.pending && s.now().Sub(tw.lastTry) >= TeamRetry {
		tw.lastTry = s.now()
		s.reconcileSafe(ctx)
	}
	s.announceAdded(ctx)
}

// loadTeamFile adopts the file as the new team when it is valid and different.
func (s *Supervisor) loadTeamFile() {
	data, err := os.ReadFile(s.teamPath())
	if err != nil {
		return
	}
	t, err := config.Parse(data)
	if err != nil {
		if msg := err.Error(); msg != s.tw.lastErr {
			s.tw.lastErr = msg
			s.logf("team.yaml changed but is invalid; keeping the previous team: %v", err)
		}
		s.escalate(escalation{agent: "team", kind: EscTeamInvalid, user: true,
			text: "team.yaml is invalid, so slai keeps running the previous team: " + err.Error()})
		return
	}
	s.tw.lastErr = ""
	_ = s.Store.ClearEscalation("team", EscTeamInvalid)
	if sameTeam(t, s.Team) {
		return
	}
	s.Team = t
	s.tw.pending = true
	s.tw.lastTry = time.Time{}
}

func sameTeam(a, b *config.Team) bool {
	x, err1 := config.Marshal(a)
	y, err2 := config.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// reconcileSafe is Reconcile restricted to what cannot hurt: it starts agents that
// were added, and only stops or restarts an agent that is idle/finished/dead (and,
// for a removal, owes no open task). What it leaves stays pending and is retried.
func (s *Supervisor) reconcileSafe(ctx context.Context) {
	plan, err := s.planReconcile(ctx)
	if err != nil {
		s.logf("team reload: %v", err)
		return
	}
	rows := map[string]store.Agent{}
	if as, err := s.Store.GetAgents(); err == nil {
		for _, a := range as {
			rows[a.ID] = a
		}
	}
	paused, _ := s.Store.PausedAgents()
	tasks, _ := s.Store.ListTasks("")
	safe := Plan{Adopt: plan.Adopt}
	pending := false
	blocked := map[string]bool{}
	for _, id := range plan.Stop {
		a, ok := rows[id]
		if !ok || s.removable(a, tasks) {
			safe.Stop = append(safe.Stop, id)
			continue
		}
		pending, blocked[id] = true, true
		s.escalate(escalation{agent: id, kind: EscTeamRemovalBlocked, user: true,
			text: fmt.Sprintf("%s was removed from team.yaml but is %s with open work, so it was left running; it will be stopped once it is idle with nothing assigned", id, a.Status)})
	}
	for _, id := range plan.Restart {
		if a, ok := rows[id]; ok && s.sessionLive(ctx, a) && !s.upgradeSafe(ctx, a, paused) {
			pending = true // its config changed; restart when it next goes idle
			continue
		}
		safe.Restart = append(safe.Restart, id)
	}
	for _, id := range plan.Start {
		if _, known := rows[id]; !known { // a dead known agent is auto-revive's, not ours
			safe.Start = append(safe.Start, id)
		}
	}
	if err := s.applyReconcile(ctx, safe); err != nil {
		s.logf("team reload: %v", err)
		pending = true
	}
	if escs, err := s.Store.OpenEscalations(); err == nil {
		for _, e := range escs {
			if e.Kind == EscTeamRemovalBlocked && !blocked[e.Agent] {
				_ = s.Store.ClearEscalation(e.Agent, e.Kind)
			}
		}
	}
	s.tw.pending = pending
}

func (s *Supervisor) removable(a store.Agent, tasks []store.Task) bool {
	switch a.Status {
	case "idle", "finished", "dead":
	default:
		return false
	}
	for _, t := range tasks {
		if t.Assignee != a.ID {
			continue
		}
		switch t.Status {
		case store.StatusMerged, store.StatusDone, store.StatusCancelled, store.StatusReverted:
		default:
			return false
		}
	}
	return true
}

// announceAdded tells the orchestrator once about each agent that joined the team
// while it runs (whoever started it: this loop or `slai init --edit`).
func (s *Supervisor) announceAdded(ctx context.Context) {
	for _, cfg := range s.Team.Agents {
		if s.kvGet(kvAnnounced+cfg.ID) != "" {
			continue
		}
		a, err := s.Store.AgentByID(cfg.ID)
		if err != nil || !s.sessionLive(ctx, a) {
			continue
		}
		_ = s.Store.SetKV(kvAnnounced+cfg.ID, "1")
		s.event(cfg.ID, "agent_added", map[string]any{"role": cfg.Role, "focus": cfg.Focus})
		if orch := s.orchestrator(); orch != "" && orch != cfg.ID {
			focus := cfg.Focus
			if focus == "" {
				focus = "none"
			}
			s.notify(orch, "", fmt.Sprintf("New teammate %s (%s, focus: %s) joined. Assign it with create_task.", cfg.ID, cfg.Role, focus))
		}
	}
}

// teamStalledTick tells the user when the whole team has gone quiet: tasks are open, no agent
// is working (or waiting on the user, or rate limited, which have their own notices) and
// nothing has moved for TeamStalledAfter. The notice is a user escalation, so it reaches the
// dashboard banner, the web inbox and the notify command (event "escalation"), names the stalled
// tasks and what slai already tried, and is not repeated within TeamStalledRepeat. It never
// wakes or types to anyone.
func (s *Supervisor) teamStalledTick() {
	if !s.Driver || s.Paused() {
		return
	}
	now := s.now()
	if last := int64(atoi(s.kvGet(kvStalledScan))); last != 0 && now.Sub(time.Unix(last, 0)) < StalledScanEvery {
		return
	}
	_ = s.Store.SetKV(kvStalledScan, strconv.FormatInt(now.Unix(), 10))
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return
	}
	var open []store.Task
	moved := int64(0)
	for _, t := range tasks {
		if t.UpdatedAt > moved {
			moved = t.UpdatedAt
		}
		if openTask(t) {
			open = append(open, t)
		}
	}
	for _, a := range agents {
		switch a.Status {
		case "working", "waiting_on_user", "rate_limited":
			open = nil // somebody is busy, or the user or the rate limit is what we wait for
		}
		// Only what the agent itself did counts: a hook or a slai tool call. Its status_at is
		// moved by slai (the optimistic "working" after a typed wake-up), which a stall's own
		// reminders would use to keep the team looking alive forever.
		if at, err := s.Store.LastHookEventAt(a.ID); err == nil && at > moved {
			moved = at
		}
		if seen := int64(atoi(s.kvGet(kvLastSeen + a.ID))); seen > moved {
			moved = seen
		}
	}
	// Likewise only agents' messages: slai's reminders and board checks keep arriving for the
	// whole stall, and a manual `slai send` is already counted as a manual nudge.
	if ms, err := s.Store.RecentMessages(200); err == nil {
		for _, m := range ms { // newest first
			if m.From == "slai" || m.From == "user" {
				continue
			}
			if m.CreatedAt > moved {
				moved = m.CreatedAt
			}
			break
		}
	}
	if len(open) == 0 || now.Sub(time.Unix(moved, 0)) < TeamStalledAfter {
		_ = s.Store.ClearEscalation("team", EscTeamStalled)
		return
	}
	text := s.stalledText(now.Sub(time.Unix(moved, 0)), open)
	if s.escalate(escalation{agent: "team", kind: EscTeamStalled, user: true, text: text}) {
		_ = s.Store.SetKV(kvStalledAt, strconv.FormatInt(now.Unix(), 10))
		return
	}
	// still stalled and already told: repeat at most once per TeamStalledRepeat
	if at := int64(atoi(s.kvGet(kvStalledAt))); at != 0 && now.Sub(time.Unix(at, 0)) >= TeamStalledRepeat {
		_ = s.Store.ClearEscalation("team", EscTeamStalled)
		if s.escalate(escalation{agent: "team", kind: EscTeamStalled, user: true, text: text, data: map[string]any{"repeat": true}}) {
			_ = s.Store.SetKV(kvStalledAt, strconv.FormatInt(now.Unix(), 10))
		}
	}
}

// stalledText names the stalled tasks and what slai already tried for each: why its owner's
// mail is waiting (H-1's wake.skip reason), how many reminders it got, and the board checks the
// orchestrator was sent.
func (s *Supervisor) stalledText(quiet time.Duration, open []store.Task) string {
	const show = 5
	var b strings.Builder
	fmt.Fprintf(&b, "nothing has moved for %s: %d open task(s) and no agent is working.", quiet.Round(time.Minute), len(open))
	for i, t := range open {
		if i == show {
			fmt.Fprintf(&b, " ... and %d more.", len(open)-show)
			break
		}
		fmt.Fprintf(&b, " %s [%s, %s]", t.ID, t.Status, t.Assignee)
		var tried []string
		if skip := strings.SplitN(s.kvGet(kvWakeSkip+t.Assignee), "|", 2); skip[0] != "" {
			tried = append(tried, "its mail is not being delivered ("+skip[0]+")")
		}
		if f := strings.Split(s.kvGet(kvNudge+t.Assignee), "|"); len(f) == 3 && atoi(f[1]) > 0 {
			tried = append(tried, fmt.Sprintf("reminded %dx", atoi(f[1])))
		}
		if len(tried) == 0 {
			tried = append(tried, "no reminder sent yet")
		}
		b.WriteString(": " + strings.Join(tried, ", ") + ".")
	}
	if orch := s.orchestrator(); orch != "" {
		if n := atoi(s.kvGet(kvIgnored + orch)); n > 0 {
			fmt.Fprintf(&b, " The orchestrator was sent %d board check(s) without acting.", n)
		}
	}
	b.WriteString(" Run `slai status`, or `slai send orch \"...\"`.")
	return b.String()
}

// RecordManualNudge records a manual_nudge event when the user's `slai send` goes to an agent
// that is idle, owes work (or unanswered asks) and has been silent for NudgeIdleAfter: the
// wake-up, reminder and board-check machinery should have moved it without help, so each of
// these is counted (target: zero per week, see `slai status --stats`). An answer to an agent
// parked on ask_user, or a send to a paused agent, is not one. It reports whether it recorded.
func (s *Supervisor) RecordManualNudge(agent string) bool {
	cfg, ok := s.agentCfg(agent)
	if !ok {
		return false
	}
	a, err := s.Store.AgentByID(agent)
	if err != nil || !idleish(a.Status) || s.idleFor(a) < NudgeIdleAfter {
		return false
	}
	if paused, _ := s.Store.PausedAgents(); paused[agent] {
		return false
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return false
	}
	owed := s.owedAll(cfg, tasks)
	if len(owed) == 0 {
		return false
	}
	ids := make([]string, 0, len(owed))
	for _, t := range owed {
		ids = append(ids, t.ID)
	}
	s.event(agent, "manual_nudge", map[string]any{"owed": ids, "idle_seconds": int(s.idleFor(a).Seconds())})
	return true
}
