package harness

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/oorbt/slai/internal/claude"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/tmux"
)

//go:embed templates/*
var templatesFS embed.FS

// openCodeHarness runs an agent in OpenCode (opencode 1.18.x, spike T-0025).
//
// Isolation: the user's own ~/.config/opencode (which may be invalid, and carries
// their MCP servers and plugins) is never read: XDG_CONFIG_HOME points at an empty
// dir under the agent's run dir, OPENCODE_CONFIG names a generated config, and
// OPENCODE_DISABLE_PROJECT_CONFIG keeps a repository's own .opencode/ out. Auth
// lives in the data dir and stays reachable. Never --pure (plugins must load) and
// never -c/--continue (it resumes the newest session in the directory, crossing agents).
type openCodeHarness struct{}

func (openCodeHarness) Kind() Kind { return OpenCode }

// NewSessionID is "": opencode assigns its own ses_... ids, which the plugin reports
// on session.created and `slai hook session_start` stores in run/<agent>/session-id.
func (openCodeHarness) NewSessionID() string       { return "" }
func (openCodeHarness) StatusSource() StatusSource { return EventPush }

// Keys: EnterDelay 0 submits (spike); Escape does not clear the input and interrupts a
// running turn, so the clear sequence is C-u alone.
func (openCodeHarness) Keys() KeySpec {
	return KeySpec{ClearSeq: []tmux.Key{tmux.Named("C-u")}, SubmitKey: tmux.Named("Enter"), InterruptSeq: []tmux.Key{tmux.Named("Escape")}}
}

func (openCodeHarness) Features() Features {
	return Features{Hooks: true, LaneGuard: true, Statusline: false, Usage: true, ResumeByID: false,
		PermissionPrompts: true, PromptSubmitEvent: true, TrustGate: false}
}

func (openCodeHarness) Trusted(LaunchSpec) (bool, string) { return true, "" }

// PaneRules (fixtures: internal/hooks/testdata/panes/opencode). A permission dialog
// replaces the input box, so "ctrl+p commands" / "esc interrupt" / the home hints
// drawn below a "Permission required" line mean it is history, not a live prompt.
func (openCodeHarness) PaneRules() hooks.Rules {
	return hooks.Rules{
		Working:      regexp.MustCompile(`esc interrupt`),
		Idle:         regexp.MustCompile(`ctrl\+p commands`),
		Modal:        regexp.MustCompile(`Permission required`),
		Option:       regexp.MustCompile(`Allow once|Allow always|Reject|enter confirm|ctrl\+f fullscreen`),
		RateLimit:    regexp.MustCompile(`\[retrying in \d+s attempt #\d+\]`),
		Below:        regexp.MustCompile(`ctrl\+p commands|esc interrupt|tab agents`),
		ContextAfter: true,
		MainColumn:   true,
	}
}

// AgentName is the opencode agent definition slai generates for an agent.
func openCodeAgentName(id string) string { return "slai-" + id }

func ocRunDir(s LaunchSpec) string { return filepath.Join(s.ProjectDir, ".slai", "run", s.AgentID) }

func (h openCodeHarness) bin(s LaunchSpec) string {
	if s.SlaiBin == "" {
		return "slai"
	}
	return s.SlaiBin
}

// WriteRunFiles writes system.md, plugin.js, opencode.json and the empty xdg/ dir.
func (h openCodeHarness) WriteRunFiles(s LaunchSpec) error {
	if err := claude.CheckSystemPrompt(s.ProjectDir, s.AgentID, s.SystemPrompt); err != nil {
		return err
	}
	dir := ocRunDir(s)
	if err := os.MkdirAll(filepath.Join(dir, "xdg"), 0o755); err != nil {
		return err
	}
	plugin, err := renderOpenCodePlugin(s, h.bin(s))
	if err != nil {
		return err
	}
	cfg, err := json.MarshalIndent(h.config(s), "", "  ")
	if err != nil {
		return err
	}
	for name, body := range map[string][]byte{
		"plugin.js":     plugin,
		"opencode.json": append(cfg, '\n'),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return err
		}
	}
	return writeSystemPrompt(s, dir)
}

// config is the generated opencode.json. Identity is pinned in the MCP entry twice
// (environment and argv) and in the plugin source.
func (h openCodeHarness) config(s LaunchSpec) map[string]any {
	name := openCodeAgentName(s.AgentID)
	return map[string]any{
		"$schema":       "https://opencode.ai/config.json",
		"autoupdate":    false,
		"share":         "disabled",
		"model":         s.Model,
		"default_agent": name,
		"agent": map[string]any{name: map[string]any{
			"mode": "primary", "description": "slai agent " + s.AgentID, "prompt": s.SystemPrompt,
		}},
		"mcp": map[string]any{"slai": map[string]any{
			"type":        "local",
			"command":     []string{h.bin(s), "mcp", "--agent", s.AgentID, "--project", s.ProjectDir},
			"environment": map[string]string{"SLAI_AGENT_ID": s.AgentID, "SLAI_PROJECT": s.ProjectDir},
		}},
		"plugin":     []string{filepath.Join(ocRunDir(s), "plugin.js")},
		"permission": openCodePermission(s.PermissionMode, s.Allow, s.Deny),
	}
}

