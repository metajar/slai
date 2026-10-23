package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oorbt/slai/internal/health"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// Liveness: every tool call stamps the caller's last_seen (KV "lastseen.<agent>").
// ping(agent) sends a "ping" message and waits for the agent to answer with
// pong(message_id). Who answered comes from the MCP connection (slai mcp --agent), never
// from the agent's shell, so a pong needs no environment. An old prompt may still pass
// agent_id (the shell's $SLAI_AGENT_ID); it is ignored unless it names someone else, which
// shows up as a mismatch, not silence.

const (
	PingDefaultTimeout = 60 * time.Second
	PingMaxTimeout     = 120 * time.Second
)

type PingArgs struct {
	Agent          string `json:"agent" jsonschema:"agent id to check"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for the ack (default 60, max 120)"`
}
type PongArgs struct {
	MessageID int64  `json:"message_id" jsonschema:"id of the ping message you are answering"`
	AgentID   string `json:"agent_id,omitempty" jsonschema:"deprecated and not needed: slai knows who you are from your connection"`
}

type pongRec struct {
	AnsweredBy string `json:"answered_by"` // identity the MCP server runs as
	Reported   string `json:"reported"`    // agent_id the agent volunteered (old prompts only), "" if none
	At         int64  `json:"at"`
}

// pingRec is the latest ping result for an agent (KV "ping.<agent>").
type pingRec struct {
	Result string `json:"result"` // alive | identity_mismatch | no_response
	At     int64  `json:"at"`
	Note   string `json:"note,omitempty"`
}

func lastSeenKey(agent string) string { return "lastseen." + agent }
func pingKey(agent string) string     { return "ping." + agent }
func pongKey(id int64) string         { return "pong." + strconv.FormatInt(id, 10) }

// touch records that agent just made a tool call. Best effort.
func (s *Service) touch(agent string) {
	_ = s.Store.SetKV(lastSeenKey(agent), strconv.FormatInt(time.Now().Unix(), 10))
}

// Touch stamps agent's last_seen. The MCP server calls it for every tools/call
// request, before argument validation, so a call that reached the server counts
// even if it is rejected (the supervisor's MCP-crash check relies on this).
func (s *Service) Touch(agent string) { s.touch(agent) }

func (s *Service) lastSeen(agent string) int64 {
	v, ok, _ := s.Store.GetKV(lastSeenKey(agent))
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (s *Service) lastPing(agent string) (pingRec, bool) {
	v, ok, _ := s.Store.GetKV(pingKey(agent))
	var r pingRec
	if !ok || json.Unmarshal([]byte(v), &r) != nil {
		return r, false
	}
	return r, true
}

func (s *Service) Pong(ctx context.Context, caller string, a PongArgs) (any, error) {
	if err := s.begin(caller, "pong", a); err != nil {
		return nil, err
	}
	m, err := s.Store.GetMessage(a.MessageID)
	if err != nil || m.Kind != "ping" {
		return nil, fmt.Errorf("message %d is not a ping", a.MessageID)
	}
	b, _ := json.Marshal(pongRec{AnsweredBy: caller, Reported: a.AgentID, At: time.Now().Unix()})
	if err := s.Store.SetKV(pongKey(a.MessageID), string(b)); err != nil {
		return nil, err
	}
	return map[string]any{"ack": true}, nil
}

func (s *Service) Ping(ctx context.Context, caller string, a PingArgs) (any, error) {
	if err := s.begin(caller, "ping", a); err != nil {
		return nil, err
	}
	if !s.agentExists(a.Agent) || a.Agent == caller {
		return nil, fmt.Errorf("unknown agent %q", a.Agent)
	}
	timeout := PingDefaultTimeout
	if a.TimeoutSeconds > 0 {
		timeout = min(time.Duration(a.TimeoutSeconds)*time.Second, PingMaxTimeout)
	}
	body := "slai liveness check from " + caller + ". Do nothing else: call the `pong` tool with this message's id as message_id."
	id, err := s.Store.AddMessage(store.Message{From: caller, To: a.Agent, Kind: "ping", Body: body})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var pong pongRec
	got := false
	for {
		if v, ok, _ := s.Store.GetKV(pongKey(id)); ok && json.Unmarshal([]byte(v), &pong) == nil {
			got = true
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("ping cancelled")
		case <-time.After(pingPoll):
		}
	}
	rec := pingRec{At: time.Now().Unix()}
	out := map[string]any{"agent": a.Agent, "assigned_id": a.Agent, "message_id": id}
	switch {
	case !got:
		rec.Result = "no_response"
		out["result"] = "no_response"
		out["detail"] = fmt.Sprintf("no ack within %s. An agent that is mid-turn may not see the ping before the timeout, so this means \"did not answer\", not \"gone\": retry with a longer timeout_seconds or check get_board liveness / last_seen", timeout)
	case pong.AnsweredBy != a.Agent || (pong.Reported != "" && pong.Reported != a.Agent):
		rec.Result = "identity_mismatch"
		rec.Note = fmt.Sprintf("assigned %s, MCP answered as %s, shell reported %s", a.Agent, pong.AnsweredBy, pong.Reported)
		out["result"] = "identity_mismatch"
		out["answered_by"], out["reported_id"], out["detail"] = pong.AnsweredBy, pong.Reported, rec.Note
	default:
		rec.Result = "alive"
		out["result"] = "alive"
		out["answered_by"], out["reported_id"] = pong.AnsweredBy, pong.Reported
	}
	if b, err := json.Marshal(rec); err == nil {
		_ = s.Store.SetKV(pingKey(a.Agent), string(b))
	}
	out["last_seen"] = s.lastSeen(a.Agent)
	return out, nil
}

// pingPoll is how often Ping checks for the ack (a var so tests can speed it up).
var pingPoll = 250 * time.Millisecond

type agentView struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	Liveness     string `json:"liveness"` // alive | idle | busy | blocked | gated | stale | dead | rate_limited
	Reason       string `json:"reason,omitempty"`
	LastSeen     int64  `json:"last_seen,omitempty"`
	LastActivity int64  `json:"last_activity,omitempty"` // latest tool call, hook event or status change
	Status       string `json:"status,omitempty"`
	CurrentTask  string `json:"current_task,omitempty"`
	// StaleBuild: the agent's slai MCP server still runs code older than the installed binary.
	StaleBuild bool `json:"stale_build,omitempty"`
	// Harness contract (spec harnesses.md AC 15/16): which agent CLI runs this agent,
	// what it lacks, and whether the subscription gauges mean anything for it.
	Harness      string   `json:"harness,omitempty"`
	Degraded     []string `json:"degraded,omitempty"`      // capabilities this harness lacks, e.g. statusline, lane_guard
	Gauges       string   `json:"gauges,omitempty"`        // "ok", or "n/a" when 5h/weekly gauges are not available (render n/a, never 0)
	GaugesReason string   `json:"gauges_reason,omitempty"` // why they are n/a
	// Wake gates: why the supervisor is not waking this agent.
	Paused        bool  `json:"paused"`
	HeldUntil     int64 `json:"held_until"` // unix, 0 = no loop-guard hold
	QueuedForSlot bool  `json:"queued_for_slot"`
}

