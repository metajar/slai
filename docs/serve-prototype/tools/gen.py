#!/usr/bin/env python3
"""Generates the static pages of the slai serve prototype (run from anywhere:
python3 docs/serve-prototype/tools/gen.py). Plain HTML out, no template engine, no network."""
import html, os, sys
sys.path.insert(0, os.path.dirname(__file__))
from data import *

OUT = os.path.normpath(os.path.join(os.path.dirname(__file__), ".."))
e = html.escape

NAV = [
    ("index.html", "Dashboard", "layout-dashboard", None),
    ("agents.html", "Agents", "robot", None),
    ("board.html", "Board", "layout-kanban", None),
    ("reviews.html", "Reviews", "eye-check", "2"),
    ("inbox.html", "Inbox", "message-question", "needs"),
    ("stats.html", "Stats", "chart-bar", None),
    ("memory.html", "Memory", "brain", None),
    ("doctor.html", "Doctor", "stethoscope", None),
    ("log.html", "Log", "list-details", None),
    ("commit.html", "Commit preview", "git-pull-request", None),
]
SCREENS = {}  # page file -> title for the README

def chip(key, text=None):
    c, ic, lab = LIVE[key]
    return f'<span class="chip chip-{c}"><i class="ti ti-{ic}"></i>{e(text or lab)}</span>'

def status_chip(st):
    return f'<span class="chip chip-{STATUS_CLS[st]}"><i class="ti ti-{STATUS_ICON[st]}"></i>{e(STATUS_LABEL[st])}</span>'

def dot(key):
    c = LIVE[key][0]
    cls = {"ok": "ok pulse", "warn": "warn", "bad": "bad", "info": "info", "user": "user", "idle": ""}[c]
    return f'<span class="dot {cls}" title="{e(LIVE[key][2])}"></span>'

def avatar(a, size="md"):
    return f'<span class="avatar avatar-{size} avatar-{a["av"]}" aria-hidden="true">{e(a["id"][:2].upper() if a["id"]!="orch" else "OR")}</span>'

def prio(p):
    if p == 0: return '<span class="prio lo">p0</span>'
    return f'<span class="prio {"hi" if p >= 2 else ""}">p{p}</span>'

def layout(fname, title, active, body, actions="", wide=False, pretitle="", extra="", readonly=False):
    nav = ""
    for f, label, icon, badge in NAV:
        b = ""
        if badge == "needs": b = '<span class="badge bg-red text-red-fg" data-needs-badge>5</span>'
        elif badge: b = f'<span class="badge bg-azure text-azure-fg">{badge}</span>'
        cur = ' active" aria-current="page' if f == active else ""
        nav += f'<li class="nav-item"><a class="nav-link{cur}" href="{f}"><span class="nav-link-icon"><i class="ti ti-{icon}"></i></span><span class="nav-link-title">{label}</span>{b}</a></li>'
    return f'''<!doctype html>
<html lang="en"{" data-readonly" if readonly else ""}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>{e(title)} · slai</title>
<link rel="icon" href="assets/favicon.svg" type="image/svg+xml">
<script src="assets/theme.js"></script>
<link rel="stylesheet" href="vendor/tabler/css/tabler.min.css">
<link rel="stylesheet" href="vendor/icons/tabler-icons.min.css">
<link rel="stylesheet" href="assets/app.css">
</head>
<body>
<div class="page">
  <aside class="navbar navbar-vertical navbar-expand-lg" aria-label="Main navigation">
    <div class="container-fluid">
      <button class="navbar-toggler" type="button" data-bs-toggle="collapse" data-bs-target="#sidebar-menu" aria-controls="sidebar-menu" aria-expanded="false" aria-label="Toggle navigation"><span class="navbar-toggler-icon"></span></button>
      <h1 class="navbar-brand navbar-brand-autodark mb-0"><a href="index.html" class="d-flex align-items-center text-reset text-decoration-none"><span class="brand-mark">s</span><span><span class="brand-name">slai</span><span class="brand-sub">demo-project</span></span></a></h1>
      <div class="d-lg-none"><a class="btn btn-sm btn-ghost-secondary" href="inbox.html" aria-label="Inbox"><i class="ti ti-message-question"></i><span class="badge bg-red text-red-fg ms-1" data-needs-badge>5</span></a></div>
      <div class="collapse navbar-collapse" id="sidebar-menu">
        <ul class="navbar-nav pt-lg-3">{nav}</ul>
        <div class="mt-auto p-3 d-none d-lg-block">
          <div class="card card-sm"><div class="card-body">
            <div class="d-flex align-items-center gap-2"><span class="dot ok pulse"></span><strong class="small">Supervisor running</strong></div>
            <div class="reason mt-1">heartbeat 2s ago · pid 41873 · up 3h12m</div>
          </div></div>
        </div>
      </div>
    </div>
  </aside>
  <div class="page-wrapper">
    <div class="proto-ribbon">Prototype · fake data · nothing here talks to a real team</div>
    <header class="navbar navbar-expand-md d-print-none sticky-top" style="background:var(--tblr-bg-surface);border-bottom:1px solid var(--tblr-border-color)">
      <div class="container-xl">
        <div class="d-flex align-items-center gap-2 flex-wrap w-100">
          <span class="chip chip-ok"><i class="ti ti-heartbeat"></i>Supervisor ok</span>
          <a class="chip chip-user text-decoration-none" href="inbox.html"><i class="ti ti-hand-stop"></i><span id="needs-count">5</span> need you</a>
          <div class="ms-auto d-flex align-items-center gap-2">
            <button class="btn btn-sm btn-outline-warning" data-bs-toggle="modal" data-bs-target="#hold-modal" data-hold-open><i class="ti ti-player-pause me-1"></i>Hold team</button>
            <button class="btn btn-sm btn-warning" data-hold-release hidden><i class="ti ti-player-play me-1"></i>Release hold</button>
            <button class="btn btn-sm btn-icon btn-ghost-secondary" id="theme-toggle" type="button" aria-label="Switch theme"><i class="ti ti-moon"></i></button>
          </div>
        </div>
      </div>
    </header>
    <div class="page-header d-print-none"><div class="container-xl"><div class="row g-2 align-items-center">
      <div class="col">{f'<div class="page-pretitle">{e(pretitle)}</div>' if pretitle else ''}<h2 class="page-title">{title}</h2></div>
      <div class="col-auto ms-auto"><div class="btn-list">{actions}</div></div>
    </div></div></div>
    <div class="page-body"><div class="container-xl">
      {RO_BANNER if readonly else ""}
      <div id="hold-banner" class="banner banner-hold mb-3" hidden role="status"><i class="ti ti-player-pause text-warning"></i><div><strong>Team on hold: <span id="hold-name">hold</span></strong> since 11:44 (just now). No wake-ups are being delivered, nothing is nudged or restarted; sessions stay up. <a href="#" data-hold-release-link>Use “Release hold” to resume.</a></div></div>
      {body}
    </div></div>
    <footer class="footer footer-transparent d-print-none"><div class="container-xl"><div class="small text-secondary">slai serve · same privileges as the TUI · it never approves, merges or types keystrokes into a pane · <a href="signin.html">sign-in page</a> · <a href="index-readonly.html">read-only mode</a></div></div></footer>
  </div>
</div>
<div class="modal modal-blur fade" id="hold-modal" tabindex="-1" aria-hidden="true"><div class="modal-dialog modal-dialog-centered"><div class="modal-content">
  <div class="modal-header"><h3 class="modal-title">Hold the team</h3><button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close"></button></div>
  <div class="modal-body"><p class="text-secondary">Nothing is woken, nudged or restarted while the team is on hold. Sessions stay up and work in progress is untouched. Same as <span class="mono">slai hold</span> and <kbd>H</kbd> in the TUI.</p>
    <label class="form-label" for="hold-input">Name (optional)</label><input class="form-control" id="hold-input" type="text" placeholder="release-window"></div>
  <div class="modal-footer"><button class="btn" data-bs-dismiss="modal">Cancel</button><button class="btn btn-warning" data-hold-confirm><i class="ti ti-player-pause me-1"></i>Hold team</button></div>
</div></div></div>
{extra}
<div class="toast-container position-fixed top-0 end-0 p-3 mt-5" id="toasts" aria-live="polite"></div>
<script src="vendor/tabler/js/tabler.min.js"></script>
<script src="vendor/sortable/Sortable.min.js"></script>
<script src="assets/app.js"></script>
</body>
</html>
'''

RO_BANNER = '<div class="banner banner-info mb-3" role="status"><i class="ti ti-eye text-azure"></i><div><strong>Read-only mode.</strong> <span class="mono">slai serve --read-only</span> is on: you can look at everything, but every action is switched off and the server answers 403 to any write. Restart <span class="mono">slai serve</span> without the flag to act from here.</div></div>'

