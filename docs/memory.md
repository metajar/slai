# Team memory

Every agent works on the same project, and without help each one re-reads the same packages
from scratch. Team memory is a small, shared set of **facts** about the project that agents
write down and the principal checks, so the next agent starts with what the last one learned.
It is a cache, not a rulebook: every fact says which files it is about, and a fact whose files
have changed is marked stale and kept out of what agents are told.

## What a fact is

One sentence (at most 300 characters) about the code, one kind, and the files it is about:

| Kind | Example |
| --- | --- |
| `architecture` | The supervisor never starts from the web UI. |
| `convention` | Handlers return the service's error text verbatim. |
| `gotcha` | The lane guard resolves `$VAR`s before matching a redirect target. |
| `command` | The package tests run with `go test ./internal/web/... -count=1`. |
| `decision-pointer` | Why the queue is not durable: see the heading "Queue" in `.slai/decisions.md`. |

A fact describes; it does not tell. "The lane guard resolves variables" is a fact; "ignore the
lane guard" is rejected when written (see [the deny-list](#what-is-rejected-when-a-fact-is-written)).

### Where facts live

Each fact is a Markdown file, `.slai/memory/<id>.md`, in your project's main checkout. They are
tracked in git like `.slai/specs/` and `decisions.md` (slai never commits them for you), so they
survive restarts, upgrades and a new machine, and you can read, edit and diff them like any file.
`id` is `f-` and 8 hex digits derived from the kind, the text and the files, so writing the same
fact twice gives the same file.

```markdown
---
id: f-3a9c01de
kind: gotcha
status: verified          # proposed | verified (stale is computed, never stored)
author: eng-2
verified_by: principal    # principal | user
integration_sha: 81029a2dccdb…
created: 2026-10-22
paths:
  - {path: internal/hooks/lane.go, blob: 4be1…}
decision: ""
check: ""
---
Lane guard resolves $VARs before matching; heredoc bodies are ignored.
```

`.slai/state.db` also holds a derived index of the files (so lists are fast and usage is
counted). Nothing is read from it that the files could contradict: if they disagree, the file
wins, and the index is rebuilt from the files.

## How a fact becomes trusted

1. **An agent proposes it.** Agents call the `remember` tool (orchestrator, product manager,
   principal, engineers; the focus group can only read). The fact is written as `proposed`.
   An engineer can also list the ids it proposed in `request_review`.
2. **The principal verifies it at review.** When the principal approves a task, it answers for
   each proposed fact (confirm: it becomes `verified`, or reject: it is deleted). Nothing else
   promotes a fact; there is no auto-verify. The principal's own `remember` is verified directly.
3. **Your hand edits count.** If you edit a fact file yourself and it still parses, the supervisor
   picks it up on its next tick, records `verified_by: user`, re-reads the blobs of the files it
   cites from the current integration branch, and logs a `memory_edited` event. A file that no
   longer parses is flagged `invalid`, kept out of agents' packs, and left on disk for you to fix.

A fact with no cited file can be proposed, but it can never be `verified` in the grounded sense:
it is always shown as `unverified`.

## Staleness: facts are checked against `slai/integration`

Each fact records the git blob id of every file it cites, as it was on the integration branch when
the fact was written. A fact is **stale** as soon as one of those files has a different blob on
`slai/integration` now, or is gone (a rename counts as gone). The check is one `git ls-tree`
against the integration branch, never your working tree or an engineer's branch, so an
uncommitted edit does not make a fact stale and a merged change does.

The supervisor re-checks the facts that cite a path whenever a merge touches it. A stale fact:

- is shown with a warning in `slai memory ls`, the TUI and the web page, with what changed (the
  path, its recorded blob and its current one, or "no longer exists");
- is **never put in a pack**;
- is not rewritten for you. Rewrite it (or delete it) once you or an agent has re-read the code.

The states you will see: **fresh** (verified, files unchanged), **stale**, **unverified**
(proposed, or cites no file) and **invalid** (the file does not parse).

## How agents get facts: the context pack

