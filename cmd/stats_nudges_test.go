package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func nudgeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Wednesday 2026-10-21 12:00 UTC; its week starts Monday 2026-10-19.
var nudgeNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func TestManualNudgeWeeks(t *testing.T) {
	st := nudgeStore(t)
	// events are stamped by the store clock, so count what exists now against a "now" of today
	for i := 0; i < 3; i++ {
		if err := st.AddEvent("eng-1", "manual_nudge", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	weeks, err := manualNudgeWeeks(st, time.Now())
	if err != nil || len(weeks) != nudgeWeeks || weeks[0].Count != 3 || weeks[1].Count != 0 {
		t.Fatalf("weeks = %+v, %v", weeks, err)
	}
	// a clock a week later puts them in "last week"
	weeks, _ = manualNudgeWeeks(st, time.Now().AddDate(0, 0, 7))
	if weeks[0].Count != 0 || weeks[1].Count != 3 {
		t.Fatalf("a week later: %+v", weeks)
	}
	if !strings.HasPrefix(nudgeLine(weeks), "manual nudges (target 0): this week 0, last week 3, 2 weeks ago 0, 3 weeks ago 0") {
		t.Fatalf("line = %q", nudgeLine(weeks))
	}
}

func TestWeekStartsOnMonday(t *testing.T) {
	weeks, _ := manualNudgeWeeks(nudgeStore(t), nudgeNow)
	if weeks[0].WeekStart != "2026-10-19" || weeks[1].WeekStart != "2026-10-12" {
		t.Fatalf("weeks = %+v", weeks)
	}
	sun := time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC)
	if w, _ := manualNudgeWeeks(nudgeStore(t), sun); w[0].WeekStart != "2026-10-19" {
		t.Fatalf("Sunday belongs to the week of Monday the 19th, got %+v", w)
	}
}

func TestStatsReportsManualNudgesTextAndJSON(t *testing.T) {
	st := nudgeStore(t)
	_ = st.AddEvent("eng-1", "manual_nudge", `{}`)
	var text bytes.Buffer
	if err := writeStatsWithNudges(&text, service.StatsReport{}, st, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "manual nudges (target 0): this week 1,") {
		t.Fatalf("text report lacks the nudge line:\n%s", text.String())
	}
	var js bytes.Buffer
	if err := writeStatsWithNudges(&js, service.StatsReport{}, st, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(js.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["agents"]; !ok {
		t.Errorf("existing report keys must stay: %v", got)
	}
	nw, _ := got["manual_nudges"].([]any)
	if len(nw) != nudgeWeeks || nw[0].(map[string]any)["count"].(float64) != 1 {
		t.Errorf("manual_nudges = %v", got["manual_nudges"])
	}
}