def cmd_copy(cmd):
    return f'<span class="cmd-copy"><code>{e(cmd)}</code><button class="btn btn-sm btn-icon btn-ghost-secondary" type="button" data-copy="{e(cmd)}" aria-label="Copy command"><i class="ti ti-copy"></i></button></span>'

def cli_only(label, icon, hint, extra_cls="btn-outline-secondary"):
    """A disabled control plus the TUI/CLI way to do the same thing (v1 does not do it from the web)."""
    return (f'<span class="d-inline-flex align-items-center gap-2 flex-wrap"><button class="btn {extra_cls}" disabled aria-disabled="true" title="Not available in the web UI in v1"><i class="ti ti-{icon} me-1"></i>{label}</button>'
            f'<span class="cmd-hint"><i class="ti ti-terminal-2"></i><span>{hint}</span></span></span>')

def write(fname, title, active, body, **kw):
    SCREENS[fname] = title
    with open(os.path.join(OUT, fname), "w") as f:
        f.write(layout(fname, title, active, body, **kw))

def card(inner, title=None, actions="", cls="", body_cls="", flush=False):
    head = f'<div class="card-header"><h3 class="card-title">{title}</h3><div class="card-actions">{actions}</div></div>' if title else ""
    return f'<div class="card {cls}">{head}<div class="card-body {"p-0" if flush else ""} {body_cls}">{inner}</div></div>'

def banner(kind, icon, text, extra=""):
    col = {"hold": "warning", "bad": "danger", "info": "azure", "hint": "primary"}[kind]
    return f'<div class="banner banner-{kind} mb-3" role="status"><i class="ti ti-{icon} text-{col}"></i><div class="flex-fill">{text}</div>{extra}</div>'

def sendbox(to, placeholder, label="Message"):
    return f'''<form data-fake="Delivered to {e(to)} when it is safe to type (same path as the TUI send box)." data-title="Message queued" class="sendbox">
<label class="visually-hidden" for="msg-{e(to)}">{label} {e(to)}</label>
<input class="form-control" id="msg-{e(to)}" type="text" placeholder="{e(placeholder)}" autocomplete="off">
<button class="btn btn-primary" type="submit"><i class="ti ti-send me-1"></i>Send</button></form>'''

# ---------------------------------------------------------------- dashboard
def agent_tile(a):
    st = chip(a["live"])
    return f'''<div class="col-12 col-sm-6 col-xl-4"><a href="agent.html" class="card agent-card text-reset text-decoration-none"><div class="card-body">
<div class="d-flex align-items-center gap-3">{avatar(a)}<div class="flex-fill" style="min-width:0"><div class="fw-semibold">{e(a["id"])}</div><div class="reason text-truncate">{e(a["role"].replace("_"," "))} · {e(a["harness"])}/{e(a["model"])}</div></div></div>
<div class="mt-2">{st}</div>
<div class="reason mt-2" style="min-height:2.4em">{e(a["reason"])}</div>
<div class="d-flex justify-content-between mt-2 small"><span>{('<span class="mono">'+a["task"]+'</span>') if a["task"] else '<span class="text-secondary">no task</span>'}</span><span class="text-secondary">${a["spend"]:.2f} today</span></div>
</div></a></div>'''

def dashboard(first_run=False):
    ags = AGENTS
    if first_run:
        ags = [dict(a, live="idle", reason="started 40s ago · waiting for a goal", task="", last="40s ago", spend=0.0) for a in AGENTS]
    tiles = "".join(agent_tile(a) for a in ags)
    inflight = 0 if first_run else sum(1 for t in TASKS if t[1] in ("in_progress", "in_review", "assigned", "blocked"))
    kpis = [
        ("Agents healthy", "10" if first_run else "7", "/ 10", "ok" if first_run else "warn", "heartbeat" ),
        ("Tasks in flight", str(inflight), "tasks", "info", "layout-kanban"),
        ("Waiting on you", "0" if first_run else "5", "items", "user", "hand-stop"),
        ("Spend today", "$0.00" if first_run else "$91.45", "of $120" if not first_run else "caps OFF", "ok", "coin"),
    ]
    k = "".join(f'''<div class="col-6 col-lg-3"><div class="card card-sm"><div class="card-body"><div class="d-flex align-items-center">
<span class="avatar rounded bg-{ "azure" if c=="info" else "green" if c=="ok" else "orange" if c=="warn" else "indigo"}-lt me-3"><i class="ti ti-{ic}"></i></span>
<div><div class="kpi">{v} <small>{u}</small></div><div class="reason">{l}</div></div></div></div></div></div>''' for l, v, u, c, ic in kpis)
    if first_run:
        top = banner("hint", "sparkles", '<strong>Welcome. You only need to talk to one agent: <span class="mono">orch</span>.</strong> Tell it your goal in plain words; it splits the goal into tasks and the rest of the team works for it. The other agents’ panes show machine traffic, you rarely need to type there.')
        goal = card(f'''<p class="text-secondary mb-2">Start here. Try: <em>“Add a --json flag to the status command and cover it with tests.”</em></p>
{sendbox("orch", "Type your goal for orch…", "Goal for")}''', "Your goal for orch", cls="mb-3")
        alerts = '<div class="empty-hint"><i class="ti ti-circle-check fs-1 text-success"></i><div class="mt-2 fw-semibold">Nothing needs you</div><div class="small">Alerts and questions from the team appear here.</div></div>'
        spend = '''<div class="d-flex justify-content-between"><span>Today</span><strong>$0.00</strong></div>
<div class="bar mt-1"><i style="width:0"></i></div>
<p class="reason mt-2 mb-0"><span class="chip chip-idle"><i class="ti ti-shield-off"></i>Spend caps: OFF</span> Set <span class="mono">defaults.spend_caps</span> in <span class="mono">.slai/team.yaml</span> to limit what a run can cost. Ten Claude sessions are running.</p>'''
        banners = ""
    else:
        top = ""
        goal = card(sendbox("orch", "Message orch (your goal, a correction, an answer)…"), None, cls="mb-3")
        banners = (
            banner("bad", "alert-octagon", '<strong>Running an old build.</strong> The supervisor was started from <span class="mono">9b2c1e0</span>; <span class="mono">slai/integration</span> is at <span class="mono">22e7e63</span> (14 commits ahead). New fixes are not active until you run <span class="mono">slai up --restart</span>.')
            + banner("info", "ghost-2", '<strong>An orphan team is running.</strong> “old-demo” has 3 tmux sessions for <span class="mono">/tmp/old-demo</span> and no supervisor. <a href="doctor.html">See Doctor</a> · <span class="mono">slai clean</span> removes them.'))
        alerts = '''<div class="list-group list-group-flush list-group-hoverable">
<a class="list-group-item" href="inbox.html"><div class="d-flex gap-2"><span class="chip chip-bad"><i class="ti ti-lock-exclamation"></i>stuck</span><div><strong>eng-4</strong> has had a permission dialog open for 11 minutes.<div class="reason">Bash(rm -rf node_modules) · focus its pane in the TUI and answer once</div></div></div></a>
<a class="list-group-item" href="inbox.html"><div class="d-flex gap-2"><span class="chip chip-warn"><i class="ti ti-clock-pause"></i>rate limit</span><div><strong>fg-skeptic</strong> hit the 5h limit. Resets 14:20.<div class="reason">Wakes are held for it until then; nothing is lost.</div></div></div></a>
<a class="list-group-item" href="inbox.html"><div class="d-flex gap-2"><span class="chip chip-bad"><i class="ti ti-skull"></i>dead</span><div><strong>fg-power</strong>: revive gave up after 3 tries.<div class="reason">tmux session gone · <span class="mono">v</span> in the TUI revives it</div></div></div></a>
<a class="list-group-item" href="inbox.html"><div class="d-flex gap-2"><span class="chip chip-user"><i class="ti ti-help"></i>question</span><div><strong>orch</strong> asks: include a first-run variant in the prototype?</div></div></a>
</div>'''
        spend = '''<div class="d-flex justify-content-between"><span>Today</span><strong>$91.45 <span class="text-secondary fw-normal">/ $120.00</span></strong></div>
<div class="bar warn mt-1"><i style="width:76%"></i></div>
<div class="reason mt-1"><span class="chip chip-warn"><i class="ti ti-alert-triangle"></i>76% of the daily cap</span> warns at 80%, pauses at 100%</div>
<hr class="my-3">
<div class="d-flex justify-content-between small"><span>eng-1 <span class="text-secondary">per-agent cap $25</span></span><strong>$21.30</strong></div>
<div class="bar bad mt-1"><i style="width:85%"></i></div>
<div class="reason mt-1"><span class="chip chip-warn"><i class="ti ti-alert-triangle"></i>85%: over the warning line</span></div>'''
    gauges = '''<div class="mb-3"><div class="d-flex justify-content-between small"><span>5-hour window</span><strong>41%</strong></div><div class="bar ok mt-1"><i style="width:41%"></i></div><div class="reason mt-1">resets 14:20 · claude</div></div>
<div class="mb-3"><div class="d-flex justify-content-between small"><span>Weekly</span><strong>27%</strong></div><div class="bar ok mt-1"><i style="width:27%"></i></div><div class="reason mt-1">resets Mon 09:00 · claude</div></div>
<div><div class="d-flex justify-content-between small"><span>opencode (eng-4)</span><span class="na" title="OpenCode reports no rate-limit data to hooks, so there is nothing to show.">n/a</span></div><div class="reason mt-1">no limit data from this harness</div></div>'''
    act = ""
    activity = '<ul class="timeline">' + "".join(
        f'<li><span class="dot {c}"></span><div class="small"><strong>{e(who)}</strong> {e(what)} <span class="text-secondary">· {t}</span></div></li>'
        for who, what, t, c in [("principal", "requested no changes on T-0075 (round 2)", "11:42", "ok"), ("eng-1", "asked review for T-0079", "11:40", "info"),
                                 ("supervisor", "woke eng-3 (2 new items)", "11:39", "idle"), ("eng-3", "finished T-0086, merged", "11:38", "ok"),
                                 ("supervisor", "denied eng-4 write outside its worktree", "11:36", "bad")]) + "</ul>"
    if first_run:
        activity = '<div class="empty-hint">No activity yet. It starts when you send orch a goal.</div>'
    body = f'''{top}{banners}
<div class="row row-cards mb-3">{k}</div>
{goal}
<div class="row row-cards">
  <div class="col-lg-8"><div class="d-flex align-items-center mb-2"><h3 class="mb-0">Team health</h3><span class="ms-2 reason">one chip per agent: icon + text, never colour alone</span></div>
    <div class="row row-cards">{tiles}</div></div>
  <div class="col-lg-4">
    {card(alerts, "Needs attention", '<a href="inbox.html" class="small">Inbox</a>', cls="mb-3", flush=True)}
    {card(spend, "Spend and caps", cls="mb-3")}
    {card(gauges, "Rate limits", cls="mb-3")}
    {card(activity, "Recent activity", cls="mb-3")}
  </div>
</div>'''
    write("index-first-run.html" if first_run else "index.html", "Dashboard" if not first_run else "Dashboard (first run)", "index.html", body,
          pretitle="demo-project · integration slai/integration" if not first_run else "demo-project · just started")

