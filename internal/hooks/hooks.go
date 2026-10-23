// Package hooks maps Claude Code hook events to slai agent state (PRD 3.3).
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/usage"
)

// Agent statuses written by hooks.
const (
	StatusWorking       = "working"
	StatusWaiting       = "waiting"
	StatusIdle          = "idle"
	StatusFinished      = "finished"
	StatusWaitingOnUser = "waiting_on_user"
	StatusRateLimited   = "rate_limited" // pane shows a usage/rate limit; set by the supervisor, cleared by the next real activity
)

// Payload is the subset of Claude Code's hook JSON slai uses.
type Payload struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	Message              string          `json:"message"`
	NotificationType     string          `json:"notification_type"`
	StopHookActive       bool            `json:"stop_hook_active"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Source               string          `json:"source"`

	// Neutral payload fields (see neutral.go); absent in Claude's JSON.
	Event string        `json:"event"`
	Tool  string        `json:"tool"`
	Usage *NeutralUsage `json:"usage"`

	fromNeutral bool // set by Adapt: tool name and input were normalized to Claude's
}

// Options tunes Handle.
type Options struct {
	MaxTurnsPerTask int             // <=0 disables the guard
	Prices          usage.Overrides // user price overrides
	ProjectDir      string          // enables the PreToolUse lane guard when set
}

// Guard describes a tripped max-turns guard.
type Guard struct {
	Agent, Task string
	Turns       int
}

// Result reports what Handle did.
type Result struct {
	Status  string // new status ("" if unchanged)
	Guard   *Guard
	Usage   usage.Delta
	UsageOK bool
	// Decision is non-nil when a PreToolUse call was denied (lane guard).
	Decision *Decision
}

// Handle applies one hook event. event is case-insensitive.
func Handle(st *store.Store, agentID, event string, p Payload, opt Options) (Result, error) {
	var res Result
	ev := strings.ToLower(event)
	ag, err := st.AgentByID(agentID)
	if err != nil {
		return res, err
	}
	var errs []error

	next := nextStatus(ev, ag.Status, p)
	if next != "" && next != ag.Status {
		if err := st.SetAgentStatus(agentID, next); err != nil {
			errs = append(errs, err)
		} else {
			res.Status = next
		}
	} else if next != "" {
		// refresh status_at so the supervisor sees fresh hook data
		errs = append(errs, st.SetAgentStatus(agentID, next))
	}

	data, _ := json.Marshal(map[string]any{
		"session": p.SessionID, "tool": p.ToolName, "type": p.NotificationType, "status": next,
	})
	errs = append(errs, st.AddEvent(agentID, "hook:"+ev, string(data)))

	stopRecorded := false
	switch ev {
	case "sessionstart":
		errs = append(errs, noteSessionStart(st, agentID, p.Source))
	case "pretooluse":
		if strings.HasPrefix(p.ToolName, "mcp__slai__") {
			// The service stamps lastseen.<agent> when the call arrives; the
			// supervisor flags this agent's MCP connection if no stamp follows.
			errs = append(errs, st.SetKV(KVMCPPending+agentID, strconv.FormatInt(time.Now().Unix(), 10)))
		}
		if opt.ProjectDir != "" {
			l := LaneFor(ag.Role, agentID, ag.Worktree, opt.ProjectDir, p.Cwd)
			d := JudgeTool(l, p)
			if d != nil {
				res.Decision = d
				data, _ := json.Marshal(map[string]any{"agent": agentID, "tool": p.ToolName, "reason": d.Reason})
				errs = append(errs, st.AddEvent(agentID, "guard:lane", string(data)))
				// the same denial under the name the TUI log and doctor show
				ld, _ := json.Marshal(map[string]any{"agent": agentID, "tool": p.ToolName, "path": toolTarget(p.ToolInput), "reason": d.Reason})
				errs = append(errs, st.AddEvent(agentID, "lane_denied", string(ld)))
			}
		}
	case "userpromptsubmit":
		n, err := st.BumpTurns(agentID)
		if err != nil {
			errs = append(errs, err)
		} else if opt.MaxTurnsPerTask > 0 && ag.CurrentTask != "" && n == opt.MaxTurnsPerTask+1 {
			// fire once, on first crossing
			g := &Guard{Agent: agentID, Task: ag.CurrentTask, Turns: n}
			res.Guard = g
			d, _ := json.Marshal(map[string]any{"agent": g.Agent, "task": g.Task, "turns": g.Turns})
			errs = append(errs, st.AddEvent(agentID, "guard:max_turns", string(d)))
			_, err := st.AddMessage(store.Message{From: "slai", To: agentID, Kind: "note", TaskID: g.Task,
				Body: fmt.Sprintf("Turn limit reached: %d turns on task %s (max %d). Stop and call report(blocked) explaining where you are stuck.", n, g.Task, opt.MaxTurnsPerTask)})
			errs = append(errs, err)
		}
	case "stop":
		recorded := true
		if p.TranscriptPath != "" {
			d, err := usage.Record(st, agentID, p.TranscriptPath, opt.Prices)
			errs = append(errs, err)
			if err == nil {
				res.Usage, res.UsageOK = d, true
			}
			recorded = err == nil
		}
		stopRecorded = recorded
	}
	if u := p.Usage; u != nil && (u.Prompt > 0 || u.Output > 0 || u.Cost > 0) {
		if err := st.AddUsage(agentID, max(u.Prompt, 0), max(u.Output, 0), max(u.Cost, 0)); err != nil {
			errs = append(errs, err)
		} else {
			res.Usage, res.UsageOK = usage.Delta{Prompt: max(u.Prompt, 0), Output: max(u.Output, 0), CostUSD: max(u.Cost, 0)}, true
		}
	}
	// The review charge (see store.KVReviewEndCharge) ends only once this turn's usage is
	// written; if recording failed the supervisor's idle fallback ends it.
	if stopRecorded {
		st.FinishReviewCharge(agentID)
	}
	return res, errors.Join(errs...)
}

// JudgeTool is the whole PreToolUse lane decision for an adapted payload (see Adapt):
// Claude's lane check, plus the fail-closed neutral guard for a plugin or extension.
// It reads and writes no state, so `slai doctor --lane` can probe it offline.
func JudgeTool(l Lane, p Payload) *Decision {
	d := CheckLane(l, p.ToolName, p.ToolInput)
	if d == nil && p.fromNeutral {
		d = neutralToolGuard(l, p)
	}
	return d
}

// nextStatus returns the status to set, or "" for no change.
func nextStatus(ev, cur string, p Payload) string {
	switch ev {
	case "userpromptsubmit":
		return StatusWorking // the user answering clears waiting_on_user
	}
	if cur == StatusWaitingOnUser {
		return "" // only a user prompt clears it
	}
	switch ev {
	case "pretooluse", "posttooluse":
		return StatusWorking
	case "notification":
		switch p.NotificationType {
		case "permission_prompt", "agent_needs_input", "elicitation_dialog":
			return StatusWaiting
		case "idle_prompt":
			if cur == StatusFinished {
				return "" // TUI clears finished on view
			}
			return StatusIdle
		case "":
			m := strings.ToLower(p.Message)
			switch {
			case strings.Contains(m, "permission") || strings.Contains(m, "needs your"):
				return StatusWaiting
			case strings.Contains(m, "waiting for your input"):
				if cur == StatusFinished {
					return ""
				}
				return StatusIdle
			}
		}
		return ""
	case "stop":
		if cur == StatusWorking || cur == StatusFinished {
			return StatusFinished
		}
		return StatusIdle
	case "sessionstart":
		return StatusIdle
	}
	return ""
}

// KV keys shared with the supervisor.
const (
	KVMCPPending = "mcp.pending." // + agent: unix time of its latest mcp__slai__ PreToolUse
	KVRevived    = "revived."     // + agent: unix time slai auto-revived it
	KVHookErr    = "hook.err."    // + agent: ErrRecord JSON of its hook failures
	KVMCPErr     = "mcp.err."     // + agent: ErrRecord JSON of its slai MCP failures
)

// ErrRecord is the JSON stored under hook.err.<agent> and mcp.err.<agent>; `slai
// doctor` and the supervisor read it. Count is cumulative, At the latest failure.
type ErrRecord struct {
	Count int    `json:"count"`
	Last  string `json:"last"`
	At    int64  `json:"at"`
}

