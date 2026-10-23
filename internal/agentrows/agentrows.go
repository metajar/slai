// Package agentrows is the one source of what an agent list row says: the status wording, the
// current task, how long ago the status changed, why a health chip is shown, the compact model
// name, the harness tag and the grouping. The TUI's agent list and the web UI's Agents page
// both build their rows from it, so the two cannot drift; each adds only its own presentation
// (colours and key hints in the TUI, markup in the web).
package agentrows

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Group is one section of the agent list.
type Group struct {
	Key, Title string
	Roles      []string
}

// Groups orders the list: orchestrator, planning, engineers, focus group.
var Groups = []Group{
	{"orchestrator", "Orchestrator", []string{config.RoleOrchestrator}},
	{"planning", "Planning", []string{config.RolePM, config.RolePrincipal}},
	{"engineers", "Engineers", []string{config.RoleEngineer}},
	{"focus", "Focus group", []string{config.RoleFocusGroup}},
}

// GroupKey is the key of the group a role belongs to.
func GroupKey(role string) string {
	for _, g := range Groups {
		for _, r := range g.Roles {
			if r == role {
				return g.Key
			}
		}
	}
	return "engineers"
}

// GroupTitle is a group's heading ("" key: the key itself).
func GroupTitle(key string) string {
	for _, g := range Groups {
		if g.Key == key {
			return g.Title
		}
	}
	return key
}

// IsWaiting reports whether the stored status means the agent needs the user.
func IsWaiting(status string) bool { return status == "waiting" || status == "waiting_on_user" }

// StatusLabel is the wording for a stored status.
func StatusLabel(status string) string {
	switch status {
	case "waiting", "waiting_on_user":
		return "needs you"
	case "finished":
		return "done"
	case "":
		return "dead"
	}
	return status
}

// ShortModel trims a model alias or id to a compact label.
func ShortModel(m string) string {
	l := strings.ToLower(m)
	for _, k := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(l, k) {
			return k
		}
	}
	return strings.TrimPrefix(l, "claude-")
}

// HarnessTag is the short tag shown after the id of an agent on a non-claude harness.
func HarnessTag(kind string) string {
	switch kind {
	case config.HarnessOpenCode:
		return "oc"
	case config.HarnessPi:
		return "pi"
	}
	return ""
}

// HarnessOf is the harness kind of agent id in team (claude when unknown).
func HarnessOf(team *config.Team, id string) string {
	if team != nil {
		for _, a := range team.Agents {
			if a.ID == id {
				return a.HarnessKind()
			}
		}
	}
	return config.HarnessClaude
}

// Chip is the health chip of a row: a label only for a liveness the status alone does not
// explain (stale, rate-limited, gated, or dead while the status says otherwise), and the
// reason. "blocked" gives no label (the waiting status already labels the row) but keeps its
// reason.
func Chip(h service.Health, status string) (label, detail string) {
	switch h.Liveness {
	case "stale", "rate_limited":
	case "gated":
		return "gated", h.Reason
	case "blocked":
		return "", h.Reason
	case "dead":
		if status == "dead" || status == "" {
			return "", ""
		}
	default:
		return "", ""
	}
	return strings.ReplaceAll(h.Liveness, "_", "-"), h.Reason
}

// Reason is why a row shows trouble: the liveness reason when it is blocked, gated, stale,
// dead or rate-limited, empty for a healthy agent.
func Reason(h service.Health) string {
	switch h.Liveness {
	case "blocked", "gated", "stale", "dead", "rate_limited":
		return h.Reason
	}
	return ""
}

// Age renders a compact age: 45s, 4m, 5h, 3d.
func Age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// StatusAge is how long ago the agent's status last changed ("" when never recorded).
func StatusAge(a store.Agent, now time.Time) string {
	if a.StatusAt <= 0 {
		return ""
	}
	return Age(now.Sub(time.Unix(a.StatusAt, 0)))
}

// ActivityText is the part of the activity line the two UIs word differently.
type ActivityText struct {
	// NotRunning is the line for a dead or errored agent ("not running · v to revive" in the TUI).
	NotRunning string
	// Event renders the agent's latest event after "last: " (its kind on the web, a formatted
	// line in the TUI).
	Event func(store.Event) string
}

