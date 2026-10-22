package health

import (
	"strings"
	"testing"
	"time"
)

func in(mut func(*Inputs)) Inputs {
	i := Inputs{SessionAlive: true, Status: "idle", HasActivity: true, Silent: 30 * time.Second}
	if mut != nil {
		mut(&i)
	}
	return i
}

// One row per behaviour; TestEveryLivenessHasATableRow fails when a Liveness value
// has no row, so adding a value forces its rule to be written down here.
var table = []struct {
	name   string
	in     Inputs
	want   Liveness
	reason string // substring; "" = must be empty
}{
	{"session gone", in(func(i *Inputs) { i.SessionAlive = false; i.Silent = 4 * time.Minute }), Dead, "tmux session is gone (last activity 4m0s ago)"},
	{"session gone, never seen", in(func(i *Inputs) { i.SessionAlive = false; i.HasActivity = false }), Dead, "tmux session is gone"},
	{"marked dead by the supervisor", in(func(i *Inputs) { i.Status = "dead" }), Dead, "marked dead by supervisor"},
	{"rate limited", in(func(i *Inputs) { i.Status = "rate_limited"; i.Owed = "owes T-1" }), RateLimited, "waiting for the limit to reset"},
	{"permission prompt", in(func(i *Inputs) { i.Status = "waiting" }), Blocked, "permission/trust prompt"},
	{"waiting on the user", in(func(i *Inputs) { i.Status = "waiting_on_user" }), Blocked, "waiting on user"},
	{"owes work, quiet, wakeable: stale", in(func(i *Inputs) { i.Owed = "owes T-3"; i.Silent = 11 * time.Minute }), Stale, "owes T-3, silent 11m0s"},
	{"unread, quiet: stale", in(func(i *Inputs) { i.Unread = 2; i.Silent = time.Hour }), Stale, "2 unread"},
	{"mid-turn, quiet for ages: stale", in(func(i *Inputs) { i.Status = "working"; i.Silent = 12 * time.Minute }), Stale, "mid-turn, silent 12m0s"},
	{"never seen and owing: stale", in(func(i *Inputs) { i.Owed = "owes T-3"; i.HasActivity = false }), Stale, "no activity recorded"},
	{"owes work, quiet, not wakeable: gated", in(func(i *Inputs) { i.Owed = "owes T-3"; i.Silent = 11 * time.Minute; i.GateReason = "paused" }), Gated, "paused"},
	{"gate does not matter while active", in(func(i *Inputs) { i.Owed = "owes T-3"; i.GateReason = "paused" }), Alive, ""},
	{"working and active: busy", in(func(i *Inputs) { i.Status = "working" }), Busy, ""},
	{"working, a little quiet: busy with a note", in(func(i *Inputs) { i.Status = "working"; i.Silent = 3 * time.Minute }), Busy, "working, silent 3m0s"},
	{"owes work and active: alive", in(func(i *Inputs) { i.Owed = "owes T-3" }), Alive, ""},
	{"unread and active: alive", in(func(i *Inputs) { i.Unread = 1 }), Alive, ""},
	{"nothing owed, however quiet: idle", in(func(i *Inputs) { i.Silent = 3 * time.Hour }), Idle, ""},
	{"finished with nothing owed: idle", in(func(i *Inputs) { i.Status = "finished" }), Idle, ""},

	// the live gap: a Monitor / background task keeps the agent working after its turn ends
	{"background task, turn ended (finished): busy, not idle", in(func(i *Inputs) {
		i.Status = "finished"
		i.Silent = 17 * time.Minute
		i.BackgroundTask = "1 monitor"
	}), Busy, "waiting on background task (1 monitor)"},
	{"background task beats stale", in(func(i *Inputs) {
		i.Status = "idle"
		i.Owed = "owes T-3"
		i.Silent = time.Hour
		i.BackgroundTask = "2 background tasks"
	}), Busy, "waiting on background task (2 background tasks)"},
	{"background task does not hide a prompt", in(func(i *Inputs) { i.Status = "waiting"; i.BackgroundTask = "1 monitor" }), Blocked, "permission/trust prompt"},
	{"background task does not hide a dead session", in(func(i *Inputs) { i.SessionAlive = false; i.BackgroundTask = "1 monitor" }), Dead, "tmux session is gone"},

	// failed pings
	{"unanswered ping, idle, no recent hooks: dead", in(func(i *Inputs) { i.Ping = &Ping{Result: "no_response", Newer: true} }), Dead, "last ping: no_response"},
	{"unanswered ping with a note", in(func(i *Inputs) { i.Ping = &Ping{Result: "identity_mismatch", Note: "answered as pm", Newer: true} }), Dead, "last ping: identity_mismatch (answered as pm)"},
	{"unanswered ping while working: busy", in(func(i *Inputs) {
		i.Status = "working"
		i.Ping = &Ping{Result: "no_response", Newer: true}
	}), Busy, "ping unanswered but the agent is active"},
	{"unanswered ping with recent hooks: busy", in(func(i *Inputs) { i.Ping = &Ping{Result: "no_response", Newer: true, HooksRecent: true} }), Busy, "ping unanswered but the agent is active"},
	{"a ping older than later activity says nothing", in(func(i *Inputs) { i.Ping = &Ping{Result: "no_response", Newer: false} }), Idle, ""},
	{"an answered ping changes nothing", in(func(i *Inputs) { i.Ping = &Ping{Result: "alive", Newer: true} }), Idle, ""},
	{"a failed ping does not override gated", in(func(i *Inputs) {
		i.Owed = "owes T-3"
		i.Silent = time.Hour
		i.GateReason = "spend capped"
		i.Ping = &Ping{Result: "no_response", Newer: true}
	}), Gated, "spend capped"},
	{"a failed ping does not override a prompt", in(func(i *Inputs) { i.Status = "waiting"; i.Ping = &Ping{Result: "no_response", Newer: true} }), Blocked, "permission/trust prompt"},
	{"a failed ping does not override a background task", in(func(i *Inputs) {
		i.BackgroundTask = "1 monitor"
		i.Ping = &Ping{Result: "no_response", Newer: true}
	}), Busy, "waiting on background task"},
}

