// Package config defines team.yaml: load, validate, presets.
package config

type Team struct {
	Version  int               `yaml:"version"`
	Project  Project           `yaml:"project"`
	Defaults Defaults          `yaml:"defaults"`
	Models   map[string]string `yaml:"models"`
	Agents   []Agent           `yaml:"agents"`
	Notify   Notify            `yaml:"notify,omitempty"`
	Memory   Memory            `yaml:"memory,omitempty"`
}

// Memory configures the team's shared fact memory.
type Memory struct {
	// PackTokens is the most tokens (ceil(bytes/4)) of the memory pack an agent receives
	// through get_task and get_memory. Zero means DefaultPackTokens.
	PackTokens int `yaml:"pack_tokens,omitempty"`
}

// DefaultPackTokens is memory.pack_tokens when it is left out.
const DefaultPackTokens = 1500

// PackBudget is the effective memory pack budget.
func (m Memory) PackBudget() int {
	if m.PackTokens > 0 {
		return m.PackTokens
	}
	return DefaultPackTokens
}

// Notify runs a user command on key events (README "notify").
type Notify struct {
	// Command is run with `sh -c`; the event is on stdin as JSON and in SLAI_EVENT.
	Command string `yaml:"command,omitempty"`
	// Events limits which events fire; empty means all of NotifyEvents.
	Events []string `yaml:"events,omitempty"`
	// TimeoutSeconds kills a command that hangs; zero means DefaultNotifyTimeoutSeconds.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
}

// NotifyEvents are the event names notify.events accepts.
var NotifyEvents = []string{"awaiting_user", "review_requested", "review_approved", "escalation", "spend_warn", "spend_cap", "run_finished"}

const DefaultNotifyTimeoutSeconds = 10

// DefaultTestTimeoutSeconds is how long the merge-time test_command may run (10 minutes).
const DefaultTestTimeoutSeconds = 600

// Wants reports whether event should run the command.
func (n Notify) Wants(event string) bool {
	if n.Command == "" {
		return false
	}
	if len(n.Events) == 0 {
		return true
	}
	return contains(n.Events, event)
}

type Project struct {
	Name              string `yaml:"name"`
	Description       string `yaml:"description"`
	MainBranch        string `yaml:"main_branch"`
	IntegrationBranch string `yaml:"integration_branch"`
	// TestCommand, when set, runs (sh -c) on every merge result before the integration
	// branch moves; a non-zero exit refuses the merge. Empty = no gate.
	TestCommand string `yaml:"test_command,omitempty"`
	// TestTimeoutSeconds kills a hanging TestCommand; 0 means DefaultTestTimeoutSeconds.
	TestTimeoutSeconds int `yaml:"test_timeout_seconds,omitempty"`
}

type Defaults struct {
	PermissionMode      string `yaml:"permission_mode"`
	MaxTurnsPerTask     int    `yaml:"max_turns_per_task"`
	MaxConcurrentAgents int    `yaml:"max_concurrent_agents"`
	RequireReview       bool   `yaml:"require_review"`
	// Caps are optional spend caps in USD; zero means no cap.
	Caps SpendCaps `yaml:"spend_caps,omitempty"`
	// UpgradeRestartAgents restarts agents (one at a time, when idle) onto a newly
	// installed slai binary. Unset means true.
	UpgradeRestartAgents *bool `yaml:"upgrade_restart_agents,omitempty"`
	// IdentityEveryBoardChecks re-sends the orchestrator's identity line with every Nth board
	// check slai sends it. Unset means 10; negative turns it off.
	IdentityEveryBoardChecks int `yaml:"identity_every_board_checks,omitempty"`
}

// IdentityEvery is how many board checks pass between identity lines for the orchestrator
// (0 = off).
func (d Defaults) IdentityEvery() int {
	switch {
	case d.IdentityEveryBoardChecks < 0:
		return 0
	case d.IdentityEveryBoardChecks == 0:
		return 10
	}
	return d.IdentityEveryBoardChecks
}

// UpgradeRestart reports whether idle agents are restarted after a slai upgrade.
func (d Defaults) UpgradeRestart() bool {
	return d.UpgradeRestartAgents == nil || *d.UpgradeRestartAgents
}

// SpendCaps are the supervisor's spend guard thresholds (0 = disabled).
type SpendCaps struct {
	PerAgentUSD float64 `yaml:"per_agent_usd,omitempty"`
	DailyUSD    float64 `yaml:"daily_usd,omitempty"`
	// PerTaskUSD caps the spend attributed to one task (usage recorded while an agent's
	// current task is that task, summed across agents). PerRunUSD caps the spend since the
	// run started (the later of `slai up` and the last `finish`).
	PerTaskUSD float64 `yaml:"per_task_usd,omitempty"`
	PerRunUSD  float64 `yaml:"per_run_usd,omitempty"`
}

type Agent struct {
	ID      string `yaml:"id"`
	Role    string `yaml:"role"`
	Model   string `yaml:"model"`
	Focus   string `yaml:"focus,omitempty"`
	Persona string `yaml:"persona,omitempty"`
	// PermissionMode overrides defaults.permission_mode for this agent.
	PermissionMode string `yaml:"permission_mode,omitempty"`
	// Harness is the agent CLI that runs this agent: claude (default), opencode or pi.
	// For opencode and pi, Model is passed through verbatim as provider/model.
	Harness string `yaml:"harness,omitempty"`
}

// Agent harnesses.
const (
	HarnessClaude   = "claude"
	HarnessOpenCode = "opencode"
	HarnessPi       = "pi"
)

// Harnesses lists the valid values of Agent.Harness.
var Harnesses = []string{HarnessClaude, HarnessOpenCode, HarnessPi}

// HarnessKind is the agent's harness, defaulting to claude.
func (a Agent) HarnessKind() string {
	if a.Harness == "" {
		return HarnessClaude
	}
	return a.Harness
}

const (
	RoleOrchestrator = "orchestrator"
	RolePM           = "product_manager"
	RolePrincipal    = "principal_engineer"
	RoleEngineer     = "engineer"
	RoleFocusGroup   = "focus_group"
)
