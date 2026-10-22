package agents

// CLIOnly is an action v1 deliberately leaves to the TUI or CLI. The agent page shows each
// as a disabled control that names where to do it, from this one table. A test checks the
// TUI keys against the TUI's own help text.
type CLIOnly struct {
	Name string // the control's label
	Why  string // one line on what it does
	Key  string // the TUI key ("" when it has none)
	Cmd  string // the command to use instead ("" when it has none)
}

// CLIOnlyActions are the agent actions that are not offered in the web UI.
var CLIOnlyActions = []CLIOnly{
	{Name: "Revive", Why: "bring a dead agent back, resuming its conversation", Key: "v", Cmd: "slai up"},
	{Name: "Restart fresh", Why: "restart the agent with a new conversation (asks first)", Key: "R"},
	{Name: "Reassign tasks", Why: "move a task to another agent", Cmd: "ask the orchestrator (update_task assignee)"},
}