func TestClassify(t *testing.T) {
	for _, c := range table {
		t.Run(c.name, func(t *testing.T) {
			got, why := Classify(c.in)
			if got != c.want {
				t.Fatalf("liveness = %s (%q), want %s", got, why, c.want)
			}
			if c.reason == "" && why != "" || !strings.Contains(why, c.reason) {
				t.Fatalf("reason = %q, want it to contain %q", why, c.reason)
			}
			if !got.Valid() {
				t.Fatalf("%q is not in All", got)
			}
		})
	}
}

// Removing or adding a value breaks this: every value of All must have a row above that
// produces it, and no row may produce something outside All.
func TestEveryLivenessHasATableRow(t *testing.T) {
	seen := map[Liveness]bool{}
	for _, c := range table {
		seen[c.want] = true
	}
	for _, l := range All {
		if !seen[l] {
			t.Errorf("liveness %q has no row in the Classify table", l)
		}
	}
	for l := range seen {
		if !l.Valid() {
			t.Errorf("table produces %q which is not in All", l)
		}
	}
	if len(All) != 8 {
		t.Errorf("the vocabulary is dead, rate_limited, blocked, gated, busy, alive, idle, stale; All has %d", len(All))
	}
}

func TestThresholds(t *testing.T) {
	if StaleAfter != 10*time.Minute {
		t.Fatalf("StaleAfter = %s", StaleAfter)
	}
	at := func(d time.Duration) Liveness {
		l, _ := Classify(in(func(i *Inputs) { i.Owed = "owes T-1"; i.Silent = d }))
		return l
	}
	if at(StaleAfter) != Alive || at(StaleAfter+time.Second) != Stale {
		t.Fatalf("boundary: %s / %s", at(StaleAfter), at(StaleAfter+time.Second))
	}
}
