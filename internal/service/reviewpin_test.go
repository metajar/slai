package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitMore(t *testing.T, s *Service, branch, file string) string {
	t.Helper()
	gitRun(t, s.Dir, "checkout", branch)
	os.WriteFile(filepath.Join(s.Dir, file), []byte("more\n"), 0o644)
	gitRun(t, s.Dir, "add", "-A")
	gitRun(t, s.Dir, "commit", "-m", "more "+file)
	sha := gitOut(t, s.Dir, "rev-parse", "HEAD")
	gitRun(t, s.Dir, "checkout", "slai/integration")
	return sha
}

func lastMergeRequest(t *testing.T, s *Service) map[string]string {
	t.Helper()
	for _, e := range must(s.Store.RecentEvents(200)) { // newest first
		if e.Kind == "merge_requested" {
			var d map[string]string
			_ = json.Unmarshal([]byte(e.Data), &d)
			return d
		}
	}
	t.Fatal("no merge_requested event")
	return nil
}

// An approval is of one commit: review_diff names it, approve pins it, the merge
// request carries it; a branch that moved since the review cannot be approved by accident.
func TestApprovalPinsTheReviewedCommit(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	branch := "slai/eng-1/" + id
	commitOnBranch(t, s, branch)
	head1 := gitOut(t, s.Dir, "rev-parse", branch)

	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"})))
	if r["head_sha"] != head1 {
		t.Fatalf("request_review should record the head: %v", r)
	}
	d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if d["reviewed_sha"] != head1 {
		t.Fatalf("review_diff should name the commit it showed: %v", d)
	}

	// the engineer commits again before the principal approves
	head2 := commitMore(t, s, branch, "late.txt")
	_, err := s.Approve(ctx, "prin", ApproveArgs{TaskID: id, Notes: "lgtm"})
	wantErr(t, err, "new commits since the review you saw")
	for _, e := range must(s.Store.RecentEvents(50)) {
		if e.Kind == "merge_requested" {
			t.Fatal("a merge was queued for a branch that moved after the review")
		}
	}

	// reviewing again shows the new head, and approving pins exactly it
	d = j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if d["reviewed_sha"] != head2 || !strings.Contains(d["patch"].(string), "late.txt") {
		t.Fatalf("second review_diff: %v", d["reviewed_sha"])
	}
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, Notes: "lgtm"})))
	if ap["approved_sha"] != head2 {
		t.Fatalf("approve: %v", ap)
	}
	if mr := lastMergeRequest(t, s); mr["sha"] != head2 || mr["task_id"] != id {
		t.Fatalf("merge request = %v, want sha %s", mr, head2)
	}
}

// Without a review_diff the approval pins what request_review recorded.
func TestApprovalWithoutReviewDiffPinsTheRequestedHead(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	branch := "slai/eng-1/" + id
	commitOnBranch(t, s, branch)
	head1 := gitOut(t, s.Dir, "rev-parse", branch)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id})))
	if ap["approved_sha"] != head1 || lastMergeRequest(t, s)["sha"] != head1 {
		t.Fatalf("approve: %v", ap)
	}
	// and a commit after request_review (no review_diff yet) is caught too
	s2 := newSvc(t, true)
	id2 := createTask(t, s2)
	must(s2.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id2}))
	commitOnBranch(t, s2, "slai/eng-1/"+id2)
	must(s2.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id2, Summary: "done"}))
	commitMore(t, s2, "slai/eng-1/"+id2, "sneaky.txt")
	_, err := s2.Approve(ctx, "prin", ApproveArgs{TaskID: id2})
	wantErr(t, err, "new commits")
}

// A second request_review while the task is in review (after more commits) re-pins the
// head and tells the principal again.
func TestRequestReviewAgainWhileInReviewRepins(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	branch := "slai/eng-1/" + id
	commitOnBranch(t, s, branch)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "first"}))
	must(s.GetInbox(ctx, "prin"))
	head2 := commitMore(t, s, branch, "more.txt")
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "second"})))
	if r["status"] != "in_review" || r["head_sha"] != head2 {
		t.Fatalf("re-request: %v", r)
	}
	if in := j(must(s.GetInbox(ctx, "prin")))["items"].([]any); len(in) != 1 || !strings.Contains(in[0].(map[string]any)["body"].(string), head2[:8]) {
		t.Fatalf("principal should be asked again, naming the commit: %v", in)
	}
	if ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id}))); ap["approved_sha"] != head2 {
		t.Fatalf("approve after re-request: %v", ap)
	}
}

