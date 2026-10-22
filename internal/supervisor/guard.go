package supervisor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/store"
)

// Lane guard registration (spec harnesses.md AC 10, decision 8c). Claude's guard is
// a PreToolUse hook written into settings.json. A plugin or extension is different:
// if it never loaded there is no guard at all, so a non-Claude agent has to prove it
// registered one. OpenCode's plugin sends a `guard_ready` event on its first session
// event; Pi's extension writes <run>/guard_ready after a successful session_start
// probe. Nothing within GuardReadyWithin of the launch means the agent is running
// unguarded.
//
//   - pi in a role that can write: pi has no permission layer, so the lane guard is its
//     only write control. The agent is held (paused: nothing is delivered to it) and
//     the user is told, never "warn and run". The hold lifts by itself once the guard
//     registers.
//   - opencode (it has a permission block) and read-only roles: warn the orchestrator
//     once and show the agent as degraded; nothing is held.
var GuardReadyWithin = 30 * time.Second

// EscGuardMissing: a plugin/extension agent has no registered lane guard.
const EscGuardMissing = "guard_missing"

const (
	kvLaunchedAt = "launched."   // + agent: unix time slai started its session
	kvGuardHeld  = "guard.held." // + agent: "1" while slai holds it for a missing guard
)

// canWrite reports whether a role can write files (the lane guard matters for it).
func canWrite(role string) bool {
	switch role {
	case config.RoleEngineer, config.RolePM, config.RolePrincipal:
		return true
	}
	return false
}

// guardReady reports whether the agent's guard registered since its latest launch.
func (s *Supervisor) guardReady(a store.Agent) bool {
	if _, err := os.Stat(harness.GuardReadyPath(s.Dir, a.ID)); err == nil {
		return true
	}
	launched := int64(atoi(s.kvGet(kvLaunchedAt + a.ID)))
	if evs, err := s.Store.EventsByKind(a.ID, "hook:guard_ready", 1); err == nil && len(evs) > 0 {
		return evs[0].At >= launched
	}
	return false
}

// guardAgent checks one agent's guard registration.
func (s *Supervisor) guardAgent(ctx context.Context, a store.Agent, cfg config.Agent) {
	h := s.harnessOfCfg(cfg)
	if h.Kind() == harness.Claude || !h.Features().LaneGuard || !canWrite(cfg.Role) {
		return
	}
	if s.kvGet(kvStopped+a.ID) != "" || !s.sessionLive(ctx, a) {
		return
	}
	launched := int64(atoi(s.kvGet(kvLaunchedAt + a.ID)))
	if launched == 0 || s.now().Sub(time.Unix(launched, 0)) < GuardReadyWithin || s.guardReady(a) {
		return
	}
	mandatory := h.Kind() == harness.Pi
	if mandatory {
		_ = s.Store.SetAgentPaused(a.ID, true)
		_ = s.Store.SetKV(kvGuardHeld+a.ID, "1")
	}
	why := fmt.Sprintf("%s agent %s has not registered its lane guard %s after launch", h.Kind(), a.ID, GuardReadyWithin)
	text := why + "; its writes are unconfined"
	if mandatory {
		text = why + ": held (nothing is delivered to it) until the guard registers; restart it, or check its extension in .slai/run/" + a.ID
	}
	s.escalate(escalation{agent: a.ID, kind: EscGuardMissing, user: mandatory, text: text,
		orchMsg: text + ". Do not rely on it for work that touches files until it recovers."})
}

// guardRecovered lifts the hold slai put on an agent for a missing guard.
func (s *Supervisor) guardRecovered(id string) {
	if s.kvGet(kvGuardHeld+id) != "" {
		_ = s.Store.SetAgentPaused(id, false)
		_ = s.Store.DeleteKV(kvGuardHeld + id)
	}
}