# ---------------------------------------------------------------- agents
def agents_list():
    rows = ""
    for a in AGENTS:
        gauge = '<span class="na" title="no rate-limit data from opencode hooks">n/a</span>' if a["harness"] == "opencode" else f'{40 + len(a["id"]) * 3}%'
        rows += f'''<tr data-row data-live="{a["live"]}" data-harness="{a["harness"]}"><td><div class="d-flex align-items-center gap-2">{avatar(a, "sm")}<div><a class="fw-semibold" href="agent.html">{e(a["id"])}</a><div class="reason">{e(a["role"].replace("_"," "))}</div></div></div></td>
<td>{chip(a["live"])}<div class="reason mt-1">{e(a["reason"])}</div></td>
<td>{('<a class="mono" href="task.html">'+a["task"]+'</a>') if a["task"] else '<span class="text-secondary">—</span>'}</td>
<td class="text-nowrap">{e(a["last"])}</td><td class="text-nowrap">{e(a["harness"])}/{e(a["model"])}</td><td>{gauge}</td><td class="text-end">${a["spend"]:.2f}</td></tr>'''
    opts = "".join(f'<option value="{k}">{LIVE[k][2]}</option>' for k in LIVE)
    body = f'''<div data-filter-scope>
<div class="card mb-3"><div class="card-body"><div class="row g-2 align-items-end">
<div class="col-12 col-md-5"><label class="form-label" for="al-q">Search</label><input class="form-control" id="al-q" data-filter="text" placeholder="agent, task, reason…"></div>
<div class="col-6 col-md-3"><label class="form-label" for="al-live">Status</label><select class="form-select" id="al-live" data-filter="live"><option value="">All</option>{opts}</select></div>
<div class="col-6 col-md-3"><label class="form-label" for="al-h">Harness</label><select class="form-select" id="al-h" data-filter="harness"><option value="">All</option><option>claude</option><option>opencode</option></select></div>
<div class="col-12 col-md-1 text-secondary small"><span data-filter-count>10</span> shown</div></div></div></div>
<div class="card"><div class="table-responsive"><table class="table table-vcenter table-sl card-table">
<thead><tr><th>Agent</th><th>Status and why</th><th>Task</th><th>Last activity</th><th>Harness/model</th><th>5h gauge</th><th class="text-end">Spend</th></tr></thead>
<tbody>{rows}</tbody></table>
<div class="empty-hint m-3" data-filter-empty hidden>No agent matches. Clear a filter.</div></div></div></div>
'''
    write("agents.html", "Agents", "agents.html", body,
          actions='<a class="btn btn-primary" data-write href="agent-new.html"><i class="ti ti-plus me-1"></i>New agent</a>')

PANE = '''<span class="c5">✻</span> <span class="b">Welcome to Claude Code</span>  <span class="dim">· /help for help · cwd: .slai/worktrees/eng-2</span>

<span class="c2">●</span> <span class="b">Read</span>(docs/serve-prototype/tools/data.py)
  <span class="dim">└  Read 119 lines</span>

<span class="c2">●</span> <span class="b">Write</span>(docs/serve-prototype/assets/app.js)
  <span class="dim">└  Wrote 214 lines to</span> docs/serve-prototype/assets/app.js
      <span class="c2">+ var board = $("#board");</span>
      <span class="c2">+ var TERMINAL = { merged: 1, cancelled: 1, reverted: 1 };</span>

<span class="c2">●</span> <span class="b">Bash</span>(node --check docs/serve-prototype/assets/app.js)
  <span class="dim">└  (No output)</span>

<span class="c2">●</span> The kanban drag rules now mirror <span class="c6">store.CanTransition</span>: only cancel,
  unblock and in-column reorder are accepted; everything else is refused
  with the reason. Next: the agents pages.

<span class="c1">✗</span> a literal <span class="c3">&lt;script&gt;alert(1)&lt;/script&gt;</span> printed by the agent stays text,
  and <span class="dim">\\x1b]52;c;…</span> clipboard escapes are dropped by the renderer.

<span class="c5">✽</span> <span class="c3">Moonwalking…</span> <span class="dim">(31s · ↓ 1.7k tokens)</span>
<span class="dim">────────────────────────────────────────────────────────────────</span>
<span class="inv"> &gt; </span>
<span class="dim">────────────────────────────────────────────────────────────────</span>
  <span class="dim">⏵⏵ accept edits on (shift+tab to cycle)</span>'''

