// Drives the kanban with real mouse events (SortableJS fallback mode, ?fallback=1) and checks
// that illegal drops snap back with a refusal toast and the allowed ones stick.
// Usage: NODE_PATH=/path/to/node_modules node docs/serve-prototype/tools/check-board.cjs
const puppeteer = require("puppeteer-core"), path = require("path");
const url = "file://" + path.resolve(__dirname, "..", "board.html") + "?theme=light&fallback=1";
let fails = 0;
const ok = (c, m) => { console.log((c ? "ok   " : "FAIL ") + m); if (!c) fails++; };
(async () => {
  const b = await puppeteer.launch({ executablePath: "/usr/bin/google-chrome", headless: "new", args: ["--no-sandbox"] });
  const p = await b.newPage();
  await p.setViewport({ width: 1280, height: 1600 });
  await p.goto(url, { waitUntil: "networkidle0" });
  const colOf = (id) => p.evaluate((i) => document.querySelector('.tcard[data-id="' + i + '"]').closest(".kcol").dataset.status, id);
  const toasts = () => p.evaluate(() => [...document.querySelectorAll("#toasts .toast")].map((t) => t.textContent));
  async function drag(id, toStatus) {
    const from = await (await p.$('.tcard[data-id="' + id + '"] .tid')).boundingBox();
    const to = await (await p.$('.kcol[data-status="' + toStatus + '"] .kcol-head')).boundingBox();
    await p.mouse.move(from.x + 5, from.y + 5); await p.mouse.down();
    await p.mouse.move(from.x + 20, from.y + 20, { steps: 4 });
    await p.mouse.move(to.x + 40, to.y + to.height + 30, { steps: 12 });
    await p.mouse.move(to.x + 41, to.y + to.height + 32, { steps: 3 });
    await p.mouse.up(); await new Promise((r) => setTimeout(r, 400));
  }
  await drag("T-0079", "merged");
  ok((await colOf("T-0079")) === "in_review", "in_review -> merged snaps back");
  ok((await toasts()).some((t) => /can't be moved to merged/.test(t)), "refusal toast names the reason");
  await drag("T-0075", "in_progress");
  ok((await colOf("T-0075")) === "in_review", "in_review -> in_progress snaps back");
  ok((await toasts()).some((t) => /illegal transition in_review → in_progress/.test(t)), "toast for in_review -> in_progress");
  await drag("T-0086", "assigned");
  ok((await colOf("T-0086")) === "merged", "merged is final: stays merged");
  await drag("T-0081", "cancelled");
  ok((await colOf("T-0081")) === "cancelled", "assigned -> cancelled is accepted");
  ok((await toasts()).some((t) => /T-0081 cancelled/.test(t)), "cancel toast");
  await drag("T-0082", "in_progress");
  ok((await colOf("T-0082")) === "in_progress", "blocked -> in_progress (unblock) is accepted");
  const n = await p.evaluate(() => document.querySelector('.kcol[data-status="merged"] .count').textContent);
  ok(n === "7", "column counters stay right (" + n + ")");
  // read-only variant: nothing writes
  const r = await b.newPage();
  await r.setViewport({ width: 1280, height: 1600 });
  await r.goto(url.replace("board.html", "board-readonly.html"), { waitUntil: "networkidle0" });
  const off = await r.evaluate(() => ({ btn: document.querySelector("[data-bs-target='#newtask']").disabled, banner: /Read-only mode/.test(document.body.textContent) }));
  ok(off.btn && off.banner, "read-only board: banner shown, New task disabled");
  const from = await (await r.$('.tcard[data-id="T-0081"] .tid')).boundingBox();
  const to = await (await r.$('.kcol[data-status="cancelled"] .kcol-head')).boundingBox();
  await r.mouse.move(from.x + 5, from.y + 5); await r.mouse.down(); await r.mouse.move(to.x + 40, to.y + to.height + 30, { steps: 12 }); await r.mouse.up();
  await new Promise((x) => setTimeout(x, 400));
  ok((await r.evaluate(() => document.querySelector('.tcard[data-id="T-0081"]').closest(".kcol").dataset.status)) === "assigned", "read-only board: drag does nothing");
  await b.close();
  process.exit(fails ? 1 : 0);
})();
