package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/tmux"
)

// piHarness runs an agent in pi (pi-coding-agent 1.3.x). Facts it encodes come from
// the T-0025 spike and the T-0029 live run (decisions.md "Harness spike results",
// "Pi adapter"): pi has no permission prompts, so the extension's tool_call guard is
// the only write control and fails closed; an explicit -e extension needs no project
// trust, and --no-approve keeps a repo's own .pi/ from raising a trust modal;
// --session-id resumes in the same directory; plain Enter submits under tmux.
type piHarness struct{}

// piGuardTimeoutMS bounds one lane-guard call; a call that takes longer is blocked,
// not allowed (a var so tests can shorten it).
var piGuardTimeoutMS = 6000

func (piHarness) Kind() Kind                        { return Pi }
func (piHarness) NewSessionID() string              { return claude.NewSessionID() } // pi accepts an exact --session-id
func (piHarness) StatusSource() StatusSource        { return EventPush }
func (piHarness) Trusted(LaunchSpec) (bool, string) { return true, "" } // --no-approve: there is no trust dialog

// Keys: EnterDelay 0 submits (spike); Escape does not clear the input and interrupts
// a running turn, so the clear sequence is C-u alone.
func (piHarness) Keys() KeySpec {
	return KeySpec{ClearSeq: []tmux.Key{tmux.Named("C-u")}, SubmitKey: tmux.Named("Enter"), InterruptSeq: []tmux.Key{tmux.Named("Escape")}}
}

func (piHarness) Features() Features {
	return Features{Hooks: true, LaneGuard: true, Statusline: false, Usage: true, ResumeByID: true,
		PermissionPrompts: false, PromptSubmitEvent: true, TrustGate: false}
}

// PaneRules are built on the captured fixtures (internal/hooks/testdata/panes/pi).
// Pi never asks for approval, so the only blocking dialog is the project-trust
// modal, which --no-approve prevents; Modal is kept so an unexpected one is still
// seen. The idle footer is drawn only when no dialog is live, so it is Below: trust
// text that scrolled above a footer is history. Pi's rate-limit text could not be
// provoked cheaply (spike): RateLimit is a conservative guess on error lines, Reset
// never matches.
func (piHarness) PaneRules() hooks.Rules {
	return hooks.Rules{
		Working:   regexp.MustCompile(`[⠀-⣿]\s+Working\b`),
		Idle:      piFooterRe,
		Modal:     regexp.MustCompile(`(?m)^\s*→\s*Trust\b|escape/ctrl\+c cancel`),
		Option:    regexp.MustCompile(`^\s*(→|\s)\s*(Trust|Do not trust)`),
		RateLimit: regexp.MustCompile(`(?im)^\s*Error:.*(\b429\b|rate.?limit|too many requests|quota|insufficient balance)`),
		Reset:     regexp.MustCompile(`$^`),
		Below:     piFooterRe,
	}
}

// piFooterRe is pi's status footer ("0.3%/1.0M (auto)"), always drawn under the editor.
var piFooterRe = regexp.MustCompile(`\d+(\.\d+)?%/\d+(\.\d+)?[KM]\s+\(auto\)`)

func piRunDir(projectDir, agentID string) string {
	return filepath.Join(projectDir, ".slai", "run", agentID)
}

// PiExtensionPath is the generated extension file.
func PiExtensionPath(projectDir, agentID string) string {
	return filepath.Join(piRunDir(projectDir, agentID), "pi-extension.mjs")
}

// PiGuardReadyPath is the marker the extension writes once its guard is proven
// reachable. No marker within the start-up window means the agent runs unguarded
// and the supervisor must hold it (spec decision 8c).
func PiGuardReadyPath(projectDir, agentID string) string {
	return filepath.Join(piRunDir(projectDir, agentID), "guard_ready")
}

// PiGuardReadyPending describes why a pi agent has no guard yet ("" once it has).
func PiGuardReadyPending(projectDir, agentID string) string {
	if _, err := os.Stat(PiGuardReadyPath(projectDir, agentID)); err == nil {
		return ""
	}
	return fmt.Sprintf("pi extension has not reported guard_ready (%s missing)", PiGuardReadyPath(projectDir, agentID))
}

func piSlaiBin(s LaunchSpec) string {
	if s.SlaiBin == "" {
		return "slai"
	}
	return s.SlaiBin
}

// renderPiExtension renders the extension for s. Identity, project and the slai
// binary are baked in as JSON string literals; nothing is read from the ambient env.
func renderPiExtension(s LaunchSpec) (string, error) {
	src, err := templatesFS.ReadFile("templates/pi-extension.mjs.tmpl")
	if err != nil {
		return "", err
	}
	t, err := template.New("pi-extension").Funcs(template.FuncMap{
		"json": func(v string) (string, error) { b, err := json.Marshal(v); return string(b), err },
	}).Parse(string(src))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]string{
		"Agent": s.AgentID, "Project": s.ProjectDir, "Slai": piSlaiBin(s), "RunDir": piRunDir(s.ProjectDir, s.AgentID),
		"GuardTimeoutMS": strconv.Itoa(piGuardTimeoutMS),
	})
	if err != nil {
		return "", fmt.Errorf("render pi extension: %w", err)
	}
	return buf.String(), nil
}

// WriteRunFiles writes system.md and pi-extension.mjs. It is idempotent.
func (piHarness) WriteRunFiles(s LaunchSpec) error {
	dir := piRunDir(s.ProjectDir, s.AgentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ext, err := renderPiExtension(s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(PiExtensionPath(s.ProjectDir, s.AgentID), []byte(ext), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "system.md"), []byte(s.SystemPrompt), 0o644)
}

// Command returns the shell line run inside tmux. Identity is set inline on the
// command, not in the tmux session environment; the extension pins it again for
// the MCP server and every hook. Flags:
//
//	--no-extensions   the user's own pi extensions stay out (explicit -e still loads).
//	                  It also switches off pi's built-in MCP support, which is what
//	                  connects the slai MCP server the extension registers, so
//	                  "-e builtin:mcp" puts exactly that back (found in the T-0029 live run)
//	--no-approve      never raise or answer a project-trust modal; project .pi/ is ignored
//	--session-id      pi creates the session with this exact id, or resumes it
//
// There is no --continue, which would cross agents sharing a directory, and no
// --approve.
func (piHarness) Command(s LaunchSpec) string {
	args := []string{
		"env", "SLAI_AGENT_ID=" + shq(s.AgentID), "SLAI_PROJECT=" + shq(s.ProjectDir),
		"pi", "--model", shq(s.Model),
		"--append-system-prompt", `"$(cat ` + shq(filepath.Join(piRunDir(s.ProjectDir, s.AgentID), "system.md")) + `)"`,
		"--no-extensions", "-e", "builtin:mcp", "-e", shq(PiExtensionPath(s.ProjectDir, s.AgentID)),
		"--no-approve",
	}
	if s.SessionID != "" {
		args = append(args, "--session-id", shq(s.SessionID))
	}
	return strings.Join(args, " ")
}
