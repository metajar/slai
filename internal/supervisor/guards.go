package supervisor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/service"
)

// Guard thresholds (PRD 8).
const (
	LoopMessageThreshold = 10               // more than this many messages between a pair...
	LoopWindow           = 10 * time.Minute // ...within this window triggers the guard
	SpendWarnFraction    = 0.8
	// LoopHold is how long the guard holds wake-ups for a looping pair. It is a
	// cool-down, not a pause: it always ends by itself, so the team can never be
	// left stalled waiting for a human.
	LoopHold = 90 * time.Second
)

// The user is asked about a message loop only when one pair trips the guard LoopAskAfter times
// within LoopAskWindow; a single trip is just logged (a loop_hold event).
const (
	LoopAskAfter  = 3
	LoopAskWindow = time.Hour
)

// loopTrips counts the loop_hold events of the pair within LoopAskWindow (including this one).
func (s *Supervisor) loopTrips(a, b string) int {
	evs, err := s.Store.EventsByKind("", "loop_hold", 100)
	if err != nil {
		return 1
	}
	since := s.now().Add(-LoopAskWindow).Unix()
	n := 0
	for _, e := range evs {
		var d struct {
			Agents []string `json:"agents"`
		}
		if json.Unmarshal([]byte(e.Data), &d) != nil || len(d.Agents) != 2 || e.At < since {
			continue
		}
		if (d.Agents[0] == a && d.Agents[1] == b) || (d.Agents[0] == b && d.Agents[1] == a) {
			n++
		}
	}
	return n
}

// KVHoldPrefix + agent id holds the unix time a guard hold on that agent ends.
const KVHoldPrefix = "hold.until."

// checkLoops holds wake-ups (for LoopHold) for both agents of any pair exceeding
// the message-loop threshold and records an ask_user notice. Only conversational
// messages count: task assignments, reports, review traffic and slai's own notices
// are normal work and never trip it. Returns the pairs it acted on.
func (s *Supervisor) checkLoops() [][2]string {
	since := s.now().Add(-LoopWindow).Unix()
	if v, ok, _ := s.Store.GetKV(KVLoopReset); ok {
		if r, _ := strconv.ParseInt(v, 10, 64); r+1 > since {
			since = r + 1 // strictly after the resume
		}
	}
	pairs, err := s.Store.MessagePairCounts(since, "user", "slai", "all")
	if err != nil {
		s.logf("loop guard: %v", err)
		return nil
	}
	paused, _ := s.Store.PausedAgents()
	var acted [][2]string
	for _, p := range pairs {
		if p.Count <= LoopMessageThreshold {
			continue
		}
		if _, ok := s.agentCfg(p.A); !ok {
			continue
		}
		if _, ok := s.agentCfg(p.B); !ok {
			continue
		}
		if paused[p.A] && paused[p.B] {
			continue
		}
		until := fmt.Sprint(s.now().Add(LoopHold).Unix())
		for _, id := range []string{p.A, p.B} {
			_ = s.PauseAgentQuiet(id)
			_ = s.Store.SetKV(KVHoldPrefix+id, until)
		}
		// Every trip is logged; the user is only asked when the same pair keeps looping
		// (LoopAskAfter trips within LoopAskWindow): the hold releases by itself.
		trip := map[string]any{"agents": []string{p.A, p.B}, "count": p.Count}
		s.event("", "loop_hold", trip)
		if s.loopTrips(p.A, p.B) >= LoopAskAfter {
			s.event("", "ask_user", map[string]any{
				"reason":   "message_loop",
				"agents":   []string{p.A, p.B},
				"count":    p.Count,
				"question": fmt.Sprintf("%s and %s keep looping: %d chat messages in %s, held for %s again (%d times in the last hour; each hold releases by itself, `u` or `slai resume` releases it sooner). Look at what they are arguing about.", p.A, p.B, p.Count, LoopWindow, LoopHold, LoopAskAfter),
			})
		}
		acted = append(acted, [2]string{p.A, p.B})
	}
	return acted
}

// PauseAgentQuiet pauses an agent, ignoring agents without a store row.
func (s *Supervisor) PauseAgentQuiet(id string) error { return s.Store.SetAgentPaused(id, true) }

// spendGate says which wake-ups are blocked by spend caps.
type spendGate struct {
	daily  bool
	run    bool            // per_run_usd reached: the whole team
	agents map[string]bool // per-agent cap, or the per-task cap of the agent's task
}

func (g spendGate) blocked(agent string) bool { return g.daily || g.run || g.agents[agent] }

