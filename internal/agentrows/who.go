package agentrows

import (
	"fmt"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// Who is one line of the "Who is working" block that opens `slai status` and the dashboard:
// the agent's state in the board's own liveness words, the last thing it did, and the last
// thing slai or the orchestrator did about it.
type Who struct {
	ID    string
	State string // "working", "idle, nothing owed", "needs you", "stale (silent 12m)", "blocked: ...", ...
	Last  string // "Bash 2m ago", or "" when it has done nothing yet
	Slai  string // "slai reminded it 3m ago", "orch sent a note 5m ago", or ""
}

// Line renders it for the terminal: "eng-1  working · last: Bash 2m ago · slai woke it 3m ago".
func (w Who) Line() string {
	parts := []string{w.State}
	if w.Last != "" {
		parts = append(parts, "last: "+w.Last)
	}
	if w.Slai != "" {
		parts = append(parts, w.Slai)
	}
	return strings.Join(parts, " · ")
}

// WhoInput is what BuildWho reads, all of it already fetched once for the whole team.
type WhoInput struct {
	Agent    store.Agent
	Role     string
	Health   service.Health
	Tasks    []store.Task    // every task
	Unread   int             // undelivered messages to the agent
	Events   []store.Event   // recent, newest first (store.RecentEvents)
	Messages []store.Message // recent, newest first (store.RecentMessages)
	Orch     string          // the orchestrator's id
	Now      time.Time
}

// restartWindow is how far back "restarted N×" looks.
const restartWindow = time.Hour

// slaiActions words the events slai itself records about an agent.
var slaiActions = map[string]string{
	"wake": "woke it", "wake_retyped": "re-typed its wake-up", "unread_nudge": "re-sent its wake-up",
	"nudge": "reminded it", "stuck_nudge": "nudged it", "redirect": "redirected it",
	"identity_ground": "re-grounded it", "rate_limit_wake": "woke it after its rate limit",
}

// BuildWho computes one agent's line. The state uses only the liveness terms the board already has.
func BuildWho(in WhoInput) Who {
	a, now := in.Agent, in.Now
	w := Who{ID: a.ID}
	owed := 0
	for _, t := range in.Tasks {
		switch t.Status {
		case store.StatusAssigned, store.StatusInProgress, store.StatusChangesRequested:
			if t.Assignee == a.ID {
				owed++
			}
		case store.StatusInReview:
			if in.Role == config.RolePrincipal {
				owed++
			}
		}
	}
	silent := ""
	if in.Health.LastActivity > 0 {
		silent = Age(now.Sub(time.Unix(in.Health.LastActivity, 0)))
	}
	switch {
	case IsWaiting(a.Status):
		w.State = "needs you"
	case in.Health.Liveness == "busy" || in.Health.Liveness == "alive":
		w.State = "working"
	case in.Health.Liveness == "stale":
		w.State = "stale"
		if silent != "" {
			w.State += " (silent " + silent + ")"
		}
	case in.Health.Liveness == "blocked", in.Health.Liveness == "gated":
		w.State = in.Health.Liveness
		if in.Health.Reason != "" {
			w.State += ": " + in.Health.Reason
		}
	case in.Health.Liveness == "rate_limited":
		w.State = "rate-limited"
	case in.Health.Liveness == "dead" || a.Status == "dead" || a.Status == "":
		w.State = "dead"
	case owed == 0 && in.Unread == 0:
		w.State = "idle, nothing owed"
	default:
		w.State = "idle"
		if owed > 0 {
			w.State += fmt.Sprintf(", owes %d task(s)", owed)
		}
		if in.Unread > 0 {
			w.State += fmt.Sprintf(", %d unread", in.Unread)
		}
	}
	// restarted N× in the last hour (a deliberate stop or removal is not a restart)
	restarts := 0
	for _, e := range in.Events {
		if e.Agent == a.ID && now.Sub(time.Unix(e.At, 0)) <= restartWindow && isRestart(e.Kind) {
			restarts++
		}
	}
	if restarts > 0 {
		w.State += fmt.Sprintf(" · restarted %d× in the last hour", restarts)
	}
	for _, e := range in.Events { // newest first: the first tool hook is the last thing it did
		if e.Agent != a.ID || !strings.HasPrefix(e.Kind, "hook:") {
			continue
		}
		if tool := hookTool(e.Data); tool != "" {
			w.Last = tool + " " + Age(now.Sub(time.Unix(e.At, 0))) + " ago"
			break
		}
	}
	// the last thing done about it: whichever is newer, a slai action or a note from orch
	var slaiAt int64
	for _, e := range in.Events {
		if e.Agent == a.ID {
			if what, ok := slaiActions[e.Kind]; ok {
				w.Slai, slaiAt = "slai "+what+" "+Age(now.Sub(time.Unix(e.At, 0)))+" ago", e.At
				break
			}
		}
	}
	for _, m := range in.Messages {
		if m.To == a.ID && m.From == in.Orch && m.From != a.ID {
			if m.CreatedAt >= slaiAt {
				w.Slai = in.Orch + " sent a " + m.Kind + " " + Age(now.Sub(time.Unix(m.CreatedAt, 0))) + " ago"
			}
			break
		}
	}
	return w
}

func isRestart(kind string) bool {
	switch kind {
	case "agent_stopped", "agent_removed":
		return false
	}
	for _, k := range supervisor.RestartKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// WhoAll builds the block for every agent: one pass over the store for the shared inputs.
func WhoAll(st *store.Store, team *config.Team, agents []store.Agent, health map[string]service.Health, now time.Time) []Who {
	tasks, _ := st.ListTasks("")
	events, _ := st.RecentEvents(500)
	msgs, _ := st.RecentMessages(300)
	orch := ""
	roles := map[string]string{}
	if team != nil {
		for _, a := range team.Agents {
			roles[a.ID] = a.Role
			if a.Role == config.RoleOrchestrator && orch == "" {
				orch = a.ID
			}
		}
	}
	var out []Who
	for _, a := range agents {
		unread, _ := st.UnreadCount(a.ID)
		out = append(out, BuildWho(WhoInput{Agent: a, Role: firstNonEmpty(roles[a.ID], a.Role), Health: health[a.ID], Tasks: tasks, Unread: unread,
			Events: events, Messages: msgs, Orch: orch, Now: now}))
	}
	return out
}
