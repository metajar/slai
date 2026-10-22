package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Neutral hook payloads let a non-Claude harness's plugin or extension report
// what it sees without impersonating Claude Code's JSON:
//
//	slai hook <event> --agent <id> --project <dir>   (JSON on stdin, all fields optional)
//	{"event":"tool","tool":"bash","tool_input":{...},"session_id":"...","cwd":"...",
//	 "usage":{"prompt":1200,"output":80,"cost":0.004}}
//
// Neutral events map onto the Claude events the rest of slai already understands,
// so the in-flight-tool guard, stuck detection and the wake-landed check work
// unchanged. usage is a delta: what was spent since the previous report.

// NeutralUsage is spend since the previous report.
type NeutralUsage struct {
	Prompt int64   `json:"prompt"`
	Output int64   `json:"output"`
	Cost   float64 `json:"cost"`
}

// neutralEvent is one row of the neutral -> Claude mapping.
type neutralEvent struct {
	Claude       string // the Claude hook event it is handled as
	Notification string // notification_type for Claude's "notification"
}

// NeutralEvents is the mapping table (published in decisions.md; a test asserts each row).
var NeutralEvents = map[string]neutralEvent{
	"session_start": {Claude: "sessionstart"},
	"prompt":        {Claude: "userpromptsubmit"},
	"tool":          {Claude: "pretooluse"},
	"tool_end":      {Claude: "posttooluse"},
	"idle":          {Claude: "notification", Notification: "idle_prompt"},
	"stop":          {Claude: "stop"},
	"waiting":       {Claude: "notification", Notification: "permission_prompt"},
	// A plugin/extension reports that its tool-call guard is loaded; recorded as
	// hook:guard_ready with no status effect (a harness without one is held, spec 8c).
	"guard_ready": {Claude: "guard_ready"},
}

// neutralOnly are neutral event names that are not Claude event names; "stop" is
// both, so it is neutral only when the payload says so (an "event" field).
func isNeutralName(n string) bool {
	_, ok := NeutralEvents[n]
	return ok && n != "stop"
}

// Adapt turns a hook invocation into the Claude-shaped event and payload Handle
// takes. argEvent is the command-line event, p the decoded stdin. neutral reports
// whether it was a neutral payload (the reply format differs); an unknown neutral
// event is an error.
func Adapt(argEvent string, p Payload) (event string, out Payload, neutral bool, err error) {
	name := strings.ToLower(argEvent)
	neutral = p.Event != "" || isNeutralName(name)
	if name == "" { // the event may come from the payload alone
		name = strings.ToLower(p.Event)
		if name == "" {
			name = strings.ToLower(p.HookEventName)
		}
	}
	if !neutral {
		return name, p, false, nil
	}
	m, ok := NeutralEvents[name]
	if !ok {
		return name, p, true, fmt.Errorf("unknown neutral hook event %q", name)
	}
	out = p
	out.fromNeutral = true
	out.HookEventName = m.Claude
	if m.Notification != "" {
		out.NotificationType = m.Notification
	}
	if p.Tool != "" {
		out.ToolName, out.ToolInput = normalizeTool(p.Tool, p.ToolInput)
	}
	return m.Claude, out, true, nil
}

// Other harnesses name their tools in lower case and key the target differently
// (OpenCode filePath, Pi path); the lane guard knows Claude's names and file_path.
var toolAliases = map[string]string{
	"write": "Write", "edit": "Edit", "multiedit": "MultiEdit", "notebookedit": "NotebookEdit",
	"bash": "Bash", "shell": "Bash",
}

