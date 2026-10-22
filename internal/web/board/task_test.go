package board_test

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/board"
)

func page(t *testing.T, fx *fix, id string) string {
	t.Helper()
	r := fx.h.Get("/tasks/" + id)
	fx.h.CheckPage(r)
	return r.Body
}

// P5 AC2 + focus-group AC15: per-AC pass/fail with notes, "unanswered" claims, and one summary line.
func TestTaskShowsClaimsVerdictsAndSummary(t *testing.T) {
	fx := newFix(t)
	id := fx.ids["review"]
	if _, err := fx.h.Store.SetACClaims(id, []store.ACClaim{{AC: "AC 1", Met: true, Note: "TestOne"}, {AC: "AC 2", Met: true, Note: "TestTwo"}, {AC: "AC 3", Met: false, Note: "not done: needs eng-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := fx.h.Store.SaveACVerdicts(id, "principal", []store.ACVerdict{{AC: "AC 1", Pass: true, Note: "good"}, {AC: "ac 2", Pass: false, Note: "no test"}}); err != nil {
		t.Fatal(err)
	}
	b := page(t, fx, id)
	if !strings.Contains(b, "1 of 3 criteria pass, 1 fail, 1 unanswered") {
		t.Errorf("summary missing: %s", between(b, `id="ac-summary"`, "</p>"))
	}
	// the summary sits above the table
	if strings.Index(b, "ac-summary") > strings.Index(b, "<table") {
		t.Error("summary is not above the AC table")
	}
	for _, w := range []string{"TestOne", "good", "no test", "unanswered", "the reviewer has not answered this claim yet", "not met", "principal"} {
		if !strings.Contains(b, w) {
			t.Errorf("AC table lacks %q", w)
		}
	}
	// no claims yet
	if b := page(t, fx, fx.ids["assigned"]); !strings.Contains(b, "No acceptance criteria have been claimed yet.") {
		t.Error("empty claims message missing")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], b)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

func patchOf(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "diff --git a/f%02d.go b/f%02d.go\nindex 1..2 100644\n--- a/f%02d.go\n+++ b/f%02d.go\n@@ -1,2 +1,2 @@\n ctx <b>line</b>\n-old\n+new\n", i, i, i, i)
	}
	return b.String()
}

// P5 AC3 + focus-group AC15: unified, escaped diff for in_review tasks, expanded; files collapse past 30.
func TestTaskDiffExpandedForInReviewAndCollapsedWhenLarge(t *testing.T) {
	fx := newFix(t)
	fx.f.diff = &board.Diff{Patch: patchOf(3), Base: "slai/integration"}
	b := page(t, fx, fx.ids["review"])
	if n := strings.Count(b, `<details class="slai-diff-file" open>`); n != 3 {
		t.Errorf("%d of 3 files open, want all", n)
	}
	for _, w := range []string{"f00.go", "+1", "−1", "against slai/integration", `<span class="slai-ln slai-ln-add">&#43;new`, `<span class="slai-ln slai-ln-del">-old`, "ctx &lt;b&gt;line&lt;/b&gt;"} {
		if !strings.Contains(b, w) {
			t.Errorf("diff lacks %q", w)
		}
	}
	if strings.Contains(b, "<b>line</b>") {
		t.Error("diff text is not escaped")
	}
	fx.f.diff = &board.Diff{Patch: patchOf(32), Base: "slai/integration"}
	b = page(t, fx, fx.ids["review"])
	if n := strings.Count(b, `<details class="slai-diff-file" open>`); n != 1 {
		t.Errorf("%d files open of 32, want only the first", n)
	}
	if !strings.Contains(b, "32 files: only the first starts open") {
		t.Error("collapse note missing")
	}
	// a task that is not in review says so instead
	if b := page(t, fx, fx.ids["working"]); !strings.Contains(b, "The diff is shown while a task is in review.") || strings.Contains(b, "slai-diff-file") {
		t.Error("non-review task shows a diff")
	}
	fx.f.diffErr = fmt.Errorf("fatal: bad revision")
	if b := page(t, fx, fx.ids["review"]); !strings.Contains(b, "The diff could not be read: fatal: bad revision") {
		t.Error("diff error not shown")
	}
}

