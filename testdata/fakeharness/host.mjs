// Node host for fakeharness: loads a REAL generated pi extension or opencode plugin
// against a fake host API and fires its handlers on request.
//   node host.mjs pi|opencode <module path> <cwd>
// stdin  (JSON lines): {"id":1,"op":"fire","name":"tool_call","args":[...]}
// stdout (JSON lines): {"hello":{...}} once, then {"id":1,"result":...,"error":null}
import readline from "node:readline";
import { pathToFileURL } from "node:url";

const [kind, modPath, cwd] = process.argv.slice(2);
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const handlers = {};
let mcp = null;

const mod = await import(pathToFileURL(modPath).href);
if (kind === "pi") {
  const pi = {
    registerMcpServer: (name, cfg) => { mcp = { name, ...cfg }; },
    on: (ev, fn) => { handlers[ev] = fn; },
  };
  mod.default(pi);
} else {
  const factory = Object.values(mod).find((v) => typeof v === "function");
  const hooks = await factory({ directory: cwd, worktree: cwd, project: {}, client: {}, $: () => {} });
  Object.assign(handlers, hooks);
}
out({ hello: { kind, mcp, handlers: Object.keys(handlers) } });

const rl = readline.createInterface({ input: process.stdin });
let chain = Promise.resolve();
rl.on("line", (line) => {
  chain = chain.then(async () => {
    const m = JSON.parse(line);
    if (m.op === "quit") { setTimeout(() => process.exit(0), 300); return; } // let fire-and-forget reports flush
    const fn = handlers[m.name];
    if (!fn) { out({ id: m.id, result: null, error: "no handler " + m.name }); return; }
    try {
      // give contexts that carry a sessionId the pi-style sessionManager
      const args = (m.args || []).map((a) => {
        if (a && typeof a === "object" && "sessionId" in a) a.sessionManager = { getSessionId: () => a.sessionId };
        return a;
      });
      const r = await fn(...args);
      out({ id: m.id, result: r === undefined ? null : r, error: null });
    } catch (e) {
      out({ id: m.id, result: null, error: String(e && e.message || e) });
    }
  });
});
rl.on("close", () => setTimeout(() => process.exit(0), 300));
