package supervisor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// AttachedBlockingAfter is how long mail must have waited, and how long every attached
// client must have been idle, before slai tells the user their attached terminal is why an
// agent is not getting its mail.
const AttachedBlockingAfter = 3 * time.Minute

// EscAttachedBlocking: a tmux client is attached to an agent and has been idle while mail waits.
const EscAttachedBlocking = "attached_blocking"

// kvPresenceSkip + agent holds the unix time mail first waited because a user was present.
const kvPresenceSkip = "presence.skip."

// listClients reads the clients attached to a session (a var so tests can stand in a user).
var listClients = func(ctx context.Context, c *tmux.Client, session string) ([]tmux.ClientInfo, error) {
	return c.Clients(ctx, session)
}

// presence is who is at an agent's session right now.
type presence struct {
	focused bool          // the TUI is focused on the agent (the user is typing into it)
	clients int           // tmux clients attached to its session
	idle    time.Duration // how long the most recently active attached client has been idle
}

// present: someone is there, so slai must not type into the pane.
func (p presence) present() bool { return p.focused || p.clients > 0 }

// active: someone is there and not idle for AttachedBlockingAfter. A focused TUI is always
// active (focus lapses on its own when the user stops).
func (p presence) active() bool {
	return p.focused || (p.clients > 0 && p.idle < AttachedBlockingAfter)
}

// presenceOf reads the focus window and the attached clients. A tmux error means "no
// client we can see", not "attached": a failing list-clients must not silence an agent.
func (s *Supervisor) presenceOf(ctx context.Context, a store.Agent) presence {
	now := s.now()
	p := presence{focused: s.Store.Focused(a.ID, now)}
	if live, err := s.Tmux.HasSession(ctx, a.TmuxSession); err != nil || !live {
		return p
	}
	cs, err := listClients(ctx, s.Tmux, a.TmuxSession)
	if err != nil {
		return p
	}
	p.clients = len(cs)
	for i, c := range cs {
		if idle := now.Sub(c.Activity); i == 0 || idle < p.idle {
			p.idle = idle
		}
	}
	return p
}

// userPresent is the one answer to "may slai type into this agent's pane?": false when the
// agent's TUI focus is live or a client is attached to its session.
func (s *Supervisor) userPresent(ctx context.Context, a store.Agent) bool {
	return s.presenceOf(ctx, a).present()
}

// presenceWatch runs for an agent the health pass is holding off because a user is present.
// It never types and never detaches anyone; it only makes the silence visible: when mail has
// waited AttachedBlockingAfter and every attached client has been idle that long, it raises a
// user escalation naming the agent and what to do. It also marks the mail as skipped for
// presence, which oldUnread counts once the user is gone.
func (s *Supervisor) presenceWatch(ctx context.Context, a store.Agent) {
	msgs, err := s.Store.UndeliveredFor(a.ID)
	if err != nil || len(msgs) == 0 {
		_ = s.Store.DeleteKV(kvPresenceSkip + a.ID)
		return
	}
	p := s.presenceOf(ctx, a)
	if !p.present() {
		return
	}
	now := s.now()
	oldest := msgs[0].CreatedAt
	for _, m := range msgs {
		oldest = min(oldest, m.CreatedAt)
	}
	if s.kvGet(kvPresenceSkip+a.ID) == "" {
		_ = s.Store.SetKV(kvPresenceSkip+a.ID, strconv.FormatInt(now.Unix(), 10))
	}
	waited := now.Sub(time.Unix(oldest, 0))
	if waited < AttachedBlockingAfter || p.active() {
		return
	}
	text := fmt.Sprintf("a tmux client is attached to %s and %s waiting (oldest %s): slai never types over an attached terminal. Detach it (tmux: ctrl+b d) and slai delivers the mail, or tell the agent in that pane to call get_inbox.",
		a.ID, plural(len(msgs), "message"), waited.Truncate(time.Second))
	s.escalate(escalation{agent: a.ID, kind: EscAttachedBlocking, user: true, text: text})
}

// presenceCleared reports whether an attached_blocking escalation has resolved: the agent is
// gone, nobody is attached any more (the wake tick then delivers), or nothing is waiting.
func (s *Supervisor) presenceCleared(ctx context.Context, a store.Agent, ok bool) bool {
	if !ok {
		return true
	}
	if n, err := s.Store.UnreadCount(a.ID); err != nil || n == 0 {
		return true
	}
	return !s.userPresent(ctx, a)
}
