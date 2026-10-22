package supervisor

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// Stuck-agent self-healing. An agent whose status is "working" but that has had no
// hook event or slai tool call for StuckAfter, with a pane that has stopped
// changing, is not working: typically the wake line was typed into its input box
// but never submitted, so no UserPromptSubmit/Stop hook fired and the status never
// left "working". Steps, no human involved:
//
//	(a) clear the input box (Escape, C-u) and type the wake line again, pausing
//	    before Enter;
//	(b) if still silent StuckRestartAfter later, kill the session and let
//	    autoRevive start a clean one (respecting kvStopped and ReviveMax).
//
// The orchestrator is told only when neither step can help.
var (
	StuckAfter        = 3 * time.Minute  // silence before an agent counts as stuck
	StuckPaneGap      = 10 * time.Second // the pane must stay unchanged this long
	StuckRestartAfter = 60 * time.Second // wait after the nudge before restarting
)

// StuckKeyPause separates the clearing keys from the retyped line (var for tests).
var StuckKeyPause = 300 * time.Millisecond

const (
	kvStuck    = "stuck."    // + agent: "<stage>|<at unix>|<activity baseline>"
	kvLastSeen = "lastseen." // + agent: unix time of its last slai tool call (set by the service)

	stuckNudged    = 1 // step (a) done
	stuckRestarted = 2 // step (b) done; waiting for the new session to show activity
	stuckFailed    = 3 // neither step can help; orchestrator told
)

type paneSeen struct {
	hash string
	at   time.Time
}

type stuckState struct {
	stage    int
	at, base int64
}

func (s *Supervisor) loadStuck(agent string) stuckState {
	var st stuckState
	if f := strings.Split(s.kvGet(kvStuck+agent), "|"); len(f) == 3 {
		st.stage = atoi(f[0])
		st.at, _ = strconv.ParseInt(f[1], 10, 64)
		st.base, _ = strconv.ParseInt(f[2], 10, 64)
	}
	return st
}

func (s *Supervisor) saveStuck(agent string, st stuckState) {
	_ = s.Store.SetKV(kvStuck+agent, fmt.Sprintf("%d|%d|%d", st.stage, st.at, st.base))
}

// lastActivity is the latest sign of life: the status change that marked it
// working, a hook event, or a slai tool call. Supervisor-written events (wake,
// nudge, ...) deliberately do not count.
func (s *Supervisor) lastActivity(a store.Agent) int64 {
	last := a.StatusAt
	if at, err := s.Store.LastHookEventAt(a.ID); err == nil && at > last {
		last = at
	}
	if seen := int64(atoi(s.kvGet(kvLastSeen + a.ID))); seen > last {
		last = seen
	}
	return last
}

