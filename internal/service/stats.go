package service

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

// StatsRow is the quality numbers of one agent or of one harness+model group.
// The counts are raw so a group's ratios are computed from its totals, not averaged.
type StatsRow struct {
	Agent   string   `json:"agent,omitempty"`  // set on a per-agent row
	Agents  []string `json:"agents,omitempty"` // set on a group row
	Role    string   `json:"role,omitempty"`
	Harness string   `json:"harness"`
	Model   string   `json:"model"`

	Tasks         int   `json:"tasks"`                             // assigned in the window
	Merged        int   `json:"merged"`                            // currently merged
	Cancelled     int   `json:"cancelled"`                         //
	Reverted      int   `json:"reverted"`                          // merged, then taken back out with `slai revert`
	ReviewRounds  int   `json:"review_rounds"`                     // request_review calls across the merged tasks
	Reworked      int   `json:"reworked"`                          // merged tasks that got at least one request_changes
	MergeRefused  int   `json:"merge_refused"`                     // merge_refused events across all the tasks
	MedianSeconds int64 `json:"median_assigned_to_merged_seconds"` // 0 when nothing merged

	SpendUSD float64 `json:"spend_usd"`

	// Derived; 0 when nothing merged.
	RoundsPerMerged float64 `json:"rounds_per_merged"`
	ReworkRate      float64 `json:"rework_rate"`
	SpendPerMerged  float64 `json:"spend_per_merged_usd"`

	durations []int64
}

// StatsReport is what `slai status --stats` prints and --json emits. The JSON shape is
// stable: new fields are only ever added.
type StatsReport struct {
	Since  int64      `json:"since"` // unix seconds; 0 is all time
	Agents []StatsRow `json:"agents"`
	Groups []StatsRow `json:"groups"`
	// TotalSpendUSD is the recorded usage of every agent in the window, including
	// agents with no merged tasks (orchestrator, PM, principal, focus groups).
	TotalSpendUSD float64 `json:"total_spend_usd"`
}

// Stats aggregates per-agent and per-harness+model quality numbers from tasks, events
// and usage. since filters by assignment time (the task's creation): tasks assigned
// before it are left out, and spend counts only usage recorded at or after it. The zero
// time means all time. Every engineer in the team is listed, with zeros when it has no
// tasks; any other assignee with tasks, and any agent with recorded usage, is listed too. It only reads the store, so it is
// safe to call from the TUI on every refresh.
func (s *Service) Stats(since time.Time) (StatsReport, error) {
	var from int64
	if !since.IsZero() {
		from = since.Unix()
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return StatsReport{}, err
	}
	evs, err := s.Store.TaskEventStats()
	if err != nil {
		return StatsReport{}, err
	}
	spend, err := s.Store.UsageByAgent(from)
	if err != nil {
		return StatsReport{}, err
	}

	rows := map[string]*StatsRow{}
	var order []string
	add := func(id, role, harness, model string) *StatsRow {
		if r := rows[id]; r != nil {
			return r
		}
		r := &StatsRow{Agent: id, Role: role, Harness: harness, Model: model}
		rows[id] = r
		order = append(order, id)
		return r
	}
	team := s.team()
	byID := map[string]config.Agent{}
	for _, a := range team.Agents {
		byID[a.ID] = a
		if a.Role == config.RoleEngineer {
			add(a.ID, a.Role, a.HarnessKind(), team.ModelFor(a))
		}
	}
	for _, t := range tasks {
		if t.CreatedAt < from || t.Assignee == "" {
			continue
		}
		r := rows[t.Assignee]
		if r == nil {
			role, harness, model := "", config.HarnessClaude, ""
			if a, ok := byID[t.Assignee]; ok {
				role, harness, model = a.Role, a.HarnessKind(), team.ModelFor(a)
			}
			r = add(t.Assignee, role, harness, model)
		}
		r.Tasks++
		e := evs[t.ID]
		if e == nil {
			e = &store.TaskEventStats{}
		}
		r.MergeRefused += e.Refused
		switch t.Status {
		case store.StatusCancelled:
			r.Cancelled++
		case store.StatusReverted:
			r.Reverted++
		case store.StatusMerged:
			r.Merged++
			r.ReviewRounds += e.Reviews
			if e.Changes > 0 {
				r.Reworked++
			}
			end := e.MergedAt
			if end == 0 {
				end = t.UpdatedAt
			}
			d := end - t.CreatedAt
			if d < 0 {
				d = 0
			}
			r.durations = append(r.durations, d)
		}
	}

	// Agents that only spent (orch, pm, principal, focus groups, or ids no longer in
	// team.yaml) get a row too, so the by-agent spend column adds up to the total.
	var spenders []string
	for id := range spend {
		if rows[id] == nil && spend[id] > 0 {
			spenders = append(spenders, id)
		}
	}
	sort.Strings(spenders)
	for _, id := range spenders {
		role, harness, model := "", config.HarnessClaude, ""
		if a, ok := byID[id]; ok {
			role, harness, model = a.Role, a.HarnessKind(), team.ModelFor(a)
		}
		add(id, role, harness, model)
	}

	rep := StatsReport{Since: from, Agents: []StatsRow{}, Groups: []StatsRow{}}
	groups := map[string]*StatsRow{}
	var gorder []string
	for _, id := range order {
		r := rows[id]
		r.SpendUSD = spend[id]
		rep.TotalSpendUSD += r.SpendUSD
		g := groups[r.Harness+"/"+r.Model]
		if g == nil {
			g = &StatsRow{Harness: r.Harness, Model: r.Model}
			groups[r.Harness+"/"+r.Model] = g
			gorder = append(gorder, r.Harness+"/"+r.Model)
		}
		g.Agents = append(g.Agents, id)
		g.Tasks += r.Tasks
		g.Merged += r.Merged
		g.Cancelled += r.Cancelled
		g.Reverted += r.Reverted
		g.ReviewRounds += r.ReviewRounds
		g.Reworked += r.Reworked
		g.MergeRefused += r.MergeRefused
		g.SpendUSD += r.SpendUSD
		g.durations = append(g.durations, r.durations...)
		r.finish()
		rep.Agents = append(rep.Agents, *r)
	}
	for _, k := range gorder {
		g := groups[k]
		g.finish()
		rep.Groups = append(rep.Groups, *g)
	}
	return rep, nil
}

// finish fills the derived fields from the raw counts.
func (r *StatsRow) finish() {
	if r.Merged > 0 {
		m := float64(r.Merged)
		r.RoundsPerMerged = float64(r.ReviewRounds) / m
		r.ReworkRate = float64(r.Reworked) / m
		r.SpendPerMerged = r.SpendUSD / m
	}
	r.MedianSeconds = median(r.durations)
	r.durations = nil
}

func median(d []int64) int64 {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	if n := len(d); n%2 == 0 {
		return (d[n/2-1] + d[n/2]) / 2
	}
	return d[len(d)/2]
}

// ParseSince reads a --since value: a count of days or weeks ("7d", "2w") or any
// time.ParseDuration string ("36h"). It returns the cutoff time, now minus that span.
func ParseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if u, ok := unit[v[len(v)-1]]; ok {
		n, err := strconv.Atoi(v[:len(v)-1])
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("--since %q: want a span like 7d, 2w or 36h", v)
		}
		return now.Add(-time.Duration(n) * u), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("--since %q: want a span like 7d, 2w or 36h", v)
	}
	return now.Add(-d), nil
}
