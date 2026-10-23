package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Context reset engine (spec context-reset-and-liveness, Part A, as amended). A request
// queued with service.QueueReset is carried out here, on the supervisor's tick:
//
//	queued -> (refused: skipped) | (busy: deferred, retried every tick) | restarted
//	restarted -> re-grounded: the agent calls whoami -> cleared
//	          -> no whoami in ResetResend: the identity line is sent once more
//	          -> still none after ResetGiveUp more: failed, the user is told ("needs you")
//
// A reset is a restart with a fresh session id (never a typed /clear): the old session id
// and the started marker are removed, so StartAgent launches a new conversation for any
// harness. What survives is everything slai keeps outside the conversation: role prompt,
// tools and model, the agent's tasks, branches and worktree, its inbox, team memory and
// spend.
var (
	ResetResend = 2 * time.Minute // no whoami this long after the restart: send the identity line again
	ResetGiveUp = 2 * time.Minute // ... and this much longer: give up and tell the user
)

// EscRegroundFailed is the user-facing escalation for an agent that did not call whoami
// after a reset, even after its identity line was sent twice.
const EscRegroundFailed = "reground_failed"

// recordProgressNote is what an agent that owes a task is told before it is cleared.
const recordProgressNote = "slai will clear your context when you are idle. Record progress now: `remember` what you learned, commit your work and update the task, so nothing is lost. Then stop; do not start anything new."

// resetTick advances every pending reset. It runs each tick and from `slai clear`.
func (s *Supervisor) resetTick(ctx context.Context) {
	paused, _ := s.Store.PausedAgents()
	for _, cfg := range s.Team.Agents {
		req, ok := service.LoadReset(s.Store, cfg.ID)
		if !ok {
			continue
		}
		if req.ResetAt != 0 {
			// a paused, stopped or attended agent cannot be woken, so waiting on it proves nothing
			if a, err := s.Store.AgentByID(cfg.ID); err == nil && s.holdOff(ctx, a, paused) {
				continue
			}
			s.regroundStep(ctx, cfg, req)
			continue
		}
		s.resetStep(ctx, cfg, req, paused)
	}
}

// finishReset records how a request ended, closes it and tells whoever asked.
func (s *Supervisor) finishReset(cfg config.Agent, req service.ResetReq, state, detail string) {
	b := service.ResetRes{Req: req.At, State: state, Detail: detail, At: s.now().Unix()}
	_ = s.Store.SetKV(service.KVResetRes+cfg.ID, jsonString(b))
	_ = s.Store.DeleteKV(service.KVResetReq + cfg.ID)
	s.event(cfg.ID, "context_reset_"+state, map[string]any{"agent": cfg.ID, "trigger": req.Trigger, "detail": detail})
	if req.Requester != "" {
		msg := "clear_context " + cfg.ID + ": " + state
		if detail != "" {
			msg += " (" + detail + ")"
		}
		s.notify(req.Requester, "", msg+".")
	}
}

// resetStep decides one queued reset: refuse it, wait for the agent to be idle, or restart.
func (s *Supervisor) resetStep(ctx context.Context, cfg config.Agent, req service.ResetReq, paused map[string]bool) {
	if why := service.ClearGuard(s.Team, s.Store, req.Trigger, req.Requester, cfg.ID); why != "" {
		s.finishReset(cfg, req, "skipped", why)
		return
	}
	if s.kvGet(kvStopped+cfg.ID) != "" {
		s.finishReset(cfg, req, "skipped", "it was stopped on purpose; start it first")
		return
	}
	tasks, _ := s.Store.ListTasks("")
	if why := s.owesUnrecordedWork(ctx, cfg, tasks); why != "" {
		s.finishReset(cfg, req, "skipped", why)
		return
	}
	a, err := s.Store.AgentByID(cfg.ID)
	live := err == nil && s.sessionLive(ctx, a)
	if live && owesTask(cfg, tasks) && req.NoteID == 0 {
		// has committed progress and a clean tree, so nothing is lost: still ask it to record
		// what it knows, then clear once it has gone idle again
		id, err := s.Store.AddMessage(store.Message{From: "slai", To: cfg.ID, Kind: "note", Body: recordProgressNote})
		if err != nil {
			s.logf("reset %s: %v", cfg.ID, err)
			return
		}
		req.NoteID, req.Deferred = id, true
		_ = service.SaveReset(s.Store, cfg.ID, req)
		return
	}
	if live && !s.idleForReset(ctx, a, req, paused) {
		if !req.Deferred {
			req.Deferred = true
			_ = service.SaveReset(s.Store, cfg.ID, req)
		}
		return
	}
	s.executeReset(ctx, cfg, a, live, req)
}

