package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// Context reset (spec context-reset-and-liveness, Part A). A reset is a request in the kv
// store that the supervisor carries out when the agent is idle: restart the session with a
// fresh session id, re-ground it, and report. The CLI (`slai clear`), the orchestrator's
// clear_context tool and the automatic reset all queue the same request, so they share one
// code path and the same guards.
const (
	KVResetReq = "reset.req." // + agent: ResetReq, a reset waiting for its moment or being verified
	KVResetRes = "reset.res." // + agent: ResetRes, how the last request ended
	KVWhoami   = "whoami."    // + agent: unix time of its last whoami call (proof it re-grounded)
)

// Who asked for a reset.
const (
	TriggerUser = "user"
	TriggerOrch = "orch"
	TriggerAuto = "auto"
)

// ResetReq is a queued reset.
type ResetReq struct {
	Trigger   string `json:"trigger"`             // user | orch | auto
	Requester string `json:"requester,omitempty"` // agent to tell how it ended (the orchestrator, for clear_context)
	Reason    string `json:"reason,omitempty"`
	At        int64  `json:"at"` // when it was queued; identifies the request
	// Progress, owned by the supervisor:
	Deferred bool  `json:"deferred,omitempty"` // seen, but the agent is busy: it clears when idle
	NoteID   int64 `json:"note_id,omitempty"`  // the "record progress now" message, once sent
	ResetAt  int64 `json:"reset_at,omitempty"` // when the session was restarted; the agent is then being re-grounded
	Resent   bool  `json:"resent,omitempty"`   // the identity line was sent a second time
}

// ResetRes is how a request ended.
type ResetRes struct {
	Req    int64  `json:"req"`    // the request's At
	State  string `json:"state"`  // cleared | skipped | failed
	Detail string `json:"detail"` // the reason, for skipped and failed
	At     int64  `json:"at"`
}

// LoadReset returns the pending reset for agent.
func LoadReset(st *store.Store, agent string) (ResetReq, bool) {
	v, ok, _ := st.GetKV(KVResetReq + agent)
	var r ResetReq
	if !ok || json.Unmarshal([]byte(v), &r) != nil {
		return ResetReq{}, false
	}
	return r, true
}

// SaveReset stores r as agent's pending reset.
func SaveReset(st *store.Store, agent string, r ResetReq) error {
	b, _ := json.Marshal(r)
	return st.SetKV(KVResetReq+agent, string(b))
}

// LoadResetRes returns how agent's last reset request ended.
func LoadResetRes(st *store.Store, agent string) (ResetRes, bool) {
	v, ok, _ := st.GetKV(KVResetRes + agent)
	var r ResetRes
	if !ok || json.Unmarshal([]byte(v), &r) != nil {
		return ResetRes{}, false
	}
	return r, true
}

// QueueReset queues a reset for agent. It reports false, changing nothing, when one is
// already pending (asking twice is harmless). The returned request carries its At.
func QueueReset(st *store.Store, agent string, r ResetReq) (ResetReq, bool, error) {
	if cur, ok := LoadReset(st, agent); ok {
		return cur, false, nil
	}
	r.At = time.Now().UnixNano()
	_ = st.DeleteKV(KVResetRes + agent)
	return r, true, SaveReset(st, agent, r)
}

// ClearGuard is the reason a reset requested by trigger/requester for target must be
// refused, or "" when it may go ahead. It holds for every way of asking.
func ClearGuard(t *config.Team, st *store.Store, trigger, requester, target string) string {
	var role string
	for _, a := range t.Agents {
		if a.ID == target {
			role = a.Role
		}
	}
	switch {
	case role == "":
		return fmt.Sprintf("unknown agent %q", target)
	case trigger != TriggerUser && role == config.RoleOrchestrator:
		return "only the user can reset the orchestrator, with `slai clear " + target + "`"
	case trigger == TriggerOrch && requester == target:
		return "an agent may not clear its own context"
	case trigger == TriggerOrch && role == config.RolePrincipal:
		if tasks, err := st.ListTasks(store.StatusInReview); err == nil && len(tasks) > 0 {
			return fmt.Sprintf("%s is reviewing %s and cannot be cleared while a review is open", target, tasks[0].ID)
		}
	}
	return ""
}

type ClearContextArgs struct {
	Agent  string `json:"agent" jsonschema:"id of the agent whose context to clear"`
	Reason string `json:"reason,omitempty" jsonschema:"why (shown in the activity log)"`
}

// ClearContext is the orchestrator's clear_context tool. It queues the same reset as
// `slai clear`, with the same guards; the supervisor carries it out when the agent is idle
// and the orchestrator is told by message how it ended.
func (s *Service) ClearContext(ctx context.Context, caller string, a ClearContextArgs) (any, error) {
	if err := s.begin(caller, "clear_context", a); err != nil {
		return nil, err
	}
	if err := checkText("reason", a.Reason); err != nil {
		return nil, err
	}
	a.Agent = strings.TrimSpace(a.Agent)
	if why := ClearGuard(s.team(), s.Store, TriggerOrch, caller, a.Agent); why != "" {
		return nil, fmt.Errorf("denied: %s", why)
	}
	req, queued, err := QueueReset(s.Store, a.Agent, ResetReq{Trigger: TriggerOrch, Requester: caller, Reason: a.Reason})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"agent": a.Agent, "queued": queued, "requested_at": req.At}
	if queued {
		out["status"] = "queued"
		out["detail"] = "slai clears it when it is idle and tells you by message how it ended (cleared, skipped with the reason, or failed)"
	} else {
		out["status"] = "already_pending"
		out["detail"] = "a reset is already pending for " + a.Agent + "; you will be told when it ends"
	}
	return out, nil
}
