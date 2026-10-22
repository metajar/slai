package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// doctorPanes is `slai doctor --panes`: it runs each live agent's harness pane rules
// against its real window and prints what they decide and which rule matched, so a UI
// change that breaks a detector shows up here before it stops an agent from being woken.
// The excerpt is raw pane text: it can quote a prompt, so it is printed for the human,
// never fed back to an agent.
func doctorPanes(ctx context.Context, dir string, tm *tmux.Client, w io.Writer) error {
	team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml"))
	if err != nil {
		return fmt.Errorf("no valid .slai/team.yaml here: %w", err)
	}
	status := map[string]string{} // what state.db believes, for comparison
	if st, err := store.Open(filepath.Join(dir, ".slai", "state.db")); err == nil {
		defer st.Close()
		if as, err := st.GetAgents(); err == nil {
			for _, a := range as {
				status[a.ID] = a.Status
			}
		}
	}
	live := 0
	fmt.Fprintln(w, "pane decisions (what the detectors read from each live window):")
	for _, a := range team.Agents {
		kind := harness.Kind(a.HarnessKind())
		h, err := harness.For(kind)
		if err != nil {
			fmt.Fprintf(w, "  %-12s %-9s ✗ %v\n", a.ID, kind, err)
			continue
		}
		session := tmux.SessionName(dir, a.ID)
		if ok, err := tm.HasSession(ctx, session); err != nil || !ok {
			fmt.Fprintf(w, "  %-12s %-9s - not running\n", a.ID, kind)
			continue
		}
		pane, err := tm.CapturePane(ctx, session, false, 0)
		if err != nil {
			fmt.Fprintf(w, "  %-12s %-9s ✗ cannot read the pane: %v\n", a.ID, kind, err)
			continue
		}
		live++
		d := hooks.ExplainWith(h.PaneRules(), pane)
		known := status[a.ID]
		if known == "" {
			known = "unknown"
		}
		if !d.OK {
			fmt.Fprintf(w, "  %-12s %-9s pane: nothing recognised   (state.db: %s)\n", a.ID, kind, known)
			continue
		}
		fmt.Fprintf(w, "  %-12s %-9s pane: %-13s rule: %s.%s   (state.db: %s)\n", a.ID, kind, d.Status, kind, d.Rule, known)
		fmt.Fprintf(w, "      matched: %q\n", d.Excerpt)
	}
	if live == 0 {
		fmt.Fprintln(w, "  no agent is running here (is the team up? `slai up`)")
	}
	return nil
}

// panesFlagRun runs --panes for the doctor command, in the current directory.
func panesFlagRun(ctx context.Context, w io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return doctorPanes(ctx, dir, tmux.New(socketName()), w)
}