func normalizeTool(tool string, input json.RawMessage) (string, json.RawMessage) {
	name := tool
	if c, ok := toolAliases[strings.ToLower(tool)]; ok {
		name = c
	}
	if !editToolNames[name] || len(input) == 0 {
		return name, input
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return name, input
	}
	if v, ok := m["file_path"].(string); !ok || v == "" {
		for _, k := range []string{"filePath", "path", "file", "notebook_path"} {
			if v, ok := m[k].(string); ok && v != "" {
				m["file_path"] = v
				break
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return name, input
	}
	return name, b
}

// Tools that cannot change anything on disk, by the names OpenCode and Pi give them
// (OpenCode 1.18 tool ids, Pi 1.3 built-ins). Everything else is judged.
var readOnlyTools = map[string]bool{
	"read": true, "grep": true, "glob": true, "list": true, "ls": true, "find": true,
	"webfetch": true, "websearch": true, "codesearch": true, "todowrite": true, "todoread": true,
	"task": true, "skill": true, "lsp": true, "question": true, "plan_enter": true, "plan_exit": true,
}

// patchFileRe finds the files a patch touches: apply_patch's "*** Add/Update/Delete
// File:" and "*** Move to:" headers and unified-diff "+++ b/path" lines.
var patchFileRe = regexp.MustCompile(`(?m)^(?:\*\*\* (?:Add File|Update File|Delete File|Move to):|\+\+\+) ?(?:b/)?(.+?)\s*$`)

// neutralToolGuard judges what the Claude-shaped lane check cannot, for a caller that
// has no permission layer behind the guard (a plugin or extension), and fails closed:
//   - a write tool whose target path is unknown is denied;
//   - a patch tool is checked file by file, from the patch text (none found: denied);
//   - a tool the guard does not know that carries a path-like argument is denied when
//     that path is outside the agent's lane ("unknown tool <name>"). Known read-only
//     tools, and unknown tools with no path argument, are left alone.
func neutralToolGuard(l Lane, p Payload) *Decision {
	if editToolNames[p.ToolName] {
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(p.ToolInput, &in)
		if in.FilePath == "" && in.NotebookPath == "" {
			return deny("%s call without a target path cannot be checked against your lane", p.ToolName)
		}
		return nil
	}
	name := strings.ToLower(p.ToolName)
	switch {
	case p.ToolName == "Bash" || readOnlyTools[name] || strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "slai_"):
		return nil
	case name == "patch" || name == "apply_patch" || name == "applypatch":
		var in map[string]any
		_ = json.Unmarshal(p.ToolInput, &in)
		text := ""
		for _, k := range []string{"patchText", "patch_text", "patch", "diff", "input"} {
			if v, ok := in[k].(string); ok && v != "" {
				text = v
				break
			}
		}
		files := patchFileRe.FindAllStringSubmatch(text, -1)
		if len(files) == 0 {
			return deny("the %s tool did not name any file the lane guard can check: use edit or write", p.ToolName)
		}
		for _, m := range files {
			if f := strings.Trim(m[1], `"`); f != "/dev/null" && !l.writable(f) {
				return deny("%s may not patch %s: %s", l.Role, f, l.describe())
			}
		}
		return nil
	}
	for _, path := range pathArgs(p.ToolInput) {
		if !l.writable(path) {
			return deny("unknown tool %s targets %s outside your lane: %s", p.ToolName, path, l.describe())
		}
	}
	return nil
}

var pathKeyRe = regexp.MustCompile(`(?i)path|file|dir|folder|target|dest|source|src|location|cwd`)

// pathArgs lists the path-looking string arguments of a tool call: values under a
// path-like key that contain a slash or start with ~ or . (so "file_type":"go" is not one).
func pathArgs(raw json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	add := func(v string) {
		if strings.ContainsAny(v, "/\\") || strings.HasPrefix(v, "~") || strings.HasPrefix(v, ".") {
			out = append(out, v)
		}
	}
	for _, k := range keys {
		if !pathKeyRe.MatchString(k) {
			continue
		}
		switch v := m[k].(type) {
		case string:
			add(v)
		case []any:
			for _, e := range v {
				if str, ok := e.(string); ok {
					add(str)
				}
			}
		}
	}
	return out
}

// NeutralJSON renders a lane-guard decision for a neutral plugin: nothing is
// printed for allow; deny prints {"decision":"deny","reason":...}.
func (d Decision) NeutralJSON() []byte {
	dec := "allow"
	if d.Deny {
		dec = "deny"
	}
	return []byte(fmt.Sprintf(`{"decision":%q,"reason":%q}`, dec, d.Reason))
}

// SaveSessionID persists the session id a harness reported, where the supervisor
// resumes from (.slai/run/<agent>/session-id). It never writes an empty id.
func SaveSessionID(project, agent, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	p := filepath.Join(project, ".slai", "run", agent, "session-id")
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == id {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(id+"\n"), 0o644)
}
