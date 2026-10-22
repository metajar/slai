package inbox_test

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/inbox"
	"github.com/oorbt/slai/internal/web/webtest"
)

// seed puts orch to wait on the user with one question.
func seed(t *testing.T, question string) webtest.Option {
	return webtest.Setup(func(st *store.Store, _ *config.Team) {
		if err := st.UpsertAgent(store.Agent{ID: "orch", Role: "orchestrator", Status: "waiting_on_user"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer", Status: "idle"}); err != nil {
			t.Fatal(err)
		}
		st.AddEvent("orch", "ask_user", fmt.Sprintf(`{"question":%q,"reason":"needs a decision"}`, question))
	})
}

func newH(t *testing.T, opts ...webtest.Option) *webtest.Harness {
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(inbox.Module)}, opts...)...)
	h.Login()
	return h
}

func eventID(h *webtest.Harness, kind string) string {
	evs := h.AllEvents()
	for _, e := range evs {
		if e.Kind == kind {
			return fmt.Sprint(e.ID)
		}
	}
	h.T.Fatalf("no %s event", kind)
	return ""
}

// P7 AC1: an open ask_user appears with its asking agent; answering calls SendUser and the
// item is marked answered.
func TestAnswerSendsToTheAskingAgentAndMarksItAnswered(t *testing.T) {
	h := newH(t, seed(t, "Which port should the demo use?"))
	page := h.Get("/inbox")
	h.CheckPage(page)
	for _, want := range []string{"Which port should the demo use?", "<strong>orch</strong>", "needs a decision", `name="answer"`, "Questions for you"} {
		if !strings.Contains(page.Body, want) {
			t.Fatalf("inbox lacks %q:\n%s", want, page.Body)
		}
	}
	id := eventID(h, "ask_user")
	r := h.Post("/inbox/"+id+"/answer", url.Values{"answer": {"  Use 8080.  "}})
	if r.Code != 303 {
		t.Fatalf("answer = %d %s", r.Code, r.Body)
	}
	if got := h.Sent(); len(got) != 1 || got[0].To != "orch" || got[0].Body != "Use 8080." {
		t.Fatalf("SendUser calls: %v", got)
	}
	page = h.Get("/inbox")
	if strings.Contains(page.Body, `name="answer"`) || !strings.Contains(page.Body, "Answered today") || !strings.Contains(page.Body, "You answered") || !strings.Contains(page.Body, "Use 8080.") {
		t.Fatalf("answered item not marked:\n%s", page.Body)
	}
	// answering again is refused and sends nothing
	r = h.Post("/inbox/"+id+"/answer", url.Values{"answer": {"again"}})
	if r.Code != 422 || !strings.Contains(r.Body, "not waiting for an answer") || len(h.Sent()) != 1 {
		t.Fatalf("second answer = %d %s sent=%v", r.Code, r.Body, h.Sent())
	}
	// the htmx flavour reloads the page
	h2 := newH(t, seed(t, "q"))
	req := h2.Browser("POST", "/inbox/"+eventID(h2, "ask_user")+"/answer", url.Values{"answer": {"ok"}})
	req.Header.Set("HX-Request", "true")
	if r := h2.Do(req); r.Code != 204 || r.Header.Get("HX-Redirect") != "/inbox" {
		t.Fatalf("htmx answer = %d %v", r.Code, r.Header)
	}
}

// Failed answers show the service's text verbatim; nothing is sent.
func TestAnswerErrorsAreShownAndSendNothing(t *testing.T) {
	h := newH(t, seed(t, "q"), webtest.SupervisorRunning(func() bool { return true }))
	id := eventID(h, "ask_user")
	for name, tc := range map[string]struct {
		path, answer, want string
	}{
		"empty":        {"/inbox/" + id + "/answer", "   ", "the answer is empty"},
		"too long":     {"/inbox/" + id + "/answer", strings.Repeat("x", 4001), "answer is too long (4001 &gt; 4000 chars)"},
		"unknown":      {"/inbox/99999/answer", "x", "unknown question"},
		"not a number": {"/inbox/abc/answer", "x", "unknown question"},
	} {
		r := h.Post(tc.path, url.Values{"answer": {tc.answer}})
		if r.Code != 422 || !strings.Contains(r.Body, tc.want) {
			t.Errorf("%s: %d, want 422 with %q:\n%.300s", name, r.Code, tc.want, r.Body)
		}
	}
	// an ask_user that is not an agent's question, and an agent that moved on
	h.Store.AddEvent("orch", "report", `{}`)
	if r := h.Post("/inbox/"+eventID(h, "report")+"/answer", url.Values{"answer": {"x"}}); r.Code != 422 {
		t.Errorf("non-question event = %d", r.Code)
	}
	h.Store.SetAgentStatus("orch", "working")
	if r := h.Post("/inbox/"+id+"/answer", url.Values{"answer": {"x"}}); r.Code != 422 || !strings.Contains(r.Body, "not waiting for an answer") {
		t.Errorf("moved-on agent = %d %s", r.Code, r.Body)
	}
	if len(h.Sent()) != 0 {
		t.Fatalf("nothing should have been sent: %v", h.Sent())
	}
}

