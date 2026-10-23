package agents

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// Row is one agent as the list shows it. The fields are the ones the TUI's agent rows
// show (status, current task, activity, harness tag, model, health chip), computed from
// the same store rows and the same classifier (service.AgentHealthGated).
type Row struct {
	ID, Role, Group string
	Status          string // the stored status
	StatusLabel     string // what the TUI prints for it ("needs you", "done", ...)
	Waiting         bool   // the agent needs the user
	Task, TaskTitle string
	Activity        string // task, else the latest tool or event, else "idle"
	Harness         string // claude | opencode | pi
	Tag             string // "oc" / "pi" / "" (the TUI's harness tag)
	Model           string
	Liveness        string // service.Health.Liveness
	Reason          string // why, when blocked, gated, stale, dead or rate-limited (agentrows.Reason)
	ChipLabel       string // TUI health chip: "", "gated", "stale", "rate-limited", ...
	Ago             string // how long ago the status changed (agentrows.StatusAge), "" if never
	Paused          bool
	Grounded        string // "re-grounded 3m ago" / "never re-grounded" (agentrows.Identity)
	Correction      string // the open identity correction, "" when none
	Ctx             string // "ctx 142k" (with the `slai clear` hint when over the threshold), "" when not recorded yet
	CtxOver         bool
	Reset           string // "reset 3m ago" / "never reset"
	Gauges          string // "n/a — not supported by pi" when the harness has no subscription gauges
	NoGauges        bool   // the harness has no plan gauges at all (the list says so in its harness cell)
}

func features(kind string) harness.Features {
	h, err := harness.For(harness.Kind(kind))
	if err != nil {
		h, _ = harness.For(harness.Claude)
	}
	return h.Features()
}

// degradedText is the TUI's wording of a harness's missing capabilities.
func degradedText(kind string) string {
	d := features(kind).Degraded()
	if len(d) == 0 {
		return "none"
	}
	return strings.Join(d, ", ") + " (not supported by " + kind + ")"
}

func gaugesNA(kind string) bool { return !features(kind).Statusline }

func parseData(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Ago renders an age like the TUI: 45s, 3m, 2h, 4d (agentrows.Age).
func Ago(d time.Duration) string { return agentrows.Age(d) }

// Inputs is everything the list is built from, already read (so a test can feed the TUI and the
// web the same facts).
type Inputs struct {
	Agents []store.Agent
	Tasks  []store.Task
	Events []store.Event // newest first
	Paused map[string]bool
	Health map[string]service.Health
	// Identity is each agent's identity grounding (agentrows.IdentityOf); a missing entry reads as never re-grounded.
	Identity map[string]agentrows.Identity
	// Context is each agent's context size and last reset (agentrows.ContextOf); a missing entry reads as not recorded.
	Context map[string]agentrows.Context
	Team    *config.Team
	Now     time.Time
}

// BuildRows is the list's row builder: every agent in the store, grouped like the TUI list
// (orchestrator, planning, engineers, focus group; agents that need the user first).
func BuildRows(st *store.Store, team *config.Team, now time.Time) ([]Row, error) {
	agents, err := st.GetAgents()
	if err != nil {
		return nil, err
	}
	tasks, err := st.ListTasks("")
	if err != nil {
		return nil, err
	}
	events, _ := st.RecentEvents(300)
	paused, _ := st.PausedAgents()
	cfg := service.GateConfigOf(team)
	health := map[string]service.Health{}
	ident := map[string]agentrows.Identity{}
	ctxs := map[string]agentrows.Context{}
	for _, a := range agents {
		ident[a.ID] = agentrows.IdentityOf(st, a.ID)
		ctxs[a.ID] = agentrows.ContextOf(st, a.ID, team.Defaults.ContextThreshold())
		alive := a.Status != "dead" && a.Status != ""
		health[a.ID] = service.AgentHealthGated(st, a.ID, alive, now, cfg)
	}
	return RowsFrom(Inputs{Agents: agents, Tasks: tasks, Events: events, Paused: paused, Health: health, Identity: ident, Context: ctxs, Team: team, Now: now}), nil
}

// RowsFrom builds the rows from already-read inputs. Every field of the row that the TUI list
// also shows comes from agentrows.Build, the TUI's builder too.
func RowsFrom(in Inputs) []Row {
	titles := map[string]string{}
	for _, t := range in.Tasks {
		titles[t.ID] = t.Title
	}
	text := agentrows.ActivityText{NotRunning: "not running", Event: func(e store.Event) string { return e.Kind }}
	var rows []Row
	for _, a := range in.Agents {
		kind := agentrows.HarnessOf(in.Team, a.ID)
		c := agentrows.Build(a, in.Health[a.ID], agentrows.Input{Titles: titles, Events: in.Events, Harness: kind, Identity: in.Identity[a.ID], Context: in.Context[a.ID], Now: in.Now, Text: text})
		r := Row{ID: c.ID, Role: c.Role, Group: c.Group, Status: c.Status, StatusLabel: c.StatusLabel, Waiting: c.Waiting,
			Task: c.Task, TaskTitle: c.TaskTitle, Activity: c.Activity, Harness: c.Harness, Tag: c.Tag, Model: c.Model,
			Liveness: c.Liveness, Reason: c.Reason, ChipLabel: c.ChipLabel, Ago: c.Age, Paused: in.Paused[a.ID], Grounded: c.Identity.GroundText(in.Now), Correction: c.Identity.Correction, Ctx: c.Context.Size(), CtxOver: c.Context.Over(), Reset: c.Context.Reset(in.Now)}
		if gaugesNA(kind) {
			r.Gauges = "n/a — not supported by " + kind
			r.NoGauges = true
		} else {
			r.Gauges = "shared plan gauges: see the Dashboard"
		}
		rows = append(rows, r)
	}
	order := map[string]int{}
	for i, g := range agentrows.Groups {
		order[g.Key] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if order[rows[i].Group] != order[rows[j].Group] {
			return order[rows[i].Group] < order[rows[j].Group]
		}
		return rows[i].Waiting && !rows[j].Waiting
	})
	return rows
}

