package supervisor

import (
	"context"
	"fmt"
	"strings"
)

// Why did it restart? Every time the supervisor kills or restarts an agent's session
// on its own initiative, the event it records carries a non-empty, human-readable
// `cause` ("no hook activity 3m12s; pane unchanged"). killSession is the only place
// that kills an agent's session, and restartEvent the only way to record one, so
// the rule is enforced in two places instead of at every call site
// (TestEveryRestartRecordsACause).

// RestartKinds are the events that record a supervisor-initiated kill or restart; each
// carries a `cause`.
var RestartKinds = []string{
	"stuck_restart",   // stuck turn: nudge did not help; unread mail; MCP dead or crashed
	"orch_restart",    // the orchestrator was down or silent
	"agent_upgraded",  // rolling restart after a slai upgrade
	"agent_restarted", // team.yaml changed the agent's launch config
	"agent_removed",   // team.yaml no longer lists the agent
	"agent_stopped",   // stopped on request
	"auto_revive",     // a dead session was started again
}

// unspecified is what a missing cause is replaced with, loudly, so a call site that
// forgets one shows up in the event log instead of in a silent restart.
const unspecified = "unspecified (slai bug: a restart was recorded without a cause)"

func cleanCause(cause string) string {
	if cause = strings.TrimSpace(cause); cause == "" {
		return unspecified
	}
	return cause
}

// killSession kills an agent's tmux session. It is the supervisor's single choke point
// for that: every caller says why.
func (s *Supervisor) killSession(ctx context.Context, agentID, session, cause string) error {
	cause = cleanCause(cause)
	s.logf("killing %s (%s): %s", agentID, session, cause)
	return s.Tmux.KillSession(ctx, session)
}

// restartEvent records a restart-class event with its cause.
func (s *Supervisor) restartEvent(agentID, kind, cause string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["cause"] = cleanCause(cause)
	s.event(agentID, kind, data)
}

func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}
