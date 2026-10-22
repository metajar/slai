// Screenshots every page at 1280 light, 1280 dark and 360, and fails on console errors or any
// request that is not a local file. Usage (needs puppeteer-core; any Chrome):
//   NODE_PATH=/path/to/node_modules node docs/serve-prototype/tools/shoot.cjs
const puppeteer = require("puppeteer-core");
const fs = require("fs"), path = require("path");
const root = path.resolve(__dirname, "..");
const pages = fs.readFileSync(path.join(root, "pages.txt"), "utf8").trim().split("\n");
const shots = [["light-1280", 1280, 900, "light"], ["dark-1280", 1280, 900, "dark"], ["light-360", 360, 760, "light"]];
(async () => {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME || "/usr/bin/google-chrome", headless: "new", args: ["--no-sandbox", "--allow-file-access-from-files"] });
  let bad = 0;
  const jobs = pages.map((p) => [p, "", ""]).concat([["board.html", "#demo-illegal", "-illegal-move"], ["agent-new.html", "#demo-error", "-error"], ["agent-new.html", "#demo-success", "-success"], ["signin.html", "#demo-error", "-wrong-token"]]);
  for (const [file, hash, suffix] of jobs) {
    for (const [name, w, h, theme] of shots) {
      const page = await browser.newPage();
      await page.setViewport({ width: w, height: h, deviceScaleFactor: w < 600 ? 2 : 1 });
      const problems = [];
      page.on("console", (m) => { if (m.type() === "error" || m.type() === "warning") problems.push("console " + m.type() + ": " + m.text()); });
      page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
      page.on("requestfailed", (r) => problems.push("request failed: " + r.url()));
      page.on("request", (r) => { const u = r.url(); if (!u.startsWith("file://") && !u.startsWith("data:")) problems.push("external request: " + u); });
      await page.goto("file://" + path.join(root, file) + "?theme=" + theme + hash, { waitUntil: "networkidle0" });
      await new Promise((r) => setTimeout(r, hash ? 700 : 200));
      // a fixed sidebar only spans the viewport: grow the viewport to the whole page first
      const full = await page.evaluate(() => document.documentElement.scrollHeight);
      await page.setViewport({ width: w, height: Math.max(h, full), deviceScaleFactor: w < 600 ? 2 : 1 });
      await new Promise((r) => setTimeout(r, 150));
      const base = file.replace(/\.html$/, "") + suffix + "-" + name + ".png";
      await page.screenshot({ path: path.join(root, "screenshots", base), fullPage: true });
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
      if (overflow && w < 600) problems.push("horizontal page scroll at " + w + "px");
      if (problems.length) { bad++; console.log(base + "\n  " + [...new Set(problems)].join("\n  ")); }
      await page.close();
    }
  }
  await browser.close();
  console.log(bad ? bad + " screenshot(s) had problems" : "all clean: " + jobs.length * shots.length + " screenshots, no console errors, no external requests");
  process.exit(bad ? 1 : 0);
})();
