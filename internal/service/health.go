package service

import (
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/store"
)

// Health is the truthful state of one agent, combining session state, agent
// status and every kind of activity (tool call, hook event, status change).
type Health struct {
	Liveness     string // alive | idle | busy | blocked | gated | stale | dead | rate_limited (health.All)
	Reason       string // short human string for anything not alive/idle
	LastActivity int64  // unix; 0 if never seen
}

// KVBackground + agent holds "<unix>|<what>": the supervisor saw the agent's pane
// footer count a background task or monitor at that time (Claude Code keeps working
// after the turn ends while one runs). It is believed for BackgroundFresh.
const KVBackground = "bgtask."

// BackgroundFresh is how long a background-task sighting counts without a new one.
const BackgroundFresh = 45 * time.Second

// StaleAfter is how long an agent that owes work may be silent before it reads stale.
const StaleAfter = health.StaleAfter

// InputOpts tunes CollectInputs.
type InputOpts struct {
	Cfg   GateConfig
	Gates *GateSource // optional pre-read gate inputs (a board builds one for all agents)
	// Ping, when set, folds the latest failed ping into the classification.
	Ping    func(lastActivity int64) *health.Ping
	Harness string
	// Agent, when set, is the caller's snapshot of the agent row: its status, status
	// time and role are used instead of reading them again. The supervisor classifies
	// the agents it listed at the start of a pass, so a status it changed mid-pass is
	// acted on next pass, not half-way through this one.
	Agent *store.Agent
}

// CollectInputs gathers everything health.Classify needs for id from the store. The
// board, doctor, the TUI and the supervisor all call it, so they classify the same
// facts. It returns the inputs and the unix time of the agent's last activity.
func CollectInputs(st *store.Store, id string, sessionAlive bool, now time.Time, o InputOpts) (health.Inputs, int64) {
	in := health.Inputs{SessionAlive: sessionAlive, Harness: o.Harness}
	last := lastSeenAt(st, id)
	role := ""
	row := o.Agent
	if row == nil {
		if a, err := st.AgentByID(id); err == nil {
			row = &a
		}
	}
	if row != nil {
		in.Status, role = row.Status, row.Role
		last = max(last, row.StatusAt)
	}
	if at, err := st.LastHookEventAt(id); err == nil {
		last = max(last, at)
	}
	in.HasActivity = last != 0
	in.Silent = now.Sub(time.Unix(last, 0))
	if n, err := st.UnreadCount(id); err == nil {
		in.Unread = n
	}
	if ts, err := st.ListTasks(""); err == nil {
		for _, t := range ts {
			// mirrors the supervisor's owedBy: the principal owes reviews
			if (t.Assignee == id && (t.Status == store.StatusAssigned || t.Status == store.StatusInProgress || t.Status == store.StatusChangesRequested)) ||
				(role == config.RolePrincipal && t.Status == store.StatusInReview) {
				in.Owed = "owes " + t.ID
				break
			}
		}
	}
	// Only an agent that owes something can be gated or stale; skip the gate reads otherwise.
	if in.Status == "working" || in.Unread > 0 || in.Owed != "" {
		gates := o.Gates
		if gates == nil {
			gates = NewGateSource(st, o.Cfg, now)
		}
		in.GateReason = gates.Agent(id).Reason(WakeGate{Paused: gates.global, TeamHold: gates.held})
	}
	if v, ok, _ := st.GetKV(KVBackground + id); ok {
		at, what, _ := strings.Cut(v, "|")
		if n, err := strconv.ParseInt(at, 10, 64); err == nil && now.Sub(time.Unix(n, 0)) < BackgroundFresh && what != "" {
			in.BackgroundTask = what
		}
	}
	if o.Ping != nil {
		in.Ping = o.Ping(last)
	}
	return in, last
}

// AgentHealth derives liveness for id. sessionAlive is whether its tmux
// session exists. The rules live in health.Classify.
func AgentHealth(st *store.Store, id string, sessionAlive bool, now time.Time) Health {
	return AgentHealthGated(st, id, sessionAlive, now, GateConfig{})
}

// AgentHealthGated is AgentHealth that also knows the team's gate settings. An
// agent that owes work but cannot be woken (paused, loop-guard hold, spend cap,
// no free concurrency slot) is `gated` with that reason, not `stale`.
func AgentHealthGated(st *store.Store, id string, sessionAlive bool, now time.Time, cfg GateConfig) Health {
	return agentHealth(st, id, sessionAlive, now, InputOpts{Cfg: cfg})
}

// AgentHealthWith is AgentHealthGated with the gate inputs pre-read (see InputOpts):
// what the board and the supervisor call, so they classify the same facts.
func AgentHealthWith(st *store.Store, id string, sessionAlive bool, now time.Time, o InputOpts) Health {
	return agentHealth(st, id, sessionAlive, now, o)
}

// agentHealth is the shared path: gather, classify.
func agentHealth(st *store.Store, id string, sessionAlive bool, now time.Time, o InputOpts) Health {
	in, last := CollectInputs(st, id, sessionAlive, now, o)
	l, why := health.Classify(in)
	return Health{Liveness: string(l), Reason: why, LastActivity: last}
}
