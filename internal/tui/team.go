package tui

import (
	"fmt"
	"github.com/oorbt/slai/internal/agentrows"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func isWaiting(status string) bool { return agentrows.IsWaiting(status) }

// glyph returns the status glyph (PRD 7.1/7.3).
func glyph(status string) string {
	switch status {
	case "working", "idle":
		return "●"
	case "waiting", "waiting_on_user":
		return "▲"
	case "review", "finished":
		return "◆"
	}
	return "○" // dead, errored, unknown
}

// orderAgents merges the team config order with stored state (grouping and
// waiting-first ordering happen in buildItems).
// Agents in the team but not yet in the store appear as "dead".
func orderAgents(team *config.Team, stored []store.Agent) []store.Agent {
	byID := map[string]store.Agent{}
	for _, a := range stored {
		byID[a.ID] = a
	}
	var out []store.Agent
	seen := map[string]bool{}
	if team != nil {
		for _, ta := range team.Agents {
			a, ok := byID[ta.ID]
			if !ok {
				a = store.Agent{ID: ta.ID, Role: ta.Role, Model: ta.Model, Status: "dead"}
			}
			if a.Model == "" {
				a.Model = ta.Model
			}
			if a.Role == "" {
				a.Role = ta.Role
			}
			out = append(out, a)
			seen[ta.ID] = true
		}
	}
	for _, a := range stored {
		if !seen[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// headerRight renders "5 agents · 2 working · 1 needs you · $3.42 today".
func headerRight(agents []store.Agent, spend float64, paused bool) string {
	var working, waiting, dead int
	for _, a := range agents {
		switch {
		case a.Status == "working":
			working++
		case isWaiting(a.Status):
			waiting++
		case a.Status == "dead" || a.Status == "errored":
			dead++
		}
	}
	parts := []string{fmt.Sprintf("%d agents", len(agents))}
	if working > 0 {
		parts = append(parts, fmt.Sprintf("%d working", working))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d needs you", waiting))
	}
	if dead > 0 {
		parts = append(parts, fmt.Sprintf("%d dead", dead))
	}
	parts = append(parts, fmt.Sprintf("~$%.2f today", spend))
	if paused {
		parts = append(parts, "PAUSED")
	}
	return strings.Join(parts, " · ")
}

// newlyWaiting returns the agents that should ring the bell (waiting now and
// not previously notified) and the updated notified set.
func newlyWaiting(notified map[string]bool, agents []store.Agent) (next map[string]bool, ring []string) {
	next = map[string]bool{}
	for _, a := range agents {
		if isWaiting(a.Status) {
			next[a.ID] = true
			if !notified[a.ID] {
				ring = append(ring, a.ID)
			}
		}
	}
	return next, ring
}

// headerStyled is headerRight with colour: spend turns amber at 80% of the
// daily cap and red at the cap (cap 0 = none); PAUSED is amber.
func headerStyled(agents []store.Agent, spend, dailyCap float64, paused bool) string {
	plain := headerRight(agents, spend, false)
	spendTxt := fmt.Sprintf("~$%.2f today", spend)
	if dailyCap > 0 {
		newTxt := fmt.Sprintf("~$%.2f/$%.2f today", spend, dailyCap)
		switch {
		case spend >= dailyCap:
			newTxt = styleErr.Render(newTxt)
		case spend >= 0.8*dailyCap:
			newTxt = styleAmber.Render(newTxt)
		}
		plain = strings.Replace(plain, spendTxt, newTxt, 1)
	}
	if paused {
		plain += " · " + styleAmber.Render("⏸ PAUSED")
	}
	return plain
}

// attentionBanner builds the §7.3 line for agents waiting on the user, with
// the question from their latest ask_user event (events are newest first).
func attentionBanner(agents []store.Agent, events []store.Event) string {
	var parts []string
	for _, a := range agents {
		if a.Status != "waiting_on_user" {
			continue
		}
		q := ""
		for _, e := range events {
			if e.Kind == "ask_user" && e.Agent == a.ID {
				q = firstString(parseData(e.Data), "question", "reason")
				break
			}
		}
		s := "▲ " + a.ID + " is waiting on you"
		if q != "" {
			s += ": " + trunc(q, 100)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return styleAmber.Render(strings.Join(parts, "  "))
}

// systemBannerWindow is how long a supervisor-raised ask_user (no agent: daily
// spend cap, message loop) stays in the banner.
const systemBannerWindow = 15 * time.Minute

// systemBanner surfaces the newest supervisor ask_user event that has no
// asking agent (so attentionBanner cannot show it) while it is still fresh.
func systemBanner(events []store.Event, now time.Time) string {
	for _, e := range events { // newest first
		if e.Kind != "ask_user" || e.Agent != "" {
			continue
		}
		if now.Sub(time.Unix(e.At, 0)) > systemBannerWindow {
			return ""
		}
		d := parseData(e.Data)
		msg := firstString(d, "question", "reason")
		if r := firstString(d, "reason"); r != "" && r != msg {
			msg = r + ": " + msg
		}
		return styleAmber.Render("▲ supervisor needs you: " + trunc(msg, 100))
	}
	return ""
}
