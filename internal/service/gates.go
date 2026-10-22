package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// KV keys the supervisor writes (internal/supervisor: KVPaused, KVHoldPrefix);
// read here without importing it.
const (
	kvGlobalPaused = "paused"
	kvTeamHold     = "team.hold" // JSON {"name","since"} while `slai hold` is in force
	kvHoldPrefix   = "hold.until."
)

// KVRunStart holds the unix time the current run started: the later of `slai up` and the
// last `finish`. per_run_usd counts spend from then.
const KVRunStart = "run.start"

// WakeGate says which team-wide conditions stop wake-ups from being delivered.
type WakeGate struct {
	Paused      bool `json:"paused"`       // global pause
	SpendCapped bool `json:"spend_capped"` // daily cap, or any agent's per-agent cap, reached
	LoopHold    bool `json:"loop_hold"`    // a loop-guard hold is active
	// TeamHold: `slai hold` is in force (Paused is also true). Sessions stay up; nothing
	// is woken, nudged or restarted until `slai resume`.
	TeamHold  bool   `json:"team_hold"`
	HoldName  string `json:"hold_name,omitempty"`
	HoldSince int64  `json:"hold_since,omitempty"` // unix
}

type teamHold struct {
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

func readTeamHold(st *store.Store) (teamHold, bool) {
	v, ok, _ := st.GetKV(kvTeamHold)
	var h teamHold
	if !ok || json.Unmarshal([]byte(v), &h) != nil || !globalPaused(st) {
		return h, false
	}
	return h, true
}

// GateConfig is the part of the team config the gates depend on (zero = unknown).
type GateConfig struct {
	MaxConcurrent int
	DailyUSD      float64
	PerAgentUSD   float64
	PerTaskUSD    float64
	PerRunUSD     float64
}

// GateConfigOf extracts the gate settings from a team (nil = zero).
func GateConfigOf(t *config.Team) GateConfig {
	if t == nil {
		return GateConfig{}
	}
	d := t.Defaults
	return GateConfig{d.MaxConcurrentAgents, d.Caps.DailyUSD, d.Caps.PerAgentUSD, d.Caps.PerTaskUSD, d.Caps.PerRunUSD}
}

// AgentGate is one agent's wake gate.
type AgentGate struct {
	Paused        bool  // paused by the user or a loop-guard hold (agents.paused)
	HeldUntil     int64 // unix end of a loop-guard hold; 0 if none
	QueuedForSlot bool  // unread items but max_concurrent_agents is full
	SpendCapped   bool  // the daily cap, the run cap or this agent's own cap is reached
	TaskCapped    bool  // a task this agent owns has reached per_task_usd
}

func globalPaused(st *store.Store) bool {
	v, ok, _ := st.GetKV(kvGlobalPaused)
	return ok && v == "1"
}

func heldUntil(st *store.Store, id string, now time.Time) int64 {
	v, ok, _ := st.GetKV(kvHoldPrefix + id)
	if !ok {
		return 0
	}
	until, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if until <= now.Unix() {
		return 0
	}
	return until
}

// GateSource holds the store state the wake gates are derived from, read once.
// Deriving the gate of every agent on the board from one source costs a fixed number
// of queries however many agents there are.
type GateSource struct {
	st     *store.Store
	cfg    GateConfig
	now    time.Time
	global bool
	hold   teamHold
	held   bool
	paused map[string]bool
	spent  float64            // today's total spend (read only when a daily cap is set)
	per    map[string]float64 // today's spend per agent (read only when a cap is set)
	status map[string]string  // agent statuses (read only when max_concurrent is set)
	work   int                // agents currently working
	runCap bool               // the per-run cap is reached
	capped map[string]bool    // assignees of open tasks at the per-task cap
}

// gateLoads counts GateSource loads (tests assert one per board call).
var gateLoads atomic.Int64

// NewGateSource reads the gate inputs.
func NewGateSource(st *store.Store, cfg GateConfig, now time.Time) *GateSource {
	gateLoads.Add(1)
	g := &GateSource{st: st, cfg: cfg, now: now, global: globalPaused(st)}
	g.hold, g.held = readTeamHold(st)
	g.paused, _ = st.PausedAgents()
	if cfg.DailyUSD > 0 {
		g.spent, _ = st.TodaySpend()
	}
	if cfg.DailyUSD > 0 || cfg.PerAgentUSD > 0 {
		g.per, _ = st.SpendByAgentToday()
	}
	if cfg.PerRunUSD > 0 {
		var base int64
		if v, ok, _ := st.GetKV(KVRunStart); ok {
			base, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
		run, _ := st.SpendSince(base)
		g.runCap = run >= cfg.PerRunUSD
	}
	if cfg.PerTaskUSD > 0 {
		g.capped = map[string]bool{}
		ts, _ := st.OpenTaskSpend()
		for _, v := range ts {
			if v.USD >= cfg.PerTaskUSD {
				g.capped[v.Assignee] = true
			}
		}
	}
	if cfg.MaxConcurrent > 0 {
		g.status = map[string]string{}
		if ags, err := st.GetAgents(); err == nil {
			for _, a := range ags {
				g.status[a.ID] = a.Status
				if a.Status == "working" {
					g.work++
				}
			}
		}
	}
	return g
}

// Wake derives the team-wide gate; ids are the agents to check for loop holds.
func (g *GateSource) Wake(ids []string) WakeGate {
	w := WakeGate{Paused: g.global, TeamHold: g.held, HoldName: g.hold.Name, HoldSince: g.hold.Since}
	if g.cfg.DailyUSD > 0 && g.spent >= g.cfg.DailyUSD {
		w.SpendCapped = true
	}
	if g.runCap {
		w.SpendCapped = true
	}
	if g.cfg.PerAgentUSD > 0 {
		for _, v := range g.per {
			w.SpendCapped = w.SpendCapped || v >= g.cfg.PerAgentUSD
		}
	}
	for _, id := range ids {
		if heldUntil(g.st, id, g.now) != 0 {
			w.LoopHold = true
		}
	}
	return w
}

// Agent derives one agent's gate.
func (g *GateSource) Agent(id string) AgentGate {
	ag := AgentGate{Paused: g.paused[id], HeldUntil: heldUntil(g.st, id, g.now)}
	if g.cfg.DailyUSD > 0 && g.spent >= g.cfg.DailyUSD {
		ag.SpendCapped = true
	}
	if g.cfg.PerAgentUSD > 0 && g.per[id] >= g.cfg.PerAgentUSD {
		ag.SpendCapped = true
	}
	ag.SpendCapped = ag.SpendCapped || g.runCap
	ag.TaskCapped = g.capped[id]
	if g.cfg.MaxConcurrent > 0 {
		status := g.status[id]
		if n, err := g.st.UnreadCount(id); err == nil && n > 0 && (status == "idle" || status == "finished") && g.work >= g.cfg.MaxConcurrent {
			ag.QueuedForSlot = true
		}
	}
	return ag
}

// ReadWakeGate derives the team-wide gate from store state.
func ReadWakeGate(st *store.Store, cfg GateConfig, ids []string, now time.Time) WakeGate {
	return NewGateSource(st, cfg, now).Wake(ids)
}

// ReadAgentGate derives one agent's gate.
func ReadAgentGate(st *store.Store, cfg GateConfig, id string, now time.Time) AgentGate {
	return NewGateSource(st, cfg, now).Agent(id)
}

// Reason is why the agent cannot be woken ("" if it can): the string AgentHealth
// reports instead of `stale`.
func (g AgentGate) Reason(global WakeGate) string {
	switch {
	case g.HeldUntil != 0:
		return "held until " + time.Unix(g.HeldUntil, 0).Format("15:04")
	case global.TeamHold:
		return "team hold"
	case g.Paused || global.Paused:
		return "paused"
	case g.SpendCapped:
		return "spend capped"
	case g.TaskCapped:
		return "task cap"
	case g.QueuedForSlot:
		return "queued for slot"
	}
	return ""
}
