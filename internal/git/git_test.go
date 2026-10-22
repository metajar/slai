package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func setup(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@e.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		t.Setenv(k, v)
	}
	r := &Repo{Dir: t.TempDir()}
	if r.IsRepo(ctx) {
		t.Skip("tempdir inside a repo")
	}
	if err := r.InitRepo(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.IsRepo(ctx) {
		t.Fatal("not repo")
	}
	commit(t, r, "a.txt", "one\n", "init")
	return r
}

func commit(t *testing.T, r *Repo, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git(ctx, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git(ctx, "commit", "-m", msg); err != nil {
		t.Fatal(err)
	}
}

func TestBranchesWorktreesDiff(t *testing.T) {
	r := setup(t)
	main, err := r.DetectMainBranch(ctx)
	if err != nil || main != "main" {
		t.Fatalf("main=%q %v", main, err)
	}
	if err := r.EnsureBranch(ctx, "slai/integration", main); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureBranch(ctx, "slai/integration", main); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateBranch(ctx, "slai/integration", main); err == nil {
		t.Fatal("create existing should fail")
	}
	wt := filepath.Join(r.Dir, ".slai", "worktrees", "eng-1")
	for i := 0; i < 2; i++ {
		if err := r.AddWorktree(ctx, wt, "slai/eng-1/t1", "slai/integration"); err != nil {
			t.Fatal(err)
		}
	}
	w := &Repo{Dir: wt}
	if ok, _ := r.HasCommits(ctx, "slai/integration", "slai/eng-1/t1"); ok {
		t.Fatal("no commits yet")
	}
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("line\n")
	}
	commit(t, w, "b.txt", sb.String(), "add b")
	if ok, _ := r.HasCommits(ctx, "slai/integration", "slai/eng-1/t1"); !ok {
		t.Fatal("expected commits")
	}
	d, err := r.Diff(ctx, "slai/integration", "slai/eng-1/t1", 10)
	if err != nil || !d.Truncated || len(d.Files) != 1 || d.Files[0] != "b.txt" {
		t.Fatalf("diff %+v %v", d, err)
	}
	d, _ = r.Diff(ctx, "slai/integration", "slai/eng-1/t1", 0)
	if d.Truncated || !strings.Contains(d.Patch, "+line") {
		t.Fatal("full diff wrong")
	}
	if h, err := w.CurrentHead(ctx); err != nil || len(h) < 40 {
		t.Fatalf("head %q %v", h, err)
	}
	if err := r.RemoveWorktree(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveWorktree(ctx, wt); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestMerge(t *testing.T) {
	r := setup(t)
	_, _ = r.git(ctx, "checkout", "-b", "slai/integration")
	_ = r.CreateBranch(ctx, "ok", "main")
	_ = r.CreateBranch(ctx, "bad", "main")
	_, _ = r.git(ctx, "checkout", "ok")
	commit(t, r, "c.txt", "c\n", "c")
	_, _ = r.git(ctx, "checkout", "bad")
	commit(t, r, "a.txt", "bad\n", "bad")
	_, _ = r.git(ctx, "checkout", "slai/integration")
	commit(t, r, "a.txt", "integration\n", "int")

	if err := r.MergeNoFF(ctx, "ok", "merge ok"); err != nil {
		t.Fatal(err)
	}
	out, _ := r.git(ctx, "rev-list", "--merges", "--count", "HEAD")
	if strings.TrimSpace(out) != "1" {
		t.Fatalf("expected merge commit, got %q", out)
	}
	err := r.MergeNoFF(ctx, "bad", "merge bad")
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Files) != 1 || ce.Files[0] != "a.txt" {
		t.Fatalf("expected conflict, got %v", err)
	}
	if st, _ := r.git(ctx, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("not clean after abort: %q", st)
	}
}

func TestEnsureGitignoreLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".gitignore")
	_ = os.WriteFile(p, []byte("node_modules"), 0o644)
	for i := 0; i < 2; i++ {
		if err := EnsureGitignoreLines(p, ".slai/state.db*", "node_modules", ".slai/run/"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if string(b) != "node_modules\n.slai/state.db*\n.slai/run/\n" {
		t.Fatalf("got %q", b)
	}
}

func TestPrepareTaskBranchStartsCleanFromBase(t *testing.T) {
	r := setup(t)
	if err := r.CreateBranch(ctx, "slai/integration", "main"); err != nil {
		t.Fatal(err)
	}
	wt := &Repo{Dir: filepath.Join(t.TempDir(), "wt")}
	if err := r.AddWorktree(ctx, wt.Dir, "slai/eng-1/T-1", "slai/integration"); err != nil {
		t.Fatal(err)
	}
	// integration moves on while the task branch (no work of its own) sits in the worktree
	commit(t, r, "feature.txt", "new\n", "feature")
	if _, err := r.git(ctx, "branch", "-f", "slai/integration", "main"); err != nil {
		t.Fatal(err)
	}
	created, err := wt.PrepareTaskBranch(ctx, "slai/eng-1/T-1", "slai/integration")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "feature.txt")); err != nil {
		t.Fatalf("new integration file missing: %v", err)
	}
	if dirty, _ := wt.IsDirty(ctx); dirty {
		t.Fatal("worktree dirty after PrepareTaskBranch")
	}
	if staged, _ := wt.StagedFiles(ctx); len(staged) != 0 {
		t.Fatalf("staged: %v", staged)
	}
	// a branch with its own commits is kept
	commit(t, wt, "mine.txt", "x\n", "work")
	created, err = wt.PrepareTaskBranch(ctx, "slai/eng-1/T-1", "slai/integration")
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "mine.txt")); err != nil {
		t.Fatal("existing work lost")
	}
}

