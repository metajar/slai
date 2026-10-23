package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/store"
)

// B3: the orchestrator owns outcomes. A board check that lists a task whose owner is not busy and
// that has had no activity for StalledTaskAfter asks the orchestrator to record a decision on it.
// A decision is any update_task, cancel_task or create_task naming the task, a send_message to
// its owner, or an ask_user. With none within DecisionWithin the user is told.
const (
	StalledTaskAfter = 15 * time.Minute
	DecisionWithin   = 10 * time.Minute
	// EscNoDecision: the orchestrator ignored a board check about stalled tasks.
	EscNoDecision = "board_check_ignored"

	kvDecisions = "drive.decisions" // JSON map task id -> pendingDecision
)

type pendingDecision struct {
	Owner string `json:"owner"`
	Since int64  `json:"since"` // unix, when the board check named it
	After int64  `json:"after"` // last event id then: only later orchestrator events count
}

func (s *Supervisor) loadDecisions() map[string]pendingDecision {
	m := map[string]pendingDecision{}
	_ = json.Unmarshal([]byte(s.kvGet(kvDecisions)), &m)
	return m
}

func (s *Supervisor) saveDecisions(m map[string]pendingDecision) {
	if len(m) == 0 {
		_ = s.Store.DeleteKV(kvDecisions)
		return
	}
	b, _ := json.Marshal(m)
	_ = s.Store.SetKV(kvDecisions, string(b))
}

// healthyBusy: the owner is mid-turn or active with work owed; its task needs no decision.
func healthyBusy(l health.Liveness) bool { return l == health.Busy || l == health.Alive }

// ownerLiveness is the classifier's view of the task's assignee (Idle when it has no row).
func (s *Supervisor) ownerLiveness(ctx context.Context, byID map[string]store.Agent, owner string) health.Liveness {
	a, ok := byID[owner]
	if !ok {
		return health.Idle
	}
	return s.livenessOf(ctx, a)
}

// quietFor is how long a task has seen no activity: the later of its last change and its
// owner's last sign of life.
func (s *Supervisor) quietFor(t store.Task, byID map[string]store.Agent) time.Duration {
	last := t.UpdatedAt
	if a, ok := byID[t.Assignee]; ok {
		last = max(last, s.lastActivity(a))
	}
	if last == 0 {
		return 0
	}
	return s.now().Sub(time.Unix(last, 0))
}

// stalledTask: assigned or in progress, quiet for StalledTaskAfter.
func (s *Supervisor) stalledTask(t store.Task, byID map[string]store.Agent) bool {
	return (t.Status == store.StatusAssigned || t.Status == store.StatusInProgress) && s.quietFor(t, byID) >= StalledTaskAfter
}

func (s *Supervisor) lastEventID() int64 {
	if evs, err := s.Store.RecentEvents(1); err == nil && len(evs) > 0 {
		return evs[0].ID
	}
	return 0
}

// expectDecisions records that the board check just sent named these stalled tasks.
func (s *Supervisor) expectDecisions(tasks []store.Task) {
	if len(tasks) == 0 {
		return
	}
	m := s.loadDecisions()
	after := s.lastEventID()
	for _, t := range tasks {
		if _, ok := m[t.ID]; !ok {
			m[t.ID] = pendingDecision{Owner: t.Assignee, Since: s.now().Unix(), After: after}
		}
	}
	s.saveDecisions(m)
}

// decisionVia says whether orchestrator event e records a decision about task id owned by owner.
func decisionVia(e store.Event, id, owner string) string {
	switch e.Kind {
	case "update_task":
		if strings.Contains(e.Data, `"`+id+`"`) {
			return "updated"
		}
	case "cancel_task":
		if strings.Contains(e.Data, `"`+id+`"`) {
			return "cancelled"
		}
	case "create_task":
		if strings.Contains(e.Data, id) {
			return "split"
		}
	case "send_message":
		if strings.Contains(e.Data, `"to":"`+owner+`"`) {
			return "messaged"
		}
	case "ask_user":
		return "asked-user"
	}
	return ""
}

// decisionTick settles the tasks the last board check named: a recorded decision, or progress
// on the task, clears it; an unanswered one is escalated to the user after DecisionWithin.
func (s *Supervisor) decisionTick(tasks []store.Task) {
	orch := s.orchestrator()
	m := s.loadDecisions()
	if len(m) == 0 || orch == "" {
		return
	}
	byID := map[string]store.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var open []string
	var oldest int64
	for _, id := range ids {
		p := m[id]
		t, ok := byID[id]
		if !ok || (t.Status != store.StatusAssigned && t.Status != store.StatusInProgress) || t.UpdatedAt > p.Since {
			delete(m, id) // closed, moved on, or someone changed it since: nothing left to decide
			continue
		}
		evs, _ := s.Store.EventsAfter(p.After, "", 500)
		decided := ""
		for _, e := range evs {
			if e.Agent == orch {
				if decided = decisionVia(e, id, p.Owner); decided != "" {
					break
				}
			}
		}
		if decided != "" {
			s.event(orch, "board_decision", map[string]any{"task_id": id, "owner": p.Owner, "decision": decided})
			delete(m, id)
			continue
		}
		open = append(open, id)
		if oldest == 0 || p.Since < oldest {
			oldest = p.Since
		}
	}
	s.saveDecisions(m)
	if len(m) == 0 {
		_ = s.Store.ClearEscalation(orch, EscNoDecision)
		return
	}
	if s.now().Unix()-oldest >= int64(DecisionWithin/time.Second) {
		s.escalate(escalation{agent: orch, kind: EscNoDecision, user: true,
			text: fmt.Sprintf("the orchestrator has not recorded a decision on %s within %d min of the board check naming %s (no update_task, cancel_task, create_task, message to the owner or ask_user). The owner is not busy and the task has been quiet for %d+ min: look at it.",
				plural(len(open), "stalled task"), int(DecisionWithin/time.Minute), strings.Join(open, ", "), int(StalledTaskAfter/time.Minute))})
	}
}
