package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/orphans"
	"github.com/oorbt/slai/internal/supervisor"
	"github.com/oorbt/slai/internal/tui"
)

// untrustedDirs returns the project folder while Claude Code has not recorded
// trust for it. Claude keys trust by the main checkout, so worktrees inherit it;
// agents that launched before it was recorded still show the dialog and are
// marked "needs you" individually by the supervisor's pane detector.
func untrustedDirs(s *supervisor.Supervisor) []string {
	if claude.IsTrusted(s.Dir) {
		return nil
	}
	return []string{s.Dir}
}

// trustWarning is shown (TUI banner, `slai up`) while Claude Code still has to
// ask "trust this folder?" in some agent's directory.
func trustWarning(dirs []string) string {
	return "Claude Code asks “trust this folder?” once: focus orch (enter) and choose “Yes”. The rest of the team starts automatically after that. slai never edits Claude’s trust settings."
}

// runTUI starts missing agents, makes sure a detached supervisor is running and
// opens the interface. The dashboard is only a view: quitting it (or crashing)
// must never stop work being delivered to agents, so the supervisor is a separate
// daemon, and the dashboard respawns it if it ever dies.
func runTUI(ctx context.Context) error {
	s, err := load(ctx)
	if err != nil {
		return err
	}
	defer s.Store.Close()
	if _, err := s.Up(ctx); err != nil {
		return err
	}
	var startLoop func(context.Context)
	sup := "daemon"
	ensure := func() error {
		if _, ok := supervisor.Running(s.Dir); ok {
			return nil
		}
		return startDaemon(s.Dir)
	}
	if err := ensure(); err != nil {
		// could not start a daemon: fall back to running the loop inside the dashboard
		if lock, lerr := supervisor.Acquire(s.Dir); lerr == nil {
			defer lock.Release()
			startLoop = func(c context.Context) { _ = s.Run(c) }
			sup = "tui"
		}
	} else {
		startLoop = func(c context.Context) { // watchdog: respawn the supervisor if it dies
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-c.Done():
					return
				case <-t.C:
					_ = ensure()
				}
			}
		}
	}
	stop := s.StopAgent // deliberate stop: the autonomy driver will not revive it
	var (
		pending []string
		checked time.Time
	)
	return tui.Run(tui.Options{
		Project: s.Team.Project.Name, Dir: s.Dir, Team: s.Team, Store: s.Store, Tmux: s.Tmux, Git: s.Git, Memory: memoryFor(s),
		SessionName: s.Session,
		Supervisor:  sup,
		Warn: func() string {
			// ~/.claude.json can be large: re-read at most every 5s.
			if time.Since(checked) >= 5*time.Second {
				checked = time.Now()
				pending = untrustedDirs(s)
			}
			if len(pending) == 0 {
				return ""
			}
			return trustWarning(pending)
		},
		OtherTeams: func(c context.Context) ([]orphans.Team, error) {
			if os.Getenv("SLAI_NO_ORPHAN_SCAN") != "" { // test harnesses: ignore the host's other teams
				return nil, nil
			}
			return OtherTeams(c, s.Dir)
		},
		ResumeAgent: s.ResumeAgent,
		Revive: func(c context.Context, id string) error {
			for _, a := range s.Team.Agents {
				if a.ID == id {
					_, err := s.StartAgent(c, a)
					return err
				}
			}
			return fmt.Errorf("unknown agent %q", id)
		},
		Stop: stop,
		Restart: func(c context.Context, id string) error {
			for _, a := range s.Team.Agents {
				if a.ID == id {
					if err := stop(c, id); err != nil {
						return err
					}
					// No marker and no session id => the next launch is a fresh conversation.
					_ = os.Remove(filepath.Join(claude.RunDir(s.Dir, id), ".started"))
					_ = os.Remove(claude.SessionIDPath(s.Dir, id))
					_, err := s.StartAgent(c, a)
					return err
				}
			}
			return fmt.Errorf("unknown agent %q", id)
		},
		SetPaused: func(p bool) error {
			if p {
				return s.Pause()
			}
			return s.Resume()
		},
		IsPaused:    func() bool { return s.Paused() },
		SetPriority: s.SetPriority,
		CurrentHold: func() (string, int64, bool) {
			h, ok := s.CurrentHold()
			return h.Name, h.Since, ok
		},
		HoldTeam: s.HoldTeam,
		ReleaseHold: func() error {
			_, _, err := s.ReleaseHold()
			return err
		},
		StartLoop: startLoop,
	})
}

// memoryFor is the project's team memory for the TUI's Memory view.
func memoryFor(s *supervisor.Supervisor) *memory.Memory {
	rev := config.DefaultIntegrationBranch
	if s.Team != nil && s.Team.Project.IntegrationBranch != "" {
		rev = s.Team.Project.IntegrationBranch
	}
	return memory.New(filepath.Join(s.Dir, ".slai", "memory"), s.Store, s.Git, rev)
}
