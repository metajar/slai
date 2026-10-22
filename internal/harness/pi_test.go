package harness

import (
	"encoding/json"
	"github.com/oorbt/slai/internal/hooks"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var piH = piHarness{}

func spec(dir string) LaunchSpec {
	return LaunchSpec{AgentID: "eng-1", ProjectDir: dir, WorkDir: filepath.Join(dir, ".slai", "worktrees", "eng-1"),
		SlaiBin: "/usr/local/bin/slai", SystemPrompt: "You are eng-1.\nSecond 'line'.", Model: "zai/glm-5.3",
		SessionID: "11111111-2222-4333-8444-555555555555"}
}

func TestCommandPinsIdentityAndFlags(t *testing.T) {
	t.Setenv("SLAI_AGENT_ID", "intruder")
	d := "/proj"
	s := spec(d)
	cmd := piH.Command(s)
	for _, want := range []string{
		"env SLAI_AGENT_ID='eng-1' SLAI_PROJECT='/proj' pi ",
		"--model 'zai/glm-5.3'",
		`--append-system-prompt "$(cat '/proj/.slai/run/eng-1/system.md')"`,
		"--no-extensions -e builtin:mcp -e '/proj/.slai/run/eng-1/pi-extension.mjs'",
		"--no-approve",
		"--session-id '11111111-2222-4333-8444-555555555555'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "intruder") {
		t.Error("ambient identity leaked into the command")
	}
	// never a flag that would cross agents or answer trust
	for _, bad := range []string{"--continue", " -c ", "--resume", " -r ", "--approve", " -a "} {
		if strings.Contains(cmd+" ", bad) {
			t.Errorf("command must not contain %q: %s", bad, cmd)
		}
	}
	if cmd != piH.Command(s) {
		t.Error("Command() must be byte-stable")
	}
	s.SessionID = ""
	if strings.Contains(piH.Command(s), "--session-id") {
		t.Error("no session id, no flag")
	}
	// the same flag starts and resumes: there is no separate resume form
	s.SessionID = "abc"
	if !strings.Contains(piH.Command(s), "--session-id 'abc'") {
		t.Error("resume uses --session-id")
	}
}

func TestCommandQuotesHostileValues(t *testing.T) {
	s := spec("/p r'oj")
	s.AgentID = "eng-1"
	s.Model = "zai/glm'; rm -rf /; '"
	cmd := piH.Command(s)
	if !strings.Contains(cmd, `--model 'zai/glm'\''; rm -rf /; '\'''`) || !strings.Contains(cmd, `SLAI_PROJECT='/p r'\''oj'`) {
		t.Errorf("not shell-quoted: %s", cmd)
	}
}

func TestWriteRunFilesIsIdempotentAndPinsIdentity(t *testing.T) {
	t.Setenv("SLAI_AGENT_ID", "intruder")
	t.Setenv("SLAI_PROJECT", "/somewhere/else")
	d := t.TempDir()
	s := spec(d)
	if err := piH.WriteRunFiles(s); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(PiExtensionPath(d, "eng-1"))
	sys, _ := os.ReadFile(filepath.Join(piRunDir(d, "eng-1"), "system.md"))
	if string(sys) != s.SystemPrompt {
		t.Errorf("system.md = %q", sys)
	}
	if err := piH.WriteRunFiles(s); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(PiExtensionPath(d, "eng-1"))
	if string(first) != string(second) {
		t.Error("WriteRunFiles must be idempotent")
	}
	src := string(first)
	jd, _ := json.Marshal(d)
	for _, want := range []string{
		`const AGENT = "eng-1";`,
		"const PROJECT = " + string(jd) + ";",
		`const SLAI = "/usr/local/bin/slai";`,
		`args: ["mcp", "--agent", AGENT, "--project", PROJECT]`,
		"env: PINNED_ENV",
		"const GUARD_TIMEOUT_MS = 6000;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("extension lacks %q", want)
		}
	}
	if strings.Contains(src, "intruder") || strings.Contains(src, "/somewhere/else") {
		t.Error("ambient identity leaked into the extension")
	}
	if strings.Contains(src, "process.env.SLAI_AGENT_ID") || strings.Contains(src, "process.env.SLAI_PROJECT") {
		t.Error("the extension must not read identity from the ambient environment")
	}
}

func TestRenderEscapesHostileStrings(t *testing.T) {
	s := spec(t.TempDir())
	s.AgentID = `eng-1"; process.exit(1); "`
	src, err := renderPiExtension(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, `"; process.exit(1); "`) && !strings.Contains(src, `\"; process.exit(1); \"`) {
		t.Error("agent id is not escaped as a JSON string")
	}
}

func needNode(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: extension syntax and guard behaviour tests skipped (AC 9)")
	}
	return p
}

