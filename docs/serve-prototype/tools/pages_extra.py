"""Pages added by the principal/user addenda to serve.md: the New Agent form, the sign-in
page and the read-only-mode variants. gen.py calls build(globals()) and post(globals())."""
import os


def build(g):
    card, write, AGENTS, SCREENS, OUT = g["card"], g["write"], g["AGENTS"], g["SCREENS"], g["OUT"]
    roles = ["orchestrator", "product_manager", "principal_engineer", "engineer", "focus_group"]
    harn = ["claude", "opencode", "pi"]
    perms = ["(team default)", "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"]
    ro = "".join(f'<option value="{r}"{" selected" if r == "engineer" else ""}>{r}</option>' for r in roles)
    ho = "".join(f"<option>{h}</option>" for h in harn)
    po = "".join(f"<option>{x}</option>" for x in perms)
    existing = ",".join(a["id"] for a in AGENTS)
    form = f"""<form id="newagent-form" novalidate data-existing="{existing}">
<div class="alert alert-danger" id="na-summary" role="alert" hidden><div class="d-flex gap-2"><i class="ti ti-alert-octagon fs-2"></i><div><strong>Nothing was written.</strong> <span id="na-summary-text">Fix the fields below.</span></div></div></div>
<div class="row g-3">
 <div class="col-md-6"><label class="form-label" for="na-role">Role</label><select class="form-select" id="na-role" name="role">{ro}</select><div class="invalid-feedback"></div></div>
 <div class="col-md-6"><label class="form-label" for="na-harness">Harness</label><select class="form-select" id="na-harness" name="harness">{ho}</select><div class="form-hint" id="na-harness-hint">claude needs the <span class="mono">claude</span> binary on PATH.</div><div class="invalid-feedback"></div></div>
 <div class="col-md-6"><label class="form-label" for="na-id">Agent id</label><input class="form-control" id="na-id" name="id" type="text" autocomplete="off" placeholder="eng-5" value="eng-5"><div class="form-hint">lowercase letters, digits and dashes; starts with a letter; 2–24 characters.</div><div class="invalid-feedback"></div></div>
 <div class="col-md-6"><label class="form-label" for="na-focus">Focus</label><input class="form-control" id="na-focus" name="focus" type="text" placeholder="backend, frontend, docs…" value="frontend"><div class="form-hint">One word that helps orch route work. Optional.</div></div>
 <div class="col-md-6"><label class="form-label" for="na-model">Model</label><input class="form-control" id="na-model" name="model" type="text" value="sonnet" autocomplete="off"><div class="form-hint" id="na-model-hint">a key in <span class="mono">models:</span> (opus, sonnet, haiku) or a raw <span class="mono">claude-…</span> id.</div><div class="invalid-feedback"></div></div>
 <div class="col-md-6" id="na-perm-wrap"><label class="form-label" for="na-perm">Permission mode</label><select class="form-select" id="na-perm" name="perm">{po}</select><div class="form-hint">Passed to <span class="mono">claude --permission-mode</span>. pi has no permission layer, so the field disappears for it.</div><div class="invalid-feedback"></div></div>
 <div class="col-12" id="na-persona-wrap" hidden><label class="form-label" for="na-persona">Persona</label><textarea class="form-control" id="na-persona" rows="3" placeholder="Who is this focus-group member? What do they care about?"></textarea><div class="form-hint">Only for focus groups.</div></div>
</div>
<div class="mt-4 d-flex gap-2 flex-wrap align-items-center"><button class="btn btn-primary" type="submit" data-write><i class="ti ti-user-plus me-1"></i>Add agent</button><a class="btn" href="agents.html">Cancel</a>
<span class="reason ms-auto"><i class="ti ti-lock"></i> same validation and same <span class="mono">team.yaml</span> writer as the TUI form · logged as a <span class="mono">web_action</span> event</span></div>
</form>"""
    success = """<div id="na-success" hidden><div class="text-center py-4"><span class="avatar avatar-xl bg-green-lt mb-3"><i class="ti ti-check" style="font-size:2rem"></i></span><h3 class="mb-1"><span id="na-ok-id">eng-5</span> was added</h3>
<p class="text-secondary mb-3">Written to <span class="mono">.slai/team.yaml</span>. The supervisor picks it up in a few seconds and starts the session; the agent then appears on the dashboard and in the list.</p></div>
<div class="pane mb-3" style="max-height:none"><span class="dim"># .slai/team.yaml (appended)</span>
<span class="c4">- id:</span> <span id="na-ok-yaml-id">eng-5</span>
  <span class="c4">role:</span> <span id="na-ok-yaml-role">engineer</span>
  <span class="c4">harness:</span> <span id="na-ok-yaml-h">claude</span>
  <span class="c4">model:</span> <span id="na-ok-yaml-m">sonnet</span>
  <span class="c4">focus:</span> <span id="na-ok-yaml-f">frontend</span></div>
<div class="d-flex gap-2 flex-wrap justify-content-center"><a class="btn btn-primary" href="agents.html"><i class="ti ti-robot me-1"></i>See it in Agents</a><a class="btn" href="agent-new.html" data-reset-form>Add another</a></div></div>"""
    side = """<ul class="small ps-3 mb-0"><li class="mb-2">An invalid id, role, model or harness is refused with the validator’s own message, inline. Nothing is written.</li><li class="mb-2">A duplicate id is refused; <span class="mono">team.yaml</span> stays as it was.</li><li class="mb-2">Not allowed in <span class="mono">--read-only</span> mode: the form is hidden and the server answers 403.</li><li>Try it: set the id to <span class="mono">eng-3</span> (duplicate) or <span class="mono">Eng_5</span>, or pick opencode and leave the model as <span class="mono">sonnet</span>.</li></ul>
<hr><div class="btn-list"><a class="btn btn-sm btn-outline-danger" href="agent-new.html#demo-error">Show an error state</a><a class="btn btn-sm btn-outline-success" href="agent-new.html#demo-success">Show the success state</a></div>"""
    body = f"""<div class="row row-cards"><div class="col-lg-8">{card(form + success, "New agent", cls="mb-3")}</div><div class="col-lg-4">{card(side, "What happens", cls="mb-3")}</div></div>"""
    write("agent-new.html", "New agent", "agents.html", body, pretitle="Agents · add to team.yaml")

    signin = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Sign in · slai</title>