// RecordErr bumps the record under key with a new failure.
func RecordErr(st *store.Store, key, msg string, at time.Time) error {
	r, _ := LoadErr(st, key)
	r.Count++
	r.Last, r.At = msg, at.Unix()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return st.SetKV(key, string(b))
}

// LoadErr reads the record under key (zero value and false if none or unparseable).
func LoadErr(st *store.Store, key string) (ErrRecord, bool) {
	v, ok, _ := st.GetKV(key)
	var r ErrRecord
	if !ok || json.Unmarshal([]byte(v), &r) != nil {
		return ErrRecord{}, false
	}
	return r, true
}

// reviveFreshWindow is how long after an auto-revive its SessionStart still
// counts as the revived session starting.
const reviveFreshWindow = 5 * time.Minute

// noteSessionStart checks a revived agent's SessionStart: Claude Code reports
// source "resume" when the session was resumed and "startup" when it began blank,
// which means the previous conversation was lost.
func noteSessionStart(st *store.Store, agent, source string) error {
	v, ok, err := st.GetKV(KVRevived + agent)
	if err != nil || !ok {
		return err
	}
	at, _ := strconv.ParseInt(v, 10, 64)
	if time.Since(time.Unix(at, 0)) > reviveFreshWindow {
		return st.DeleteKV(KVRevived + agent)
	}
	if source == "" {
		return nil // an old Claude Code that does not report the source: say nothing
	}
	_ = st.DeleteKV(KVRevived + agent)
	if source == "resume" || source == "compact" {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"source": source})
	errs := []error{st.AddEvent(agent, "revive_fresh", string(data))}
	ags, _ := st.GetAgents()
	for _, a := range ags {
		if a.Role == "orchestrator" && a.ID != agent {
			_, err := st.AddMessage(store.Message{From: "slai", To: a.ID, Kind: "note",
				Body: agent + " was restarted but its session did not resume, so it lost its conversation context. It will be told to call get_inbox and get_task; re-brief it if it was mid-task."})
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// toolTarget names what a tool call touches: its file path, or the command line.
func toolTarget(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "filePath", "path", "notebook_path", "command"} {
		if v, ok := m[k].(string); ok && v != "" {
			if len(v) > 200 {
				v = v[:200] + "…"
			}
			return v
		}
	}
	return ""
}
