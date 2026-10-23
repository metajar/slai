package supervisor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/usage"
)

// Automatic context reset (spec context-reset-and-liveness A7, A8). With
// defaults.context_reset.auto on, an agent that is idle, owes nothing and whose context
// (usage.Context, the prompt size of its latest turn) is over the threshold is queued for a
// reset with trigger "auto"; the reset engine (reset.go) then does it like any other.
// Off by default in v1: the status and dashboard hint show the size either way.

// AutoResetEvery is the least time between two automatic resets of one agent (loop guard).
var AutoResetEvery = 30 * time.Minute

const kvAutoResetAt = "autoreset.at." // + agent: unix time of the last automatic reset request

// autoResetTick queues an automatic reset for every agent that qualifies. It does nothing
// unless the team enabled auto.
func (s *Supervisor) autoResetTick(ctx context.Context) {
	cr := s.Team.Defaults.ContextReset
	if !cr.Auto || s.Paused() {
		return
	}
	if _, held := s.CurrentHold(); held {
		return
	}
	threshold := int64(s.Team.Defaults.ContextThreshold())
	tasks, _ := s.Store.ListTasks("")
	paused, _ := s.Store.PausedAgents()
	gate := s.checkSpend()
	for _, cfg := range s.Team.Agents {
		if cfg.Role == config.RoleOrchestrator {
			continue // resetting the orchestrator is the user's call, from the CLI (A11)
		}
		tokens, _, ok := usage.Context(s.Store, cfg.ID)
		if !ok || tokens <= threshold {
			continue
		}
		if why := s.autoResetBlocked(ctx, cfg, tasks, paused, gate); why != "" {
			continue
		}
		reason := fmt.Sprintf("context is %d tokens, over the %d threshold", tokens, threshold)
		if _, queued, err := service.QueueReset(s.Store, cfg.ID, service.ResetReq{Trigger: service.TriggerAuto, Reason: reason}); err != nil || !queued {
			continue
		}
		_ = s.Store.SetKV(kvAutoResetAt+cfg.ID, strconv.FormatInt(s.now().Unix(), 10))
		s.event(cfg.ID, "auto_reset_queued", map[string]any{"agent": cfg.ID, "tokens": tokens, "threshold": threshold})
	}
}

// autoResetBlocked says why an agent may not be reset automatically right now ("" = it may):
// it is not idle, owes work or has unread mail, is held, paused or attended, is spend
// capped, is performing a review, was reset automatically less than AutoResetEvery ago, or
// already has a reset waiting.
func (s *Supervisor) autoResetBlocked(ctx context.Context, cfg config.Agent, tasks []store.Task, paused map[string]bool, gate spendGate) string {
	a, err := s.Store.AgentByID(cfg.ID)
	switch {
	case err != nil || !s.sessionLive(ctx, a):
		return "no live session"
	case !idleish(a.Status):
		return "not idle"
	case len(owedBy(cfg, tasks)) > 0:
		return "owes a task"
	case paused[cfg.ID] || s.holdOff(ctx, a, paused):
		return "held, paused or attended"
	case gate.blocked(cfg.ID):
		return "spend capped"
	case s.reviewing(a, tasks):
		return "performing a review"
	}
	if msgs, _ := s.Store.UndeliveredFor(cfg.ID); len(msgs) > 0 {
		return "unread inbox"
	}
	if at, err := strconv.ParseInt(s.kvGet(kvAutoResetAt+cfg.ID), 10, 64); err == nil && s.now().Sub(time.Unix(at, 0)) < AutoResetEvery {
		return "reset automatically less than " + AutoResetEvery.String() + " ago"
	}
	if _, pending := service.LoadReset(s.Store, cfg.ID); pending {
		return "a reset is already pending"
	}
	return ""
}

// reviewing reports whether the agent is in the middle of a review: its current task (set
// by review_diff) is in review, or it is the principal and any task is waiting for review.
func (s *Supervisor) reviewing(a store.Agent, tasks []store.Task) bool {
	for _, t := range tasks {
		if t.Status != store.StatusInReview {
			continue
		}
		if a.Role == config.RolePrincipal || (a.CurrentTask != "" && a.CurrentTask == t.ID) {
			return true
		}
	}
	return false
}
