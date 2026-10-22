// Loads a generated pi extension against a fake pi API and drives it.
// usage: node guard_harness.mjs <extension.mjs> <scenario.json>
// scenario: {"calls":[{"type":"tool_call","tool":"bash","input":{...}} | {"type":"event","name":"..","arg":{..}}]}
import { pathToFileURL } from "node:url";
import fs from "node:fs";

const [ext, scenarioPath] = process.argv.slice(2);
const scenario = JSON.parse(fs.readFileSync(scenarioPath, "utf8"));
const mod = await import(pathToFileURL(ext).href);
const handlers = {};
const registered = [];
const pi = {
  registerMcpServer: (name, cfg) => registered.push({ name, cfg }),
  on: (ev, fn) => (handlers[ev] = fn),
};
mod.default(pi);
const results = { registered, calls: [] };
for (const c of scenario.calls) {
  if (c.type === "tool_call") {
    const r = await handlers.tool_call({ toolName: c.tool, input: c.input }, { cwd: c.cwd || "/work" });
    results.calls.push({ result: r === undefined ? null : r });
  } else {
    const fn = handlers[c.name];
    let threw = null;
    try {
      await fn(c.arg || {}, { cwd: "/work", sessionManager: { getSessionId: () => c.sessionId || "sess-1" } });
    } catch (e) {
      threw = String(e);
    }
    results.calls.push({ threw });
  }
}
await new Promise((r) => setTimeout(r, 400));
console.log(JSON.stringify(results));
