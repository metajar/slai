// Runs in <head> before first paint so there is no light/dark flash.
// ?theme=dark|light forces and stores a theme (used by the screenshot script);
// otherwise: stored choice, else the OS preference. All storage access is wrapped.
(function () {
  var t = null;
  try {
    var q = new URLSearchParams(location.search).get("theme");
    if (q === "dark" || q === "light") { localStorage.setItem("sl-theme", q); }
    t = localStorage.getItem("sl-theme");
  } catch (e) {}
  if (!t) { t = window.matchMedia && matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"; }
  document.documentElement.setAttribute("data-bs-theme", t);
})();
