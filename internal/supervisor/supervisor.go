// Package supervisor owns agent lifecycle: worktrees, run files, tmux sessions.
package supervisor

import (
	"context"
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
	"github.com/oorbt/slai/internal/reaper"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

type Supervisor struct {
	Dir     string // absolute project root
	Team    *config.Team
	Tmux    *tmux.Client
	Git     *git.Repo
	Store   *store.Store
	SlaiBin string

	// GateOnTrust staggers startup: while Claude Code has not recorded trust for the
	// project folder, only the orchestrator starts, and the rest follow once trust is
	// recorded, so the user answers "trust this folder?" once instead of once per agent.
	GateOnTrust bool

	// Driver turns on the autonomy driver (reminders, orchestrator digests, auto-revive).
	Driver bool

	// Loop tuning (zero values pick defaults; tests override).
	Interval time.Duration                    // tick period, default 1s
	Now      func() time.Time                 // clock, default time.Now
	Detect   func(pane string) (string, bool) // pane status detector; default: the agent harness's rules
	Log      func(format string, a ...any)    // default log.Printf

	// Upgrade: re-exec onto a newly installed binary (zero values pick the running one).
	Build    string // this process's build id, default buildinfo.ID()
	ExePath  string // executable watched for replacement, default os.Executable()
	ExecArgs []string
	Exec     func(path string, args, env []string) error // default syscall.Exec

	rt      runtime
	gates   *service.GateSource // wake-gate inputs read once per Tick
	exe     *exeWatch
	planned map[string]string // config hashes the last reconcile plan was built from
	tw      teamWatch
	mem     memWatch
}

func (s *Supervisor) Session(agentID string) string { return tmux.SessionName(s.Dir, agentID) }

// Prepare creates the integration branch (idempotent).
func (s *Supervisor) Prepare(ctx context.Context) error {
	_ = s.writeAgentsFile() // best effort: a read-only .slai must not stop the team
	return s.Git.EnsureBranch(ctx, s.Team.Project.IntegrationBranch, s.Team.Project.MainBranch)
}

// Up launches every agent that is not already running. Returns the ids started.
func (s *Supervisor) Up(ctx context.Context) ([]string, error) {
	if err := s.Prepare(ctx); err != nil {
		return nil, err
	}
	var started []string
	agents := s.Team.Agents
	if s.GateOnTrust && len(agents) > 1 && !s.claudeTrusted() {
		first := agents[0]
		for _, a := range agents {
			if a.Role == config.RoleOrchestrator {
				first = a
				break
			}
		}
		var rest []string
		for _, a := range agents {
			if a.ID != first.ID {
				rest = append(rest, a.ID)
			}
		}
		_ = s.Store.SetKV(KVPendingStart, strings.Join(rest, ","))
		agents = []config.Agent{first}
	}
	for _, a := range agents {
		ok, err := s.StartAgent(ctx, a)
		if err != nil {
			return started, fmt.Errorf("agent %s: %w", a.ID, err)
		}
		if ok {
			started = append(started, a.ID)
		}
	}
	if _, ok, _ := s.Store.GetKV(service.KVRunStart); len(started) > 0 || !ok {
		_ = s.Store.SetKV(service.KVRunStart, strconv.FormatInt(s.now().Unix(), 10)) // per_run_usd counts from here
	}
	return started, nil
}

// releasePending starts the agents held back by GateOnTrust once Claude Code has
// recorded trust for the project folder.
func (s *Supervisor) releasePending(ctx context.Context) {
	v, ok, err := s.Store.GetKV(KVPendingStart)
	if err != nil || !ok || v == "" {
		return
	}
	if !s.claudeTrusted() {
		return
	}
	_ = s.Store.SetKV(KVPendingStart, "")
	for _, id := range strings.Split(v, ",") {
		for _, a := range s.Team.Agents {
			if a.ID == id {
				if _, err := s.StartAgent(ctx, a); err != nil {
					s.logf("start %s after trust: %v", id, err)
				}
			}
		}
	}
}

// StartAgent launches one agent; false means it was already running.
func (s *Supervisor) StartAgent(ctx context.Context, a config.Agent) (bool, error) {
	_ = s.Store.DeleteKV(kvStopped + a.ID) // starting an agent cancels an earlier deliberate stop
	session := s.Session(a.ID)
	if live, err := s.Tmux.HasSession(ctx, session); err != nil {
		return false, err
	} else if live {
		return false, nil
	}
	workDir, worktree := s.Dir, ""
	if a.Role == config.RoleEngineer {
		worktree = filepath.Join(s.Dir, ".slai", "worktrees", a.ID)
		branch := fmt.Sprintf("slai/%s/base", a.ID)
		if err := s.Git.AddWorktree(ctx, worktree, branch, s.Team.Project.IntegrationBranch); err != nil {
			return false, err
		}
		workDir = worktree
	}
	h := s.harnessOfCfg(a)
	spec, err := s.specFor(a, workDir)
	if err != nil {
		return false, err
	}
	// Every agent has its own session id. Older builds revived with --continue, which
	// resumes the most recent conversation in the directory and so mixed up agents that
	// share it: a leftover marker without a session id is not trusted for resuming.
	// (A harness that assigns its own ids has none until its plugin reports one, and
	// then starts fresh.)
	sid, hadID, err := harness.LoadOrCreateSessionID(h, s.Dir, a.ID)
	if err != nil {
		return false, err
	}
	spec.SessionID = sid
	marker := filepath.Join(claude.RunDir(s.Dir, a.ID), ".started")
	if _, err := os.Stat(marker); err == nil && hadID {
		spec.Resume = true // revive: resume this agent's own conversation
	}
	if err := h.WriteRunFiles(spec); err != nil {
		return false, err
	}
	_ = os.Remove(harness.GuardReadyPath(s.Dir, a.ID)) // a new launch must register its guard again
	_ = s.Store.SetKV(kvLaunchedAt+a.ID, strconv.FormatInt(s.now().Unix(), 10))
	// The tag every process of the team carries (reaper, orphan scan); the harness's
	// own command line pins identity again inline.
	env := map[string]string{"SLAI_AGENT_ID": a.ID, "SLAI_PROJECT": s.Dir}
	if err := s.Tmux.NewSession(ctx, session, workDir, env, h.Command(spec)); err != nil {
		return false, err
	}
	_ = os.WriteFile(marker, nil, 0o644)
	_ = s.Store.SetKV(CfgKey(a.ID), ConfigHash(s.Team, a, s.Dir)) // effective config this session was started with
	if err := s.Store.UpsertAgent(store.Agent{
		ID: a.ID, Role: a.Role, Model: spec.Model, Status: "idle",
		TmuxSession: session, Worktree: worktree,
	}); err != nil {
		return true, err
	}
	// UpsertAgent keeps the old status on update; a (re)started session is idle.
	return true, s.Store.SetAgentStatus(a.ID, "idle")
}

// specFor builds the launch spec for agent a working in workDir.
func (s *Supervisor) specFor(a config.Agent, workDir string) (harness.LaunchSpec, error) {
	prompt, err := roles.Render(s.Team, a, s.Dir)
	if err != nil {
		return harness.LaunchSpec{}, err
	}
	allow, deny := roles.ClaudeToolRules(a.Role, workDir)
	return harness.LaunchSpec{
		AgentID: a.ID, Model: s.Team.ModelFor(a), PermissionMode: s.Team.PermissionModeFor(a),
		ProjectDir: s.Dir, WorkDir: workDir, SystemPrompt: prompt, Allow: allow, Deny: deny, SlaiBin: s.SlaiBin,
	}, nil
}

// SpecFor returns the launch spec for agent a (engineers get their worktree path).
func (s *Supervisor) SpecFor(a config.Agent) (harness.LaunchSpec, error) {
	wd := s.Dir
	if a.Role == config.RoleEngineer {
		wd = filepath.Join(s.Dir, ".slai", "worktrees", a.ID)
	}
	return s.specFor(a, wd)
}

// TeardownReport says what Teardown stopped.
type TeardownReport struct {
	Sessions  []string      // tmux sessions killed
	Killed    int           // stray processes killed
	Survivors []reaper.Proc // processes that would not die
}

// Teardown stops everything this project's agents started: their tmux sessions
// (on the private server and on tmux's default server, where older slai builds put
// them, including sessions of agents no longer in team.yaml) and every process that
// carries the project tag (Claude background daemons, MCP servers, dev servers).
func (s *Supervisor) Teardown(ctx context.Context) (TeardownReport, error) {
	var rep TeardownReport
	for _, ss := range s.projectSessions(ctx) {
		if err := ss.client.KillSession(ctx, ss.name); err != nil {
			return rep, err
		}
		rep.Sessions = append(rep.Sessions, ss.name)
	}
	if as, err := s.Store.GetAgents(); err == nil {
		for _, a := range as {
			_ = s.Store.SetAgentStatus(a.ID, "dead")
		}
	}
	killed, survivors, err := reaper.KillAll(s.Dir, 3*time.Second)
	rep.Killed, rep.Survivors = killed, survivors
	return rep, err
}

type projSession struct {
	client *tmux.Client
	name   string
}

// projectSessions lists this project's agent sessions on the private server and on
// tmux's default server (where older slai builds put them).
func (s *Supervisor) projectSessions(ctx context.Context) []projSession {
	prefix := tmux.SessionName(s.Dir, "")
	clients := []*tmux.Client{s.Tmux}
	if s.Tmux.Socket != "" {
		clients = append(clients, tmux.New(""))
	}
	var out []projSession
	for _, c := range clients {
		names, _ := c.ListSessions(ctx, prefix)
		for _, n := range names {
			out = append(out, projSession{c, n})
		}
	}
	return out
}

// Sessions lists the tmux sessions Teardown would kill.
func (s *Supervisor) Sessions(ctx context.Context) []string {
	var names []string
	for _, ss := range s.projectSessions(ctx) {
		names = append(names, ss.name)
	}
	return names
}

// Down stops all agents and everything they started.
func (s *Supervisor) Down(ctx context.Context) error {
	rep, err := s.Teardown(ctx)
	if err != nil {
		return err
	}
	if len(rep.Survivors) > 0 {
		return fmt.Errorf("%d process(es) would not stop: %v", len(rep.Survivors), rep.Survivors)
	}
	return nil
}
