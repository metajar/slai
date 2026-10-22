package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/tmux"
)

// CfgKey is the kv key holding the effective-config hash an agent was started with.
func CfgKey(agentID string) string { return "agent.cfg." + agentID }

// ConfigHash hashes the parts of an agent's configuration whose change
// requires a restart: resolved model, role, focus, persona, permission mode
// and the per-role prompt override (PRD 2.1).
func ConfigHash(t *config.Team, a config.Agent, projectDir string) string {
	override, _ := os.ReadFile(filepath.Join(projectDir, ".slai", "prompts", a.Role+".md"))
	parts := []string{a.Role, t.ModelFor(a), a.Focus, a.Persona, t.PermissionModeFor(a), string(override)}
	if a.HarnessKind() != config.HarnessClaude {
		// Appended only for other harnesses, so every existing claude agent keeps its
		// recorded hash (no restart on upgrade); changing harness: or the model as
		// written restarts the agent.
		parts = append(parts, a.HarnessKind(), a.Model)
	}
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Plan is the outcome of diffing the desired team against what is running.
type Plan struct {
	Start, Restart, Stop []string
	Adopt                []string // running with no recorded hash: record, do not restart
}

// Empty reports whether nothing needs doing.
func (p Plan) Empty() bool {
	return len(p.Start)+len(p.Restart)+len(p.Stop)+len(p.Adopt) == 0
}

// PlanReconcile is the pure diff. desired maps agent id -> current config hash
// (ordered by order), stored maps id -> hash recorded at start, live is the
// set of agents with a live tmux session, known lists ids that exist in state
// or tmux (candidates for removal).
func PlanReconcile(order []string, desired, stored map[string]string, live map[string]bool, known []string) Plan {
	var p Plan
	for _, id := range order {
		switch {
		case !live[id]:
			p.Start = append(p.Start, id)
		case stored[id] == "":
			p.Adopt = append(p.Adopt, id)
		case stored[id] != desired[id]:
			p.Restart = append(p.Restart, id)
		}
	}
	for _, id := range known {
		if _, ok := desired[id]; !ok {
			p.Stop = append(p.Stop, id)
		}
	}
	return p
}

// Reconcile brings running agents in line with s.Team: starts new agents,
// stops removed ones and restarts (with --continue) those whose config changed.
func (s *Supervisor) Reconcile(ctx context.Context) (Plan, error) {
	p, err := s.planReconcile(ctx)
	if err != nil {
		return p, err
	}
	return p, s.applyReconcile(ctx, p)
}

// planReconcile diffs s.Team against what is running.
func (s *Supervisor) planReconcile(ctx context.Context) (Plan, error) {
	if err := s.Prepare(ctx); err != nil {
		return Plan{}, err
	}
	prefix := tmux.SessionName(s.Dir, "")
	sessions, err := s.Tmux.ListSessions(ctx, prefix)
	if err != nil {
		return Plan{}, err
	}
	liveSess := map[string]bool{}
	for _, n := range sessions {
		liveSess[n] = true
	}
	order, desired, stored, live := []string{}, map[string]string{}, map[string]string{}, map[string]bool{}
	for _, a := range s.Team.Agents {
		order = append(order, a.ID)
		desired[a.ID] = ConfigHash(s.Team, a, s.Dir)
		stored[a.ID], _, _ = s.Store.GetKV(CfgKey(a.ID))
		live[a.ID] = liveSess[s.Session(a.ID)]
	}
	var known []string
	seen := map[string]bool{}
	if rows, err := s.Store.GetAgents(); err == nil {
		for _, r := range rows {
			known = append(known, r.ID)
			seen[r.ID] = true
		}
	}
	for n := range liveSess {
		id := n[len(prefix):]
		if !seen[id] {
			known = append(known, id)
		}
	}
	p := PlanReconcile(order, desired, stored, live, known)
	// remember what the plan was computed against
	s.planned = desired
	return p, nil
}

// applyReconcile carries out a plan.
func (s *Supervisor) applyReconcile(ctx context.Context, p Plan) error {
	byID := map[string]config.Agent{}
	for _, a := range s.Team.Agents {
		byID[a.ID] = a
	}
	for _, id := range p.Stop {
		cause := "removed from team.yaml"
		if err := s.killSession(ctx, id, s.Session(id), cause); err != nil {
			return err
		}
		_ = s.Store.DeleteAgent(id)
		_ = s.Store.DeleteKV(CfgKey(id))
		s.restartEvent(id, "agent_removed", cause, map[string]any{"agent": id})
	}
	for _, id := range p.Adopt {
		_ = s.Store.SetKV(CfgKey(id), s.planned[id])
	}
	for _, id := range p.Restart {
		cause := "team.yaml changed this agent's launch config (model, harness, focus, persona, permission mode or role prompt)"
		if err := s.killSession(ctx, id, s.Session(id), cause); err != nil {
			return err
		}
		if _, err := s.StartAgent(ctx, byID[id]); err != nil {
			return fmt.Errorf("restart %s: %w", id, err)
		}
		s.restartEvent(id, "agent_restarted", cause, map[string]any{"agent": id})
	}
	for _, id := range p.Start {
		if _, err := s.StartAgent(ctx, byID[id]); err != nil {
			return fmt.Errorf("start %s: %w", id, err)
		}
	}
	return nil
}
