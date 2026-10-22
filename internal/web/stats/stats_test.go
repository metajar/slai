package stats_test

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/stats"
	"github.com/oorbt/slai/internal/web/webtest"
)

// seed: eng-1 (claude) merged two tasks with usage; eng-2 (opencode) merged one with no
// usage at all; eng-3 has no tasks.
func seed(t *testing.T) webtest.Option {
	return webtest.Setup(func(st *store.Store, tm *config.Team) {
		tm.Agents = append(tm.Agents,
			config.Agent{ID: "eng-2", Role: config.RoleEngineer, Harness: config.HarnessOpenCode, Model: "zai/glm"},
			config.Agent{ID: "eng-3", Role: config.RoleEngineer, Model: "sonnet"})
		for _, id := range []string{"eng-1", "eng-2"} {
			st.UpsertAgent(store.Agent{ID: id, Role: "engineer"})
		}
		mk := func(title, who, status string) string {
			tk, err := st.CreateTask(store.Task{Title: title, Assignee: who, Status: status})
			if err != nil {
				t.Fatal(err)
			}
			return tk.ID
		}
		a := mk("first", "eng-1", store.StatusMerged)
		b := mk("second", "eng-1", store.StatusMerged)
		c := mk("third", "eng-2", store.StatusMerged)
		mk("open one", "eng-1", store.StatusInProgress)
		ev := func(who, kind, id string) { st.AddEvent(who, kind, fmt.Sprintf(`{"task_id":%q}`, id)) }
		ev("eng-1", "request_review", a)
		ev("eng-1", "request_review", a)
		ev("principal", "request_changes", a)
		ev("eng-1", "request_review", b)
		ev("eng-2", "request_review", c)
		ev("eng-1", "merge_refused", b)
		st.SetAgentTask("eng-1", a)
		st.AddUsage("eng-1", 1000, 500, 1.5)
		st.SetAgentTask("eng-1", b)
		st.AddUsage("eng-1", 2000, 1000, 2.5)
	})
}

func cellsOf(body, name string) []string {
	m := regexp.MustCompile(`(?s)<tr><td class="fw-semibold">` + regexp.QuoteMeta(name) + `</td>(.*?)</tr>`).FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var out []string
	for _, c := range regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, strings.TrimSpace(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(c[1], "")))
	}
	return out
}

func newH(t *testing.T, opts ...webtest.Option) *webtest.Harness {
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(stats.Module), seed(t)}, opts...)...)
	h.Login()
	return h
}

// P8 AC1: the table's numbers are the numbers service.Stats (so `slai status --stats`)
// reports, for every agent and group.
func TestStatsPageMatchesServiceStats(t *testing.T) {
	h := newH(t)
	r := h.Get("/stats")
	h.CheckPage(r)
	rep, err := (&service.Service{Store: h.Store, Team: h.Team, Now: func() time.Time { return time.Now() }}).Stats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	e1 := cellsOf(r.Body, "eng-1")
	want := []string{"claude/sonnet", "2", "0/0", "1.5", "50%", "1", "$4.00", "$2.00"}
	for i, w := range want {
		if i >= len(e1) || e1[i] != w {
			t.Fatalf("eng-1 cells = %v, want prefix %v", e1, want)
		}
	}
	for _, a := range rep.Agents {
		if a.Agent == "eng-1" && (a.Merged != 2 || fmt.Sprintf("%.1f", a.RoundsPerMerged) != "1.5" || fmt.Sprintf("%.2f", a.SpendUSD) != "4.00") {
			t.Fatalf("service numbers drifted from the fixture: %+v", a)
		}
	}
	// groups: claude/sonnet holds eng-1 and eng-3
	g := cellsOf(r.Body, "claude/sonnet")
	if len(g) < 3 || g[0] != "eng-1, eng-3" || g[1] != "2" {
		t.Fatalf("group row = %v", g)
	}
	if o := cellsOf(r.Body, "opencode/zai/glm"); len(o) < 2 || o[1] != "1" {
		t.Fatalf("opencode group = %v", o)
	}
}

