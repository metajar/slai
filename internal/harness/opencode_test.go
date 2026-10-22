package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/hooks"
)

func ocSpec(t *testing.T) LaunchSpec {
	t.Helper()
	d := t.TempDir()
	return LaunchSpec{AgentID: "oc-1", Model: "zhipuai-coding-plan/glm-5.3", PermissionMode: "auto", ProjectDir: d,
		WorkDir: filepath.Join(d, ".slai", "worktrees", "oc-1"), SlaiBin: "/usr/local/bin/slai", SystemPrompt: "You are **oc-1**, role: Engineer.\n\nLine two.",
		Deny:  []string{"Bash(git push:*)", "Bash(git reset --hard:*)", "EnterWorktree"},
		Allow: []string{"Bash(go test:*)", "Edit(//wt/**)"}}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// AC 4.
func TestOpenCodeRunFiles(t *testing.T) {
	h, _ := For(OpenCode)
	ls := ocSpec(t)
	if err := h.WriteRunFiles(ls); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(ls.ProjectDir, ".slai", "run", "oc-1")
	cfg := readJSON(t, filepath.Join(run, "opencode.json"))

	slai := cfg["mcp"].(map[string]any)["slai"].(map[string]any)
	wantEnv := map[string]any{"SLAI_AGENT_ID": "oc-1", "SLAI_PROJECT": ls.ProjectDir}
	if !reflect.DeepEqual(slai["environment"], wantEnv) || slai["type"] != "local" {
		t.Errorf("mcp.slai = %v", slai)
	}
	if got := slai["command"].([]any); len(got) != 6 || got[0] != "/usr/local/bin/slai" || got[2] != "--agent" || got[3] != "oc-1" || got[4] != "--project" || got[5] != ls.ProjectDir {
		t.Errorf("mcp command = %v", got)
	}
	agent := cfg["agent"].(map[string]any)["slai-oc-1"].(map[string]any)
	if agent["prompt"] != ls.SystemPrompt || cfg["default_agent"] != "slai-oc-1" {
		t.Errorf("agent prompt/default = %v / %v", agent["prompt"], cfg["default_agent"])
	}
	if cfg["model"] != ls.Model {
		t.Errorf("model = %v", cfg["model"])
	}
	if plug := cfg["plugin"].([]any); len(plug) != 1 || plug[0] != filepath.Join(run, "plugin.js") {
		t.Errorf("plugin = %v (must be an absolute path to the generated plugin)", plug)
	}
	perm := cfg["permission"].(map[string]any)
	if perm["external_directory"] != "allow" || perm["edit"] != "allow" {
		t.Errorf("permission = %v", perm)
	}
	if b, _ := os.ReadFile(filepath.Join(run, "system.md")); string(b) != ls.SystemPrompt {
		t.Errorf("system.md = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(run, "xdg")); err != nil || !fi.IsDir() {
		t.Error("isolated XDG_CONFIG_HOME dir missing")
	}
	// idempotent
	before, _ := os.ReadFile(filepath.Join(run, "opencode.json"))
	if err := h.WriteRunFiles(ls); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(run, "opencode.json")); string(after) != string(before) {
		t.Error("WriteRunFiles is not idempotent")
	}
}

func TestOpenCodePermissionMapping(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		allow, deny []string
		want        map[string]any
	}{
		{"auto allows everything but the denied bash prefixes", "auto", []string{"Bash(go test:*)"}, []string{"Bash(git push:*)", "EnterWorktree"},
			map[string]any{"edit": "allow", "bash": map[string]string{"*": "allow", "go test *": "allow", "git push *": "deny"}}},
		{"read-only roles deny the edit tools outright", "auto", nil, []string{"Edit", "Write", "MultiEdit", "NotebookEdit"},
			map[string]any{"edit": "deny", "bash": "allow"}},
		{"scoped Edit allows are left to the lane guard", "auto", []string{"Edit(.slai/specs/**)"}, nil,
			map[string]any{"edit": "allow", "bash": "allow"}},
		{"manual asks, as the user chose", "manual", []string{"Bash(go test:*)"}, nil,
			map[string]any{"edit": "ask", "bash": map[string]string{"*": "ask", "go test *": "allow"}}},
		{"plan is read only", "plan", nil, nil, map[string]any{"edit": "deny", "bash": "deny"}},
		{"bypassPermissions", "bypassPermissions", nil, nil, map[string]any{"edit": "allow", "bash": "allow"}},
		{"a bare Bash deny wins over allowed prefixes", "auto", []string{"Bash(go test:*)"}, []string{"Bash"}, map[string]any{"bash": "deny"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := openCodePermission(c.mode, c.allow, c.deny)
			for k, v := range c.want {
				if !reflect.DeepEqual(got[k], v) {
					t.Errorf("permission[%s] = %#v, want %#v", k, got[k], v)
				}
			}
			// whatever the mode, the dialogs slai cannot answer are switched off
			if got["external_directory"] != "allow" || got["doom_loop"] != "allow" {
				t.Errorf("external_directory/doom_loop = %v / %v", got["external_directory"], got["doom_loop"])
			}
		})
	}
}

