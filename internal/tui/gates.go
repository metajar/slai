package tui

import (
	"fmt"
	"strings"
)

// wakeGateLine summarises why wake-ups are not being delivered: the global
// pause, the daily spend cap, and agents held by slai. "" means no gate is set.
func wakeGateLine(paused bool, spend, dailyCap float64, held, queued int) string {
	var parts []string
	if paused {
		parts = append(parts, "wake-ups paused (p resumes)")
	}
	if dailyCap > 0 && spend >= dailyCap {
		parts = append(parts, fmt.Sprintf("daily spend cap reached ($%.2f/$%.2f)", spend, dailyCap))
	}
	if held > 0 {
		parts = append(parts, fmt.Sprintf("%d agent(s) held by slai (u resumes)", held))
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("%d agent(s) queued for a slot (max_concurrent_agents full)", queued))
	}
	if len(parts) == 0 {
		return ""
	}
	return "⏸ wake gate: " + strings.Join(parts, " · ")
}

// agentSpendCapped reports an agent at or over its per-agent daily cap.
func agentSpendCapped(spent, perAgentCap float64) bool {
	return perAgentCap > 0 && spent >= perAgentCap
}
