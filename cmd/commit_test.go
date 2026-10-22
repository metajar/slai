package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newLandRepo: main has one commit; slai/integration adds files (incl. its own committed
// .gitignore) on top, exactly as engineers' merged work does.
func newLandRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	d := t.TempDir()
	run(t, d, "init", "-q", "-b", "main")
	write(t, d, "README.md", "# x\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "init")
	run(t, d, "checkout", "-q", "-b", "slai/integration")
	write(t, d, ".gitignore", "minion\ndist/\n")
	write(t, d, "main.go", "package main\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "[T-0003] backend")
	run(t, d, "checkout", "-q", "main")
	return d
}

func opts(target string) landOpts {
	return landOpts{Integration: "slai/integration", Target: target, From: target, Project: "demo"}
}

// The /tmp/minion case: main has an UNTRACKED .gitignore holding only slai's rules,
// and integration commits its own .gitignore. A plain merge refuses; commit clears it.
func TestCommitClearsSlaiOnlyUntrackedGitignoreAndFastForwards(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, ".gitignore", strings.Join(SlaiIgnoreLines, "\n")+"\n")
	if out, err := exec.Command("git", "-C", d, "merge", "--ff-only", "slai/integration").CombinedOutput(); err == nil {
		t.Fatalf("precondition: a plain merge should refuse, got %s", out)
	}
	ctx := context.Background()
	p, err := planLand(ctx, d, "slai/integration", false, false, false)
	if err != nil || !p.FastForward || len(p.Commits) != 1 || len(p.Blockers) != 0 || len(p.Moves) != 1 || p.Moves[0] != ".gitignore" {
		t.Fatalf("plan = %+v err=%v", p, err)
	}
	if err := doLand(ctx, d, opts("main"), p); err != nil {
		t.Fatal(err)
	}
	if got := run(t, d, "rev-parse", "main"); got != run(t, d, "rev-parse", "slai/integration") {
		t.Fatal("main was not fast-forwarded")
	}
	if b, _ := os.ReadFile(filepath.Join(d, ".gitignore")); string(b) != "minion\ndist/\n" {
		t.Fatalf("working .gitignore should be the committed one, got %q", b)
	}
	ex, _ := os.ReadFile(excludePath(d))
	for _, l := range SlaiIgnoreLines {
		if !strings.Contains(string(ex), l) {
			t.Errorf("%q missing from .git/info/exclude", l)
		}
	}
	if st := run(t, d, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean afterwards: %q", st)
	}
}

func TestCommitRefusesWhenItCannotBeSafe(t *testing.T) {
	ctx := context.Background()
	d := newLandRepo(t) // an untracked .gitignore with the user's OWN rules must not be deleted
	write(t, d, ".gitignore", "node_modules/\nmy-private-notes\n")
	p, _ := planLand(ctx, d, "slai/integration", false, false, false)
	if len(p.Blockers) != 1 || p.Blockers[0] != ".gitignore" || len(p.Moves) != 0 {
		t.Fatalf("user's own untracked file must block, got %+v", p)
	}
	d2 := newLandRepo(t) // an edit to a file the merge changes blocks
	run(t, d2, "checkout", "-q", "slai/integration")
	write(t, d2, "README.md", "# v2\n")
	run(t, d2, "commit", "-qam", "touch README")
	run(t, d2, "checkout", "-q", "main")
	write(t, d2, "README.md", "# mine\n")
	if p2, _ := planLand(ctx, d2, "slai/integration", false, false, false); len(p2.Dirty) != 1 {
		t.Fatalf("edit to an incoming file must block: %+v", p2)
	}
	d3 := newLandRepo(t) // staged changes block even when unrelated
	write(t, d3, "README.md", "# edited\n")
	run(t, d3, "add", "README.md")
	if p3, _ := planLand(ctx, d3, "slai/integration", false, false, false); len(p3.Dirty) != 1 {
		t.Fatalf("staged change must block: %+v", p3)
	}
	d4 := newLandRepo(t) // nothing to add
	run(t, d4, "merge", "-q", "--ff-only", "slai/integration")
	if p4, _ := planLand(ctx, d4, "slai/integration", false, false, false); len(p4.Commits) != 0 {
		t.Fatalf("nothing should be left: %+v", p4)
	}
}

// The target is the branch you are on, not a hardcoded main.
func TestCommitGoesOntoTheCurrentBranch(t *testing.T) {
	d := newLandRepo(t)
	run(t, d, "switch", "-q", "-c", "feature/dashboard")
	write(t, d, "WIP.md", "mine\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "my own work on the feature branch")
	ctx := context.Background()
	p, err := planLand(ctx, d, "slai/integration", false, false, false)
	if err != nil || p.FastForward || len(p.Commits) != 1 {
		t.Fatalf("a branch with its own commits needs a merge commit: %+v %v", p, err)
	}
	if err := doLand(ctx, d, opts("feature/dashboard"), p); err != nil {
		t.Fatal(err)
	}
	if cur := run(t, d, "rev-parse", "--abbrev-ref", "HEAD"); cur != "feature/dashboard" {
		t.Fatalf("moved to %s", cur)
	}
	for _, f := range []string{"main.go", "WIP.md"} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Errorf("%s missing on the feature branch", f)
		}
	}
	if m := run(t, d, "rev-parse", "main"); m == run(t, d, "rev-parse", "HEAD") {
		t.Error("main must be untouched")
	}
}

