# Demo: what a tiny goal costs, with and without the review gate

Two runs of the same tiny goal, with what each cost and what each number includes:

1. a 2-agent `solo` team (orchestrator + one engineer, no review gate);
2. a 3-agent team (orchestrator + **principal** + one engineer, review gate on), so the principal's review is part of the cost.

- **Run date:** 2026-10-23
- **slai:** built from `slai/integration` at 91e7ea9 (includes `slai status --stats` with spend of every agent, and T-0017)
- **Models:** the preset defaults: orch on `opus` (recorded as `claude-opus-5-5`), eng-1 on `sonnet` (`claude-sonnet-5-5`), principal on `fable` (`claude-fable-5-1`)
- **Goal (both runs):** add `Add(a, b int) int` and a table-driven test to a one-file Go package.

## What the cost numbers mean

All cost figures come from slai's own usage records (the `usage` table in `.slai/state.db`): slai estimates them from each agent's transcript with its price table, so they are slai's figures, not an invoice. Each record counts the whole context the model read, so token counts are large and not proportional to the goal.

- **spend (per agent):** all recorded usage of that agent in the window: every turn it took, task work or not. It includes the orchestrator and the principal.
- **total spend:** the sum of the spend of every agent. This is "what the run cost".
- **$/merged:** an agent's own spend divided by the tasks it merged. It exists only for agents that were assigned a task, so it is the *engineer's* cost per merged task. Orchestrator and principal spend is in the total, **not** in any `$/merged`; for the real cost per merged task divide the total by the merged count.
- **Per task** (`per_task_usd`, usage by `task_id`): each turn's usage is stamped with the agent's current task at the end of that turn. The assignee's turns carry the task. A reviewer's turns carry it from the turn in which it calls `review_diff` through the turn in which it calls `approve` or `request_changes` (whole turns: work done earlier in the `review_diff` turn is included too). Before T-0024 slai cleared the reviewer's charge when `approve`/`request_changes` was called, before that turn's usage was recorded, so a reviewer's last turn went to no task; the Run 2 numbers below were measured before that fix.

## Run 1: solo (no review gate)

```sh
mkdir demo && cd demo && git init -q
printf 'module demo\n\ngo 1.22\n' > go.mod
printf 'package demo\n' > demo.go
git add -A && git commit -qm init

slai init --yes --preset solo --name demo --description "tiny demo"
```

Optional safety net: add a spend cap under `defaults:` in `.slai/team.yaml` (this run used $3):

```yaml
  spend_caps:
    per_run_usd: 3
```

Start the team and give orch the goal. Plain `slai` is the normal way to start (it starts the agents and supervisor and opens the dashboard). This demo uses the headless `slai up`, which does the same without the dashboard, so every step is a copyable command:

```sh
slai up
```

Claude Code asks "trust this folder?" once for the orchestrator. Run `slai attach orch` (or open `slai`), choose "Yes, I trust this folder", and detach. eng-1 starts by itself after that.

```sh
slai send orch 'Goal: in demo.go add a function Add(a, b int) int that returns a+b, and a table-driven test in demo_test.go. go test ./... must pass. Keep it minimal; one task for eng-1.'
```

Wait for `slai status` to show the run finished (this run: under a minute), then:

```sh
slai status
slai status --stats
git log --oneline slai/integration
sqlite3 -readonly .slai/state.db \
  "select agent, count(*), sum(input_tokens), sum(output_tokens), round(sum(cost_usd),4) from usage group by agent"
slai down
```

What happened: one task (T-0001), merged by eng-1, no review (`solo` has no principal), no rework.

```
by agent:
                               merged can/rev rounds rework refused    spend $/merged    time
eng-1 claude/sonnet                 1     0/0    1.0     0%       0    $0.20    $0.20     16s
orch claude/opus                    0     0/0      -      -       0    $0.32        -       -

total spend: $0.52 (all agents)
```

| agent | records | input tokens | output tokens | cost |
|-------|---------|--------------|---------------|------|
| orch (opus) | 2 | 301,487 | 1,051 | $0.32 |
| eng-1 (sonnet) | 2 | 351,526 | 1,065 | $0.20 |
| **total** | | 653,013 | 2,116 | **$0.52** |

- Everything above is for the whole run: the orchestrator's planning and checking, the engineer's task, and nothing else. $0.52 per merged task is the honest number here (one task); eng-1's `$/merged` of $0.20 is only the engineer's part.

## Run 2: orchestrator + principal + engineer (review gate on)

Same repo and goal. The team is the `squad` preset cut down to `orch`, `principal` and `eng-1` (delete the other agents from `.slai/team.yaml`), with `require_review: true` (the squad default) and a run cap of $5:

```sh
slai init --yes --preset squad --name demo --description "tiny demo"
# edit .slai/team.yaml: keep only the agents orch, principal and eng-1, and under defaults add:
#   spend_caps:
#     per_run_usd: 5
slai up        # then trust the folder once, as above
slai send orch 'Goal: in demo.go add a function Add(a, b int) int that returns a+b, and a table-driven test in demo_test.go. go test ./... must pass. Keep it minimal; one task for eng-1.'
```

What happened: eng-1 built T-0001 and called `request_review`; the principal ran `review_diff`, checked the tests and approved on the first round ("Add is correct; TestAdd is table-driven ... all met"); the supervisor merged it. About 80 seconds from sending the goal to the orchestrator's final summary. One review round, no rework.

```
by agent:
                               merged can/rev rounds rework refused    spend $/merged    time
eng-1 claude/sonnet                 1     0/0    1.0     0%       0    $0.20    $0.20     53s
orch claude/opus                    0     0/0      -      -       0    $0.39        -       -
principal claude/claude-fable…      0     0/0      -      -       0    $0.96        -       -

total spend: $1.56 (all agents)
```

| agent | records | input tokens | output tokens | cost | of which charged to T-0001 |
|-------|---------|--------------|---------------|------|----------------------------|
| orch (opus) | 3 | 406,605 | 1,605 | $0.39 | $0 |
| eng-1 (sonnet) | 2 | 351,938 | 1,069 | $0.20 | $0.20 |
| principal (fable) | 2 | 497,481 | 2,335 | $0.96 | $0 |
| **total** | | 1,256,024 | 5,009 | **$1.56** | $0.20 |

- **The review gate roughly tripled the cost of this tiny task** ($0.52 without it, $1.56 with it), almost entirely the principal ($0.96, more than the engineer who wrote the code). On a bigger task the gate is a smaller share; this sample only shows it is not free.
- **The principal's review tokens were not charged to the task in this run.** Usage records are written when a turn ends, but `approve` ended the charge in the middle of that turn, so the review turn's $0.66 was recorded with no task (the table's last column is as measured). This was fixed by T-0024 after the run: the charge now ends only after the turn's usage row is written. With the fix, the review turn's $0.66 (the turn with `review_diff` and `approve`) would count toward T-0001, making its per-task spend about $0.86 ($0.20 eng-1 + $0.66 principal). The principal's other $0.30 is the next turn (the wake after the merge), which carries no task. I did not re-run the squad demo for this fix (about $1.56 per run); the change is covered by the tests `TestReviewerSpendCountsTowardTheTask` and `TestReviewChargeEndsViaStoreFallback`.
- One `lane_denied` event was recorded on the principal's second turn (after the merge); it did not affect the run.

## Reading the result

A single run is one sample: cost varies with the goal, the models and the number of review rounds. These runs had one task, one review round and no rework. To cap what you spend, set `defaults.spend_caps` in `.slai/team.yaml` before `slai up`; `0` or leaving a cap out means no cap.
