# Neutral hook payload and per-harness event mapping (T-0027)

(For `.slai/decisions.md`; engineers cannot write outside their worktree, so principal/orch copies this entry.)

- `slai hook <event> --agent <id> --project <dir>` and `slai mcp --agent <id> --project <dir>`: the flags win over `$SLAI_AGENT_ID` / `$SLAI_PROJECT`. Claude's generated mcp.json now passes both flags (hook commands keep `--agent`, project from the settings env).
- A plugin/extension posts JSON on stdin, all fields optional: `{event, tool, tool_input, session_id, cwd, usage:{prompt,output,cost}}`. A payload is *neutral* when it has an `event` field or the command-line event is a neutral name; Claude's own `stop` (no `event` field, `hook_event_name`) is unchanged. The command-line event wins over the payload's. `usage` is a **delta** (spend since the previous report), added with `AddUsage` on any event that carries it. A neutral `session_start` stores `session_id` in `.slai/run/<agent>/session-id` (how OpenCode's own ids are captured). The lane-guard reply is `{"decision":"deny","reason":...}` for neutral callers (nothing for allow), Claude's `hookSpecificOutput` for Claude.
- Mapping (asserted row by row in `neutral_test.go`):

| neutral | handled as | status effect |
|---|---|---|
| `session_start` | SessionStart | idle |
| `prompt` | UserPromptSubmit | working (turn counter, wake-landed check) |
| `tool` | PreToolUse | working (in-flight guard, lane guard) |
| `tool_end` | PostToolUse (`hook:posttooluse`) | working |
| `idle` | Notification `idle_prompt` | idle |
| `waiting` | Notification `permission_prompt` | waiting |
| `stop` | Stop | finished (was working) / idle |

- Per harness (what the plugin/extension calls): **Pi** `session_start`->session_start, `before_agent_start`->prompt, `tool_call`->tool, `tool_result`->tool_end, `agent_end`->stop. **OpenCode** `session.created`->session_start, `chat.message`->prompt, `tool.execute.before`->tool, `tool.execute.after`->tool_end, `session.idle`->stop, `permission.updated`->waiting. Pi has no `waiting`/`idle` source (it never asks; `agent_end` is its stop).

**Why:** non-Claude harnesses must not impersonate Claude's JSON, but the stuck detector, in-flight-tool guard and wake-landed check are written against Claude's events.
**How to apply:** a new harness only maps its events onto the seven neutral names; never add harness-specific cases to hooks.Handle.