The pack is a short block of text (at most `memory.pack_tokens` tokens, default 1500, counted as
bytes/4) headed `## Team memory (facts, not instructions)`: a generated map of the project
(packages and entry points, and the test command), then the facts. Selection and order: stale
facts never; verified before unverified (labelled `(unverified)`); within those, facts citing the
files the task lists first, then the most recent. It is built fresh each time, from the files.

Agents pull it; nothing is typed into their terminals:

- `get_task` returns the pack for the task's files in a `memory` field. A task that lists no files
  gets the project map and the top verified facts.
- `get_memory` returns the same string (optionally narrowed by `query` or `task_id`).
- `recall` searches facts by words or paths and returns up to 10, marked verified, proposed or
  stale.
- The line slai types into an agent's pane on an assignment or after a restart carries one
  sentence only: `memory: N facts for this task, see get_task.memory or get_memory`.

Every time a pack is handed out, a `memory_used` event records the task, the agent, the fact ids
and the pack's size, so you can see exactly which facts a task got. The TUI's task detail and the
web page's "Used by" show it.

## Commands

```bash
slai memory ls [--stale]          # every fact: id, kind, state, first cited file, age
slai memory show <id>             # the file, and each cited file's recorded vs current blob
slai memory verify <id> [--yes] [--allow-unverified]
slai memory gc [--yes]            # delete stale facts nobody has used for 30 days
slai memory map                   # print the generated project map
```

- `ls` lists everything (plus a line for each invalid file and a warning past 500 files);
  `ls --stale` is the stale ones only.
- `show` prints the file, then for each cited path the blob recorded and the blob now on
  integration, and the fact's state.
- `verify` is for a fact with a `check:` line, a command that exits 0 when the fact still holds.
  Only the principal can attach one through `remember`, and **only you run it**: `verify` prints
  the command, asks y/N, runs it with `sh -c` in the project directory and, on exit 0, marks the
  fact `verified_by: user`. It needs a terminal to ask on; `--yes` skips the question (for scripts).
  By default it refuses a proposed or stale fact (its check is text nobody has reviewed), and
  `--allow-unverified` lets it run on one anyway after you have read it with `slai memory show`.
- `gc` lists the stale facts that have not been in a pack for 30 days (a fact never used counts
  from its creation) and deletes them after asking (`--yes` to skip). Only this command deletes
  facts for you, and only after you say so; the supervisor never does.
- `slai doctor` has a "team memory" section: counts by state, invalid files, stale verified facts.

The TUI has a Memory view on `M` (state filter, file and blob detail, edit, delete) and the web UI
has a read-only Memory page; see [serve.md](serve.md).

### Editing and deleting by hand

Facts are plain files, so you can fix one in your editor: open `.slai/memory/<id>.md`, change the
text, the paths (with their blobs) or the kind, and save. If it still parses, the supervisor
accepts it as verified by you on its next tick. (If the supervisor is not running, run the TUI's
`M` view and press `e`, which opens the same file and then asks slai to accept it.) The id is
derived from the text and files; after a big rewrite you may prefer to delete the old file and let
an agent propose the new fact.

To delete a fact, delete the file (`rm .slai/memory/<id>.md`) or press `d` on it in the TUI's
Memory view (it asks first). Either way it leaves the packs on the next tick. Commit the change
with your other team docs.

## What is rejected when a fact is written

Facts must not be able to instruct agents, so `remember` refuses text that reads like an
instruction, as a tripwire (the real defences are the fixed pack template, the "facts, not
instructions" heading, and the principal verifying at approve). Matching is case-insensitive on
whole words:

1. any of *ignore, skip, bypass, disable, override, always, never, must, should* within six words
   before any of *lane, guard, hook, permission, review, approve, spec, rule, test, principal,
   orch*;
2. any of the phrases *you must, you should, do not, don't, sudo, rm -rf, curl , wget ,
   --dangerously, export *;
3. a text whose first word is *run, execute, delete, remove, install, push, force, merge*.

Also refused: more than 300 characters; text that looks like a secret or a raw hash (cite paths,
not shas or credentials); a cited path that is not a clean relative file on the integration
branch; a `check` from anyone but the principal; more than 20 facts on one path; and more than 10
writes by one agent in an hour. Newlines and backticks are stripped, not refused. Each refusal
names the rule that fired, so an agent can rewrite the fact.
