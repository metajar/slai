// Package sse is the live-update hub of `slai serve`: one GET /events stream per browser
// tab, fed from the events table.
//
// The stream carries notices, never HTML: {"kind":"topic","topic":"tasks","ids":["T-0012"]}.
// A page makes a region live by naming the topics it depends on and the fragment URL that
// renders it:
//
//	<div data-live="tasks task:T-0012" data-live-src="/f/board"> ... </div>
//
// and slai.js refetches data-live-src (htmx) when a notice for one of those topics arrives,
// and refetches every live region on a "resync" notice. Pages never talk to the hub
// directly. Code that has news the events table does not carry (the pane push) calls
// Hub.Publish with its own topic ("pane:eng-1").
package sse

import (
	"encoding/json"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// Topic names. A topic with an id is Topic+":"+id (task:T-0012, agent:eng-1, pane:eng-1).
const (
	TopicAgents = "agents" // anything about agents: status, hooks, health
	TopicTasks  = "tasks"  // the board: any task change
	TopicInbox  = "inbox"  // messages and ask_user questions
	TopicAlerts = "alerts" // things that need attention: stuck, spend caps, supervisor trouble, holds
	TopicSpend  = "spend"
	TopicMemory = "memory"
	TopicEvents = "events" // every event (the Log page)
	TopicStats  = "stats"  // anything that moves the numbers on the Stats page
)

// Notice is one thing the browser is told. It is small on purpose: the page decides what
// to refetch.
type Notice struct {
	Kind  string   `json:"kind"` // "topic" or "resync"
	Topic string   `json:"topic,omitempty"`
	IDs   []string `json:"ids,omitempty"`
}

func (n Notice) json() string {
	b, _ := json.Marshal(n)
	return string(b)
}

var taskKinds = setOf("create_task", "update_task", "cancel_task", "request_review", "request_changes", "approve", "report",
	"get_task", "priority", "merge_requested", "merged", "merge_error", "merge_refused", "merge_conflict", "merge_skipped",
	"revert_requested", "reverted", "revert_refused", "revert_failed", "dependent_rebased", "blocked_answered", "settled",
	"spec_addendum", "spec_rewritten", "finish_accepted_unmet")

var alertKinds = setOf("spend_warn", "spend_cap", "stuck_detected", "stuck_nudge", "stuck_failed", "stuck_cleared", "stuck_recovered",
	"stalled", "supervisor_panic", "supervisor_respawn", "supervisor_upgraded", "mcp_dead", "mcp_restart", "wake_gave_up",
	"team_hold", "team_resume", "loop_released", "orch_restart", "rate_limit_wake", "lane_denied", "guard:lane", "guard:max_turns",
	"guard_missing", "revive_fresh", "agent_stopped", "agent_added", "web_denied")

var inboxKinds = setOf("ask_user", "send_message", "ask", "reply", "ping", "pong", "unread_nudge", "digest")

// TopicsFor maps an event row to the notices it causes. Every event notifies "events"; the
// rest depends on its kind. The ids are the task or agent the event is about.
func TopicsFor(e store.Event) []Notice {
	var d struct {
		TaskID string `json:"task_id"`
		Task   string `json:"task"`
	}
	_ = json.Unmarshal([]byte(e.Data), &d)
	task := d.TaskID
	if task == "" {
		task = d.Task
	}
	out := []Notice{{Kind: "topic", Topic: TopicEvents}}
	add := func(topic string, ids ...string) {
		var keep []string
		for _, id := range ids {
			if id != "" {
				keep = append(keep, id)
			}
		}
		out = append(out, Notice{Kind: "topic", Topic: topic, IDs: keep})
	}
	switch k := e.Kind; {
	case strings.HasPrefix(k, "hook:") || k == "pretooluse" || k == "tool":
		add(TopicAgents, e.Agent)
	case strings.HasPrefix(k, "memory_"):
		add(TopicMemory)
	case taskKinds[k]:
		add(TopicStats)
		add(TopicTasks, task)
		if task != "" {
			add("task:"+task, task)
		}
		if e.Agent != "" {
			add(TopicAgents, e.Agent)
		}
	case inboxKinds[k]:
		add(TopicInbox, e.Agent)
		if task != "" {
			add("task:"+task, task)
		}
	case alertKinds[k]:
		add(TopicAlerts, e.Agent)
		if strings.HasPrefix(k, "spend_") {
			add(TopicSpend)
			add(TopicStats)
		}
		if e.Agent != "" {
			add(TopicAgents, e.Agent)
		}
	case e.Agent != "":
		add(TopicAgents, e.Agent)
	}
	return out
}

func setOf(w ...string) map[string]bool {
	m := make(map[string]bool, len(w))
	for _, x := range w {
		m[x] = true
	}
	return m
}
