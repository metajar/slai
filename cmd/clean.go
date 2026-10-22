package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/reaper"
	"github.com/oorbt/slai/internal/supervisor"
)

type worktree struct{ Path, Branch string }

type cleanPlan struct {
	sessions  []string
	procs     []reaper.Proc
	worktrees []worktree // linked worktrees slai or its agents created
	files     []string   // runtime files/dirs to delete
	branches  []string
}

func cleanCmd() *cobra.Command {
	var branches, all, dry, yes bool
	c := &cobra.Command{
		Use:   "clean",
		Short: "Stop everything and remove slai's runtime state from this project",
		Long: `clean stops the supervisor, every agent session and every process the agents started,
then removes the runtime state: .slai/run, .slai/state.db, the engineer worktrees and any
worktree an agent created under .claude/worktrees.

It keeps your team definition (.slai/team.yaml), specs and prompts, and the slai/* git
branches (they hold merged work). Add --branches to delete those too, or --all to remove the
whole .slai directory as well. --dry-run shows the plan and changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			plan, err := buildCleanPlan(c, s, branches || all, all)
			if err != nil {
				s.Store.Close()
				return err
			}
			printPlan(plan)
			if dry {
				s.Store.Close()
				fmt.Println("\n(dry run: nothing changed)")
				return nil
			}
			if !yes && !confirm("\nProceed?") {
				s.Store.Close()
				fmt.Println("cancelled")
				return nil
			}
			if stopped, err := supervisor.StopDaemon(s.Dir, 10*time.Second); err == nil && stopped {
				fmt.Println("supervisor stopped")
			}
			rep, err := s.Teardown(c.Context())
			printTeardown(rep)
			s.Store.Close() // release state.db before deleting it
			if err != nil {
				return err
			}
			if len(rep.Survivors) > 0 {
				return fmt.Errorf("%d process(es) would not stop; nothing was deleted", len(rep.Survivors))
			}
			return applyClean(s.Dir, plan)
		}}
	c.Flags().BoolVar(&branches, "branches", false, "also delete the slai/* git branches (merged work lives there!)")
	c.Flags().BoolVar(&all, "all", false, "remove everything: also .slai/ (team.yaml, specs, prompts) and the slai/* branches")
	c.Flags().BoolVar(&dry, "dry-run", false, "show what would be removed and change nothing")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return c
}

func buildCleanPlan(c *cobra.Command, s *supervisor.Supervisor, branches, all bool) (cleanPlan, error) {
	var p cleanPlan
	p.sessions = s.Sessions(c.Context())
	procs, err := reaper.Find(s.Dir)
	if err != nil {
		return p, err
	}
	p.procs = procs

	wts, err := linkedWorktrees(s.Dir)
	if err != nil {
		return p, err
	}
	sep := string(os.PathSeparator)
	for _, w := range wts {
		for _, root := range []string{filepath.Join(s.Dir, ".slai"), filepath.Join(s.Dir, ".claude", "worktrees")} {
			if strings.HasPrefix(w.Path+sep, root+sep) {
				p.worktrees = append(p.worktrees, w)
				break
			}
		}
	}
	if all {
		p.files = []string{filepath.Join(s.Dir, ".slai")}
	} else {
		for _, f := range []string{"run", "worktrees", "state.db", "state.db-wal", "state.db-shm"} {
			path := filepath.Join(s.Dir, ".slai", f)
			if _, err := os.Stat(path); err == nil {
				p.files = append(p.files, path)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".claude", "worktrees")); err == nil {
		p.files = append(p.files, filepath.Join(s.Dir, ".claude", "worktrees"))
	}
	if branches {
		out, err := exec.Command("git", "-C", s.Dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
		if err == nil {
			gone := map[string]bool{} // branches of worktrees agents created, e.g. worktree-pm-x
			for _, w := range p.worktrees {
				gone[w.Branch] = true
			}
			for _, b := range strings.Fields(string(out)) {
				if strings.HasPrefix(b, "slai/") || (gone[b] && b != s.Team.Project.MainBranch) {
					p.branches = append(p.branches, b)
				}
			}
		}
	}
	return p, nil
}

// linkedWorktrees lists the repo's linked (non-main) worktrees.
func linkedWorktrees(dir string) ([]worktree, error) {
	out, err := exec.Command("git", "-C", dir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	var wts []worktree
	var cur worktree
	flush := func() {
		if cur.Path != "" && filepath.Clean(cur.Path) != filepath.Clean(dir) {
			wts = append(wts, cur)
		}
		cur = worktree{}
	}
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(l, "worktree ")
		case strings.HasPrefix(l, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(l, "branch "), "refs/heads/")
		}
	}
	flush()
	return wts, nil
}

func printPlan(p cleanPlan) {
	fmt.Println("slai clean will:")
	fmt.Printf("  stop %d agent session(s)\n", len(p.sessions))
	for _, s := range p.sessions {
		fmt.Println("    ", s)
	}
	fmt.Printf("  kill %d process(es) the agents started\n", len(p.procs))
	for i, pr := range p.procs {
		if i == 12 {
			fmt.Printf("     … and %d more\n", len(p.procs)-12)
			break
		}
		fmt.Println("    ", pr)
	}
	fmt.Printf("  remove %d git worktree(s)\n", len(p.worktrees))
	for _, w := range p.worktrees {
		fmt.Printf("     %s (%s)\n", w.Path, w.Branch)
	}
	fmt.Printf("  delete %d file(s)/dir(s)\n", len(p.files))
	for _, f := range p.files {
		fmt.Println("    ", f)
	}
	if len(p.branches) > 0 {
		fmt.Printf("  delete %d branch(es): %s\n", len(p.branches), strings.Join(p.branches, ", "))
	}
}

func applyClean(dir string, p cleanPlan) error {
	for _, w := range p.worktrees {
		// -f -f also removes locked worktrees (Claude locks the ones it creates)
		if out, err := exec.Command("git", "-C", dir, "worktree", "remove", "-f", "-f", w.Path).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: worktree %s: %v: %s\n", w.Path, err, strings.TrimSpace(string(out)))
		}
	}
	_ = exec.Command("git", "-C", dir, "worktree", "prune").Run()
	for _, f := range p.files {
		if err := os.RemoveAll(f); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(dir, ".claude")) // only if now empty; never touches user settings
	for _, b := range p.branches {
		if out, err := exec.Command("git", "-C", dir, "branch", "-D", b).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: branch %s: %s\n", b, strings.TrimSpace(string(out)))
		}
	}
	fmt.Println("clean")
	return nil
}

func confirm(q string) bool {
	fmt.Print(q + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
}