// P8 AC2: data the page cannot know is n/a with the reason, never 0.
func TestMissingDataIsNAWithReasonNeverZero(t *testing.T) {
	h := newH(t)
	body := h.Get("/stats").Body
	e2 := cellsOf(body, "eng-2") // opencode: merged 1 but no usage rows
	if len(e2) < 9 || e2[6] != "n/a" || e2[7] != "n/a" {
		t.Fatalf("eng-2 spend cells = %v, want n/a (no usage recorded)", e2)
	}
	if !strings.Contains(body, "no usage was recorded for this agent in this window; opencode may not report per-turn cost") {
		t.Error("the reason for the n/a spend is missing")
	}
	e3 := cellsOf(body, "eng-3")             // no tasks at all
	for _, i := range []int{3, 4, 6, 7, 8} { // rounds, rework, spend, $/merged, median
		if len(e3) < 9 || e3[i] != "n/a" {
			t.Fatalf("eng-3 cell %d = %v, want n/a", i, e3)
		}
	}
	if !strings.Contains(body, "no task has been merged in this window yet") {
		t.Error("reason for rounds/rework/median is missing")
	}
	if strings.Contains(strings.Join(e3, "|"), "$0.00") || strings.Contains(strings.Join(e2, "|"), "$0.00") {
		t.Errorf("an unknown value rendered as $0.00: %v %v", e2, e3)
	}
	// per-task: a task with no usage is n/a with the harness reason; one with usage shows numbers
	if !regexp.MustCompile(`T-0003</a></td><td>eng-2</td><td><span class="na"[^>]*title="eng-2 runs on opencode: it may not report per-turn cost">n/a`).MatchString(body) {
		t.Errorf("per-task n/a for the opencode task missing:\n%s", body)
	}
	if !regexp.MustCompile(`T-0001</a></td><td>eng-1</td><td>\$1\.50</td><td>1k</td>`).MatchString(body) {
		t.Errorf("per-task numbers for T-0001 missing")
	}
}

// Focus-group AC 24: column headers carry their definitions as tooltips and the footnote
// lists them.
func TestHeadersCarryDefinitions(t *testing.T) {
	body := newH(t).Get("/stats").Body
	for _, d := range stats.Definitions {
		if !strings.Contains(body, `title="`+html.EscapeString(d.Meaning)+`"`) {
			t.Errorf("header %q lacks its tooltip", d.Col)
		}
		if !strings.Contains(body, "<em>"+d.Col+"</em>") {
			t.Errorf("footnote lacks %q", d.Col)
		}
	}
}

// --since filters by assignment time like the CLI; a bad value is explained and ignored.
func TestSinceFilterAndBadValue(t *testing.T) {
	h := newH(t)
	h.Advance(3 * 24 * time.Hour)
	h.Store.CreateTask(store.Task{Title: "recent", Assignee: "eng-1", Status: store.StatusMerged})
	all := cellsOf(h.Get("/stats").Body, "eng-1")
	recent := cellsOf(h.Get("/stats?since=1d").Body, "eng-1")
	if all[1] != "3" || recent[1] != "1" {
		t.Fatalf("merged all-time %v, last day %v", all[1], recent[1])
	}
	if b := h.Get("/stats?since=1d").Body; !strings.Contains(b, "tasks assigned since") || !strings.Contains(b, `value="1d"`) {
		t.Error("window text or input missing")
	}
	if b := h.Get("/stats?since=banana").Body; !strings.Contains(b, "want a span like 7d") || !strings.Contains(b, "all time") {
		t.Errorf("bad since:\n%s", b)
	}
}

// Spend vs caps: OFF is explicit when no cap is set; 80% and 100% are textual and distinct.
func TestSpendVsCaps(t *testing.T) {
	h := newH(t)
	if b := h.Get("/stats").Body; !strings.Contains(b, "Caps: OFF") {
		t.Fatalf("no caps must say OFF:\n%s", b)
	}
	h.Team.Defaults.Caps = config.SpendCaps{DailyUSD: 5, PerAgentUSD: 4}
	b := h.Get("/stats").Body
	for _, want := range []string{"Team, today", "$4.00 / $5.00 · over 80%", "eng-1, today", "$4.00 / $4.00 · cap reached", `class="slai-progress warn"`, `class="slai-progress bad"`} {
		if !strings.Contains(b, want) {
			t.Errorf("caps lack %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "Caps: OFF") {
		t.Error("OFF shown with caps set")
	}
}

// The fragment is the live region itself (an outerHTML swap keeps it live); the page has no
// polling trigger (see internal/web/pages/live_test.go for the topics and the hub).
func TestFragmentIsTheLiveRegion(t *testing.T) {
	h := newH(t)
	page := h.Get("/stats").Body
	if !strings.Contains(page, `data-live="stats spend" data-live-src="/f/stats?since="`) || strings.Contains(page, "hx-trigger") {
		t.Fatal("the page is not a live region, or still polls")
	}
	f := h.Get("/f/stats")
	if f.Code != 200 || strings.Contains(f.Body, "<html") || !strings.Contains(f.Body, "Quality by agent") || !strings.Contains(f.Body, `id="stats-body" data-live="stats spend"`) {
		t.Fatalf("fragment: %d %.200s", f.Code, f.Body)
	}
}

// The page is read-only: its only routes are GETs, and it changes nothing.
func TestStatsIsReadOnly(t *testing.T) {
	h := newH(t)
	before := len(h.AllEvents())
	h.Get("/stats")
	h.Get("/f/stats?since=7d")
	if len(h.AllEvents()) != before {
		t.Fatal("reading the stats page wrote an event")
	}
	for _, rt := range h.Srv.Routes() {
		if strings.HasPrefix(rt.Pattern, "/stats") || strings.HasPrefix(rt.Pattern, "/f/stats") {
			if rt.Write() {
				t.Errorf("stats exposes a write route: %s %s", rt.Method, rt.Pattern)
			}
		}
	}
}
