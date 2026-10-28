package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// Mid-turn mail notice (P-1). Mail for a busy agent used to wait for its Stop hook: the
// supervisor only types a wake-up line into an idle pane, so an engineer deep in a long turn
// heard nothing for 5 to 10 minutes. Claude Code lets a PostToolUse hook add `additionalContext`
// to the turn in progress (verified on 2.1.309, docs/reviews/comms-reliability-review.md "P-1
// result"), so after each tool call this hook tells the agent about mail that arrived meanwhile.
//
// It only ever tells the agent about mail; it does not mark anything delivered (get_inbox does
// that), starts no turn and types nothing. Rules:
//   - each message is announced at most once (kv postnotice.<agent> = highest id announced);
//   - only mail the wake loop has not announced itself (woken_at == 0): a line already typed
//     into the pane is queued behind the turn and needs no second notice;
//   - never right after the agent's own get_inbox call;
//   - never for an agent slai is deliberately not waking (paused, team hold, loop-guard hold,
//     or the wake loop's own "held" reason, which covers spend caps), so a hold is never
//     bypassed by a busy turn;
//   - after a context gap the notice opens with the identity sentence, like a wake-up line.
const kvPostNotice = "postnotice." // + agent: highest message id already announced mid-turn

// postToolNotice returns the PostToolUse hook output for agent, or nil when there is nothing
// to say. now is only used for the loop-guard hold.
func postToolNotice(st *store.Store, team *config.Team, agent, tool string, now time.Time) []byte {
	if tool == "mcp__slai__get_inbox" {
		return nil
	}
	if heldFromWaking(st, agent, now) {
		return nil
	}
	msgs, err := st.ClaimableFor(agent)
	if err != nil {
		return nil
	}
	last := int64(0)
	if v, ok, _ := st.GetKV(kvPostNotice + agent); ok {
		last, _ = strconv.ParseInt(v, 10, 64)
	}
	var fresh []store.Message
	top := last
	for _, m := range msgs {
		if m.ID > top {
			top = m.ID
		}
		if m.ID > last && m.WokenAt == 0 {
			fresh = append(fresh, m)
		}
	}
	if top > last {
		_ = st.SetKV(kvPostNotice+agent, strconv.FormatInt(top, 10))
	}
	if len(fresh) == 0 {
		return nil
	}
	line := fmt.Sprintf("[slai] You have %d new items. Call get_inbox.", len(fresh))
	if _, pending, _ := st.GetKV(hooks.KVIdentityPending + agent); pending && team != nil {
		if ident := roles.IdentityLineFor(team, agent); ident != "" {
			line = ident + " " + line
			_ = st.DeleteKV(hooks.KVIdentityPending + agent)
		}
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PostToolUse", "additionalContext": line,
	}})
	return out
}

// heldFromWaking reports whether slai is deliberately not waking agent right now.
func heldFromWaking(st *store.Store, agent string, now time.Time) bool {
	if v, ok, _ := st.GetKV(supervisor.KVPaused); ok && v == "1" {
		return true
	}
	if _, ok, _ := st.GetKV(supervisor.KVTeamHold); ok {
		return true
	}
	if paused, _ := st.PausedAgents(); paused[agent] {
		return true
	}
	if v, ok, _ := st.GetKV(supervisor.KVHoldPrefix + agent); ok {
		if until, err := strconv.ParseInt(v, 10, 64); err == nil && until > now.Unix() {
			return true
		}
	}
	if v, ok, _ := st.GetKV("wake.skip." + agent); ok && strings.HasPrefix(v, supervisor.SkipHeld+"|") {
		return true
	}
	return false
}
