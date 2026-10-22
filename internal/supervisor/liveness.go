package supervisor

import (
	"context"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// What state an agent is in is decided by health.Classify, fed by
// service.CollectInputs: the same function the board (get_board, the TUI, doctor)
// uses. The supervisor asks through liveness / livenessOf and never re-derives it from
// the raw status (TestSupervisorBranchesOnTheSharedClassifier checks the branches).

// livenessOf is the state the board shows for a: what the last status pass concluded
// (statusTick marks an agent whose tmux session is gone dead), so the board, which cannot
// see tmux, and the supervisor agree. The supervisor acts on a status change one pass
// after it is made, never half-way through the pass that made it.
func (s *Supervisor) livenessOf(_ context.Context, a store.Agent) health.Liveness {
	l, _ := s.classify(a, true)
	return l
}

// classify is the supervisor's view of one agent: the board's, for the same inputs.
func (s *Supervisor) classify(a store.Agent, sessionAlive bool) (health.Liveness, string) {
	h := service.AgentHealthWith(s.Store, a.ID, sessionAlive, s.now(), service.InputOpts{
		Cfg: service.GateConfigOf(s.Team), Gates: s.gates, Harness: string(s.harnessOf(a.ID).Kind()), Agent: &a,
	})
	return health.Liveness(h.Liveness), h.Reason
}

// beginTick reads the wake-gate inputs once for the whole pass.
func (s *Supervisor) beginTick() {
	s.gates = service.NewGateSource(s.Store, service.GateConfigOf(s.Team), s.now())
}

// ---- background tasks / monitors ----

// BackgroundPoll is how often an agent's pane footer is read for background tasks.
const BackgroundPoll = 5 * time.Second

// backgroundTick records, per agent, whether its pane footer says it is waiting on a
// background task or monitor (Claude Code keeps working after its turn ends while one
// runs, so the agent would otherwise read as idle for the whole run). The board reads
// the sighting back through service.KVBackground.
func (s *Supervisor) backgroundTick(ctx context.Context) {
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	now := s.now()
	s.rt.mu.Lock()
	if s.rt.lastBG == nil {
		s.rt.lastBG = map[string]time.Time{}
	}
	s.rt.mu.Unlock()
	for _, a := range agents {
		rules := s.harnessOf(a.ID).PaneRules()
		if a.Status == "dead" || a.TmuxSession == "" || rules.Background == nil {
			continue // a harness without a background-task footer has nothing to read
		}
		s.rt.mu.Lock()
		due := now.Sub(s.rt.lastBG[a.ID]) >= BackgroundPoll
		if due {
			s.rt.lastBG[a.ID] = now
		}
		s.rt.mu.Unlock()
		if !due {
			continue
		}
		pane, err := s.Tmux.CapturePane(ctx, a.TmuxSession, false, 0)
		if err != nil {
			continue
		}
		if what := hooks.BackgroundWith(rules, pane); what != "" && !s.isBlockedPane(a, pane) {
			_ = s.Store.SetKV(service.KVBackground+a.ID, strconv.FormatInt(now.Unix(), 10)+"|"+what)
		} else {
			_ = s.Store.DeleteKV(service.KVBackground + a.ID)
		}
	}
}

// isBlockedPane: a pane parked on a prompt is not "waiting on a background task".
func (s *Supervisor) isBlockedPane(a store.Agent, pane string) bool {
	return s.modalExcerpt(a, pane) != ""
}