// review_diff charges the reviewer's tokens to the reviewed task; approve and
// request_changes stop doing so.
func TestReviewerSpendCountsTowardTheTask(t *testing.T) {
	for _, verdict := range []string{"approve", "request_changes"} {
		t.Run(verdict, func(t *testing.T) {
			curT = t
			s := newSvc(t, true)
			id := createTask(t, s)
			must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
			commitOnBranch(t, s, "slai/eng-1/"+id)
			must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
			taskOf := func() string {
				if err := s.Store.AddUsage("prin", 10, 5, 0.01); err != nil {
					t.Fatal(err)
				}
				u, err := s.Store.UsageByTask(0)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := u[id]; ok {
					return id
				}
				return ""
			}
			if taskOf() != "" {
				t.Fatal("usage before review_diff was charged to the task")
			}
			must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
			if got := taskOf(); got != id {
				t.Fatalf("usage after review_diff carries task %q, want %q", got, id)
			}
			if verdict == "approve" {
				must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id}))
			} else {
				must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "fix"}))
			}
			// The verdict is called mid-turn: the usage row for this turn is not written yet, so the
			// charge must still be on until the Stop hook has recorded it.
			if cur := s.Store.AgentTask("prin"); cur != id {
				t.Fatalf("current_task right after %s = %q, want %q until the Stop hook", verdict, cur, id)
			}
			stop := func(prompt int64) {
				t.Helper()
				if _, err := hooks.Handle(s.Store, "prin", "stop", hooks.Payload{Usage: &hooks.NeutralUsage{Prompt: prompt, Output: 1, Cost: 0.5}}, hooks.Options{}); err != nil {
					t.Fatal(err)
				}
			}
			stop(100) // the review turn's own usage row
			if cur := s.Store.AgentTask("prin"); cur != "" {
				t.Fatalf("current_task after the Stop hook = %q, want empty", cur)
			}
			if _, ok, _ := s.Store.GetKV(store.KVReviewEndCharge + "prin"); ok {
				t.Fatal("end-charge key left behind")
			}
			u := must(s.Store.UsageByTask(0))
			if got := u[id].CostUSD; got < 0.5 {
				t.Fatalf("review turn's usage not on the task: %+v", u[id])
			}
			stop(200) // a second turn after the verdict carries no task
			if again := must(s.Store.UsageByTask(0))[id].CostUSD; again != u[id].CostUSD {
				t.Fatalf("second turn charged to the task: %v -> %v", u[id].CostUSD, again)
			}
		})
	}
}

// The T-0010 case: the reviewer saw one commit, the engineer rebased and re-requested review,
// and approve must not silently take the new head.
func TestApproveRefusesWhenHeadMovedSinceReviewDiff(t *testing.T) {
	setup := func(t *testing.T) (*Service, string, string, string) {
		curT = t
		s := newSvc(t, true)
		id := createTask(t, s)
		must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
		branch := "slai/eng-1/" + id
		commitOnBranch(t, s, branch)
		head1 := gitOut(t, s.Dir, "rev-parse", branch)
		must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
		must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
		head2 := commitMore(t, s, branch, "late.txt")
		must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "again"}))
		return s, id, head1, head2
	}
	t.Run("refused naming both shas", func(t *testing.T) {
		s, id, head1, head2 := setup(t)
		_, err := s.Approve(ctx, "prin", ApproveArgs{TaskID: id})
		if err == nil || !strings.Contains(err.Error(), head1[:8]) || !strings.Contains(err.Error(), head2[:8]) || !strings.Contains(err.Error(), "head_moved_ok") {
			t.Fatalf("err = %v", err)
		}
		for _, e := range must(s.Store.RecentEvents(50)) {
			if e.Kind == "merge_requested" {
				t.Fatal("a merge was queued")
			}
		}
	})
	t.Run("head_moved_ok approves the current head", func(t *testing.T) {
		s, id, _, head2 := setup(t)
		ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, HeadMovedOK: true})))
		if ap["approved_sha"] != head2 || lastMergeRequest(t, s)["sha"] != head2 {
			t.Fatalf("approve: %v", ap)
		}
	})
	t.Run("a fresh review_diff approves normally", func(t *testing.T) {
		s, id, _, head2 := setup(t)
		must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
		if ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id}))); ap["approved_sha"] != head2 {
			t.Fatalf("approve: %v", ap)
		}
	})
}

