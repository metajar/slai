package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/buildinfo"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

func upCmd() *cobra.Command {
	var noWatch, foreground, detach bool
	c := &cobra.Command{Use: "up", Short: "Start all agents and the supervisor without the dashboard (`slai` does this and opens it)", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			pid, running := supervisor.Running(s.Dir)
			if foreground && !noWatch && running {
				return fmt.Errorf("supervisor already running (pid %d); use `slai down` first", pid)
			}
			if noWatch {
				s.GateOnTrust = false // nothing would release the held-back agents
			}
			fmt.Println(costNotice(s.Team))
			started, err := s.Up(c.Context())
			if len(started) == 0 {
				fmt.Println("all agents already running")
			} else {
				fmt.Printf("started %d agent(s): %v\n", len(started), started)
			}
			if err != nil {
				return err
			}
			if dirs := untrustedDirs(s); len(dirs) > 0 {
				fmt.Fprintln(os.Stderr, trustWarning(dirs))
			}
			if noWatch {
				return nil
			}
			if foreground {
				fmt.Println(goalHint(s.Team))
				return runForeground(c.Context(), s)
			}
			if running {
				fmt.Printf("supervisor already running (pid %d)\n", pid)
			} else if err := spawnDaemon(s.Dir); err != nil {
				return err
			}
			fmt.Println("supervisor running in the background")
			fmt.Println(goalHint(s.Team))
			return nil
		}}
	c.Flags().BoolVar(&noWatch, "no-watch", false, "start agents and exit without any supervisor loop")
	c.Flags().BoolVar(&foreground, "foreground", false, "run the supervisor loop in this terminal (ctrl-c stops it; agents keep running)")
	c.Flags().BoolVarP(&detach, "detach", "d", false, "deprecated: detaching is the default")
	_ = c.Flags().MarkHidden("detach")
	return c
}

// goalHint is the one "next" line of `slai up` (headless: the agents are already running):
// who to give the goal to, and how.
func goalHint(t *config.Team) string {
	orch := "orch"
	for _, a := range t.Agents {
		if a.Role == config.RoleOrchestrator {
			orch = a.ID
		}
	}
	return fmt.Sprintf("next: give %s your goal — it drives the rest of the team: slai send %s \"<your goal>\"", orch, orch)
}

func runForeground(ctx context.Context, s *supervisor.Supervisor) error {
	_ = buildinfo.ID() // hash the binary now, before the lock: `up` waits for it, and the first tick must not
	lock, err := supervisor.Acquire(s.Dir)
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("supervisor running; ctrl-c to stop (agents keep running)")
	return s.Run(ctx)
}

// spawnDaemon starts the detached supervisor and reports it.
func spawnDaemon(dir string) error {
	if err := startDaemon(dir); err != nil {
		return err
	}
	pid, _ := supervisor.Running(dir)
	fmt.Printf("supervisor running in background (pid %d), log %s\n", pid, supervisor.LogPath(dir))
	return nil
}

// startDaemon starts the detached supervisor and waits until it holds the lock.
func startDaemon(dir string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(supervisor.LogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(bin, "supervise")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 50; i++ {
		if _, ok := supervisor.Running(dir); ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("supervisor did not start; see %s", supervisor.LogPath(dir))
}

func superviseCmd() *cobra.Command {
	return &cobra.Command{Use: "supervise", Short: "Run the supervisor loop (used by `up`)", Hidden: true, Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			return runForeground(c.Context(), s)
		}}
}

func downCmd() *cobra.Command {
	var orphansOnly, dry bool
	c := &cobra.Command{Use: "down", Short: "Stop the supervisor, every agent session and anything the agents started", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if dry && !orphansOnly {
				return fmt.Errorf("--dry-run only applies to --orphans")
			}
			if orphansOnly { // other projects' abandoned teams; this project is left alone
				dir, err := os.Getwd()
				if err != nil {
					return err
				}
				return downOrphans(c.Context(), dir, dry, c.OutOrStdout())
			}
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			supervisor.MarkStopped(s.Dir) // so a Stop hook does not bring it back
			if stopped, err := supervisor.StopDaemon(s.Dir, 10*time.Second); err != nil {
				return err
			} else if stopped {
				fmt.Println("supervisor stopped")
			}
			rep, err := s.Teardown(c.Context())
			printTeardown(rep)
			if err != nil {
				return err
			}
			if len(rep.Survivors) > 0 {
				return fmt.Errorf("%d process(es) would not stop", len(rep.Survivors))
			}
			return nil
		}}
	c.Flags().BoolVar(&orphansOnly, "orphans", false, "instead, stop other projects' abandoned teams (no supervisor, project gone or idle >12h); this project is untouched")
	c.Flags().BoolVar(&dry, "dry-run", false, "with --orphans: only list what would be stopped")
	return c
}

