# Which command do I use?

slai has two kinds of commands, and they share some names:

- **CLI commands** are typed by you, the human, in a terminal: `slai send`, `slai status`, ...
- **MCP tools** are called by the agents inside their own Claude sessions: `send_message`, `ask`, `get_inbox`, ... You never type these. You can see their effect (messages, tasks) in the dashboard and `slai status`.

Terms such as *orch*, *dashboard*, *worktree* and *integration branch* are defined in the [README glossary](../README.md#glossary).

## Talking to the team

| Name | Who calls it | What it does | Example |
| --- | --- | --- | --- |
| `slai send <agent> "msg"` | **You** (human CLI) | Queues a message from you to one agent; it shows up in that agent's inbox. Normally send to `orch`. | `slai send orch "add a --verbose flag to the CLI"` |
| `send_message` | **Agent** (MCP tool; orchestrator, PM, principal, engineer) | Agent-to-agent note to one agent id, or `all` (everyone except focus-group personas). One-way: no answer is expected. | `send_message {"to": "eng-1", "body": "T-0003 is merged, rebase before you continue"}` |
| `ask` | **Agent** (MCP tool; PM, principal, engineer) | Asks another agent a question. The asker keeps working; the answer arrives in its inbox. | `ask {"to": "principal", "question": "Is a new flag OK here, or should this go in team.yaml?"}` |
| `reply` | **Agent** (MCP tool; every role) | Answers an `ask` that was sent to it. Not a CLI command: you cannot `slai reply`. To answer an agent yourself, use `slai send`. | `reply {"message_id": 12, "body": "Put it in team.yaml."}` |
| `ask_user` | **Agent** (MCP tool; orchestrator, PM) | Asks **you** a question and parks that agent as waiting on you. The dashboard shows it as `awaiting_user`; answer it with the `space` prompt in the dashboard or with `slai send <agent> "answer"`. | `ask_user {"question": "Should the CLI keep supporting Go 1.21?"}` |
| `get_inbox` | **Agent** (MCP tool; all roles) | Returns and marks as read the agent's unread messages and task assignments. The pane line `[slai] You have N new items` is slai telling the agent to call it; you need do nothing. | `get_inbox {}` |

## Looking at the team

| Name | Who calls it | What it does | Example |
| --- | --- | --- | --- |
| `slai status` | **You** (human CLI) | Prints the agents and open tasks, and whether the supervisor is running. `--json` for scripts, `--stats` for per-agent merge, rework and spend numbers. | `slai status` |
| `get_board` | **Agent** (MCP tool; orchestrator, PM, principal, focus group) | The agents' version of `slai status`: tasks with assignee, status and dependencies, plus each agent's liveness. Engineers do not have it; they use `get_task`. | `get_board {}` |
| `get_task` | **Agent** (MCP tool; engineer, PM) | Full details of the agent's own task (description, spec path, review comments). | `get_task {}` |
| `slai` (no arguments) | **You** (human CLI) | Starts any missing agent sessions and the supervisor, then opens the dashboard, where you talk to `orch`. | `slai` |
| `slai attach <agent>` | **You** (human CLI) | Opens that agent's tmux pane to watch it. | `slai attach eng-1` |

## Controlling and finishing

| Name | Who calls it | What it does | Example |
| --- | --- | --- | --- |
| `slai up` | **You** (human CLI) | Like `slai` but headless: starts agents and the supervisor without opening the dashboard. | `slai up` |
| `slai hold` / `slai resume` | **You** (human CLI) | Stop the whole team from being woken (e.g. overnight) / lift it. | `slai hold` |
| `slai priority <task> <n\|up\|down\|top>` | **You** (human CLI) | Sets a task's priority; higher runs first. | `slai priority T-0004 top` |
| `slai commit` | **You** (human CLI) | Previews, then commits the team's finished work onto your current branch. | `slai commit` |
| `slai down` / `slai clean` | **You** (human CLI) | Stop everything / also remove slai's runtime state. | `slai down` |
| `report` | **Agent** (MCP tool; engineer, PM) | Tells `orch` a task is done or blocked. | `report {"status": "done", "summary": "docs/commands.md added"}` |
| `request_review` | **Agent** (MCP tool; engineer) | Hands a committed task branch to the principal for review. | `request_review {"task_id": "T-0006", "summary": "..."}` |
| `approve` / `request_changes` | **Agent** (MCP tool; principal) | Principal's verdict on a task in review. | `approve {"task_id": "T-0006"}` |
| `create_task` / `update_task` / `cancel_task` / `finish` | **Agent** (MCP tool; orchestrator) | Manage the board and declare your request complete. | `create_task {"title": "...", "assignee": "eng-1", "description": "..."}` |

## Quick answers

- **I want to tell an agent something:** `slai send <agent> "..."`.
- **An agent is asking me something:** answer with `slai send <agent> "..."` or in the dashboard. There is no `slai reply`.
- **I want to see what is going on:** `slai status`, or the dashboard (`slai`).
- **Why does a pane say `[slai] You have 2 new items`?** An agent was nudged to call `get_inbox`. It is machine traffic.

Every CLI command and its flags: `slai --help` and `slai <command> --help`. The full list of agent tools per role is in the README under [Roles and tools](../README.md#roles-and-tools).
