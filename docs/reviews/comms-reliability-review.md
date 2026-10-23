# Comms reliability, hangs and identity: findings and fix plan (T-0027)

Author: eng-1. Date: 2026-10-23/24. Build under test: slai `719c46f`. Input credited: the principal's independent read (`.slai/decisions.md`, "Comms reliability: principal's read of the wake/delivery path" and "Identity: how an agent knows who it is") and orch's two data points. Where I only read code and did not observe the failure, the text says **code-read**; where I saw it happen it says **observed**.

## Summary

1. **Identity loss, the top item, has a proven root cause in a real run, and it is fixed in the current tree.** In the user's `/tmp/minion` run an older slai build restarted the whole team twice (at +41 min and +55 min). It resumed the six agents that share the repo root (orch, pm, principal, fg-newbie, fg-power, fg-skeptic) into *one* Claude conversation, the principal's. That transcript now contains the system prompts of five different agents. That is how an orchestrator "forgot who it was". The current code starts each agent with its own `--session-id` and resumes with `--resume <that id>` (`internal/claude/launch.go:121-124`, `internal/supervisor/supervisor.go:152-163`), so this path is closed. What remains are weaker, secondary paths (section A).
2. **Mail that never arrives has four proven causes**, ranked by how long they stall the team: (1) a failed worktree switch for an assignment silently blocks *all* mail to that engineer (27 min in the minion run, observed in events); (2) a tmux client attached to an agent silently blocks every wake and every recovery step for it, with no timeout and no message (10.4 min in my repro, observed); (3) the old message-loop guard paused agent pairs indefinitely (about 24 min in minion; already fixed to a 90 s hold in the current tree); (4) mail to a busy agent waits for the end of its turn (5 to 10 min for engineers, by design).
3. **The happy path is fast and healthy**: 1 to 4 s from message to the recipient reading it when it is idle. The sluggishness the user sees is almost entirely the four stalls above plus per-hop LLM latency, not the supervisor's tick.
4. Orch's two extra data points are both confirmed in code (sections C.7, C.8).

## A. Identity (top priority)

### A.1 How an agent learns who it is today (verified by reading the launch path and by experiment)

| Channel | Where | Survives `/clear` | Survives `/compact` | Survives restart / `--resume` |
|---|---|---|---|---|
| Role prompt "You are **orch**, role: ..., Teammates" in the **system prompt**: `claude --append-system-prompt "$(cat .slai/run/<id>/system.md)"` | `roles.Render` -> `internal/claude/launch.go:106` | **yes** (observed) | **yes** (observed) | **yes** (observed, and see note) |
| MCP server pinned to the agent: `slai mcp --agent <id> --project <dir>` plus `env` | `launch.go:62` | yes (process-level) | yes | re-written on every launch |
| Hooks pinned: `slai hook <event> --agent <id>`; `settings.json` `env` | `launch.go:63-72` | yes | yes | re-written on every launch |
| `SLAI_AGENT_ID` in the tmux session env | `supervisor.go:172` | yes | yes | yes |
| Wake-up line typed into the pane | `loop.go:358` | n/a | n/a | the restart variant says "You were restarted and lost your previous context" but does **not** say who you are |
| Any identity text in the conversation itself (first user turn) | none | n/a | n/a | n/a |

