package cmd

import (
	"fmt"
	"strings"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// statusWaiting says why an agent's mail (or its start) is not moving, in words that name the
// thing to do. Reason is the supervisor's wake-skip reason (supervisor.Skip*) or "trust".
type statusWaiting struct {
	Reason   string `json:"reason"`
	Since    int64  `json:"since,omitempty"`
	Messages int    `json:"messages"`
	Text     string `json:"text"`
}

// skipTrust is the waiting reason for the first-run trust prompt: Claude Code has not recorded
// trust for the folder, so slai started only the orchestrator and holds the rest back.
const skipTrust = "trust"

// waitingFor reports why agent id is not getting what is waiting for it, or nil when nothing
// is held up. The trust prompt comes first: it is the likeliest reason a fresh project "hangs".
func waitingFor(st *store.Store, id, orch string, live bool) *statusWaiting {
	if v, _, _ := st.GetKV(supervisor.KVPendingStart); v != "" {
		if id == orch && live {
			return &statusWaiting{Reason: skipTrust, Text: fmt.Sprintf("%s is waiting for you to trust this folder: `slai attach %s`, choose \"Yes, I trust this folder\" (the other agents start once you do)", id, orch)}
		}
		for _, p := range strings.Split(v, ",") {
			if p == id {
				return &statusWaiting{Reason: skipTrust, Text: fmt.Sprintf("%s has not started: it waits for you to trust this folder in %s's pane (`slai attach %s`)", id, orch, orch)}
			}
		}
	}
	msgs, err := st.UndeliveredFor(id)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	reason, since := supervisor.WakeSkipReason(st, id)
	if reason == "" {
		return nil
	}
	n := len(msgs)
	w := &statusWaiting{Reason: reason, Since: since, Messages: n}
	var count string
	if n == 1 {
		count = "1 message"
	} else {
		count = fmt.Sprintf("%d messages", n)
	}
	var why string
	switch reason {
	case supervisor.SkipAttached:
		why = "a tmux client is attached; detach or press enter in its pane"
	case supervisor.SkipFocus:
		why = "you are typing into it in the dashboard; it gets them when you leave its pane"
	case supervisor.SkipModal:
		why = "it is at a permission or trust prompt only you can answer: `slai attach " + id + "`"
	case supervisor.SkipHeld:
		why = "slai is holding its wake-ups (paused, team hold, spend cap, loop guard or too many unanswered wakes): `slai resume " + id + "` lifts a pause, `slai doctor` says which"
	case supervisor.SkipSlot:
		why = "it waits for a free slot (max_concurrent_agents is full)"
	case supervisor.SkipPrepFailed:
		why = "its worktree could not switch to the assignment's branch; slai retries every tick (`slai doctor` shows the error)"
	case supervisor.SkipBusy:
		why = "it is mid-turn; they are delivered when the turn ends"
	default:
		why = "waiting (" + reason + ")"
	}
	w.Text = fmt.Sprintf("%s: %s waiting, %s", id, count, why)
	return w
}

// doctorWaiting prints a warning for every agent whose mail or start is held up for a reason
// someone has to act on (an attached terminal, a prompt, a hold, a failed worktree switch, the
// first-run trust prompt). Waiting for a slot, for you to leave its pane or for a turn to end
// is normal and only shows in `slai status`. Warnings do not count as failures.
func doctorWaiting(w interface{ Write([]byte) (int, error) }, st *store.Store, team []string, orch string, live func(id string) bool) {
	for _, id := range team {
		wf := waitingFor(st, id, orch, live(id))
		if wf == nil {
			continue
		}
		switch wf.Reason {
		case supervisor.SkipBusy, supervisor.SkipSlot, supervisor.SkipFocus:
			continue
		}
		fmt.Fprintf(w, "! warn: %s\n", wf.Text)
	}
}