func TestPrepareTaskBranchResyncsStaleIndex(t *testing.T) {
	r := setup(t)
	// Move the checked-out branch's ref under the worktree: the index stays at the old tree.
	commit(t, r, "feature.txt", "new\n", "feature")
	if _, err := r.git(ctx, "branch", "T", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	wt := &Repo{Dir: filepath.Join(t.TempDir(), "wt")}
	if err := r.AddWorktree(ctx, wt.Dir, "T", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git(ctx, "update-ref", "refs/heads/T", "main"); err != nil {
		t.Fatal(err)
	}
	if staged, _ := wt.StagedFiles(ctx); len(staged) == 0 {
		t.Fatal("expected a stale staged index")
	}
	// the supervisor stashes dirty state first; emulate that
	if _, err := wt.StashAll(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.PrepareTaskBranch(ctx, "T", "main"); err != nil {
		t.Fatal(err)
	}
	if staged, _ := wt.StagedFiles(ctx); len(staged) != 0 {
		t.Fatalf("staged after prepare: %v", staged)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, "feature.txt")); err != nil {
		t.Fatal("tree not at base tip")
	}
}

func TestRebaseOnto(t *testing.T) {
	r := setup(t)
	base, _ := r.CurrentBranch(ctx)
	mustGit := func(args ...string) string {
		out, err := r.git(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	mustGit("checkout", "-q", "-b", "work")
	commit(t, r, "mine.txt", "mine\n", "my work")
	mustGit("checkout", "-q", base)
	commit(t, r, "theirs.txt", "theirs\n", "landed")
	mustGit("checkout", "-q", "work")

	up, err := r.RebaseOnto(ctx, base)
	if err != nil || up {
		t.Fatalf("clean rebase: upToDate=%v err=%v", up, err)
	}
	if out := mustGit("log", "--format=%s", "-3"); !strings.HasPrefix(out, "my work\nlanded") {
		t.Fatalf("history after rebase:\n%s", out)
	}
	if up, err := r.RebaseOnto(ctx, base); err != nil || !up {
		t.Fatalf("second rebase should be a no-op: %v %v", up, err)
	}

	// conflict: aborted, branch exactly as it was
	mustGit("checkout", "-q", "-b", "clash", base)
	commit(t, r, "a.txt", "clash\n", "edit a")
	mustGit("checkout", "-q", base)
	commit(t, r, "a.txt", "main side\n", "edit a differently")
	mustGit("checkout", "-q", "clash")
	before := mustGit("rev-parse", "HEAD")
	_, err = r.RebaseOnto(ctx, base)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Files) != 1 || ce.Files[0] != "a.txt" {
		t.Fatalf("expected a conflict on a.txt, got %v", err)
	}
	if mustGit("rev-parse", "HEAD") != before || mustGit("status", "--porcelain") != "" {
		t.Fatal("a conflicting rebase was not fully aborted")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, ".git", "rebase-merge")); err == nil {
		t.Fatal("rebase state left behind")
	}

	// dirty tree: refused, untouched
	if err := os.WriteFile(filepath.Join(r.Dir, "wip.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RebaseOnto(ctx, base); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty tree: %v", err)
	}
	if mustGit("rev-parse", "HEAD") != before {
		t.Fatal("dirty rebase moved HEAD")
	}
}

func TestMergeCommitFindsTheTasksMerge(t *testing.T) {
	r := setup(t)
	base, _ := r.CurrentBranch(ctx)
	if _, err := r.git(ctx, "checkout", "-q", "-b", "t1"); err != nil {
		t.Fatal(err)
	}
	commit(t, r, "x.txt", "x\n", "[T-0001] work")
	if _, err := r.git(ctx, "checkout", "-q", base); err != nil {
		t.Fatal(err)
	}
	if err := r.MergeNoFF(ctx, "t1", "Merge [T-0001] the work"); err != nil {
		t.Fatal(err)
	}
	want, _ := r.git(ctx, "rev-parse", "--short", "HEAD")
	if got := r.MergeCommit(ctx, base, "T-0001"); got == "" || !strings.HasPrefix(strings.TrimSpace(want), got[:4]) {
		t.Fatalf("MergeCommit = %q, head %q", got, want)
	}
	if r.MergeCommit(ctx, base, "T-0002") != "" {
		t.Fatal("found a merge for a task that never merged")
	}
}
