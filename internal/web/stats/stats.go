// Package stats is the Stats page of `slai serve`: the quality numbers per agent and per
// harness/model (the same service.Stats that `slai status --stats` prints), spend against
// the caps, and per-task spend. It is read-only. A number the data cannot support is shown
// as "n/a" with the reason, never as 0.
package stats

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// warnFraction is where a cap turns amber (the supervisor warns at 80%).
const warnFraction = 0.8

// The page body is a live region (data-live="stats spend", see the template): slai.js refetches
// /f/stats on a notice from the SSE hub, and every 30 s only while the stream is down. The
// page itself carries no polling trigger.

// Module registers /stats and its refetch fragment.
func Module(d *server.Deps) []server.Route {
	m := &mod{d: d}
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/stats", Calls: "service.Stats (the numbers behind `slai status --stats`), the spend queries and the task usage totals; read-only", Handler: m.page},
		{Method: http.MethodGet, Pattern: "/f/stats", Calls: "the same read-only data, as the page's refetch fragment", Handler: m.fragment},
	}
}

type mod struct{ d *server.Deps }

// Definitions are the tooltips and footnote of the column headers (one source for both).
var Definitions = []struct{ Col, Meaning string }{
	{"merged", "tasks merged into the integration branch"},
	{"cancelled / reverted", "tasks cancelled, and tasks merged then taken back out with slai revert"},
	{"rounds", "request_review calls per merged task"},
	{"rework", "share of merged tasks that got at least one request_changes"},
	{"refused", "merge_refused events: a merge the test gate or a moved commit stopped"},
	{"spend", "recorded cost of the agent's turns"},
	{"$ / merged", "spend divided by merged tasks"},
	{"median time", "median time from assignment to merge"},
}

// Cell is one table cell: a value, or n/a with the reason it is not known.
type Cell struct {
	Text string
	NA   bool
	Why  string
}

type Row struct {
	Name, Where        string
	Merged, Cancelled  int
	Reverted, Refused  int
	Rounds, Rework     Cell
	Spend, Per, Median Cell
}

type CapRow struct {
	Label, Text, Class, Note string
	Pct                      int
}

type TaskRow struct {
	ID, Title, Assignee string
	Spend, Tokens       Cell
	Rounds              int
}

type View struct {
	Since    string // the ?since value as typed ("" = all time)
	LiveSrc  string // the live region's refetch URL (built here so the template does not re-escape it)
	Window   string
	Agents   []Row
	Groups   []Row
	Caps     []CapRow
	CapsOff  bool
	Tasks    []TaskRow
	Defs     []struct{ Col, Meaning string }
	BadSince string
}

func (m *mod) view(r *http.Request) (View, error) {
	v := View{Since: strings.TrimSpace(r.URL.Query().Get("since")), Defs: Definitions}
	v.LiveSrc = "/f/stats?" + url.Values{"since": {v.Since}}.Encode()
	now := m.d.Now()
	cut, err := service.ParseSince(v.Since, now)
	if err != nil {
		v.BadSince = err.Error()
		v.Since = ""
		cut = time.Time{}
	}
	v.Window = "all time"
	var from int64
	if !cut.IsZero() {
		from = cut.Unix()
		v.Window = "tasks assigned since " + cut.Local().Format("2006-01-02 15:04")
	}
	team := m.d.Team()
	svc := &service.Service{Store: m.d.Store, Team: team, Dir: m.d.Dir, Now: m.d.Now}
	rep, err := svc.Stats(cut)
	if err != nil {
		return v, err
	}
	spendRows, err := m.d.Store.UsageByAgent(from)
	if err != nil {
		return v, err
	}
	for _, a := range rep.Agents {
		v.Agents = append(v.Agents, toRow(a.Agent, a.Harness+"/"+a.Model, a, hasUsage(spendRows, []string{a.Agent}), a.Harness))
	}
	for _, g := range rep.Groups {
		v.Groups = append(v.Groups, toRow(g.Harness+"/"+g.Model, strings.Join(g.Agents, ", "), g, hasUsage(spendRows, g.Agents), g.Harness))
	}
	v.Caps, v.CapsOff = m.caps(team, now)
	v.Tasks, err = m.tasks(team, from)
	return v, err
}

func hasUsage(spend map[string]float64, agents []string) bool {
	for _, a := range agents {
		if _, ok := spend[a]; ok {
			return true
		}
	}
	return false
}

func na(why string) Cell { return Cell{NA: true, Why: why} }

