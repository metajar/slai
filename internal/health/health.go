// Package health is the one place that decides what state an agent is in. The board
// (get_board, the TUI, doctor) and the supervisor both build Inputs from the store and
// call Classify, so they cannot disagree "by discipline" (the everyone-stale incident
// and the vocabulary churn after it were exactly that).
//
// The vocabulary is the typed enum below; decisions.md links here instead of
// restating it. Adding a value means adding a row to TestEveryLivenessHasATableRow's
// table and to the consumers' switches; removing one breaks the build or that test.
package health

import (
	"fmt"
	"strings"
	"time"
)

// Liveness is the state of one agent.
type Liveness string

const (
	// Dead: the tmux session is gone, or the supervisor marked the agent dead.
	Dead Liveness = "dead"
	// RateLimited: the harness hit a usage cap; slai waits for the reset.
	RateLimited Liveness = "rate_limited"
	// Blocked: parked on a permission/trust prompt or on the user. Nobody but a human can help.
	Blocked Liveness = "blocked"
	// Gated: owes work and has been silent, but slai is deliberately not waking it
	// (paused, loop-guard hold, spend cap, no free slot). Nobody needs to act.
	Gated Liveness = "gated"
	// Busy: working (mid-turn, or waiting on a background task) and active.
	Busy Liveness = "busy"
	// Alive: owes work or has unread mail, and is active.
	Alive Liveness = "alive"
	// Idle: session up and nothing owed, however long it has been quiet. Healthy.
	Idle Liveness = "idle"
	// Stale: owes work (or has unread mail, or is mid-turn) but has been silent for StaleAfter.
	Stale Liveness = "stale"
)

// All lists every value, in the order they are checked.
var All = []Liveness{Dead, RateLimited, Blocked, Gated, Busy, Alive, Idle, Stale}

func (l Liveness) String() string { return string(l) }

// Valid reports whether l is one of All.
func (l Liveness) Valid() bool {
	for _, v := range All {
		if v == l {
			return true
		}
	}
	return false
}

// Thresholds.
const (
	// StaleAfter is how long an agent that owes work may be silent before it is stale.
	StaleAfter = 10 * time.Minute
	// BusyQuiet is how long a working agent may be silent before the reason says so.
	BusyQuiet = time.Minute
)

// Ping is the outcome of the latest liveness ping.
type Ping struct {
	Result string // "alive", "no_response", "identity_mismatch", ...
	Note   string
	// Newer: the ping happened at or after the agent's last activity. A failed ping
	// older than a later tool call or hook event says nothing about now.
	Newer bool
	// HooksRecent: the agent's hooks fired shortly before the ping, so an unanswered
	// ping means "mid-turn", not "dead" (wake-ups are only typed into idle agents).
	HooksRecent bool
}

// Inputs is everything the classification depends on. It is plain data: a caller
// gathers it (service.CollectInputs does, for the board and the supervisor alike).
type Inputs struct {
	SessionAlive bool
	// Status is the agent's recorded status: working, idle, finished, waiting,
	// waiting_on_user, rate_limited, dead, ...
	Status string
	// Harness is the agent's harness kind ("" means claude); carried for reasons.
	Harness string
	// HasActivity is false when no tool call, hook event or status change was ever seen.
	HasActivity bool
	// Silent is the time since the latest of those (meaningful when HasActivity).
	Silent time.Duration
	// Owed is why the agent owes work ("owes T-0007"), "" when it owes none.
	Owed string
	// Unread is the number of unread messages.
	Unread int
	// GateReason is why slai is not waking it ("paused", "held until 14:05", "spend
	// capped", "queued for slot"), "" when it can be woken.
	GateReason string
	// BackgroundTask names a background task or monitor the agent is waiting on
	// ("1 monitor"), "" when none. Such an agent is busy however quiet it is.
	BackgroundTask string
	// Ping is the latest failed or answered ping; nil when none or not wanted.
	Ping *Ping
}

// Classify returns the agent's state and, for anything but a plain alive/idle, why.
//
//   - dead: the session is gone, or the supervisor marked the agent dead
//   - rate_limited: status rate_limited
//   - blocked: status waiting (permission/trust prompt) or waiting_on_user
//   - busy: waiting on a background task or monitor, however quiet (a Claude Code
//     agent whose turn ended while a Monitor runs is not idle)
//   - idle: nothing owed and nothing unread
//   - gated / stale: owes work and silent for StaleAfter (or never seen): gated when
//     slai is deliberately not waking it, else stale
//   - busy: working and active; alive: owes work and active
//
// A failed ping then turns anything but dead/rate_limited/blocked/gated/background-busy
// into dead, unless the agent is mid-turn or its hooks fired recently (busy).
func Classify(in Inputs) (Liveness, string) {
	last := "no activity recorded"
	if in.HasActivity {
		last = "silent " + in.Silent.Round(time.Second).String()
	}
	switch {
	case !in.SessionAlive:
		r := "tmux session is gone"
		if in.HasActivity {
			r += fmt.Sprintf(" (last activity %s ago)", in.Silent.Round(time.Second))
		}
		return Dead, r
	case in.Status == "dead":
		return Dead, "agent marked dead by supervisor"
	case in.Status == "rate_limited":
		return RateLimited, "rate limited; waiting for the limit to reset"
	case in.Status == "waiting":
		return Blocked, "permission/trust prompt"
	case in.Status == "waiting_on_user":
		return Blocked, "waiting on user"
	case in.BackgroundTask != "":
		return Busy, "waiting on background task (" + in.BackgroundTask + ")"
	}

	owes, why := in.Status == "working", "mid-turn"
	if !owes && in.Unread > 0 {
		owes, why = true, fmt.Sprintf("%d unread", in.Unread)
	}
	if !owes && in.Owed != "" {
		owes, why = true, in.Owed
	}
	var l Liveness
	var reason string
	switch {
	case !owes:
		l = Idle
	case !in.HasActivity || in.Silent > StaleAfter:
		if in.GateReason != "" {
			l, reason = Gated, in.GateReason
			break
		}
		l, reason = Stale, fmt.Sprintf("%s, %s", why, last)
	case in.Status == "working":
		l = Busy
		if in.Silent > BusyQuiet {
			reason = "working, " + last
		}
	default:
		l = Alive
	}

	if p := in.Ping; p != nil && p.Result != "alive" && p.Newer && l != Gated {
		if p.Result == "no_response" && (in.Status == "working" || p.HooksRecent) {
			return Busy, "ping unanswered but the agent is active"
		}
		reason = "last ping: " + p.Result
		if strings.TrimSpace(p.Note) != "" {
			reason += " (" + p.Note + ")"
		}
		return Dead, reason
	}
	return l, reason
}
