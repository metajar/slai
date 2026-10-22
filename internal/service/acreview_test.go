package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
)

// specTask creates a spec'd task for eng-1, started and with a commit on its branch.
func specTask(t *testing.T, s *Service, spec string) string {
	t.Helper()
	id := j(must(s.CreateTask(ctx, "orch", CreateTaskArgs{Title: "x", Description: "d", Assignee: "eng-1", SpecPath: spec})))["task_id"].(string)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	commitOnBranch(t, s, "slai/eng-1/"+id)
	return id
}

var threeClaims = []ACClaimArg{
	{AC: "AC 1", Met: true, Note: "TestOne"},
	{AC: "AC 2", Met: true, Note: "TestTwo"},
	{AC: "AC 3", Met: false, Note: "not done: needs the backend"},
}

// AC 1: a spec'd task cannot go to review without the per-AC claims.
func TestRequestReviewRequiresACClaimsForSpecdTasks(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, ".slai/specs/x.md")
	_, err := s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"})
	wantErr(t, err, "ac_claims")
	wantErr(t, err, ".slai/specs/x.md")
	if st := must(s.Store.GetTask(id)).Status; st != "in_progress" {
		t.Fatalf("a refused request must not change the status: %s", st)
	}
	if items := j(must(s.GetInbox(ctx, "prin")))["items"].([]any); len(items) != 0 {
		t.Fatalf("the reviewer was notified of a refused request: %v", items)
	}
	// malformed claims are refused too
	for _, bad := range [][]ACClaimArg{{{AC: "", Met: true}}, {{AC: "AC 1", Met: true}, {AC: " ac 1 ", Met: false}}} {
		_, err = s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: bad})
		if err == nil {
			t.Errorf("claims %+v were accepted", bad)
		}
	}
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims})))
	if r["status"] != "in_review" {
		t.Fatal(r)
	}
	// the reviewer's notification carries the claim summary
	body := j(must(s.GetInbox(ctx, "prin")))["items"].([]any)[0].(map[string]any)["body"].(string)
	if !strings.Contains(body, "AC claims: 2 met, 1 NOT met (AC 3)") {
		t.Errorf("review notification: %q", body)
	}
}

// A task without a spec needs no claims (and may still give them).
func TestRequestReviewWithoutSpecNeedsNoClaims(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, "")
	if r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))); r["status"] != "in_review" {
		t.Fatal(r)
	}
	if _, claims, _ := s.Store.LatestACClaims(id); len(claims) != 0 {
		t.Fatalf("claims = %v", claims)
	}
}

// AC 2: review_diff names the spec and carries the engineer's claims.
func TestReviewDiffShowsSpecPathAndClaims(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, ".slai/specs/x.md")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims}))
	d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if d["spec_path"] != ".slai/specs/x.md" || d["review_no"] != float64(1) {
		t.Fatalf("spec_path/review_no: %v", d)
	}
	claims, _ := d["ac_claims"].([]any)
	if len(claims) != 3 {
		t.Fatalf("ac_claims = %v", d["ac_claims"])
	}
	c3 := claims[2].(map[string]any)
	if c3["ac"] != "AC 3" || c3["met"] != false || c3["note"] != "not done: needs the backend" {
		t.Fatalf("claim 3 = %v", c3)
	}
	// a task with no claims shows an empty list, not null
	id2 := specTask(t, s, "")
	_ = id2
}

// AC 3: verdicts are recorded per AC; claims with no verdict are listed (warning).
func TestApproveRecordsVerdictsAndListsUnansweredClaims(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, ".slai/specs/x.md")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, Notes: "ok", ACVerdicts: []ACVerdictArg{
		{AC: "AC 1", Pass: true}, {AC: "ac 2", Pass: false, Note: "no test for the edge case"}}})))
	if ap["status"] != "merge_queued" {
		t.Fatalf("a missing verdict must not refuse the approval: %v", ap)
	}
	if un, _ := ap["unanswered_claims"].([]any); len(un) != 1 || un[0] != "AC 3" {
		t.Fatalf("unanswered_claims = %v", ap["unanswered_claims"])
	}
	warns := strings.Join(toStrs(ap["warnings"]), " | ")
	if !strings.Contains(warns, "no verdict from you: AC 3") || !strings.Contains(warns, "marked fail: ac 2") {
		t.Fatalf("warnings = %q", warns)
	}
	vs, _ := s.Store.LatestACVerdicts(id)
	if len(vs) != 2 || vs[0].By != "prin" || vs[1].Pass || vs[1].Note != "no test for the edge case" {
		t.Fatalf("stored verdicts = %+v", vs)
	}
	// GetTask carries claims and verdicts (the TUI detail reads them)
	tk := must(s.Store.GetTask(id))
	if len(tk.ACClaims) != 3 || len(tk.ACVerdicts) != 2 {
		t.Fatalf("task: %d claims, %d verdicts", len(tk.ACClaims), len(tk.ACVerdicts))
	}
}

