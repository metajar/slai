// Package roles holds per-role prompt templates and tool permission rules.
package roles

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"github.com/oorbt/slai/internal/config"
)

//go:embed templates/*.md
var templatesFS embed.FS

// AllRoles lists the five roles.
var AllRoles = []string{
	config.RoleOrchestrator, config.RolePM, config.RolePrincipal, config.RoleEngineer, config.RoleFocusGroup,
}

var tools = map[string][]string{
	config.RoleOrchestrator: {"create_task", "update_task", "get_board", "send_message", "ask_user", "get_inbox", "cancel_task", "finish", "ping", "pong", "reply", "remember", "recall", "get_memory"},
	config.RolePM:           {"get_board", "get_task", "report", "send_message", "ask", "ask_user", "get_inbox", "pong", "reply", "remember", "recall", "get_memory"},
	config.RolePrincipal:    {"get_board", "review_diff", "approve", "request_changes", "send_message", "ask", "get_inbox", "pong", "reply", "remember", "recall", "get_memory"},
	config.RoleEngineer:     {"get_task", "report", "request_review", "ask", "send_message", "get_inbox", "pong", "reply", "remember", "recall", "get_memory"},
	config.RoleFocusGroup:   {"get_inbox", "reply", "get_board", "pong", "recall", "get_memory"}, // read-only
}

// ToolNames returns the bare slai tool names for a role (nil if unknown).
func ToolNames(role string) []string {
	return append([]string(nil), tools[role]...)
}

// AllowedTools returns the MCP tool names (mcp__slai__<tool>) a role may call.
func AllowedTools(role string) []string {
	var out []string
	for _, t := range tools[role] {
		out = append(out, "mcp__slai__"+t)
	}
	return out
}

var editTools = []string{"Edit", "Write", "NotebookEdit"}

// ClaudeToolRules returns allow and deny permission rules for Claude Code's
// settings.json. worktree is the engineer's worktree path (used to scope edits).
//
// Claude Code gives deny rules precedence over allow rules, so roles that may
// edit one path (PM, principal) get an allow rule for that path and no blanket
// edit deny; other paths fall back to the normal permission prompt.
func ClaudeToolRules(role, worktree string) (allow, deny []string) {
	allow = AllowedTools(role)
	// slai creates and owns worktrees; an agent that enters its own writes into a
	// throwaway copy of the repo that never reaches the real project.
	deny = []string{"Bash(git push:*)", "Bash(git reset --hard:*)", "EnterWorktree"}
	if role != config.RoleEngineer {
		// No worktree to confine rm to; engineers are checked by the PreToolUse lane hook.
		deny = append(deny, "Bash(rm -rf:*)", "Bash(rm -fr:*)", "Bash(rm -r:*)")
	}
	scoped := func(path string) {
		// Claude Code only honours Edit(path) rules; they cover Write too.
		allow = append(allow, fmt.Sprintf("Edit(%s)", path))
	}
	switch role {
	case config.RoleEngineer:
		if worktree != "" {
			// Claude Code reads "//path" as an absolute filesystem path ("/path" is project-relative).
			scoped("/" + filepath.ToSlash(worktree) + "/**")
		}
	case config.RolePM:
		scoped(".slai/specs/**")
	case config.RolePrincipal:
		scoped(".slai/decisions.md")
	case config.RoleFocusGroup:
		// Personas review designs and docs the PM points them to, screenshots included
		// (Read opens images), from anywhere on disk: without these rules a file outside
		// the project dir raises a permission prompt nobody answers. Nothing here
		// writes; Edit/Write stay denied and Bash stays unlisted.
		allow = append(allow, "Read", "Glob", "Grep")
		deny = append(deny, editTools...)
	default: // orchestrator, unknown: read-only
		deny = append(deny, editTools...)
	}
	return allow, deny
}

type teammate struct{ ID, Role, Focus string }

type data struct {
	Dir         string // absolute project root
	Project     config.Project
	Agent       config.Agent
	RoleName    string
	Integration string // integration branch name (reviews/merges target it)
	Main        string // the project's main branch
	Teammates   []teammate
}

var roleNames = map[string]string{
	config.RoleOrchestrator: "Orchestrator", config.RolePM: "Product manager",
	config.RolePrincipal: "Principal engineer", config.RoleEngineer: "Engineer",
	config.RoleFocusGroup: "Focus group persona",
}

// Render builds the system prompt for agent. If projectDir/.slai/prompts/<role>.md
// exists it replaces the embedded template (and is itself a text/template).
func Render(team *config.Team, agent config.Agent, projectDir string) (string, error) {
	src, err := templatesFS.ReadFile("templates/" + agent.Role + ".md")
	if err != nil {
		return "", fmt.Errorf("roles: unknown role %q", agent.Role)
	}
	if projectDir != "" {
		b, err := os.ReadFile(filepath.Join(projectDir, ".slai", "prompts", agent.Role+".md"))
		if err == nil {
			src = b
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	d := data{Agent: agent, RoleName: roleNames[agent.Role], Dir: projectDir}
	if team != nil {
		d.Project = team.Project
		d.Integration, d.Main = team.Project.IntegrationBranch, team.Project.MainBranch
		for _, a := range team.Agents {
			if a.ID != agent.ID {
				d.Teammates = append(d.Teammates, teammate{a.ID, a.Role, a.Focus})
			}
		}
	}
	if d.Integration == "" {
		d.Integration = config.DefaultIntegrationBranch
	}
	if d.Main == "" {
		d.Main = "main"
	}
	tpl, err := template.New(agent.Role).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}