def agent_detail():
    a = AGENT["eng-2"]
    hist = [("11:42:03", "hook", "healthy", "hook event 6s ago (posttooluse)", "ok"),
            ("10:58:21", "restart", "restarted fresh", "cause: context_full (92% of window)", "info"),
            ("10:31:09", "health", "hooks failing → ok", "cause: hook events resumed after 4m (spinner ticks, not a stall)", "warn"),
            ("09:12:44", "wake", "woken", "2 new items, idle for 3m, safe to type", "idle"),
            ("08:40:02", "start", "started", "cause: slai up", "ok")]
    hrows = "".join(f'<li><span class="dot {c}"></span><div class="small"><strong>{e(k)}</strong> {e(s)}<div class="reason">{e(why)} · {t}</div></div></li>' for t, k, s, why, c in hist)
    denied = '''<div class="d-flex align-items-baseline gap-2 mb-2"><span class="kpi">2</span><span class="reason">lane denials in the last 24h</span></div>
<ul class="list-unstyled small mb-0"><li class="mb-2"><span class="chip chip-bad"><i class="ti ti-ban"></i>denied</span> <span class="mono">Write</span> <span class="mono">internal/supervisor/loop.go</span><div class="reason">outside this agent’s lane (frontend). Coordinate with eng-1 first · 10:12</div></li>
<li><span class="chip chip-bad"><i class="ti ti-ban"></i>denied</span> <span class="mono">Bash</span> <span class="mono">cat &gt; $D/file</span><div class="reason">redirect to an unresolved shell variable · 09:47</div></li></ul>'''
    msgs = '''<ul class="timeline">
<li><span class="dot info"></span><div class="small"><strong>orch → eng-2</strong> <span class="text-secondary">11:30</span><div>New task T-0087: slai serve Phase 1 prototype. Call get_task for details.</div></div></li>
<li><span class="dot ok"></span><div class="small"><strong>eng-2 → orch</strong> <span class="text-secondary">11:12</span><div>T-0075 in review; waiting for the principal.</div></div></li>
<li><span class="dot user"></span><div class="small"><strong>you → eng-2</strong> <span class="text-secondary">10:05</span><div>Course correction from user: make it look like a polished product, not a wireframe.</div></div></li></ul>'''
    info = '''<dl class="row mb-0 small"><dt class="col-5">Harness</dt><dd class="col-7">claude 2.1.307</dd><dt class="col-5">Model</dt><dd class="col-7">sonnet</dd>
<dt class="col-5">Session</dt><dd class="col-7 mono">slai_a76018_eng-2</dd><dt class="col-5">Worktree</dt><dd class="col-7 mono text-break">.slai/worktrees/eng-2</dd>
<dt class="col-5">Branch</dt><dd class="col-7 mono text-break">slai/eng-2/T-0087</dd><dt class="col-5">Context</dt><dd class="col-7">63% of window</dd></dl>
<hr><div class="small fw-semibold mb-1">Degraded features</div><ul class="small mb-0 ps-3"><li>none for this harness</li></ul>
<div class="reason mt-2">opencode agents list e.g. “no rate-limit data”, “no hook for Notification”.</div>'''
    actions = '''<button class="btn" data-write data-toast="eng-2 is resumed: wake-ups are delivered again." data-kind="ok" data-title="Resumed"><i class="ti ti-player-play me-1"></i>Resume</button>
<button class="btn" data-write data-toast="eng-2 is paused: no wakes until you resume." data-kind="warn" data-title="Paused"><i class="ti ti-player-pause me-1"></i>Pause</button>'''
    body = f'''<div class="row row-cards">
<div class="col-lg-8">
 <div class="card mb-3"><div class="card-header"><div class="d-flex align-items-center gap-3">{avatar(a)}<div><h3 class="card-title mb-0">eng-2 {chip("working")}</h3><div class="reason">{e(a["reason"])}</div></div></div>
  <div class="card-actions pane-bar"><span class="chip chip-ok"><i class="ti ti-broadcast"></i>updates when the pane changes</span><span>last change 6s ago</span></div></div>
 <div class="card-body"><div class="pane" role="log" aria-label="Read-only copy of eng-2's terminal" tabindex="0">{PANE}</div>
  <div class="reason mt-2"><i class="ti ti-eye"></i> Read-only and escaped: colour and bold only, the last 200 lines / 64 KB. The server pushes a new copy when the pane changes (no polling). Nothing typed here reaches the pane; use the boxes below, which go through the same safe-to-type check as the TUI.</div></div></div>
 <div class="card mb-3"><div class="card-header"><h3 class="card-title">Talk to eng-2</h3></div><div class="card-body">
  {sendbox("eng-2", "Message for eng-2…")}
  <hr>
  <form class="sendbox" data-fake="Interrupted with Escape, then your correction was queued. Same as c in the TUI." data-title="Course correction sent">
   <label class="visually-hidden" for="redir">Interrupt and redirect eng-2</label>
   <input class="form-control" id="redir" type="text" placeholder="Interrupt the current turn and tell it…">
   <button class="btn btn-warning" type="submit"><i class="ti ti-arrow-back-up me-1"></i>Interrupt + redirect</button></form>
  <div class="mt-2 d-flex flex-wrap gap-2 align-items-center"><button class="btn btn-sm btn-outline-danger" data-write data-toast="Refused: a permission dialog is open in eng-4’s pane (“Do you want to proceed?”). Nothing was typed. Answer it in the TUI first." data-kind="bad" data-title="Not safe to type"><i class="ti ti-flask me-1"></i>Demo: try on a pane with a dialog open</button>
</div>
 </div></div>
</div>
<div class="col-lg-4">
 {card(hist and '<ul class="timeline">'+hrows+'</ul>', "Recent health events", cls="mb-3")}
 {card(denied, "Lane denials", cls="mb-3")}
 {card(msgs, "Message history", cls="mb-3")}
 {card(info, "Harness and session", cls="mb-3")}
 {card('<p class="small text-secondary">These need the supervisor and tmux, so in v1 they stay in the TUI where you see exactly what happens.</p>' + '<div class="d-grid gap-2">' + cli_only("Restart fresh", "refresh", "TUI: select eng-2, press <kbd>R</kbd> (asks first)") + cli_only("Revive", "heart-plus", "TUI: select a dead agent, press <kbd>v</kbd>") + cli_only("Reassign task", "arrows-exchange", "orch reassigns through its tools") + '</div>', "TUI only in v1", cls="mb-3")}
</div></div>'''
    write("agent.html", "eng-2", "agents.html", body, pretitle="Agent · engineer · claude/sonnet", actions=actions)

# ---------------------------------------------------------------- board
BLOCK_WHY = {"T-0082": "needs a decision: notify via notify-send or terminal bell?"}
def tcard(t):
    tid, st, who, p, title, deps, waiting, rounds, needs = t
    a = AGENT.get(who, {"live": "idle", "id": who})
    meta = f'<span class="d-inline-flex align-items-center gap-1">{dot(a["live"])}{e(who)}</span>{prio(p)}'
    if rounds: meta += f'<span title="review rounds"><i class="ti ti-repeat"></i> r{rounds}</span>'
    if waiting: meta += f'<span class="waiting" title="waiting on other tasks to merge (normal, not a problem)"><i class="ti ti-hourglass-high"></i> waiting on {", ".join(waiting)}</span>'
    elif deps and st not in ("merged",): meta += f'<span title="depends on"><i class="ti ti-link"></i> {", ".join(deps)} ✓</span>'
    if st == "blocked": meta += f'<span class="blockedby" title="blocked by a problem, not by a dependency"><i class="ti ti-hand-stop"></i> {e(BLOCK_WHY[tid])}</span>'
    if needs: meta += '<span class="chip chip-user"><i class="ti ti-hand-stop"></i>needs you</span>'
    if st == "in_review": meta += '<span><i class="ti ti-eye"></i> principal</span>'
    br = f'<div class="mono text-secondary text-truncate mt-1" style="font-size:.7rem"><i class="ti ti-git-branch"></i> slai/{who}/{tid}</div>'
    return f'''<div class="tcard {st}" data-id="{tid}" data-status="{st}" data-assignee="{who}" data-needs="{needs}"><span class="tid">{tid}</span>
<a class="ttl" href="task.html">{e(title)}</a><div class="meta">{meta}</div>{br}</div>'''

