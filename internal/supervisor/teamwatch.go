package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// Hot reload: a running supervisor notices edits to .slai/team.yaml and brings the
// live team in line, without ever cutting off an agent that is doing something.
const (
	EscTeamInvalid        = "team_config_invalid"  // team.yaml no longer parses/validates; the old team stays
	EscTeamRemovalBlocked = "team_removal_blocked" // an agent removed from team.yaml is busy, so it was left running

	kvAnnounced = "team.announced." // + agent: "1" once the orchestrator was told about it
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
