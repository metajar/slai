// Drives headless Chrome over the DevTools protocol (no npm packages: Node 22 has WebSocket)
// and screenshots every page of a running `slai serve --demo` at three sizes. Called by
// capture.sh, which starts the servers.
//
//   node capture.mjs <outdir> <pages.txt> <main-url> [--readonly URL] [--firstrun URL] [--signin URL]
//
// Output: <outdir>/<name>-light-1280.png, -dark-1280.png, -light-360.png. A console error, a
// failed request or a request to another origin is reported and makes the run fail; a page that
// scrolls sideways at 360 px is reported as a warning.
import { spawn } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const args = process.argv.slice(2);
const outDir = args[0], pagesFile = args[1], main = args[2];
const opt = (flag) => { const i = args.indexOf(flag); return i > 0 ? args[i + 1] : ""; };
const readonlyURL = opt("--readonly"), firstRunURL = opt("--firstrun"), signinURL = opt("--signin");
const CHROME = process.env.CHROME || "/usr/bin/google-chrome";
const SIZES = [["light-1280", 1280, 900, "light"], ["dark-1280", 1280, 900, "dark"], ["light-360", 360, 760, "light"]];
mkdirSync(outDir, { recursive: true });

const profile = mkdtempSync(join(tmpdir(), "slai-shots-"));
const port = 9300 + Math.floor(Math.random() * 500);
const chrome = spawn(CHROME, ["--headless=new", "--remote-debugging-port=" + port, "--user-data-dir=" + profile, "--no-sandbox", "--disable-gpu",
  "--hide-scrollbars", "--font-render-hinting=none", "--force-device-scale-factor=1", "about:blank"], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function wsURL() {
  for (let i = 0; i < 100; i++) {
    try { return (await (await fetch(`http://127.0.0.1:${port}/json/version`)).json()).webSocketDebuggerUrl; } catch { await sleep(100); }
  }
  throw new Error("Chrome did not start");
}

class CDP {
  constructor(url) { this.id = 0; this.cb = new Map(); this.on = new Map(); this.ws = new WebSocket(url);
    this.ready = new Promise((r) => (this.ws.onopen = r));
    this.ws.onmessage = (m) => { const d = JSON.parse(m.data);
      if (d.id && this.cb.has(d.id)) { const { res, rej } = this.cb.get(d.id); this.cb.delete(d.id); d.error ? rej(new Error(d.error.message)) : res(d.result); }
      else if (d.method) (this.on.get(d.sessionId || "") || []).forEach((f) => f(d.method, d.params)); }; }
  send(method, params = {}, sessionId) { const id = ++this.id; this.ws.send(JSON.stringify({ id, method, params, sessionId }));
    return new Promise((res, rej) => this.cb.set(id, { res, rej })); }
  listen(sessionId, f) { this.on.set(sessionId, [...(this.on.get(sessionId) || []), f]); }
}

let problems = 0, shots = 0;
const cdp = new CDP(await wsURL());
await cdp.ready;

async function openPage(base, theme, width, height) {
  const { targetId } = await cdp.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await cdp.send("Target.attachToTarget", { targetId, flatten: true });
  const found = [];
  cdp.listen(sessionId, (m, p) => {
    if (m === "Runtime.exceptionThrown") found.push("exception: " + (p.exceptionDetails.exception?.description || p.exceptionDetails.text));
    if (m === "Runtime.consoleAPICalled" && (p.type === "error" || p.type === "warning")) found.push("console " + p.type + ": " + p.args.map((a) => a.value ?? a.description).join(" "));
    if (m === "Network.loadingFailed" && !p.canceled) found.push("request failed: " + (p.errorText || ""));
    if (m === "Network.requestWillBeSent" && !p.request.url.startsWith(base) && !p.request.url.startsWith("data:") && !p.request.url.startsWith("about:")) found.push("external request: " + p.request.url);
  });
  for (const d of ["Page", "Runtime", "Network"]) await cdp.send(d + ".enable", {}, sessionId);
  await cdp.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: width < 600 }, sessionId);
  await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-reduced-motion", value: "reduce" }] }, sessionId);
  await cdp.send("Network.setCookie", { name: "slai_theme", value: theme, url: base }, sessionId);
  return { targetId, sessionId, found };
}

