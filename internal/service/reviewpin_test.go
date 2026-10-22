package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