def board():
    cols = ""
    for s in STATUSES:
        cs = "".join(tcard(t) for t in sorted([t for t in TASKS if t[1] == s], key=lambda t: (-t[3], t[0])))
        cols += f'''<section class="kcol" data-status="{s}" aria-label="{STATUS_LABEL[s]}"><div class="kcol-head">{status_chip(s)}<span class="badge bg-secondary-lt count">0</span></div>
<div class="kcol-body">{cs}<div class="kcol-empty empty-hint small p-3">Nothing here</div></div></section>'''
    ag = "".join(f'<option value="{a["id"]}">{a["id"]}</option>' for a in AGENTS if a["role"] == "engineer" or a["id"] == "pm")
    ag2 = "".join(f'<option>{a["id"]}</option>' for a in AGENTS if a["role"] == "engineer")
    body = f'''<div class="card mb-3"><div class="card-body"><div class="row g-2 align-items-end">
<div class="col-12 col-md-4"><label class="form-label" for="f-text">Search</label><input class="form-control" id="f-text" type="search" placeholder="title, id, branch…"></div>
<div class="col-6 col-md-3"><label class="form-label" for="f-assignee">Assignee</label><select class="form-select" id="f-assignee"><option value="">Everyone</option>{ag}</select></div>
<div class="col-6 col-md-3"><label class="form-check mt-4"><input class="form-check-input" type="checkbox" id="f-needs"><span class="form-check-label">Needs me</span></label></div>
<div class="col-12 col-md-2 text-md-end"><button class="btn btn-outline-danger btn-sm" data-demo-illegal data-id="T-0079" data-from="in_review" data-to="merged"><i class="ti ti-flask me-1"></i>Demo: illegal move</button></div>
</div><div class="reason mt-2">Filters live in the address bar, so a view can be bookmarked. Drag a card: only <strong>cancel</strong>, <strong>unblock</strong> and <strong>reorder within a column</strong> do anything; the green/red outline tells you before you drop.</div></div></div>
<div class="board" id="board">{cols}</div>
<div class="modal modal-blur fade" id="newtask" tabindex="-1"><div class="modal-dialog modal-lg modal-dialog-centered"><form class="modal-content" data-fake="T-0088 created and assigned to eng-3." data-title="Task created">
<div class="modal-header"><h3 class="modal-title">New task</h3><button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close"></button></div>
<div class="modal-body"><div class="row g-3">
<div class="col-12"><label class="form-label" for="nt-t">Title</label><input class="form-control" id="nt-t" type="text" placeholder="Short and specific"></div>
<div class="col-12"><label class="form-label" for="nt-d">Description</label><textarea class="form-control" id="nt-d" rows="3" placeholder="What to build, how to verify it, which acceptance criteria"></textarea></div>
<div class="col-md-4"><label class="form-label" for="nt-a">Assignee</label><select class="form-select" id="nt-a">{ag2}</select></div>
<div class="col-md-4"><label class="form-label" for="nt-dep">Depends on</label><input class="form-control" id="nt-dep" type="text" placeholder="T-0083, T-0084"></div>
<div class="col-md-4"><label class="form-label" for="nt-s">Spec path</label><input class="form-control" id="nt-s" type="text" placeholder=".slai/specs/name.md"></div>
<div class="col-12"><label class="form-label" for="nt-f">Files</label><input class="form-control" id="nt-f" type="text" placeholder="internal/tui/, cmd/"></div></div></div>
<div class="modal-footer"><button type="button" class="btn btn-ghost-danger me-auto" data-toast="dependency cycle: T-0084 → T-0083 → T-0084 (T-0084 already waits on T-0083)" data-kind="bad" data-title="create_task refused"><i class="ti ti-flask me-1"></i>Demo: error from the service</button><button type="button" class="btn" data-bs-dismiss="modal">Cancel</button><button class="btn btn-primary" type="submit">Create task</button></div></form></div></div>'''
    write("board.html", "Board", "board.html", body, pretitle="Tasks by status",
          actions='<button class="btn btn-primary" data-write data-bs-toggle="modal" data-bs-target="#newtask"><i class="ti ti-plus me-1"></i>New task</button>')

# ---------------------------------------------------------------- task detail
DIFF = [
 ("internal/tui/stats.go", "+104 −0", True, [("hunk","@@ -0,0 +1,104 @@"),("add","+package tui"),("add","+"),("add","+// statsMedianMinW is the narrowest terminal that still shows the median-time column."),("add","+const statsMedianMinW = 100"),("add","+"),("add","+func renderStats(rep service.StatsReport, w int) []string {"),("add","+\tif statsEmpty(rep) {"),("add","+\t\treturn []string{styleDim.Render(\"no finished tasks yet\")}"),("add","+\t}"),("ctx"," …")]),
 ("internal/tui/model.go", "+19 −1", False, [("hunk","@@ -81,6 +81,7 @@ const ("),("ctx"," \tovLog"),("ctx"," \tovUsage"),("add","+\tovStats"),("ctx"," )"),("hunk","@@ -1070,6 +1076,9 @@ func (m *Model) listKey("),("ctx"," \tcase \"l\":"),("ctx"," \t\tm.ov = ovLog"),("add","+\tcase \"s\":"),("add","+\t\tm.ov = ovStats"),("add","+\t\treturn m, m.refreshCmd()")]),
 ("internal/tui/stats_test.go", "+118 −0", False, [("add","+func TestStatsViewGolden80(t *testing.T) {"),("add","+\t…")]),
 ("README.md", "+1 −0", False, [("add","+| `s` | Stats view: per agent and per harness/model … |")]),
]
def diff_html(files=DIFF):
    out = ""
    for name, stat, op, lines in files:
        ls = "".join(f'<span class="ln {c}">{e(t)}</span>' for c, t in lines)
        out += f'<details class="diff-file" {"open" if op else ""}><summary><i class="ti ti-file-code"></i>{e(name)}<span class="ms-auto text-secondary">{e(stat)}</span></summary><div class="diff" style="border:0;border-radius:0">{ls}</div></details>'
    return f'<div class="diff" style="padding:0">{out}</div>'

def task_detail():
    claims = [("AC 1", "Golden test of the view at 80 and 120 columns with seeded stats", "met", "TestStatsViewGolden80, TestStatsViewGolden120ShowsMedian", "pass", "Both goldens present; widths verified."),
              ("AC 2", "The key opens and Esc closes it; the help line lists the key", "met", "TestStatsKeyOpensAndEscCloses", "pass", "Help text and footer hint both checked."),
              ("AC 3", "An empty team shows “no finished tasks yet”", "met", "TestStatsEmptyTeamSaysNoFinishedTasks", "unanswered", "")]
    rows = ""
    for ac, text, met, note, ver, vnote in claims:
        v = {"pass": '<span class="chip chip-ok"><i class="ti ti-check"></i>pass</span>', "fail": '<span class="chip chip-bad"><i class="ti ti-x"></i>fail</span>',
             "unanswered": '<span class="chip chip-warn"><i class="ti ti-help"></i>unanswered</span>'}[ver]
        rows += f'<tr><td class="fw-semibold">{ac}</td><td>{e(text)}</td><td><span class="chip chip-ok"><i class="ti ti-check"></i>{met}</span><div class="reason mono mt-1">{e(note)}</div></td><td>{v}<div class="reason mt-1">{e(vnote) or "reviewer has not answered this claim yet"}</div></td></tr>'
    tl = [("11:39", "request_review", "round 2 · e2e2821 · full suite green", "info"), ("11:12", "request_changes", "principal: 1 comment (ordering of closed tasks)", "warn"),
          ("10:58", "request_review", "round 1 · ede1a42", "info"), ("10:20", "assigned", "by orch · depends on T-0071 (merged)", "idle")]
    tlh = '<ul class="timeline">' + "".join(f'<li><span class="dot {c}"></span><div class="small"><strong>{k}</strong> <span class="text-secondary">· {t}</span><div class="reason">{e(w)}</div></div></li>' for t, k, w, c in tl) + "</ul>"
    spec = '''<h4>TUI stats panel</h4><p>A TUI view of the numbers T-0071 adds (<code>service.Stats</code>). Wait for T-0071 to merge.</p>
<h5>Build</h5><ul><li>A key opens a Stats view: one row per engineer, then a block grouped by harness+model.</li><li>Refresh with the board tick. Esc goes back.</li><li>Fits 80 columns: drop the median column below 100 cols.</li></ul>
<p class="reason mb-0">Rendered from <span class="mono">.slai/specs/stats-panel.md</span>, read-only. A path outside <span class="mono">.slai/specs/</span> is refused.</p>'''
    desc = '<p class="mb-0">A TUI view of the numbers T-0071 adds (<code>service.Stats</code>, the same data as <code>slai status --stats --json</code>). Key <kbd>s</kbd>, per-engineer rows then a harness/model block, refreshed with the board tick, median column dropped under 100 columns.</p>'
    side = f'''<dl class="row mb-0 small"><dt class="col-5">Status</dt><dd class="col-7">{status_chip("in_review")}</dd><dt class="col-5">Assignee</dt><dd class="col-7"><span class="d-inline-flex align-items-center gap-1">{dot("working")}eng-2</span></dd>
<dt class="col-5">Priority</dt><dd class="col-7">{prio(0)}</dd><dt class="col-5">Review</dt><dd class="col-7">round 2 · principal</dd><dt class="col-5">Branch</dt><dd class="col-7 mono text-break">slai/eng-2/T-0075</dd>
<dt class="col-5">Spec</dt><dd class="col-7 mono text-break">.slai/specs/stats-panel.md</dd><dt class="col-5">Files</dt><dd class="col-7 mono text-break">internal/tui/stats.go, stats_test.go, model.go</dd>
<dt class="col-5">Depends on</dt><dd class="col-7"><a class="mono" href="task.html">T-0071</a> {status_chip("merged")}</dd><dt class="col-5">Waited on by</dt><dd class="col-7"><a class="mono" href="task.html">T-0083</a> <span class="waiting"><i class="ti ti-hourglass-high"></i> waiting</span></dd></dl>'''
    body = f'''<div class="row row-cards">
<div class="col-lg-8">
 {card(desc, "Description", cls="mb-3")}
 {card(f'<div class="table-responsive"><table class="table table-vcenter table-sl card-table"><thead><tr><th>AC</th><th>Criterion</th><th>Engineer’s claim</th><th>Reviewer’s verdict</th></tr></thead><tbody>{rows}</tbody></table></div>', "Acceptance criteria", cls="mb-3", flush=True)}
 <div class="card mb-3"><div class="card-header"><h3 class="card-title">Diff</h3><div class="card-actions"><span class="reason">slai/integration…slai/eng-2/T-0075 · 4 files · +242 −1 · unified, escaped</span></div></div><div class="card-body p-0">{diff_html()}<div class="p-3 reason"><i class="ti ti-fold"></i> Diffs over 30 files collapse per file; only the first file starts open.</div></div></div>
 {card(spec, "Spec", cls="mb-3")}
</div>
<div class="col-lg-4">{card(side, "Task", cls="mb-3")}{card(tlh, "Timeline", cls="mb-3")}
 {card('<p class="small mb-2">Approving, requesting changes and merging belong to the principal. This page shows them read-only.</p><span class="mono small">approve_review T-0075</span>', "Review path", cls="mb-3")}</div></div>'''
    write("task.html", "T-0075 · TUI stats panel", "board.html", body, pretitle="Task · in review, round 2", actions=f'{status_chip("in_review")}')