// P5 AC1: the spec renders read-only from .slai/specs/; anything else is refused and never read.
func TestSpecIsReadOnlyAndConfinedToSlaiSpecs(t *testing.T) {
	dir := t.TempDir()
	specs := filepath.Join(dir, ".slai", "specs")
	os.MkdirAll(specs, 0o755)
	os.WriteFile(filepath.Join(specs, "ok.md"), []byte("# Title\n\nSome **bold** and `code` <script>alert(1)</script>.\n\n- one\n- two\n\n```\nraw <b>\n```\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOP-SECRET"), 0o644)
	os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(specs, "link.md"))
	os.WriteFile(filepath.Join(specs, "big.md"), make([]byte, 300<<10), 0o644)

	v := board.ReadSpec(dir, ".slai/specs/ok.md")
	if v.Message != "" || len(v.Blocks) != 4 || v.Blocks[0].Kind != "h1" || v.Blocks[2].Kind != "ul" || v.Blocks[3].Kind != "code" {
		t.Fatalf("ok.md: %+v", v)
	}
	for _, c := range []struct{ path, wantMsg string }{
		{"secret.txt", "outside .slai/specs/"},
		{"../etc/passwd", "outside .slai/specs/"},
		{"/etc/passwd", "outside .slai/specs/"},
		{".slai/specs/link.md", "outside .slai/specs/"},
		{".slai/specs/missing.md", "not found"},
		{".slai/specs/big.md", "too large"},
		{".slai/specs", "outside .slai/specs/"},
		{"", "no spec"},
	} {
		v := board.ReadSpec(dir, c.path)
		if !strings.Contains(v.Message, c.wantMsg) || len(v.Blocks) != 0 {
			t.Errorf("%q: %+v, want message containing %q", c.path, v, c.wantMsg)
		}
		for _, b := range v.Blocks {
			if strings.Contains(b.Code, "TOP-SECRET") {
				t.Errorf("%q read outside the specs directory", c.path)
			}
		}
	}

	// end to end: the page escapes the rendered spec
	fx := newFix(t)
	tk, _ := fx.h.Store.CreateTask(store.Task{Title: "With spec", Assignee: "eng-1", SpecPath: ".slai/specs/ok.md"})
	os.MkdirAll(filepath.Join(fx.h.Dir, ".slai", "specs"), 0o755)
	os.WriteFile(filepath.Join(fx.h.Dir, ".slai", "specs", "ok.md"), []byte("# Title\n\nText <script>alert(1)</script> **b**\n"), 0o644)
	b := page(t, fx, tk.ID)
	if !strings.Contains(b, "<h4>Title</h4>") || !strings.Contains(b, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(b, "<strong>b</strong>") {
		t.Errorf("spec not rendered:\n%s", between(b, `id="task-spec"`, "</div></div>"))
	}
	tk2, _ := fx.h.Store.CreateTask(store.Task{Title: "Escapes", Assignee: "eng-1", SpecPath: "../../etc/passwd"})
	if b := page(t, fx, tk2.ID); !strings.Contains(b, "outside .slai/specs/") {
		t.Error("path traversal not refused on the page")
	}
}

// P5 AC4: the timeline lists the task's events newest first, with cause fields.
func TestTaskTimelineNewestFirstWithCauses(t *testing.T) {
	fx := newFix(t)
	id := fx.ids["review"]
	st := fx.h.Store
	st.AddEvent("eng-1", "request_review", fmt.Sprintf(`{"task_id":%q,"summary":"first request"}`, id))
	st.AddEvent("principal", "request_changes", fmt.Sprintf(`{"task_id":%q,"comments":"order of closed tasks","cause":"ac 2 failed"}`, id))
	st.AddEvent("eng-1", "request_review", fmt.Sprintf(`{"task_id":%q,"summary":"second request <b>x</b>"}`, id))
	st.AddEvent("eng-1", "unrelated", `{"task_id":"T-9999"}`)
	b := page(t, fx, id)
	tl := between(b, `id="task-timeline"`, "</ol>")
	i1, i2, i3 := strings.Index(tl, "second request"), strings.Index(tl, "order of closed tasks"), strings.Index(tl, "first request")
	if !(i1 > 0 && i1 < i2 && i2 < i3) {
		t.Errorf("order wrong (second=%d changes=%d first=%d):\n%s", i1, i2, i3, tl)
	}
	if !strings.Contains(tl, "cause: ac 2 failed") || strings.Contains(tl, "T-9999") || strings.Contains(tl, "<b>x</b>") {
		t.Errorf("timeline:\n%s", tl)
	}
	st.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", TaskID: id, Body: "hello <i>there</i>"})
	if b := page(t, fx, id); !strings.Contains(b, "orch → eng-1") || !strings.Contains(b, "hello &lt;i&gt;there&lt;/i&gt;") {
		t.Error("messages tied to the task are missing or unescaped")
	}
}

