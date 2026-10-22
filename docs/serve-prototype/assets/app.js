// slai serve prototype: all behaviour is fake and client-side. No network, no inline scripts.
(function () {
  "use strict";
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  var store = {
    get: function (k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set: function (k, v) { try { localStorage.setItem(k, v); } catch (e) {} },
    del: function (k) { try { localStorage.removeItem(k); } catch (e) {} }
  };

  /* ---------- toasts ---------- */
  function toast(msg, kind, title) {
    var host = $("#toasts");
    if (!host) return;
    var icon = { ok: "ti-circle-check", bad: "ti-ban", warn: "ti-alert-triangle", info: "ti-info-circle" }[kind || "info"];
    var color = { ok: "success", bad: "danger", warn: "warning", info: "primary" }[kind || "info"];
    var el = document.createElement("div");
    el.className = "toast show border-0";
    el.setAttribute("role", kind === "bad" ? "alert" : "status");
    var head = document.createElement("div");
    head.className = "toast-header";
    var i = document.createElement("i"); i.className = "ti " + icon + " me-2 text-" + color;
    var strong = document.createElement("strong"); strong.className = "me-auto"; strong.textContent = title || (kind === "bad" ? "Refused" : "Done");
    var small = document.createElement("small"); small.className = "text-secondary"; small.textContent = "just now";
    var close = document.createElement("button"); close.className = "btn-close"; close.setAttribute("aria-label", "Close");
    close.addEventListener("click", function () { el.remove(); });
    head.appendChild(i); head.appendChild(strong); head.appendChild(small); head.appendChild(close);
    var body = document.createElement("div"); body.className = "toast-body"; body.textContent = msg;
    el.appendChild(head); el.appendChild(body);
    host.appendChild(el);
    setTimeout(function () { el.remove(); }, kind === "bad" ? 9000 : 5000);
  }
  window.slToast = toast;

  /* ---------- theme ---------- */
  var tt = $("#theme-toggle");
  function syncThemeIcon() {
    var dark = document.documentElement.getAttribute("data-bs-theme") === "dark";
    if (tt) { tt.innerHTML = '<i class="ti ' + (dark ? "ti-sun" : "ti-moon") + '"></i>'; tt.setAttribute("aria-label", dark ? "Switch to light theme" : "Switch to dark theme"); }
  }
  if (tt) tt.addEventListener("click", function () {
    var next = document.documentElement.getAttribute("data-bs-theme") === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-bs-theme", next);
    store.set("sl-theme", next); syncThemeIcon();
  });
  syncThemeIcon();

  /* ---------- team hold / pause banner (shared by every page) ---------- */
  var holdBanner = $("#hold-banner");
  function renderHold() {
    var h = store.get("sl-hold");
    if (holdBanner) holdBanner.hidden = !h;
    var nm = $("#hold-name"); if (nm && h) nm.textContent = h;
    $$("[data-hold-open]").forEach(function (b) { b.hidden = !!h; });
    $$("[data-hold-release]").forEach(function (b) { b.hidden = !h; });
  }
  $$("[data-hold-release]").forEach(function (b) {
    b.addEventListener("click", function () { store.del("sl-hold"); renderHold(); toast("Wake-ups resume. Anything that queued while on hold is delivered.", "ok", "Team resumed"); });
  });
  var holdGo = $("[data-hold-confirm]");
  if (holdGo) holdGo.addEventListener("click", function () {
    var n = $("#hold-input"), name = (n && n.value.trim()) || "hold";
    store.set("sl-hold", name); renderHold();
    var m = $("#hold-modal"); if (m && window.bootstrap) bootstrap.Modal.getOrCreateInstance(m).hide();
    toast("Nothing will be woken, nudged or restarted. Sessions stay up.", "warn", "Team on hold: " + name);
  });
  renderHold();

  /* ---------- generic fake actions: <button data-toast="..." data-kind="ok|bad|warn|info" data-title="..."> ---------- */
  document.addEventListener("click", function (e) {
    var t = e.target.closest("[data-toast]");
    if (!t) return;
    if (t.tagName === "A") e.preventDefault();
    toast(t.getAttribute("data-toast"), t.getAttribute("data-kind") || "info", t.getAttribute("data-title") || undefined);
    var hide = t.getAttribute("data-hide"); if (hide) { var el = document.getElementById(hide); if (el) el.hidden = true; }
  });
  $$("form[data-fake]").forEach(function (f) {
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      toast(f.getAttribute("data-fake"), "ok", f.getAttribute("data-title") || "Sent");
      var inp = $("input[type=text],textarea", f); if (inp) inp.value = "";
    });
  });

  /* ---------- kanban ---------- */
  var board = $("#board");
  var TERMINAL = { merged: 1, cancelled: 1, reverted: 1 };
  var COLNAME = { assigned: "assigned", in_progress: "in progress", blocked: "blocked", in_review: "in review", merged: "merged", cancelled: "cancelled", reverted: "reverted" };
  // The web never has more power than the TUI: only these drops do something.
  function verdict(id, from, to) {
    if (from === to) return { ok: true, kind: "reorder" };
    if (TERMINAL[from]) return { ok: false, why: id + " is " + COLNAME[from] + ", which is final. " + (from === "merged" ? "A merged task can only be taken back with `slai revert " + id + "` (see Commit preview)." : "Create a new task instead.") };
    if (to === "cancelled") return { ok: true, kind: "cancel" };
    if (from === "blocked" && (to === "assigned" || to === "in_progress")) return { ok: true, kind: "unblock" };
    if (to === "merged") return { ok: false, why: id + " can't be moved to merged here. A task merges when the principal approves its review (approve_review); the web never approves or merges." };
    if (to === "reverted") return { ok: false, why: "Reverting is a git operation: `slai revert " + id + "`. The Commit preview page shows the plan, read-only." };
    if (to === "in_review") return { ok: false, why: id + " moves to in review when its engineer calls request_review, not by dragging." };
    if (from === "in_review" && to === "in_progress") return { ok: false, why: "illegal transition in_review → in_progress. A review goes back to the engineer only through request_changes (principal)." };
    if (to === "blocked") return { ok: false, why: id + " blocks itself: the engineer calls report(blocked) with what it needs. You can answer in the Inbox." };
    return { ok: false, why: "illegal transition " + from + " → " + to + ": engineers move " + id + " forward with the MCP tools (report, request_review)." };
  }
  function refuse(id, from, to) {
    var v = verdict(id, from, to);
    toast(v.why || "ok", "bad", "Illegal move refused");
  }
  window.slRefuse = refuse;
  function updateCounts() {
    $$(".kcol", board).forEach(function (c) { var n = $$(".tcard", c).filter(function (x) { return !x.hidden; }).length; var b = $(".count", c); if (b) b.textContent = n; var e = $(".kcol-empty", c); if (e) e.hidden = n > 0; });
  }
  if (board && window.Sortable) {
    $$(".kcol-body", board).forEach(function (body) {
      Sortable.create(body, {
        group: "tasks", forceFallback: /[?&]fallback=1/.test(location.search), disabled: document.documentElement.hasAttribute("data-readonly"), animation: 160, ghostClass: "sortable-ghost", draggable: ".tcard", filter: ".kcol-empty", delay: 120, delayOnTouchOnly: true,
        onMove: function (evt) {
          var to = evt.to.closest(".kcol"), from = evt.dragged.getAttribute("data-status");
          $$(".kcol", board).forEach(function (c) { c.classList.remove("drop-ok", "drop-bad"); });
          var v = verdict(evt.dragged.getAttribute("data-id"), from, to.getAttribute("data-status"));
          to.classList.add(v.ok ? "drop-ok" : "drop-bad");
          return true;
        },
        onEnd: function (evt) {
          $$(".kcol", board).forEach(function (c) { c.classList.remove("drop-ok", "drop-bad"); });
          var card = evt.item, id = card.getAttribute("data-id"), from = card.getAttribute("data-status"), to = evt.to.closest(".kcol").getAttribute("data-status");
          var v = verdict(id, from, to);
          if (!v.ok) {
            // snap back to where it was
            var ref = evt.from.children[evt.oldIndex] || null;
            evt.from.insertBefore(card, ref);
            refuse(id, from, to); updateCounts(); return;
          }
          if (v.kind === "reorder") {
            if (evt.oldIndex !== evt.newIndex) toast(id + " moved " + (evt.newIndex < evt.oldIndex ? "up" : "down") + " in " + COLNAME[to] + ". Same call as `slai priority " + id + " " + (evt.newIndex < evt.oldIndex ? "up" : "down") + "`.", "ok", "Priority changed");
          } else if (v.kind === "cancel") {
            card.setAttribute("data-status", "cancelled"); card.classList.add("cancelled");
            toast(id + " cancelled. Its engineer is told; the worktree is left alone.", "ok", "Task cancelled");
          } else if (v.kind === "unblock") {
            card.setAttribute("data-status", to);
            toast(id + " unblocked and handed back to its engineer (" + COLNAME[to] + ").", "ok", "Task unblocked");
          }
          updateCounts();
        }
      });
    });
    if (location.hash === "#demo-illegal") { setTimeout(function () { refuse("T-0079", "in_review", "merged"); }, 250); }
  }
  $$("[data-demo-illegal]").forEach(function (b) { b.addEventListener("click", function () { refuse(b.getAttribute("data-id"), b.getAttribute("data-from"), b.getAttribute("data-to")); }); });

  // filters: state lives in the query string so a view can be bookmarked
  var fa = $("#f-assignee"), ft = $("#f-text"), fn = $("#f-needs");
  function applyFilters(push) {
    if (!board) return;
    var a = fa ? fa.value : "", t = ft ? ft.value.toLowerCase() : "", n = fn && fn.checked;
    $$(".tcard", board).forEach(function (c) {
      var ok = (!a || c.getAttribute("data-assignee") === a) && (!t || c.textContent.toLowerCase().indexOf(t) >= 0) && (!n || c.getAttribute("data-needs") === "1");
      c.hidden = !ok;
    });
    updateCounts();
    if (push) { var p = new URLSearchParams(); if (a) p.set("assignee", a); if (t) p.set("q", t); if (n) p.set("needs", "1"); try { history.replaceState(null, "", location.pathname + (p.toString() ? "?" + p : "") + location.hash); } catch (e) {} }
  }
  if (board) {
    var qs = new URLSearchParams(location.search);
    if (fa && qs.get("assignee")) fa.value = qs.get("assignee");
    if (ft && qs.get("q")) ft.value = qs.get("q");
    if (fn && qs.get("needs")) fn.checked = true;
    [fa, ft, fn].forEach(function (el) { if (el) el.addEventListener("input", function () { applyFilters(true); }); });
    applyFilters(false);
  }

  /* ---------- inbox ---------- */
  function refreshNeedsMe() {
    if (!$("[data-needs-item]")) return; // only the Inbox has the items; elsewhere the static count stays
    var n = $$("[data-needs-item]").filter(function (x) { return !x.hidden && !x.classList.contains("answered"); }).length;
    $$("[data-needs-badge]").forEach(function (b) { b.textContent = n; b.hidden = n === 0; });
    var base = document.title.replace(/^\(\d+\)\s*/, "");
    document.title = (n ? "(" + n + ") " : "") + base;
    var h = $("#needs-count"); if (h) h.textContent = n;
  }
  $$("form[data-answer]").forEach(function (f) {
    f.addEventListener("submit", function (e) {
      e.preventDefault();
      var item = f.closest("[data-needs-item]");
      var inp = $("input[type=text]", f);
      if (inp && !inp.value.trim()) { inp.focus(); toast("Type an answer first.", "warn", "Nothing to send"); return; }
      item.classList.add("answered");
      var st = $(".answer-state", item); if (st) { st.innerHTML = ""; var c = document.createElement("span"); c.className = "chip chip-ok"; c.innerHTML = '<i class="ti ti-check"></i>'; c.appendChild(document.createTextNode("Answered, delivered to " + f.getAttribute("data-answer"))); st.appendChild(c); }
      f.hidden = true;
      toast("Delivered to " + f.getAttribute("data-answer") + " the way a message from you is (SendUser).", "ok", "Answer sent");
      refreshNeedsMe();
    });
  });
  $$("[data-dismiss]").forEach(function (b) {
    b.addEventListener("click", function () {
      var item = b.closest("[data-needs-item]"); item.hidden = true;
      toast("Hidden in this browser only. The underlying state (" + b.getAttribute("data-dismiss") + ") is unchanged and shows again on Doctor.", "info", "Dismissed in the UI");
      refreshNeedsMe();
    });
  });
  refreshNeedsMe();

  /* ---------- generic table/list filtering: input[data-filter-target=#id] over [data-row] ---------- */
  function setupFilter(scope) {
    var inputs = $$("[data-filter]", scope);
    function run() {
      var rows = $$("[data-row]", scope);
      rows.forEach(function (r) {
        var ok = true;
        inputs.forEach(function (i) {
          var k = i.getAttribute("data-filter"), v = (i.value || "").toLowerCase();
          if (!v) return;
          if (k === "text") ok = ok && r.textContent.toLowerCase().indexOf(v) >= 0;
          else ok = ok && (r.getAttribute("data-" + k) || "").toLowerCase() === v;
        });
        r.hidden = !ok;
      });
      var cnt = $("[data-filter-count]", scope); if (cnt) cnt.textContent = rows.filter(function (r) { return !r.hidden; }).length;
      var emp = $("[data-filter-empty]", scope); if (emp) emp.hidden = rows.some(function (r) { return !r.hidden; });
    }
    inputs.forEach(function (i) { i.addEventListener("input", run); });
    run();
  }
  $$("[data-filter-scope]").forEach(setupFilter);

  /* ---------- memory: open a fact ---------- */
  $$("[data-fact]").forEach(function (r) {
    r.addEventListener("click", function () {
      var src = document.getElementById("fd-" + r.getAttribute("data-fact"));
      var dst = $("#fact-body"); if (!src || !dst) return;
      dst.innerHTML = src.innerHTML;
      var oc = $("#fact-offcanvas");
      if (oc && window.bootstrap) bootstrap.Offcanvas.getOrCreateInstance(oc).show();
    });
    r.addEventListener("keydown", function (e) { if (e.key === "Enter") r.click(); });
  });

  /* ---------- copy a command ---------- */
  $$("[data-copy]").forEach(function (b) {
    b.addEventListener("click", function () {
      var t = b.getAttribute("data-copy");
      try { navigator.clipboard.writeText(t).then(function () { toast(t, "ok", "Copied"); }, function () { toast("Select the command and copy it: " + t, "info", "Copy failed"); }); }
      catch (e) { toast("Select the command and copy it: " + t, "info", "Copy failed"); }
    });
  });

  /* ---------- read-only mode (slai serve --read-only): every write is switched off, the server would answer 403 ---------- */
  if (document.documentElement.hasAttribute("data-readonly")) {
    var why = "Read-only mode: this action is switched off (the server answers 403).";
    $$(".sendbox input, .sendbox button, [data-write], [data-hold-open], [data-answer] input, [data-answer] button").forEach(function (el) {
      el.disabled = true; el.setAttribute("aria-disabled", "true"); el.title = why; el.classList.add("disabled");
      el.removeAttribute("data-bs-toggle");
    });
    $$("a[data-write]").forEach(function (a) { a.removeAttribute("href"); });
    $$("[data-needs-item] form").forEach(function (f) { f.addEventListener("submit", function (e) { e.preventDefault(); }); });
    $$("[data-demo-illegal]").forEach(function (b) { b.disabled = true; });
  }

  /* ---------- New agent form: same rules as config.Validate (copied here as fake client-side validation) ---------- */
  var naf = $("#newagent-form");
  if (naf) {
    var ID_RE = /^[a-z][a-z0-9-]{1,23}$/;
    var existing = (naf.getAttribute("data-existing") || "").split(",");
    var f = function (n) { return naf.elements[n]; };
    var setErr = function (el, msg) {
      var fb = el.parentNode.querySelector(".invalid-feedback");
      if (msg) { el.classList.add("is-invalid"); if (fb) fb.textContent = msg; } else { el.classList.remove("is-invalid"); if (fb) fb.textContent = ""; }
    };
    var syncFields = function () {
      var role = f("role").value, h = f("harness").value;
      $("#na-persona-wrap").hidden = role !== "focus_group";
      $("#na-perm-wrap").hidden = h === "pi";
      var hint = $("#na-harness-hint"), mh = $("#na-model-hint");
      if (h === "claude") { hint.innerHTML = 'claude needs the <span class="mono">claude</span> binary on PATH.'; mh.innerHTML = 'a key in <span class="mono">models:</span> (opus, sonnet, haiku) or a raw <span class="mono">claude-…</span> id.'; }
      else { hint.innerHTML = h + ' needs the <span class="mono">' + h + '</span> binary on PATH.'; mh.innerHTML = 'written <span class="mono">provider/model</span>, e.g. <span class="mono">zai/glm-5</span>.'; }
    };
    var validate = function () {
      var errs = [], id = f("id").value.trim(), model = f("model").value.trim(), role = f("role").value, h = f("harness").value;
      ["role", "harness", "id", "model", "perm"].forEach(function (n) { setErr(f(n), ""); });
      if (!ID_RE.test(id)) { setErr(f("id"), 'id "' + id + '" must match ^[a-z][a-z0-9-]{1,23}$'); errs.push("id"); }
      else if (existing.indexOf(id) >= 0) { setErr(f("id"), 'duplicate id "' + id + '"'); errs.push("id"); }
      if (role === "orchestrator") { setErr(f("role"), "exactly one orchestrator required, found 2"); errs.push("role"); }
      if (role === "principal_engineer") { setErr(f("role"), "at most one principal_engineer allowed, found 2"); errs.push("role"); }
      if (role === "product_manager") { setErr(f("role"), "at most one product_manager allowed, found 2"); errs.push("role"); }
      if (h === "claude") {
        if (["opus", "sonnet", "haiku"].indexOf(model) < 0 && model.indexOf("claude-") !== 0) { setErr(f("model"), 'model "' + model + '" is neither a key in models nor a raw string starting with "claude-"'); errs.push("model"); }
      } else {
        var parts = model.split("/");
        if (parts.length < 2 || !parts[0].trim() || !parts.slice(1).join("/").trim()) { setErr(f("model"), 'model "' + model + '" must be provider/model for harness ' + h); errs.push("model"); }
      }
      return errs;
    };
    ["role", "harness"].forEach(function (n) { f(n).addEventListener("change", syncFields); });
    var finish = function () {
      var errs = validate(), sum = $("#na-summary");
      if (errs.length) { sum.hidden = false; $("#na-summary-text").textContent = errs.length + " field" + (errs.length > 1 ? "s" : "") + " refused by the validator. team.yaml is unchanged."; return false; }
      sum.hidden = true;
      $("#na-ok-id").textContent = $("#na-ok-yaml-id").textContent = f("id").value.trim();
      $("#na-ok-yaml-role").textContent = f("role").value; $("#na-ok-yaml-h").textContent = f("harness").value;
      $("#na-ok-yaml-m").textContent = f("model").value.trim(); $("#na-ok-yaml-f").textContent = f("focus").value.trim() || "(none)";
      naf.hidden = true; $("#na-success").hidden = false;
      toast("team.yaml written and a web_action event recorded. The supervisor will start the session.", "ok", f("id").value.trim() + " added");
      return true;
    };
    naf.addEventListener("submit", function (e) { e.preventDefault(); if (document.documentElement.hasAttribute("data-readonly")) return; finish(); });
    syncFields();
    if (location.hash === "#demo-error") { f("id").value = "Eng_5"; f("harness").value = "opencode"; f("model").value = "sonnet"; syncFields(); finish(); }
    if (location.hash === "#demo-success") { f("id").value = "eng-5"; finish(); }
  }

  /* ---------- sign-in page (fake: no token is ever accepted or stored) ---------- */
  var sif = $("#signin-form");
  if (sif) {
    var tok = $("#si-token"), eye = $("#si-eye");
    eye.addEventListener("click", function () { tok.type = tok.type === "password" ? "text" : "password"; });
    sif.addEventListener("submit", function (e) {
      e.preventDefault();
      var ok = /^[0-9a-f]{64}$/i.test(tok.value.trim());
      $("#si-error").hidden = ok;
      if (ok) { toast("Token accepted: the session cookie is set and the token is stripped from the address.", "ok", "Signed in"); setTimeout(function () { location.href = "index.html"; }, 900); }
    });
    if (location.hash === "#demo-error") { tok.type = "text"; tok.value = "hunter2"; $("#si-error").hidden = false; }
  }
})();