func (s *Service) agentWorking(id string) bool {
	ags, err := s.Store.GetAgents()
	if err != nil {
		return false
	}
	for _, a := range ags {
		if a.ID == id {
			return a.Status == "working"
		}
	}
	return false
}

// liveness is the board view of id: a failed ping that no later tool call has
// contradicted is dead (or busy, if the agent was mid-turn); otherwise
// AgentHealth decides. The supervisor marks agents whose tmux session is gone
// as status "dead", which AgentHealth treats as a dead session.
func (s *Service) liveness(id string, now time.Time) agentView {
	cfg := GateConfigOf(s.team())
	return s.livenessWith(id, now, NewGateSource(s.Store, cfg, now))
}

// livenessWith is liveness with the gate inputs already read (one board, one read).
func (s *Service) livenessWith(id string, now time.Time, gates *GateSource) agentView {
	cfg := GateConfigOf(s.team())
	kind := ""
	for _, a := range s.team().Agents {
		if a.ID == id {
			kind = a.HarnessKind()
		}
	}
	// A failed ping only counts if nothing (tool call, hook event, status change)
	// has happened since: a live session with recent hooks is never dead. The rule
	// itself lives in health.Classify.
	h := agentHealth(s.Store, id, true, now, InputOpts{Cfg: cfg, Gates: gates, Harness: kind,
		Ping: func(last int64) *health.Ping {
			r, ok := s.lastPing(id)
			if !ok {
				return nil
			}
			return &health.Ping{Result: r.Result, Note: r.Note, Newer: r.At >= last, HooksRecent: hooksRecent(s.Store, id, now)}
		}})
	v := agentView{ID: id, LastSeen: s.lastSeen(id), Liveness: h.Liveness, Reason: h.Reason, LastActivity: h.LastActivity}
	if a, err := s.Store.AgentByID(id); err == nil {
		v.Status = a.Status
		v.CurrentTask, _ = s.Store.WorkingTask(id) // what it is on, not its latest assignment
	}
	g := gates.Agent(id)
	v.Paused, v.HeldUntil, v.QueuedForSlot = g.Paused, g.HeldUntil, g.QueuedForSlot
	return v
}

// PingBusyWindow is how recent a hook event must be for an unanswered ping to
// read as busy rather than dead.
const PingBusyWindow = 3 * time.Minute

func hooksRecent(st *store.Store, id string, now time.Time) bool {
	at, err := st.LastHookEventAt(id)
	return err == nil && at != 0 && now.Sub(time.Unix(at, 0)) < PingBusyWindow
}

func lastSeenAt(st *store.Store, agent string) int64 {
	v, ok, _ := st.GetKV(lastSeenKey(agent))
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}
