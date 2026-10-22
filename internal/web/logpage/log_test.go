package logpage_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/logpage"
	"github.com/oorbt/slai/internal/web/webtest"
)

func newH(t *testing.T, setup func(*store.Store), opts ...webtest.Option) *webtest.Harness {
	s := webtest.Setup(func(st *store.Store, _ *config.Team) {
		if setup != nil {
			setup(st)
		}
	})
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(logpage.Module), s}, opts...)...)
	h.Login()
	return h
}

func rowsOf(body string) int { return strings.Count(body, `<td class="mono text-nowrap">`) }

func seed(st *store.Store) {
	st.AddEvent("eng-1", "request_review", `{"task_id":"T-0080","summary":"memory M2"}`)
	st.AddEvent("principal", "request_changes", `{"task_id":"T-0080","comments":"fix <b>AC 2</b>"}`)
	st.AddEvent("eng-2", "request_review", `{"task_id":"T-0087","summary":"prototype"}`)
	st.AddEvent("", "merge_refused", `{"task_id":"T-0074","cause":"the tests failed on the merge result"}`)
	st.AddEvent("eng-1", "hook:posttooluse", `{"tool":"Write"}`)
	st.AddEvent("eng-1", "usage:mark", `{}`)
	st.AddEvent("orch", "send_message", `{"to":"eng-3","body":"pick up T-0081"}`)
}

// P9 AC3: filters by agent, kind and task; hook events hidden by default, newest first.
func TestFiltersByAgentKindTaskAndText(t *testing.T) {
	h := newH(t, seed)
	all := h.Get("/log")
	h.CheckPage(all)
	if rowsOf(all.Body) != 5 { // the hook and usage events are hidden
		t.Fatalf("default log has %d rows, want 5:\n%s", rowsOf(all.Body), all.Body)
	}
	if strings.Contains(all.Body, "hook:posttooluse") && !strings.Contains(all.Body, "<option>hook:posttooluse</option>") {
		t.Error("hook events shown by default")
	}
	// newest first
	if strings.Index(all.Body, "pick up T-0081") > strings.Index(all.Body, "memory M2") {
		t.Error("not newest first")
	}
	for query, want := range map[string]int{
		"agent=eng-1":         1, // the hook is hidden
		"agent=supervisor":    1,
		"kind=request_review": 2,
		"task=T-0080":         2,
		"q=prototype":         1,
		"agent=eng-1&kind=request_review&task=T-0080": 1,
		"task=T-9999":         0,
		"hooks=1":             7,
		"hooks=1&agent=eng-1": 3,
	} {
		if got := rowsOf(h.Get("/log?" + query).Body); got != want {
			t.Errorf("?%s = %d rows, want %d", query, got, want)
		}
	}
	// the filter menus offer what is in the log, supervisor included, and keep the choice
	b := h.Get("/log?agent=eng-1&kind=request_review").Body
	for _, want := range []string{"<option>supervisor</option>", `<option selected>eng-1</option>`, `<option selected>request_review</option>`, "<option>hook:posttooluse</option>"} {
		if !strings.Contains(b, want) {
			t.Errorf("menus lack %q", want)
		}
	}
	if !strings.Contains(h.Get("/log?task=T-9999").Body, "No event matches these filters.") {
		t.Error("empty state")
	}
	// text search is literal: % and _ are not wildcards
	h.Store.AddEvent("eng-1", "note", `{"body":"100% done_here"}`)
	if rowsOf(h.Get("/log?q=100%25+done_here").Body) != 1 || rowsOf(h.Get("/log?q=%25").Body) != 1 {
		t.Error("LIKE wildcards leaked into the search")
	}
}

// P9 AC3: 100 per page, with prev/next and page links that keep the filters.
func TestPaginationHundredPerPage(t *testing.T) {
	h := newH(t, func(st *store.Store) {
		for i := 0; i < 230; i++ {
			st.AddEvent("eng-1", "report", fmt.Sprintf(`{"task_id":"T-0001","summary":"event %03d"}`, i))
		}
		st.AddEvent("eng-2", "report", `{"summary":"other agent"}`)
	})
	p1 := h.Get("/log?agent=eng-1")
	if rowsOf(p1.Body) != 100 || !strings.Contains(p1.Body, "Showing 1–100 of 230 · newest first · 100 per page") || !strings.Contains(p1.Body, "event 229") || strings.Contains(p1.Body, "event 129") {
		t.Fatalf("page 1: %d rows", rowsOf(p1.Body))
	}
	if !strings.Contains(p1.Body, `href="/log?agent=eng-1&amp;page=2">next`) || !strings.Contains(p1.Body, `<span class="page-link">prev</span>`) {
		t.Errorf("page 1 links:\n%s", regexp.MustCompile(`(?s)<ul class="pagination.*?</ul>`).FindString(p1.Body))
	}
	p3 := h.Get("/log?agent=eng-1&page=3")
	if rowsOf(p3.Body) != 30 || !strings.Contains(p3.Body, "Showing 201–230 of 230") || !strings.Contains(p3.Body, "event 000") || !strings.Contains(p3.Body, `page=2">prev`) {
		t.Fatalf("page 3: %d rows", rowsOf(p3.Body))
	}
	// a page number past the end shows the last page; garbage is page 1
	if rowsOf(h.Get("/log?agent=eng-1&page=99").Body) != 30 || rowsOf(h.Get("/log?agent=eng-1&page=banana").Body) != 100 || rowsOf(h.Get("/log?agent=eng-1&page=-4").Body) != 100 {
		t.Error("out-of-range pages")
	}
}

// Event data is escaped; the task links to its page; a task-less event has no link.
func TestRowsAreEscapedAndLinked(t *testing.T) {
	h := newH(t, seed)
	b := h.Get("/log").Body
	if strings.Contains(b, "<b>AC 2</b>") || !strings.Contains(b, "fix &lt;b&gt;AC 2&lt;/b&gt;") {
		t.Error("comments not escaped")
	}
	if !strings.Contains(b, `<a href="/tasks/T-0080">T-0080</a>`) || !strings.Contains(b, "→ eng-3 pick up T-0081") {
		t.Errorf("task link or recipient missing:\n%s", b)
	}
	if !strings.Contains(b, `chip chip-warn">merge_refused`) {
		t.Error("merge_refused should be a warning chip")
	}
}

func TestLogFragmentIsTheLiveRegion(t *testing.T) {
	h := newH(t, seed)
	page := h.Get("/log?agent=eng-1").Body
	if !strings.Contains(page, `data-live="events" data-live-src="/f/log?agent=eng-1"`) || strings.Contains(page, "hx-trigger") {
		t.Fatal("the page is not a live region with its filters, or still polls")
	}
	f := h.Get("/f/log?agent=eng-1")
	if f.Code != 200 || strings.Contains(f.Body, "<html") || rowsOf(f.Body) != 1 || !strings.Contains(f.Body, `id="log-body" data-live="events"`) {
		t.Fatalf("fragment: %d %d rows", f.Code, rowsOf(f.Body))
	}
}

func TestLogIsReadOnly(t *testing.T) {
	h := newH(t, seed, webtest.ReadOnly())
	before := len(h.AllEvents())
	h.CheckPage(h.Get("/log"))
	if len(h.AllEvents()) != before {
		t.Fatal("reading the log wrote an event")
	}
	for _, rt := range h.Srv.Routes() {
		if strings.HasPrefix(rt.Pattern, "/log") || strings.HasPrefix(rt.Pattern, "/f/log") {
			if rt.Write() {
				t.Errorf("log exposes a write route: %s", rt.Pattern)
			}
		}
	}
}
