# Identity: manual check against real Claude sessions

Run this before a release that touches identity, wake-up lines, hooks or launch flags. The automated test (`go test ./internal/e2e -run Identity`) uses scripted agents, so it proves what slai *says* to an agent. This check proves that a real model still knows who it is after each kind of context gap.

Pass means: after every step below, the orchestrator, the principal and an engineer each answer with their true id and role, and `whoami` agrees. It costs about $2 with every agent on `sonnet` (the run recorded below cost $1.92) and takes about 10 minutes.

## Setup

```sh
mkdir idcheck && cd idcheck && git init -q
printf 'module demo\n\ngo 1.22\n' > go.mod && printf 'package demo\n' > demo.go
git add -A && git commit -qm init
slai init --yes --preset squad --name idcheck --description "identity check"
```

Keep only `orch`, `principal` and `eng-1` in `.slai/team.yaml` (delete the other agent blocks) and set their `model:` to `sonnet` to keep the cost down. Then:

```sh
slai up
```

Claude Code asks "trust this folder?" for `orch`: `slai attach orch`, choose "Yes, I trust this folder", detach. The other two start by themselves. Do **not** leave a tmux client attached while testing: slai never types over an attached client, so wake-ups would not be sent (see `docs/reviews/comms-reliability-review.md`, C.2).

Find the sessions with `tmux ls` (names look like `slai_<hash>_orch`). Below, `S` is one of them. All typing is done without attaching: `tmux send-keys -t =S: -l '<text>'` then `tmux send-keys -t =S: Enter`.

## The question

At each checkpoint, ask all three agents (this is a plain user message, not an identity line):

```sh
for a in orch principal eng-1; do
  slai send $a "Identity check (<label>): reply in one sentence with your agent id and your role, taken from your own knowledge, then call whoami and say whether it agrees. Do nothing else."
done
```

Wait about 50 seconds, then read each pane: `tmux capture-pane -p -S -60 -t =S: | tail -12`.

**Expect**: each answer names the right id (`orch`, `principal`, `eng-1`) and role (Orchestrator, Principal engineer, Engineer) *before* it mentions whoami, and says whoami agrees. A wrong id, a different role, "I don't know", or a whoami that disagrees is a failure: stop and file it with the pane text.

## Checkpoints

1. **Baseline** (label `baseline`). Ask the question.
2. **Compaction.** Type `/compact` into each pane. After about 30 s check, for each agent:
   - `sqlite3 -readonly .slai/state.db "select agent,data from events where kind='identity_reground'"` shows `{"source":"compact"}`.
   - The first line slai typed after the compaction begins `You are <id>, the <role> of this team (see whoami). ...` (visible in the pane right below `Compacted`; the orchestrator's also says "You never write code").
   - Ask the question (label `after-compact`).
3. **/clear.** Type `/clear` into each pane. The same checks with source `clear`: the pane shows `❯ /clear` followed by the identity line. Ask the question (label `after-clear`).
4. **Restart that resumes the session.** Kill each session (`tmux kill-session -t =S`), then `slai up`. It prints `started 3 agent(s)`. The new launch uses `--resume <session id>` (`ps -eo args | grep 'claude --model'` shows `--resume '<id>'`, then the fallback `--session-id` alternatives after `||`). The first line typed starts with the identity sentence. Ask the question (label `after-resume`).
5. **Fresh session.** For each agent `rm .slai/run/<agent>/session-id`, kill its session, `slai up`. `.slai/run/<agent>/session-id` now holds a **new** id and the launch has no `--resume`. Ask the question (label `fresh`). (A fresh conversation starts from the system prompt, which names the agent, so no identity line is required here.)

## Cleanup

```sh
slai down
tmux ls | grep slai_   # none of this team's sessions remain
```

## Known limits (as of this check)

- Before I-2 (T-0040) `/clear` left `.slai/run/<agent>/session-id` holding the pre-clear session id, so a later `--resume` brought back the cleared conversation (the result below was recorded before it). Now the file follows the session id reported by `SessionStart`; checkpoint 4 resumes the post-clear conversation, and `go test ./internal/e2e -run Identity` asserts it.

## Result of the last run

2026-10-23, slai at `slai/integration` + T-0033, Claude Code 2.1.309, all three agents on `sonnet` (Sonnet 5.5), three-agent team (orch, principal, eng-1), cost $1.92 by slai's usage table (orch $0.77, eng-1 $0.61, principal $0.54).

| checkpoint | orch | principal | eng-1 |
|---|---|---|---|
| baseline | pass | pass | pass |
| compaction (+ identity line typed first, event `compact`) | pass | pass | pass |
| /clear (+ identity line typed first, event `clear`) | pass | pass | pass |
| restart, `--resume` (+ identity line typed first) | pass | pass | pass |
| fresh `--session-id` | pass | pass | pass |

Every answer named the correct id and role and said `whoami` agreed.
