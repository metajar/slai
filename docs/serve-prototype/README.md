# slai serve: clickable prototype (T-0087, phase 1 of `.slai/specs/serve.md`, incl. its 2026-10-22 addenda)

A static, fake-data prototype of the web UI the focus group reviews **before** any real build. Nothing here talks to a team: the data is invented from this very project (agents `orch`…`fg-*`, tasks T-0071..T-0087), and every button only shows what the real one would do.

## Open it

```sh
python3 -m http.server -d docs/serve-prototype 8000   # then http://127.0.0.1:8000/
# or just open docs/serve-prototype/index.html from file://
```

Light/dark follows the OS and has a toggle (top right); `?theme=dark` forces one. Works down to 360 px (the sidebar becomes a menu, the board scrolls sideways).

## Pages (all linked from the shared sidebar)

| Page | File | Spec | What to try |
|---|---|---|---|
| Dashboard | `index.html` | P2 | health chips with reasons, old-build / orphan-team banners, spend vs caps, rate-limit gauges (with an `n/a` + reason), **Hold team** (header: name it, a banner appears on every page, release it) |
| Dashboard, first run | `index-first-run.html` | P2.4 | nothing started yet: names `orch`, goal box addressed to it, “caps OFF” notice |
| Agents | `agents.html` | P3 | search + status/harness filters, **New agent** button |
| New agent | `agent-new.html` | P3.6 | a working form: role, harness, id, focus, model, permission mode (+ persona for focus groups; pi hides permission mode). Validator messages inline, duplicate id refused, success state shows the `team.yaml` entry. `#demo-error` / `#demo-success` jump to each state |
| Agent detail | `agent.html` | P3 | escaped, coloured read-only pane (incl. an XSS line that stays text), send / **interrupt + redirect**, pause/resume, a “not safe to type” refusal demo, health events with causes, lane denials, message history. **Restart fresh, revive and reassign are shown disabled with the TUI way to do them** (CLI/TUI-only in v1) |
| Board | `board.html` | P4 | **drag cards** between columns: only cancel, unblock and in-column reorder work; green/red outline while dragging; illegal drops snap back with the reason in a toast (`Demo: illegal move` button, or `board.html#demo-illegal`); filters kept in the URL; **New task** with a service-error demo; `waiting on` (amber) vs `blocked` (red) |
| Task detail | `task.html` | P5 | description, deps, AC claims vs verdicts (incl. “unanswered”), collapsible unified diff, rendered spec, timeline, read-only review path |
| Reviews | `reviews.html` | P6 | queue oldest first, diff, `merge_refused` output, verdict history; **no approve/merge button** by design |
| Inbox | `inbox.html` | P7 | answer an `ask_user` (badge and tab-title count drop), dismiss an escalation (UI-only, says so) |
| Stats | `stats.html` | P8 | quality by agent and by harness/model (same columns as `slai status --stats`), spend vs caps, `n/a` with reason |
| Memory | `memory.html` | P10 | facts with fresh / stale / unverified / invalid, search + filter, click a row for cited-path blob comparison and “used by” (read-only) |
| Doctor | `doctor.html` | P9 | checks with fix hints, a warning banner as exit state; `--lane` / `--panes` results of the last run with a copyable command (running probes is CLI-only in v1) |
| Log | `log.html` | P9 | filters by agent / kind / task, pagination footer, hook events off by default |
| Commit preview | `commit.html` | P11 | `slai commit --dry-run` and `slai revert` plans, read-only |
| Sign-in | `signin.html` | addendum: auth | the token page a non-loopback visitor sees; wrong-token state (`#demo-error`); the “token holder can see every pane” warning |
| Read-only mode | `index-readonly.html`, `board-readonly.html`, `agent-readonly.html` | addendum: `--read-only` | same pages with the banner and every write switched off (send, hold, new task/agent, drag) |

## Screenshots

`screenshots/<page>-light-1280.png`, `-dark-1280.png`, `-light-360.png` for every page (18 × 3), plus the state shots `board-illegal-move-*` (refusal toast), `agent-new-error-*`, `agent-new-success-*` and `signin-wrong-token-*`.

## How it was checked

- `tools/shoot.cjs` drives headless Chrome (via `puppeteer-core`) over every page at the three sizes, fails on any console error/warning, page error, failed request, request that is not a local file, or horizontal page scroll at 360 px. Last run: 66 screenshots, no problems.
- `tools/check-board.cjs` drags real cards with the mouse (SortableJS fallback mode, `?fallback=1`): illegal drops snap back with the refusal toast, cancel and unblock stick, counters stay right; in the read-only variant the drag does nothing.
- No external network: no CDN, fonts included. The only `http://`/`https://` strings are license comments and the `xmlns` SVG namespace inside the vendored files (`vendor/VENDOR.md` lists versions and sha256).
- No inline scripts (the real server will ship a strict CSP); all behaviour is in `assets/app.js`, theme init in `assets/theme.js`.

## Regenerate

The pages are generated so the layout stays consistent (plain Python, no dependencies):

```sh
python3 docs/serve-prototype/tools/gen.py          # rewrites the *.html files from tools/data.py + tools/gen.py
npm i puppeteer-core                                # anywhere; then:
NODE_PATH=/path/to/node_modules node docs/serve-prototype/tools/shoot.cjs
NODE_PATH=/path/to/node_modules node docs/serve-prototype/tools/check-board.cjs
```

## What it shows about v1 (from the addenda)

- **Writes on day one**: send, redirect, pause/resume/hold, priority (drag within a column), answer `ask_user`, cancel, unblock, create task, **new agent**. Everything else is CLI/TUI-only and is drawn as a disabled control plus the command or key (restart, revive, reassign, doctor probes, memory edits, commit/revert apply). Approve and merge do not appear at all.
- **Pane**: escaped, colour and bold only, last 200 lines / 64 KB, pushed when the pane changes (no polling).
- **JS**: the real build embeds only htmx and SortableJS (plus Tabler’s own assets). This prototype is static, so it uses SortableJS + Tabler; `assets/app.js` and `assets/theme.js` are throwaway scripts that fake the behaviour and are not part of the build.

## Not in the prototype (on purpose)

Real data, SSE, auth/CSRF, htmx: this answers “is this the right UI?”, not “does the server work?”. See the spec for the build plan.
