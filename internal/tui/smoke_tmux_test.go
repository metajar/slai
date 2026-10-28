package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// TestRealTmuxSmoke drives the real slai TUI inside a private tmux server
// (host), with the agents running on a separate tmux socket.
func TestRealTmuxSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	for _, b := range []string{"tmux", "git", "go"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	root, _ := filepath.Abs("../..")
	bin := t.TempDir()
	for out, pkg := range map[string]string{"slai": ".", "claude": "./testdata/fakeclaude"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, out), pkg)
		cmd.Dir = root
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, o)
		}
	}

	repo := t.TempDir()
	sh := func(name string, args ...string) {
		t.Helper()
		c := exec.Command(name, args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if o, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, o)
		}
	}
	sh("git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh("git", "add", ".")
	sh("git", "commit", "-m", "init")
	team := config.Preset("solo", "smokeproj", "smoke", "main")
	if err := os.MkdirAll(filepath.Join(repo, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(filepath.Join(repo, ".slai", "team.yaml"), team); err != nil {
		t.Fatal(err)
	}
	// Pretend Claude already trusts everything so the trust banner does not
	// change the layout (the banner itself is covered by unit tests).
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, ".claude.json"), []byte(fmt.Sprintf(`{"projects":{"/":{"hasTrustDialogAccepted":true},%q:{"hasTrustDialogAccepted":true}}}`, filepath.Join(repo, ".slai", "worktrees", "eng-1"))), 0o644); err != nil {
		t.Fatal(err)
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	host, agents := "slaihost"+id, "slaiagents"+id
	tm := func(sock string, args ...string) (string, error) {
		o, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput()
		return string(o), err
	}
	t.Cleanup(func() {
		tm(host, "kill-server") // the dashboard first: its watchdog respawns the supervisor
		// then the detached supervisor the dashboard started (it outlives its tmux session)
		if b, err := os.ReadFile(filepath.Join(repo, ".slai", "run", "supervisor.pid")); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		tm(agents, "kill-server")
		tmux.RemoveSocket(host)
		tmux.RemoveSocket(agents)
	})

	env := fmt.Sprintf("env SLAI_NO_ORPHAN_SCAN=1 PATH=%s:%s SLAI_TMUX_SOCKET=%s CLAUDE_CONFIG_DIR=%s TERM=xterm-256color FAKECLAUDE_LOG=%s ",
		bin, os.Getenv("PATH"), agents, cfgDir, filepath.Join(t.TempDir(), "fc.log"))
	if o, err := tm(host, "new-session", "-d", "-s", "ui", "-x", "200", "-y", "50", "-c", repo, env+filepath.Join(bin, "slai")); err != nil {
		t.Fatalf("host session: %v %s", err, o)
	}
	screen := func() string { o, _ := tm(host, "capture-pane", "-p", "-t", "ui"); return o }
	// tail keeps failure output readable: the last 40 lines of a screen or pane.
	tail := func(s string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(lines) > 40 {
			lines = append([]string{fmt.Sprintf("... (%d earlier lines omitted)", len(lines)-40)}, lines[len(lines)-40:]...)
		}
		return strings.Join(lines, "\n")
	}
	// waitUntil polls the host screen until ok(screen) holds; waitFor is the
	// "contains all of" case. Wait for the final state, not its first sign:
	// intermediate frames (e.g. a half-scrolled preview) satisfy partial checks.
	waitUntil := func(what string, ok func(string) bool) string {
		t.Helper()
		var s string
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if s = screen(); ok(s) {
				return s
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s; screen tail:\n%s", what, tail(s))
		return s
	}
	waitFor := func(what string, sub ...string) string {
		t.Helper()
		return waitUntil(what+fmt.Sprint(sub), func(s string) bool {
			for _, x := range sub {
				if !strings.Contains(s, x) {
					return false
				}
			}
			return true
		})
	}
	gone := func(what, sub string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !strings.Contains(screen(), sub) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s to vanish (%q); screen tail:\n%s", what, sub, tail(screen()))
	}
	keys := func(k ...string) {
		t.Helper()
		if o, err := tm(host, append([]string{"send-keys", "-t", "ui"}, k...)...); err != nil {
			t.Fatalf("send-keys: %v %s", err, o)
		}
	}
	typeText := func(s string) { keys("-l", s) }
	session := func(suffix string) string {
		list, _ := tm(agents, "list-sessions", "-F", "#{session_name}")
		for _, l := range strings.Split(list, "\n") {
			if strings.HasSuffix(l, "_"+suffix) {
				return l
			}
		}
		t.Fatalf("no session *_%s in %q", suffix, list)
		return ""
	}
	agentPane := func(suffix string) string {
		o, _ := tm(agents, "capture-pane", "-p", "-t", session(suffix))
		return o
	}
	waitAgent := func(suffix, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(agentPane(suffix), want) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s never showed %q; pane tail:\n%s", suffix, want, tail(agentPane(suffix)))
	}
	winSize := func(suffix string) (int, int) {
		o, err := tm(agents, "display-message", "-p", "-t", "="+session(suffix)+":", "#{window_width} #{window_height}")
		if err != nil {
			t.Fatalf("display-message: %v %s", err, o)
		}
		f := strings.Fields(o)
		w, _ := strconv.Atoi(f[0])
		h, _ := strconv.Atoi(f[1])
		return w, h
	}
	waitWidth := func(suffix string, want int) (int, int) {
		t.Helper()
		var w, h int
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if w, h = winSize(suffix); w == want {
				return w, h
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("window width of %s = %d, want %d", suffix, w, want)
		return w, h
	}

	// layout: agent list left, live preview right, no always-visible prompt
	s := waitFor("list + preview", "smokeproj", "AGENTS", "Orchestrator", "Engineers", "orch", "eng-1", "supervisor: ")
	for _, bad := range []string{"BOARD", "to orch:"} {
		if strings.Contains(s, bad) {
			t.Errorf("old UI element %q still on screen", bad)
		}
	}
	waitFor("orch preview", "orch · opus", "fakeclaude 0.0.0", "press enter to talk to the orchestrator")
	keys("j") // Engineers header
	keys("j") // eng-1
	waitFor("eng-1 preview", "eng-1 · sonnet", "fakeclaude 0.0.0")

	// the agent window is exactly the preview pane: w=200 -> list 40, preview 160, no side bars so inner 160
	// (rows: header, first-run goal hint, footer, 2 border rows)
	if _, ph := waitWidth("eng-1", 160); ph != 50-2-2-1 {
		t.Errorf("window height = %d, want %d", ph, 50-2-2-1)
	}

	// The quick prompt queues mail, and the supervisor sends a safe wake. The
	// fake Claude process only echoes input; it cannot call get_inbox itself.
	keys("Space")
	waitFor("quick prompt", "to eng-1:")
	typeText("smoke-hello")
	keys("Enter")
	waitAgent("eng-1", "echo: [slai] You have 1 new items. Call get_inbox.")
	waitFor("wake in preview", "Call get_inbox.")
	st, err := store.Open(filepath.Join(repo, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	msgs, err := st.UndeliveredFor("eng-1")
	if err != nil || len(msgs) != 1 || msgs[0].Body != "smoke-hello" {
		t.Fatalf("queued quick prompt: %+v, %v", msgs, err)
	}
	gone("quick prompt", "to eng-1:")
	keys("Space") // esc cancels
	waitFor("quick prompt 2", "to eng-1:")
	typeText("never-sent")
	keys("Escape")
	gone("quick prompt", "to eng-1:")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(agentPane("eng-1"), "never-sent") {
		t.Fatal("cancelled quick prompt was delivered")
	}

	// focus: every key is forwarded; the list stays visible
	keys("Enter")
	waitFor("focus", "FOCUSED: eng-1", "ctrl+q", "AGENTS")
	typeText("hello")
	keys("Enter")
	waitAgent("eng-1", "echo: hello")
	waitFor("echo in focused preview", "echo: hello")
	// bracketed multi-line paste arrives line by line
	if o, err := tm(host, "set-buffer", "-b", "t", "pa1\npa2\n"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	if o, err := tm(host, "paste-buffer", "-p", "-d", "-b", "t", "-t", "ui"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	waitAgent("eng-1", "echo: pa2")
	if !strings.Contains(agentPane("eng-1"), "echo: pa1") {
		t.Fatalf("paste lost a line:\n%s", tail(agentPane("eng-1")))
	}
	// keys other than the return key (q, ?, b) are typed into the agent, not the TUI
	typeText("qb?")
	keys("Enter")
	waitAgent("eng-1", "echo: qb?")
	// scrollback: fill the agent with >200 lines, then drive the wheel with raw
	// SGR mouse events through the host pane (button 64 = wheel up, 65 = down).
	var bulk strings.Builder
	for i := 1; i <= 240; i++ {
		fmt.Fprintf(&bulk, "n%03d\n", i)
	}
	if o, err := tm(host, "set-buffer", "-b", "bulk", bulk.String()); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	if o, err := tm(host, "paste-buffer", "-p", "-d", "-b", "bulk", "-t", "ui"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	waitAgent("eng-1", "echo: n240")
	waitFor("bottom of the output", "echo: n240")
	sgr := func(code, x, y int, final string) { typeText(fmt.Sprintf("\x1b[<%d;%d;%d%s", code, x, y, final)) }
	for i := 0; i < 10; i++ {
		sgr(64, 100, 20, "M") // wheel up over the preview
	}
	// all ten notches must have been processed: the first one alone already shows
	// "lines back" while the newest line is still on screen.
	s = waitUntil("scrolled back past the newest line", func(s string) bool {
		return strings.Contains(s, "lines back") && !strings.Contains(s, "echo: n240")
	})
	if !strings.Contains(s, "FOCUSED: eng-1") {
		t.Fatalf("lost focus while scrolling:\n%s", tail(s))
	}
	for i := 0; i < 3; i++ {
		sgr(65, 100, 20, "M") // wheel down
	}
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(agentPane("eng-1"), "[<64") || strings.Contains(agentPane("eng-1"), "[<65") {
		t.Fatalf("wheel events leaked into the agent:\n%s", tail(agentPane("eng-1")))
	}
	// a drag inside the preview selects (reverse video) and copies
	hostSGR := func() string { o, _ := tm(host, "capture-pane", "-p", "-e", "-t", "ui"); return o }
	sgr(0, 60, 10, "M")
	sgr(32, 75, 11, "M")
	sgr(0, 90, 12, "m")
	waitFor("copied status", "copied 3 lines")
	if !strings.Contains(hostSGR(), "\x1b[7m") {
		t.Fatal("selection is not highlighted")
	}
	// typing returns to the bottom, clears the selection and reaches the agent
	typeText("zz")
	keys("Enter")
	waitAgent("eng-1", "echo: zz")
	waitUntil("back at the bottom", func(s string) bool {
		return strings.Contains(s, "echo: zz") && !strings.Contains(s, "lines back")
	})
	// resizing the terminal resizes the agent window with it: 160 wide -> preview 120 (no side bars, so the window is 120 too)
	if o, err := tm(host, "resize-window", "-t", "ui", "-x", "160", "-y", "40"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	_, h := waitWidth("eng-1", 120)
	if h != 40-2-2-1 {
		t.Errorf("window height after resize = %d, want %d", h, 40-2-2-1)
	}
	if o, err := tm(host, "resize-window", "-t", "ui", "-x", "200", "-y", "50"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	waitWidth("eng-1", 160)
	// ctrl+q returns to the list
	keys("C-q")
	gone("focus", "FOCUSED")
	waitFor("list again", "AGENTS", "quick prompt")

	// overlays: b = tasks, l = log, esc returns
	keys("b")
	waitFor("tasks", "Tasks", "no tasks yet")
	gone("agents list hidden behind tasks", "AGENTS")
	keys("Escape")
	waitFor("back from tasks", "AGENTS")
	keys("l")
	waitFor("log", "Log", "newest at the bottom")
	keys("Escape")
	waitFor("back from log", "AGENTS")

	keys("?")
	waitFor("help", "slai keys")
	keys("x")
	gone("help", "slai keys")

	keys("p")
	waitFor("paused", "PAUSED")
	keys("p")
	gone("paused", "PAUSED")

	// full-screen attach / detach (ctrl+q)
	keys("a")
	gone("TUI while attached", "AGENTS")
	waitFor("attached view", "echo: zz")
	if o, _ := tm(agents, "list-keys", "-T", "root"); !strings.Contains(o, "C-q") || !strings.Contains(o, "detach-client") {
		t.Errorf("C-q not bound to detach-client on agents socket:\n%s", o)
	}
	keys("C-q")
	waitFor("back in TUI", "AGENTS", "smokeproj")
	waitWidth("eng-1", 160) // preview size is re-applied after the attach

	// An app that handles the mouse itself (like Claude Code: alternate screen
	// + SGR mouse reporting). fakeclaude switches modes on the "@altmouse" line.
	keys("Enter")
	waitFor("focus eng-1 again", "FOCUSED: eng-1")
	typeText("@altmouse")
	keys("Enter")
	waitAgent("eng-1", "ALT SCREEN READY")
	waitFor("alt-screen preview", "ALT SCREEN READY", "mouse: app")
	mouseLines := func() []string {
		var out []string
		for _, l := range strings.Split(agentPane("eng-1"), "\n") {
			if strings.HasPrefix(l, "MOUSE") {
				out = append(out, strings.TrimSpace(l))
			}
		}
		return out
	}
	waitMouse := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			all := strings.Join(mouseLines(), "\n")
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(all, w)
			}
			if ok {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("agent never saw %v; mouse lines %v\n%s", want, mouseLines(), tail(agentPane("eng-1")))
	}
	// host (1-based) x=100,y=20 -> preview left 40 (no side bars), top 3 (0-based; the goal hint takes a row) -> agent col 60, row 17
	sgr(64, 100, 20, "M")
	waitMouse("MOUSE wheel-up col=60 row=17")
	// the notch is several reports; the first arriving says nothing about the rest
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) &&
		strings.Count(strings.Join(mouseLines(), "\n"), "wheel-up") < wheelReportsPerNotch; {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // and none beyond it may follow
	if n := strings.Count(strings.Join(mouseLines(), "\n"), "wheel-up"); n != wheelReportsPerNotch {
		t.Fatalf("%d wheel-up reports for one notch, want %d: %v", n, wheelReportsPerNotch, mouseLines())
	}
	sgr(0, 100, 20, "M")
	sgr(0, 100, 20, "m")
	waitMouse("MOUSE press left col=60 row=17", "MOUSE release left col=60 row=17")
	// a drag is selection + copy and never reaches the app
	nBefore := len(mouseLines())
	sgr(0, 42, 4, "M")
	sgr(32, 55, 5, "M")
	sgr(0, 60, 6, "m")
	waitFor("copied status (app mouse)", "copied 3 lines")
	time.Sleep(300 * time.Millisecond)
	if len(mouseLines()) != nBefore {
		t.Fatalf("drag reached the app: %v", mouseLines()[nBefore:])
	}
	// the wheel in list mode also reaches the app
	keys("C-q")
	gone("focus", "FOCUSED")
	sgr(65, 100, 20, "M")
	waitMouse("MOUSE wheel-down col=60 row=17")

	// quit leaves agents alive
	keys("q")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := tm(host, "has-session", "-t", "ui"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TUI did not quit:\n%s", tail(screen()))
		}
		time.Sleep(150 * time.Millisecond)
	}
	list, _ := tm(agents, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(list, "_orch") || !strings.Contains(list, "_eng-1") {
		t.Fatalf("agent sessions gone after quit: %q", list)
	}
}