const evalJS = async (pg, expr) => (await cdp.send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true }, pg.sessionId)).result.value;

async function go(pg, url) {
  await cdp.send("Page.navigate", { url }, pg.sessionId);
  for (let i = 0; i < 100; i++) { if ((await evalJS(pg, "document.readyState")) === "complete") break; await sleep(100); }
  await sleep(500);
}

async function shoot(pg, file, width) {
  const h = Math.max(await evalJS(pg, "Math.ceil(document.documentElement.scrollHeight)"), 400);
  await cdp.send("Emulation.setDeviceMetricsOverride", { width, height: h, deviceScaleFactor: 1, mobile: width < 600 }, pg.sessionId);
  await sleep(200);
  const { data } = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false }, pg.sessionId);
  writeFileSync(join(outDir, file), Buffer.from(data, "base64"));
  shots++;
  if (width < 600 && (await evalJS(pg, "document.documentElement.scrollWidth > window.innerWidth + 1"))) pg.found.push("WARN: the page scrolls sideways at " + width + " px");
}

let warnings = 0;
async function close(pg, label) {
  const uniq = [...new Set(pg.found)];
  const bad = uniq.filter((x) => !x.startsWith("WARN:"));
  if (uniq.length) console.log(label + "\n  " + uniq.join("\n  "));
  if (bad.length) problems++;
  else if (uniq.length) warnings++;
  await cdp.send("Target.closeTarget", { targetId: pg.targetId });
}

// one page at the three sizes; setup(pg) may act on the page before the shot
async function capture(base, name, path, setup) {
  for (const [label, w, h, theme] of SIZES) {
    const pg = await openPage(base, theme, w, h);
    await go(pg, base + path);
    if (setup) await setup(pg);
    await shoot(pg, `${name}-${label}.png`, w);
    await close(pg, `${name}-${label}`);
  }
}

const pages = readFileSync(pagesFile, "utf8").split("\n").filter((l) => l.trim() && !l.startsWith("#")).map((l) => l.trim().split(/\s+/));
for (const [name, path] of pages) await capture(main, name, path);

// ---- state variants ----
if (firstRunURL) await capture(firstRunURL, "dashboard-first-run", "/");
if (readonlyURL) {
  await capture(readonlyURL, "dashboard-read-only", "/");
  await capture(readonlyURL, "board-read-only", "/board");
  await capture(readonlyURL, "agent-detail-read-only", "/agents/eng-1");
}
if (signinURL) await capture(signinURL, "sign-in", "/");

// a refused drop: the same request the board sends when a card is dropped on "merged"
await capture(main, "board-illegal-move", "/board", async (pg) => {
  await evalJS(pg, `htmx.ajax("POST", "/tasks/T-0007/move", {target: "#board-msg", swap: "innerHTML", values: {to: "merged"}}).then(() => true)`);
  await sleep(600);
});
// New Agent: a refused form (bad id), then a written one (last: it changes team.yaml of the demo)
const fillAgent = (id, role) => `(() => { const f = document.querySelector("form[action='/agents'], form[hx-post='/agents']"); if (!f) return "no form";
  const set = (n, v) => { const e = f.elements[n]; if (e) { e.value = v; e.dispatchEvent(new Event("input", {bubbles: true})); } };
  set("id", ${JSON.stringify(id)}); set("role", ${JSON.stringify(role)}); f.requestSubmit(); return "ok"; })()`;
await capture(main, "agent-new-error", "/agents/new", async (pg) => { await evalJS(pg, fillAgent("Bad Id!", "engineer")); await sleep(800); });
await capture(main, "agent-new-success", "/agents/new", async (pg) => { await evalJS(pg, fillAgent("eng-5", "engineer")); await sleep(800); });

await cdp.send("Browser.close").catch(() => {});
chrome.kill();
await sleep(1000);
try { rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 }); } catch { /* the temp profile is disposable */ }
const note = warnings ? ` (${warnings} with a layout warning, see above)` : "";
console.log(problems ? `${problems} screenshot(s) had problems` : `all clean: ${shots} screenshots, no console errors, no external requests${note}`);
process.exit(problems ? 1 : 0);
