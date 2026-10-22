// Package git wraps the git CLI via os/exec.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a git repository (or worktree) rooted at Dir.
type Repo struct {
	Dir string
	// Trace, when set, is called with the arguments of every git invocation (tests count them).
	Trace func(args []string)
}

// ConflictError is returned by MergeNoFF when the merge conflicts; the merge has been aborted.
type ConflictError struct {
	Branch string
	Files  []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merge of %s conflicted in %d file(s): %s", e.Branch, len(e.Files), strings.Join(e.Files, ", "))
}

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	if r.Trace != nil {
		r.Trace(args)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (r *Repo) ok(ctx context.Context, args ...string) bool {
	_, err := r.git(ctx, args...)
	return err == nil
}

// IsRepo reports whether Dir is inside a git work tree.
func (r *Repo) IsRepo(ctx context.Context) bool {
	out, err := r.git(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// InitRepo runs git init (initial branch main).
func (r *Repo) InitRepo(ctx context.Context) error {
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	_, err := r.git(ctx, "init", "-b", "main")
	return err
}

func (r *Repo) branchExists(ctx context.Context, name string) bool {
	return r.ok(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
}

// DetectMainBranch returns the main branch: origin/HEAD target, else main, master, else current branch.
func (r *Repo) DetectMainBranch(ctx context.Context) (string, error) {
	if out, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); b != "" && r.branchExists(ctx, b) {
			return b, nil
		}
	}
	for _, b := range []string{"main", "master"} {
		if r.branchExists(ctx, b) {
			return b, nil
		}
	}
	out, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("cannot detect main branch")
	}
	return strings.TrimSpace(out), nil
}

// CreateBranch creates branch name at from (no checkout). Errors if it exists.
func (r *Repo) CreateBranch(ctx context.Context, name, from string) error {
	_, err := r.git(ctx, "branch", name, from)
	return err
}

// EnsureBranch creates the branch from `from` if it does not already exist.
func (r *Repo) EnsureBranch(ctx context.Context, name, from string) error {
	if r.branchExists(ctx, name) {
		return nil
	}
	return r.CreateBranch(ctx, name, from)
}

// AddWorktree adds a worktree at path on newBranch (created from `from` if missing). Idempotent:
// if path is already a worktree it is left alone.
func (r *Repo) AddWorktree(ctx context.Context, path, newBranch, from string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if wts, err := r.worktrees(ctx); err == nil {
		for _, w := range wts {
			if samePath(w, abs) {
				return nil
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if r.branchExists(ctx, newBranch) {
		_, err = r.git(ctx, "worktree", "add", abs, newBranch)
	} else {
		_, err = r.git(ctx, "worktree", "add", "-b", newBranch, abs, from)
	}
	return err
}

func samePath(a, b string) bool {
	if ea, err := filepath.EvalSymlinks(a); err == nil {
		a = ea
	}
	if eb, err := filepath.EvalSymlinks(b); err == nil {
		b = eb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (r *Repo) worktrees(ctx context.Context) ([]string, error) {
	out, err := r.git(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			res = append(res, p)
		}
	}
	return res, nil
}

// RemoveWorktree removes the worktree at path (forced) and prunes. Missing worktrees are not an error.
func (r *Repo) RemoveWorktree(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if wts, err := r.worktrees(ctx); err == nil {
		found := false
		for _, w := range wts {
			if samePath(w, abs) {
				found = true
			}
		}
		if !found {
			_, _ = r.git(ctx, "worktree", "prune")
			return nil
		}
	}
	_, err = r.git(ctx, "worktree", "remove", "--force", abs)
	return err
}

// AddDetached adds a worktree at path checked out detached at rev (a scratch checkout
// that no branch owns). An existing worktree at path is removed first.
func (r *Repo) AddDetached(ctx context.Context, path, rev string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	_ = r.RemoveWorktree(ctx, abs)
	_ = os.RemoveAll(abs)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	_, err = r.git(ctx, "worktree", "add", "--detach", abs, rev)
	return err
}

// FastForward moves the branch checked out in Dir to rev, which must descend from it.
func (r *Repo) FastForward(ctx context.Context, rev string) error {
	_, err := r.git(ctx, "merge", "--ff-only", rev)
	return err
}

// CurrentHead returns the commit SHA of HEAD.
func (r *Repo) CurrentHead(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// RevParse resolves rev to a commit SHA.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.git(ctx, "rev-parse", rev)
	return strings.TrimSpace(out), err
}

// MergeBase returns the best common ancestor of a and b.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.git(ctx, "merge-base", a, b)
	return strings.TrimSpace(out), err
}

// DiffResult is the output of Diff.
type DiffResult struct {
	Files     []string
	Patch     string
	Truncated bool
}

// Diff returns the changes on branch since its merge base with base (base...branch),
// truncating the patch to maxLines lines (maxLines <= 0 means no limit).
func (r *Repo) Diff(ctx context.Context, base, branch string, maxLines int) (*DiffResult, error) {
	rng := base + "..." + branch
	names, err := r.git(ctx, "diff", "--name-only", rng)
	if err != nil {
		return nil, err
	}
	patch, err := r.git(ctx, "diff", rng)
	if err != nil {
		return nil, err
	}
	res := &DiffResult{}
	for _, l := range strings.Split(names, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res.Files = append(res.Files, l)
		}
	}
	if maxLines > 0 {
		lines := strings.SplitAfter(patch, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > maxLines {
			patch = strings.Join(lines[:maxLines], "") + fmt.Sprintf("\n... [truncated: %d more lines]\n", len(lines)-maxLines)
			res.Truncated = true
		}
	}
	res.Patch = patch
	return res, nil
}

// HasCommits reports whether branch has commits not reachable from base.
func (r *Repo) HasCommits(ctx context.Context, base, branch string) (bool, error) {
	out, err := r.git(ctx, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "0", nil
}

// MergeNoFF merges branch into the branch checked out in Dir with --no-ff. On conflict it
// aborts the merge and returns a *ConflictError.
func (r *Repo) MergeNoFF(ctx context.Context, branch, message string) error {
	_, err := r.git(ctx, "merge", "--no-ff", "-m", message, branch)
	if err == nil {
		return nil
	}
	out, derr := r.git(ctx, "diff", "--name-only", "--diff-filter=U")
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	if derr == nil && len(files) > 0 {
		_, _ = r.git(ctx, "merge", "--abort")
		return &ConflictError{Branch: branch, Files: files}
	}
	_, _ = r.git(ctx, "merge", "--abort")
	return err
}

// RebaseOnto rebases the branch checked out in Dir onto onto. The tree must be clean.
// On conflict it aborts the rebase, leaving the branch exactly as it was, and returns a
// *ConflictError naming the files; any other failure also aborts and is returned.
// upToDate is true when the branch already contained onto (nothing was rewritten).
func (r *Repo) RebaseOnto(ctx context.Context, onto string) (upToDate bool, err error) {
	if dirty, derr := r.IsDirty(ctx); derr != nil {
		return false, derr
	} else if dirty {
		return false, fmt.Errorf("worktree %s has uncommitted changes", r.Dir)
	}
	if r.ok(ctx, "merge-base", "--is-ancestor", onto, "HEAD") {
		return true, nil // already contains it
	}
	_, rerr := r.git(ctx, "rebase", onto)
	if rerr == nil {
		return false, nil
	}
	out, _ := r.git(ctx, "diff", "--name-only", "--diff-filter=U")
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	_, _ = r.git(ctx, "rebase", "--abort")
	if len(files) > 0 {
		return false, &ConflictError{Branch: onto, Files: files}
	}
	return false, rerr
}

// MergeCommit finds the merge commit that brought taskID's work into rev (its subject
// is "Merge [T-0001] title"); "" when there is none.
func (r *Repo) MergeCommit(ctx context.Context, rev, taskID string) string {
	out, err := r.git(ctx, "log", "-1", "--format=%h", "--fixed-strings", "--grep=Merge ["+taskID+"]", rev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// RevertMerge reverts the merge commit sha (against its first parent, the branch it was
// merged into) as one new commit with message. On any failure the revert is aborted and
// the tree is left as it was.
func (r *Repo) RevertMerge(ctx context.Context, sha, message string) error {
	if _, err := r.git(ctx, "revert", "-m", "1", "--no-commit", sha); err != nil {
		_, _ = r.git(ctx, "revert", "--abort")
		_, _ = r.git(ctx, "reset", "--hard", "HEAD")
		return err
	}
	if _, err := r.git(ctx, "commit", "--allow-empty", "-m", message); err != nil {
		_, _ = r.git(ctx, "revert", "--abort")
		_, _ = r.git(ctx, "reset", "--hard", "HEAD")
		return err
	}
	return nil
}

// TreeMentions reports whether word appears (as a whole word) in any file of rev's tree.
func (r *Repo) TreeMentions(ctx context.Context, rev, word string) (bool, error) {
	_, err := r.git(ctx, "grep", "-q", "-w", "-F", word, rev)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // git grep: 1 = no match
	}
	return false, err
}

// ResetClean throws away anything uncommitted or half-merged in Dir. Only for
// checkouts slai owns (the integration worktree), never an agent's worktree.
func (r *Repo) ResetClean(ctx context.Context) error {
	_, _ = r.git(ctx, "merge", "--abort")
	if _, err := r.git(ctx, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := r.git(ctx, "clean", "-fdq")
	return err
}

// EnsureGitignoreLines appends any of lines missing from the file at path (creating it). Idempotent.
func EnsureGitignoreLines(path string, lines ...string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add strings.Builder
	for _, l := range lines {
		if !have[l] {
			add.WriteString(l + "\n")
			have[l] = true
		}
	}
	if add.Len() == 0 {
		return nil
	}
	s := string(data)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s+add.String()), 0o644)
}

// IsDirty reports whether the working tree has uncommitted changes (tracked or untracked).
func (r *Repo) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// BranchExists reports whether a local branch exists.
func (r *Repo) BranchExists(ctx context.Context, name string) bool { return r.branchExists(ctx, name) }

// CurrentBranch returns the checked-out branch name ("" when detached).
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// CheckoutBranchFrom does `git checkout -B branch from`, resetting branch to from.
func (r *Repo) CheckoutBranchFrom(ctx context.Context, branch, from string) error {
	_, err := r.git(ctx, "checkout", "-B", branch, from)
	return err
}

// PrepareTaskBranch checks out the task branch in Dir. A fresh branch is
// created from base (`checkout -B`); if the branch already exists and holds
// commits beyond base it is checked out as-is so prior work is never reset.
// Refuses (error) if the working tree is dirty. Returns true if the branch was (re)created.
// StashAll stashes tracked and untracked changes (git stash push -u). It reports
// whether anything was stashed.
func (r *Repo) StashAll(ctx context.Context, msg string) (bool, error) {
	dirty, err := r.IsDirty(ctx)
	if err != nil || !dirty {
		return false, err
	}
	if _, err := r.git(ctx, "stash", "push", "-u", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repo) PrepareTaskBranch(ctx context.Context, branch, base string) (bool, error) {
	if dirty, err := r.IsDirty(ctx); err != nil {
		return false, err
	} else if dirty {
		return false, fmt.Errorf("worktree %s is dirty; refusing to switch to %s", r.Dir, branch)
	}
	// Always `checkout -B <branch> <start>` rather than a plain switch: a plain
	// checkout of the branch we are already on (or one whose ref moved under the
	// worktree) leaves the index at the old tree, so the next `git commit -a` would
	// silently revert whatever landed on the branch since.
	start, created := base, true
	if r.branchExists(ctx, branch) {
		if has, err := r.HasCommits(ctx, base, branch); err == nil && has {
			start, created = branch, false
		}
	}
	if err := r.CheckoutBranchFrom(ctx, branch, start); err != nil {
		return false, err
	}
	// The tree was clean before the switch, so anything dirty now is index/tree
	// drift from the switch itself; resync to HEAD.
	if dirty, err := r.IsDirty(ctx); err != nil {
		return false, err
	} else if dirty {
		if _, err := r.git(ctx, "reset", "-q", "--hard", "HEAD"); err != nil {
			return false, err
		}
	}
	return created, nil
}

// StagedFiles lists paths with changes staged in the index (what the next
// `git commit` would record), as "XY path" porcelain lines.
func (r *Repo) StagedFiles(ctx context.Context) ([]string, error) {
	out, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 3 && l[0] != ' ' && l[0] != '?' && l[0] != '!' {
			files = append(files, l)
		}
	}
	return files, nil
}

// LsTree returns the blob id of each of paths in rev's tree, with one git invocation.
// A path that is not a blob there (missing, or a directory) is absent from the result.
func (r *Repo) LsTree(ctx context.Context, rev string, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 {
		return out, nil
	}
	args := append([]string{"ls-tree", "-z", "--full-tree", rev, "--"}, paths...)
	raw, err := r.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(raw, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode type sha
		if len(f) == 3 && f[1] == "blob" {
			out[path] = f[2]
		}
	}
	return out, nil
}

// DiffNames lists the paths that differ between revisions a and b (renames are
// reported as a delete of the old path and an add of the new one).
func (r *Repo) DiffNames(ctx context.Context, a, b string) ([]string, error) {
	raw, err := r.git(ctx, "diff", "--name-only", "--no-renames", "-z", a, b)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(raw, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}
