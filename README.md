# slai

> **What it is:** slai runs a team of Claude Code agents (an orchestrator, engineers, optionally a principal reviewer) on one git repo, each a long-lived session in its own tmux pane (tmux: a terminal multiplexer that keeps sessions running in the background) with its own model, harness and permission settings.  
> **Who it is for:** a developer who wants to hand a multi-part goal to a team and review what lands, not to drive one agent turn by turn.  
> **Claude Code subagents / agent teams** run inside one Claude Code session on one checkout; slai gives every engineer its own worktree and one branch per task (a worktree is a second checkout of your repo in its own directory), plus a task board in SQLite that survives restarts and spend caps (per agent, day, task and run).  
> **Plain tmux + worktrees** is what you would wire up by hand; slai adds the shared task board, a principal review gate (an engineer's work merges only after the principal engineer approves it, criterion by criterion), merge to `slai/integration` behind your `project.test_command`, and liveness checks that revive dead agents.  
> **What it is not:** it does not touch your branches or remote until you run `slai commit`, and it never passes `--dangerously-skip-permissions`. New here? Start at [First 10 minutes](#first-10-minutes).

Single-binary Go CLI that turns a git repo into a managed AI software team. **You only talk to the orchestrator (`orch`); every other pane is agents talking to each other, so you can ignore it.** Role-specialised Claude Code agents (orchestrator, product manager, principal engineer, engineers, focus group) each run in their own tmux session with their own model, coordinate through an MCP tool server (MCP is the protocol Claude Code uses to call tools; here the tools are slai's) backed by SQLite, and ship work through git worktrees and a principal-review gate. See `PRD.md` for the full design.

## Install

Requires `tmux`, `git`, and [Claude Code](https://docs.claude.com/claude-code) on PATH (Linux, macOS, or WSL2). **There are no prebuilt releases yet, so Go is required to install** (`go install` below, or `go build` in a clone). The Homebrew line works only once the first release is published.

```bash
go install github.com/oorbt/slai@latest        # or: brew install oorbt/tap/slai (after the first release)
slai doctor                                      # verify the toolchain
```

## Quickstart

```bash
cd my-repo
slai                 # first run: wizard writes .slai/team.yaml; then opens the dashboard
slai send orch "build X"   # type your goal to the orchestrator; or a group: engineers, all
slai status --json   # scriptable state
slai serve           # the team in a browser (local web UI)
slai status --stats [--since 7d] [--json]   # merges, rework, review rounds and spend per agent and per harness/model
slai down            # stop supervisor and all agent sessions
```

## First 10 minutes

From nothing to one merged task. There is one start path below: `slai init --yes --preset ...`. (If you run bare `slai` in a repo with no `.slai/team.yaml` it opens a setup wizard that asks the same questions.) Commands you type start with `$`.

Pick a preset; this walk-through uses `solo`:

| Preset | Starts | Review gate |
| --- | --- | --- |
| `solo` | 2 Claude sessions: `orch` and `eng-1` | none: no principal, so finished work merges without review |
| `squad` (default) | 8: `orch`, `pm`, `principal`, `eng-1` (backend), `eng-2` (frontend), three focus-group personas | principal reviews every task |
| `full` | 10: `squad` plus `eng-3` and `eng-4` | principal reviews every task |

**How to read the outputs.** Output marked *(real)* was copied from a run of the binary built from this repo in a scratch repo (paths shortened). Output marked *(illustrative)* shows the shape, not exact text: a real run depends on your goal, models and timing.

1. **Check the toolchain.** *(real, first lines; a `slai doctor` also lists leftover teams from other directories)*

   ```
   $ slai doctor
   ✓ tmux    tmux 3.4
   ✓ git     git version 2.43.0
   ✓ claude  2.1.309 (Claude Code)
   ✓ permission modes match `claude --help`
   ✓ git repository
   - no .slai/team.yaml (run `slai init`); skipping project checks
   ```

   A `✗` line says what is missing. Nothing below works without tmux, git and `claude`.

2. **Write the team file.** In your repo, answer the wizard, or skip it with flags. *(real)*

   ```
   $ cd my-repo
   $ slai init --yes --preset solo --name demo --description "demo"
   wrote .../my-repo/.slai/team.yaml (2 agents). Next: run `slai`
   Will start 2 Claude sessions when you run `slai`. Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)
   ```

   Nothing is running yet. **Spend caps: OFF** means no limit, and `0` or a missing cap also means no limit. Before you give the first goal, open `.slai/team.yaml` and, if you want a ceiling, add (amounts in USD):

   ```yaml
   defaults:
     spend_caps:
       daily_usd: 10      # whole team, per day; at 80% slai warns, at 100% it stops waking agents and asks you
   ```

   More caps (per agent, per task, per run): [What this costs](#what-this-costs).

3. **Start the team.** *(the dashboard is illustrative)*

   ```
   $ slai
   ```

   Bare `slai` starts the agent sessions, makes sure the supervisor is running and opens the dashboard. (`slai up` does the first two without the dashboard, for scripts and servers.) Before you run it, `slai status` lists the configured agents with `-` for liveness *(real)*:

   ```
   $ slai status
   orch         orchestrator         opus     -
   eng-1        engineer             sonnet   -

   supervisor: not running. Next: `slai`
   ```

   Once `slai` is running, the same command shows each agent's state.

   **First run only: trust the folder.** Claude Code asks "do you trust this folder?" the first time it runs in a directory, and the dashboard shows a banner until you answer. Select `orch` in the list and press `a` (full-screen attach; `ctrl+q` comes back), choose **Yes, I trust this folder**, then `ctrl+q`. (`slai attach orch` does the same from a plain terminal.) slai never edits Claude's trust settings for you.

4. **Give the orchestrator a goal.** From any terminal (or `o` in the dashboard, which opens a prompt to `orch`): *(illustrative)*

   ```
   $ slai send orch "add a --version flag to the CLI that prints the build version"
   ```

   You only talk to `orch`; the other pane is the engineer, and you can ignore it.

5. **Watch it.** In the dashboard press `b` for the Tasks view, or run `slai serve` for the browser. *(illustrative)*

   ```
   T-0001  Add --version flag    in_progress   eng-1
   ```

   If an agent needs an answer from you, it appears under "Questions for you" in the `slai serve` Inbox, and `slai status` shows its ask id. Answer it with `slai reply <ask-id> "..."` (for example `slai reply 17 "use the second option"`; the id here is the example from `slai reply --help`).

6. **It merges.** With `solo` there is no principal, so when `eng-1` asks for review its commit is queued straight for merge into `slai/integration` (with `project.test_command` set, only if that command passes there). *(illustrative)*

   ```
   T-0001  Add --version flag    merged        eng-1
   ```

   With the `squad` and `full` presets a principal reads the diff first; see the [Worked example](#worked-example).

7. **Look, then take it.** Your own branch has not moved. *(output shape in the [Worked example](#worked-example) is real)*

   ```
   $ slai commit --dry-run     # shows exactly what would be added; changes nothing
   $ slai commit               # shows the same plan, asks "Apply? [y/N]"
   ```

8. **Stop.** `slai down` stops the supervisor and every agent session. If something goes wrong on the way, see [If the team goes wrong](#if-the-team-goes-wrong).

Which command does what, for you versus for agents: [docs/commands.md](docs/commands.md). A recorded two-agent run with its measured token cost: [docs/demo.md](docs/demo.md).

## What this costs

Every agent is its own Claude session, billed to your Claude account: the default `squad` preset starts 8 of them (`solo` starts 2, `full` 10). They are only busy while there is work, but a long goal can use a lot. `slai init` prints one line saying how many sessions will start and whether spend caps are on (`slai up` prints the same line, as "Starting", when it actually starts them):

```
Will start 8 Claude sessions when you run `slai`. Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)
```

To cap it, set `defaults.spend_caps` in `.slai/team.yaml` (see the config reference). At 80% of a cap the header warns; at 100% wake-ups pause and you are asked. `0` (or leaving a cap out) means **no cap**, not a zero-dollar cap.

## Worked example

A tiny goal, start to finish. **The board states below are illustrative** (a real run varies with your goal, models and timing, and takes minutes, not seconds). The `slai commit --dry-run` and `slai clean` output is copied from a real scratch run in a throwaway repo.

```bash
slai init --yes --preset squad --name app --description "demo"
# Will start 8 Claude sessions when you run `slai`. Spend caps: OFF (set defaults.spend_caps in .slai/team.yaml)
slai        # starts the sessions and opens the dashboard; send the goal below from another terminal
slai send orch "add a --version flag to the CLI that prints the build version"
```

1. **Goal received.** `orch` is working, everyone else is idle. In the dashboard `orch` shows `busy`; the other panes are quiet.
2. **Tasks created.** `orch` splits the goal and assigns it. Press `b` for the Tasks view:

   ```
   T-0001  Add --version flag             in_progress   eng-1
   T-0002  Document the flag in README    assigned      eng-2
   ```
3. **Review.** `eng-1` finishes, commits on its own branch (`slai/eng-1/T-0001`) and asks for review; the principal reads the diff:

   ```
   T-0001  Add --version flag             in_review     eng-1
   T-0002  Document the flag in README    in_progress   eng-2
   ```
4. **Merged.** The principal approves and slai merges the task into `slai/integration`. Your own branch has not moved.

   ```
   T-0001  Add --version flag             merged        eng-1
   T-0002  Document the flag in README    merged        eng-2
   ```
5. **Finish.** `orch` checks the result against your request and calls `finish`; the dashboard banner says `run finished`, and `slai status` shows the summary.
6. **Look before you take it.** From the branch you want the work on:

   ```
   $ slai commit --dry-run
   2 commit(s) from slai/integration will be added to main:
     53dc0c1 [T-0002] Add greeting
     a7ce562 [T-0001] Add hello.go
   files changed:
     hello.go | 5 +++++
     2 files changed, 9 insertions(+)
   as a fast-forward (the team's commits are kept, no merge commit).

   (dry run: nothing changed)
   ```

   (The `files changed` lines are illustrative.) Plain `slai commit` on a terminal prints this same plan and then asks `Apply? [y/N]`; answering `n` changes nothing.
7. **Take it.** `slai commit` (or `slai commit -b my-feature` to put it on a new branch and leave your current one alone). Nothing is pushed.

### If the team goes wrong

- Your branches, your working tree and your remote are never touched until `slai commit`, and `slai commit --dry-run` shows exactly what it would add. If the result is not what you wanted, don't commit it.
- To take **one task** back out before you commit anything, `slai revert T-0002 --dry-run` shows the plan and `slai revert T-0002` carries it out: it git-reverts that task's merge on `slai/integration`, marks the task `reverted`, and tells the orchestrator and the engineer. A later `slai commit` does not carry the reverted change. It refuses if later merged tasks depend on the task (`--force` overrides) or if the integration checkout has uncommitted changes. The supervisor runs it, so it must be up.
- To throw the team's work away and start over, `slai clean --branches --dry-run` shows the plan and `slai clean --branches` carries it out (it asks first):

  ```
  $ slai clean --branches --dry-run
  slai clean will:
    stop 0 agent session(s)
    kill 0 process(es) the agents started
    remove 0 git worktree(s)
    delete 3 file(s)/dir(s)
       .slai/state.db
       .slai/state.db-wal
       .slai/state.db-shm
    delete 1 branch(es): slai/integration

  (dry run: nothing changed)
  ```
- If you already ran `slai commit`, undo it like any other commit of yours (`git reset` / `git revert` on your branch); `slai revert` only works on the integration branch, not on commits already on yours.
- To stop spending while you decide, `slai down`. To redirect the team instead of stopping it, message a whole group at once: `slai send engineers "..."` (or `slai send all "stop and commit what you have"`, from `slai send --help`). *(real, in a `squad` team)*:

  ```
  $ slai send engineers "rebase on slai/integration"
  queued for engineers: eng-1 (message 1), eng-2 (message 2)
  ```

- If an agent has asked you a question and is waiting, `slai status` shows its ask id: answer it with `slai reply <ask-id> "..."`.

## Glossary

- **tmux**: a terminal multiplexer. Each agent runs in its own tmux session on a private server (socket `slai`), so nothing shows up in your own tmux. `slai attach <agent>` or `enter` in the dashboard takes you into one.
- **worktree**: a second checkout of your repo in its own directory (`.slai/worktrees/<engineer>`), so engineers can edit in parallel without touching your files.
- **integration branch** (`slai/integration`): where approved work is merged. It is **not your branch**: your branches are untouched until you run `slai commit`.
- **principal-review gate**: an engineer's work only reaches the integration branch after the principal engineer approves it (`require_review`, on by default; the `solo` preset has no principal and so no gate).
- **`permission_mode`**: how Claude Code asks before running commands. `manual` asks about everything; `auto` is Claude's classifier-based mode, which lets routine commands through and still blocks risky ones. `auto` is **not** a bypass mode; slai never passes `--dangerously-skip-permissions`.
- **`team.yaml`**: `.slai/team.yaml`, the team definition (agents, models, caps), written by `slai init` and edited with `slai init --edit`.
- **orch vs dashboard**: `orch` is the orchestrator agent, the one you give goals to. The dashboard is the `slai` TUI you use to watch the team and talk to `orch`; it is not an agent.
- **`slai doctor` lines**: `idle` means the agent is alive and waiting with nothing owed; it is normal. `escalation=none` means no open problem has been raised for that agent. `reason=` says why the state is what it is.

## More commands

`slai up` starts the agents and a detached supervisor without the dashboard (`--foreground` keeps the supervisor in this terminal) and ends by telling you who to send your goal to. Plain `slai` starts missing agents and runs the supervisor inside the TUI.

Other commands: `slai init --edit` (change the team; only agents whose config changed are restarted), `slai send <agent|group> "msg"` (group: `all`, or a role such as `engineers`, `orchestrator`, `product-manager`, `principal-engineer`, `focus-group`; see `slai send --help`), `slai reply <ask-id> "text"` (answer one specific question an agent asked you), `slai attach <agent>`, `slai hold [name]` (stop the whole team from being woken, nudged or restarted, e.g. overnight; sessions stay up and the hold survives a restart) and `slai resume` (lift it, and release agents held by the message-loop guard; `slai resume <agent>` releases just that agent), `slai commit`, `slai clean`, `slai doctor [--fix]`, `slai version`. `slai up --no-watch` starts the agents without any supervisor loop.

Non-interactive: `slai init --yes --preset squad --name app --description "..."`, and `slai init --edit --yes --set-model eng-1=haiku --permission-mode plan`. Add `--check-models` to test each model with a one-line `claude -p` call (warn only).

## Config reference (`.slai/team.yaml`)

```yaml
version: 1
project:
  name: my-app
  description: What the product is (fed to every agent)
  main_branch: main
  integration_branch: slai/integration
  test_command: go test ./...  # optional: run on every merge result before slai/integration moves (see below)
  test_timeout_seconds: 600    # a hanging test_command is killed after this (default 600)
defaults:
  permission_mode: manual      # claude --permission-mode; never bypass by default
  max_turns_per_task: 40
  max_concurrent_agents: 5
  require_review: true         # needs a principal_engineer
  spend_caps:                  # optional, USD. 0 or omitted = NO cap (not a $0 cap)
    per_agent_usd: 5           # warn at 80%, pause at 100%, per agent
    daily_usd: 25              # same, summed over the whole team per day
    per_task_usd: 4            # spend attributed to one task, summed across agents (see below)
    per_run_usd: 40            # spend since the run started (see below)
models:                        # alias -> --model value
  opus: opus
  sonnet: sonnet
  haiku: haiku
  fable: claude-fable-5-1
memory:                        # optional: the team's shared fact memory (slai memory ls)
  pack_tokens: 1500            # most tokens of facts + project map an agent gets with get_task / get_memory
notify:                        # optional: run a command on key events (see below)
  command: notify-send "slai: $SLAI_EVENT"
  events: [awaiting_user, escalation, spend_cap, run_finished]   # omit = all events
  timeout_seconds: 10          # a command that hangs is killed after this (default 10)
agents:
  - {id: orch, role: orchestrator, model: opus}
  - {id: eng-1, role: engineer, model: sonnet, focus: backend, permission_mode: auto}  # per-agent override
  - {id: fg-newbie, role: focus_group, model: haiku, persona: First-time user}
```

Rules: one orchestrator; at most one `principal_engineer` and `product_manager`; ids match `^[a-z][a-z0-9-]{1,23}$`; `model` is an alias or a raw `claude-*` string; unknown keys are errors. Per-role prompt overrides go in `.slai/prompts/<role>.md`. `permission_mode` on an agent overrides `defaults.permission_mode`; the presets and the wizard give the **orchestrator, PM, principal and engineers `auto`** (Claude's classifier-based mode, so routine commands like `go test` and `git status` don't stop them for approval) and leave focus-group personas on the team default (`manual`). `auto` is not a bypass mode. Use `manual` on any agent to be asked about every command. Presets: `solo`, `squad` (default), `full`.

### Merge-time test gate (`project.test_command`)

Two branches can each pass their own tests and fail together. With `project.test_command` set, every approved merge is first made in a scratch checkout, the command runs there (with `sh -c`), and `slai/integration` only moves if it exits 0. If it fails or times out (`test_timeout_seconds`, default 600), nothing is merged, the task stays `in_review`, and the engineer and the principals each get one note with the last 40 lines of output; the engineer merges `slai/integration` into their branch, fixes it and requests review again. Unset, merges behave exactly as before.

### Notifications (`notify:`)

With `notify.command` set, the supervisor runs it (with `sh -c`, in the project directory) once for each event below, so you can get a desktop or phone ping instead of watching the TUI. The event name is in `$SLAI_EVENT`, the agent in `$SLAI_AGENT`, and a JSON object (`event`, `agent`, `at`, `project`, `data`) is on stdin. It runs in the background: a failing command is only logged, and one that hangs is killed after `timeout_seconds` (default 10), so it can never slow the supervisor. Events from before you turned it on are not replayed, and each event fires once (an escalation does not repeat while it stays open).

| Event | When |
| --- | --- |
| `awaiting_user` | the team asks you a question or needs your decision (`ask_user`) |
| `review_requested` | an engineer asked for review |
| `review_approved` | the principal approved and the merge was requested |
| `escalation` | slai raised a problem about an agent (rate limit, stuck, crashed MCP server, ...) |
| `spend_warn` / `spend_cap` | a spend cap reached 80% / 100% |
| `run_finished` | the orchestrator called `finish` |

`events:` limits which of these fire; an unknown name fails validation and lists the allowed ones. Without a `notify:` section nothing changes.

## How agents get woken and what it costs

Agents never poll. Each is an idle Claude session until slai types one line into its tmux pane, and that line is the only thing that starts a turn.

**What wakes an agent.** The supervisor's wake tick types `[slai] You have N new items. Call get_inbox.` into an agent's pane when all of these hold: the agent has unread messages or assignments; it is idle (or finished); it is not paused, held or spend-capped; it is under `max_concurrent_agents`; its session is alive; nobody is attached to or focused on its pane; and the pane is safe to type into (no permission prompt). An agent that is waiting on you (`ask_user`) is woken only when the item is your answer, or after 15 minutes for other work. If the line goes unanswered it is typed again after 60 seconds.

**What that line is.** A nudge, not a message: `N` is how many unread items are waiting, and the agent reads them by calling `get_inbox` ([which command is which](docs/commands.md)). You never need to act on it.

**What a wake costs.** One agent turn: the `get_inbox` call plus whatever the items ask for. An idle agent with nothing owed costs nothing. The one background exception: while tasks are open and nothing is moving, the orchestrator gets a periodic board digest, which is also a turn. Spend is counted per agent and per task; to limit it, see [Spend caps](#spend-caps-per-task-and-per-run) (80% warns once, 100% stops waking that scope and asks you once; `0` or absent means no cap).

**When an agent ignores its inbox.** The **driver** (disable with `SLAI_NO_DRIVER=1`) watches for the quiet ways a team stalls:

- **Owed work:** an agent that is idle while it still owns an open task (assigned, in progress, changes requested; or in review, for the principal) gets "Reminder from slai (k of 4)" after 90 s, then every 3 min. After four reminders without progress the orchestrator is told which agent is stuck and on what (the orchestrator itself: you are told).
- **What `slai status` and the board call it:** `stale` is an agent that owes work or has unread items and has been silent for 10+ minutes; `gated` is silent because slai is deliberately not waking it (paused, held, spend capped, queued for a slot), so nobody needs to act; `blocked` means a human must act (a permission prompt, or an `ask_user`); `dead` is a session that is gone, and it is revived automatically if it has work (at most 3 times per 10 min).
- **Stuck mid-turn:** an agent that has been working for 3 min with no activity and an unchanged pane is interrupted and nudged, then killed and revived if that fails.

Other things the driver does:

- **The orchestrator never goes quiet:** while tasks are open and nothing is moving it gets a periodic board digest; when the last task closes it is told to verify the result against the original request, then call `finish` with a summary. `slai status` shows the finished run and its summary.
- **Crashes:** a dead agent that still has work or unread messages is revived automatically (at most 3 times per 10 min) into its **own** Claude session: each agent gets a persistent session id and revives with `--resume <that id>`, never `--continue` (which resumes whichever conversation in the directory is newest and would cross agents that share the project directory). `slai`'s dashboard respawns the supervisor if it dies, and a panic in one supervisor stage cannot stop the others.
- **Guards never deadlock the team:** the message-loop guard counts only real chatter (not assignments, reports or review traffic) and holds a pair for 90 s, then releases itself. Wake-ups to an agent that ignores them are capped: four reminders (90 s idle, then every 3 min), after which the orchestrator is told (see **Owed work** above). An agent parked on `ask_user` still receives other work after 15 min. Leftover uncommitted files in an engineer's worktree are stashed (and the engineer told), not allowed to block the next task.
- **Merges don't lose approvals:** transient merge failures retry; the supervisor-owned integration checkout is reset before every merge; after four failures the task returns to its engineer and the engineer, principal and orchestrator are told. Cancelled work is never merged.
- **Every task can finish:** tasks go to engineers (code, finished by `request_review` → merge) or the product manager (specs, finished by `report(done)`). `report(done)` on code that was never sent to review is refused with an explanation, a blocked task can be unblocked by the orchestrator with `update_task(status="in_progress")`, and reassigning a task gives the new engineer its own branch.

## Getting the work into your repo: `slai commit`

The team merges finished tasks into `slai/integration`; your own branches are never touched while it works. When you are happy, run `slai commit` **from the branch you want the work on**:

```
slai commit                 # add the team's work to the branch you are on
slai commit -b NAME         # create NAME from the current branch and put the work there (the branch you were on is left as it was)
slai commit --squash -m "Add the dashboard"   # one commit with your message instead of the team's per-task commits
slai commit --no-rebase     # merge instead of rebasing when your branch has moved on
slai commit --no-ff         # always make a merge commit, even when a fast-forward is possible
slai commit --dry-run       # show the plan, change nothing
slai commit --yes           # apply without asking
```

`slai commit` first previews what it will do: the target branch, the commits, the files changed (`--stat`), whether it will fast-forward, rebase or merge, and whether it conflicts with your branch (a trial `git merge-tree`, which touches nothing in your checkout), then asks `Apply? [y/N]`. `--dry-run` prints exactly the same plan and stops. Without a terminal (a script, CI) it refuses unless you pass `--yes` or `--dry-run`.

It fast-forwards when it can (keeping the team's per-task commits). If your branch has moved on, it first rebases the team's commits onto it in a scratch worktree (`slai/integration` is never rewritten); if that rebase conflicts nothing changes and you can retry with `--no-rebase` to merge and resolve by hand. It and it **pushes nothing**. It refuses, with the reason, if you are not on a branch (use `-b`), have uncommitted changes, or have an untracked file that differs from what would be merged. It clears one case automatically: an untracked `.gitignore` holding only slai's own rules (slai keeps those in `.git/info/exclude`, so `slai init` no longer creates a `.gitignore` that could block this). If `-b` fails partway, the half-made branch is removed and you are back where you started. (`slai land` still works as an alias.)

## Stopping and cleaning up

- `slai down` stops the supervisor, **every** agent session (on slai's private tmux server, on tmux's default server where older builds put them, and sessions of agents no longer in `team.yaml`), and **every process the agents started**. Agents are launched with `SLAI_PROJECT=<dir>` in their environment and children inherit it, so Claude background sessions and their daemon, MCP servers and dev servers an agent left running are found and killed too. It reports what it stopped and exits non-zero if anything would not die.
- `slai clean` does `down`, then removes the runtime state: `.slai/run`, `.slai/state.db`, the engineer worktrees and any worktree an agent created under `.claude/worktrees`. It keeps `team.yaml`, specs, prompts and the `slai/*` branches. Add `--branches` to delete the branches too (merged work lives there) or `--all` to remove the whole `.slai` directory. `slai clean --dry-run` prints the plan and changes nothing; it asks for confirmation unless you pass `--yes`.

## Roles and tools

You only talk to `orch`. The other panes are machine traffic: agents hand each other tasks, reviews and messages through slai, and you can watch but never need to answer. In particular, a line like `[slai] You have 2 new items` in an agent's pane is slai nudging that agent to read its inbox; it needs no action from you. The TUI says so under every non-orchestrator preview ("driven by orch; you rarely need to type here"). The only time to step in is a pane marked `needs you` (see Troubleshooting).

| Role | Works in | Writes | slai tools |
| --- | --- | --- | --- |
| orchestrator | repo root | nothing | create_task, update_task, cancel_task, get_board, send_message, ask_user, get_inbox, reply, finish |
| product_manager | repo root | `.slai/specs/**` | get_board, get_task, report, send_message, ask, ask_user, get_inbox, reply |
| principal_engineer | repo root | `.slai/decisions.md` | get_board, review_diff, approve, request_changes, send_message, ask, get_inbox, reply |
| engineer | `.slai/worktrees/<id>` | own worktree | get_task, report, request_review, ask, send_message, get_inbox, reply |
| focus_group | repo root | nothing | get_inbox, reply, get_board |

Every role can also answer a liveness check, and every role except focus_group can write to the team's shared memory with `remember` (all roles can `recall` and `get_memory`; see [Team memory](#team-memory)). `reply` is the way any agent answers an `ask` sent to it. You answer a specific question an agent asked you with `slai reply <ask-id> "..."` (the id is shown next to the waiting agent in `slai status` and on the web inbox); `slai send` is the free-form path for everything else.

## TUI

`slai` opens an Agent-Manager-style dashboard: the **agent list** on the left (grouped and foldable: Orchestrator, Planning, Engineers, Focus group; each row shows status, id, model and what the agent is doing), a large **live preview** of the selected agent on the right, a header with counts / spend / pause state, and a one-line key legend that changes with the mode. Nothing is hidden behind a prompt bar: you either move around the list, or you are *focused* on one agent.

Agents run on a private tmux server (socket `slai`, override with `SLAI_TMUX_SOCKET`; inspect with `tmux -L slai ls`), so they never show up in your own tmux.

**Talking to an agent in place.** Select it and press `enter` (or `→`). The preview title and footer turn green with `FOCUSED: orch — ctrl+q to return to list`, and every key (including Esc, arrows, ctrl+c, function keys, alt+key and multi-line bracketed paste) is forwarded to that agent's pane. The agent's tmux window is resized to exactly the preview size, so what you see is what it renders. `ctrl+q` (or `ctrl+\`) returns to the list. First run? Select `orch`, press `enter`, and just type your request.

**Keys (list mode)**

| Key | Action |
| --- | --- |
| `↑ ↓` / `j k` | move; `←`/`→` (or `enter`) fold / unfold a group header |
| `enter` / `→` | focus the selected agent (keys go to its pane) |
| `ctrl+q` | (focused) back to the list; also `ctrl+\` |
| `space` | quick prompt: type one line, `enter` sends it to the agent without focusing, `esc` cancels. `tab` completes the word under the cursor: task ids (`T-00…`), branches (`slai/…`) and agent ids (`pri…` or `@pri…`); `tab` again cycles, `shift+tab` goes back, `esc` restores what you typed. Answers a pending `ask_user` |
| `c` | redirect a `busy` agent: type one line; slai interrupts its turn (Escape, only when no permission dialog is open) and delivers it as "Course correction from user: ...". Refused with a reason on dead, paused or not-busy agents |
| `g` | jump to the next agent that needs you (amber `▲`) |
| `b` | Tasks view (grouped by status; each row shows its priority `pN`, highest first within a group; `+` / `-` raise / lower the selected task, `T` sends it to the top, `enter` detail, `d` diff, `esc` back; same as `slai priority <task> <n|up|down|top>`) |
| `l` | Log view (`h` toggles hook noise, `esc` back) |
| `s` | Stats view: per agent and per harness/model, merged tasks, review rounds per task, rework %, merge refusals, $ per merged task (and median time to merge on terminals 100+ wide); same data as `slai status --stats`. `esc` back |
| `M` | Team memory: the facts under `.slai/memory/` with their state against the integration branch (fresh / stale / unverified / invalid; stale rows are amber with the reason), `f` filters by state, `enter` shows the file and each cited file's recorded vs current blob, `e` edits the file in `$EDITOR` (the edit is then validated and recorded as verified by you), `d` deletes after asking. A task's detail view (`t`) lists the facts of its last delivery and a stats line (facts delivered, pack tokens, tokens vs tasks without a pack). Same data as `slai memory ls` |
| `t` / `d` | detail / diff of the selected agent's task |
| `v` | revive a dead agent (`claude --resume` of its own session, falling back to a fresh one) |
| `x` / `R` | stop the session / restart with a fresh conversation (both ask first) |
| `a` | full-screen tmux attach (`ctrl+q` detaches back) |
| `p` | pause / resume the team |
| `H` | hold the team like `slai hold [name]`: prompts for an optional name, a banner shows "team on hold: NAME since TIME, no wakes are being delivered", agent rows read `gated · team hold`; `H` again (or `p`) resumes |
| `?` / `q` | help / quit (agents keep running) |
| `m` | toggle mouse capture (off = the terminal's own selection / scrolling) |

**Mouse.** Click an agent to select it, click a group header to fold it, click the preview to focus; the wheel scrolls the list when over the list.

- *Wheel over the preview* depends on what the agent's pane is doing (slai reads tmux's `alternate_on` / `mouse_any_flag` / `mouse_sgr_flag`):
  - the app handles the mouse itself (Claude Code does: alternate screen plus SGR mouse reporting): the wheel is forwarded as SGR reports (button 64/65, 3 per notch, translated to the pane's 1-based cell coordinates), also while unfocused. Claude then scrolls itself and shows its own "Jump to bottom (click)" control. Legacy (non-SGR) encodings fall back to X10 reports where the coordinates fit;
  - alternate screen without mouse reporting: three `Up` / `Down` keys per notch (pager style);
  - normal screen (tmux has scrollback): the wheel scrolls the tmux history, 3 lines per notch. The title and footer show `↑ N lines back`; the view holds its position while new output arrives. While scrolled back, `PgUp` / `PgDn` / `Home` / `End` move the view instead of going to the agent; typing any other key snaps back to the bottom and is forwarded.
  The wheel is never sent to a mouse-reporting app as keys.
- *Clicks.* A click on the preview of an unfocused agent only focuses it. When the agent is focused and handles the mouse, a click is forwarded as press + release (right and middle too), so the app's own clickable UI works; a double click is then two forwarded clicks. The focus footer shows `mouse: app · drag selects` or `select: drag`.
- *Selecting and copying.* Drag inside the preview to select (confined to the pane: it never grabs the list or the borders); on release the text is copied (ANSI stripped, trailing spaces trimmed) and the footer says `copied N lines`. Drags are never forwarded to the app. Double click selects a word and triple click a line when the app has no mouse handling of its own. `Esc` or any key clears the selection (in focus mode `Esc` that clears a selection is not forwarded). Copying uses `pbcopy` / `wl-copy` / `xclip` / `xsel` when present *and* an OSC 52 escape (wrapped in tmux's DCS passthrough when `$TMUX` is set), so it also works over SSH. Inside tmux you need `set -g allow-passthrough on` for OSC 52 to reach your terminal.
- *Native terminal selection.* In list mode press `m` to release mouse capture: your terminal's own selection and scrolling work again (the footer says so); press `m` again to capture. Most terminals also let you bypass capture with `shift`+drag. The preview has no vertical bars at its left and right edges (only a rule above and below), so a native selection copies the agent's text and not the frame. The built-in drag selection copies only pane text; when every selected line sits in a box-drawing gutter (Claude Code's input box and dialogs), the leading `│ ` and trailing ` │` are dropped as well.

**Supervisor.** Whoever starts first owns the supervisor loop: `slai` runs it inside the TUI, or a detached `slai up` does (the header then says `supervisor: daemon`, and the TUI just watches). `slai down` stops both.

**Trusting the folder.** Claude Code asks "do you trust this folder?" the first time it runs in a directory. slai never edits Claude's trust settings; while the folder is untrusted the TUI shows a banner. Focus an agent and answer the dialog once (or run `claude` in the project directory and accept).

## Web UI: `slai serve`

`slai serve` opens the team in a browser: a dashboard, the agents (with a read-only copy of each
terminal), a task board you can drag cards on, the reviews and questions waiting for you, stats,
doctor, the event log and team memory. It uses the same data and the same functions as the TUI.

```bash
slai serve                        # 127.0.0.1 on a random port; open the printed URL
slai serve --read-only            # look, don't touch
slai serve --listen 0.0.0.0:8080 --insecure-http   # other machines on a trusted LAN (prints a one-time token)
```

It is a separate process that never starts a supervisor, sign-in cannot be turned off, and it
cannot approve a review, merge, `slai commit` or type into a terminal; `/capabilities` lists every
write it can make. Plain HTTP off-loopback needs `--insecure-http`, and the token is a password:
whoever holds it can read every agent's terminal. Full guide, the TLS-proxy recipe, the page list
and the web-vs-CLI table: [docs/serve.md](docs/serve.md).

## Team memory

Agents write down what they learn about the project as short **facts** (one Markdown file each in
`.slai/memory/`, tracked in git); the principal verifies them when it approves a task, and your own
edits count as verified. A fact records the files it is about and is marked **stale** when one of
them changes on `slai/integration`; stale facts are never handed to agents. Agents get a small pack
(`memory.pack_tokens`, default 1500) through `get_task` and `get_memory`, never typed into a pane.

```bash
slai memory ls [--stale]      # facts and their state: fresh / stale / unverified / invalid
slai memory show <id>         # the file, and each cited file's recorded vs current blob
slai memory verify <id>       # run a fact's check: command (asks first), mark it verified by you
```

`M` in the TUI and the Memory page of `slai serve` show the same facts. Details, the write-time
deny-list and how to edit or delete a fact: [docs/memory.md](docs/memory.md).

## Safety guards (PRD section 8)

| Risk | Guard |
| --- | --- |
| Runaway spend | `spend_caps`: 80% warn event, 100% pauses wake-ups + `ask_user`; per task and per run too (see below) |
| Too many agents | `max_concurrent_agents`: idle agents are not woken past the cap |
| Review ping-pong | 3 `request_changes` rounds block the task and notify orchestrator + user |
| Message loops | >10 messages between a pair in 10 min pauses both and asks the user |
| Long tasks | hook sends the agent "report(blocked)" after `max_turns_per_task` turns |
| Out-of-lane edits | generated `settings.json` allow/deny rules plus a `PreToolUse` hook that denies Edit/Write/NotebookEdit and file-writing shell commands outside the role's lane |
| Branch collisions | one worktree per engineer, one branch per task, merges serialized by a mutex |
| Destructive commands | deny `git push`, `git reset --hard`; `rm -rf` denied for non-engineers and confined to the worktree for engineers |
| Permissions | Claude Code's prompts stay on; slai never passes `--dangerously-skip-permissions`; the wizard warns on `bypassPermissions` |

The shell checks are best-effort static analysis, not a sandbox.

Every denial is recorded as a `lane_denied` event (agent, tool, and the path or command it aimed at). See them in the TUI: `I` on an agent lists the count for the last 24 h and the newest three, and `l` (Log) shows every one. `slai status` appends `lane-denied(24h)=N` to an agent's line when N > 0, and `slai status --json` has `denied_count` per agent.

### Spend caps per task and per run

`per_task_usd` and `per_run_usd` sit beside `per_agent_usd` and `daily_usd` under `defaults.spend_caps`; `0` or leaving them out turns them off. Editing `team.yaml` is picked up on the next tick, so raising a cap lifts its gate without a restart.

- **Attribution.** Every usage record is stamped with the task its agent was working on at that moment (the agent's current task). A task's spend is the sum over all agents, so a reviewer's tokens spent on a task count toward it. Spend recorded while an agent has no current task, and spend before this feature, belongs to no task and counts toward the run cap only.
- **`per_task_usd`.** At 80% the assignee and the orchestrator each get one note and a `spend_warn` event (which `notify` can forward). At 100% the task's assignee is gated (the board shows liveness `gated`, reason `task cap`; other agents keep working) and you get one `ask_user`: raise the cap or cancel the task. A finished (merged, cancelled, done, reverted) task no longer gates anyone.
- **`per_run_usd`.** The run starts at the later of `slai up` (when it starts any session) and the last `finish`; spend is counted from then. At 80% there is a `spend_warn` event; at 100% the whole team is gated (`wake_gate.spend_capped`, reason `spend capped`) with one `ask_user`. It uses the cap path, not `slai hold`, so raising the cap resumes everything. Before the first `slai up` of an older database, the run is all recorded spend.
- Each warning and ask fires once per cap value: after raising a cap, crossing the new one warns and asks again.

## Architecture

```
            +--------------------- .slai/state.db (SQLite WAL) ---------------------+
            |  agents  tasks  messages  events  usage  kv                          |
            +----^------------^-----------------^-----------------^----------------+
                 |            |                 |                 |
   TUI (slai) ---+   supervisor (slai up/TUI)    +-- slai mcp ----+-- slai hook <event>
   agent list/       tmux wake-ups, merges,         (stdio, one per    (Claude Code hooks:
   live preview      guards, reconcile              agent, role-checked) status, usage, lane guard)
                         |
                  tmux session per agent:  claude --model ... --settings run/<id>/settings.json
```

Agents never talk directly: tool calls write rows, the supervisor turns them into one-line `tmux send-keys` wake-ups.

## Testing

```bash
go build ./... && go test -count=1 ./... && go vet ./...
```

Integration tests use a real tmux server in a temp dir and `testdata/fakeclaude`, a stand-in `claude` binary, so no tokens are spent. Golden launch configs live in `internal/supervisor/testdata/golden` (`go test ./internal/supervisor -update` rewrites them).

## Releasing

`goreleaser release --clean` builds linux/darwin on amd64/arm64 (`CGO_ENABLED=0`, version via ldflags) and updates the Homebrew tap (`oorbt/homebrew-tap`, configured in `.goreleaser.yaml`, which must exist first).

## Troubleshooting

- `slai doctor` reports tool versions, `--permission-mode` drift versus `claude --help`, orphan tmux sessions, stale supervisor pidfile, missing worktrees/integration branch, and agent rows that disagree with tmux. `slai doctor --fix` removes the stale pidfile and kills orphan sessions.
- Agent shows `needs you` (amber `▲`): it is at a permission prompt or asked a question; press `g` to jump to it, `enter` to focus it and answer (or `space` for a one-line answer). If it asked you a question, `slai reply <ask-id> "..."` answers that exact question from any terminal.
- Agents are on tmux socket `slai`: `tmux -L slai ls`, or `slai attach <id>`.
- A hook's `hook.log` is at `.slai/run/<agent>/hook.log`; the supervisor log at `.slai/run/supervisor.log`.
- A wake-up is skipped while you are attached to that agent's session.