// The question text is data: it is escaped, never markup.
func TestQuestionTextIsEscaped(t *testing.T) {
	h := newH(t, seed(t, `<script>alert(1)</script><img src=x onerror=alert(2)>`))
	body := h.Get("/inbox").Body
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") || !strings.Contains(body, "&lt;script&gt;alert(1)") {
		t.Fatalf("not escaped:\n%s", body)
	}
}

// Supervisor-raised ask_user events (no asking agent) are notices: shown and counted while
// fresh, with no answer box.
func TestSupervisorNoticesAreShownAndExpire(t *testing.T) {
	h := newH(t, webtest.Setup(func(st *store.Store, _ *config.Team) {
		st.AddEvent("", "ask_user", `{"reason":"spend_cap","question":"Spend cap reached for daily ($20.00 of $20.00); wake-ups paused."}`)
	}))
	body := h.Get("/inbox").Body
	if !strings.Contains(body, "supervisor notice") || !strings.Contains(body, "Spend cap reached for daily") || strings.Contains(body, `name="answer"`) {
		t.Fatalf("notice:\n%s", body)
	}
	if !regexp.MustCompile(`<title>\(1\) Inbox`).MatchString(body) {
		t.Fatal("a fresh notice should count")
	}
	h.Advance(20 * time.Minute)
	if body := h.Get("/inbox").Body; strings.Contains(body, "Spend cap reached") || strings.Contains(body, "<title>(") {
		t.Fatal("a notice older than 15 minutes should be gone")
	}
}

func raise(t *testing.T, st *store.Store, kind, agent, text string, since int64) {
	t.Helper()
	if _, err := st.RaiseEscalation(store.Escalation{Agent: agent, Kind: kind, Text: text, Since: since, User: true}); err != nil {
		t.Fatal(err)
	}
}

// P7 AC2 + focus-group AC 21: an escalation shows kind, agent, since and text; dismissing it
// hides it in the UI only: slai's own state is untouched, and it stays listed as dismissed
// with its real state.
func TestEscalationsDismissOnlyHidesInTheUI(t *testing.T) {
	since := time.Now().Add(-11 * time.Minute).Unix()
	h := newH(t, webtest.Setup(func(st *store.Store, _ *config.Team) {
		raise(t, st, "modal_stuck", "eng-1", "Permission dialog open for 11 minutes", since)
		raise(t, st, "rate_limited", "orch", "Rate limit reached", since)
		st.RaiseEscalation(store.Escalation{Agent: "eng-1", Kind: "stalled", Text: "orchestrator-only", Since: since}) // not for the user
	}))
	body := h.Get("/inbox").Body
	for _, want := range []string{"modal_stuck", "eng-1", "Permission dialog open for 11 minutes", "since", "Dismiss", "answer it once", "rate_limited"} {
		if !strings.Contains(body, want) {
			t.Errorf("escalations lack %q", want)
		}
	}
	if strings.Contains(body, "orchestrator-only") {
		t.Error("an escalation the user was not told about must not be listed")
	}
	if !regexp.MustCompile(`<title>\(2\) Inbox`).MatchString(body) {
		t.Errorf("count should be 2:\n%s", regexp.MustCompile(`<title>[^<]*`).FindString(body))
	}

	before, _ := h.Store.OpenEscalations()
	r := h.Post("/inbox/escalations/modal_stuck/eng-1/dismiss", nil)
	if r.Code != 303 {
		t.Fatalf("dismiss = %d %s", r.Code, r.Body)
	}
	after, _ := h.Store.OpenEscalations()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("dismissing changed slai's escalations:\nbefore %v\nafter  %v", before, after)
	}
	body = h.Get("/inbox").Body
	if !strings.Contains(body, "Dismissed (still active in slai)") || !strings.Contains(body, "Show again") {
		t.Fatalf("dismissed escalation should stay listed:\n%s", body)
	}
	if !regexp.MustCompile(`<title>\(1\) Inbox`).MatchString(body) {
		t.Error("the count drops by one")
	}
	// the sidebar tooltip still tells the truth about the dismissed one
	if !strings.Contains(body, "1 dismissed escalation(s) are still active in slai") {
		t.Errorf("tooltip lacks the dismissed escalation:\n%s", body)
	}
	// restore
	if r := h.Post("/inbox/escalations/modal_stuck/eng-1/restore", nil); r.Code != 303 {
		t.Fatalf("restore = %d", r.Code)
	}
	if strings.Contains(h.Get("/inbox").Body, "Dismissed (still active in slai)") {
		t.Error("restored escalation still listed as dismissed")
	}
	// an unknown or user-hidden escalation is refused
	if r := h.Post("/inbox/escalations/modal_stuck/nobody/dismiss", nil); r.Code != 422 {
		t.Errorf("unknown escalation = %d", r.Code)
	}
	if r := h.Post("/inbox/escalations/stalled/eng-1/dismiss", nil); r.Code != 422 {
		t.Errorf("orchestrator-only escalation = %d", r.Code)
	}
	// dismissing, then the problem clearing and coming back (a new Since): shown again
	h.Post("/inbox/escalations/modal_stuck/eng-1/dismiss", nil)
	h.Store.ClearEscalation("eng-1", "modal_stuck")
	raise(t, h.Store, "modal_stuck", "eng-1", "again", since+600)
	if body := h.Get("/inbox").Body; strings.Contains(body, "Dismissed (still active") || !strings.Contains(body, "again") {
		t.Errorf("a re-raised escalation must come back:\n%s", body)
	}
}