// AC 9: the generated extension is syntax-checked.
func TestExtensionIsSyntacticallyValid(t *testing.T) {
	node := needNode(t)
	d := t.TempDir()
	if err := piH.WriteRunFiles(spec(d)); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", PiExtensionPath(d, "eng-1")).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

type harnessResult struct {
	Registered []struct {
		Name string
		Cfg  struct {
			Command  string
			Args     []string
			Env      map[string]string
			Exposure string
		}
	}
	Calls []struct {
		Result *struct {
			Block  bool
			Reason string
		}
		Threw *string
	}
}

// runExt renders the extension against a stub slai script and drives it with a
// scenario in node. The stub appends "argv|stdin|SLAI_AGENT_ID" lines to log.
func runExt(t *testing.T, stubBody string, mutate func(*LaunchSpec), calls []map[string]any) (res harnessResult, log []string, dir string) {
	t.Helper()
	node := needNode(t)
	dir = t.TempDir()
	stub := filepath.Join(dir, "slai-stub.sh")
	script := "#!/bin/bash\nin=$(cat)\necho \"$*|$in|$SLAI_AGENT_ID|$SLAI_PROJECT\" >> '" + filepath.Join(dir, "stub.log") + "'\n" + stubBody + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := piGuardTimeoutMS
	piGuardTimeoutMS = 700
	t.Cleanup(func() { piGuardTimeoutMS = old })
	s := spec(dir)
	s.SlaiBin = stub
	if mutate != nil {
		mutate(&s)
	}
	if err := piH.WriteRunFiles(s); err != nil {
		t.Fatal(err)
	}
	if calls == nil {
		calls = []map[string]any{}
	}
	sc, _ := json.Marshal(map[string]any{"calls": calls})
	scPath := filepath.Join(dir, "scenario.json")
	_ = os.WriteFile(scPath, sc, 0o644)
	cmd := exec.Command(node, "testdata/guard_harness.mjs", PiExtensionPath(dir, "eng-1"), scPath)
	cmd.Env = append(os.Environ(), "SLAI_AGENT_ID=intruder", "SLAI_PROJECT=/somewhere/else")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, out)
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("bad harness output %q: %v", out, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "stub.log"))
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			log = append(log, l)
		}
	}
	return
}

func bashCall(cmd string) map[string]any {
	return map[string]any{"type": "tool_call", "tool": "bash", "input": map[string]any{"command": cmd}}
}

// AC 10 (pi half): the guard fails closed on every way the hook can go wrong, and
// only an explicit allow lets the call through.
func TestGuardFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		stub    string
		mutate  func(*LaunchSpec)
		allow   bool
		reason  string // substring of the block reason
		elapsed time.Duration
	}{
		{"silence is not consent", "exit 0", nil, false, "no explicit allow", 0},
		{"explicit allow", `echo '{"decision":"allow","reason":""}'`, nil, true, "", 0},
		{"deny carries the reason", `echo '{"decision":"deny","reason":"slai lane guard: eng-1 may not write /etc"}'`, nil, false, "may not write /etc", 0},
		{"nonzero exit", "exit 3", nil, false, "exited 3", 0},
		{"killed by a signal", "kill -9 $$", nil, false, "killed by SIGKILL", 0},
		{"unreadable answer", "echo '<<not json>>'", nil, false, "unreadable answer", 0},
		{"unknown decision", `echo '{"decision":"maybe"}'`, nil, false, "unexpected answer", 0},
		{"timeout", "sleep 20", nil, false, "ETIMEDOUT", 0},
		{"slai binary missing", "exit 0", func(s *LaunchSpec) { s.SlaiBin = "/nonexistent/slai" }, false, "ENOENT", 0},
	}
	for _, c := range cases {
		res, _, _ := runExt(t, c.stub, c.mutate, []map[string]any{bashCall("echo hi")})
		if len(res.Calls) != 1 {
			t.Fatalf("%s: calls = %d", c.name, len(res.Calls))
		}
		r := res.Calls[0].Result
		switch {
		case c.allow && r != nil:
			t.Errorf("%s: blocked: %+v", c.name, r)
		case !c.allow && (r == nil || !r.Block):
			t.Errorf("%s: NOT blocked (fail open!): %+v", c.name, r)
		case !c.allow && !strings.Contains(r.Reason, c.reason):
			t.Errorf("%s: reason %q lacks %q", c.name, r.Reason, c.reason)
		}
	}
}

