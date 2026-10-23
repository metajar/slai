package roles

import (
	"encoding/json"
	"strings"

	"github.com/oorbt/slai/internal/config"
)

// responsibilities is the one-line job of each role. It is the single source for the
// .slai/agents.json mapping, the whoami tool and the identity line slai types after a
// gap in an agent's context.
var responsibilities = map[string]string{
	config.RoleOrchestrator: "You turn the user's goal into tasks, assign them, unblock the team and own getting every task done",
	config.RolePM:           "You write specs and acceptance criteria and keep the work aligned with what the user needs",
	config.RolePrincipal:    "You review engineers' branches against their specs, approve or request changes, and own architecture decisions",
	config.RoleEngineer:     "You implement one assigned task at a time in your own worktree, test it and send it for review",
	config.RoleFocusGroup:   "You react to designs, docs and transcripts as your persona and report what confuses you; you do not write code",
}

// Responsibility is the one-line job of role ("" for an unknown role).
func Responsibility(role string) string { return responsibilities[role] }

// Teammate is another agent on the team.
type Teammate struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

// Identity is what slai records about one agent: the data behind .slai/agents.json and
// the whoami tool.
type Identity struct {
	ID               string     `json:"id"`
	Role             string     `json:"role"`
	Responsibilities string     `json:"responsibilities"`
	Model            string     `json:"model"`
	Harness          string     `json:"harness"`
	ReportsTo        string     `json:"reports_to"` // an agent id, or "user"
	Teammates        []Teammate `json:"teammates"`
}

// reportsTo names who an agent answers to: engineers to the principal, the principal and
// PM to the orchestrator, personas to the PM, the orchestrator to the user. A missing role
// falls back to the orchestrator.
func reportsTo(t *config.Team, a config.Agent) string {
	first := func(role string) string {
		for _, x := range t.Agents {
			if x.Role == role && x.ID != a.ID {
				return x.ID
			}
		}
		return ""
	}
	var chain []string
	switch a.Role {
	case config.RoleOrchestrator:
		return "user"
	case config.RoleEngineer:
		chain = []string{config.RolePrincipal, config.RoleOrchestrator}
	case config.RoleFocusGroup:
		chain = []string{config.RolePM, config.RoleOrchestrator}
	default:
		chain = []string{config.RoleOrchestrator}
	}
	for _, role := range chain {
		if id := first(role); id != "" {
			return id
		}
	}
	return "user"
}

// IdentityOf builds the record for agent id from the team (false if it is not on it).
func IdentityOf(t *config.Team, id string) (Identity, bool) {
	for _, a := range t.Agents {
		if a.ID != id {
			continue
		}
		out := Identity{ID: a.ID, Role: a.Role, Responsibilities: responsibilities[a.Role],
			Model: t.ModelFor(a), Harness: string(a.HarnessKind()), ReportsTo: reportsTo(t, a), Teammates: []Teammate{}}
		for _, x := range t.Agents {
			if x.ID != a.ID {
				out.Teammates = append(out.Teammates, Teammate{x.ID, x.Role})
			}
		}
		return out, true
	}
	return Identity{}, false
}

// AgentsFile is the content of .slai/agents.json: every agent's Identity keyed by id, as
// indented JSON ending in a newline. Agents appear in team.yaml order inside "order".
func AgentsFile(t *config.Team) []byte {
	doc := struct {
		Order  []string            `json:"order"`
		Agents map[string]Identity `json:"agents"`
	}{Order: []string{}, Agents: map[string]Identity{}}
	for _, a := range t.Agents {
		id, _ := IdentityOf(t, a.ID)
		doc.Order = append(doc.Order, a.ID)
		doc.Agents[a.ID] = id
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n')
}

// IdentityLine is the sentence that starts every line slai types to an agent after a gap
// (restart, resume, /clear, compaction): who it is and what it is for.
func IdentityLine(id Identity) string {
	s := "You are " + id.ID + ", the " + id.Role + " of this team (see `whoami`). " + strings.TrimSuffix(id.Responsibilities, ".") + "."
	if id.Role == config.RoleOrchestrator {
		s += " You never write code; you own getting tasks done."
	}
	return s
}

// IdentityLineFor is IdentityLine for agent id of team t ("" if the agent is not on it).
func IdentityLineFor(t *config.Team, id string) string {
	if i, ok := IdentityOf(t, id); ok {
		return IdentityLine(i)
	}
	return ""
}