// Command returns the shell line run inside tmux. The identity and the isolation are
// inline env on the command, not the tmux session's environment. A revive resumes the
// session id the plugin captured and falls back to a fresh session, so the agent is
// never left at a shell prompt; without a captured id every launch is fresh.
func (h openCodeHarness) Command(s LaunchSpec) string {
	dir := ocRunDir(s)
	env := strings.Join([]string{
		"SLAI_AGENT_ID=" + shq(s.AgentID),
		"SLAI_PROJECT=" + shq(s.ProjectDir),
		"XDG_CONFIG_HOME=" + shq(filepath.Join(dir, "xdg")),
		"OPENCODE_CONFIG=" + shq(filepath.Join(dir, "opencode.json")),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
	}, " ")
	with := func(sessionID string) string {
		args := []string{env, "opencode", "-m", shq(s.Model), "--agent", shq(openCodeAgentName(s.AgentID))}
		if sessionID != "" {
			args = append(args, "-s", shq(sessionID))
		}
		return strings.Join(args, " ")
	}
	if s.Resume && s.SessionID != "" {
		return with(s.SessionID) + " || " + with("")
	}
	return with("")
}

func shq(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// openCodePermission derives opencode's permission block from a Claude permission
// mode and Claude-style allow/deny rules. It never leaves a decision to a dialog
// slai would have to leave unanswered: unless the mode is "manual" (ask, as the user
// chose) every tool is allowed or denied outright.
//
//   - mode: plan -> edit and bash denied; manual -> asked; everything else allowed.
//   - external_directory and doom_loop are always allowed: engineers read specs and the
//     main checkout from outside their worktree, and either default prompt would be a
//     dialog nobody answers (spike T-0025, BD5).
//   - A bare deny rule for a tool (Edit, Write, ...) denies that tool; Bash(prefix:*)
//     becomes a bash pattern (later, more specific rules win). Scoped Edit(path) allow
//     rules are not translated: path syntax differs between the harnesses and the
//     plugin's lane guard already confines writes for every role.
func openCodePermission(mode string, allow, deny []string) map[string]any {
	base := "allow"
	switch mode {
	case "manual", "default":
		base = "ask"
	case "plan":
		base = "deny"
	}
	perm := map[string]any{
		"edit": base, "bash": base, "read": "allow", "webfetch": "allow",
		"external_directory": "allow", "doom_loop": "allow",
	}
	editTool := map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
	bashAllow, bashDeny := map[string]bool{}, map[string]bool{}
	for _, r := range deny {
		tool, arg, scoped := splitRule(r)
		switch {
		case editTool[tool] && !scoped:
			perm["edit"] = "deny"
		case tool == "Bash" && !scoped:
			perm["bash"] = "deny"
		case tool == "Bash":
			bashDeny[bashPattern(arg)] = true
		case tool == "Read" && !scoped:
			perm["read"] = "deny"
		case tool == "WebFetch" && !scoped:
			perm["webfetch"] = "deny"
		}
	}
	for _, r := range allow {
		if tool, arg, scoped := splitRule(r); tool == "Bash" && scoped {
			bashAllow[bashPattern(arg)] = true
		}
	}
	if perm["bash"] != "deny" && (len(bashAllow) > 0 || len(bashDeny) > 0) {
		rules := map[string]string{"*": base}
		for _, p := range sortedKeys(bashAllow) {
			rules[p] = "allow"
		}
		for _, p := range sortedKeys(bashDeny) {
			rules[p] = "deny" // a deny beats an allow of the same pattern
		}
		perm["bash"] = rules
	}
	return perm
}

var ruleRe = regexp.MustCompile(`^([A-Za-z_]+)(?:\((.*)\))?$`)

// splitRule parses "Tool" or "Tool(arg)".
func splitRule(r string) (tool, arg string, scoped bool) {
	m := ruleRe.FindStringSubmatch(strings.TrimSpace(r))
	if m == nil {
		return "", "", false
	}
	if strings.HasPrefix(r[len(m[1]):], "(") {
		return m[1], m[2], m[2] != "" && m[2] != "*"
	}
	return m[1], "", false
}

// bashPattern turns Claude's "git push:*" prefix rule into opencode's "git push *".
func bashPattern(arg string) string {
	if p, ok := strings.CutSuffix(arg, ":*"); ok {
		return p + " *"
	}
	return arg
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func renderOpenCodePlugin(s LaunchSpec, bin string) ([]byte, error) {
	src, err := templatesFS.ReadFile("templates/opencode_plugin.js.tmpl")
	if err != nil {
		return nil, err
	}
	t, err := template.New("opencode_plugin").Parse(string(src))
	if err != nil {
		return nil, err
	}
	q := func(v string) string { b, _ := json.Marshal(v); return string(b) } // JS-safe string literal
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]string{
		"AgentName": s.AgentID, "Bin": q(bin), "Agent": q(s.AgentID), "Project": q(s.ProjectDir),
	})
	if err != nil {
		return nil, fmt.Errorf("render opencode plugin: %w", err)
	}
	return buf.Bytes(), nil
}

// RunFileNames are the generated files `slai doctor` checks exist for an agent.
func (openCodeHarness) RunFileNames() []string {
	return []string{"opencode.json", "plugin.js", "system.md"}
}