func printTeardown(rep supervisor.TeardownReport) {
	fmt.Printf("stopped %d agent session(s)", len(rep.Sessions))
	if rep.Killed > 0 {
		fmt.Printf(" and killed %d stray process(es) the agents had started", rep.Killed)
	}
	fmt.Println()
	for _, p := range rep.Survivors {
		fmt.Println("  still running:", p)
	}
}

type statusAgent struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Model       string `json:"model"`
	Status      string `json:"status"`
	CurrentTask string `json:"current_task"`
	// DeniedCount is the agent's lane denials in the last 24 hours.
	DeniedCount int `json:"denied_count"`
}

type statusHold struct {
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

type statusTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}

func statusCmd() *cobra.Command {
	var asJSON, stats bool
	var since string
	c := &cobra.Command{Use: "status", Short: "Print agents and open tasks", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if since != "" && !stats {
				return fmt.Errorf("--since only applies with --stats")
			}
			cut, err := parseSince(since, time.Now())
			if err != nil {
				return err
			}
			if dir, _ := os.Getwd(); !hasTeam(dir) {
				fmt.Println(noTeamMessage)
				return nil
			}
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			if stats {
				rep, err := (&service.Service{Store: s.Store, Team: s.Team, Dir: s.Dir}).Stats(cut)
				if err != nil {
					return err
				}
				return writeStats(os.Stdout, rep, asJSON)
			}
			agents, err := s.Store.GetAgents()
			if err != nil {
				return err
			}
			out := struct {
				Agents []statusAgent `json:"agents"`
				Tasks  []statusTask  `json:"tasks"`
				Hold   *statusHold   `json:"hold,omitempty"` // the team hold, while one is in force
			}{Agents: []statusAgent{}, Tasks: []statusTask{}}
			denied, _ := s.Store.LaneDenialCounts(time.Now().Add(-24 * time.Hour).Unix())
			if h, ok := s.CurrentHold(); ok {
				out.Hold = &statusHold{h.Name, h.Since}
			}
			rows := map[string]store.Agent{}
			for _, a := range agents {
				rows[a.ID] = a
			}
			// Every configured agent is listed, even before anything has started: an agent with
			// no live session is "dead" if slai ever registered it (it had a session, now gone: after
			// `slai down`, a crash), and "-" if it is only configured and has never been started.
			for _, ta := range s.Team.Agents {
				a, known := rows[ta.ID]
				live := false
				if known {
					live, _ = s.Tmux.HasSession(c.Context(), a.TmuxSession)
				}
				st := a.Status
				switch {
				case live:
				case known:
					st = "dead"
				default:
					st = "-"
				}
				model := ta.Model
				if known {
					model = a.Model
				}
				out.Agents = append(out.Agents, statusAgent{ta.ID, ta.Role, model, st, a.CurrentTask, denied[ta.ID]})
			}
			tasks, err := s.Store.ListTasks("")
			if err != nil {
				return err
			}
			for _, t := range tasks {
				if t.Status == store.StatusMerged || t.Status == store.StatusCancelled || t.Status == store.StatusDone || t.Status == store.StatusReverted {
					continue
				}
				out.Tasks = append(out.Tasks, statusTask{t.ID, t.Title, t.Assignee, t.Status, t.Priority})
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			for _, a := range out.Agents {
				fmt.Printf("%-12s %-20s %-8s %-16s %s%s\n", a.ID, a.Role, a.Model, a.Status, a.CurrentTask, deniedNote(a.DeniedCount))
			}
			if len(out.Tasks) > 0 {
				fmt.Println("\nopen tasks:")
				for _, t := range out.Tasks {
					fmt.Printf("%-8s %-18s %-10s %s\n", t.ID, t.Status, t.Assignee, t.Title)
				}
			}
			printHealth(s, len(out.Tasks))
			return nil
		}}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&stats, "stats", false, "per-agent and per-harness/model quality numbers (merges, rework, spend)")
	c.Flags().StringVar(&since, "since", "", "with --stats: only tasks assigned in this span, e.g. 7d, 2w, 36h (default all time)")
	return c
}