// AC 5: string-level.
func TestOpenCodeCommand(t *testing.T) {
	h, _ := For(OpenCode)
	ls := ocSpec(t)
	run := filepath.Join(ls.ProjectDir, ".slai", "run", "oc-1")
	cmd := h.Command(ls)
	for _, want := range []string{
		"SLAI_AGENT_ID='oc-1'", "SLAI_PROJECT='" + ls.ProjectDir + "'",
		"XDG_CONFIG_HOME='" + filepath.Join(run, "xdg") + "'",
		"OPENCODE_CONFIG='" + filepath.Join(run, "opencode.json") + "'",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"opencode -m 'zhipuai-coding-plan/glm-5.3' --agent 'slai-oc-1'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
	for _, never := range []string{"--pure", " -c ", "--continue", " -s ", "--session"} {
		if strings.Contains(cmd, never) {
			t.Errorf("a fresh launch must not contain %q: %s", never, cmd)
		}
	}
	if h.Command(ls) != cmd {
		t.Error("Command is not byte-stable")
	}
	// revive with a captured id: resume it, fall back to a fresh session
	ls.SessionID, ls.Resume = "ses_abc123", true
	rev := h.Command(ls)
	parts := strings.Split(rev, " || ")
	if len(parts) != 2 || !strings.HasSuffix(parts[0], "-s 'ses_abc123'") || strings.Contains(parts[1], " -s ") {
		t.Errorf("revive:\n%s", rev)
	}
	for _, never := range []string{"--pure", " -c ", "--continue"} {
		if strings.Contains(rev, never) {
			t.Errorf("revive must not contain %q", never)
		}
	}
	// revive without a captured id starts fresh (the wake line says it was restarted)
	ls.SessionID = ""
	if got := h.Command(ls); strings.Contains(got, " -s ") || strings.Contains(got, " || ") {
		t.Errorf("no captured id: %s", got)
	}
	if h.NewSessionID() != "" {
		t.Error("opencode assigns its own session ids")
	}
}

func TestOpenCodeKeysAndFeatures(t *testing.T) {
	h, _ := For(OpenCode)
	k := h.Keys()
	if k.EnterDelay != 0 || len(k.ClearSeq) != 1 || k.ClearSeq[0].S != "C-u" {
		t.Errorf("keys = %+v: Escape must never be part of the clear sequence", k)
	}
	if ok, _ := h.Trusted(ocSpec(t)); !ok {
		t.Error("opencode has no trust gate")
	}
}

// AC 12 (opencode half): fixtures captured from the real TUI.
func TestOpenCodePaneFixtures(t *testing.T) {
	h, _ := For(OpenCode)
	rules := h.PaneRules()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "hooks", "testdata", "panes", "opencode", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		file   string
		status string
		modal  []string // substrings the excerpt must contain; nil = no modal
	}{
		{"idle-home.txt", hooks.StatusIdle, nil},
		{"idle-after-turn.txt", hooks.StatusIdle, nil},
		{"working.txt", hooks.StatusWorking, nil},
		{"permission-dialog.txt", hooks.StatusWaiting, []string{"permission prompt", "Shell command", "echo spike-ok"}},
		{"permission-dialog-external-dir.txt", hooks.StatusWaiting, []string{"permission prompt", "external directory /etc"}},
		{"provider-error-retrying.txt", hooks.StatusRateLimited, nil},
		// negative: the dialog's words quoted in the conversation, input box drawn below
		{"quoted-dialog-in-scrollback.txt", hooks.StatusIdle, nil},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			pane := read(c.file)
			if got, ok := hooks.DetectWith(rules, pane); !ok || got != c.status {
				t.Errorf("status = %q (%v), want %q", got, ok, c.status)
			}
			ex := hooks.ModalExcerptWith(rules, pane)
			if c.modal == nil && ex != "" {
				t.Errorf("modal excerpt %q for a pane with no live prompt", ex)
			}
			for _, w := range c.modal {
				if !strings.Contains(ex, w) {
					t.Errorf("excerpt %q lacks %q", ex, w)
				}
			}
			for _, bad := range []string{"Permission required", "Allow once", "Reject", "enter confirm"} {
				if strings.Contains(ex, bad) {
					t.Errorf("excerpt %q repeats the prompt (%q): it could look like a prompt itself", ex, bad)
				}
			}
		})
	}
	// Claude's rules must not read an OpenCode dialog, and the reverse
	if _, ok := hooks.DetectFromPane(read("permission-dialog.txt")); ok {
		t.Error("the Claude rules matched an OpenCode pane")
	}
}