**Experiments** (scratch repo, solo preset, orch on Opus, Claude Code 2.1.309):
- After typing `/clear` in orch's pane, then a message "say who you are": it answered "I'm orch, the Orchestrator for project demo2 ... my only teammate is eng-1". Identity retained.
- After `/compact`: same answer. Retained.
- `claude -p --resume <orch's pre-clear session> --fork-session` **without** `--append-system-prompt` still answered "I'm orch, the Orchestrator for the demo2 project". So on resume Claude Code evidently restores the earlier system prompt/conversation; slai passing the flag again on resume is belt and braces. (I did not test a resume where `system.md` changed; I'd expect the new flag text to win, but it is untested.)
- Hook behaviour (answers pm's question): typing `/clear` fires `SessionStart` with a **new session id** (`0f636f39...`; the old id was `1654493e...`). `/compact` fires `SessionStart` with the **same** session id. Claude Code documents the `source` field as `startup|resume|clear|compact`, but **slai drops it**: the stored event data is `{session, status, tool, type}` only (`internal/hooks/hooks.go:95`), so I could not observe the raw `source` value. Recommended: store it (task I-2).
- After `/clear`, `.slai/run/orch/session-id` still holds the *old* id (observed). Nothing in slai updates it (only opencode captures an id). A later revive would `--resume` the conversation the user just cleared.

**Per harness (answers pm for C4/C5):** Claude and pi use `--append-system-prompt` from `system.md` (`launch.go:106`, `harness/pi.go:157`): a system prompt, re-applied every launch, not removable by `/clear` or compaction. opencode puts the same text in the agent-mode `prompt` of the generated `opencode.json` (`harness/opencode.go:95,117`), also a system-level prompt. I verified Claude by experiment; pi and opencode by code only.

### A.2 Where identity can be lost or confused, ranked

1. **Resuming by directory instead of by id (`--continue`). PROVEN, fixed.** Evidence from `/tmp/minion` (state.db plus `~/.claude/projects/-tmp-minion/`):
   - `hook:sessionstart` events at +2449 s and +3283 s show orch, pm, principal, fg-skeptic, fg-power and fg-newbie all reporting the **same** session id `f5253f20...`, the principal's original session. The two engineers, who have their own worktrees, kept their own ids.
   - That one transcript file contains `You are **orch**` 3x, `You are **fg-newbie**` 3x, `fg-power` 3x, `fg-skeptic` 3x and `You are **principal**` 10x.
   - The run files from that build had `mcp.json` with `"args": ["mcp"]` only and `settings.json` with no `env`: no `--agent` pin at all. So identity also depended on an inherited env var.
   - At +4278 s (03:31:12) a newer build relaunched everyone with eight distinct ids, and `session-id` files were written then.
   - The fix is visible in comments: `launch.go:29-31`, `supervisor.go:152-155`, `harness/pi.go:151`, `harness/opencode.go:29`. Unit tests exist. `git log` is squashed in this repo so I cannot date it.
   - **Residual risk:** a team running an old build keeps the old behaviour until restarted by the new binary. `slai doctor` should flag run files that predate the pin (task I-3).
2. **Compaction summaries written from other agents' text.** Code-read (principal's point, which I agree with): the system prompt survives, but a long conversation full of "eng-2 requests review ..." can make the summary narrate someone else's role. Nothing re-grounds after a compact: `noteSessionStart` (`hooks.go:254-279`) acts only within 5 minutes of a revive and ignores `compact`/`clear`. The spec's C3/C5 cover this; the compact SessionStart is the trigger.
3. **`pong` takes the id from the shell** (`echo $SLAI_AGENT_ID`), the weakest source. The spec addendum already drops that argument.
4. **`/clear` leaves a stale `session-id` file** (A.1). The spec decided resets are restarts with a fresh `--session-id`, which avoids this, but a user typing `/clear` in an attached pane still creates it. Fix: on `SessionStart` with a session id different from the file's, rewrite the file (task I-2).
5. **Fresh fallback when `--resume` fails.** `launch.go:124` falls back to `--session-id <same>` and then to a brand new id. The agent keeps its role prompt but loses history. When the agent is the orchestrator, `noteSessionStart` tells *other* orchestrators only (`hooks.go:285-289`), so nobody is told. The restart wake line says "lost your previous context" but not the agent's name or role.
6. **A project or global `CLAUDE.md` that talks about a different role.** Claude Code loads `~/.claude/CLAUDE.md` and the project/worktree `CLAUDE.md` into every agent. Example from this very repo: a user-global CLAUDE.md that tells the reader to "read `.mex/ROUTER.md` at the start of every session". It does not contradict identity, but any file that says "You are X" would compete with the appended prompt. Not observed to cause harm. Risk: low/medium; mitigation is the identity line in wakes (C3).
7. **Wake-up lines and inbox items never name the reader.** Messages say "From: orch" but the reader is never told "you are eng-1". An agent working through a long inbox of other agents' words has nothing to anchor against. (C3.)
8. **An empty or missing `system.md` starts an agent with no role, silently** (principal's point). `StartAgent` does not check. (C4 task.)

### A.3 Proposals

Covered by the existing spec `context-reset-and-liveness.md` Part C (C1 agents.json, C2 `whoami`, C3 identity line in wakes, C4 per-harness test and refuse on empty system.md, C5 compaction re-ground, C6 detection, C7 orchestrator guard, C9 e2e). My additions, none overlapping those files (details in section D, tasks I-1..I-3):
- Record `source` on SessionStart events and use `compact`/`clear` as the C5 trigger.
- Rewrite `.slai/run/<id>/session-id` when a SessionStart reports a different session id.
- Make the restart wake line state the agent's id and role, and tell the *user* (not only other orchestrators) when the orchestrator itself came back without context.
- `slai doctor` check: run files from an older build (no `--agent` in `mcp.json`, no `env` in `settings.json`, or two agents sharing a session id in `hook:sessionstart` events) = red.

## B. How delivery and wake-ups work today

```
send_message / ask / reply / create_task / notify  (internal/service)
   -> INSERT into messages (delivered_at=0, woken_at=0)                   store.go:501
supervisor.Tick, every 1 s (DefaultInterval)                               loop.go:86-118
   ... worktree, memory, merge, redirect steps ...
   -> wakeTick                                                            loop.go:276
        pending = PendingWake(): delivered_at=0 AND (woken_at=0 OR woken_at < now-60s)   super_queries.go:87
        per agent with pending mail, skip if ANY of:
          paused / spend gate / team hold / loop-guard hold               loop.go:324
          status not idle|finished (unless waiting_on_user + a user msg)  loop.go:327-335
          wakeHeld: 5 ignored wakes, or last wake <5 min ago & no Stop    loop.go:339,464-485
          working >= max_concurrent_agents (ping exempt)                  loop.go:339
          tmux session missing                                            loop.go:342
          user focused it in the TUI                                      loop.go:345
          any tmux client attached                                        loop.go:348
          prepareAssignments fails (worktree switch)                      loop.go:352
          pane shows a permission/trust modal                             loop.go:355 -> health.go:106
        else: type "[slai] You have N new items. Call get_inbox." + Enter via tmux send-keys   loop.go:358-373
              set woken_at, optimistic status=working                     loop.go:377-387
agent reads it:  UserPromptSubmit hook -> status working; agent calls get_inbox -> delivered_at set
verification:    landingAgent: no UserPromptSubmit within 15 s -> clear input, retype once   health.go:605,671
                 unreadAgent: woken >10 min ago and still unread -> retype, then restart     health.go:396-455
                 stuck.go: 3 min silent + unchanged pane -> nudge, restart
                 driver.go: reminders (90 s idle / 3 min / 4x), orch board digest
```

Latency sources by design: the 1 s tick; mail waits until the recipient is idle (agents are never interrupted mid-turn; there is no PostToolUse delivery); `WakeRetry` 60 s; `WakeBusyHold` 5 min; `FallbackStale` 30 s before pane polling replaces missing hooks (`loop.go:26`); `max_concurrent_agents` slots.

Loss points by design: the typed line is a keystroke into a TUI (can be swallowed by startup, a dialog, or a queued prompt); status is derived from hooks (a missed Stop hook leaves an agent "working" and un-wakeable until pane polling corrects it).

## C. Root causes, ranked by impact, with evidence

### Measured: healthy path (observed, my scratch repo, solo preset)
From the `messages` table: created -> woken -> read.

| message | created -> woken | woken -> read | note |
|---|---|---|---|
| goal to orch (first run) | 18 s | 22 s total | held by the trust prompt I had not answered; then the first wake line was typed at SessionStart and **swallowed**, `wake_retyped` fired 16 s later |
| orch -> eng-1 assignment | 1 s | 2 s | 3 s total |
| second message while orch idle | 1 s | 2 s | |
| slai note to eng-1 while it was mid-turn | 4 s | 1 s | waited for its Stop hook |
| E5: message while a tmux client was attached | **625 s** | n/a | woken 1 to 2 s after detach |

End to end the demo goal took 135 s for a trivial two-file change; supervisor overhead is a few seconds of that.

Real team, `/tmp/minion` (131 messages over 78 minutes, older build): mean created->woken 140 s (eng-1), 382 s (eng-2), 17 s (orch), 4 s (pm, principal, fg-*). 13 messages took over 120 s to be read; the worst were 1907 s.

### C.1 A failed worktree switch silently blocks all mail to an engineer (observed in events + code-read)
- minion event 465 at +790 s: `wake_error: worktree .../eng-2 is dirty; refusing to switch to slai/eng-2/T-0007`. Messages 49 (assignment), 55, 65 and 69 (notes) to eng-2 were not woken until +2449 s: **27 minutes**. The stall ended only when a team-wide restart ran the newer build, which auto-stashed (`worktree_stashed` at +2449).
- Mechanism (current code): `prepareAssignments` returns false on `switchWorktree` error (`loop.go:633-668`), and `wakeTick` does `continue` (`loop.go:352`), dropping **every** pending message for that agent, not only the assignment. The error is reported once per message (`reportSwitchError`, `onceKey`), as a note to the orchestrator; nothing reaches the user, and nothing retries with a different outcome.
- Current-code status: `switchWorktree` now stashes first (`loop.go:605`), so the dirty case self-heals. But any other failure (stash fails, branch checked out elsewhere, locked index) reproduces the whole stall. Impact: high (whole engineer offline), likelihood: medium.

### C.2 An attached tmux client or TUI focus silently blocks all wakes and all recovery (OBSERVED)
- Repro: attach a client to eng-1's session (`script -qfc "tmux attach ..."`), `slai send eng-1 ...`. After 625 s the message was still unwoken; `slai status` showed eng-1 as plain `idle`, no events were written, `slai doctor` showed `stale ... 1 unread, silent 18m0s` with no reason. Detaching woke the agent within 2 s.
- Code: `loop.go:345-350` (focus, `ClientCount`), and the same test in `holdOff` (`health.go:92-98`, which gates *every* health step: modal, unread, hooks, stuck), `stuck.go:120-126`, `driver.go:227`, `redirect.go:72`. A `ClientCount` error counts as attached (`err != nil || n > 0`).
- Why this hurts in practice: a user who leaves `slai attach orch` or a `tmux attach` window open while watching ("I just keep it open") silently stops that agent from receiving anything, and all the safety nets are off for it too.
- `unreadAgent` cannot rescue it: `oldUnread` requires `WokenAt != 0` (`health.go:391-406`) and a never-woken message has 0.

### C.3 Message-loop guard paused pairs indefinitely (observed, already fixed)
- minion event 647 at +1043 s: "eng-1 and orch exchanged 11 messages in 10m0s; both are paused. Resume them with `u` or `slai resume`." Orch's messages 71 and 73 were read at +1431 s and +1418 s after creation: **24 min**; eng-1's message 72 took 1602 s.
- The current guard holds for 90 s and releases itself (`guards.go:19-24`, `releaseHolds`). Residual: the threshold counts 11 chat messages in 10 min, which a normal review ping-pong reaches; the user still gets an `ask_user` for it.

### C.4 Typed wake not submitted; wake counter and 5-minute hold (observed once; rest code-read)
- Observed: the first wake typed at SessionStart was lost; `landingAgent` retyped 16 s later (events 4 -> 6 -> 7). Good recovery, but note it costs one of the five counted wakes.
- Code: after a miss, `WakeBusyHold` (5 min) blocks another wake until a Stop hook arrives (`loop.go:484`); `MaxUnansweredWakes = 5` and then the hold is permanent for the same oldest message: `loop.go:466-483` returns true forever after the single "may be stuck" notice to the orchestrator. If the stuck agent *is* the orchestrator, the notice goes to itself. Recovery then depends on `mcpDead` (restart) or on `unreadAgent`.
- Counter resets only when the oldest pending message id changes (`loadWake`, principal's #1). I did not reproduce a five-wake lock-out; the principal's repro (five wakes landing during long turns) should be used.

### C.5 Status stuck at "working" and the slot counter (code-read)
- `wakeTick` sets status "working" optimistically (`loop.go:385`) and counts it in `working` (`loop.go:317-322, 386`). If the Stop hook is missed, only pane polling after 30 s (`statusTick`, `loop.go:211-264`, skipped for `waiting_on_user`) can correct it; if `detectPane` does not recognise the idle prompt, the agent stays un-wakeable and holds one of the `max_concurrent_agents` slots. The `squad` preset has 8 agents and **5** slots (`presets.go:28`, only `solo` and `full` override it), so three phantom "working" agents can starve the rest. The skip is silent: no event, though `get_board` computes `queued_for_slot`.
- Not reproduced: needs a deliberately dropped hook.

### C.6 First run in a fresh project (observed)
- `slai up` starts only orch until Claude Code trusts the folder (`supervisor.go:74-89`, `KVPendingStart`). The trust dialog sits in orch's pane; if the user runs headless (`slai up`) or does not look, the team never starts. Observed: it waited 18 s for me; `type_skipped_modal` and `pane_decision` events were written. `modalAgent` escalates after `ModalPatience` 2 min (`health.go:30,350-380`), but only into the dashboard/web inbox/notify channel, nothing on the CLI. `slai up` does print the instruction once. This is the likeliest explanation for "I started slai and it just hung" in a fresh project; I cannot confirm the user's actual case.
- If the supervisor process itself dies (it did in my run when its parent shell was torn down), `slai status` now says "no supervisor is running, so 1 message(s) are waiting and nobody will wake the agents. Run `slai`." Good.

### C.7 Hook-silence false alarm (orch data point 1; code-read, plausible)
- `hooksAgent` (`health.go:707-762`) raises `hooks_failing` when the pane "reads working" and changes while no hook arrived for 5 min. It exempts a tool call in flight (`ToolRunning`) and subagents, but **not** an agent waiting on a *background task/shell*, which `liveness.go` already knows how to read (the footer "waiting on background task (1 shell)"). A long background shell therefore looks like silent hooks. I did not reproduce it. Also the alarm text points to `.slai/run/<agent>/hook.log`, which `hook.go:84` only creates when a hook **errors**; in the false-alarm case there is no such file, as orch saw.

### C.8 Leaked test supervisors add load (orch data point 2; observed)
`ps` shows supervisors, `slai mcp` processes and dedicated tmux servers (`tmux -L slai-e2e-...`) left by `internal/e2e` tests: `TestSelfHealOrchDownpi` alive 35 h, `TestSelfHealHungTurnopencode` alive 22 h, and several tests running concurrently right now (`TestRealTmuxSmoke`, `TestMergeConflictResolved`, `TestMemoryPlantedWrongFactGoesStale`). `slai doctor` lists them as "Other slai teams". Each runs a 1 s tick doing several tmux calls per agent. It does not break delivery in a real project but loads the machine the team runs on, and `go test ./...` on a shared box is slow (the e2e package took about 200 s for me).

### C.10 Board check calls working agents "idle" (orch data point 3; code-read, matches orch's observation)
- `taskLine` (`internal/supervisor/driver.go:149-155`) prints `idle <now - task.UpdatedAt>`: the time since the *task row* last changed, not whether its assignee is in a turn. An engineer 13 minutes into a `go test ./...` has an old `UpdatedAt` and is listed as "idle 13m".
- The digest fires when the board signature (`task:status:assignee`) has not changed for 10 minutes even though someone is busy (`driver.go:365-371`, `busy && stuckFor < 10*time.Minute` returns only before 10 min), and lists every open task under "Need a decision" (`driver.go:385-390`), including tasks whose owner is `working`/mid-turn. So a normal long turn produces a "decide now" prompt for work that is fine, which trains the orchestrator to ignore real stalls and costs it a billable turn every 4 minutes (`DigestEvery`).
- Fix (task H-3): label it "no board change for N" instead of "idle", annotate each line with the assignee's liveness from `health.Classify` (busy / idle / stale / blocked), and list under "Need a decision" only tasks whose owner is not busy; busy ones go under "In progress, no action needed".

### C.9 Lower
- Mail to a busy agent waits for its turn to end: engineers' long turns gave 480 to 600 s delays for plain notes (minion messages 16, 20, 21). By design; see task P-1 for a mid-turn option.
- A turn's worth of tmux calls per agent per tick (`loop.go:322-341`); not measured as a problem.

## D. Fix proposals as independent, file-disjoint tasks

Each task owns its files exclusively. `cmd/daemon.go` is owned by H-4 only; `internal/supervisor/loop.go` by H-1 only; `internal/supervisor/health.go` by H-2 only; `internal/hooks/hooks.go` by I-2 only. Dependencies are listed.

**H-1 Wake gating robustness** (M). Files: `internal/supervisor/loop.go`, new `internal/supervisor/wakegate_test.go`.
- If `prepareAssignments` fails for an agent, still wake it for its non-assignment mail, hold only the failed assignment, retry the switch every tick, and after 5 minutes of failure raise an escalation visible to the **user** (not only orch).
- Record why mail is waiting: per agent `wake.skip.<id>` = `reason|since` in kv for each skip branch (`held`, `slot`, `focus`, `attached`, `modal`, `prep_failed`, `busy`), cleared when woken. This is the data H-4 displays.
- Count a "working" agent toward the slot cap only if it showed hook or tool activity within `StaleAfter`.
- After `MaxUnansweredWakes`, re-arm with backoff (one more wake every 10 minutes) instead of holding forever; if the stuck agent is the orchestrator, notify the user directly.
- Verify: `go test ./internal/supervisor -run 'Wake|Gate'`; new tests: failing switch still delivers a note; skip reasons recorded; phantom working agent does not consume a slot.

**H-2 Presence and health** (M). Files: `internal/supervisor/health.go`, new `internal/supervisor/presence.go`, tests `internal/supervisor/health_presence_test.go`.
- Introduce one `userPresent(ctx, agent)` helper (focus window or attached client) with a policy: still never type over a user who is *active*, but if the agent has unread mail older than 3 minutes and the attached client has been idle (`#{client_activity}`) for 3 minutes, raise a user-visible escalation `attached_blocking` ("a tmux client is attached to eng-1 and mail is waiting; detach or press enter in its pane") instead of silence. `ClientCount` errors stop counting as attached.
- `oldUnread` counts mail whose wake was *skipped* for presence reasons.
- `hooksAgent`: exempt agents waiting on a background task (reuse the `liveness.go` footer reader); the alarm text stops naming `hook.log` when it does not exist.
- Depends on: H-1 for the kv reason keys (can land after).
- Verify: `go test ./internal/supervisor -run 'Hooks|Unread|Presence'`.

**H-3 Presence call sites and honest board check** (S/M). Files: `internal/supervisor/stuck.go`, `internal/supervisor/driver.go`, `internal/supervisor/redirect.go`. Switch their `ClientCount` checks to the H-2 helper; fix `taskLine` and the digest as in C.10. Depends on H-2. Verify: `go test ./internal/supervisor -run 'Digest|Drive|Stuck'`; new test: a busy assignee with an old `UpdatedAt` is not listed under "Need a decision".

**H-4 Say why mail is waiting** (S/M). Files: `cmd/daemon.go` (status), `cmd/doctor.go`, `cmd/doctor_hooks.go`, tests `cmd/status_waiting_test.go`. Show per agent the kv reason from H-1/H-2 ("eng-1: 2 messages waiting, a tmux client is attached; detach or `slai resume`"), the trust-prompt case ("orch is waiting for you to trust this folder: `slai attach orch`"), and the unreadable-hook-log wording. Depends on H-1, H-2. Verify: golden output test per reason; `go test ./cmd`.

**H-5 Test hygiene** (S). Files: `internal/e2e/*.go` only. Every e2e test registers a `t.Cleanup` that runs `slai down`, kills its `tmux -L` server and the supervisor, and fails if processes remain; serialize the real-tmux tests (`t.Setenv`-based lock) so a full `go test ./...` does not run a dozen supervisors at once. Verify: `go test ./internal/e2e` then `pgrep -f slai-e2e` is empty.

**I-1 Identity text in the restart wake and orchestrator notice** (S). Files: `internal/supervisor/loop.go` is H-1's, so this goes through H-1 as one extra bullet: the restart variant of the wake line starts with "You are <id>, the <role>." (spec C3). The notice to the user when the orchestrator itself restarted without context lives in I-2.

**I-2 SessionStart bookkeeping** (S). Files: `internal/hooks/hooks.go`, `internal/hooks/hooks_test.go`. Store `source` in the `hook:sessionstart` event data; on a session id that differs from `.slai/run/<id>/session-id`, rewrite the file; `compact`/`clear` raise the C5 re-ground; tell the *user* when the orchestrator revived fresh (`hooks.go:285-289` today tells only other orchestrators). Verify: new test that `/clear` (new session id) updates the file, `go test ./internal/hooks`.

**I-3 Doctor: stale run files and shared sessions** (S). Files: new `cmd/doctor_identity.go` + test (the existing `cmd/identity.go` is not touched). Red when `mcp.json` lacks `--agent`, `settings.json` lacks `env`, `system.md` is empty, or two agents share a session id in recent `hook:sessionstart` events. Verify: `go test ./cmd -run DoctorIdentity`.

**P-1 Mid-turn delivery spike** (M, optional, after the identity tasks because it touches `internal/claude/launch.go` and `cmd/hook.go`). Add `PostToolUse` to `HookEvents` and, when the agent has unread mail and the hook is allowed, return `additionalContext: "[slai] You have N new items. Call get_inbox."` so a busy agent learns of mail between tool calls rather than after its turn. **Unverified**: whether Claude Code 2.1.309 honours `additionalContext` from PostToolUse; the first step is a one-hour experiment with a scratch hook. If it works, 5-10 minute waits for engineers drop to seconds.

Order: H-1, H-2 and I-2 can start now in parallel; H-3 and H-4 after H-2/H-1; H-5 and I-3 independent; P-1 last.

## E. Open questions (with my recommended default)

1. **Attached client policy.** Stay "never type over an attached user", plus surface it (H-2)? Or auto-detach clients idle for N minutes? Default: surface only; never touch a user's client.
2. **Should a failed assignment switch block the engineer at all?** Default: no; deliver other mail, retry the switch (H-1).
3. **Per-hop wake cost.** Should mail to an idle orchestrator be batched (e.g. wait 2 s for more)? Default: no; latency is already 1 to 4 s.
4. **Mid-turn delivery (P-1).** Worth the risk of interrupting an agent's tool chain? Default: spike first.
5. **Message-loop threshold.** 10 chat messages in 10 min trips on normal review ping-pong. Default: keep 90 s hold; stop asking the user for it (log only) unless it trips three times in an hour.
6. **`max_concurrent_agents` default for `squad`** (8 agents, 5 slots). Default: set to 8 like `full`, since idle agents do not hold slots.
7. **Which team is "the other project"?** I found real evidence in `/tmp/minion` (older build). If the user means another project, its `.slai/state.db` and `.slai/run/<agent>/mcp.json` would confirm whether it is the same old build: the quickest check is whether `mcp.json` has `--agent` in `args`.

## F. What I did not verify
- Five-wake lock-out (C.4) and phantom-working slot starvation (C.5) are code-read only.
- The hook-silence false alarm (C.7) is code-read; I did not get an agent into that state.
- Resume with a *changed* `system.md`; pi and opencode behaviour after `/clear`/`/compact`.
- The raw `source` value of SessionStart (slai discards it).
- My repro teams were torn down with `slai down`; the repro scratch directory is under the session scratchpad, not in the repo.

## P-1 result (T-0044): mail reaches a busy agent between tool calls

**Claude Code 2.1.309 honours `additionalContext` returned from a `PostToolUse` hook.** Test, scratch repo outside this tree: a `PostToolUse` command hook printed `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"... SECRET-CODEWORD is PINEAPPLE-7731 ..."}}`; the model (haiku, `claude -p`) ran one `echo hello` and, asked for the codeword it had been given after the tool ran, answered `PINEAPPLE-7731` (and the stream-json log shows the text in the turn). Without the hook the codeword cannot be known, so the context really reaches the model mid-turn.

**What shipped.** `PostToolUse` is now one of `HookEvents` (`internal/claude/launch.go`), and `slai hook posttooluse` (`cmd/hook_midturn.go`, called from `cmd/hook.go`) answers with `[slai] You have N new items. Call get_inbox.` when mail arrived while the agent was in its turn. It records no event and changes no status, delivers nothing (only `get_inbox` does), and:
- announces each message at most once (kv `postnotice.<agent>` = highest id announced);
- skips mail the wake loop already typed into the pane (`woken_at != 0`), and any tool call that is the agent's own `get_inbox`;
- never bypasses a hold: paused agent, global pause, team hold, loop-guard hold, or the wake loop's own `held` skip reason (covers spend caps);
- opens with the identity sentence when a context gap left `identity.pending` set (then clears it), like a wake-up line.

**Measured** (scratch repo, solo preset, eng-1 on sonnet doing six sequential 12 s tool calls; a probe message sent 22 s in; "read" = the time `get_inbox` marked it delivered):

| build | probe read after | what happened |
|---|---|---|
| before (integration) | **67 s** | waited for the turn's Stop hook (+65 s), then the wake line, read 2 s later |
| after (this change) | **13 s** | read at the agent's next tool call, mid-turn |

One earlier run of each (with a first message that told the agent to call `get_inbox` at the end) was discarded: it measured the agent's own final `get_inbox` (65 s and 52 s), not delivery. Limits: one sample per build; the agent still decides when to act on the notice (here it did so at once); a hook process now also runs after every tool call (PreToolUse already did), a few tens of milliseconds each.