// The guard speaks slai's neutral protocol with the pinned identity, mapping pi's
// tool names onto the ones the lane guard knows.
func TestGuardCallShape(t *testing.T) {
	res, log, dir := runExt(t, "exit 0", nil, []map[string]any{
		bashCall("rm -rf /"),
		{"type": "tool_call", "tool": "write", "input": map[string]any{"path": "/etc/passwd", "content": "x"}, "cwd": "/work/eng-1"},
		{"type": "tool_call", "tool": "mcp__slai__get_inbox", "input": map[string]any{}},
	})
	if len(res.Calls) != 3 || len(log) != 3 {
		t.Fatalf("calls=%d log=%v", len(res.Calls), log)
	}
	type want struct{ tool, key, val string }
	for i, w := range []want{{"Bash", "command", "rm -rf /"}, {"Write", "file_path", "/etc/passwd"}, {"mcp__slai__get_inbox", "", ""}} {
		parts := strings.SplitN(log[i], "|", 4)
		if len(parts) != 4 {
			t.Fatalf("log line %q", log[i])
		}
		if parts[0] != "hook tool --agent eng-1 --project "+dir {
			t.Errorf("argv = %q", parts[0])
		}
		var p struct {
			Event     string
			Tool      string
			ToolInput map[string]any `json:"tool_input"`
			Cwd       string
		}
		if err := json.Unmarshal([]byte(parts[1]), &p); err != nil {
			t.Fatalf("payload %q: %v", parts[1], err)
		}
		if p.Event != "tool" || p.Tool != w.tool || (w.key != "" && p.ToolInput[w.key] != w.val) {
			t.Errorf("payload = %+v, want tool %s %s=%s", p, w.tool, w.key, w.val)
		}
		// the hook child sees the PINNED identity although node ran with an intruder in its env
		if parts[2] != "eng-1" || parts[3] != dir {
			t.Errorf("child env identity = %q %q", parts[2], parts[3])
		}
	}
	if !strings.Contains(log[1], `"cwd":"/work/eng-1"`) {
		t.Errorf("cwd not passed: %s", log[1])
	}
}

func TestExtensionRegistersPinnedMCPServer(t *testing.T) {
	res, _, dir := runExt(t, "exit 0", nil, nil)
	if len(res.Registered) != 1 || res.Registered[0].Name != "slai" {
		t.Fatalf("registered = %+v", res.Registered)
	}
	c := res.Registered[0].Cfg
	if strings.Join(c.Args, " ") != "mcp --agent eng-1 --project "+dir || c.Env["SLAI_AGENT_ID"] != "eng-1" || c.Env["SLAI_PROJECT"] != dir || c.Exposure != "direct" {
		t.Errorf("mcp config = %+v", c)
	}
}

// Decision 8c: guard_ready exists only when the hook answered at session start.
func TestGuardReadyOnlyWhenTheHookIsReachable(t *testing.T) {
	start := []map[string]any{{"type": "event", "name": "session_start", "sessionId": "abc"}}
	_, log, dir := runExt(t, "exit 0", nil, start)
	if _, err := os.Stat(PiGuardReadyPath(dir, "eng-1")); err != nil {
		t.Errorf("reachable hook: guard_ready missing: %v", err)
	}
	if PiGuardReadyPending(dir, "eng-1") != "" {
		t.Error("pending must be empty once ready")
	}
	// the session_start report doubles as the probe; the same fact is then reported as a guard_ready event
	if len(log) != 2 || !strings.HasPrefix(log[0], "hook session_start --agent eng-1") || !strings.Contains(log[0], `"session_id":"abc"`) ||
		!strings.HasPrefix(log[1], "hook guard_ready --agent eng-1") {
		t.Errorf("session_start report = %v", log)
	}

	_, _, dir = runExt(t, "exit 1", nil, start)
	if _, err := os.Stat(PiGuardReadyPath(dir, "eng-1")); err == nil {
		t.Error("an unreachable hook must not produce guard_ready")
	}
	if !strings.Contains(PiGuardReadyPending(dir, "eng-1"), "guard_ready") {
		t.Errorf("pending text: %q", PiGuardReadyPending(dir, "eng-1"))
	}

	// a stale marker from the previous run is removed at load
	node := needNode(t)
	d := t.TempDir()
	if err := piH.WriteRunFiles(spec(d)); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(PiGuardReadyPath(d, "eng-1"), []byte("old"), 0o644)
	sc := filepath.Join(d, "sc.json")
	_ = os.WriteFile(sc, []byte(`{"calls":[]}`), 0o644)
	if out, err := exec.Command(node, "testdata/guard_harness.mjs", PiExtensionPath(d, "eng-1"), sc).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(PiGuardReadyPath(d, "eng-1")); err == nil {
		t.Error("stale guard_ready survived extension load")
	}
}

