package supervisor

import (
	"context"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

// Everything that depends on which CLI an agent runs goes through its Harness: the
// launch line and run files, how a line is typed into its TUI, and how its pane is
// read. Nothing in this package builds a harness command line or parses a pane
// itself (TestNothingOutsideTheHarnessPackagesBuildsCommandsOrParsesPanes).

// harnessOfCfg is the harness of a configured agent (claude when unknown).
func (s *Supervisor) harnessOfCfg(a config.Agent) harness.Harness {
	h, err := harness.For(harness.Kind(a.HarnessKind()))
	if err != nil {
		h, _ = harness.For(harness.Claude)
	}
	return h
}

// harnessOf is the harness of the agent with this id.
func (s *Supervisor) harnessOf(id string) harness.Harness {
	if cfg, ok := s.agentCfg(id); ok {
		return s.harnessOfCfg(cfg)
	}
	h, _ := harness.For(harness.Claude)
	return h
}

// HarnessOf exposes an agent's harness (doctor, tests).
func (s *Supervisor) HarnessOf(a config.Agent) harness.Harness { return s.harnessOfCfg(a) }

// claudeTrusted is the trust-folder gate: while Claude Code has not recorded trust
// for the project, only the first agent starts. It only concerns Claude agents.
func (s *Supervisor) claudeTrusted() bool {
	h, _ := harness.For(harness.Claude)
	ok, _ := h.Trusted(harness.LaunchSpec{WorkDir: s.Dir, ProjectDir: s.Dir})
	return ok
}

// typeLine types a line and submits it the way this agent's TUI wants.
func (s *Supervisor) typeLine(ctx context.Context, a store.Agent, line string) error {
	k := s.harnessOf(a.ID).Keys()
	return s.Tmux.SendKeysDelay(ctx, a.TmuxSession, line, true, k.EnterDelay)
}

// clearInput empties the agent's input box with the harness's own key sequence
// (never Escape where that interrupts a running turn).
func (s *Supervisor) clearInput(ctx context.Context, a store.Agent) error {
	return s.Tmux.SendSeq(ctx, a.TmuxSession, s.harnessOf(a.ID).Keys().ClearSeq)
}

// retype clears the input box, pauses, and types a line (the nudge / retry shape).
func (s *Supervisor) retype(ctx context.Context, a store.Agent, line string) error {
	err := s.clearInput(ctx, a)
	if err == nil {
		time.Sleep(StuckKeyPause)
		err = s.typeLine(ctx, a, line)
	}
	return err
}

// modalExcerpt is hooks.ModalExcerpt for this agent's harness: a harness that never
// shows permission prompts (pi) has none to find.
func (s *Supervisor) modalExcerpt(a store.Agent, pane string) string {
	h := s.harnessOf(a.ID)
	if !h.Features().PermissionPrompts {
		return ""
	}
	return hooks.ModalExcerptWith(h.PaneRules(), pane)
}

// detectPane classifies this agent's pane. A test-supplied Detect overrides it.
func (s *Supervisor) detectPane(a store.Agent, pane string) (string, bool) {
	if s.Detect != nil {
		return s.Detect(pane)
	}
	return hooks.DetectWith(s.harnessOf(a.ID).PaneRules(), pane)
}

// paneDecision records that the pane (not a hook) changed what slai did: which
// rule of which harness matched, and a 200-character excerpt of what it matched on.
// The excerpt can contain the prompt's own text: it is for the log and doctor, never
// for a note an agent will read.
func (s *Supervisor) paneDecision(a store.Agent, action, pane string) {
	h := s.harnessOf(a.ID)
	d := hooks.ModalDecision(h.PaneRules(), pane)
	if d.Rule == "" {
		return
	}
	s.event(a.ID, "pane_decision", map[string]any{
		"action": action, "rule": string(h.Kind()) + "." + d.Rule, "excerpt": d.Excerpt, "status": d.Status,
	})
}