// Activity is the second line of a row: the current task (with its title when known), else
// "not running", else the latest tool or event the agent produced (events newest first), else
// "idle". The selection is shared; the wording is ActivityText.
func Activity(a store.Agent, taskTitle string, events []store.Event, t ActivityText) string {
	if a.CurrentTask != "" {
		if taskTitle != "" {
			return a.CurrentTask + " " + taskTitle
		}
		return a.CurrentTask
	}
	if a.Status == "dead" || a.Status == "errored" {
		return t.NotRunning
	}
	for _, e := range events {
		if e.Agent != a.ID || strings.HasPrefix(e.Kind, "usage:") {
			continue
		}
		if strings.HasPrefix(e.Kind, "hook:") {
			if tool := hookTool(e.Data); tool != "" {
				return "last: " + tool
			}
			continue
		}
		if t.Event != nil {
			return "last: " + t.Event(e)
		}
		return "last: " + e.Kind
	}
	return "idle"
}

func hookTool(data string) string {
	var m struct {
		Tool string `json:"tool"`
	}
	_ = json.Unmarshal([]byte(data), &m)
	return m.Tool
}

// Row is everything an agent row says, computed once: the TUI list and the web list both read
// their fields from it.
type Row struct {
	ID, Role, Group string
	Status          string // the stored status
	StatusLabel     string // "needs you", "done", "dead", else the status
	Waiting         bool   // the agent needs the user
	Task, TaskTitle string // the current task and its title
	Activity        string // task + title, else not running, else the latest tool or event, else idle
	Age             string // how long ago the status changed ("" if never recorded)
	Liveness        string // the classifier's value (service.Health.Liveness)
	Reason          string // why, when blocked, gated, stale, dead or rate-limited
	ChipLabel       string // "", "gated", "stale", "rate-limited", ...
	ChipDetail      string // the chip's reason
	Harness, Tag    string // claude | opencode | pi, and its short tag ("" for claude)
	Model           string // compact model name
	Identity        Identity
}

// Identity is what an agent's row says about its identity grounding: when it was last
// re-grounded (a restart or compaction re-ground by the hooks, or `slai ground`) and any
// identity correction raised since.
type Identity struct {
	GroundAt int64  // unix time of the last re-ground event, 0 when never
	Source   string // what re-grounded it: a SessionStart source, or "user"
	// Correction is the open identity_correction's text ("" when none is open): the newest
	// correction event that no re-ground has followed.
	Correction   string
	CorrectionAt int64
}

// IdentityOf reads the agent's identity state from its events.
func IdentityOf(st *store.Store, id string) Identity {
	var out Identity
	var groundID int64 // event ids order events recorded in the same second
	for _, kind := range []string{"identity_reground", "identity_ground"} {
		if evs, err := st.EventsByKind(id, kind, 1); err == nil && len(evs) > 0 && evs[0].ID > groundID {
			groundID = evs[0].ID
			out.GroundAt = evs[0].At
			var d struct{ Source string }
			_ = json.Unmarshal([]byte(evs[0].Data), &d)
			out.Source = d.Source
			if kind == "identity_ground" {
				out.Source = "user"
			}
		}
	}
	if evs, err := st.EventsByKind(id, "identity_correction", 1); err == nil && len(evs) > 0 && evs[0].ID > groundID {
		var d struct{ Reason, Text, Signal string }
		_ = json.Unmarshal([]byte(evs[0].Data), &d)
		out.Correction = firstNonEmpty(d.Reason, d.Text, d.Signal, "identity correction")
		out.CorrectionAt = evs[0].At
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// GroundText words the last re-ground for a row: "re-grounded 3m ago" or "never re-grounded".
func (i Identity) GroundText(now time.Time) string {
	if i.GroundAt == 0 {
		return "never re-grounded"
	}
	return "re-grounded " + Age(now.Sub(time.Unix(i.GroundAt, 0))) + " ago"
}

// Input is what Build needs besides the agent and its health.
type Input struct {
	Titles   map[string]string // task id -> title
	Events   []store.Event     // newest first
	Harness  string            // the agent's harness kind
	Identity Identity          // the agent's identity grounding (IdentityOf)
	Now      time.Time
	Text     ActivityText
}

// Build computes one agent's row from the stored agent and the classifier's verdict h. It is
// the only place the row's fields are derived.
func Build(a store.Agent, h service.Health, in Input) Row {
	chip, detail := Chip(h, a.Status)
	kind := in.Harness
	if kind == "" {
		kind = config.HarnessClaude
	}
	return Row{
		ID: a.ID, Role: a.Role, Group: GroupKey(a.Role), Status: a.Status, StatusLabel: StatusLabel(a.Status), Waiting: IsWaiting(a.Status),
		Task: a.CurrentTask, TaskTitle: in.Titles[a.CurrentTask], Activity: Activity(a, in.Titles[a.CurrentTask], in.Events, in.Text),
		Age: StatusAge(a, in.Now), Liveness: h.Liveness, Reason: Reason(h), ChipLabel: chip, ChipDetail: detail,
		Harness: kind, Tag: HarnessTag(kind), Model: ShortModel(a.Model), Identity: in.Identity,
	}
}
