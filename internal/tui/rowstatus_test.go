package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

var rowNow = time.Unix(2_000_000, 0)

func rowFixture() ([]store.Agent, rowExtras) {
	ago := func(d time.Duration) int64 { return rowNow.Add(-d).Unix() }
	agents := []store.Agent{
		{ID: "orch", Role: "orchestrator", Model: "opus", Status: "idle", StatusAt: ago(3 * time.Hour)},
		{ID: "eng-1", Role: "engineer", Model: "sonnet", Status: "working", StatusAt: ago(4 * time.Minute), CurrentTask: "T-0007"},
		{ID: "eng-2", Role: "engineer", Model: "sonnet", Status: "working", StatusAt: ago(12 * time.Minute), CurrentTask: "T-0009"},
		{ID: "eng-3", Role: "engineer", Model: "sonnet", Status: "idle", StatusAt: ago(30 * time.Second)},
		{ID: "pm", Role: "product_manager", Model: "sonnet", Status: "idle", StatusAt: ago(50 * time.Hour)},
	}
	h := map[string]service.Health{
		"eng-2": {Liveness: "stale", Reason: "owes work but silent for 12m: no hook events"},
		"pm":    {Liveness: "gated", Reason: "held until 15:04"},
		"eng-3": {Liveness: "dead", Reason: "tmux session gone"},
	}
	return agents, rowExtras{health: h, now: rowNow}
}

func rowLines(w int) []string {
	agents, x := rowFixture()
	items := buildItems(agents, map[string]bool{})
	var out []string
	for _, l := range renderListH(items, -1, w, agents, nil, nil, x, false) {
		out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
	}
	return out
}

// AC 1: every row shows state, current task id and the age of its status. The
// digit at the start of the row is the key that jumps to it.
func TestAgentRowShowsStateTaskAndAge(t *testing.T) {
	for _, w := range []int{26, 30} { // list pane width at 80 and 120 columns
		got := rowLines(w)
		find := func(prefix string) (l1, l2 string) {
			for i, l := range got {
				if strings.Contains(l, prefix) && i+1 < len(got) {
					return l, got[i+1]
				}
			}
			t.Fatalf("w=%d: no row for %s in\n%s", w, prefix, strings.Join(got, "\n"))
			return
		}
		l1, l2 := find(" eng-1")
		if !strings.Contains(l1, "working") || !strings.HasPrefix(strings.TrimSpace(l1), "3") || !strings.HasPrefix(strings.TrimSpace(l2), "T-0007 · 4m") {
			t.Errorf("w=%d eng-1: %q / %q", w, l1, l2)
		}
		if _, l2 := find(" orch"); !strings.Contains(l2, "idle · 3h") {
			t.Errorf("w=%d orch: %q", w, l2)
		}
		if _, l2 := find(" pm"); !strings.Contains(l2, "gated: held until") && !strings.Contains(l2, "⚠ gated") {
			t.Errorf("w=%d pm: %q", w, l2)
		}
		for _, l := range got {
			if ansi.StringWidth(l) > w {
				t.Errorf("w=%d: line wider than the row: %q", w, l)
			}
		}
	}
}

// AC 2: a blocked, gated, stale or dead agent shows its reason, cut to the row.
func TestAgentRowShowsReasonTruncatedWithoutWrapping(t *testing.T) {
	wide := rowLines(100)
	var stale, dead string
	for i, l := range wide {
		switch {
		case strings.Contains(l, " eng-2"):
			stale = wide[i+1]
		case strings.Contains(l, " eng-3"):
			dead = wide[i+1]
		}
	}
	if !strings.Contains(stale, "⚠ stale: owes work but silent for 12m: no hook events") || !strings.Contains(stale, "T-0009 · 12m") {
		t.Errorf("stale row: %q", stale)
	}
	if !strings.Contains(dead, "tmux session gone") || !strings.Contains(dead, "30s") {
		t.Errorf("dead row: %q", dead)
	}
	for _, w := range []int{26, 30} {
		got := rowLines(w)
		for i, l := range got {
			if strings.Contains(l, " eng-2") {
				l2 := got[i+1]
				if !strings.HasPrefix(strings.TrimSpace(l2), "⚠ stale: owes work") || !strings.HasSuffix(l2, "…") || ansi.StringWidth(l2) > w {
					t.Errorf("w=%d truncated reason: %q", w, l2)
				}
			}
		}
		// two lines per agent, never more: nothing wraps
		agents, _ := rowFixture()
		if want := len(agents)*2 + 3; len(got) != want { // + 3 group headers (orchestrator, product, engineers)
			t.Errorf("w=%d: %d lines, want %d (a row wrapped?)\n%s", w, len(got), want, strings.Join(got, "\n"))
		}
	}
}