// checkSpend evaluates caps: 80% records a spend_warn event, 100% records
// spend_cap + ask_user events and blocks wake-ups. Events fire once per scope per day.
func (s *Supervisor) checkSpend() spendGate {
	g := spendGate{agents: map[string]bool{}}
	caps := s.Team.Defaults.Caps
	if caps.DailyUSD <= 0 && caps.PerAgentUSD <= 0 && caps.PerTaskUSD <= 0 && caps.PerRunUSD <= 0 {
		return g
	}
	day := s.Store.Today()
	// stamp makes an event fire once per scope per period: a day for the daily and
	// per-agent caps; a cap value (and run) for task and run caps, so raising a cap and
	// crossing the new one warns and asks again.
	stamp := day
	once := func(key string) bool {
		k := key + "." + stamp
		if _, ok, _ := s.Store.GetKV(k); ok {
			return false
		}
		_ = s.Store.SetKV(k, "1")
		return true
	}
	eval := func(scope, agent string, spent, limit float64) bool {
		if limit <= 0 {
			return false
		}
		data := map[string]any{"scope": scope, "spent_usd": spent, "cap_usd": limit}
		switch {
		case spent >= limit:
			if once("spend.cap." + scope) {
				s.event(agent, "spend_cap", data)
				s.event(agent, "ask_user", map[string]any{"reason": "spend_cap", "scope": scope,
					"question": fmt.Sprintf("Spend cap reached for %s ($%.2f of $%.2f); wake-ups paused.", scope, spent, limit)})
			}
			return true
		case spent >= SpendWarnFraction*limit:
			if once("spend.warn." + scope) {
				s.event(agent, "spend_warn", data)
			}
		}
		return false
	}
	if caps.DailyUSD > 0 {
		total, err := s.Store.TodaySpend()
		if err == nil {
			g.daily = eval("daily", "", total, caps.DailyUSD)
		}
	}
	if caps.PerAgentUSD > 0 {
		per, err := s.Store.SpendByAgentToday()
		if err == nil {
			for id, v := range per {
				if eval("agent:"+id, id, v, caps.PerAgentUSD) {
					g.agents[id] = true
				}
			}
		}
	}
	if caps.PerTaskUSD > 0 {
		stamp = fmt.Sprintf("%g", caps.PerTaskUSD)
		tasks, err := s.Store.OpenTaskSpend()
		if err == nil {
			for id, ts := range tasks {
				scope := "task:" + id
				switch {
				case ts.USD >= caps.PerTaskUSD:
					if s.spendEvent(once, "spend.cap."+scope, ts.Assignee, "spend_cap", scope, ts.USD, caps.PerTaskUSD,
						fmt.Sprintf("Task %s reached its spend cap ($%.2f of $%.2f); %s is paused on it. Raise defaults.spend_caps.per_task_usd in .slai/team.yaml to continue, or cancel the task.", id, ts.USD, caps.PerTaskUSD, ts.Assignee)) {
						s.warnTask(id, ts.Assignee, fmt.Sprintf("[%s] reached its spend cap ($%.2f of $%.2f); %s is paused on it until the cap is raised or the task is cancelled.", id, ts.USD, caps.PerTaskUSD, ts.Assignee))
					}
					if ts.Assignee != "" {
						g.agents[ts.Assignee] = true
					}
				case ts.USD >= SpendWarnFraction*caps.PerTaskUSD:
					if once("spend.warn." + scope) {
						s.event(ts.Assignee, "spend_warn", map[string]any{"scope": scope, "spent_usd": ts.USD, "cap_usd": caps.PerTaskUSD})
						s.warnTask(id, ts.Assignee, fmt.Sprintf("[%s] has spent $%.2f of its $%.2f cap (80%%+). Wrap it up or tell the orchestrator if it needs more.", id, ts.USD, caps.PerTaskUSD))
					}
				}
			}
		}
	}
	if caps.PerRunUSD > 0 {
		base := s.kvGet(service.KVRunStart)
		stamp = fmt.Sprintf("%s.%g", base, caps.PerRunUSD)
		since, _ := strconv.ParseInt(base, 10, 64)
		spent, err := s.Store.SpendSince(since)
		if err == nil {
			switch {
			case spent >= caps.PerRunUSD:
				s.spendEvent(once, "spend.cap.run", "", "spend_cap", "run", spent, caps.PerRunUSD,
					fmt.Sprintf("Run spend cap reached ($%.2f of $%.2f since the run started); the whole team is paused. Raise defaults.spend_caps.per_run_usd in .slai/team.yaml to continue, or stop the run.", spent, caps.PerRunUSD))
				g.run = true
			case spent >= SpendWarnFraction*caps.PerRunUSD:
				if once("spend.warn.run") {
					s.event("", "spend_warn", map[string]any{"scope": "run", "spent_usd": spent, "cap_usd": caps.PerRunUSD})
				}
			}
		}
	}
	return g
}

// spendEvent records the cap crossing (spend_cap + one ask_user) once. It reports
// whether this call recorded it.
func (s *Supervisor) spendEvent(once func(string) bool, key, agent, kind, scope string, spent, limit float64, question string) bool {
	if !once(key) {
		return false
	}
	s.event(agent, kind, map[string]any{"scope": scope, "spent_usd": spent, "cap_usd": limit})
	s.event(agent, "ask_user", map[string]any{"reason": "spend_cap", "scope": scope, "question": question})
	return true
}

// warnTask tells a task's assignee, and the orchestrator when it is someone else, about
// the task's spend.
func (s *Supervisor) warnTask(task, assignee, body string) {
	s.notify(assignee, task, body)
	if o := s.orchestrator(); o != assignee {
		s.notify(o, task, body)
	}
}

// releaseHolds ends guard holds whose time is up and restarts the loop-guard
// window, so a pair is only held again after another burst of chatter.
func (s *Supervisor) releaseHolds() {
	paused, err := s.Store.PausedAgents()
	if err != nil {
		return
	}
	now := s.now().Unix()
	for id := range paused {
		v, ok, _ := s.Store.GetKV(KVHoldPrefix + id)
		if !ok || v == "" {
			continue // a manual pause: only the user ends it
		}
		until, _ := strconv.ParseInt(v, 10, 64)
		if until > now {
			continue
		}
		_ = s.Store.SetAgentPaused(id, false)
		_ = s.Store.DeleteKV(KVHoldPrefix + id)
		_ = s.Store.SetKV(KVLoopReset, fmt.Sprint(now))
		s.event(id, "loop_released", map[string]any{"after": LoopHold.String()})
	}
}