# ---------------------------------------------------------------- reviews
def reviews():
    q = [("T-0079", "Team memory M1: fact store and index", "eng-1", "principal", "12m", "1", True), ("T-0075", "TUI stats panel: per-agent / per-model quality numbers", "eng-2", "principal", "38m", "2", False)]
    rows = "".join(f'<tr><td><a class="mono fw-semibold" href="task.html">{i}</a></td><td>{e(t)}{" "+chip("hooks","spec rewritten") if sp else ""}</td><td>{who}</td><td>{rv}</td><td class="text-nowrap">{age}</td><td>r{r}</td></tr>' for i, t, who, rv, age, r, sp in sorted(q, key=lambda x: -int(x[4][:-1])))
    refused = '''<pre class="plan mb-0">merge_refused T-0074 · 11:31:47
test gate failed (go test ./... -count=1)

--- FAIL: TestSpendHeaderShowsCaps (0.02s)
    header_test.go:88: header = "spend $12.00"
        want "spend $12.00 / $120.00 (10%)"
FAIL	github.com/oorbt/slai/internal/tui	0.412s
ok  	github.com/oorbt/slai/internal/store	1.204s
FAIL</pre>'''
    body = f'''{banner("info", "info-circle", "<strong>Approving and merging happen through the principal.</strong> This page has no approve or merge button on purpose: the web UI never has more power than the TUI. The principal uses <span class='mono'>approve_review</span> / <span class='mono'>request_changes</span>.")}
<div class="row row-cards"><div class="col-lg-7">
{card(f'<div class="table-responsive"><table class="table table-vcenter table-sl card-table"><thead><tr><th>Task</th><th>Title</th><th>Engineer</th><th>Reviewer</th><th>Waiting</th><th>Round</th></tr></thead><tbody>{rows}</tbody></table></div>', "In-review queue · oldest first", cls="mb-3", flush=True)}
<div class="card mb-3"><div class="card-header"><h3 class="card-title">T-0075 · diff and claims</h3><div class="card-actions"><a href="task.html" class="small">Open task</a></div></div><div class="card-body p-0">{diff_html(DIFF[:2])}</div></div>
</div><div class="col-lg-5">
{card(refused, "Latest merge_refused", cls="mb-3")}
{card('<ul class="timeline"><li><span class="dot warn"></span><div class="small"><strong>T-0075 round 1: changes requested</strong><div class="reason">“isOpenTask treats only merged and cancelled as closed” · principal · 11:12</div></div></li><li><span class="dot ok"></span><div class="small"><strong>T-0071 approved</strong><div class="reason">round 1 · merged 10:02</div></div></li><li><span class="dot bad"></span><div class="small"><strong>T-0074 merge refused</strong><div class="reason">test gate failed · fixed in the next commit</div></div></li></ul>', "Verdict history", cls="mb-3")}
{card('<p class="small mb-0"><span class="chip chip-warn"><i class="ti ti-file-diff"></i>spec addendum</span> T-0083’s spec gained an addendum after it was assigned. Its engineer is told at the next wake.</p>', "Warnings", cls="mb-3")}
</div></div>'''
    write("reviews.html", "Reviews", "reviews.html", body, pretitle="Principal’s queue, read-only")

# ---------------------------------------------------------------- inbox
def inbox():
    ask = [("orch", "Should the serve prototype include a first-run variant of the dashboard?", "11:31", "Orchestrator wants to show a non-technical first screen to the focus group."),
           ("pm", "Do you want the focus group to review screenshots or the live files?", "11:20", "Personas could not open files in earlier rounds.")]
    ah = ""
    for who, q, t, ctx in ask:
        ah += f'''<div class="list-group-item" data-needs-item><div class="d-flex gap-2 mb-2"><span class="chip chip-user"><i class="ti ti-help"></i>question</span><div class="flex-fill"><strong>{who}</strong> <span class="text-secondary small">· ask_user · {t}</span><div>{e(q)}</div><div class="reason">{e(ctx)}</div></div></div>
<div class="answer-state mb-2"></div><form class="sendbox" data-answer="{who}"><label class="visually-hidden" for="ans-{who}">Answer {who}</label><input class="form-control" id="ans-{who}" type="text" placeholder="Your answer…"><button class="btn btn-primary" type="submit"><i class="ti ti-send me-1"></i>Answer</button></form></div>'''
    esc = [("stuck", "bad", "lock-exclamation", "eng-4", "Permission dialog open for 11 minutes", "Bash(rm -rf node_modules). The supervisor will not type over a dialog. Open the agent in the TUI (key 7) and answer it once.", "modal-stuck", "11:31"),
           ("rate-limit", "warn", "clock-pause", "fg-skeptic", "Rate limit reached", "Resets 14:20 (in 38m). Wakes are held until then. Nothing is lost.", "rate-limit", "11:38"),
           ("revive-gave-up", "bad", "skull", "fg-power", "Revive gave up after 3 tries", "The tmux session is gone. Press v on it in the TUI, or restart it from its page.", "revive-gave-up", "11:41")]
    eh = ""
    for k, c, ic, who, t, d, st, at in esc:
        eh += f'''<div class="list-group-item" data-needs-item><div class="d-flex gap-2"><span class="chip chip-{c}"><i class="ti ti-{ic}"></i>{k}</span><div class="flex-fill"><strong>{who}</strong> · {t} <span class="text-secondary small">· since {at}</span><div class="reason">{d}</div></div>
<div class="btn-list flex-nowrap"><a class="btn btn-sm" href="agent.html">Open</a><button class="btn btn-sm btn-ghost-secondary" data-dismiss="{st}">Dismiss</button></div></div></div>'''
    amsg = '''<div class="list-group-item"><div class="d-flex gap-2"><span class="chip chip-info"><i class="ti ti-message-reply"></i>awaiting reply</span><div><strong>pm → fg-skeptic</strong> <span class="text-secondary small">· ask · 11:27</span><div>React to the serve prototype screenshots.</div><div class="reason">fg-skeptic is rate limited until 14:20, so the reply will be late.</div></div></div></div>'''
    body = f'''<div class="row row-cards"><div class="col-lg-8">
{card(ah, "Questions for you", cls="mb-3", flush=True).replace('<div class="card-body p-0 ">', '<div class="card-body p-0 list-group list-group-flush">')}
{card(eh, "Escalations", '<span class="reason">Dismiss hides it here only</span>', cls="mb-3", flush=True).replace('<div class="card-body p-0 ">', '<div class="card-body p-0 list-group list-group-flush">')}
</div><div class="col-lg-4">
{card('<div class="kpi"><span id="needs-count">5</span> <small>items need you</small></div><p class="reason mt-2 mb-0">The browser tab title and the sidebar badge show this count, so you can leave the page open on another monitor.</p>', "Count", cls="mb-3")}
{card(amsg, "Waiting on the team", cls="mb-3", flush=True).replace('<div class="card-body p-0 ">', '<div class="card-body p-0 list-group list-group-flush">')}
</div></div>'''
    write("inbox.html", "Inbox", "inbox.html", body, pretitle="Questions and escalations")