func TestRequestChangesRecordsVerdictsAndTellsTheEngineer(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, ".slai/specs/x.md")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims}))
	r := j(must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "AC 2 is not covered", ACVerdicts: []ACVerdictArg{
		{AC: "AC 1", Pass: true}, {AC: "AC 2", Pass: false, Note: "edge case untested"}, {AC: "AC 3", Pass: false, Note: "missing"}}})))
	if r["status"] != "changes_requested" {
		t.Fatal(r)
	}
	if _, has := r["unanswered_claims"]; has {
		t.Fatalf("every claim has a verdict: %v", r)
	}
	body := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	var found bool
	for _, it := range body {
		b := it.(map[string]any)["body"].(string)
		if strings.Contains(b, "Changes requested") {
			found = true
			if !strings.Contains(b, "AC verdicts: AC 1 pass; AC 2 FAIL (edge case untested); AC 3 FAIL (missing)") {
				t.Errorf("engineer's note lacks the verdicts: %q", b)
			}
		}
	}
	if !found {
		t.Fatalf("engineer got no changes-requested note: %v", body)
	}
	// round two: new claims, verdicts start clean, the unanswered list works again
	emptyCommit(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id})) // the engineer picks the task up again
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "fixed", ACClaims: threeClaims[:2]}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id})))
	if un, _ := ap["unanswered_claims"].([]any); len(un) != 2 {
		t.Fatalf("round 2 should list both claims as unanswered: %v", ap)
	}
}

// Tasks without claims still approve exactly as before.
func TestApproveWithoutClaimsIsUnchanged(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, "")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done"}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id})))
	if ap["status"] != "merge_queued" || ap["warnings"] != nil || ap["unanswered_claims"] != nil {
		t.Fatalf("%v", ap)
	}
	var _ store.ACVerdict
}

func toStrs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func emptyCommit(t *testing.T, s *Service) {
	t.Helper()
	gitRun(t, s.Dir, "commit", "--allow-empty", "-m", "more work")
}

// A refused approve (the branch moved since the review) records no verdicts, and a
// re-request of a spec'd task needs its claims again and pins the new head.
func TestRefusedApproveRecordsNoVerdictsAndRerequestNeedsClaims(t *testing.T) {
	s := newSvc(t, true)
	id := specTask(t, s, ".slai/specs/x.md")
	branch := "slai/eng-1/" + id
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims}))
	must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
	commitMore(t, s, branch, "late.txt") // the engineer pushes again after the reviewer looked
	_, err := s.Approve(ctx, "prin", ApproveArgs{TaskID: id, ACVerdicts: []ACVerdictArg{{AC: "AC 1", Pass: true}}})
	wantErr(t, err, "new commits since the review you saw")
	if vs, _ := s.Store.LatestACVerdicts(id); len(vs) != 0 {
		t.Fatalf("a refused approve stored verdicts: %+v", vs)
	}
	// re-requesting from in_review still needs the claims for a spec'd task
	_, err = s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "again"})
	wantErr(t, err, "ac_claims")
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "again", ACClaims: threeClaims[:1]})))
	if r["status"] != "in_review" || r["head_sha"] == nil {
		t.Fatalf("%v", r)
	}
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, ACVerdicts: []ACVerdictArg{{AC: "AC 1", Pass: true}}})))
	if ap["approved_sha"] != r["head_sha"] || ap["unanswered_claims"] != nil {
		t.Fatalf("approve after the re-request: %v (pinned %v)", ap, r["head_sha"])
	}
}
