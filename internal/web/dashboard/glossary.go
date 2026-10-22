package dashboard

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/health"
)

// Glossary explains the words the dashboard uses for an agent's state, in one line each.
// It is the single source for the tooltips and for the text under each Needs-attention row.
// Every health.Liveness value has an entry (TestEveryLivenessHasAGlossaryEntry), plus the
// everyday words a newcomer asks about.
var Glossary = map[string]string{
	"alive":        "Working on something it owes, and active just now.",
	"busy":         "In the middle of a turn (or waiting on a background task) and active.",
	"idle":         "Session is up and nothing is owed to it. This is healthy, however long it has been quiet.",
	"stuck":        "Slai's self-heal: the agent was working but nothing happened for 3 minutes and its screen did not change, so slai nudges it, then restarts it.",
	"stale":        "A state, not an alarm: the agent owes work (or has unread mail) but has been silent for 10 minutes or more. Slai will nudge it.",
	"stalled":      "An alarm: slai has already reminded the agent several times about its open work and it still made no progress, so it is telling you.",
	"orphaned":     "A slai team whose supervisor is gone but whose terminal sessions are still running, usually left over from an earlier run. `slai down --orphans` stops only those leftovers (a process it stops could be an editor or a git command of that old team: its unsaved buffers are lost; files on disk are untouched).",
	"n/a":          "No data, not zero: the harness has not reported this figure yet (or never does), so slai shows n/a and says why instead of a made-up number.",
	"hold":         "Put the whole team on hold: nothing is woken, nudged or restarted until you resume. Sessions stay up, and the hold survives a restart of slai.",
	"pause":        "Pause wake-ups: slai stops delivering wake-ups and reminders until you resume; unlike a hold it carries no name and is ended by any resume.",
	"dead":         "Its terminal session is gone. Slai revives agents on its own; if that fails it tells you here.",
	"rate_limited": "The AI provider's usage cap was hit. Slai waits for the reset time and then wakes the agent.",
	"rate limit":   "The AI provider's usage cap was hit. Slai waits for the reset time and then wakes the agent.",
	"blocked":      "Parked on a permission prompt or waiting for you. Only a person can unblock it.",
	"gated":        "Has work waiting, but slai is deliberately not waking it (paused, held, spend cap, or no free slot). Nothing for you to do.",
	"waiting":      "Waiting on a dependency: other tasks must merge first. No action needed.",
}

// gloss returns the explanation for a state word, "" when there is none.
func gloss(word string) string {
	return Glossary[strings.ToLower(strings.ReplaceAll(word, "-", "_"))]
}

// glossaryWords are the words the page promises to explain (AC 6), checked by a test.
var glossaryWords = []string{"stuck", "stale", "stalled", "dead", "rate limit", "gated", "blocked", "orphaned", "n/a", "hold", "pause"}

func init() {
	for _, l := range health.All {
		if Glossary[string(l)] == "" {
			panic(fmt.Sprintf("dashboard: no glossary entry for liveness %q", l))
		}
	}
}