func priorityCmd() *cobra.Command {
	return &cobra.Command{Use: "priority <task> <n|up|down|top>", Short: "Set a task's priority (higher runs first; 0 is the default)",
		Long: `priority changes the order the team works in. A number sets it, up/down move it one step,
and top puts it above every other open task. When max_concurrent_agents is the limit, the agent
owed the highest-priority task is woken first, and an agent's inbox lists higher-priority tasks
first. Nothing is interrupted: a turn already running is left alone.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			p, err := s.SetPriority(args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("%s priority is now %d\n", args[0], p)
			return nil
		}}
}

func holdCmd() *cobra.Command {
	return &cobra.Command{Use: "hold [name]", Short: "Hold the whole team (e.g. overnight); `slai resume` continues",
		Long: `hold stops the team from being woken, nudged or restarted, without killing any session:
agents finish the turn they are in and then wait. The hold is saved, so it survives a
supervisor restart and slai down + slai up. Give it a name to remember why ("lunch", "waiting on API key").
` + "`slai resume`" + ` lifts it and the team picks up what it was owed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			name := "hold"
			if len(args) == 1 {
				name = args[0]
			}
			if err := s.HoldTeam(name); err != nil {
				return err
			}
			fmt.Printf("team held (%s): agents finish their current turn and wait; sessions stay up. `slai resume` continues.\n", name)
			return nil
		}}
}

func resumeCmd() *cobra.Command {
	var holdOnly bool
	c := &cobra.Command{Use: "resume [agent...]", Short: "Lift a team hold and resume agents held by slai (message-loop guard); no args = all",
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			if len(args) == 0 || holdOnly {
				h, was, err := s.ReleaseHold()
				if err != nil {
					return err
				}
				if was {
					fmt.Printf("team hold %q lifted (held since %s); the team picks up what it was owed\n", h.Name, time.Unix(h.Since, 0).Format("15:04"))
				}
			}
			if holdOnly {
				return nil
			}
			if len(args) == 0 {
				held, err := s.Store.PausedAgents()
				if err != nil {
					return err
				}
				for id := range held {
					args = append(args, id)
				}
			}
			for _, id := range args {
				if err := s.ResumeAgent(id); err != nil {
					return err
				}
				fmt.Println("resumed", id)
			}
			if len(args) == 0 {
				fmt.Println("no agents are held")
			}
			return nil
		}}
	c.Flags().BoolVar(&holdOnly, "hold", false, "only lift the team hold, leave loop-guard holds alone")
	return c
}

// printHealth says plainly why work might be stuck: no supervisor, paused agents,
// or messages nobody has picked up.
func printHealth(s *supervisor.Supervisor, openTasks int) {
	var problems []string
	pid, running := supervisor.Running(s.Dir)
	waiting := 0
	for _, a := range s.Team.Agents {
		if msgs, err := s.Store.UndeliveredFor(a.ID); err == nil {
			waiting += len(msgs)
		}
	}
	if !running && waiting > 0 {
		problems = append(problems, fmt.Sprintf("no supervisor is running, so %d message(s) are waiting and nobody will wake the agents. Run `slai`.", waiting))
	}
	if h, ok := s.CurrentHold(); ok {
		problems = append(problems, fmt.Sprintf("the team is on hold %q since %s: nobody is woken or nudged, sessions stay up. `slai resume` continues.", h.Name, time.Unix(h.Since, 0).Format("Jan 2 15:04")))
	} else if s.Paused() {
		problems = append(problems, "the whole team is paused (p in the dashboard). Press p again to resume.")
	}
	if held, err := s.Store.PausedAgents(); err == nil && len(held) > 0 {
		var ids []string
		for id := range held {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		problems = append(problems, fmt.Sprintf("held agents: %s (guard holds release by themselves; `slai resume` releases them now)", strings.Join(ids, ", ")))
	}
	fmt.Println()
	if v, ok, _ := s.Store.GetKV("run.done"); ok && v != "" {
		fmt.Println("run finished. Final summary from the orchestrator:")
		fmt.Println("  " + strings.ReplaceAll(strings.TrimSpace(v), "\n", "\n  "))
		fmt.Println()
	}
	if running {
		fmt.Printf("supervisor: running (pid %d)\n", pid)
	} else {
		fmt.Println("supervisor: not running. " + nextStep(s.Team, false, openTasks))
	}
	for _, p := range problems {
		fmt.Println("! " + p)
	}
	if running {
		fmt.Println(nextStep(s.Team, true, openTasks))
	}
}

// deniedNote is the status-line suffix for lane denials; nothing when there are none.
func deniedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("  lane-denied(24h)=%d", n)
}
