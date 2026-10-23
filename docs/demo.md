# Demo: a 2-agent `solo` run with measured cost

A tiny, repeatable goal on the `solo` preset (an orchestrator and one engineer), with what it cost.

- **Run date:** 2026-10-23
- **Models:** the preset's defaults: orch on `opus` (recorded by Claude Code as `claude-opus-5-5`), eng-1 on `sonnet` (`claude-sonnet-5-5`)
- **slai:** built from `slai/integration` at 719c46f
- **Goal:** add `Add(a, b int) int` and a table-driven test to a one-file Go package.

## Run it

Needs `tmux`, `git`, `go` and `claude` (check with `slai doctor`). Everything below runs in a throwaway directory.

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

Start the team and give orch the goal. Plain `slai` is the normal way to start (it starts the agents and supervisor and opens the dashboard). This demo uses the headless `slai up` instead, which does the same start without the dashboard, so every step is a copyable command; run `slai` if you would rather watch:

```sh
slai up
```

Claude Code asks "trust this folder?" once for the orchestrator. Run `slai attach orch` (or open `slai`), choose "Yes, I trust this folder", and detach. eng-1 starts by itself after that.

```sh
slai send orch 'Goal: in demo.go add a function Add(a, b int) int that returns a+b, and a table-driven test in demo_test.go. go test ./... must pass. Keep it minimal; one task for eng-1.'
```

Wait for `slai status` to show both agents `finished` (this run: about 2 minutes 15 seconds from sending the goal), then check the result:

```sh
slai status
slai status --stats
git log --oneline slai/integration
git show slai/integration:demo.go slai/integration:demo_test.go
sqlite3 -readonly .slai/state.db \
  "select agent, count(*), sum(input_tokens), sum(output_tokens), round(sum(cost_usd),4) from usage group by agent"
slai down
```

## What I saw

`slai status` ended with the orchestrator's summary:

```
orch         orchestrator         opus     finished
eng-1        engineer             sonnet   finished

run finished. Final summary from the orchestrator:
  Built: `func Add(a, b int) int` in demo.go ... and a table-driven TestAdd in demo_test.go with 5 cases ...
  Lives on branch slai/integration (T-0001 merged, commit 638ec43) ...
```

`git log --oneline slai/integration` showed one task, merged:

```
58e0780 Merge [T-0001] Add Add(a, b int) int with table-driven test
638ec43 [T-0001] Add Add(a, b int) int with table-driven test
165d1e6 init
```

`go test ./...` in the `slai/integration` checkout printed `ok  demo`.

## Measured cost

Spend comes from slai's own usage records (`usage` table in `.slai/state.db`, estimated by slai from the agents' transcripts and its price table, so it is slai's figure, not an invoice):

| agent | records | input tokens | output tokens | cost |
|-------|---------|--------------|---------------|------|
| orch (opus) | 2 | 351,700 | 1,192 | $0.35 |
| eng-1 (sonnet) | 2 | 350,892 | 1,141 | $0.20 |
| **total** | | 702,592 | 2,333 | **$0.55** |

- `slai status --stats` showed only eng-1 ($0.20): it lists agents that merged a task, so the orchestrator's $0.35 appears only in the raw table above.
- The input-token counts are large because each record counts the whole context the model read, not new text. Do not expect them to be proportional to the size of the goal.
- A single run is one sample. Cost varies with the goal, the models and how many review rounds happen. Here there was one task, one review round, no rework.

## Notes from this run

- Fixed since this run (T-0005): at the time, `slai init` printed "Starting 2 Claude sessions" and `slai status` right after it printed only "supervisor: NOT running", although nothing starts until `slai up` (or `slai`). Init now says "Will start 2 Claude sessions when you run `slai`" and status lists the agents and ends "Next: `slai`".
- Fixed since this run (T-0010): the `slai up` cost line ("Spend caps: OFF") only reported the per-agent and daily caps (`cmd/cost.go`), so `per_run_usd: 3`, which was active in this run (it would have paused the team at $3), showed as OFF. The line now lists every cap that is set and prints OFF only when all four are unset; with `per_run_usd: 3` it ends "Spend caps: per-run $3".
