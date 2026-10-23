// Package claude builds the claude CLI invocation and per-agent run files.
// All claude flag construction lives here so CLI drift is a one-file fix.
package claude

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HookEvents are the Claude Code hook events slai listens to.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Notification", "Stop"}

// Spec describes one agent launch.
type Spec struct {
	AgentID        string
	Model          string // resolved value passed to --model
	PermissionMode string
	ProjectDir     string // absolute repo root
	WorkDir        string // repo root or engineer worktree
	SystemPrompt   string
	Allow, Deny    []string // Claude Code permission rules
	SlaiBin        string   // path used in hook/mcp commands; default "slai"
	// SessionID is this agent's own Claude Code conversation. Agents that share a
	// directory (everyone but the engineers) must never use --continue, which resumes
	// whichever conversation in the directory was touched last and so crosses agents.
	SessionID string
	Resume    bool // revive: resume SessionID rather than start it
}

// ErrNoIdentity is returned (wrapped) when an agent would be launched with no usable role
// prompt: a session with no system prompt does not know who it is.
var ErrNoIdentity = errors.New("no role prompt")

// CheckSystemPrompt refuses a launch whose role prompt is empty or does not name the agent.
// Every harness passes this text on its system-prompt channel (claude and pi
// --append-system-prompt "$(cat system.md)", opencode the agent's prompt), so an empty or
// foreign prompt means an ungrounded session.
func CheckSystemPrompt(projectDir, agentID, prompt string) error {
	file := filepath.Join(RunDir(projectDir, agentID), "system.md")
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("agent %s: %w: %s would be empty, so the session would not know who it is; refusing to launch (check .slai/prompts/ and team.yaml)", agentID, ErrNoIdentity, file)
	}
	if !strings.Contains(prompt, agentID) {
		return fmt.Errorf("agent %s: %w: the prompt for %s does not name the agent; refusing to launch", agentID, ErrNoIdentity, file)
	}
	return nil
}

// RunDir is where generated files for the agent live.
func RunDir(projectDir, agentID string) string {
	return filepath.Join(projectDir, ".slai", "run", agentID)
}

// Env returns the environment the agent process needs.
func (s Spec) Env() map[string]string {
	return map[string]string{"SLAI_AGENT_ID": s.AgentID, "SLAI_PROJECT": s.ProjectDir}
}

func (s Spec) bin() string {
	if s.SlaiBin == "" {
		return "slai"
	}
	return s.SlaiBin
}

// WriteRunFiles writes system.md, mcp.json and settings.json.
func (s Spec) WriteRunFiles() error {
	if err := CheckSystemPrompt(s.ProjectDir, s.AgentID, s.SystemPrompt); err != nil {
		return err
	}
	dir := RunDir(s.ProjectDir, s.AgentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Identity is pinned in the run files, never taken from the inherited process
	// env: Claude Code can re-host a session in a background process that carries
	// another agent's SLAI_AGENT_ID. The --agent flag and this env both win over it.
	mcp := map[string]any{"mcpServers": map[string]any{
		"slai": map[string]any{"command": s.bin(), "args": []string{"mcp", "--agent", s.AgentID, "--project", s.ProjectDir}, "env": s.Env()},
	}}
	hooks := map[string]any{}
	for _, ev := range HookEvents {
		hooks[ev] = []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": fmt.Sprintf("%s hook %s --agent %s", s.bin(), strings.ToLower(ev), s.AgentID), "timeout": 30,
		}}}}
	}
	settings := map[string]any{
		"env":         s.Env(), // Bash tools and hooks see the right id
		"hooks":       hooks,
		"statusLine":  map[string]any{"type": "command", "command": s.bin() + " statusline --agent " + s.AgentID},
		"permissions": map[string]any{"allow": nonNil(s.Allow), "deny": nonNil(s.Deny)},
	}
	for name, v := range map[string]any{"mcp.json": mcp, "settings.json": settings} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "system.md"), []byte(s.SystemPrompt), 0o644)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Command returns the shell command line to run inside tmux.
// --mcp-config is variadic, so it is never the last flag before a positional.
//
// A new agent starts its own session with --session-id. A revive resumes exactly
// that session with --resume. If the session does not exist yet (the agent died
// before its first prompt) it is started under the same id, and if the id is
// unusable for some other reason a fresh id is the last resort, so an agent is
// never left dead at a shell prompt.
func (s Spec) Command() string {
	dir := RunDir(s.ProjectDir, s.AgentID)
	tail := []string{
		"--append-system-prompt", `"$(cat ` + shq(filepath.Join(dir, "system.md")) + `)"`,
		"--settings", shq(filepath.Join(dir, "settings.json")),
		"--permission-mode", shq(s.PermissionMode),
		"--mcp-config", shq(filepath.Join(dir, "mcp.json")),
	}
	with := func(sessionFlag, id string) string {
		args := []string{"claude", "--model", shq(s.Model)}
		if id != "" {
			args = append(args, sessionFlag, shq(id))
		}
		return strings.Join(append(args, tail...), " ")
	}
	if s.SessionID == "" { // no session management (tests, odd callers)
		return with("", "")
	}
	if !s.Resume {
		return with("--session-id", s.SessionID)
	}
	return with("--resume", s.SessionID) + " || " + with("--session-id", s.SessionID) + " || " + with("--session-id", NewSessionID())
}

// NewSessionID returns a random UUID (version 4) for a Claude Code session.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SessionIDPath is where an agent's session id is persisted.
func SessionIDPath(projectDir, agentID string) string {
	return filepath.Join(RunDir(projectDir, agentID), "session-id")
}

// LoadOrCreateSessionID returns the agent's persisted session id, creating one
// the first time. existed reports whether it was already there.
func LoadOrCreateSessionID(projectDir, agentID string) (id string, existed bool, err error) {
	p := SessionIDPath(projectDir, agentID)
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, true, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", false, err
	}
	id = NewSessionID()
	return id, false, os.WriteFile(p, []byte(id+"\n"), 0o644)
}

func shq(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// SortedEnv renders env for display/tests.
func SortedEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
