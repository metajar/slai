package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// webSup is a supervisor over the land repo, enough for the plan previews.
func webSup(t *testing.T, d string) *webActions {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := config.Preset("solo", "demo", "d", "main")
	team.Project.IntegrationBranch = "slai/integration"
	return newWebActions(&supervisor.Supervisor{Dir: d, Team: team, Store: st, Git: &git.Repo{Dir: d}}, func() *config.Team { return team })
}

// P11 AC1: the web preview is the text `slai commit --dry-run` prints, byte for byte, with
// the --stat summary and the statement of how conflicts were checked (focus-group AC 23).
func TestWebCommitPreviewEqualsDryRun(t *testing.T) {
	d := newLandRepo(t)
	a := webSup(t, d)
	got, err := a.CommitPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := buildLandRun(a.Supervisor, "")
	if err != nil {
		t.Fatal(err)
	}
	var cli strings.Builder
	r.Dry, r.Out, r.In = true, &cli, strings.NewReader("")
	if err := runLand(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got != cli.String() {
		t.Fatalf("web preview differs from the dry-run text:\nweb:\n%s\ncli:\n%s", got, cli.String())
	}
	for _, want := range []string{"files changed:", "main.go", "conflicts with main: none (checked with a trial `git merge-tree`", "(dry run: nothing changed)"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview lacks %q:\n%s", want, got)
		}
	}
	if run(t, d, "status", "--porcelain") != "" {
		t.Error("previewing changed the working tree")
	}
}

// AC 23: a conflict is named by the trial merge, the checkout stays untouched, and the plan
// still prints (it is information, not a blocker: `slai commit` itself stops with no change).
func TestCommitPlanNamesConflictsFromTrialMerge(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, "main.go", "package other\n")
	run(t, d, "add", "-A")
	run(t, d, "commit", "-qm", "conflicting")
	head := run(t, d, "rev-parse", "HEAD")
	got, err := webSup(t, d).CommitPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "conflicts with main: main.go (found by a trial `git merge-tree`: your working tree was not touched") {
		t.Fatalf("conflict not reported:\n%s", got)
	}
	if run(t, d, "rev-parse", "HEAD") != head || run(t, d, "status", "--porcelain") != "" {
		t.Error("the trial merge touched the checkout")
	}
	if wl := run(t, d, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("scratch worktree leaked: %s", wl)
	}
}

// A plan that cannot be applied yet still comes back as text, with the reason last.
func TestWebCommitPreviewShowsBlockedPlans(t *testing.T) {
	d := newLandRepo(t)
	write(t, d, "README.md", "# changed\n")
	run(t, d, "add", "-A") // staged: blocks
	got, err := webSup(t, d).CommitPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "BLOCKED: uncommitted change") || !strings.HasSuffix(strings.TrimSpace(got), "cannot commit yet; see above") {
		t.Fatalf("blocked plan:\n%s", got)
	}
	// no integration branch yet: an error, not text
	run(t, d, "reset", "-q", "--hard")
	run(t, d, "branch", "-q", "-D", "slai/integration")
	if _, err := webSup(t, d).CommitPreview(context.Background()); err == nil || !strings.Contains(err.Error(), "does not exist yet") {
		t.Fatalf("no integration branch: %v", err)
	}
}

func TestRevertPlanTextAndPreviewErrors(t *testing.T) {
	plan := supervisor.RevertPlan{Task: store.Task{ID: "T-0076", Title: "False alert"}, MergeSHA: "7c4be21", Integration: "slai/integration",
		Later: []string{"T-0080"}, OpenAfter: []string{"T-0090"}}
	got := revertPlanText(plan)
	for _, want := range []string{`will revert merge 7c4be21 of T-0076 "False alert" on slai/integration`, "forced: T-0080 already merged", "note: T-0090 (not merged yet)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	d := newLandRepo(t)
	if _, err := webSup(t, d).RevertPreview(context.Background(), "T-9999"); err == nil {
		t.Error("unknown task should be an error")
	}
}
