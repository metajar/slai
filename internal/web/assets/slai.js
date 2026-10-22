/* slai.js: the only first-party script. It wires SortableJS drops on the board to htmx POSTs
   (htmx adds the CSRF header and the session cookie, and swaps the reply into the page itself).
   It never builds HTML, evaluates text or handles a token. */
(function () {
  "use strict";
  if (!window.Sortable || !window.htmx) { return; }

  var wired = new WeakSet();
  var pending = {}; /* request path -> where the card came from, to snap it back on a refusal */

  function neighbour(el, step) {
    for (var n = el[step]; n; n = n[step]) {
      if (n.classList && n.classList.contains("slai-card")) { return n.getAttribute("data-task"); }
    }
    return "";
  }

  function dropped(evt) {
    var card = evt.item;
    var id = card.getAttribute("data-task");
    var to = evt.to.getAttribute("data-col");
    var path = "/tasks/" + encodeURIComponent(id) + "/move";
    var view = document.getElementById("board-cols");
    if (evt.from === evt.to && evt.oldIndex === evt.newIndex) { return; }
    pending[path] = { card: card, from: evt.from, index: evt.oldIndex };
    window.htmx.ajax("POST", path, {
      target: "#board-msg",
      swap: "innerHTML",
      values: {
        to: to,
        before: neighbour(card, "nextElementSibling"),
        after: neighbour(card, "previousElementSibling"),
        view: view ? (view.getAttribute("data-view") || "") : ""
      }
    });
  }

  function wire() {
    var lists = document.querySelectorAll(".slai-col-body");
    for (var i = 0; i < lists.length; i++) {
      if (wired.has(lists[i])) { continue; }
      wired.add(lists[i]);
      window.Sortable.create(lists[i], {
        group: "tasks",
        draggable: ".slai-card",
        filter: ".slai-col-empty",
        animation: 150,
        delay: 150,
        delayOnTouchOnly: true,
        onEnd: dropped
      });
    }
  }

  /* a refused drop (any 4xx): htmx has already shown the service's words in #board-msg;
     put the card back where it was */
  document.body.addEventListener("htmx:afterRequest", function (e) {
    var cfg = e.detail && e.detail.requestConfig;
    var p = cfg && pending[cfg.path];
    if (!p) { return; }
    delete pending[cfg.path];
    if (!e.detail.successful) {
      p.from.insertBefore(p.card, p.from.children[p.index] || null);
    }
  });

  /* the columns are replaced after every write and every refetch: wire the new lists */
  document.body.addEventListener("htmx:afterSettle", wire);
  wire();
})();

/* Live regions. GET /events streams small notices ({kind, topic, ids}), never HTML. An element
   with data-live="topic topic ..." and data-live-src="/f/..." is refetched with htmx (so the
   CSRF header and swap rules apply) when a notice names one of its topics, and every live
   element is refetched on a resync. A pane:<agent> topic is also named in ?watch=, so the
   server captures a terminal only while a stream asks for it. A hidden tab closes its stream
   (nothing is watched, nothing captured) and reopens it, refetching, when shown again.
   While the stream is down a visible page refetches every 30 s. */
(function () {
  "use strict";
  if (!window.EventSource || !window.htmx) { return; }
  var es = null, up = false;

  function regions() { return document.querySelectorAll("[data-live][data-live-src]"); }

  function refetch(topic) {
    var els = regions();
    for (var i = 0; i < els.length; i++) {
      var topics = els[i].getAttribute("data-live").split(" ");
      if (topic && topics.indexOf(topic) < 0) { continue; }
      window.htmx.ajax("GET", els[i].getAttribute("data-live-src"), { target: els[i], swap: "outerHTML" });
    }
  }

  function watching() {
    var seen = [], els = regions();
    for (var i = 0; i < els.length; i++) {
      var topics = els[i].getAttribute("data-live").split(" ");
      for (var j = 0; j < topics.length; j++) {
        if (topics[j].indexOf("pane:") === 0 && seen.indexOf(topics[j]) < 0) { seen.push(topics[j]); }
      }
    }
    return seen.length ? "?watch=" + encodeURIComponent(seen.join(",")) : "";
  }

  function open() {
    if (es) { return; }
    es = new EventSource("/events" + watching());
    es.onopen = function () { up = true; };
    es.onerror = function () { up = false; };
    es.addEventListener("resync", function () { refetch(""); });
    es.addEventListener("topic", function (e) {
      var n;
      try { n = JSON.parse(e.data); } catch (err) { return; }
      refetch(n.topic);
    });
  }

  function close() {
    if (es) { es.close(); es = null; up = false; }
  }

  document.addEventListener("visibilitychange", function () {
    if (document.hidden) { close(); } else { open(); refetch(""); }
  });
  window.setInterval(function () { if (!up && !document.hidden) { refetch(""); } }, 30000);
  if (!document.hidden) { open(); }
})();
