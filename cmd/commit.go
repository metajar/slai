package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// SlaiIgnoreLines are the local-only paths slai keeps out of git status.
var SlaiIgnoreLines = []string{".slai/state.db", ".slai/state.db-wal", ".slai/state.db-shm", ".slai/run/", ".slai/worktrees/"}

// excludePath is the repo's .git/info/exclude (local, never committed or merged).
func excludePath(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-path", "info/exclude").Output()
	p := strings.TrimSpace(string(out))
	if err != nil || p == "" {
		return filepath.Join(dir, ".git", "info", "exclude")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

// ensureExclude makes sure slai's local-only paths are ignored through .git/info/exclude.
func ensureExclude(dir string) error {
	return git.EnsureGitignoreLines(excludePath(dir), SlaiIgnoreLines...)
}

type landPlan struct {
	Commits     []string // "sha subject" lines that will be added
	FastForward bool
	Rebase      bool     // the work must be replayed onto the current branch first (it has moved on)
	Moves       []string // untracked files that are only slai ignore rules or identical to the incoming file: safe to clear
	Blockers    []string // untracked files that would be overwritten and differ: a human decides
	Dirty       []string // uncommitted changes that make committing unsafe (staged, or in files the merge touches)
	Stat        string   // `git diff --stat` of what the work changes
	Kept        []string // unstaged edits to files the merge does not touch: left exactly as they are
	// Conflicts are the files a trial merge of the work into HEAD conflicts in. Checked says
	// whether the trial ran (git merge-tree needs git 2.38+); without it the plan says so.
	Conflicts []string
	Checked   bool
}

// blockKept turns the kept edits into blockers, for modes that rewrite the checkout
// (--squash, -b) and so cannot promise to leave them alone.
func (p *landPlan) blockKept() {
	p.Dirty = append(p.Dirty, p.Kept...)
	p.Kept = nil
}

func commitCmd() *cobra.Command {
	var (
		newBranch, message     string
		dry, yes, noff, squash bool
		noRebase               bool
	)
	c := &cobra.Command{
		Use:     "commit",
		Aliases: []string{"land"},
		Short:   "Preview, then commit the team's finished work onto your current branch (-b NAME: onto a new branch)",
		Long: `commit takes what the team merged into slai/integration and puts it on the branch you are
on. Your current branch gets the team's commits (fast-forwarded when possible, otherwise a merge
commit); with --squash it becomes a single commit instead. Nothing is pushed.

When your branch has moved on since the team branched, the team's commits are first rebased onto
it (in a scratch worktree; slai/integration itself is never rewritten), so the result is a clean
line of commits. If that rebase conflicts it falls back to a merge commit; --no-rebase skips the
rebase attempt. If the merge conflicts too, nothing is changed and you are told so.

  slai commit            add the work to the current branch
  slai commit -b NAME    create branch NAME from the current one and put the work there; the
                         branch you were on is left exactly as it was
  slai commit --squash -m "Add the dashboard"
                         one commit with your message instead of the team's per-task commits

It first clears the one thing that routinely gets in the way: an untracked .gitignore holding only
slai's own rules (slai keeps those in .git/info/exclude now). It refuses, and says why, if your
checkout has staged changes, edits to files the merge also changes, or untracked files that differ
from what would be merged. Unstaged edits to other files are left exactly as they are (not with
--squash or -b, which rewrite the checkout).
On a terminal it first prints the plan (target branch, the commits, the files changed with
--stat, and whether it will fast-forward, rebase or merge) and asks "Apply? [y/N]"; answering n
changes nothing. --dry-run prints the same plan and stops. Without a terminal it refuses unless
you pass --yes (apply without asking) or --dry-run.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := load(c.Context())
			if err != nil {
				return err
			}
			defer s.Store.Close()
			base, err := buildLandRun(s, newBranch)
			if err != nil {
				return err
			}
			fi, _ := os.Stdin.Stat()
			base.Message, base.Dry, base.Yes, base.NoFF, base.Squash, base.NoRebase = message, dry, yes, noff, squash, noRebase
			base.In, base.Out, base.TTY = os.Stdin, os.Stdout, fi != nil && fi.Mode()&os.ModeCharDevice != 0
			return runLand(c.Context(), base)
		}}
	c.Flags().StringVarP(&newBranch, "branch", "b", "", "create this branch from the current one and put the work there")
	c.Flags().StringVarP(&message, "message", "m", "", "commit message (with --squash; also used for a merge commit)")
	c.Flags().BoolVar(&squash, "squash", false, "make one commit instead of keeping the team's per-task commits")
	c.Flags().BoolVar(&dry, "dry-run", false, "show what would happen and change nothing")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	c.Flags().BoolVar(&noRebase, "no-rebase", false, "merge instead of rebasing the team's commits onto your branch when it has moved on")
	c.Flags().BoolVar(&noff, "no-ff", false, "always create a merge commit, even when a fast-forward is possible")
	return c
}

// buildLandRun is the part of `slai commit` that looks at the project: the integration branch,
// the branch the work would go on, and a note for each task that is still open. The CLI and
// the web preview both call it, so they plan from the same facts.
func buildLandRun(s *supervisor.Supervisor, newBranch string) (landRun, error) {
	integration := s.Team.Project.IntegrationBranch
	cur, _ := gitOut(s.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	target := cur
	switch {
	case newBranch != "":
		if err := validNewBranch(s.Dir, newBranch); err != nil {
			return landRun{}, err
		}
		target = newBranch
	case cur == "":
		return landRun{}, fmt.Errorf("your checkout is not on a branch (detached HEAD): switch to one, or use -b NAME to put the work on a new branch")
	}
	if cur == integration || target == integration {
		return landRun{}, fmt.Errorf("you are on %s, the team's own branch: switch to the branch you want the work on (for example git switch %s), or use -b NAME", integration, s.Team.Project.MainBranch)
	}
	var open []string
	if tasks, err := s.Store.ListTasks(""); err == nil {
		for _, t := range tasks {
			if t.Status != store.StatusMerged && t.Status != store.StatusCancelled && t.Status != store.StatusDone && t.Status != store.StatusReverted {
				open = append(open, fmt.Sprintf("note: task %s [%s] %s is still open and is not part of this.\n", t.ID, t.Status, t.Title))
			}
		}
	}
	return landRun{Dir: s.Dir, Integration: integration, Cur: cur, Target: target, Project: s.Team.Project.Name, NewBranch: newBranch, Open: open}, nil
}

// landRun is one `slai commit` invocation, with its input and output injected so the
// preview and prompt can be tested without a terminal.
type landRun struct {
	Dir, Integration, Cur, Target, Project string
	NewBranch, Message                     string
	Dry, Yes, NoFF, Squash, NoRebase       bool
	Open                                   []string // notes about tasks that are still open
	In                                     io.Reader
	Out                                    io.Writer
	TTY                                    bool // In is a terminal: a prompt can be answered
}

// landPreview is the plan as the user sees it. --dry-run and the confirmation prompt
// print exactly this text, so what you preview is what you are asked to apply.
func landPreview(r landRun, p landPlan) string {
	var b strings.Builder
	printLandPlan(&b, p, r.Target, r.Integration, r.NewBranch != "", r.Cur, r.Squash)
	return b.String()
}

func runLand(ctx context.Context, r landRun) error {
	plan, err := planLand(ctx, r.Dir, r.Integration, r.NoFF, r.Squash, !r.NoRebase)
	if err != nil {
		return err
	}
	for _, n := range r.Open {
		fmt.Fprint(r.Out, n)
	}
	if r.Squash || r.NewBranch != "" {
		plan.blockKept()
	}
	fmt.Fprint(r.Out, landPreview(r, plan))
	if len(plan.Dirty) > 0 || len(plan.Blockers) > 0 {
		return fmt.Errorf("cannot commit yet; see above")
	}
	if len(plan.Commits) == 0 {
		return nil
	}
	if r.Dry {
		fmt.Fprintln(r.Out, "\n(dry run: nothing changed)")
		return nil
	}
	if !r.Yes {
		if !r.TTY {
			return fmt.Errorf("not applying: there is no terminal to ask you on. Run `slai commit --dry-run` to see the plan, or `slai commit --yes` to apply it without asking")
		}
		fmt.Fprint(r.Out, "\nApply? [y/N] ")
		line, _ := bufio.NewReader(r.In).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			fmt.Fprintln(r.Out, "cancelled; nothing changed")
			return nil
		}
	}
	return doLand(ctx, r.Dir, landOpts{Integration: r.Integration, Target: r.Target, From: r.Cur, NewBranch: r.NewBranch,
		NoFF: r.NoFF, Squash: r.Squash, Message: r.Message, Project: r.Project}, plan)
}

func validNewBranch(dir, name string) error {
	if _, err := gitOut(dir, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("%q is not a valid branch name", name)
	}
	if _, err := gitOut(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		return fmt.Errorf("branch %q already exists: pick another name, or switch to it and run slai commit without -b", name)
	}
	return nil
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// planLand inspects the repo and reports what committing would do against the
// current HEAD, without changing anything.
func planLand(ctx context.Context, dir, integration string, noff, squash, rebase bool) (landPlan, error) {
	var p landPlan
	if _, err := gitOut(dir, "rev-parse", "--verify", "--quiet", integration); err != nil {
		return p, fmt.Errorf("branch %s does not exist yet: the team has not merged anything", integration)
	}
	log, _ := gitOut(dir, "log", "--oneline", "HEAD.."+integration)
	for _, l := range strings.Split(log, "\n") {
		if strings.TrimSpace(l) != "" {
			p.Commits = append(p.Commits, l)
		}
	}
	p.Stat, _ = gitOut(dir, "diff", "--stat", "HEAD..."+integration)
	p.Conflicts, p.Checked = trialMerge(dir, integration)
	_, ffErr := gitOut(dir, "merge-base", "--is-ancestor", "HEAD", integration)
	p.Rebase = ffErr != nil && rebase
	p.FastForward = (ffErr == nil || p.Rebase) && !noff && !squash
	// Unstaged edits to files the incoming work does not touch survive a merge untouched
	// (git itself allows it), so they do not block. Staged changes would be swept into the
	// merge commit, and edits to files the merge changes would be overwritten: those block.
	touched := map[string]bool{}
	names, _ := gitOut(dir, "diff", "--name-only", "HEAD..."+integration)
	for _, f := range strings.Split(names, "\n") {
		touched[strings.TrimSpace(f)] = true
	}
	if st, _ := gitOut(dir, "status", "--porcelain", "--untracked-files=no"); st != "" {
		for _, l := range strings.Split(st, "\n") {
			if len(l) < 4 {
				continue
			}
			path := l[3:]
			if i := strings.Index(path, " -> "); i >= 0 { // rename: old -> new
				path = path[i+4:]
			}
			path = strings.Trim(path, "\"")
			if l[0] == ' ' && !touched[path] && !touched[strings.Trim(l[3:], "\"")] {
				p.Kept = append(p.Kept, strings.TrimSpace(l))
			} else {
				p.Dirty = append(p.Dirty, strings.TrimSpace(l))
			}
		}
	}
	// Untracked files git would refuse to overwrite: those the incoming branch adds.
	added, _ := gitOut(dir, "diff", "--name-only", "--diff-filter=A", "HEAD..."+integration)
	for _, f := range strings.Split(added, "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		path := filepath.Join(dir, f)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, err := gitOut(dir, "ls-files", "--error-unmatch", f); err == nil {
			continue // tracked: a normal merge handles it
		}
		have, _ := os.ReadFile(path)
		incoming, _ := gitOut(dir, "show", integration+":"+f)
		switch {
		case strings.TrimSpace(string(have)) == strings.TrimSpace(incoming):
			p.Moves = append(p.Moves, f) // identical: safe to clear, the merge recreates it
		case f == ".gitignore" && onlySlaiRules(string(have)):
			p.Moves = append(p.Moves, f)
		default:
			p.Blockers = append(p.Blockers, f)
		}
	}
	return p, nil
}

// trialMerge asks git whether merging integration into HEAD would conflict, without touching
// the working tree, the index or any branch: `git merge-tree --write-tree` (git 2.38+) only
// writes objects. It returns the conflicted files, and checked=false when git cannot do it.
func trialMerge(dir, integration string) (conflicts []string, checked bool) {
	out, err := exec.Command("git", "-C", dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "HEAD", integration).Output()
	if err == nil {
		return nil, true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, l := range lines[1:] { // line 0 is the tree id
		if l = strings.TrimSpace(l); l != "" {
			conflicts = append(conflicts, l)
		}
	}
	return conflicts, true
}

func onlySlaiRules(content string) bool {
	known := map[string]bool{}
	for _, l := range SlaiIgnoreLines {
		known[l] = true
	}
	for _, l := range strings.Split(content, "\n") {
		if l = strings.TrimSpace(l); l != "" && !known[l] && !strings.HasPrefix(l, "#") {
			return false
		}
	}
	return true
}

func printLandPlan(w io.Writer, p landPlan, target, integration string, isNew bool, from string, squash bool) {
	if isNew {
		fmt.Fprintf(w, "will create branch %s from %s", target, from)
		fmt.Fprintf(w, " and put %d commit(s) from %s on it (%s stays as it is):\n", len(p.Commits), integration, from)
	} else {
		fmt.Fprintf(w, "%d commit(s) from %s will be added to %s:\n", len(p.Commits), integration, target)
	}
	for i, c := range p.Commits {
		if i == 12 {
			fmt.Fprintf(w, "  ... and %d more\n", len(p.Commits)-12)
			break
		}
		fmt.Fprintln(w, "  "+c)
	}
	if len(p.Commits) == 0 {
		fmt.Fprintf(w, "nothing to add: %s already contains everything on %s.\n", target, integration)
		return
	}
	if p.Stat != "" {
		fmt.Fprintln(w, "files changed:")
		for _, l := range strings.Split(p.Stat, "\n") {
			fmt.Fprintln(w, "  "+strings.TrimSpace(l))
		}
	}
	switch {
	case squash:
		fmt.Fprintln(w, "as ONE commit (--squash).")
	case p.Rebase && p.FastForward:
		fmt.Fprintf(w, "rebased onto %s first (it has moved on), then fast-forwarded; no merge commit.\n", target)
	case p.FastForward:
		fmt.Fprintln(w, "as a fast-forward (the team's commits are kept, no merge commit).")
	default:
		fmt.Fprintln(w, "with a merge commit (the branch has moved on, or --no-ff was given).")
	}
	switch {
	case !p.Checked:
		fmt.Fprintf(w, "conflicts with %s: not checked (this git has no `merge-tree --write-tree`; slai commit still stops and changes nothing if the merge conflicts)\n", target)
	case len(p.Conflicts) == 0:
		fmt.Fprintf(w, "conflicts with %s: none (checked with a trial `git merge-tree`: no files in your working tree were touched)\n", target)
	default:
		fmt.Fprintf(w, "conflicts with %s: %s (found by a trial `git merge-tree`: your working tree was not touched; slai commit would stop and change nothing)\n", target, strings.Join(p.Conflicts, ", "))
	}
	for _, f := range p.Moves {
		fmt.Fprintf(w, "  will clear your untracked %s first (it only duplicates what comes in or holds slai's own ignore rules; those stay in .git/info/exclude)\n", f)
	}
	for _, f := range p.Blockers {
		fmt.Fprintf(w, "  BLOCKED: untracked %s differs from the version being merged. Move or commit it, then run slai commit again.\n", f)
	}
	for _, f := range p.Kept {
		fmt.Fprintf(w, "  note: your uncommitted change (%s) is not touched by this work and will be left as it is.\n", f)
	}
	for _, f := range p.Dirty {
		fmt.Fprintf(w, "  BLOCKED: uncommitted change: %s. Commit or stash it first (it is staged, or in a file this work also changes).\n", f)
	}
}

type landOpts struct {
	Integration, Target, From, NewBranch string
	NoFF, Squash                         bool
	Message, Project                     string
}

func doLand(ctx context.Context, dir string, o landOpts, p landPlan) (err error) {
	before, _ := gitOut(dir, "rev-parse", "HEAD")
	if o.NewBranch != "" {
		if out, e := gitOut(dir, "switch", "-c", o.NewBranch); e != nil {
			return fmt.Errorf("could not create branch %s: %s", o.NewBranch, out)
		}
		// if anything below fails, put the user back exactly where they were
		defer func() {
			if err != nil {
				_, _ = gitOut(dir, "reset", "--hard", before)
				if o.From != "" {
					_, _ = gitOut(dir, "switch", o.From)
				} else {
					_, _ = gitOut(dir, "switch", "--detach", before)
				}
				_, _ = gitOut(dir, "branch", "-D", o.NewBranch)
			}
		}()
	}
	for _, f := range p.Moves {
		if f == ".gitignore" {
			if e := ensureExclude(dir); e != nil { // keep slai's rules working
				return e
			}
		}
		if e := os.Remove(filepath.Join(dir, f)); e != nil {
			return e
		}
	}
	if p.Rebase {
		if sha, e := rebasedTip(dir, o.Integration, before); e != nil {
			// Replaying commit by commit can collide with a copy of the same change that
			// main already has (integration merged main in earlier). A merge handles that.
			fmt.Printf("note: rebase not possible (%s); merging instead.\n", firstLine(e.Error()))
			p.Rebase, p.FastForward = false, false
		} else {
			o.Integration = sha
		}
	}
	msg := o.Message
	switch {
	case o.Squash:
		if msg == "" {
			msg = defaultMessage(o, p)
		}
		if out, e := gitOut(dir, "merge", "--squash", o.Integration); e != nil {
			_, _ = gitOut(dir, "reset", "--hard", before)
			return fmt.Errorf("git merge --squash failed: %s\n(nothing was committed; your checkout is as it was)", out)
		}
		if out, e := gitOut(dir, "commit", "-m", msg); e != nil {
			_, _ = gitOut(dir, "reset", "--hard", before)
			return fmt.Errorf("git commit failed: %s\n(nothing was committed; your checkout is as it was)", out)
		}
	case p.FastForward:
		if out, e := gitOut(dir, "merge", "--ff-only", o.Integration); e != nil {
			return fmt.Errorf("git merge --ff-only failed: %s\n(nothing was committed; your checkout is as it was)", out)
		}
	default:
		if msg == "" {
			msg = "Add work from " + o.Integration
		}
		if out, e := gitOut(dir, "merge", "--no-ff", "-m", msg, o.Integration); e != nil {
			_, _ = gitOut(dir, "merge", "--abort")
			return fmt.Errorf("git merge failed: %s\n(nothing was committed; your checkout is as it was)", out)
		}
	}
	head, _ := gitOut(dir, "rev-parse", "--short", "HEAD")
	fmt.Printf("done: %s is now at %s (%d commit(s) added). Nothing was pushed.\n", o.Target, head, len(p.Commits))
	return nil
}

// rebasedTip replays integration onto onto in a throwaway worktree and returns the
// resulting commit. The integration branch and the user's checkout are untouched,
// and on conflict nothing is left behind.
func rebasedTip(dir, integration, onto string) (string, error) {
	tmp, err := os.MkdirTemp("", "slai-rebase-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if out, e := gitOut(dir, "worktree", "add", "--detach", wt, integration); e != nil {
		return "", fmt.Errorf("could not set up a scratch worktree for the rebase: %s", out)
	}
	defer func() { _, _ = gitOut(dir, "worktree", "remove", "--force", wt) }()
	if out, e := gitOut(wt, "rebase", onto); e != nil {
		_, _ = gitOut(wt, "rebase", "--abort")
		return "", fmt.Errorf("rebasing %s onto your branch hit conflicts: %s", integration, out)
	}
	sha, e := gitOut(wt, "rev-parse", "HEAD")
	if e != nil {
		return "", fmt.Errorf("could not read the rebased commit: %s", sha)
	}
	return sha, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func defaultMessage(o landOpts, p landPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Add work from the %s team (%d commits)\n\n", o.Project, len(p.Commits))
	for _, c := range p.Commits {
		b.WriteString("- " + c + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
