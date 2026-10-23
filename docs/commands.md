# Which command do I use?

slai has two kinds of commands, and they share some names:

- **CLI commands** are typed by you, the human, in a terminal: `slai send`, `slai status`, ...
- **MCP tools** are called by the agents inside their own Claude sessions: `send_message`, `ask`, `get_inbox`, ... You never type these. You can see their effect (messages, tasks) in the dashboard and `slai status`.

Terms such as *orch*, *dashboard*, *worktree* and *integration branch* are defined in the [README glossary](../README.md#glossary).

## Commands you type

| Name | What it does | Example |
| --- | --- | --- |
| `slai` (no arguments) | Starts any missing agent sessions and the supervisor, then opens the dashboard, where you talk to `orch`. With no `.slai/team.yaml` yet it runs `slai init` first. | `slai` |
| `slai send <agent\|group> "msg"` | Queues a message from you to one agent, or to a group (`all`, or a role: `orchestrator`, `product-manager`, `principal-engineer`, `engineers`, `focus-group`; one message per member, and it prints who it was queued for). Normally send to `orch`. It is the free-form path; to answer a specific question an agent asked you, use `slai reply`. | `slai send orch "add a --verbose flag to the CLI"` |
| `slai reply <ask-id> "answer"` | Answers one specific question an agent asked you (`ask_user`). The ask id is printed next to the waiting agent in `slai status` and shown on the web inbox. The answer goes to the asking agent linked to the ask, and the web inbox shows the question as answered. Refuses an unknown id or a question already answered. (Not the same as the agent-side `reply` MCP tool below.) | `slai reply 17 "use the second option"` |
| `slai status` | Prints the agents and open tasks, and whether the supervisor is running. `--json` for scripts, `--stats` for per-agent merge, rework and spend numbers. | `slai status` |
| `slai attach <agent>` | Opens that agent's tmux pane to watch it. | `slai attach eng-1` |
| `slai up` | Like `slai` but headless: starts agents and the supervisor without opening the dashboard. | `slai up` |
| `slai hold` / `slai resume` | Stop the whole team from being woken (e.g. overnight) / lift it. | `slai hold` |
| `slai ground <agent>` | Reminds an agent who it is: queues its identity line (name, role, what it is for), and the next wake-up opens with it, typed only when the agent is idle and its pane is safe. Asking again while one is still waiting changes nothing. `slai status` shows when each agent was last re-grounded (`last_ground_at` in `--json`) and any open identity correction. | `slai ground eng-1` |
| `slai priority <task> <n\|up\|down\|top>` | Sets a task's priority; higher runs first. | `slai priority T-0004 top` |
| `slai commit` | Previews, then commits the team's finished work onto your current branch. | `slai commit` |
| `slai down` / `slai clean` | Stop everything / also remove slai's runtime state. | `slai down` |
| `slai doctor` | Diagnoses tmux, claude, git and stale state. | `slai doctor` |

`slai reply` (above) is for you; the `reply` tool below is the agent-side one. Every CLI command and its flags: `slai --help` and `slai <command> --help`.

## Agent-only: you never type these

These are MCP tools. Agents call them inside their own Claude sessions; you see their effect in the dashboard and in `slai status`. Roles are from `internal/roles/roles.go`; `get_inbox`, `reply`, `pong`, `recall` and `get_memory` are available to every role.

| Name | Who may call it | What it does | Example |
| --- | --- | --- | --- |
| `get_inbox` | all roles | Returns and marks as read the agent's unread messages and task assignments. The pane line `[slai] You have N new items` is slai telling the agent to call it; you need do nothing. | `get_inbox {}` |
| `send_message` | orchestrator, PM, principal, engineer | Note to one agent id, or `all` (everyone except you and focus-group personas). One-way: no answer is expected. | `send_message {"to": "eng-1", "body": "T-0003 is merged, rebase before you continue"}` |
| `ask` | PM, principal, engineer | Asks another agent a question. The asker keeps working; the answer arrives in its inbox. | `ask {"to": "principal", "question": "Is a new flag OK here, or should this go in team.yaml?"}` |
| `reply` | all roles (the only tool a focus group has for speaking) | Answers an `ask` that was sent to that agent. | `reply {"message_id": 12, "body": "Put it in team.yaml."}` |
| `ask_user` | orchestrator, PM | Asks **you** a question and parks that agent as waiting on you. The dashboard shows "<agent> is waiting on you" and the board lists the agent as blocked; answer it with the `space` prompt in the dashboard, with `slai reply <ask-id> "answer"` (the id is in `slai status`), or with `slai send <agent> "answer"`. | `ask_user {"question": "Should the CLI keep supporting Go 1.21?"}` |
| `get_board` | orchestrator, PM, principal, focus group | The agents' version of `slai status`: tasks with assignee, status and dependencies, plus each agent's liveness. Engineers do not have it; they use `get_task`. | `get_board {}` |
| `get_task` | engineer, PM | Full details of the agent's own task (description, spec path, review comments). | `get_task {}` |
| `report` | engineer, PM | Tells `orch` a task is done or blocked. | `report {"status": "done", "summary": "docs/commands.md added"}` |
| `request_review` | engineer | Hands a committed task branch to the principal for review. | `request_review {"task_id": "T-0006", "summary": "..."}` |
| `review_diff` | principal | Shows the diff of a task branch in review. | `review_diff {"task_id": "T-0006"}` |
| `approve` / `request_changes` | principal | The principal's verdict on a task in review. | `approve {"task_id": "T-0006"}` |
| `create_task` / `update_task` / `cancel_task` / `finish` | orchestrator | Manage the board and declare your request complete. | `create_task {"title": "...", "assignee": "eng-1", "description": "..."}` |
| `ping` | orchestrator | Checks that another agent is alive and answering. | `ping {"agent": "eng-1"}` |
| `pong` | all roles | Answers a `ping`. | `pong {"message_id": 7, "agent_id": "eng-1"}` |
| `remember` / `recall` / `get_memory` | `remember`: orchestrator, PM, principal, engineer; `recall`, `get_memory`: all roles | Write and read the team's shared fact memory (see [docs/memory.md](memory.md)). | `recall {"query": "wake tick"}` |

## Quick answers

- **I want to tell an agent (or a group) something:** `slai send <agent|group> "..."`, e.g. `slai send engineers "..."`.
- **An agent is asking me something:** answer with `slai reply <ask-id> "..."` (the id is shown in `slai status` and on the web inbox), or in the dashboard. `slai send` also reaches the agent, but nothing records it as the answer: the web inbox keeps listing the question until the agent reads your message and moves on, and never shows what you answered.
- **I want to see what is going on:** `slai status`, or the dashboard (`slai`).
- **Why does a pane say `[slai] You have 2 new items`?** An agent was nudged to call `get_inbox`. It is machine traffic.

Every CLI command and its flags: `slai --help` and `slai <command> --help`. The full list of agent tools per role is in the README under [Roles and tools](../README.md#roles-and-tools).