// AC 9: the plugin is generated from an embedded template and must parse.
func TestOpenCodePluginSyntaxAndBakedIdentity(t *testing.T) {
	ls := ocSpec(t)
	ls.ProjectDir = filepath.Join(ls.ProjectDir, `odd "dir"`, "o'brien\\x")
	src, err := renderOpenCodePlugin(ls, ls.SlaiBin)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{`const AGENT = "oc-1"`, `const SLAI = "/usr/local/bin/slai"`, `const PROJECT = ` + jsString(ls.ProjectDir)} {
		if !strings.Contains(s, want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	for _, bad := range []string{"process.env.SLAI", "{{", "}}"} {
		if strings.Contains(s, bad) {
			t.Errorf("plugin contains %q", bad)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: plugin syntax not checked")
	}
	p := filepath.Join(t.TempDir(), "plugin.mjs")
	_ = os.WriteFile(p, src, 0o644)
	if out, err := exec.Command(node, "--check", p).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

func jsString(v string) string { b, _ := json.Marshal(v); return string(b) }

// The generated plugin, run under node against a fake `slai`, behaves as the spec
// requires: statuses and usage reach `slai hook`, and the lane guard fails closed.
func TestOpenCodePluginBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "hook.log")
	reply := filepath.Join(dir, "reply")
	fake := filepath.Join(dir, "slai")
	script := "#!/bin/sh\n" +
		"[ -f " + shq(filepath.Join(dir, "down")) + " ] && exit 3\n" +
		"printf '%s\\t' \"$*\" >> " + shq(log) + "\ncat >> " + shq(log) + "\necho >> " + shq(log) + "\n" +
		"[ -f " + shq(reply) + " ] && cat " + shq(reply) + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ls := ocSpec(t)
	ls.SlaiBin = fake
	src, err := renderOpenCodePlugin(ls, fake)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "plugin.mjs"), src, 0o644)
	driver := `
import { SlaiPlugin } from "./plugin.mjs"
import { existsSync, writeFileSync, rmSync } from "node:fs"
const out = {}
const p = await SlaiPlugin()
const tryTool = async (args) => { try { await p["tool.execute.before"]({ tool: "write", sessionID: "s1" }, { args }); return "allowed" } catch (e) { return "blocked: " + e.message } }

await p.event({ event: { type: "session.created", properties: { sessionID: "ses_42", info: { id: "ses_42", directory: "/wt" } } } })
await p["chat.message"]()
out.silent = await tryTool({ filePath: "/wt/a.txt" })
writeFileSync(process.argv[2], JSON.stringify({ decision: "allow" }))
out.allowed = await tryTool({ filePath: "/wt/a.txt" })
writeFileSync(process.argv[2], JSON.stringify({ decision: "deny", reason: "outside your lane" }))
out.denied = await tryTool({ filePath: "/etc/passwd" })
rmSync(process.argv[2])
writeFileSync(process.argv[3], "")
out.unreachable = await tryTool({ filePath: "/wt/a.txt" })
rmSync(process.argv[3])
await p["tool.execute.after"]({ tool: "write", sessionID: "s1" })
await p.event({ event: { type: "message.updated", properties: { info: { id: "m1", role: "assistant", cost: 0.01, tokens: { input: 100, output: 10, reasoning: 5, cache: { read: 50, write: 0 } } } } } })
await p.event({ event: { type: "message.updated", properties: { info: { id: "m1", role: "assistant", cost: 0.03, tokens: { input: 300, output: 20, reasoning: 5, cache: { read: 50, write: 0 } } } } } })
await p.event({ event: { type: "permission.asked", properties: {} } })
await p.event({ event: { type: "session.status", properties: { status: { type: "idle" } } } })
await p.event({ event: { type: "session.idle", properties: {} } }) // a second idle must not send a second stop
console.log(JSON.stringify(out))
`
	_ = os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o644)
	cmd := exec.Command(node, "driver.mjs", reply, filepath.Join(dir, "down"))
	cmd.Dir = dir
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outb)
	}
	var res map[string]string
	if err := json.Unmarshal(outb, &res); err != nil {
		t.Fatalf("driver output %q: %v", outb, err)
	}
	if res["allowed"] != "allowed" {
		t.Errorf("explicit allow: %s", res["allowed"])
	}
	if !strings.HasPrefix(res["silent"], "blocked:") {
		t.Errorf("silence from the guard must block (only an explicit allow lets a call through), got %s", res["silent"])
	}
	if !strings.Contains(res["denied"], "blocked: outside your lane") {
		t.Errorf("guard denial: %s", res["denied"])
	}
	if !strings.HasPrefix(res["unreachable"], "blocked:") {
		t.Errorf("an unreachable guard must block the tool call, got %s", res["unreachable"])
	}
	lines := readLines(t, log)
	count := func(prefix string) (n int) {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return
	}
	pin := " --agent oc-1 --project " + ls.ProjectDir
	for _, ev := range []string{"guard_ready", "session_start", "prompt", "tool", "tool_end", "waiting", "stop"} {
		if count("hook "+ev+pin+"\t") == 0 {
			t.Errorf("no `slai hook %s` with the baked-in identity; log:\n%s", ev, strings.Join(lines, "\n"))
		}
	}
	if n := count("hook guard_ready"); n != 1 {
		t.Errorf("guard_ready sent %d times, want once", n)
	}
	if n := count("hook stop"); n != 1 {
		t.Errorf("stop sent %d times for one busy period, want 1", n)
	}
	var sess, stop string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "hook session_start"):
			sess = l
		case strings.HasPrefix(l, "hook stop"):
			stop = l
		}
	}
	if !strings.Contains(sess, `"session_id":"ses_42"`) {
		t.Errorf("session id not reported: %s", sess)
	}
	// usage is the delta since the previous report: (300+50+0)+... = prompt 350, output 25, cost 0.03
	if !strings.Contains(stop, `"prompt":350`) || !strings.Contains(stop, `"output":25`) || !strings.Contains(stop, `"cost":0.03`) {
		t.Errorf("stop usage: %s", stop)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
