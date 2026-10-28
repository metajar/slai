---
id: architecture-agent-communication
type: architecture-design
status: draft
title: Agent communication and delivery
tags: [agent-communication]
---

## Drivers

Agent turns can be long, tmux input can collide with a user or modal prompt, and an engineer's worktree is shared across successive tasks. A wake is an invitation to fetch mail, not the mail itself. Delivery must remain safe when messages arrive during a turn or between supervisor checks.

## Decisions

The shared SQLite inbox is the communication boundary across the service, supervisor, hooks, and TUI. Agents and user prompts enqueue messages; only the supervisor types wake-ups into an idle, safe pane. A busy agent can learn of *claimable* mail through PostToolUse additional context without starting a new turn or consuming the item. The TUI quick prompt uses that same queue, even when a user is answering an agent waiting for input.

Engineer assignments acquire a per-message inbox hold atomically with insertion. The supervisor releases it after switching to the task branch. A dependency-blocked task releases an explicit wait notice; an otherwise startable assignment queued behind active work stays held until the later worktree switch. The hold is per item so other mail is not delayed. This boundary is necessary because refreshing a wake snapshot alone cannot prevent a concurrent inbox claim. Late arrivals are merged by message ID at the last-moment per-recipient refresh, not chosen by list length.

Wake candidates and outstanding asks/pings are selected by recipient. An owed reply remains eligible throughout its time window regardless of unrelated team traffic; explicit replies, direct answers to an ask, and pong acknowledgments close that obligation. Wake timestamps and hold lookups are batched to avoid per-message round trips during a backlog.

## Invariants

- `get_inbox` and mid-turn notices agree on which undelivered items are claimable; neither exposes a held engineer assignment.
- Persisting a user prompt precedes any attempt to wake its recipient. If persistence fails, nothing is typed.
- A failed worktree switch cannot expose that assignment, and it does not block unrelated mail.
- A supervisor wake is gated by presence, status, spend/slot limits, and pane safety; the TUI never bypasses those gates.

## Out of scope

The supervisor's tick remains serial and polling-based; this change does not replace SQLite with a push transport or guarantee exactly-once processing after an agent claims an inbox response.
