# slai — Product Requirements Document

Oct 19, 2026 · @Jarjar

## 1. Overview

slai (pronounced "slay") is a single-binary Go CLI that turns a coding directory into a managed AI software team. Running `slai` in a repo either walks you through building a team (first run) or opens a TUI where you talk to an orchestrator that plans, delegates, and ships work through role-specialized Claude Code instances, each running in its own tmux pane with its own model.

### Problem

Running several Claude Code sessions in parallel today means juggling terminal tabs, manually copying context between them, and hand-resolving branch collisions. Tools like [agent-manager](https://github.com/YoanWai/agent-manager) solve visibility (one TUI over many tmux sessions) but the agents are strangers: nobody plans, delegates, reviews, or merges.

### Goals (v1)

1. **One-command team setup.** `slai` in a fresh repo runs a wizard that writes `.slai/team.yaml`; no hand-editing required.
2. **Per-role models.** Every agent declares its model (e.g. orchestrator = Opus, principal = Fable, engineers = Sonnet, focus group = Haiku).
3. **Real coordination.** Agents exchange work through structured MCP tools backed by a shared task board, never by pasting text between panes.
4. **Full visibility.** A TUI shows every agent's status, live pane tail, the task board, and the message log; you can attach to any agent at any time.
5. **Safe parallelism.** Each engineer works in its own git worktree; nothing merges without principal review.
6. **Survives restarts.** Agents live in tmux and state lives in SQLite, so quitting the TUI or crashing loses nothing.

### Non-goals (v1)

- Supporting non-Claude agents (Codex, OpenCode). The tool adapter is kept clean so this is possible later.
- Windows-native support (WSL2 only, same as tmux).
- Cloud or remote execution; everything runs on the local machine.
- A web UI.
- Auto-pushing to remotes or opening PRs (v2).

## 2. User experience

The user only ever types `slai`; everything else is decided by whether `.slai/team.yaml` exists.

### 2.1 First run: team setup wizard

Triggered when `slai` runs in a directory with no `.slai/` (or via `slai init`). Built with Bubble Tea `huh` forms.

1. **Preflight.** Check `tmux`, `git`, and `claude` are on PATH and that the directory is a git repo (offer `git init` if not). Fail with a clear fix-it message for anything missing.
2. **Project basics.** Project name (default: folder name), one-paragraph description of what the product is (fed to every agent's prompt), main branch (auto-detected).
3. **Pick a preset or build custom.**
   - *Solo+* — orchestrator + 1 engineer.
   - *Squad* (default) — orchestrator, PM, principal, 2 engineers, 3-persona focus group.
   - *Full team* — orchestrator, PM, principal, 4 engineers, focus group.
   - *Custom* — add/remove roles one by one.
4. **Assign models per role.** A table of roles with a model dropdown each (`opus`, `sonnet`, `haiku`, `fable`, or a raw model string). Presets fill sensible defaults.
5. **Limits.** Max concurrent agents, max turns per task, review required (yes/no), permission mode.
6. **Review screen.** Shows the generated YAML; confirm to write `.slai/`, add `.slai/state.db` and `.slai/run/` to `.gitignore`, and open the main TUI.

Re-run anytime with `slai init --edit` to change the team; running agents are restarted only if their config changed.

### 2.2 Normal run: the main TUI

`slai` with an existing `.slai/team.yaml` starts (or reattaches to) the tmux server for this project, launches any agents that are not running, and opens the TUI with the orchestrator focused. The user types a request like "add Stripe billing with a usage-based plan" into the prompt bar.

### 2.3 Core flow: request to merged work

1. User sends request to the orchestrator.
2. Orchestrator calls `create_task` for the PM: "write a spec".
3. PM writes `.slai/specs/<task-id>.md` with acceptance criteria; optionally calls `ask` on focus-group personas for critique; calls `report(done)`.
4. Orchestrator reads the spec, splits it into engineering tasks with file/module ownership, assigns each with `create_task`.
5. Each engineer works in its own worktree on branch `slai/<agent>/<task-id>`, commits, and calls `request_review`.
6. Principal reviews the diff. It either `approve`s (slai merges into the integration branch) or `request_changes` (task goes back to the engineer with comments).
7. When all subtasks are merged, orchestrator reports a summary to the user. The user decides when to merge the integration branch into main.

At any point an agent can `ask` the user (status turns `waiting`, the TUI highlights it) or `ask` another agent.

### 2.4 CLI surface

| Command | What it does |
| --- | --- |
| `slai` | Init wizard if no team, else open TUI |
| `slai init [--edit]` | Run / re-run the team wizard |
| `slai up` / `slai down` | Start / stop all agent sessions without the TUI |
| `slai status` | Print agents, statuses, open tasks (scriptable, `--json`) |
| `slai send <agent> "msg"` | Send a user message to an agent |
| `slai attach <agent>` | Attach straight to an agent's tmux pane |
| `slai mcp` | (internal) stdio MCP server; launched by each agent |
| `slai hook <event>` | (internal) Claude Code hook handler; writes status |
| `slai doctor` | Diagnose tmux, claude, git, stale state |

## 3. Architecture

slai is one Go binary playing four parts: the TUI, a supervisor that owns tmux and state, a stdio MCP server each agent launches, and a hook handler Claude Code calls. All four share one SQLite database through WAL mode, so no long-lived daemon or socket is needed in v1.

&#91;embedded content: slai architecture · 4 parts, 5 roles\]

Agents never talk to each other directly: tool calls land as rows in SQLite, and the supervisor turns those rows into tmux wake-ups.

### 3.1 Components

| Component | Runs as | Responsibility |
| --- | --- | --- |
| TUI | `slai` (foreground) | Render agents, board, messages; send prompts; attach |
| Supervisor | goroutine inside the TUI, or `slai up` | Launch/revive agents, poll status, deliver wake-ups, enforce limits, run merges |
| MCP server | `slai mcp` (one per agent, stdio, spawned by Claude Code) | Expose team tools; read/write SQLite; identify caller via `SLAI_AGENT_ID` env |
| Hook handler | `slai hook <event>` (spawned by Claude Code hooks) | Write lifecycle status + token usage to SQLite |
| State | `.slai/state.db` (SQLite, WAL) | Agents, tasks, messages, reviews, events, usage |

Only the supervisor writes to tmux. The MCP server and hook handler never touch tmux; they write rows, and the supervisor reacts.

### 3.2 Launching an agent

Each agent is an interactive `claude` process in its own tmux session, named `slai_<project-hash>_<agent-id>`. For every agent the supervisor generates three files into `.slai/run/<agent-id>/` and launches:

```bash
cd <worktree-or-repo-root> && SLAI_AGENT_ID=eng-1 SLAI_PROJECT=<abs path> \
  claude --model sonnet \
    --append-system-prompt "$(cat .slai/run/eng-1/system.md)" \
    --mcp-config .slai/run/eng-1/mcp.json \
    --settings .slai/run/eng-1/settings.json \
    --permission-mode <from team.yaml>
```

- `system.md` — role prompt (section 5) rendered with project description, teammates, and rules.
- `mcp.json` — registers one server: `{"slai": {"command": "slai", "args": ["mcp"]}}`.
- `settings.json` — hooks for `UserPromptSubmit`, `PreToolUse`, `Notification`, `Stop`, `SessionStart`, each running `slai hook <event>`; plus allowed/denied tools per role.

**Implementation note for Claude Code:** verify every flag above against `claude --help` on the installed version before coding; flag names have changed between releases. Keep all flag construction in one `internal/claude/launch.go` so it is a one-file fix.

Revive uses `claude --continue` in the same directory, so an agent resumes its conversation after a crash or `slai down`.

### 3.3 Status detection

Primary source: hooks. `UserPromptSubmit` / `PreToolUse` → `working`; `Notification` (permission or input needed) → `waiting`; `Stop` → `idle` (or `finished` until the user views it). Fallback: the supervisor polls `tmux capture-pane` every 2s and applies regex rules (as agent-manager does) when hook data is older than 30s. Dead tmux session → `dead`.

### 3.4 Messaging and wake-ups

Agents never type into each other's panes. A message or task assignment is a row in SQLite. The supervisor watches for undelivered rows addressed to an agent and, when that agent is `idle`, injects a single short line with `tmux send-keys`:

> `[slai] You have 2 new items. Call get_inbox.`

If the agent is `working`, a PostToolUse hook can add a short notice about claimable mail to its current turn; no pane input is sent. Otherwise the wake-up waits until it is safe to type. User quick prompts also enter the inbox before the supervisor wakes the recipient. This keeps panes readable and context small: the agent pulls the details through the `get_inbox` tool. Engineer assignments are held out of the inbox until the supervisor prepares their task branch; dependency-blocked assignments are readable as explicit wait notices, while startable work queued behind an active task stays held until its branch is selected. The precise cross-component delivery contract is in `ARCHITECTURE.md`.

### 3.5 Git worktrees and merging

- On init, slai creates an integration branch `slai/integration` from main.
- Each engineer gets a persistent worktree at `.slai/worktrees/<agent-id>` (gitignored). Each task gets a fresh branch from the current integration head: `slai/<agent-id>/<task-id>`.
- Orchestrator, PM, principal, and focus group run in the repo root, read-only for code (enforced by tool permissions).
- On principal `approve`, the supervisor merges the task branch into `slai/integration` with `--no-ff`. On conflict, it aborts the merge and reassigns the task to the same engineer with the conflict list.
- The user merges `slai/integration` into main manually (v1).

## 4. Team config and .slai layout

`.slai/team.yaml` is the single source of truth for the team and is meant to be committed. Everything runtime lives next to it and is gitignored.

### 4.1 Folder layout

```text
.slai/
  team.yaml            # committed: the team definition
  prompts/             # committed: optional per-role prompt overrides (<role>.md)
  specs/               # committed: PM specs, one per task (<task-id>.md)
  decisions.md         # committed: append-only architecture decision log (principal)
  state.db             # gitignored: SQLite (WAL)
  run/<agent-id>/      # gitignored: generated system.md, mcp.json, settings.json, pane.log
  worktrees/<agent-id>/# gitignored: engineer worktrees
```

### 4.2 team.yaml schema

```yaml
version: 1
project:
  name: my-app
  description: >
    B2B invoicing SaaS. Next.js frontend, Go API, Postgres.
  main_branch: main
  integration_branch: slai/integration

defaults:
  permission_mode: default      # passed to claude --permission-mode
  max_turns_per_task: 40
  max_concurrent_agents: 5
  require_review: true

models:                         # aliases → values passed to --model
  opus: opus
  sonnet: sonnet
  haiku: haiku
  fable: claude-fable-5-1

agents:
  - id: orch
    role: orchestrator
    model: opus
  - id: pm
    role: product_manager
    model: sonnet
  - id: principal
    role: principal_engineer
    model: fable
  - id: eng-1
    role: engineer
    model: sonnet
    focus: backend              # optional hint used by the orchestrator when assigning
  - id: eng-2
    role: engineer
    model: sonnet
    focus: frontend
  - id: fg-newbie
    role: focus_group
    model: haiku
    persona: First-time user, non-technical small-business owner
  - id: fg-power
    role: focus_group
    model: haiku
    persona: Accountant who processes 500 invoices a month
```

### 4.3 Validation rules

- Exactly one `orchestrator`. At most one `principal_engineer` and one `product_manager`.
- `require_review: true` requires a `principal_engineer`.
- Agent ids: `^[a-z][a-z0-9-]{1,23}$`, unique.
- `model` must be a key in `models` or a raw string starting with `claude-`.
- Unknown keys are an error, not a warning (catches typos).
- Config is parsed into Go structs in `internal/config` with a `Validate()` that returns all errors at once.

## 5. Roles

Each role is a prompt template plus a tool allowlist. Templates live in `internal/roles/templates/<role>.md` (embedded with `go:embed`) and can be overridden per project in `.slai/prompts/<role>.md`.

| Role | Default model | Works in | Can edit code | slai tools |
| --- | --- | --- | --- | --- |
| Orchestrator | opus | repo root | No | create\_task, update\_task, get\_board, send\_message, ask\_user, get\_inbox, cancel\_task |
| Product manager | sonnet | repo root | Only `.slai/specs/` | get\_board, report, send\_message, ask, ask\_user, get\_inbox |
| Principal engineer | fable | repo root | Only `.slai/decisions.md` | get\_board, review\_diff, approve, request\_changes, send\_message, ask, get\_inbox |
| Engineer | sonnet | own worktree | Yes, own worktree only | get\_task, report, request\_review, ask, send\_message, get\_inbox |
| Focus group | haiku | repo root | No (read-only) | get\_inbox, reply, get\_board |

### 5.1 Shared prompt preamble (every role)

- Who you are (id, role), who your teammates are (ids, roles, focus), and the project description.
- You coordinate only through `slai` tools. Never assume a teammate saw something unless you sent it.
- When you see `[slai] You have N new items`, call `get_inbox` first.
- Keep messages short: what, why, where (file paths / branch), open questions. Link to files rather than pasting them.
- When finished with your current item, call `report` (or the role's equivalent), then stop.

### 5.2 Role-specific rules

**Orchestrator.** Owns the plan and the user relationship. Never writes code. For each user request: get a spec from the PM (skip for trivial asks), split into tasks with explicit file/module ownership so no two engineers touch the same files, assign, track via `get_board`, and summarize to the user when done. Escalates to the user with `ask_user` instead of guessing on product decisions.

**Product manager.** Turns a request into `.slai/specs/<task-id>.md` with: problem, user stories, acceptance criteria (testable, numbered), out of scope, open questions. May `ask` focus-group personas for reactions before finalizing.

**Principal engineer.** Quality gate and architecture owner. On review: reads the diff via `review_diff`, checks it against the spec's acceptance criteria, runs tests if present, then `approve` or `request_changes` with specific, file-and-line comments. Records non-obvious architecture decisions in `.slai/decisions.md`. Engineers may `ask` it design questions before building.

**Engineer.** Implements exactly one task at a time in its worktree. Reads the task and spec, writes code and tests, commits with message `[<task-id>] <summary>`, runs the test suite, then `request_review`. If blocked for more than a few attempts, uses `ask` instead of thrashing.

**Focus group persona.** Answers as its `persona` from team.yaml. Gives candid user-perspective reactions to specs or descriptions of features: what is confusing, what is missing, what they would pay for. Never proposes implementation.

## 6. MCP tools and data model

The `slai mcp` server is the team's only communication channel. It identifies the caller from `SLAI_AGENT_ID`, rejects tools the caller's role may not use, and writes an `events` row for every call (that row feeds the TUI's activity log). Use the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`) over stdio.

### 6.1 Tool reference

| Tool | Input | Effect |
| --- | --- | --- |
| `get_inbox` | — | Returns and marks delivered all unread messages and task assignments for the caller |
| `get_board` | `status?` | All tasks: id, title, assignee, status, branch, parent |
| `get_task` | `task_id` | Full task: description, spec path, acceptance criteria, history, review comments |
| `create_task` | `title, description, assignee, parent_id?, spec_path?, files?` | New task in `assigned`; queues an inbox item for the assignee |
| `update_task` | `task_id, fields` | Edit title, description, assignee, files |
| `cancel_task` | `task_id, reason` | Moves to `cancelled`; notifies assignee |
| `report` | `task_id, status (done, blocked), summary` | Updates task; notifies orchestrator |
| `request_review` | `task_id, summary` | Verifies branch has commits; moves task to `in_review`; notifies principal |
| `review_diff` | `task_id` | Returns `git diff integration...task-branch` (truncated at 2,000 lines with a file list) |
| `approve` | `task_id, notes?` | Supervisor merges branch; task → `merged` (or back to `in_progress` on conflict) |
| `request_changes` | `task_id, comments` | Task → `changes_requested`; increments `review_rounds`; notifies engineer |
| `send_message` | `to, body` | Free-form note to one agent or `all` |
| `ask` | `to, question` | Like send\_message but expects a `reply`; asker sees it in its next inbox |
| `reply` | `message_id, body` | Answers an `ask` |
| `ask_user` | `question` | Marks caller `waiting_on_user`; TUI highlights it; the user's answer arrives as an inbox item |

All tools return compact JSON. Text fields are capped (body ≤ 4,000 chars) to force agents to reference files instead of pasting them.

### 6.2 Task lifecycle

`assigned → in_progress → in_review → merged`, with side exits: `in_review → changes_requested → in_progress`, any → `blocked` (returns to `in_progress` when unblocked), any → `cancelled`. A task moves to `in_progress` automatically on the assignee's first `get_task`.

### 6.3 SQLite schema (v1)

```sql
CREATE TABLE agents (
  id TEXT PRIMARY KEY, role TEXT, model TEXT,
  status TEXT,              -- working|waiting|waiting_on_user|idle|finished|errored|dead
  status_at INTEGER, tmux_session TEXT, worktree TEXT,
  current_task TEXT, turns INTEGER DEFAULT 0
);
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,      -- T-0001
  parent_id TEXT, title TEXT, description TEXT,
  assignee TEXT, created_by TEXT, status TEXT,
  spec_path TEXT, branch TEXT, files TEXT,   -- files: JSON array
  review_rounds INTEGER DEFAULT 0,
  created_at INTEGER, updated_at INTEGER
);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY, from_agent TEXT, to_agent TEXT,
  kind TEXT,                -- note|ask|reply|assignment|review|user
  task_id TEXT, reply_to INTEGER, body TEXT,
  created_at INTEGER, delivered_at INTEGER, woken_at INTEGER
);
CREATE TABLE events (
  id INTEGER PRIMARY KEY, at INTEGER, agent TEXT,
  kind TEXT, data TEXT      -- JSON
);
CREATE TABLE usage (
  agent TEXT, at INTEGER, input_tokens INTEGER,
  output_tokens INTEGER, cost_usd REAL
);
```

Use `modernc.org/sqlite` (pure Go, no cgo) so the binary cross-compiles cleanly. Migrations are numbered SQL files embedded with `go:embed`.

## 7. TUI

The TUI has three panes and a prompt bar, built with Bubble Tea, Lip Gloss, and Bubbles. It never blocks on tmux or SQLite: all I/O runs in commands that return messages.

### 7.1 Layout

```text
┌ slai · my-app ───────── 5 agents · 2 working · 1 waiting · $3.42 today ┐
│ TEAM              │ eng-1 · sonnet · working · T-0004 Add Stripe webhook │
│ ● orch     idle   │                                                       │
│ ● pm       idle   │   (live tail of the selected agent's tmux pane,       │
│ ◆ principal review│    refreshed every 1s while selected)                 │
│ ● eng-1    working│                                                       │
│ ▲ eng-2    waiting│                                                       │
│ ○ fg-newbie idle  │                                                       │
├───────────────────┼───────────────────────────────────────────────────────┤
│ BOARD  (tab: LOG) │ T-0003 in_review  eng-2  Invoice list UI     r1       │
│                   │ T-0004 in_progress eng-1 Stripe webhook               │
├───────────────────┴───────────────────────────────────────────────────────┤
│ > to orch: _                                                     [tab] ↹  │
└───────────────────────────────────────────────────────────────────────────┘
```

- **Header:** project, agent counts by status, today's spend.
- **Team (left):** one row per agent: status glyph and colour, id, status. Selecting a row drives the preview.
- **Preview (right):** header with model, status, and current task; body is `tmux capture-pane -p -e` of the selected agent (ANSI preserved).
- **Bottom panel:** toggles between **Board** (tasks grouped by status) and **Log** (events stream: "eng-1 → principal: request\_review T-0004").
- **Prompt bar:** sends to the selected agent (default: orchestrator). Text goes in via `send-keys` and is also logged as a `user` message.

### 7.2 Keybindings

| Key | Action |
| --- | --- |
| `↑ ↓` / `j k` | Move selection in the focused pane |
| `tab` | Cycle focus: team → bottom panel → prompt |
| `enter` | On an agent: attach to its tmux pane (`ctrl+q` detaches back) |
| `i` or `/` | Focus prompt bar for the selected agent |
| `b` / `l` | Bottom panel: Board / Log |
| `t` | On a task: open detail (description, spec, history, review comments) |
| `d` | On a task in review: full-screen diff of its branch |
| `r` | Revive a dead agent (`claude --continue`) |
| `x` | Interrupt the selected agent (sends `Esc`) |
| `p` | Pause / resume the team (supervisor stops delivering wake-ups) |
| `?` | Help |
| `q` | Quit TUI (agents keep running in tmux) |

### 7.3 Attention model

Any agent in `waiting` or `waiting_on_user` shows ▲ in amber, sorts to the top of the team list, and rings the terminal bell once. A `finished` turn shows a dot until the user selects that agent. This is how the user knows where they are needed without watching every pane.

## 8. Safety, limits, and guardrails

Autonomous agents in parallel fail in predictable ways: they burn money, loop, collide, and act outside their lane. Every guard below is enforced by slai's code, not just by prompts.

| Risk | Guard | Enforced in |
| --- | --- | --- |
| Runaway spend | Per-agent and daily spend caps from `usage`; at 80% warn in header, at 100% pause wake-ups and `ask_user` | supervisor |
| Too many live agents | `max_concurrent_agents`: idle agents beyond the cap are not woken until a slot frees | supervisor |
| Review ping-pong | `review_rounds` ≥ 3 on a task → task goes `blocked` and orchestrator plus user are notified | MCP `request_changes` |
| Message loops | More than 10 messages between the same two agents in 10 minutes → pause both, `ask_user` | supervisor |
| Long-running task | Turns on a task > `max_turns_per_task` → engineer is told to `report(blocked)` | hook handler |
| Agent edits outside its lane | Role tool allowlists in generated `settings.json`; engineers' cwd is their worktree; PM/principal write paths restricted | launch config |
| Branch collisions | One worktree per engineer, one branch per task, merges serialized by a mutex in the supervisor | supervisor |
| Destructive shell commands | Default `permission_mode` keeps Claude Code's prompts; deny `git push`, `git reset --hard`, `rm -rf` outside the worktree in settings | launch config |
| Lost state on crash | SQLite WAL; tmux sessions outlive the TUI; `claude --continue` on revive | design |

### 8.1 Permissions

Default to Claude Code's normal permission prompts for every role. A pending permission prompt shows as `waiting`, so the user approves it by attaching (`enter`). The wizard offers a looser mode for engineers only, scoped to their worktree, with a clear warning. slai never passes a flag that skips all permissions by default.

### 8.2 Cost visibility

The hook handler records token usage per turn into `usage`. Cost is computed from a price table in `internal/usage/prices.go`; the user can override prices in team.yaml. **Note for implementation:** confirm what usage data the installed Claude Code exposes to hooks. If token counts are not available there, fall back to parsing the session transcript JSONL that hooks point to.

## 9. Build plan

Build in five milestones; each one ends in something you can run and demo. Do not start a milestone until the previous one's acceptance criteria pass.

### 9.1 Tech stack

| Concern | Choice |
| --- | --- |
| Language | Go 1.23+ |
| CLI | `spf13/cobra` |
| TUI | `charmbracelet/bubbletea`, `lipgloss`, `bubbles`, `huh` (forms) |
| Config | `gopkg.in/yaml.v3` with `KnownFields(true)` |
| State | `modernc.org/sqlite` (WAL mode) |
| MCP | `modelcontextprotocol/go-sdk` (stdio server) |
| Processes | `tmux` via `os/exec` (wrap in `internal/tmux`) |
| Git | `git` CLI via `os/exec` (wrap in `internal/git`) |
| Release | GoReleaser, Homebrew tap |

### 9.2 Repo structure

```text
slai/
  main.go
  cmd/                 # cobra commands: root, init, up, down, status, send, attach, mcp, hook, doctor
  internal/
    config/            # team.yaml structs, load, validate, presets
    store/             # SQLite open, migrations/, typed queries
    tmux/              # new-session, send-keys, capture-pane, has-session, kill
    git/               # worktree add/remove, branch, diff, merge
    claude/            # launch.go: build flags + run files; revive
    roles/             # templates/*.md (go:embed), render, tool allowlists
    mcpserver/         # tool definitions + handlers, role checks
    hooks/             # hook event → status/usage writes
    supervisor/        # agent lifecycle, wake-ups, merges, guards
    usage/             # price table, cost calc
    tui/               # model.go, team.go, preview.go, board.go, log.go, prompt.go, wizard/
  testdata/
  .goreleaser.yaml
```

### 9.3 Milestones

**M1 — Skeleton and launcher.** Config, wizard, and launching role-configured agents in tmux.

- [ ] `slai init` wizard writes a valid `team.yaml` for all four presets
- [ ] `slai up` creates one tmux session per agent with the right model, cwd, and generated `system.md`
- [ ] `slai attach eng-1` drops into that agent's pane; `slai down` kills all sessions
- [ ] Engineer worktrees and the integration branch are created
- [ ] `slai doctor` reports missing tmux/claude/git

**M2 — MCP server and messaging (the proof of concept).** Agents can coordinate.

- [ ] `slai mcp` serves all section 6 tools with role checks
- [ ] Hook handler writes status; `slai status` shows live statuses
- [ ] Supervisor delivers wake-ups to idle agents only
- [ ] **Demo:** orchestrator + 2 engineers complete a 2-task feature in a sample repo with no human message after the initial request

**M3 — TUI.** See and steer everything.

- [ ] Team list, live preview, board/log panel, prompt bar per section 7
- [ ] Attach/detach round-trip with `enter` / `ctrl+q`
- [ ] Waiting agents float to the top with the bell
- [ ] TUI quit and restart loses nothing

**M4 — Review and merge.** Quality gate.

- [ ] `request_review` → principal `review_diff` → `approve` merges to integration
- [ ] `request_changes` round-trips to the engineer with comments
- [ ] Merge conflicts send the task back with the conflict list
- [ ] Diff view (`d`) in the TUI

**M5 — PM, focus groups, and guards.** The full team, safely.

- [ ] PM writes specs to `.slai/specs/`; focus personas answer `ask`s
- [ ] Every guard in section 8 works and has a test
- [ ] Spend shown in header; caps pause the team
- [ ] GoReleaser builds macOS and Linux binaries

### 9.4 Testing

- **Unit:** config validation, task state machine, role permission matrix, guard thresholds, flag construction in `launch.go` (golden files).
- **Integration:** a real tmux server in a temp dir; a fake `claude` binary (`testdata/fakeclaude`) that echoes input, calls `slai hook`, and invokes MCP tools from a script. This tests the whole loop without spending tokens.
- **End-to-end (manual, per milestone):** the M2 demo against real Claude Code on `testdata/sample-repo`.

### 9.5 How to hand this to Claude Code

Save this doc as `PRD.md` at the root of a new `slai` repo. Then start Claude Code there and work one milestone at a time:

> Read PRD.md. Implement milestone M1 only. Before writing launch code, run `claude --help` and adapt section 3.2's flags to what this version supports. Write tests as you go. Stop when every M1 checkbox passes and tell me how to verify it.

## 10. Open questions and risks

### Open questions

- [ ] **Subscription vs API key.** Will users run slai on a Claude subscription (shared usage limits across all agents) or API keys (per-token cost)? This changes the cost display and the default team size. Check Anthropic's current terms for running multiple Claude Code instances programmatically.
- [ ] **Fable access.** Confirm the user's plan includes the Fable model; the wizard should test each chosen model with a one-line `claude -p` call and fall back with a warning if one fails.
- [ ] **Auto-merge to main.** Should v2 open a PR from `slai/integration` instead of leaving it to the user?
- [ ] **Headless roles.** Focus-group personas could run as one-shot `claude -p` calls instead of always-on panes, saving resources. Decide after M2 based on how often they are used.
- [ ] **Native Claude Code multi-agent features.** Review what Claude Code now offers built-in (subagents and any team features) and decide whether slai builds on it or stays independent.

### Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Claude Code CLI flags or hook payloads change | Launch or status breaks | All flag construction in one file; `slai doctor` checks the version; integration tests with fake claude |
| Agents ignore role rules in prompts | Out-of-lane edits, chaos | Enforce with tool allowlists and cwd, not prompts alone |
| Wake-up injection lands mid-typing when the user is attached | Garbled input | Skip wake-ups for agents the user is attached to; deliver on detach |
| Context bloat from long conversations | Quality drops, cost rises | Agents reference files; orchestrator can restart an agent fresh between tasks (`/clear` via send-keys) |
| Usage limits hit mid-feature | Team stalls | Header shows usage; caps pause cleanly; everything resumes with `claude --continue` |