// -b NAME: the work goes on a NEW branch and the branch you were on is left as it was.
func TestCommitMinusBPutsWorkOnANewBranch(t *testing.T) {
	d := newLandRepo(t)
	mainBefore := run(t, d, "rev-parse", "main")
	ctx := context.Background()
	if err := validNewBranch(d, "main"); err == nil {
		t.Fatal("an existing branch name must be refused")
	}
	if err := validNewBranch(d, "bad..name"); err == nil {
		t.Fatal("an invalid branch name must be refused")
	}
	p, _ := planLand(ctx, d, "slai/integration", false, false, false)
	o := opts("minion-dashboard")
	o.From, o.NewBranch = "main", "minion-dashboard"
	if err := doLand(ctx, d, o, p); err != nil {
		t.Fatal(err)
	}
	if cur := run(t, d, "rev-parse", "--abbrev-ref", "HEAD"); cur != "minion-dashboard" {
		t.Fatalf("should end on the new branch, on %s", cur)
	}
	if run(t, d, "rev-parse", "main") != mainBefore {
		t.Fatal("the branch you were on must be left exactly as it was")
	}
	if run(t, d, "rev-parse", "minion-dashboard") != run(t, d, "rev-parse", "slai/integration") {
		t.Fatal("new branch should carry the team's work")
	}
}

// If -b fails after the branch was created, nothing is left behind.
func TestCommitMinusBRollsBackOnFailure(t *testing.T) {
	d := newLandRepo(t)
	ctx := context.Background()
	p, _ := planLand(ctx, d, "slai/integration", false, false, false)
	p.FastForward = true
	o := opts("will-fail")
	o.From, o.NewBranch, o.Integration = "main", "will-fail", "slai/does-not-exist" // the merge step fails
	if err := doLand(ctx, d, o, p); err == nil {
		t.Fatal("expected a failure")
	}
	if cur := run(t, d, "rev-parse", "--abbrev-ref", "HEAD"); cur != "main" {
		t.Fatalf("should be back on main, on %s", cur)
	}
	if out, err := exec.Command("git", "-C", d, "rev-parse", "--verify", "--quiet", "refs/heads/will-fail").CombinedOutput(); err == nil {
		t.Fatalf("the half-made branch was left behind: %s", out)
	}
}

func TestCommitSquashMakesOneCommitWithYourMessage(t *testing.T) {
	d := newLandRepo(t)
	run(t, d, "checkout", "-q", "slai/integration")
	write(t, d, "second.go", "package main\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "[T-0004] frontend")
	run(t, d, "checkout", "-q", "main")
	ctx := context.Background()
	p, _ := planLand(ctx, d, "slai/integration", false, true, false)
	if p.FastForward || len(p.Commits) != 2 {
		t.Fatalf("squash never fast-forwards: %+v", p)
	}
	o := opts("main")
	o.Squash, o.Message = true, "Add the dashboard"
	if err := doLand(ctx, d, o, p); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "rev-list", "--count", "main"); n != "2" { // init + one squashed commit
		t.Fatalf("expected init + ONE new commit, have %s commits", n)
	}
	if subj := run(t, d, "log", "-1", "--format=%s"); subj != "Add the dashboard" {
		t.Fatalf("subject = %q", subj)
	}
	if _, err := os.Stat(filepath.Join(d, "second.go")); err != nil {
		t.Fatal("squashed content missing")
	}
	// default message lists what went in
	o2 := opts("main")
	if m := defaultMessage(o2, p); !strings.Contains(m, "[T-0003] backend") || !strings.Contains(m, "[T-0004] frontend") {
		t.Fatalf("default squash message should list the commits: %q", m)
	}
}

