# `slai serve`: real screenshots of every page

Screenshots of the **real UI** (not the prototype) over a **fake, seeded project**, for the
second focus-group round (spec `serve.md`, addendum T-0102, open question 3). Personas can read
the PNGs from the integration worktree.

## What you are looking at

`slai serve --demo` (hidden flag) builds a throw-away project and serves it. Nothing in it is
real: the agents, tasks, reviews, spend and panes are invented, and it never touches the project
you run it from. The demo has:

- a team of nine agents in every health state (busy, stale, blocked on a question, rate
  limited, dead, idle) with a daily cap at 76% and a per-agent cap at 85%, a 5-hour rate limit
  and a weekly one that says `n/a`;
- thirteen tasks in every column (assigned, in progress, blocked, in review, merged, cancelled,
  reverted, changes requested), one waiting on another, two in review with AC claims, one of
  them part-answered, a `merge_refused` with its test output;
- an open `ask_user` question, escalations, messages, lane denials, health events;
- memory facts: verified, proposed, stale (its file changed on integration) and one invalid;
- canned panes (colour, an XSS line and a fake key that stay text), canned commit/revert
  previews and doctor report.

## Run it

```sh
slai serve --demo                       # prints http://127.0.0.1:PORT/ ; Ctrl-C ends it
slai serve --demo --demo-first-run      # the same team with nothing asked yet
slai serve --demo --read-only           # every write refused
slai serve --demo --demo-dir /tmp/d     # keep the project in /tmp/d (must be new or an old demo)
```

`--demo` refuses a directory that is not empty and not a previous demo, so it cannot be pointed
at a real project. The clock is fixed (2026-10-22 14:30 UTC), ids are fixed, and `TZ=UTC` is set
by the script, so two runs show the same pages.

## Re-capture

```sh
docs/serve-screens/capture.sh            # needs go, Node 22 and google-chrome
OUT=/tmp/shots docs/serve-screens/capture.sh
```

It builds the binary, starts four demo servers (main, first-run, read-only and, when the machine
has a non-loopback address, a LAN one for the sign-in page), and drives headless Chrome over the
DevTools protocol (`capture.mjs`, no npm packages) at **1280 light, 1280 dark and 360 light**.
It fails on a console error, a failed request or a request to another origin, and warns when a
page scrolls sideways at 360 px.

`pages.txt` lists the pages; a test (`TestEveryPageRouteHasScreens`) checks that every page
route of the server has a line there and that each line has its three PNGs.

## Files

`<name>-light-1280.png`, `<name>-dark-1280.png`, `<name>-light-360.png` for each page in
`pages.txt`, and these states:

| name | what it shows |
|---|---|
| `dashboard-first-run` | nothing asked yet: names the orchestrator, send box |
| `dashboard-read-only`, `board-read-only`, `agent-detail-read-only` | `--read-only`: banner, writes off |
| `sign-in` | what a LAN visitor sees without the token |
| `board-illegal-move` | the refusal toast for a card dropped on Merged (the same request a drop sends) |
| `agent-new-error`, `agent-new-success` | the New Agent form refused (bad id) and written |
