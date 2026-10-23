package service

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

const statsT0 = 1_000_000

// seedStats builds a team of three engineers on two harness+model groups and a history:
//
//	eng-1 claude/sonnet: T1 merged (2 reviews, 1 changes, 1h), T2 merged (1 review, 2h, one
//	      merge_refused), T3 cancelled, T4 reverted
//	eng-2 opencode/zai/glm: T5 merged (3 reviews, 2 changes, 10m), T6 still in progress
//	eng-3 claude/sonnet: no tasks
func seedStats(t *testing.T) *Service {
	t.Helper()
	s := newSvc(t, false)
	s.Team.Agents = append(s.Team.Agents,
		config.Agent{ID: "eng-2", Role: config.RoleEngineer, Harness: config.HarnessOpenCode, Model: "zai/glm"},
		config.Agent{ID: "eng-3", Role: config.RoleEngineer})
	for i := range s.Team.Agents {
		if s.Team.Agents[i].ID == "eng-1" || s.Team.Agents[i].ID == "eng-3" {
			s.Team.Agents[i].Model = "sonnet"
		}
	}
	at := func(sec int64) { s.Store.SetClock(func() time.Time { return time.Unix(statsT0+sec, 0) }) }
	task := func(created int64, assignee, status string) string {
		at(created)
		tk, err := s.Store.CreateTask(store.Task{Title: "t", Assignee: assignee, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	ev := func(sec int64, agent, kind, id string) {
		at(sec)
		if err := s.Store.AddEvent(agent, kind, fmt.Sprintf(`{"task_id":%q}`, id)); err != nil {
			t.Fatal(err)
		}
	}
	use := func(sec int64, agent string, usd float64) {
		at(sec)
		if err := s.Store.AddUsage(agent, 1, 1, usd); err != nil {
			t.Fatal(err)
		}
	}
	t1 := task(0, "eng-1", store.StatusMerged)
	ev(10, "eng-1", "request_review", t1)
	ev(20, "prin", "request_changes", t1)
	ev(30, "eng-1", "request_review", t1)
	ev(3600, "eng-1", "merged", t1)
	t2 := task(100, "eng-1", store.StatusMerged)
	ev(110, "eng-1", "request_review", t2)
	ev(120, "eng-1", "merge_refused", t2)
	ev(7300, "eng-1", "merged", t2)
	task(300, "eng-1", store.StatusCancelled)
	task(400, "eng-1", store.StatusReverted)
	t5 := task(200, "eng-2", store.StatusMerged)
	ev(210, "eng-2", "request_review", t5)
	ev(220, "prin", "request_changes", t5)
	ev(230, "eng-2", "request_review", t5)
	ev(240, "prin", "request_changes", t5)
	ev(250, "eng-2", "request_review", t5)
	ev(800, "eng-2", "merged", t5)
	task(500, "eng-2", store.StatusInProgress)
	use(50, "eng-1", 1.0)
	use(600, "eng-1", 2.0)
	use(600, "eng-2", 0.5)
	return s
}

func statsRow(t *testing.T, rows []StatsRow, who string) StatsRow {
	t.Helper()
	for _, r := range rows {
		if r.Agent == who || (r.Agent == "" && r.Harness+"/"+r.Model == who) {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", who, rows)
	return StatsRow{}
}

// AC1: counts, rework rate, rounds per task and spend per merged task, per agent and
// per harness+model group.
func TestStatsPerAgentAndGroup(t *testing.T) {
	s := seedStats(t)
	rep, err := s.Stats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	e1 := statsRow(t, rep.Agents, "eng-1")
	want1 := StatsRow{Agent: "eng-1", Role: "engineer", Harness: "claude", Model: e1.Model, Tasks: 4, Merged: 2, Cancelled: 1, Reverted: 1,
		ReviewRounds: 3, Reworked: 1, MergeRefused: 1, MedianSeconds: (3600 + 7200) / 2, SpendUSD: 3,
		RoundsPerMerged: 1.5, ReworkRate: 0.5, SpendPerMerged: 1.5}
	if !reflect.DeepEqual(e1, want1) {
		t.Fatalf("eng-1\n got %+v\nwant %+v", e1, want1)
	}
	e2 := statsRow(t, rep.Agents, "eng-2")
	want2 := StatsRow{Agent: "eng-2", Role: "engineer", Harness: "opencode", Model: "zai/glm", Tasks: 2, Merged: 1,
		ReviewRounds: 3, Reworked: 1, MedianSeconds: 600, SpendUSD: 0.5,
		RoundsPerMerged: 3, ReworkRate: 1, SpendPerMerged: 0.5}
	if !reflect.DeepEqual(e2, want2) {
		t.Fatalf("eng-2\n got %+v\nwant %+v", e2, want2)
	}
	// listed with zeros
	e3 := statsRow(t, rep.Agents, "eng-3")
	if e3.Tasks != 0 || e3.Merged != 0 || e3.SpendUSD != 0 || e3.RoundsPerMerged != 0 || e3.MedianSeconds != 0 {
		t.Fatalf("eng-3 should be all zeros: %+v", e3)
	}
	if len(rep.Agents) != 3 {
		t.Fatalf("want only the three engineers, got %d rows", len(rep.Agents))
	}

	if len(rep.Groups) != 2 {
		t.Fatalf("want 2 groups, got %+v", rep.Groups)
	}
	c := statsRow(t, rep.Groups, "claude/"+e1.Model)
	if c.Merged != 2 || c.Tasks != 4 || c.RoundsPerMerged != 1.5 || c.ReworkRate != 0.5 || c.SpendPerMerged != 1.5 ||
		!reflect.DeepEqual(c.Agents, []string{"eng-1", "eng-3"}) {
		t.Fatalf("claude group: %+v", c)
	}
	o := statsRow(t, rep.Groups, "opencode/zai/glm")
	if o.Merged != 1 || o.RoundsPerMerged != 3 || o.ReworkRate != 1 || o.SpendPerMerged != 0.5 || o.MedianSeconds != 600 ||
		!reflect.DeepEqual(o.Agents, []string{"eng-2"}) {
		t.Fatalf("opencode group: %+v", o)
	}
}

// A group's ratios come from its totals: the median is over all its merged tasks, not a
// mean of the members' medians.
func TestStatsGroupMedianPoolsTasks(t *testing.T) {
	s := seedStats(t)
	s.Team.Agents[len(s.Team.Agents)-2].Harness, s.Team.Agents[len(s.Team.Agents)-2].Model = "claude", "sonnet" // eng-2 joins the group
	rep, err := s.Stats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	g := statsRow(t, rep.Groups, "claude/"+statsRow(t, rep.Agents, "eng-1").Model)
	if g.Merged != 3 || g.MedianSeconds != 3600 { // 600, 3600, 7200
		t.Fatalf("pooled group: merged %d median %d", g.Merged, g.MedianSeconds)
	}
}

// AC2: --since filters by assignment time (and spend by when it was recorded); the zero
// time is all time.
func TestStatsSince(t *testing.T) {
	s := seedStats(t)
	rep, err := s.Stats(time.Unix(statsT0+150, 0))
	if err != nil {
		t.Fatal(err)
	}
	e1 := statsRow(t, rep.Agents, "eng-1")
	if e1.Tasks != 2 || e1.Merged != 0 || e1.Cancelled != 1 || e1.Reverted != 1 || e1.SpendUSD != 2 || e1.MergeRefused != 0 {
		t.Fatalf("eng-1 since: %+v", e1)
	}
	e2 := statsRow(t, rep.Agents, "eng-2")
	if e2.Tasks != 2 || e2.Merged != 1 {
		t.Fatalf("eng-2 since: %+v", e2)
	}
	if rep.Since != statsT0+150 {
		t.Fatalf("since %d", rep.Since)
	}
	if all, _ := s.Stats(time.Time{}); statsRow(t, all.Agents, "eng-1").Tasks != 4 || all.Since != 0 {
		t.Fatalf("default is all time: %+v", all)
	}
}

// AC4: Stats reads the store and writes nothing.
func TestStatsIsReadOnly(t *testing.T) {
	s := seedStats(t)
	snap := func() string {
		tasks, _ := s.Store.ListTasks("")
		u, _ := s.Store.UsageByAgent(0)
		ev, _ := s.Store.TaskEventStats()
		var evs []string
		for id, e := range ev {
			evs = append(evs, fmt.Sprintf("%s:%+v", id, *e))
		}
		sort.Strings(evs)
		return fmt.Sprintf("%v|%d|%v|%v", tasks, s.Store.LastEventID(), u, evs)
	}
	before := snap()
	for i := 0; i < 2; i++ {
		if _, err := s.Stats(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if after := snap(); after != before {
		t.Fatalf("Stats changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

// Agents that only spend (orchestrator, an id no longer in team.yaml) get a by-agent row
// and count toward the total, which therefore equals the sum of the spend column.
func TestStatsIncludesSpendOnlyAgents(t *testing.T) {
	s := seedStats(t)
	for _, u := range []struct {
		id  string
		usd float64
	}{{"orch", 0.35}, {"gone", 0.25}} {
		if err := s.Store.AddUsage(u.id, 1, 1, u.usd); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Stats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	o := statsRow(t, rep.Agents, "orch")
	if o.SpendUSD != 0.35 || o.Merged != 0 || o.Tasks != 0 {
		t.Errorf("orch row = %+v", o)
	}
	if g := statsRow(t, rep.Agents, "gone"); g.SpendUSD != 0.25 {
		t.Errorf("gone row = %+v", g)
	}
	var sum float64
	for _, r := range rep.Agents {
		sum += r.SpendUSD
	}
	if diff := rep.TotalSpendUSD - sum; diff > 1e-9 || diff < -1e-9 || rep.TotalSpendUSD < 4.09 {
		t.Errorf("total = %v, sum of rows = %v", rep.TotalSpendUSD, sum)
	}
	var gsum float64
	for _, g := range rep.Groups {
		gsum += g.SpendUSD
	}
	if diff := gsum - rep.TotalSpendUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("group spend %v != total %v", gsum, rep.TotalSpendUSD)
	}
}