func toRow(name, where string, a service.StatsRow, usage bool, harness string) Row {
	r := Row{Name: name, Where: where, Merged: a.Merged, Cancelled: a.Cancelled, Reverted: a.Reverted, Refused: a.MergeRefused}
	const noMerge = "no task has been merged in this window yet"
	if a.Merged == 0 {
		r.Rounds, r.Rework, r.Median = na(noMerge), na(noMerge), na(noMerge)
	} else {
		r.Rounds = Cell{Text: fmt.Sprintf("%.1f", a.RoundsPerMerged)}
		r.Rework = Cell{Text: fmt.Sprintf("%.0f%%", a.ReworkRate*100)}
		r.Median = Cell{Text: shortDur(a.MedianSeconds)}
	}
	if !usage {
		why := "no usage was recorded for this agent in this window"
		if harness != config.HarnessClaude {
			why += "; " + harness + " may not report per-turn cost to the hooks"
		}
		r.Spend, r.Per = na(why), na(why)
		return r
	}
	r.Spend = Cell{Text: fmt.Sprintf("$%.2f", a.SpendUSD)}
	if a.Merged == 0 {
		r.Per = na(noMerge)
	} else {
		r.Per = Cell{Text: fmt.Sprintf("$%.2f", a.SpendPerMerged)}
	}
	return r
}

// shortDur renders a duration like the TUI's stats panel: 45s, 12m, 2h05m, 3d04h.
func shortDur(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", sec)
}

// caps shows today's spend against each configured cap; with none set it says OFF.
func (m *mod) caps(team *config.Team, now time.Time) ([]CapRow, bool) {
	c := team.Defaults.Caps
	if c.PerAgentUSD <= 0 && c.DailyUSD <= 0 && c.PerTaskUSD <= 0 && c.PerRunUSD <= 0 {
		return nil, true
	}
	var rows []CapRow
	add := func(label string, spent, limit float64, note string) {
		pct := 0
		if limit > 0 {
			pct = int(spent / limit * 100)
		}
		class, state := "", "ok"
		switch {
		case limit > 0 && spent >= limit:
			class, state = "bad", "cap reached"
		case limit > 0 && spent >= warnFraction*limit:
			class, state = "warn", "over 80%"
		}
		rows = append(rows, CapRow{Label: label, Text: fmt.Sprintf("$%.2f / $%.2f · %s", spent, limit, state), Class: class, Pct: min(pct, 100), Note: note})
	}
	if c.DailyUSD > 0 {
		total, _ := m.d.Store.TodaySpend()
		add("Team, today", total, c.DailyUSD, "daily_usd: since 00:00 local, from recorded per-agent cost")
	}
	if c.PerAgentUSD > 0 {
		per, _ := m.d.Store.SpendByAgentToday()
		ids := make([]string, 0, len(per))
		for id := range per {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			add(id+", today", per[id], c.PerAgentUSD, "per_agent_usd: since 00:00 local")
		}
	}
	if c.PerRunUSD > 0 {
		var base int64
		if v, ok, _ := m.d.Store.GetKV(service.KVRunStart); ok {
			fmt.Sscan(v, &base)
		}
		run, _ := m.d.Store.SpendSince(base)
		add("This run", run, c.PerRunUSD, "per_run_usd: since the run started (slai up or the last finish)")
	}
	if c.PerTaskUSD > 0 {
		open, _ := m.d.Store.OpenTaskSpend()
		ids := make([]string, 0, len(open))
		for id := range open {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			add(id, open[id].USD, c.PerTaskUSD, "per_task_usd: spend attributed to this open task")
		}
	}
	return rows, false
}

// tasks lists per-task spend, tokens and review rounds for tasks assigned in the window.
func (m *mod) tasks(team *config.Team, from int64) ([]TaskRow, error) {
	all, err := m.d.Store.ListTasks("")
	if err != nil {
		return nil, err
	}
	usage, err := m.d.Store.UsageByTask(from)
	if err != nil {
		return nil, err
	}
	ev, err := m.d.Store.TaskEventStats()
	if err != nil {
		return nil, err
	}
	harness := map[string]string{}
	for _, a := range team.Agents {
		harness[a.ID] = a.HarnessKind()
	}
	var out []TaskRow
	for _, t := range all {
		if t.CreatedAt < from || t.Assignee == "" {
			continue
		}
		row := TaskRow{ID: t.ID, Title: t.Title, Assignee: t.Assignee}
		if e := ev[t.ID]; e != nil {
			row.Rounds = e.Reviews
		}
		if u, ok := usage[t.ID]; ok {
			row.Spend = Cell{Text: fmt.Sprintf("$%.2f", u.CostUSD)}
			row.Tokens = Cell{Text: tokens(u.Tokens)}
		} else {
			why := "no usage was recorded against this task"
			if h := harness[t.Assignee]; h != "" && h != config.HarnessClaude {
				why = t.Assignee + " runs on " + h + ": it may not report per-turn cost"
			}
			row.Spend, row.Tokens = na(why), na(why)
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	v, err := m.view(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("stats")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Stats", Active: "stats", Data: v})
}

func (m *mod) fragment(w http.ResponseWriter, r *http.Request) {
	v, err := m.view(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	tpl, err := m.d.Template("stats")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Fragment(w, r, tpl, "stats-live", v)
}
