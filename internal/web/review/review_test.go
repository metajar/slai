package review_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/review"
	"github.com/oorbt/slai/internal/web/webtest"
)

func newH(t *testing.T, opts ...webtest.Option) *webtest.Harness {
	team := webtest.Setup(func(_ *store.Store, tm *config.Team) {
		tm.Agents = append(tm.Agents, config.Agent{ID: "principal", Role: config.RolePrincipal, Model: "opus"})
	})
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(review.Module), webtest.WithGit(), team}, opts...)...)
	h.Login()
	return h
}

// inReview makes a task in review whose branch has one commit.
func inReview(t *testing.T, h *webtest.Harness, title, file, content string) string {
	t.Helper()
	n := len(h.AllTasks()) + 1
	branch := fmt.Sprintf("slai/eng-1/T-%04d", n)
	tk, err := h.Store.CreateTask(store.Task{Title: title, Assignee: "eng-1", Status: store.StatusInProgress, Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	h.CommitFile(tk.Branch, file, content)
	if err := h.Store.UpdateTaskStatus(tk.ID, store.StatusInReview); err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

func event(h *webtest.Harness, agent, kind, data string) { h.Store.AddEvent(agent, kind, data) }

// P6 AC1: the queue lists in-review tasks oldest first with age and reviewer; a task that is
// not in review is not listed.
func TestQueueIsOldestFirstWithAgeAndReviewer(t *testing.T) {
	h := newH(t)
	a := inReview(t, h, "First asked", "a.go", "package a\n")
	event(h, "eng-1", "request_review", fmt.Sprintf(`{"task_id":%q}`, a))
	h.Advance(38 * time.Minute)
	b := inReview(t, h, "Second asked", "b.go", "package b\n")
	event(h, "eng-1", "request_review", fmt.Sprintf(`{"task_id":%q}`, b))
	h.Advance(2 * time.Minute)
	open, _ := h.Store.CreateTask(store.Task{Title: "Still working", Assignee: "eng-1", Status: store.StatusInProgress})

	r := h.Get("/reviews")
	h.CheckPage(r)
	ia, ib := strings.Index(r.Body, ">"+a+"<"), strings.Index(r.Body, ">"+b+"<")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("queue order: %s at %d, %s at %d", a, ia, b, ib)
	}
	for _, want := range []string{"40m", "2m", "principal", "eng-1", "r1", "First asked", "Second asked"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("queue lacks %q", want)
		}
	}
	if strings.Contains(r.Body, open.ID) {
		t.Error("a task that is not in review is listed")
	}
	// a re-request after changes is round 2
	for _, st := range []string{store.StatusChangesRequested, store.StatusInProgress, store.StatusInReview} {
		if err := h.Store.UpdateTaskStatus(a, st); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(h.Get("/reviews").Body, "r2") {
		t.Error("round after a request_changes should be r2")
	}
}

func TestEmptyQueueSaysWhatHappensNext(t *testing.T) {
	r := newH(t).Get("/reviews")
	if r.Code != 200 || !strings.Contains(r.Body, "Nothing is waiting for review") {
		t.Fatalf("empty state: %d %s", r.Code, r.Body)
	}
}

// Focus-group AC 22: the first in-review item's diff opens expanded, and the diff text is
// escaped data.
func TestFirstItemsDiffIsExpandedAndEscaped(t *testing.T) {
	h := newH(t)
	a := inReview(t, h, "First", "evil.go", "package a\n// <script>alert(1)</script>\n")
	b := inReview(t, h, "Second", "other.go", "package b\n")
	event(h, "eng-1", "request_review", fmt.Sprintf(`{"task_id":%q}`, a))
	h.Advance(time.Minute)
	event(h, "eng-1", "request_review", fmt.Sprintf(`{"task_id":%q}`, b))
	body := h.Get("/reviews").Body
	if !strings.Contains(body, `<details class="diff-file" open>`) || !strings.Contains(body, "evil.go") || !strings.Contains(body, "&#43;package a") {
		t.Fatalf("first item's diff not expanded:\n%s", body)
	}
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("diff text is not escaped")
	}
	if strings.Contains(body, "other.go") {
		t.Error("only the selected task's diff is shown")
	}
	// choosing the second shows its diff
	body = h.Get("/reviews?task=" + b).Body
	if !strings.Contains(body, "other.go") || strings.Contains(body, "evil.go") || !regexp.MustCompile(`aria-current="true">`+b).MatchString(body) {
		t.Errorf("selecting a task:\n%s", body)
	}
	// an unknown ?task= falls back to the first
	if !strings.Contains(h.Get("/reviews?task=T-9999").Body, "evil.go") {
		t.Error("unknown task should fall back to the first in the queue")
	}
}

// P6: claims and verdicts side by side, including claims nobody answered, and the history.
func TestClaimsVerdictsAndHistory(t *testing.T) {
	h := newH(t)
	id := inReview(t, h, "Claims", "c.go", "package c\n")
	event(h, "eng-1", "request_review", fmt.Sprintf(`{"task_id":%q}`, id))
	h.Store.SetACClaims(id, []store.ACClaim{{AC: "AC 1", Met: true, Note: "TestOne"}, {AC: "AC 2", Met: true, Note: "TestTwo"}, {AC: "AC 3", Met: false, Note: "not done"}})
	h.Store.SaveACVerdicts(id, "principal", []store.ACVerdict{{AC: "ac 1", Pass: true, Note: "ok"}, {AC: "AC 2", Pass: false, Note: "<b>wrong</b>"}})
	event(h, "principal", "request_changes", fmt.Sprintf(`{"task_id":%q,"comments":"fix AC 2"}`, id))
	body := h.Get("/reviews").Body
	for _, want := range []string{"Claims of review request 1", "AC 1", "TestOne", "chip-ok", ">pass<", "chip-bad", ">fail<", "chip-idle", ">unanswered<", "&lt;b&gt;wrong&lt;/b&gt;", "Changes requested by principal", "fix AC 2", "Review requested by eng-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>wrong</b>") {
		t.Error("verdict note not escaped")
	}
}

// P6 AC2: merge_refused events show the output tail the event carries; the text is escaped.
func TestMergeRefusedShowsTheOutputTail(t *testing.T) {
	h := newH(t)
	event(h, "eng-1", "merge_refused", `{"task_id":"T-0074","cause":"the tests failed on the merge result","command":"go test ./...","exit":1,"tail":"--- FAIL: TestSpend (0.02s)\n    <boom> got 1 want 2\nFAIL"}`)
	event(h, "eng-1", "merge_refused", `{"task_id":"T-0075","branch":"b","approved":"aa","tip":"bb","cause":"moved on"}`) // older shape: no tail
	body := h.Get("/reviews").Body
	for _, want := range []string{"T-0074", "the tests failed on the merge result", "go test ./...", "exit 1", "--- FAIL: TestSpend", "&lt;boom&gt; got 1 want 2", "T-0075", "moved on"} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, "<boom>") {
		t.Error("tail not escaped")
	}
	if !strings.Contains(newH(t).Get("/reviews").Body, "No merge has been refused") {
		t.Error("empty refusals state")
	}
}

