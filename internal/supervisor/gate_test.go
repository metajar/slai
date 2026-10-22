package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func inReviewTask(t *testing.T, s *Supervisor, title, file string) store.Task {
	t.Helper()
	tk, _ := s.Store.CreateTask(store.Task{Title: title, Assignee: "eng-1", Status: store.StatusInReview})
	s.Store.UpdateTaskFields(tk.ID, store.Task{Branch: TaskBranch("eng-1", tk.ID)})
	tk, _ = s.Store.GetTask(tk.ID)
	commitOnTask(t, s, tk, file, title+"\n")
	return tk
}

func integrationHead(t *testing.T, s *Supervisor) string {
	return strings.TrimSpace(sh(t, s.Dir, "git", "rev-parse", "slai/integration"))
}

// Two branches that are each fine and together are not: the second merge is refused,
// integration stays on the first, and the engineer and principal each hear once, with
// the last 40 lines of the output.
func TestMergeGateRefusesCombinedRed(t *testing.T) {
	s := tmuxEnv(t, team(5))
	ctx := context.Background()
	// green alone, red when both x.txt and y.txt exist; noisy so the tail matters
	s.Team.Project.TestCommand = `seq 1 100; if [ -f x.txt ] && [ -f y.txt ]; then echo COMBINED-RED; exit 1; fi`
	a := inReviewTask(t, s, "adds x", "x.txt")
	requestMerge(s, a.ID)
	s.Tick(ctx)
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusMerged {
		t.Fatalf("a green merge was refused: %s", g.Status)
	}
	afterA := integrationHead(t, s)
	if !strings.Contains(integrationFiles(t, s), "x.txt") {
		t.Fatal("first merge did not land")
	}

	b := inReviewTask(t, s, "adds y", "y.txt")
	sh(t, filepath.Join(s.Dir, ".slai", "worktrees", "eng-1"), "git", "merge", "--no-edit", "slai/integration") // branch is up to date with x.txt
	requestMerge(s, b.ID)
	s.Tick(ctx)
	s.Tick(ctx) // the approval is consumed: nothing repeats

	if got := integrationHead(t, s); got != afterA {
		t.Fatalf("integration moved to %s although the tests failed", got)
	}
	if g, _ := s.Store.GetTask(b.ID); g.Status != store.StatusInReview {
		t.Fatalf("status = %s, want in_review", g.Status)
	}
	eng, _ := s.Store.UndeliveredFor("eng-1")
	prin, _ := s.Store.UndeliveredFor("principal")
	if countBody(eng, "COMBINED-RED") != 1 || countBody(prin, "COMBINED-RED") != 1 {
		t.Fatalf("each of engineer and principal should hear once: eng %+v prin %+v", eng, prin)
	}
	for _, m := range append(eng, prin...) {
		if strings.Contains(m.Body, "COMBINED-RED") {
			if !strings.Contains(m.Body, "\n100\n") || strings.Contains(m.Body, "\n61\n") || !strings.Contains(m.Body, "\n62\n") {
				t.Fatalf("note should carry exactly the last 40 lines (62..100 and the marker):\n%s", m.Body)
			}
		}
	}
	if !hasEvent(s, "merge_refused") || countEvents(s, "merge_refused") != 1 {
		t.Fatalf("merge_refused events: %d", countEvents(s, "merge_refused"))
	}
	// the event carries the output tail itself (the web Reviews page shows it from there)
	evs, _ := s.Store.RecentEvents(500)
	var tail string
	for _, e := range evs {
		if e.Kind == "merge_refused" {
			var d struct {
				Tail string `json:"tail"`
			}
			_ = json.Unmarshal([]byte(e.Data), &d)
			tail = d.Tail
		}
	}
	if !strings.Contains(tail, "COMBINED-RED") || !strings.Contains(tail, "\n100") {
		t.Fatalf("merge_refused event lacks the output tail: %q", tail)
	}
	if out := sh(t, s.Dir, "git", "worktree", "list"); strings.Contains(out, "_gate") {
		t.Fatalf("scratch gate worktree left behind:\n%s", out)
	}
}

// A passing command proceeds and the merge result is what was tested.
func TestMergeGatePassingCommandMerges(t *testing.T) {
	s := tmuxEnv(t, team(5))
	marker := filepath.Join(t.TempDir(), "ran-in")
	s.Team.Project.TestCommand = `pwd > ` + marker + `; test -f x.txt`
	a := inReviewTask(t, s, "adds x", "x.txt")
	requestMerge(s, a.ID)
	s.Tick(context.Background())
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusMerged || !strings.Contains(integrationFiles(t, s), "x.txt") {
		t.Fatalf("passing gate did not merge: %s", g.Status)
	}
	if b, _ := os.ReadFile(marker); !strings.Contains(string(b), "_gate") {
		t.Fatalf("the command should run in the scratch merge checkout, ran in %q", b)
	}
}

// A hanging command is killed after the timeout and reported as timed out.
func TestMergeGateTimeoutIsAFailure(t *testing.T) {
	s := tmuxEnv(t, team(5))
	s.Team.Project.TestCommand = `echo started; sleep 60`
	s.Team.Project.TestTimeoutSeconds = 1
	a := inReviewTask(t, s, "adds x", "x.txt")
	before := integrationHead(t, s)
	requestMerge(s, a.ID)
	start := time.Now()
	s.Tick(context.Background())
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("a hanging command held the merge for %s", d)
	}
	if integrationHead(t, s) != before {
		t.Fatal("integration moved after a timeout")
	}
	eng, _ := s.Store.UndeliveredFor("eng-1")
	if countBody(eng, "timed out") != 1 || countBody(eng, "started") != 1 {
		t.Fatalf("engineer note: %+v", eng)
	}
}

// Unset: the merge path is the old one (no scratch checkout, merge lands directly).
func TestMergeWithoutGateUsesNoScratchCheckout(t *testing.T) {
	s := tmuxEnv(t, team(5))
	a := inReviewTask(t, s, "adds x", "x.txt")
	requestMerge(s, a.ID)
	s.Tick(context.Background())
	if g, _ := s.Store.GetTask(a.ID); g.Status != store.StatusMerged {
		t.Fatalf("status %s", g.Status)
	}
	if _, err := os.Stat(s.gateDir()); err == nil {
		t.Fatal("an ungated merge created a scratch checkout")
	}
}