// P7 AC3: the tab title and the sidebar badge show the count of items needing the user, on
// every page, not only the inbox.
func TestTabTitleAndSidebarBadgeShowTheCount(t *testing.T) {
	h := newH(t, seed(t, "q1"), webtest.Setup(func(st *store.Store, _ *config.Team) {
		st.SetAgentStatus("eng-1", "waiting_on_user")
		st.AddEvent("eng-1", "ask_user", `{"question":"q2"}`)
	}))
	for _, path := range []string{"/", "/inbox"} {
		body := h.Get(path).Body
		if !regexp.MustCompile(`<title>\(2\) `).MatchString(body) {
			t.Errorf("%s: tab title lacks the count: %s", path, regexp.MustCompile(`<title>[^<]*`).FindString(body))
		}
		if !regexp.MustCompile(`class="badge bg-red text-red-fg ms-auto" title="2 need you[^"]*">2`).MatchString(body) {
			t.Errorf("%s: sidebar badge missing", path)
		}
	}
	// nothing waiting: no badge, no count in the title
	quiet := newH(t)
	body := quiet.Get("/inbox").Body
	if strings.Contains(body, "<title>(") || strings.Contains(body, "badge bg-red") || !strings.Contains(body, "Nothing is waiting for you") {
		t.Errorf("empty inbox:\n%s", body)
	}
}

// Asks waiting on a teammate's reply are listed until someone replies.
func TestWaitingOnTheTeam(t *testing.T) {
	h := newH(t)
	id, _ := h.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "ask", Body: "React to the prototype?"})
	if body := h.Get("/inbox").Body; !strings.Contains(body, "orch → eng-1") || !strings.Contains(body, "React to the prototype?") {
		t.Fatalf("open ask missing:\n%s", body)
	}
	h.Store.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "note", ReplyTo: id, Body: "yes"})
	if body := h.Get("/inbox").Body; strings.Contains(body, "React to the prototype?") {
		t.Fatal("a replied ask should leave the list")
	}
}

// Read-only: no answer or dismiss controls, and the writes are refused.
func TestReadOnlyInbox(t *testing.T) {
	h := newH(t, seed(t, "q"), webtest.ReadOnly(), webtest.Setup(func(st *store.Store, _ *config.Team) {
		raise(t, st, "modal_stuck", "eng-1", "stuck", time.Now().Unix())
	}))
	body := h.Get("/inbox").Body
	if strings.Contains(body, "hx-post") || !strings.Contains(body, "Read-only: answer with") {
		t.Fatalf("read-only inbox shows write controls:\n%s", body)
	}
	if r := h.Post("/inbox/"+eventID(h, "ask_user")+"/answer", url.Values{"answer": {"x"}}); r.Code != 403 {
		t.Errorf("answer = %d", r.Code)
	}
	if r := h.Post("/inbox/escalations/modal_stuck/eng-1/dismiss", nil); r.Code != 403 {
		t.Errorf("dismiss = %d", r.Code)
	}
}

// Every inbox write records exactly one web_action.
func TestInboxWritesRecordWebActions(t *testing.T) {
	h := newH(t, seed(t, "q"), webtest.Setup(func(st *store.Store, _ *config.Team) {
		raise(t, st, "modal_stuck", "eng-1", "stuck", time.Now().Unix())
	}))
	h.Post("/inbox/"+eventID(h, "ask_user")+"/answer", url.Values{"answer": {"ok"}})
	h.Post("/inbox/escalations/modal_stuck/eng-1/dismiss", nil)
	acts := h.WebActions()
	if len(acts) != 2 || acts[0]["action"] != "answer" || acts[1]["action"] != "dismiss_escalation" || acts[1]["target"] != "modal_stuck/eng-1" {
		t.Fatalf("web_action events: %v", acts)
	}
}