func TestSpecWarningsAreShown(t *testing.T) {
	h := newH(t)
	id := inReview(t, h, "Spec moved", "d.go", "package d\n")
	event(h, "principal", "spec_rewritten", fmt.Sprintf(`{"task_id":%q}`, id))
	event(h, "principal", "spec_addendum", fmt.Sprintf(`{"task_id":%q}`, id))
	body := h.Get("/reviews").Body
	if !strings.Contains(body, "spec rewritten") || !strings.Contains(body, "spec addendum") || !strings.Contains(body, id+": spec rewritten") {
		t.Fatalf("warnings:\n%s", body)
	}
}

// P6 AC3: the page has no approve or merge button, and the module registers no write route.
func TestNoWriteRoutesAndNoApproveButton(t *testing.T) {
	h := newH(t)
	for _, rt := range h.Srv.Routes() {
		if rt.Write() && (rt.Pattern == "/reviews" || rt.Pattern == "/commit-preview" || strings.Contains(rt.Pattern, "approve") || strings.Contains(rt.Pattern, "merge")) {
			t.Errorf("review module exposes write route %s %s", rt.Method, rt.Pattern)
		}
	}
	for _, path := range []string{"/reviews", "/commit-preview"} {
		for _, m := range []string{"POST", "DELETE"} {
			if r := h.Do(h.Browser(m, path, nil)); r.Code == 200 || r.Code == 303 {
				t.Errorf("%s %s = %d", m, path, r.Code)
			}
		}
		body := h.Get(path).Body
		if regexp.MustCompile(`(?i)<form[^>]*method="?post|hx-post|hx-delete`).MatchString(body) || regexp.MustCompile(`(?i)<(button|a|input)\b[^>]*>\s*(approve|merge|apply)`).MatchString(body) {
			t.Errorf("%s has a write control", path)
		}
	}
}

