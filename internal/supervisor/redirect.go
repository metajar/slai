package supervisor

import (
	"context"
	"strconv"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// CoursePrefix starts every course-correction message, so the agent (and the
// detail history) can tell it from ordinary traffic.
const CoursePrefix = "Course correction from user: "

// KVRedirectPrefix + agent id holds the id of a course-correction message whose
// turn still has to be interrupted. The TUI only writes the request; the
// supervisor is the one that touches tmux.
const KVRedirectPrefix = "redirect."

// RequestRedirect queues a course correction: a kind=note message from the user,
// and a request for the supervisor to interrupt the agent's current turn (Escape,
// through the same safeToType check as every other keystroke) so the normal wake
// delivers the message right away. It returns the message id.
func RequestRedirect(st *store.Store, a store.Agent, text string) (int64, error) {
	id, err := st.AddMessage(store.Message{From: "user", To: a.ID, Kind: "note", TaskID: a.CurrentTask,
		Body: CoursePrefix + strings.TrimSpace(text)})
	if err != nil {
		return 0, err
	}
	return id, st.SetKV(KVRedirectPrefix+a.ID, strconv.FormatInt(id, 10))
}

// redirectTick consumes pending interrupt requests, each exactly once. A request
// that cannot be honoured (agent dead, team paused, someone attached, a permission
// dialog open) is dropped with an event; the message itself stays queued and is
// delivered by the ordinary wake when the agent is next free.
func (s *Supervisor) redirectTick(ctx context.Context) {
	agents, err := s.Store.GetAgents()
	if err != nil {
		return
	}
	for _, a := range agents {
		v := s.kvGet(KVRedirectPrefix + a.ID)
		if v == "" {
			continue
		}
		_ = s.Store.DeleteKV(KVRedirectPrefix + a.ID)
		skip := func(why string) { s.event(a.ID, "redirect_skipped", map[string]any{"message": v, "reason": why}) }
		switch {
		case s.Paused():
			skip("team paused")
		case !s.sessionLive(ctx, a):
			skip("agent not running")
		case s.attached(ctx, a):
			skip("user attached to the pane")
		case !s.safeToType(ctx, a):
			skip("permission dialog open")
		default:
			if err := s.Tmux.SendSeq(ctx, a.TmuxSession, s.harnessOf(a.ID).Keys().InterruptSeq); err != nil {
				skip(err.Error())
				continue
			}
			// The interrupted turn fires no Stop hook; the agent is at its prompt now.
			_ = s.Store.SetAgentStatus(a.ID, "idle")
			s.event(a.ID, "redirect", map[string]any{"message": v})
		}
	}
}

// attached reports whether a user is at the agent's pane (focused in the TUI or a client
// attached); a failing tmux listing is not "attached" (see userPresent).
func (s *Supervisor) attached(ctx context.Context, a store.Agent) bool {
	return s.userPresent(ctx, a)
}