// owesTask reports whether cfg is the assignee of a task it is expected to be working on.
func owesTask(cfg config.Agent, tasks []store.Task) bool {
	for _, t := range tasks {
		if t.Assignee == cfg.ID && activeTask(t) {
			return true
		}
	}
	return false
}

func activeTask(t store.Task) bool {
	return t.Status == store.StatusAssigned || t.Status == store.StatusInProgress || t.Status == store.StatusChangesRequested
}

// owesUnrecordedWork is the A3 refusal: the agent is the assignee of an active task and
// either its worktree has uncommitted changes or its task branch has no commit yet. A
// reset would throw away work that exists nowhere else but the conversation. Only agents
// with their own worktree can be checked; "" means go ahead.
func (s *Supervisor) owesUnrecordedWork(ctx context.Context, cfg config.Agent, tasks []store.Task) string {
	if cfg.Role != config.RoleEngineer {
		return ""
	}
	wt := filepath.Join(s.Dir, ".slai", "worktrees", cfg.ID)
	if _, err := os.Stat(wt); err != nil {
		return ""
	}
	for _, t := range tasks {
		if t.Assignee != cfg.ID || !activeTask(t) {
			continue
		}
		if dirty, err := (&git.Repo{Dir: wt}).IsDirty(ctx); err == nil && dirty {
			return fmt.Sprintf("%s has uncommitted changes in its worktree for %s; it must commit first", cfg.ID, t.ID)
		}
		has := false
		if t.Branch != "" {
			has, _ = s.Git.HasCommits(ctx, s.Team.Project.IntegrationBranch, t.Branch)
		}
		if !has {
			return fmt.Sprintf("%s has no commit on its branch for %s since it was assigned; it must record progress first", cfg.ID, t.ID)
		}
	}
	return ""
}

// idleForReset is the A2 test: not mid-turn, no wake-up in flight, nobody typing into its
// pane, and (when slai asked it to record progress) that note has been read.
func (s *Supervisor) idleForReset(ctx context.Context, a store.Agent, req service.ResetReq, paused map[string]bool) bool {
	if a.Status != "idle" && a.Status != "finished" {
		return false
	}
	if s.holdOff(ctx, a, paused) {
		return false
	}
	msgs, _ := s.Store.UndeliveredFor(a.ID)
	for _, m := range msgs {
		if m.ID == req.NoteID && req.NoteID != 0 {
			return false // not read yet: the agent has not recorded progress
		}
		if m.WokenAt != 0 && s.now().Sub(time.Unix(m.WokenAt, 0)) < WakeRetry {
			return false // a wake-up is in flight
		}
	}
	return true
}