// Focus-group AC16: a Redirect control for a live assignee (same refusals as the TUI), none without one.
func TestRedirectShownOnlyForALiveAssignee(t *testing.T) {
	fx := newFix(t)
	live := fx.ids["working"] // eng-1 is working
	b := page(t, fx, live)
	if !strings.Contains(b, `hx-post="/tasks/`+live+`/redirect"`) || !strings.Contains(b, "Interrupt and redirect") {
		t.Fatal("no redirect control for a working assignee")
	}
	// the assignee is waiting for the user: shown, but explains and offers no form
	b = page(t, fx, fx.ids["stuck"]) // eng-2 waiting_on_user
	if strings.Contains(b, "hx-post=\"/tasks/"+fx.ids["stuck"]+"/redirect\"") || !strings.Contains(b, "Can’t redirect now: eng-2 is waiting for your answer") {
		t.Errorf("waiting_on_user: %s", between(b, `id="task-redirect"`, "</div></div>"))
	}
	// no live assignee: the dead one has no control at all
	fx.h.Store.SetAgentStatus("eng-1", "dead")
	if b := page(t, fx, live); strings.Contains(b, "task-redirect") {
		t.Error("redirect shown for a dead assignee")
	}
	// a finished task has nothing to redirect
	fx.h.Store.SetAgentStatus("eng-1", "working")
	if b := page(t, fx, fx.ids["merged"]); strings.Contains(b, "task-redirect") {
		t.Error("redirect shown for a merged task")
	}
}

func TestRedirectPostUsesTheRedirectPathAndRefusesLikeTheTUI(t *testing.T) {
	fx := newFix(t)
	live := fx.ids["working"]
	r := fx.post("/tasks/"+live+"/redirect", url.Values{"text": {"  use the other API  "}})
	if r.Code != 200 || !strings.Contains(r.Body, "Course correction queued for eng-1") {
		t.Fatalf("redirect: %d %s", r.Code, r.Body)
	}
	if len(fx.f.redirected) != 1 || fx.f.redirected[0] != "eng-1: use the other API" {
		t.Errorf("redirected = %v", fx.f.redirected)
	}
	if r := fx.post("/tasks/"+live+"/redirect", url.Values{"text": {" "}}); r.Code != 422 {
		t.Errorf("empty text: %d", r.Code)
	}
	if r := fx.post("/tasks/"+live+"/redirect", url.Values{"text": {strings.Repeat("x", 4001)}}); r.Code != 422 || !strings.Contains(html.UnescapeString(r.Body), "too long") {
		t.Errorf("long text: %d %s", r.Code, r.Body)
	}
	// agent paused or waiting: refused, nothing queued
	fx.h.Store.SetAgentStatus("eng-1", "waiting_on_user")
	if r := fx.post("/tasks/"+live+"/redirect", url.Values{"text": {"x"}}); r.Code != 422 || !strings.Contains(r.Body, "waiting for your answer") {
		t.Errorf("waiting: %d %s", r.Code, r.Body)
	}
	fx.h.Store.SetAgentStatus("eng-1", "idle")
	if r := fx.post("/tasks/"+live+"/redirect", url.Values{"text": {"x"}}); r.Code != 422 || !strings.Contains(r.Body, "not busy") {
		t.Errorf("idle: %d %s", r.Code, r.Body)
	}
	if len(fx.f.redirected) != 1 {
		t.Errorf("refused redirects were queued: %v", fx.f.redirected)
	}
}

// The task page's cancel/unblock go through the same paths as the board's.
func TestTaskPageCancelAndUnblock(t *testing.T) {
	fx := newFix(t)
	a := fx.ids["assigned"]
	if b := page(t, fx, a); !strings.Contains(b, `hx-post="/tasks/`+a+`/cancel"`) {
		t.Fatal("no cancel on the task page")
	}
	r := fx.post("/tasks/"+a+"/cancel", url.Values{"ret": {"task"}, "reason": {"changed my mind"}})
	if r.Code != 200 || strings.Contains(r.Body, "board-cols") {
		t.Fatalf("cancel from the task page: %d %s", r.Code, r.Body)
	}
	if tk, _ := fx.h.Store.GetTask(a); tk.Status != store.StatusCancelled {
		t.Errorf("status = %s", tk.Status)
	}
	if r := fx.post("/tasks/"+fx.ids["working"]+"/unblock", nil); r.Code != 422 || !strings.Contains(r.Body, "not blocked") {
		t.Errorf("unblock a working task: %d %s", r.Code, r.Body)
	}
	if r := fx.post("/tasks/"+fx.ids["waiting"]+"/unblock", nil); r.Code != 422 || !strings.Contains(html.UnescapeString(r.Body), "waiting on") {
		t.Errorf("unblock a waiting task: %d %s", r.Code, r.Body)
	}
	if r := fx.post("/tasks/T-9999/cancel", nil); r.Code != 422 || !strings.Contains(r.Body, "not found") {
		t.Errorf("unknown task: %d", r.Code)
	}
}