// AC 8 (pi row) + AC 15: lifecycle events become neutral reports and spend is
// reported as one delta with the stop event.
func TestLifecycleReportsAndUsageDelta(t *testing.T) {
	msg := func(in, cr, out int, cost float64) map[string]any {
		return map[string]any{"type": "event", "name": "message_end", "arg": map[string]any{"message": map[string]any{
			"role": "assistant", "usage": map[string]any{"input": in, "cacheRead": cr, "output": out, "cost": map[string]any{"total": cost}}}}}
	}
	_, log, _ := runExt(t, "exit 0", nil, []map[string]any{
		{"type": "event", "name": "before_agent_start"},
		{"type": "tool_call", "tool": "bash", "input": map[string]any{"command": "ls"}},
		{"type": "event", "name": "tool_result"},
		msg(100, 50, 10, 0.001),
		{"type": "event", "name": "message_end", "arg": map[string]any{"message": map[string]any{"role": "user"}}},
		msg(100, 50, 10, 0.001),
		{"type": "event", "name": "agent_settled"},
	})
	byEvent := map[string][]string{}
	for _, l := range log {
		f := strings.Fields(strings.SplitN(l, "|", 2)[0])
		byEvent[f[1]] = append(byEvent[f[1]], l)
	}
	for _, ev := range []string{"prompt", "tool", "tool_end", "stop"} {
		if len(byEvent[ev]) != 1 {
			t.Errorf("event %s reported %d times: %v", ev, len(byEvent[ev]), log)
		}
	}
	re := regexp.MustCompile(`"usage":\{"prompt":300,"output":20,"cost":0\.002\}`)
	if len(byEvent["stop"]) == 1 && !re.MatchString(byEvent["stop"][0]) {
		t.Errorf("stop lacks the usage delta: %s", byEvent["stop"][0])
	}
}

// The pane rules recognise the captured pi fixtures (AC 12) and nothing is
// mistaken for a modal in the quoted-dialog negative case.
func TestPaneRulesOnFixtures(t *testing.T) {
	dir := filepath.Join("..", "hooks", "testdata", "panes", "pi")
	read := func(n string) string {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	r := piH.PaneRules()
	for _, n := range []string{"working.txt"} {
		if !r.Working.MatchString(read(n)) {
			t.Errorf("%s: Working rule missed", n)
		}
	}
	for _, n := range []string{"idle-after-turn.txt", "idle-startup-extkeys-on.txt", "idle-startup-extkeys-off.txt", "quoted-dialog-in-scrollback.txt", "provider-error-badkey.txt"} {
		if r.Working.MatchString(read(n)) {
			t.Errorf("%s: idle pane read as working", n)
		}
		if !r.Idle.MatchString(read(n)) {
			t.Errorf("%s: Idle rule missed", n)
		}
	}
	if !r.Modal.MatchString(read("trust-prompt.txt")) {
		t.Error("the trust modal was not recognised")
	}
	for _, n := range []string{"idle-after-turn.txt", "working.txt", "quoted-dialog-in-scrollback.txt", "idle-startup-extkeys-on.txt"} {
		if r.Modal.MatchString(read(n)) {
			t.Errorf("%s: a non-modal pane matched the modal rule (quoted dialogs must not)", n)
		}
	}
	// T-0013 rule: trust text that scrolled above a live footer is history, not a modal
	trust := read("trust-prompt.txt")
	if hooks.ModalExcerptWith(r, trust) == "" {
		t.Error("a live trust modal must yield an excerpt")
	}
	if got := hooks.ModalExcerptWith(r, trust+"\n"+read("idle-after-turn.txt")); got != "" {
		t.Errorf("trust text above a live footer is history, got excerpt %q", got)
	}
	if r.Below == nil || !r.Below.MatchString("0.3%/1.0M (auto)") {
		t.Error("Below must be the idle footer rule")
	}
	// a lane-denied tool result (captured from a live run) is an ordinary idle pane
	if b, err := os.ReadFile(filepath.Join(dir, "lane-denied-tool-result.txt")); err != nil || !strings.Contains(string(b), "slai lane guard: engineer may not write") || r.Working.MatchString(string(b)) || !r.Idle.MatchString(string(b)) {
		t.Errorf("lane-denied fixture: %v", err)
	}
	// a 401 is an auth failure, not a rate limit
	if r.RateLimit.MatchString(read("provider-error-badkey.txt")) {
		t.Error("401 matched the rate-limit rule")
	}
	for _, s := range []string{"Error: 429: Too Many Requests", "Error: rate limit exceeded", "Error: Insufficient balance or no resource package"} {
		if !r.RateLimit.MatchString(s) {
			t.Errorf("rate-limit rule missed %q", s)
		}
	}
	if r.RateLimit.MatchString("the user said: Error handling for rate limits is done") {
		t.Error("ordinary prose matched the rate-limit rule")
	}
}