# ---------------------------------------------------------------- stats
def stats():
    ar = "".join(f'<tr><td class="fw-semibold">{a}</td><td>{h}/{m}</td><td>{mg}</td><td>{c}/{r}</td><td>{rd}</td><td>{rw}%</td><td>{rf}</td><td>${sp:.2f}</td><td>${pm:.2f}</td><td>{md}</td></tr>' for a, h, m, mg, c, r, rd, rw, rf, sp, pm, md in STATS_AGENTS)
    gr = "".join(f'<tr><td class="fw-semibold">{h}/{m}</td><td>{mg}</td><td>{c}/{r}</td><td>{rd}</td><td>{rw}%</td><td>{rf}</td><td>${sp:.2f}</td><td>${pm:.2f}</td><td>{md}</td></tr>' for h, m, mg, c, r, rd, rw, rf, sp, pm, md in STATS_GROUPS)
    th = '<thead><tr><th>{0}</th>{1}<th>merged</th><th>cancelled / reverted</th><th>rounds</th><th>rework</th><th>refused</th><th>spend</th><th>$ / merged</th><th>median time</th></tr></thead>'
    spendrows = "".join(f'<div class="mb-3"><div class="d-flex justify-content-between small"><span>{a.id}</span><strong>${a.spend:.2f} <span class="text-secondary fw-normal">/ $25</span></strong></div><div class="bar {"bad" if a.spend>=21 else "warn" if a.spend>=20 else ""}"><i style="width:{min(100,a.spend/25*100):.0f}%"></i></div></div>' for a in [type("A",(),a) for a in AGENTS if a["role"]=="engineer"])
    body = f'''<div class="row row-cards"><div class="col-12">
{card(f'<div class="table-responsive"><table class="table table-vcenter table-sl card-table">{th.format("by agent","<th>harness/model</th>")}<tbody>{ar}</tbody></table></div><div class="p-3 reason">rounds = request_review calls per merged task · rework = merged tasks that got request_changes · refused = merge_refused events. Same numbers as <span class="mono">slai status --stats</span>.</div>', "Quality by agent", '<span class="reason">all time</span>', cls="mb-3", flush=True)}
{card(f'<div class="table-responsive"><table class="table table-vcenter table-sl card-table">{th.format("by harness / model","")}<tbody>{gr}</tbody></table></div>', "Quality by harness and model", cls="mb-3", flush=True)}
</div>
<div class="col-lg-6">{card(spendrows + '<div class="reason">Per-agent daily cap $25. Warning line at 80%, pause at 100%.</div>', "Spend vs caps today", cls="mb-3")}</div>
<div class="col-lg-6">{card(f"""<div class="table-responsive"><table class="table table-vcenter table-sl card-table"><thead><tr><th>Task</th><th>Spend</th><th>Tokens</th><th>Rounds</th></tr></thead><tbody>
<tr><td class="mono">T-0075</td><td>$5.10</td><td>412k</td><td>2</td></tr><tr><td class="mono">T-0079</td><td>$6.40</td><td>530k</td><td>1</td></tr>
<tr><td class="mono">T-0080</td><td><span class="na" title="eng-4 runs on opencode: it reports no per-turn cost to hooks">n/a</span></td><td><span class="na" title="no usage data from this harness">n/a</span></td><td>0</td></tr></tbody></table></div>
<div class="p-3 reason">n/a means “this harness cannot tell us”, not zero.</div>""", "Per task", cls="mb-3", flush=True)}</div></div>'''
    write("stats.html", "Stats", "stats.html", body, pretitle="Quality and spend")

# ---------------------------------------------------------------- memory
def memory():
    ST = {"fresh": ("ok", "circle-check"), "stale": ("warn", "clock-exclamation"), "unverified": ("idle", "help-circle"), "invalid": ("bad", "circle-x")}
    rows, det = "", ""
    for mid, title, st, paths, used, text in MEMORY:
        c, ic = ST[st]
        rows += f'<tr data-row data-status="{st}" data-fact="{mid}" tabindex="0" style="cursor:pointer"><td class="mono">{mid}</td><td class="fw-semibold">{e(title)}</td><td><span class="chip chip-{c}"><i class="ti ti-{ic}"></i>{st}</span></td><td class="mono small text-break">{e(paths[0][0])}{" +"+str(len(paths)-1) if len(paths)>1 else ""}</td><td>{" ".join("<span class=mono>"+u+"</span>" for u in used) or "<span class=text-secondary>not used yet</span>"}</td></tr>'
        pr = "".join(f'<tr><td class="mono text-break">{e(p)}</td><td class="mono">{e(r)}</td><td class="mono">{e(cu)}</td><td>{"<span class=\"chip chip-ok\"><i class=\"ti ti-check\"></i>same</span>" if r==cu else "<span class=\"chip chip-bad\"><i class=\"ti ti-x\"></i>changed</span>"}</td></tr>' for p, r, cu in paths)
        det += f'''<div id="fd-{mid}" hidden><span class="chip chip-{c} mb-2"><i class="ti ti-{ic}"></i>{st}</span><h3 class="mt-1">{e(title)}</h3><p>{e(text)}</p>
<div class="fw-semibold small mb-1">Cited paths: recorded vs current blob</div><div class="table-responsive"><table class="table table-sm table-sl"><thead><tr><th>path</th><th>recorded</th><th>current</th><th></th></tr></thead><tbody>{pr}</tbody></table></div>
<div class="fw-semibold small mt-3 mb-1">Used by</div><div>{" ".join("<a class=mono href=task.html>"+u+"</a>" for u in used) or "<span class=text-secondary>no task has used this fact</span>"}</div>
<div class="mt-4 small text-secondary">Editing, re-verifying and deleting facts go through the memory service from the command line in v1; this page only reads.</div></div>'''
    body = f'''<div data-filter-scope>
<div class="card mb-3"><div class="card-body"><div class="row g-2 align-items-end">
<div class="col-12 col-md-6"><label class="form-label" for="mq">Search facts</label><input class="form-control" id="mq" data-filter="text" type="search" placeholder="spinner, migration, caps…"></div>
<div class="col-8 col-md-4"><label class="form-label" for="ms">Status</label><select class="form-select" id="ms" data-filter="status"><option value="">All</option><option>fresh</option><option>stale</option><option>unverified</option><option>invalid</option></select></div>
<div class="col-4 col-md-2 small text-secondary"><span data-filter-count>8</span> facts</div></div>
<div class="reason mt-2"><span class="fresh"><i class="ti ti-circle-check"></i> fresh</span> cited files unchanged · <span class="stale"><i class="ti ti-clock-exclamation"></i> stale</span> a cited file changed · <span class="unver"><i class="ti ti-help-circle"></i> unverified</span> never re-checked · <span class="inval"><i class="ti ti-circle-x"></i> invalid</span> a cited path is gone</div></div></div>
<div class="card"><div class="table-responsive"><table class="table table-vcenter table-sl card-table table-hover"><thead><tr><th>id</th><th>fact</th><th>status</th><th>cites</th><th>used by</th></tr></thead><tbody>{rows}</tbody></table>
<div class="empty-hint m-3" data-filter-empty hidden>No fact matches.</div></div></div></div>
<div class="offcanvas offcanvas-end" tabindex="-1" id="fact-offcanvas" style="--tblr-offcanvas-width:520px" aria-labelledby="fact-title"><div class="offcanvas-header"><h2 class="offcanvas-title" id="fact-title">Fact</h2><button type="button" class="btn-close" data-bs-dismiss="offcanvas" aria-label="Close"></button></div><div class="offcanvas-body" id="fact-body"></div></div>
<div hidden>{det}</div>'''
    write("memory.html", "Team memory", "memory.html", body, pretitle="What the team has learned")