// The same through the whole screen at 80 and 120 columns.
func TestAgentRowsFitAtEightyAndOneTwentyColumns(t *testing.T) {
	for _, cols := range []int{80, 120} {
		m, st := newModel(t)
		_ = st.SetAgentStatus("eng-2", "working")
		m.Update(tea_size(cols, 32))
		m.Update(m.refreshCmd()())
		for _, l := range strings.Split(screen(m), "\n") {
			if ansi.StringWidth(l) > cols {
				t.Errorf("cols=%d: line wider than the screen: %q", cols, l)
			}
		}
	}
}

// AC 3: a digit selects the nth visible agent; its digit again attaches.
func TestDigitKeysSelectThenAttach(t *testing.T) {
	m, _ := newModel(t)
	var order []string
	for _, it := range m.items {
		if !it.isHeader() {
			order = append(order, it.agent)
		}
	}
	if len(order) < 3 {
		t.Fatalf("need 3 agents, have %v", order)
	}
	for n := 1; n <= 3; n++ {
		m.Update(key(fmt.Sprint(n)))
		if m.sel != order[n-1] {
			t.Errorf("digit %d selected %q, want %q", n, m.sel, order[n-1])
		}
		if a, ok := m.curAgent(); !ok || a.ID != order[n-1] {
			t.Errorf("digit %d: cursor on %v", n, a.ID)
		}
	}
	// beyond the last agent: nothing changes, a hint is shown
	before := m.sel
	m.Update(key("9"))
	if m.sel != before || !strings.Contains(m.status, "no agent 9") {
		t.Errorf("digit past the end: sel=%q status=%q", m.sel, m.status)
	}
	// the digits are on the rows
	v := screen(m)
	for n := 1; n <= 3; n++ {
		if !regexpDigitRow(n).MatchString(v) {
			t.Errorf("row for agent %d lacks its digit:\n%s", n, v)
		}
	}
}

// Attach goes through the same path as the `a` key (bindDetachCmd); with no tmux
// client it reports instead of running anything.
func TestDigitOfTheSelectedAgentAttaches(t *testing.T) {
	m, _ := newModel(t)
	m.opts.Tmux = nil
	m.Update(key("2"))
	m.Update(key("2")) // already selected: attach
	if !strings.Contains(m.status, "no tmux client") {
		t.Fatalf("second press must try to attach, status=%q", m.status)
	}
}

// AC 4: the agent detail lists the 5 most recent health events with kind, age and cause.
func TestAgentDetailListsFiveRecentHealthEvents(t *testing.T) {
	m, st := newModel(t)
	m.opts.Now = func() time.Time { return time.Unix(3_000_000, 0) }
	add := func(kind, data string) { _ = st.AddEvent("eng-1", kind, data) }
	add("agent_added", `{}`)                                                            // oldest: falls off
	add("nudge", `{"count":1,"tasks":"T-0001"}`)                                        // no cause field
	add("hook:pretooluse", `{"tool":"Bash"}`)                                           // noise: not a health event
	add("stuck_restart", `{"owed":1,"cause":"silent 3m, pane unchanged"}`)              // cause
	add("guard:lane", `{"agent":"eng-1","tool":"Write","reason":"may not write /etc"}`) // reason
	add("modal_stuck", `{"text":"permission prompt: rm -rf"}`)                          // text
	add("agent_upgraded", `{"from":"a1","to":"b2"}`)
	_ = st.AddEvent("eng-2", "nudge", `{"cause":"other agent"}`) // another agent's event
	m.cursorTo("eng-1")
	m.Update(key("I"))
	v := screen(m)
	if !strings.Contains(v, "Recent health events") {
		t.Fatalf("no events section:\n%s", v)
	}
	for _, want := range []string{"agent_upgraded", "modal_stuck", "permission prompt: rm -rf", "lane_denied", "may not write /etc", "stuck_restart", "silent 3m, pane unchanged", "nudge"} {
		if !strings.Contains(v, want) {
			t.Errorf("detail lacks %q:\n%s", want, v)
		}
	}
	for _, not := range []string{"agent_added", "other agent", "pretooluse"} {
		if strings.Contains(v, not) {
			t.Errorf("detail must not show %q:\n%s", not, v)
		}
	}
	// newest first
	if strings.Index(v, "agent_upgraded") > strings.Index(v, "stuck_restart") {
		t.Errorf("events not newest first:\n%s", v)
	}
}

