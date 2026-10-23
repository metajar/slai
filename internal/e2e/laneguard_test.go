package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/harness"
)

// AC 10: the OpenCode plugin (run by node, standing in for opencode) enforces the lane
// through the real `slai hook`: a write outside the lane is blocked and reaches the
// tool result as an error, the denial is a lane_denied event, and a guard that cannot
// run blocks too (fail closed).
func TestOpenCodePluginLaneGuardEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	p := newProject(t, opts{})
	p.up()
	h, _ := harness.For(harness.OpenCode)
	ls := harness.LaunchSpec{AgentID: "eng-1", Model: "zhipuai-coding-plan/glm-5.3", PermissionMode: "auto", ProjectDir: p.dir,
		WorkDir: filepath.Join(p.dir, ".slai", "worktrees", "eng-1"), SlaiBin: filepath.Join(binDir, "slai"), SystemPrompt: "You are **eng-1**."}
	if err := h.WriteRunFiles(ls); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(p.dir, ".slai", "run", "eng-1")
	if err := os.Rename(filepath.Join(run, "plugin.js"), filepath.Join(run, "plugin.mjs")); err != nil {
		t.Fatal(err)
	}
	// the same plugin pointed at a slai that cannot run
	broken := filepath.Join(t.TempDir(), "slai")
	_ = os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	ls.SlaiBin = broken
	if err := h.WriteRunFiles(ls); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(run, "plugin.js"), filepath.Join(run, "broken.mjs")); err != nil {
		t.Fatal(err)
	}
	wt := ls.WorkDir
	driver := `
import { SlaiPlugin as good } from "./plugin.mjs"
import { SlaiPlugin as broken } from "./broken.mjs"
const call = async (plugin, tool, args) => {
  try { await plugin["tool.execute.before"]({ tool, sessionID: "s" }, { args }); return "allowed" }
  catch (e) { return "blocked: " + e.message }
}
const g = await good(), b = await broken()
const out = {
  inside: await call(g, "write", { filePath: process.argv[2] + "/a.txt", content: "x" }),
  outside: await call(g, "write", { filePath: process.argv[3], content: "x" }),
  bash: await call(g, "bash", { command: "echo x > " + process.argv[3] }),
  read: await call(g, "read", { filePath: "/etc/hostname" }),
  broken: await call(b, "write", { filePath: process.argv[2] + "/a.txt", content: "x" }),
}
console.log(JSON.stringify(out))
`
	if err := os.WriteFile(filepath.Join(run, "driver.mjs"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(p.dir, "outside-the-lane.txt")
	cmd := exec.Command(node, "driver.mjs", wt, outside)
	cmd.Dir, cmd.Env = run, p.env
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var res map[string]string
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("driver output %q: %v", b, err)
	}
	if res["inside"] != "allowed" || res["read"] != "allowed" {
		t.Errorf("in-lane calls: %v", res)
	}
	for _, k := range []string{"outside", "bash"} {
		if !strings.HasPrefix(res[k], "blocked: slai lane guard") {
			t.Errorf("%s = %q, want the lane guard's reason as the tool error", k, res[k])
		}
	}
	if !strings.HasPrefix(res["broken"], "blocked:") {
		t.Errorf("a guard that cannot run must block the call: %q", res["broken"])
	}
	denied := p.events("lane_denied", "eng-1")
	if len(denied) < 2 {
		t.Fatalf("lane_denied events = %d, want one per denied call", len(denied))
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(denied[0].Data), &d)
	if d["tool"] != "Write" || d["path"] != outside {
		t.Errorf("lane_denied data = %v", d)
	}
	if ready := p.events("hook:guard_ready", "eng-1"); len(ready) != 1 {
		t.Errorf("guard_ready events = %d, want 1", len(ready))
	}
}