<link rel="icon" href="assets/favicon.svg" type="image/svg+xml">
<script src="assets/theme.js"></script>
<link rel="stylesheet" href="vendor/tabler/css/tabler.min.css">
<link rel="stylesheet" href="vendor/icons/tabler-icons.min.css">
<link rel="stylesheet" href="assets/app.css">
</head>
<body class="d-flex flex-column">
<div class="proto-ribbon">Prototype · fake data · this is the page a non-loopback visitor sees without a valid session</div>
<div class="page page-center"><div class="container container-tight py-4">
  <div class="text-center mb-4"><a href="index.html" class="d-inline-flex align-items-center text-reset text-decoration-none"><span class="brand-mark">s</span><span class="brand-name fs-2">slai</span></a></div>
  <div class="card card-md"><div class="card-body">
    <h2 class="h2 text-center mb-1">Enter the access token</h2>
    <p class="text-secondary text-center mb-4">This <span class="mono">slai serve</span> listens on <span class="mono">0.0.0.0:8080</span>, so it needs the token it printed when it started (<span class="mono">slai status</span> shows it too).</p>
    <div class="alert alert-danger" id="si-error" role="alert" hidden><i class="ti ti-alert-octagon me-1"></i>That token was not accepted. Copy it again from the terminal; it is 64 characters.</div>
    <form id="signin-form" novalidate>
      <div class="mb-3"><label class="form-label" for="si-token">Access token</label>
        <div class="input-group input-group-flat"><input class="form-control" id="si-token" type="password" autocomplete="off" placeholder="paste the token from your terminal"><span class="input-group-text"><button class="btn btn-sm btn-icon btn-ghost-secondary" type="button" id="si-eye" aria-label="Show token"><i class="ti ti-eye"></i></button></span></div></div>
      <button class="btn btn-primary w-100" type="submit"><i class="ti ti-key me-1"></i>Sign in</button>
    </form>
  </div></div>
  <div class="banner banner-hold mt-3"><i class="ti ti-alert-triangle text-warning"></i><div class="small"><strong>Anyone with this token can see every agent’s pane</strong> (their terminal output, including file contents and anything typed). Treat it like a password. The connection is plain HTTP: use it only on a network you trust, or behind a reverse proxy with TLS.</div></div>
  <div class="text-center text-secondary small mt-3">On the same machine (<span class="mono">127.0.0.1</span>) there is no token page: the first visit simply starts a session. <a href="index.html">See the dashboard</a> · <a href="signin.html#demo-error">wrong-token state</a></div>
</div></div>
<div class="toast-container position-fixed top-0 end-0 p-3 mt-5" id="toasts" aria-live="polite"></div>
<script src="vendor/tabler/js/tabler.min.js"></script>
<script src="assets/app.js"></script>
</body>
</html>
"""
    SCREENS["signin.html"] = "Sign in"
    with open(os.path.join(OUT, "signin.html"), "w") as f:
        f.write(signin)


def post(g):
    """Read-only-mode variants: same pages, banner on top, every writing control switched off by app.js."""
    OUT, SCREENS, RO = g["OUT"], g["SCREENS"], g["RO_BANNER"]
    for src, dst in (("index.html", "index-readonly.html"), ("board.html", "board-readonly.html"), ("agent.html", "agent-readonly.html")):
        with open(os.path.join(OUT, src)) as f:
            h = f.read()
        h = h.replace('<html lang="en">', '<html lang="en" data-readonly>', 1)
        h = h.replace('<div class="page-body"><div class="container-xl">', '<div class="page-body"><div class="container-xl">\n      ' + RO, 1)
        h = h.replace("<title>", "<title>Read-only · ", 1)
        with open(os.path.join(OUT, dst), "w") as f:
            f.write(h)
        SCREENS[dst] = SCREENS[src] + " (read-only mode)"