func TestAgentDetailWithoutEventsSaysNone(t *testing.T) {
	m, _ := newModel(t)
	m.cursorTo("pm")
	m.Update(key("I"))
	if v := screen(m); !strings.Contains(v, "Recent health events") || !strings.Contains(v, "none") {
		t.Errorf("empty history:\n%s", v)
	}
}

func TestAgeShort(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Second: "0s", 0: "0s", 45 * time.Second: "45s", 4 * time.Minute: "4m", 59 * time.Minute: "59m",
		5 * time.Hour: "5h", 47 * time.Hour: "47h", 72 * time.Hour: "3d"} {
		if got := ageShort(d); got != want {
			t.Errorf("ageShort(%v) = %q, want %q", d, got, want)
		}
	}
}

func tea_size(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

func regexpDigitRow(n int) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?m)^│%d[●○◆▲◌✕]`, n))
}

// Backend event kinds that carry a `cause` show up in the agent detail with it.
func TestAgentDetailShowsBackendCauseEvents(t *testing.T) {
	m, st := newModel(t)
	m.opts.Now = func() time.Time { return time.Unix(3_000_000, 0) }
	_ = st.AddEvent("eng-1", "guard_missing", `{"cause":"no guard_ready within 30s"}`)
	_ = st.AddEvent("eng-1", "agent_stopped", `{"cause":"stopped by the user"}`)
	_ = st.AddEvent("eng-1", "orch_restart", `{"cause":"orchestrator silent for 10m"}`)
	_ = st.AddEvent("eng-1", "lane_denied", `{"tool":"Write","path":"/etc/x","reason":"outside your worktree"}`)
	m.cursorTo("eng-1")
	m.Update(key("I"))
	v := screen(m)
	for _, want := range []string{"guard_missing", "no guard_ready within 30s", "agent_stopped", "stopped by the user",
		"orch_restart", "orchestrator silent for 10m", "lane_denied", "outside your worktree"} {
		if !strings.Contains(v, want) {
			t.Errorf("detail lacks %q:\n%s", want, v)
		}
	}
}

// AC 3 (backlog #6): the task detail shows the engineer's per-AC claims next to
// the reviewer's verdicts.
func TestTaskDetailShowsACClaimsAndVerdicts(t *testing.T) {
	task := store.Task{ID: "T-0042", Title: "x", Status: "in_review",
		ACClaims:   []store.ACClaim{{AC: "AC 1", Met: true, Note: "TestOne"}, {AC: "AC 2", Met: true, Note: "TestTwo"}, {AC: "AC 3", Met: false, Note: "needs backend"}},
		ACVerdicts: []store.ACVerdict{{AC: "ac 1", Pass: true}, {AC: "AC 2", Pass: false, Note: "edge case untested"}, {AC: "AC 9", Pass: true, Note: "extra"}}}
	out := ansi.Strip(renderDetail(task, "", nil, ""))
	for _, want := range []string{"Acceptance criteria", "AC 1", "✓ met", "pass", "AC 2", "FAIL", "edge case untested", "TestTwo",
		"AC 3", "✗ not met", "– no verdict", "needs backend", "AC 9", "(no claim)"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
	if none := ansi.Strip(renderDetail(store.Task{ID: "T-1", Title: "x"}, "", nil, "")); strings.Contains(none, "Acceptance criteria") {
		t.Errorf("a task without claims must not show the section:\n%s", none)
	}
}