# ---------------------------------------------------------------- doctor
def doctor():
    chk = [("ok", "tmux", "tmux 3.4, server socket reachable", ""), ("ok", "claude", "claude 2.1.307 on PATH", ""), ("ok", "state.db", "schema at migration 0004, integrity ok", ""),
           ("ok", "hooks", "hooks installed for 9 of 9 claude agents", ""), ("ok", "lane guard", "mandatory guard active for every agent", ""),
           ("warn", "build", "supervisor runs 9b2c1e0; slai/integration is at 22e7e63 (14 commits newer)", "Run <span class='mono'>slai up --restart</span> to pick up the fixes."),
           ("warn", "orphans", "team “old-demo” has 3 live sessions and no supervisor", "Run <span class='mono'>slai clean --orphans</span> or attach with <span class='mono'>slai up</span> in /tmp/old-demo."),
           ("warn", "opencode", "eng-4: no rate-limit data, no Notification hook (degraded)", "Expected for opencode. See harnesses.md."),
           ("ok", "worktrees", "9 worktrees, all on their task branch", ""), ("ok", "disk", "412 MB under .slai/, 38 GB free", "")]
    cc = {"ok": ("ok", "circle-check"), "warn": ("warn", "alert-triangle"), "fail": ("bad", "circle-x")}
    rows = "".join(f'<div class="list-group-item"><div class="d-flex gap-3 align-items-start"><span class="chip chip-{cc[s][0]}"><i class="ti ti-{cc[s][1]}"></i>{s}</span><div><strong>{n}</strong> <span class="text-secondary">· {e(d)}</span>{f"<div class=reason>Fix: {h}</div>" if h else ""}</div></div></div>' for s, n, d, h in chk)
    lane = '''<div class="table-responsive"><table class="table table-sm table-sl"><thead><tr><th>agent</th><th>harness</th><th>probe</th><th>result</th></tr></thead><tbody>
<tr><td>eng-1</td><td>claude</td><td class="mono">Write ../outside.txt</td><td><span class="chip chip-ok"><i class="ti ti-ban"></i>denied, event recorded</span></td></tr>
<tr><td>eng-4</td><td>opencode</td><td class="mono">Write ../outside.txt</td><td><span class="chip chip-ok"><i class="ti ti-ban"></i>denied, event recorded</span></td></tr></tbody></table></div>'''
    panes = '''<div class="table-responsive"><table class="table table-sm table-sl"><thead><tr><th>agent</th><th>classifier</th><th>rule</th><th>excerpt</th></tr></thead><tbody>
<tr><td>eng-2</td><td><span class="chip chip-ok"><i class="ti ti-activity"></i>working</span></td><td class="mono">claude.working_live</td><td class="mono small">✽ Moonwalking… (31s · ↓ 1.7k tokens)</td></tr>
<tr><td>eng-4</td><td><span class="chip chip-bad"><i class="ti ti-lock-exclamation"></i>modal</span></td><td class="mono">claude.modal</td><td class="mono small">Do you want to proceed?</td></tr>
<tr><td>pm</td><td><span class="chip chip-idle"><i class="ti ti-moon-stars"></i>idle</span></td><td class="mono">claude.idle</td><td class="mono small">&gt; (input box empty)</td></tr></tbody></table></div>'''
    body = f'''{banner("hold", "alert-triangle", "<strong>2 warnings, 0 failures.</strong> The team works, but it is running an old build and an orphan team is still alive.")}
<div class="row row-cards"><div class="col-lg-7">
{card(rows, "Checks · same as slai doctor", cls="mb-3", flush=True).replace('<div class="card-body p-0 ">', '<div class="card-body p-0 list-group list-group-flush">')}
</div><div class="col-lg-5">
{card('<p class="small text-secondary">Fires a known-bad write at each agent’s lane guard and checks the denial was recorded. It probes live sessions, so in v1 you run it from a terminal; the result of the last run is shown here.</p><div class="mb-3">' + cmd_copy("slai doctor --lane") + '</div>' + lane, "slai doctor --lane · last run 10:58", cls="mb-3")}
{card('<p class="small text-secondary">Classifies every live pane with the same rules the supervisor uses and shows which rule matched.</p><div class="mb-3">' + cmd_copy("slai doctor --panes") + '</div>' + panes, "slai doctor --panes · last run 10:58", cls="mb-3")}
</div></div>'''
    write("doctor.html", "Doctor", "doctor.html", body, pretitle="Health checks", actions='<button class="btn" data-toast="Re-ran every check: 2 warnings, 0 failures." data-kind="info" data-title="Doctor re-run"><i class="ti ti-refresh me-1"></i>Re-run checks</button>')

def log():
    rows = ""
    for t, ag, kind, task, det, hook in LOG:
        c = "bad" if kind in ("lane_denied", "merge_refused") else "warn" if kind in ("pane_decision", "health", "restart") else "info" if kind in ("request_review", "report", "message", "ask", "redirect", "wake", "hold") else "idle"
        rows += f'<tr data-row data-agent="{ag}" data-kind="{kind}" data-task="{task}" data-hook="{1 if hook else 0}"><td class="mono text-nowrap">{t}</td><td>{ag}</td><td><span class="chip chip-{c}">{kind}</span></td><td class="mono">{task or "—"}</td><td>{e(det)}</td></tr>'
    ags = "".join(f'<option>{x}</option>' for x in ["eng-1", "eng-2", "eng-4", "orch", "principal", "supervisor"])
    kinds = "".join(f'<option>{x}</option>' for x in sorted({r[2] for r in LOG}))
    body = f'''<div data-filter-scope>
<div class="card mb-3"><div class="card-body"><div class="row g-2 align-items-end">
<div class="col-6 col-md-2"><label class="form-label" for="la">Agent</label><select class="form-select" id="la" data-filter="agent"><option value="">All</option>{ags}</select></div>
<div class="col-6 col-md-3"><label class="form-label" for="lk">Kind</label><select class="form-select" id="lk" data-filter="kind"><option value="">All</option>{kinds}</select></div>
<div class="col-6 col-md-2"><label class="form-label" for="lt">Task</label><input class="form-control" id="lt" data-filter="task" placeholder="T-0080"></div>
<div class="col-6 col-md-3"><label class="form-label" for="lq">Text</label><input class="form-control" id="lq" data-filter="text" placeholder="search"></div>
<div class="col-12 col-md-2"><label class="form-check"><input class="form-check-input" type="checkbox" id="hooks" data-toast="Hook events are noisy; they are shown on demand, like h in the TUI." data-kind="info"><span class="form-check-label">Hook events</span></label></div></div></div></div>
<div class="card"><div class="table-responsive"><table class="table table-vcenter table-sl card-table"><thead><tr><th>time</th><th>agent</th><th>kind</th><th>task</th><th>what</th></tr></thead><tbody>{rows}</tbody></table>
<div class="empty-hint m-3" data-filter-empty hidden>No event matches these filters.</div></div>
<div class="card-footer d-flex align-items-center"><span class="text-secondary small">Showing <span data-filter-count>15</span> of 4,312 · newest first · 100 per page</span><ul class="pagination m-0 ms-auto"><li class="page-item disabled"><a class="page-link" href="#">prev</a></li><li class="page-item active"><a class="page-link" href="#">1</a></li><li class="page-item"><a class="page-link" href="#">2</a></li><li class="page-item"><a class="page-link" href="#">3</a></li><li class="page-item"><a class="page-link" href="#">next</a></li></ul></div></div></div>'''
    write("log.html", "Log", "log.html", body, pretitle="Event stream")

def commit():
    plan = '''<pre class="plan mb-0"><span class="c2">$ slai commit --dry-run</span>
integration  slai/integration (22e7e63)
onto         main (81029a2)

  would apply 14 commits, oldest first, as one fast-forward-free merge:
    191fc37  [T-0071] slai status --stats: per-agent and per-harness/model quality numbers
    d8a91cb  Merge [T-0072] TUI send box: Tab-completes task ids, branch names and agent ids
    c767b84  Merge [T-0071] Per-agent quality stats
    …        (11 more)

  conflicts with main: none
  your working tree: clean
  main is not touched until you run `slai commit`, which stays a CLI-only action.</pre>'''
    rev = '''<pre class="plan mb-0"><span class="c2">$ slai revert T-0076 --dry-run</span>
would run, on slai/integration:
  git revert -m 1 7c4be21     # Merge [T-0076] False hooks_failing alert
files touched: internal/supervisor/health.go, internal/hooks/fallback.go
tasks that depend on T-0076: none
the task becomes "reverted"; its engineer is told at the next wake.</pre>'''
    opts = "".join(f'<option>{t[0]} · {t[4][:46]}</option>' for t in TASKS if t[1] == "merged")
    body = f'''{banner("info", "lock", "<strong>Read-only plans.</strong> Applying <span class='mono'>slai commit</span> or <span class='mono'>slai revert</span> stays on the command line, where you can see exactly what it does.")}
<div class="row row-cards"><div class="col-lg-7">{card(plan, "What slai commit would do", '<button class="btn btn-sm" data-toast="Plan refreshed from the current branch state." data-kind="info" data-title="Refreshed"><i class="ti ti-refresh me-1"></i>Refresh</button>', cls="mb-3")}</div>
<div class="col-lg-5">{card(f'<label class="form-label" for="rv">Merged task</label><select class="form-select mb-3" id="rv">{opts}</select>{rev}', "What slai revert would do", cls="mb-3")}</div></div>'''
    write("commit.html", "Commit preview", "commit.html", body, pretitle="Integration, read-only")

if __name__ == "__main__":
    import pages_extra; pages_extra.build(globals())
    dashboard(); dashboard(True); agents_list(); agent_detail(); board(); task_detail(); reviews(); inbox(); stats(); memory(); doctor(); log(); commit()
    pages_extra.post(globals())
    print("wrote", len(SCREENS), "pages")
    open(os.path.join(OUT, "assets", "favicon.svg"), "w").write('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#6366f1"/><stop offset="1" stop-color="#8b5cf6"/></linearGradient></defs><rect width="32" height="32" rx="8" fill="url(#g)"/><text x="16" y="23" font-family="Inter,sans-serif" font-size="20" font-weight="700" fill="#fff" text-anchor="middle">s</text></svg>')
    pages = sorted(SCREENS)
    open(os.path.join(OUT, "pages.txt"), "w").write("\n".join(pages) + "\n")