// executeReset restarts the agent in a fresh session and starts the re-ground.
func (s *Supervisor) executeReset(ctx context.Context, cfg config.Agent, a store.Agent, live bool, req service.ResetReq) {
	before := s.Store.LastPromptTokens(cfg.ID)
	_ = s.Store.SetKV(hooks.KVResetMark+cfg.ID, jsonString(hooks.ResetMark{Trigger: req.Trigger, Reason: req.Reason, TokensBefore: before}))
	if live {
		if err := s.killSession(ctx, cfg.ID, a.TmuxSession, "context reset ("+req.Trigger+")"); err != nil {
			_ = s.Store.DeleteKV(hooks.KVResetMark + cfg.ID)
			s.finishReset(cfg, req, "failed", "could not stop its session: "+err.Error())
			return
		}
	}
	// a fresh conversation: no session id to resume and no started marker
	_ = os.Remove(harness.SessionIDPath(s.Dir, cfg.ID))
	_ = os.Remove(filepath.Join(claude.RunDir(s.Dir, cfg.ID), ".started"))
	_ = s.Store.DeleteKV(service.KVWhoami + cfg.ID)
	started, err := s.StartAgent(ctx, cfg)
	if err != nil || !started {
		_ = s.Store.DeleteKV(hooks.KVResetMark + cfg.ID)
		detail := "its session did not start"
		if err != nil {
			detail = "its session did not start: " + err.Error()
		}
		s.finishReset(cfg, req, "failed", detail)
		return
	}
	_ = s.Store.SetKV(kvRestarted+cfg.ID, "1")
	if cfg.Role == config.RoleOrchestrator {
		// the orchestrator lost the plan with the conversation: hand it the board
		s.notify(cfg.ID, "", s.boardSummary())
	}
	s.restartEvent(cfg.ID, "context_reset_restart", "context reset ("+req.Trigger+"): fresh session, re-grounding", map[string]any{"trigger": req.Trigger, "tokens_before": before})
	req.ResetAt, req.Deferred = s.now().Unix(), false
	_ = service.SaveReset(s.Store, cfg.ID, req)
}

// boardSummary lists every open task with its owner and status.
func (s *Supervisor) boardSummary() string {
	tasks, _ := s.Store.ListTasks("")
	var lines []string
	for _, t := range tasks {
		if openTask(t) {
			lines = append(lines, fmt.Sprintf("- %s [%s] %s: %s", t.ID, t.Status, t.Assignee, t.Title))
		}
	}
	if len(lines) == 0 {
		return "Your context was reset. The board has no open tasks. Call get_board to confirm."
	}
	return "Your context was reset. Open tasks on the board:\n" + strings.Join(lines, "\n") + "\nCall get_board and get_inbox, then decide the next action for each task."
}

// regroundStep waits for the restarted agent to call whoami, re-sending the identity line
// once and then giving up loudly.
func (s *Supervisor) regroundStep(ctx context.Context, cfg config.Agent, req service.ResetReq) {
	if at, _ := strconv.ParseInt(s.kvGet(service.KVWhoami+cfg.ID), 10, 64); at >= req.ResetAt && at != 0 {
		s.finishReset(cfg, req, "cleared", "")
		return
	}
	waited := s.now().Sub(time.Unix(req.ResetAt, 0))
	switch {
	case !req.Resent && waited >= ResetResend:
		req.Resent = true
		_ = service.SaveReset(s.Store, cfg.ID, req)
		if line := roles.IdentityLineFor(s.Team, cfg.ID); line != "" {
			_ = s.Store.SetKV(hooks.KVIdentityPending+cfg.ID, "reset")
			_, _ = s.Store.AddMessage(store.Message{From: "slai", To: cfg.ID, Kind: "note",
				Body: line + " Your context was reset: call whoami now, then get_inbox and get_task to pick up where you were."})
		}
		s.event(cfg.ID, "context_reset_reground_resent", map[string]any{"agent": cfg.ID})
	case req.Resent && waited >= ResetResend+ResetGiveUp:
		s.escalate(escalation{agent: cfg.ID, kind: EscRegroundFailed, user: true,
			text: "did not re-ground after reset", data: map[string]any{"trigger": req.Trigger}})
		s.finishReset(cfg, req, "failed", "did not re-ground after reset (it never called whoami); needs you")
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