func TestInitKeepsIgnoreRulesOutOfGitignore(t *testing.T) {
	d := newLandRepo(t)
	run(t, d, "checkout", "-q", "main")
	_ = os.Remove(filepath.Join(d, ".gitignore"))
	if err := ensureExclude(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, ".gitignore")); err == nil {
		t.Fatal("init must not create a .gitignore (it blocks the later merge)")
	}
	os.MkdirAll(filepath.Join(d, ".slai", "run", "orch"), 0o755)
	write(t, filepath.Join(d, ".slai", "run", "orch"), "system.md", "x")
	if st := run(t, d, "status", "--porcelain"); strings.Contains(st, ".slai/run") {
		t.Fatalf(".slai/run should be ignored via info/exclude: %q", st)
	}
}

// When the branch has moved on, the team's commits are rebased onto it: linear history,
// fast-forward, and slai/integration itself is not rewritten.
func TestCommitRebasesOntoAMovedOnBranch(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, "WIP.md", "mine\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "main moved on")
	integBefore := run(t, d, "rev-parse", "slai/integration")
	ctx := context.Background()
	p, err := planLand(ctx, d, "slai/integration", false, false, true)
	if err != nil || !p.Rebase || !p.FastForward {
		t.Fatalf("a moved-on branch should plan a rebase + fast-forward: %+v %v", p, err)
	}
	if err := doLand(ctx, d, opts("main"), p); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "rev-list", "--merges", "--count", "HEAD"); n != "0" {
		t.Errorf("history should be linear, got %s merge commits", n)
	}
	for _, f := range []string{"main.go", "WIP.md"} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Errorf("%s missing", f)
		}
	}
	if run(t, d, "rev-parse", "slai/integration") != integBefore {
		t.Error("slai/integration must not be rewritten")
	}
}

// Unrelated unstaged edits do not block, and survive the commit untouched.
func TestCommitKeepsUnrelatedUnstagedEdits(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, "README.md", "# my wip\n")
	ctx := context.Background()
	p, _ := planLand(ctx, d, "slai/integration", false, false, true)
	if len(p.Dirty) != 0 || len(p.Kept) != 1 {
		t.Fatalf("unrelated edit should be kept, not block: %+v", p)
	}
	if err := doLand(ctx, d, opts("main"), p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "README.md")); string(b) != "# my wip\n" {
		t.Errorf("edit lost: %q", b)
	}
	if _, err := os.Stat(filepath.Join(d, "main.go")); err != nil {
		t.Error("work not landed")
	}
	p.Kept = []string{"M README.md"}
	p.blockKept()
	if len(p.Dirty) != 1 || len(p.Kept) != 0 {
		t.Errorf("blockKept: %+v", p)
	}
}

// When the rebase collides with a copy of a change main already has, fall back to a merge.
func TestCommitRebaseCollisionFallsBackToMerge(t *testing.T) {
	d := newLandRepo(t)
	run(t, d, "checkout", "-q", "slai/integration")
	write(t, d, "same.txt", "x\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "same change on integration")
	run(t, d, "checkout", "-q", "main")
	write(t, d, "same.txt", "x\n") // main has its own copy of it, different commit
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "same change on main")
	ctx := context.Background()
	p, _ := planLand(ctx, d, "slai/integration", false, false, true)
	if err := doLand(ctx, d, opts("main"), p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, "main.go")); err != nil {
		t.Error("work not landed")
	}
	if wl := run(t, d, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("scratch worktree leaked: %s", wl)
	}
}

