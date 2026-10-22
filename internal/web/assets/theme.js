/* theme.js: loaded synchronously in <head>, before first paint. When the page is in "system"
   mode (no stored preference) the server leaves data-bs-theme off and this sets it from the OS
   setting, then follows later changes. An explicit light or dark is rendered by the server and
   left alone. It reads no cookie, builds no HTML and makes no request. */
(function () {
  "use strict";
  var root = document.documentElement;
  var mq = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
  function apply() {
    if (root.getAttribute("data-slai-theme") !== "system") { return; }
    root.setAttribute("data-bs-theme", mq && mq.matches ? "dark" : "light");
  }
  apply();
  if (mq) {
    if (mq.addEventListener) { mq.addEventListener("change", apply); } else if (mq.addListener) { mq.addListener(apply); }
  }
})();
