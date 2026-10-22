package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oorbt/slai/internal/orphans"
	"github.com/oorbt/slai/internal/tmux"
)

// orphanFinder looks for other teams on slai's private tmux server and on tmux's
// default one (where older builds put theirs). dir is where the command ran; the
// team it belongs to is never reported.
func orphanFinder(dir string) orphans.Finder {
	clients := []*tmux.Client{tmux.New(socketName())}
	if socketName() != "" {
		clients = append(clients, tmux.New(""))
	}
	return orphans.Finder{Clients: clients, Current: orphans.ProjectRoots(dir)}
}

// OtherTeams lists the slai teams that are not the current project's, orphans
// flagged (the TUI banner uses the same call).
func OtherTeams(ctx context.Context, dir string) ([]orphans.Team, error) {
	return orphanFinder(dir).Teams(ctx)
}

func teamLine(t orphans.Team) string {
	project := t.Project
	if project == "" {
		project = "(unknown project)"
	}
	last := "no activity recorded"
	if !t.LastActivity.IsZero() {
		last = "last activity " + time.Since(t.LastActivity).Round(time.Minute).String() + " ago"
	}
	what := fmt.Sprintf("%d session(s)", len(t.Sessions))
	if len(t.Procs) > 0 {
		what += fmt.Sprintf(", %d process(es)", len(t.Procs))
	}
	mark := "-"
	if t.Orphan {
		mark = "ORPHAN"
	}
	return fmt.Sprintf("%s %s [%s*] %s, %s — %s", mark, project, t.Prefix, what, last, t.Reason)
}

// doctorOtherTeams prints the "Other slai teams" section and returns the number
// of orphans (each is a doctor problem).
func doctorOtherTeams(ctx context.Context, dir string, w io.Writer) int {
	if os.Getenv("SLAI_NO_ORPHAN_SCAN") != "" { // test harnesses: do not look at the host's other teams
		return 0
	}
	teams, err := OtherTeams(ctx, dir)
	if err != nil {
		fmt.Fprintf(w, "! warn: could not look for other slai teams: %v\n", err)
		return 0
	}
	if len(teams) == 0 {
		fmt.Fprintln(w, "✓ no other slai teams running")
		return 0
	}
	fmt.Fprintln(w, "Other slai teams:")
	bad := 0
	for _, t := range teams {
		fmt.Fprintln(w, "  "+teamLine(t))
		if t.Orphan {
			bad++
		}
	}
	if bad > 0 {
		fmt.Fprintln(w, "  stop the orphans with `slai down --orphans` (`--dry-run` to preview)")
	}
	return bad
}

// downOrphans is `slai down --orphans`: list the orphan teams and, unless dry, stop
// their sessions and tagged processes. The current project's team is never touched.
func downOrphans(ctx context.Context, dir string, dry bool, w io.Writer) error {
	return stopOrphans(ctx, orphanFinder(dir), dry, w)
}

func stopOrphans(ctx context.Context, f orphans.Finder, dry bool, w io.Writer) error {
	teams, err := f.Teams(ctx)
	if err != nil {
		return err
	}
	var orph []orphans.Team
	for _, t := range teams {
		if t.Orphan {
			orph = append(orph, t)
		}
	}
	if len(orph) == 0 {
		fmt.Fprintln(w, "no orphaned slai teams")
		return nil
	}
	fmt.Fprintf(w, "%d orphaned team(s):\n", len(orph))
	for _, t := range orph {
		fmt.Fprintln(w, "  "+teamLine(t))
		for _, s := range t.Sessions {
			fmt.Fprintln(w, "    session", s.Name)
		}
		for _, p := range t.Procs {
			fmt.Fprintln(w, "    process", p)
		}
	}
	if dry {
		fmt.Fprintln(w, "dry run: nothing stopped")
		return nil
	}
	var firstErr error
	for _, t := range orph {
		ns, np, err := f.Kill(ctx, t)
		fmt.Fprintf(w, "stopped %s: %d session(s), %d process(es)\n", t.Prefix, ns, np)
		if err != nil {
			fmt.Fprintf(w, "  ! %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