// If the Stop hook never fires (or its recording failed), the supervisor's idle fallback ends
// the charge through the same store call.
func TestReviewChargeEndsViaStoreFallback(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
	must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
	must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id}))
	if !s.Store.FinishReviewCharge("prin") || s.Store.AgentTask("prin") != "" {
		t.Fatalf("fallback did not clear the charge: task %q", s.Store.AgentTask("prin"))
	}
	if s.Store.FinishReviewCharge("prin") {
		t.Fatal("nothing pending the second time")
	}
}

// Several reviews in one turn: the second review_diff takes over the charge the first verdict
// left waiting for the Stop hook, so the turn's row goes to the newest reviewed task.
func TestBackToBackReviewsInOneTurnChargeTheNewest(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	var ids []string
	for i := 0; i < 2; i++ {
		id := createTask(t, s)
		must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
		commitOnBranch(t, s, "slai/eng-1/"+id)
		must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
		ids = append(ids, id)
	}
	a, b := ids[0], ids[1]
	stop := func(prompt int64) {
		t.Helper()
		if _, err := hooks.Handle(s.Store, "prin", "stop", hooks.Payload{Usage: &hooks.NeutralUsage{Prompt: prompt, Output: 1, Cost: 1}}, hooks.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: a}))
	must(s.Approve(ctx, "prin", ApproveArgs{TaskID: a}))
	must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: b}))
	stop(100)
	u := must(s.Store.UsageByTask(0))
	if u[b].CostUSD != 1 || u[a].CostUSD != 0 {
		t.Fatalf("turn row should go to %s only: a=%+v b=%+v", b, u[a], u[b])
	}
	if cur := s.Store.AgentTask("prin"); cur != b {
		t.Fatalf("current_task = %q, want %q (its charge is still live)", cur, b)
	}
	if _, ok, _ := s.Store.GetKV(store.KVReviewEndCharge + "prin"); ok {
		t.Fatal("end-charge key left behind")
	}
	must(s.Approve(ctx, "prin", ApproveArgs{TaskID: b}))
	stop(200)
	if got := must(s.Store.UsageByTask(0))[b].CostUSD; got != 2 {
		t.Fatalf("b's spend = %v, want 2", got)
	}
	if cur := s.Store.AgentTask("prin"); cur != "" {
		t.Fatalf("current_task = %q after the last Stop, want empty", cur)
	}
}

// review_diff checks the reviewed commit out in .slai/worktrees/_review and says where; the
// checkout follows the branch head when the engineer pushes more and never touches the
// project checkout.
func TestReviewDiffChecksOutTheReviewedCommit(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
	before := gitOut(t, s.Dir, "rev-parse", "--abbrev-ref", "HEAD")

	out := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	dir, _ := out["review_checkout"].(string)
	sha, _ := out["reviewed_sha"].(string)
	if dir != filepath.Join(s.Dir, ".slai", "worktrees", "_review") || sha == "" || out["review_checkout_sha"] != sha {
		t.Fatalf("response lacks the review checkout: %v", out)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("_review is at %s, want the reviewed commit %s", got, sha)
	}
	if b := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		t.Fatalf("_review is on branch %s, want a detached HEAD", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatalf("the reviewed change is not in the checkout: %v", err)
	}
	if after := gitOut(t, s.Dir, "rev-parse", "--abbrev-ref", "HEAD"); after != before {
		t.Fatalf("the project checkout moved from %s to %s", before, after)
	}

	// the engineer pushes another commit and the principal reviews again: the checkout follows
	os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("left by a test run"), 0o644)
	sha2 := commitMore(t, s, "slai/eng-1/"+id, "c.txt")
	out = j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if out["reviewed_sha"] != sha2 || gitOut(t, dir, "rev-parse", "HEAD") != sha2 {
		t.Fatalf("checkout did not follow the new head %s: %v", sha2, out["review_checkout_sha"])
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); err == nil {
		t.Fatal("scratch files from the previous review survived")
	}
}