// A change that conflicts both ways changes nothing.
func TestCommitRebaseConflictLeavesCheckoutAlone(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, "main.go", "package other\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "conflicting")
	before := run(t, d, "rev-parse", "HEAD")
	ctx := context.Background()
	p, _ := planLand(ctx, d, "slai/integration", false, false, true)
	err := doLand(ctx, d, opts("main"), p)
	if err == nil {
		t.Fatal("want a conflict error")
	}
	if run(t, d, "rev-parse", "HEAD") != before || run(t, d, "status", "--porcelain") != "" {
		t.Error("checkout must be untouched")
	}
	if wl := run(t, d, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("scratch worktree leaked: %s", wl)
	}
}

func landRunFor(d string, in string, tty bool, out *strings.Builder) landRun {
	return landRun{Dir: d, Integration: "slai/integration", Cur: "main", Target: "main", Project: "demo",
		In: strings.NewReader(in), Out: out, TTY: tty}
}

// Interactive: the plan (target, commits, --stat) is printed, then the prompt; "n" changes nothing.
func TestCommitPromptShowsPlanAndNoChangesNothing(t *testing.T) {
	d := newLandRepo(t)
	before := run(t, d, "rev-parse", "main")
	var out strings.Builder
	if err := runLand(context.Background(), landRunFor(d, "n\n", true, &out)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"will be added to main", "[T-0003] backend", "files changed:", "main.go", "Apply? [y/N]", "cancelled"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if run(t, d, "rev-parse", "main") != before {
		t.Fatal("answering n moved main")
	}
	out.Reset()
	if err := runLand(context.Background(), landRunFor(d, "y\n", true, &out)); err != nil {
		t.Fatal(err)
	}
	if run(t, d, "rev-parse", "main") != run(t, d, "rev-parse", "slai/integration") {
		t.Fatal("answering y did not apply")
	}
}

// No terminal and no --yes: refuse, pointing at --yes and --dry-run; --yes applies.
func TestCommitNonTTYRefusesWithoutYes(t *testing.T) {
	d := newLandRepo(t)
	before := run(t, d, "rev-parse", "main")
	var out strings.Builder
	err := runLand(context.Background(), landRunFor(d, "y\n", false, &out))
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("err = %v, want a refusal naming --yes and --dry-run", err)
	}
	if run(t, d, "rev-parse", "main") != before {
		t.Fatal("refusal moved main")
	}
	r := landRunFor(d, "", false, &out)
	r.Yes = true
	if err := runLand(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if run(t, d, "rev-parse", "main") != run(t, d, "rev-parse", "slai/integration") {
		t.Fatal("--yes did not apply")
	}
}

// The plan text is the same for --dry-run and for the prompt.
func TestCommitPlanTextSameForDryRunAndPrompt(t *testing.T) {
	d := newLandRepo(t)
	var dry, prompt strings.Builder
	r := landRunFor(d, "", false, &dry)
	r.Dry = true
	if err := runLand(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := runLand(context.Background(), landRunFor(d, "n\n", true, &prompt)); err != nil {
		t.Fatal(err)
	}
	plan := func(s string) string { return strings.SplitN(s, "\n\n", 2)[0] }
	if plan(dry.String()) != plan(prompt.String()) || !strings.Contains(plan(dry.String()), "files changed:") {
		t.Fatalf("dry-run plan:\n%s\nprompt plan:\n%s", dry.String(), prompt.String())
	}
}

// The preview is advertised in --help (the README is checked by the docs tests).
func TestCommitHelpMentionsPreview(t *testing.T) {
	c := commitCmd()
	if !strings.Contains(c.Long, "Apply? [y/N]") || !strings.Contains(c.Long, "--yes") {
		t.Fatal("slai commit --help does not describe the preview and prompt")
	}
}

func TestREADMEDescribesCommitPreview(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Skip("README not found")
	}
	if !strings.Contains(string(b), "Apply? [y/N]") || !strings.Contains(string(b), "slai commit --yes") {
		t.Fatal("README does not describe the slai commit preview and --yes")
	}
}

func TestREADMEMentionsRevert(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Skip("README not found")
	}
	i := strings.Index(string(b), "### If the team goes wrong")
	if i < 0 || !strings.Contains(string(b)[i:], "slai revert") {
		t.Fatal("README 'If the team goes wrong' does not mention slai revert")
	}
}