// GroupTitle is the heading of a row's group.
func GroupTitle(key string) string { return agentrows.GroupTitle(key) }

// healthEventKinds are the event kinds the detail lists as "why did slai do that"
// (the TUI's list).
var healthEventKinds = []string{
	"nudge", "unread_nudge", "stuck_detected", "stuck_nudge", "stuck_restart", "stuck_failed", "stuck_cleared", "stuck_recovered",
	"auto_revive", "revive_fresh", "agent_restarted", "agent_upgraded", "agent_added", "agent_removed",
	"mcp_restart", "mcp_dead", "stalled", "loop_released", "wake_gave_up", "spend_cap",
	"rate_limited", "rate_limited_all", "modal_stuck", "revive_gave_up", "orch_down", "unread_stuck", "stuck_gave_up",
	"guard:lane", "lane_denied", "guard_missing", "orch_restart", "agent_stopped",
}

// HealthEvent is one line of the detail's "recent health events".
type HealthEvent struct {
	Kind, Ago, Cause string
}

// HealthEvents returns the agent's n newest health events, newest first, each with the
// cause its own data gives (reason, cause, why, text, error or tool).
func HealthEvents(st *store.Store, agent string, n int, now time.Time) []HealthEvent {
	var all []store.Event
	for _, k := range healthEventKinds {
		if es, err := st.EventsByKind(agent, k, n); err == nil {
			all = append(all, es...)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	if len(all) > n {
		all = all[:n]
	}
	var out []HealthEvent
	for _, e := range all {
		kind := e.Kind
		if kind == "guard:lane" {
			kind = "lane_denied"
		}
		out = append(out, HealthEvent{Kind: kind, Ago: Ago(now.Sub(time.Unix(e.At, 0))),
			Cause: firstString(parseData(e.Data), "cause", "reason", "why", "text", "error", "tool")})
	}
	return out
}

// Denial is one lane denial.
type Denial struct{ Ago, What string }

// LaneDenials returns the count over the last 24 hours and the newest three denials.
func LaneDenials(st *store.Store, id string, now time.Time) (count int, last []Denial) {
	counts, _ := st.LaneDenialCounts(now.Add(-24 * time.Hour).Unix())
	evs, _ := st.EventsByKind(id, "lane_denied", 3)
	for _, e := range evs {
		d := parseData(e.Data)
		what := firstString(d, "tool")
		if t := firstString(d, "path", "target"); t != "" {
			what += " " + t
		}
		last = append(last, Denial{Ago: Ago(now.Sub(time.Unix(e.At, 0))), What: what})
	}
	return counts[id], last
}

// Correction is a course-correction message sent to the agent.
type Correction struct{ Ago, Text string }

// Corrections returns the newest n course corrections sent to the agent.
func Corrections(st *store.Store, id string, n int, now time.Time) []Correction {
	msgs, err := st.RecentMessages(300)
	if err != nil {
		return nil
	}
	var out []Correction
	for _, m := range msgs {
		if m.To == id && strings.HasPrefix(m.Body, supervisor.CoursePrefix) {
			out = append(out, Correction{Ago: Ago(now.Sub(time.Unix(m.CreatedAt, 0))), Text: strings.TrimPrefix(m.Body, supervisor.CoursePrefix)})
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// Message is one line of the agent's message history.
type Message struct {
	ID        int64
	From, To  string
	Kind      string
	TaskID    string
	Ago, Body string
}

// History returns the newest n messages to or from the agent, newest first.
func History(st *store.Store, id string, n int, now time.Time) []Message {
	msgs, err := st.RecentMessages(500)
	if err != nil {
		return nil
	}
	var out []Message
	for _, m := range msgs {
		if m.To != id && m.From != id {
			continue
		}
		out = append(out, Message{ID: m.ID, From: m.From, To: m.To, Kind: m.Kind, TaskID: m.TaskID,
			Ago: Ago(now.Sub(time.Unix(m.CreatedAt, 0))), Body: strings.TrimPrefix(m.Body, supervisor.CoursePrefix)})
		if len(out) == n {
			break
		}
	}
	return out
}
