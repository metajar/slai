package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// clearPoll is how often `slai clear` looks at the supervisor's progress (tests shorten it).
var clearPoll = 500 * time.Millisecond

// clearWaitMax bounds how long `slai clear` waits for an agent that is being re-grounded:
// the supervisor's own re-send and give-up times plus a margin.
func clearWaitMax() time.Duration {
	return supervisor.ResetResend + supervisor.ResetGiveUp + 30*time.Second
}

const clearUsage = "name the agents to clear: slai clear <agent>..., slai clear --role <role> or slai clear --all"

// clearTargets resolves the command line to agent ids in team order. Naming nobody is an
// error: nothing is ever cleared by default.
func clearTargets(t *config.Team, args []string, role string, all bool) ([]string, error) {
	if len(args) == 0 && role == "" && !all {
		return nil, errors.New(clearUsage)
	}
	if all && (len(args) > 0 || role != "") {
		return nil, errors.New("--all takes no agent names and no --role")
	}
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if role != "" {
		n := 0
		for _, a := range t.Agents {
			if a.Role == role {
				add(a.ID)
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("no agent has role %q (roles: %s)", role, strings.Join(teamRoles(t), ", "))
		}
	}
	for _, a := range t.Agents {
		if all {
			add(a.ID)
		}
	}
	for _, id := range args {
		add(id)
	}
	return out, nil
}

func teamRoles(t *config.Team) []string {
	set := map[string]bool{}
	for _, a := range t.Agents {
		set[a.Role] = true
	}
	var out []string
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// runClear queues the resets and reports how each ended, one line per agent. It returns the
// number that failed. The supervisor does the work; this only asks and watches.
func runClear(ctx context.Context, w, progress io.Writer, st *store.Store, team *config.Team, ids []string, wait time.Duration) (failed int) {
	type pending struct {
		id string
		at int64
	}
	var wait4 []pending
	for _, id := range ids {
		if why := service.ClearGuard(team, st, service.TriggerUser, "", id); why != "" {
			fmt.Fprintf(w, "%s: skipped (%s)\n", id, why)
			continue
		}
		req, _, err := service.QueueReset(st, id, service.ResetReq{Trigger: service.TriggerUser, Reason: "slai clear"})
		if err != nil {
			fmt.Fprintf(w, "%s: failed (%v)\n", id, err)
			failed++
			continue
		}
		wait4 = append(wait4, pending{id, req.At})
	}
	announced := map[string]bool{}
	deadline := time.Now().Add(wait)
	for len(wait4) > 0 {
		var still []pending
		for _, p := range wait4 {
			if res, ok := service.LoadResetRes(st, p.id); ok && res.Req == p.at {
				switch res.State {
				case "cleared":
					fmt.Fprintf(w, "%s: cleared\n", p.id)
				case "failed":
					fmt.Fprintf(w, "%s: failed (%s)\n", p.id, res.Detail)
					failed++
				default:
					fmt.Fprintf(w, "%s: %s (%s)\n", p.id, res.State, res.Detail)
				}
				continue
			}
			if req, ok := service.LoadReset(st, p.id); ok && req.At == p.at {
				if req.ResetAt == 0 && req.Deferred {
					fmt.Fprintf(w, "%s: deferred (busy; will clear when idle)\n", p.id)
					continue
				}
				if req.ResetAt != 0 && !announced[p.id] {
					announced[p.id] = true
					fmt.Fprintf(progress, "%s: restarted in a fresh session; waiting for it to call whoami\n", p.id)
				}
			}
			still = append(still, p)
		}
		wait4 = still
		if len(wait4) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for _, p := range wait4 {
				fmt.Fprintf(w, "%s: still pending (the supervisor has not finished it; see `slai status`)\n", p.id)
			}
			break
		}
		select {
		case <-ctx.Done():
			return failed
		case <-time.After(clearPoll):
		}
	}
	return failed
}

func clearCmd() *cobra.Command {
	var role string
	var all bool
	c := &cobra.Command{
		Use:   "clear <agent>... | --role <role> | --all",
		Short: "Clear agents' context windows (restart in a fresh session, re-grounded)",
		Long: `clear resets an agent's conversation to shrink its context. The agent restarts in a fresh
session, is told who it is (and for the orchestrator, what is on the board), and picks up
its work from the board: its tasks, branch, worktree, inbox, team memory and spend all
survive; only the conversation is lost.

A reset never interrupts a running turn: a busy agent is deferred and cleared when it goes
idle. It is skipped, with the reason, for an engineer that holds unrecorded work
(uncommitted changes, or no commit on its task branch yet). An agent counts as cleared once
it has called whoami in the new session; if it has not after a couple of minutes, it is
told again, and then you are told it needs you.

The supervisor does the work, so it must be running (` + "`slai`" + `). With nothing named, clear
errors instead of clearing everyone.`,
		Example: `  slai clear eng-1
  slai clear --role engineer
  slai clear --all`,
		RunE: func(c *cobra.Command, args []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			ids, err := clearTargets(s.Team, args, role, all)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, ok := roleOf(s.Team, id); !ok {
					return fmt.Errorf("unknown agent %q (see `slai status`)", id)
				}
			}
			if _, up := supervisor.Running(s.Dir); !up {
				return errors.New("the supervisor is not running, so nothing can restart the agents: run `slai` first")
			}
			if n := runClear(c.Context(), c.OutOrStdout(), c.ErrOrStderr(), s.Store, s.Team, ids, clearWaitMax()); n > 0 {
				return fmt.Errorf("%d agent(s) could not be cleared", n)
			}
			return nil
		}}
	c.Flags().StringVar(&role, "role", "", "clear every agent with this role (e.g. engineer)")
	c.Flags().BoolVar(&all, "all", false, "clear every agent")
	return c
}

func roleOf(t *config.Team, id string) (string, bool) {
	for _, a := range t.Agents {
		if a.ID == id {
			return a.Role, true
		}
	}
	return "", false
}
