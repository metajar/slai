package tui

import "github.com/oorbt/slai/internal/store"

// defaultTarget picks the orchestrator (by role, else id "orchestrator"/"orch", else first agent).
func defaultTarget(agents []store.Agent) string {
	for _, a := range agents {
		if a.Role == "orchestrator" {
			return a.ID
		}
	}
	for _, a := range agents {
		if a.ID == "orchestrator" || a.ID == "orch" {
			return a.ID
		}
	}
	if len(agents) > 0 {
		return agents[0].ID
	}
	return ""
}