// P11 AC1/AC2: the preview shows the plan text exactly as the actions return it (the cmd test
// proves that text equals `slai commit --dry-run`), escaped; no apply button.
func TestCommitPreviewShowsThePlanText(t *testing.T) {
	h := newH(t)
	h.CommitText = "3 commit(s) from slai/integration will be added to main:\n  abc [T-0071] <b>x</b>\nfiles changed:\n  a.go | 2 +-\nconflicts with main: none (checked with a trial `git merge-tree`)\n\n(dry run: nothing changed)\n"
	r := h.Get("/commit-preview")
	h.CheckPage(r)
	if !strings.Contains(r.Body, "3 commit(s) from slai/integration will be added to main:") || !strings.Contains(r.Body, "conflicts with main: none") || !strings.Contains(r.Body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("plan missing:\n%s", r.Body)
	}
	if strings.Contains(r.Body, "<b>x</b>") {
		t.Error("plan text not escaped")
	}
	h.CommitErr = errors.New("branch slai/integration does not exist yet: the team has not merged anything")
	if r := h.Get("/commit-preview"); !strings.Contains(r.Body, "does not exist yet") {
		t.Errorf("error text not shown: %s", r.Body)
	}
}

func TestRevertPreviewForAMergedTask(t *testing.T) {
	h := newH(t)
	mergedTask, _ := h.Store.CreateTask(store.Task{Title: "Shipped <it>", Assignee: "eng-1", Status: store.StatusMerged})
	h.Store.CreateTask(store.Task{Title: "Not merged", Assignee: "eng-1", Status: store.StatusInProgress})
	h.RevertText[mergedTask.ID] = `will revert merge 7c4be21 of ` + mergedTask.ID + ` "Shipped" on slai/integration` + "\n"
	body := h.Get("/commit-preview").Body
	if !strings.Contains(body, `<option value="`+mergedTask.ID+`">`) || strings.Contains(body, "Not merged") || !strings.Contains(body, "Shipped &lt;it&gt;") {
		t.Fatalf("merged task choices:\n%s", body)
	}
	if strings.Contains(body, "slai revert T-") {
		t.Error("no revert plan before one is chosen")
	}
	body = h.Get("/commit-preview?revert=" + mergedTask.ID).Body
	if !strings.Contains(body, "will revert merge 7c4be21") || !strings.Contains(body, "slai revert "+mergedTask.ID+" --dry-run") || !strings.Contains(body, `selected>`) {
		t.Fatalf("revert plan:\n%s", body)
	}
	h.RevertErr[mergedTask.ID] = errors.New("task " + mergedTask.ID + " has dependents: T-0099 (pass --force)")
	if body := h.Get("/commit-preview?revert=" + mergedTask.ID).Body; !strings.Contains(body, "has dependents: T-0099") {
		t.Errorf("revert error: %s", body)
	}
	if body := h.Get("/commit-preview?revert=T-0000").Body; !strings.Contains(body, "is not merged") {
		t.Errorf("unknown task: %s", body)
	}
	if body := newH(t).Get("/commit-preview").Body; !strings.Contains(body, "nothing to revert") {
		t.Error("empty merged list")
	}
}

// Read-only mode changes nothing here: both pages are reads.
func TestPagesWorkReadOnly(t *testing.T) {
	h := newH(t, webtest.ReadOnly())
	inReview(t, h, "T", "r.go", "package r\n")
	for _, p := range []string{"/reviews", "/commit-preview"} {
		h.CheckPage(h.Get(p))
	}
}
