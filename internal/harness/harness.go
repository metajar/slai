// Package harness is the seam between slai and the agent CLI that runs an agent.
// Everything that builds a harness command line, writes its run files, reads its
// TUI or knows what it can and cannot do goes through a Harness; nothing outside
// the harness packages may do so (spec harnesses.md, binding decision 2).
package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/tmux"
)

// Kind names a harness. The values are the ones accepted by `harness:` in team.yaml.
type Kind string

const (
	Claude   Kind = "claude"
	OpenCode Kind = "opencode"
	Pi       Kind = "pi"
)

// Kinds lists every harness in a stable order (claude first).
func Kinds() []Kind { return []Kind{Claude, OpenCode, Pi} }

// For returns the harness for kind; an unknown kind is an error naming the valid ones.
func For(kind Kind) (Harness, error) {
	switch kind {
	case Claude, "":
		return claudeHarness{}, nil
	case OpenCode:
		return openCodeHarness{}, nil
	case Pi:
		return piHarness{}, nil
	}
	names := make([]string, 0, 3)
	for _, k := range Kinds() {
		names = append(names, string(k))
	}
	return nil, fmt.Errorf("unknown harness %q (want one of %s)", kind, strings.Join(names, ", "))
}

// LaunchSpec is everything slai knows about one agent launch.
type LaunchSpec struct {
	AgentID, ProjectDir, WorkDir, SlaiBin, SystemPrompt string
	Model                                               string // verbatim for opencode/pi; resolved claude-* for claude
	PermissionMode                                      string // claude modes; mapped to allow/deny rules for opencode; ignored for pi
	Allow, Deny                                         []string
	SessionID                                           string
	Resume                                              bool
}

// StatusSource says how a harness reports working/idle/waiting/stop.
type StatusSource string

const (
	EventPush StatusSource = "event_push" // hooks / plugin / extension call `slai hook`
	PaneOnly  StatusSource = "pane_only"  // only the pane can be read
)

// KeySpec is how to type a line into the harness's TUI under tmux.
type KeySpec struct {
	EnterDelay time.Duration // pause between the typed text and Enter
	ClearSeq   []tmux.Key    // clears a half-typed input box
	SubmitKey  tmux.Key
	// InterruptSeq stops a running turn (the TUI's course correction). Escape on all three.
	InterruptSeq []tmux.Key
}

// Features says what a harness supports. A false capability is shown to the user
// as degraded; the pane/permission fields are not capabilities and are not listed.
type Features struct {
	Hooks, LaneGuard, Statusline, Usage, ResumeByID bool
	// PermissionPrompts is false when the harness never asks (pi): modal detection,
	// safeToType-on-modal and waiting handling are skipped, not "the pane says nothing".
	PermissionPrompts bool
	// PromptSubmitEvent is false when the harness reports no prompt-submitted event:
	// the wake-landed check is disabled for it.
	PromptSubmitEvent bool
	TrustGate         bool // a trust-folder dialog gates new agents (claude trust file, pi --approve)
}

// Degraded names the capabilities the harness lacks, in a stable order.
func (f Features) Degraded() []string {
	var out []string
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"hooks", f.Hooks},
		{"lane_guard", f.LaneGuard},
		{"statusline", f.Statusline},
		{"usage", f.Usage},
		{"resume_by_id", f.ResumeByID},
		{"wake_check", f.PromptSubmitEvent},
	} {
		if !c.ok {
			out = append(out, c.name)
		}
	}
	return out
}

// Harness is one agent CLI.
type Harness interface {
	Kind() Kind
	// WriteRunFiles writes every file under .slai/run/<agent>/ (system prompt, MCP
	// config with pinned identity, hooks/plugin/extension, permission config). Idempotent.
	WriteRunFiles(LaunchSpec) error
	// Command is the shell line run inside tmux. It is byte-stable for the same spec
	// (the reconcile hash and the regression tests depend on it).
	Command(LaunchSpec) string
	// NewSessionID returns an id slai may pass on first launch, or "" when the
	// harness assigns its own and reports it through the status source.
	NewSessionID() string
	StatusSource() StatusSource
	// PaneRules is the data-only rule set pane detection applies for this harness.
	PaneRules() hooks.Rules
	Keys() KeySpec
	Features() Features
	// Trusted replaces claude.IsTrusted in the trust gate; why explains a false.
	Trusted(LaunchSpec) (ok bool, why string)
}

// SessionIDPath is where an agent's session id is persisted (harness-neutral).
func SessionIDPath(projectDir, agentID string) string {
	return filepath.Join(projectDir, ".slai", "run", agentID, "session-id")
}

// GuardReadyPath is the marker a plugin/extension writes once its lane guard is
// registered (Pi); OpenCode reports the same fact as a hook:guard_ready event. A
// launch removes it, so it always means "this launch".
func GuardReadyPath(projectDir, agentID string) string {
	return filepath.Join(projectDir, ".slai", "run", agentID, "guard_ready")
}

// LoadOrCreateSessionID returns the agent's persisted session id, asking h for a new
// one the first time. existed reports whether it was already stored. A harness that
// assigns its own ids (NewSessionID() == "") yields "" until one is captured.
func LoadOrCreateSessionID(h Harness, projectDir, agentID string) (id string, existed bool, err error) {
	p := SessionIDPath(projectDir, agentID)
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, true, nil
		}
	}
	id = h.NewSessionID()
	if id == "" {
		return "", false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", false, err
	}
	return id, false, os.WriteFile(p, []byte(id+"\n"), 0o644)
}