// paneUnsent reports whether the pane's last lines hold unsent slai text.
func paneUnsent(pane string) bool {
	lines := strings.Split(strings.TrimRight(pane, "\n "), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	tail := strings.Join(lines, "\n")
	return strings.Contains(tail, "[slai]") && !strings.Contains(tail, "echo: [slai]")
}

func (s *Supervisor) stuckTick(ctx context.Context, agents []store.Agent, tasks []store.Task) {
	held, _ := s.Store.PausedAgents()
	now := s.now()
	for _, a := range agents {
		if a.Status != "working" || held[a.ID] || s.kvGet(kvStopped+a.ID) != "" {
			continue
		}
		cfg, ok := s.agentCfg(a.ID)
		if !ok {
			continue
		}
		base := s.lastActivity(a)
		st := s.loadStuck(a.ID)
		if st.stage != 0 && base > st.base {
			s.event(a.ID, "stuck_recovered", map[string]any{"stage": st.stage})
			_ = s.Store.DeleteKV(kvStuck + a.ID)
			st = stuckState{}
		}
		if st.stage == stuckFailed || st.stage == stuckRestarted {
			continue // restart in flight, or already escalated: wait for activity
		}
		silent := now.Sub(time.Unix(base, 0))
		if silent < StuckAfter {
			s.clearPane(a.ID)
			continue
		}
		if live, err := s.Tmux.HasSession(ctx, a.TmuxSession); err != nil || !live {
			continue
		}
		if s.Store.Focused(a.ID, now) {
			continue
		}
		if n, err := s.Tmux.ClientCount(ctx, a.TmuxSession); err != nil || n > 0 {
			continue // someone is attached and may be typing
		}
		pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
		if err != nil {
			continue
		}
		if !s.paneIdle(a.ID, pane, now) {
			continue
		}
		switch st.stage {
		case 0:
			if s.toolInFlight(a.ID, now) {
				continue // a long tool call is running: quiet, not stuck
			}
			s.stuckNudge(ctx, a, pane, silent, base)
		case stuckNudged:
			if now.Sub(time.Unix(st.at, 0)) >= StuckRestartAfter {
				s.stuckRestart(ctx, a, cfg, tasks, base, fmt.Sprintf("no hook activity for %s and the pane did not change; the nudge %s ago did not help",
					silent.Round(time.Second), now.Sub(time.Unix(st.at, 0)).Round(time.Second)))
			}
		}
	}
}

// ToolInFlightMax bounds how long a started-but-unfinished tool call excuses silence.
const ToolInFlightMax = 15 * time.Minute

// toolInFlight reports whether the newest PreToolUse hook is newer than the
// newest Stop (and, for harnesses that send it, the newest PostToolUse) and younger
// than ToolInFlightMax: the agent is inside a tool call (a long build or test run),
// which is not a stuck turn. A turn that stalls after its last tool call finished is.
func (s *Supervisor) toolInFlight(id string, now time.Time) bool {
	pre, err := s.Store.EventsByKind(id, "hook:pretooluse", 1)
	if err != nil || len(pre) == 0 {
		return false
	}
	if now.Sub(time.Unix(pre[0].At, 0)) >= ToolInFlightMax {
		return false
	}
	if post, _ := s.Store.EventsByKind(id, "hook:posttooluse", 1); len(post) > 0 && post[0].ID > pre[0].ID {
		return false // the call finished (only harnesses that report tool_end send this; Claude's settings have no PostToolUse hook)
	}
	stop, _ := s.Store.EventsByKind(id, "hook:stop", 1)
	return len(stop) == 0 || pre[0].ID > stop[0].ID
}

func (s *Supervisor) clearPane(id string) {
	s.rt.mu.Lock()
	delete(s.rt.panes, id)
	s.rt.mu.Unlock()
}

// paneIdle reports whether the pane is unchanged since a poll at least
// StuckPaneGap ago (a spinner or streaming output keeps changing it).
func (s *Supervisor) paneIdle(id, pane string, now time.Time) bool {
	sum := sha1.Sum([]byte(pane))
	h := hex.EncodeToString(sum[:])
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	if s.rt.panes == nil {
		s.rt.panes = map[string]paneSeen{}
	}
	prev, ok := s.rt.panes[id]
	if !ok || prev.hash != h {
		s.rt.panes[id] = paneSeen{h, now}
		return false
	}
	return now.Sub(prev.at) >= StuckPaneGap
}

// stuckNudge is step (a): clear whatever sits in the input box and retype the wake line.
func (s *Supervisor) stuckNudge(ctx context.Context, a store.Agent, pane string, silent time.Duration, base int64) {
	if !s.paneSafe(a, pane) {
		return
	}
	unsent := paneUnsent(pane)
	s.event(a.ID, "stuck_detected", map[string]any{"silent_s": int(silent.Seconds()), "unsent_input": unsent})
	n := 1
	if msgs, err := s.Store.UndeliveredFor(a.ID); err == nil && len(msgs) > 0 {
		n = len(msgs)
	}
	line := fmt.Sprintf("[slai] You have %d new items. Call get_inbox.", n)
	err := s.retype(ctx, a, line)
	if err != nil {
		s.logf("stuck nudge %s: %v", a.ID, err)
	}
	s.event(a.ID, "stuck_nudge", map[string]any{"unsent_input": unsent, "text": line, "error": errStr(err)})
	s.saveStuck(a.ID, stuckState{stage: stuckNudged, at: s.now().Unix(), base: base})
	s.clearPane(a.ID)
}

// stuckRestart is step (b): kill the session so autoRevive starts a clean one.
func (s *Supervisor) stuckRestart(ctx context.Context, a store.Agent, cfg config.Agent, tasks []store.Task, base int64, cause string) {
	now := s.now()
	msgs, _ := s.Store.UndeliveredFor(a.ID)
	if len(owedBy(cfg, tasks)) == 0 && len(msgs) == 0 {
		// nothing to resume: a restart would not be revived, so just release the status
		_ = s.Store.SetAgentStatus(a.ID, "idle")
		s.event(a.ID, "stuck_cleared", map[string]any{"reason": "no open work or unread messages"})
		_ = s.Store.DeleteKV(kvStuck + a.ID)
		return
	}
	recent := 0
	for _, f := range strings.Split(s.kvGet(kvRevive+a.ID), ",") {
		if ts, err := strconv.ParseInt(f, 10, 64); err == nil && now.Sub(time.Unix(ts, 0)) < ReviveWindow {
			recent++
		}
	}
	if recent >= ReviveMax {
		s.stuckGiveUp(a, cfg, base, fmt.Sprintf("it was already restarted %d times in %s", recent, ReviveWindow))
		return
	}
	if err := s.killSession(ctx, a.ID, a.TmuxSession, cause); err != nil {
		s.stuckGiveUp(a, cfg, base, "killing its session failed: "+err.Error())
		return
	}
	_ = s.Store.SetAgentStatus(a.ID, "dead") // autoRevive takes it from here
	s.restartEvent(a.ID, "stuck_restart", cause, map[string]any{"owed": len(owedBy(cfg, tasks)), "unread": len(msgs)})
	s.saveStuck(a.ID, stuckState{stage: stuckRestarted, at: now.Unix(), base: base})
}

// stuckGiveUp tells the orchestrator, once, that slai could not unstick an agent.
func (s *Supervisor) stuckGiveUp(a store.Agent, cfg config.Agent, base int64, why string) {
	s.saveStuck(a.ID, stuckState{stage: stuckFailed, at: s.now().Unix(), base: base})
	s.event(a.ID, "stuck_failed", map[string]any{"reason": why})
	s.escalate(escalation{agent: a.ID, kind: EscStuckGaveUp, user: true, taskID: a.CurrentTask,
		text:    "stuck: gave up (" + why + ")",
		orchMsg: fmt.Sprintf("%s shows as working but has been silent and its pane unchanged. slai cleared its input box and re-sent the wake-up, but that did not help, and it could not be restarted: %s. The user has been told. Check it (slai attach / the dashboard), or reassign its work with update_task.", a.ID, why)})
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
