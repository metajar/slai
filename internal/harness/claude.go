package harness

import (
	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/tmux"
)

// claudeHarness adapts package claude, which keeps building the CLI line exactly as
// before.
type claudeHarness struct{}

func (claudeHarness) Kind() Kind { return Claude }

func claudeSpec(s LaunchSpec) claude.Spec {
	return claude.Spec{
		AgentID: s.AgentID, Model: s.Model, PermissionMode: s.PermissionMode,
		ProjectDir: s.ProjectDir, WorkDir: s.WorkDir, SystemPrompt: s.SystemPrompt,
		Allow: s.Allow, Deny: s.Deny, SlaiBin: s.SlaiBin, SessionID: s.SessionID, Resume: s.Resume,
	}
}

func (claudeHarness) WriteRunFiles(s LaunchSpec) error { return claudeSpec(s).WriteRunFiles() }
func (claudeHarness) Command(s LaunchSpec) string      { return claudeSpec(s).Command() }
func (claudeHarness) NewSessionID() string             { return claude.NewSessionID() }
func (claudeHarness) StatusSource() StatusSource       { return EventPush }
func (claudeHarness) PaneRules() hooks.Rules           { return hooks.ClaudeRules() }

func (claudeHarness) Keys() KeySpec {
	return KeySpec{
		EnterDelay:   tmux.EnterDelay,
		ClearSeq:     []tmux.Key{tmux.Named("Escape"), tmux.Named("C-u")},
		SubmitKey:    tmux.Named("Enter"),
		InterruptSeq: []tmux.Key{tmux.Named("Escape")},
	}
}

func (claudeHarness) Features() Features {
	return Features{Hooks: true, LaneGuard: true, Statusline: true, Usage: true, ResumeByID: true,
		PermissionPrompts: true, PromptSubmitEvent: true, TrustGate: true}
}

func (claudeHarness) Trusted(s LaunchSpec) (bool, string) {
	if claude.IsTrusted(s.WorkDir) {
		return true, ""
	}
	return false, "Claude Code has not recorded trust for " + s.WorkDir + " (answer its trust prompt once)"
}
