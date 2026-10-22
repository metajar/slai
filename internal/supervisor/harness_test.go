package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

// asHarness reconfigures an existing agent as another harness's (its tmux session is
// still the fake claude, which is all these tests need).
func asHarness(s *Supervisor, id, kind string) {
	for i := range s.Team.Agents {
		if s.Team.Agents[i].ID == id {
			s.Team.Agents[i].Harness, s.Team.Agents[i].Model = kind, "zai/glm-5.3"
		}
	}
}

func neutralEvent(t *testing.T, s *Supervisor, id, name string, p hooks.Payload) {
	t.Helper()
	p.Event = name
	ev, p, _, err := hooks.Adapt(name, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hooks.Handle(s.Store, id, ev, p, hooks.Options{}); err != nil {
		t.Fatal(err)
	}
}

// AC 11: a harness that pushes plugin events (working -> idle -> working -> idle)
// moves the board liveness busy -> idle and stuck detection stays quiet while it is busy.
func TestPluginEventsDriveStatusAndStuckStaysQuiet(t *testing.T) {
	s, now := driverEnv(t)
	asHarness(s, "eng-1", "opencode")
	ctx := context.Background()
	_, _ = s.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress})
	live := func() string { return service.AgentHealth(s.Store, "eng-1", true, time.Now()).Liveness }
	st := func() string { return status(t, s, "eng-1") }

	neutralEvent(t, s, "eng-1", "session_start", hooks.Payload{SessionID: "ses_1"})
	if st() != "idle" {
		t.Fatalf("after session_start: %s", st())
	}
	neutralEvent(t, s, "eng-1", "prompt", hooks.Payload{})
	if st() != "working" || live() != "busy" {
		t.Fatalf("after prompt: status %s liveness %s", st(), live())
	}
	// a long turn: tool calls keep arriving, with a quiet stretch while one runs
	neutralEvent(t, s, "eng-1", "tool", hooks.Payload{Tool: "bash"})
	for i := 0; i < 3; i++ {
		*now = now.Add(StuckAfter + time.Minute) // silent for longer than the stuck threshold: a tool is in flight
		s.driveTick(ctx)
		*now = now.Add(StuckPaneGap + time.Second)
		s.driveTick(ctx)
		s.healthTick(ctx)
	}
	if hasEvent(s, "stuck_detected") || hasEvent(s, "stuck_nudge") || hasEvent(s, "stuck_restart") {
		t.Fatal("stuck detection fired during a busy turn of an event-pushing harness")
	}
	neutralEvent(t, s, "eng-1", "tool_end", hooks.Payload{Tool: "bash"})
	neutralEvent(t, s, "eng-1", "stop", hooks.Payload{})
	if got := st(); got != "finished" && got != "idle" {
		t.Fatalf("after stop: %s", got)
	}
	if l := live(); l == "busy" || l == "dead" {
		t.Fatalf("liveness after stop = %s", l)
	}
	neutralEvent(t, s, "eng-1", "prompt", hooks.Payload{})
	if st() != "working" || live() != "busy" {
		t.Fatalf("second turn: status %s liveness %s", st(), live())
	}
	neutralEvent(t, s, "eng-1", "stop", hooks.Payload{})
	neutralEvent(t, s, "eng-1", "idle", hooks.Payload{})
	if l := live(); l == "busy" {
		t.Fatalf("still busy after idle: %s", l)
	}
}

// recordingTmux is a tmux stand-in that logs every invocation and reports every
// session as missing, so StartAgent / typing can be asserted on exact arguments.
func recordingTmux(t *testing.T) (*tmux.Client, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shq(log) + "\ncase \"$*\" in *has-session*) exit 1;; esac\nexit 0\n"
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := tmux.New("fake")
	c.Bin = bin
	return c, func() []string {
		b, _ := os.ReadFile(log)
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
}

func shq(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// AC 13: typing uses the harness's own Keys(): claude clears with Escape,C-u and
// pauses before Enter; opencode (and pi) clear with C-u alone and submit at once.
func TestTypingUsesTheHarnessKeys(t *testing.T) {
	oldD, oldP := tmux.EnterDelay, StuckKeyPause
	tmux.EnterDelay, StuckKeyPause = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { tmux.EnterDelay, StuckKeyPause = oldD, oldP })
	for _, tc := range []struct {
		kind       string
		wantClear  string
		minPause   time.Duration
		neverEsc   bool
		wantSubmit string
	}{
		{"claude", "send-keys -t =SESS: Escape C-u", 30 * time.Millisecond, false, "send-keys -t SESS Enter"},
		{"opencode", "send-keys -t =SESS: C-u", 0, true, "send-keys -t SESS Enter"},
		{"pi", "send-keys -t =SESS: C-u", 0, true, "send-keys -t SESS Enter"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s := unitEnv(t, team(5))
			c, calls := recordingTmux(t)
			s.Tmux = c
			asHarness(s, "eng-1", tc.kind)
			if tc.kind == "claude" {
				s.Team.Agents[1].Harness, s.Team.Agents[1].Model = "", "sonnet"
			}
			a := store.Agent{ID: "eng-1", TmuxSession: "SESS"}
			start := time.Now()
			if err := s.retype(context.Background(), a, "[slai] You have 2 new items. Call get_inbox."); err != nil {
				t.Fatal(err)
			}
			took := time.Since(start)
			got := calls()
			if len(got) != 3 || got[0] != "-L fake "+tc.wantClear || !strings.Contains(got[1], "-l -- [slai] You have 2 new items") || got[2] != "-L fake "+tc.wantSubmit {
				t.Fatalf("tmux calls:\n%s", strings.Join(got, "\n"))
			}
			if tc.neverEsc && strings.Contains(strings.Join(got, " "), "Escape") {
				t.Errorf("%s: Escape interrupts a running turn and does not clear the box: %v", tc.kind, got)
			}
			// the pause before Enter is the harness's: none for opencode/pi
			if tc.minPause == 0 && took >= 25*time.Millisecond {
				t.Errorf("%s waited %s before Enter", tc.kind, took)
			}
			if tc.minPause > 0 && took < tc.minPause {
				t.Errorf("claude did not pause before Enter (%s)", took)
			}
		})
	}
}

// AC 14: resume. A revive resumes the id slai holds and never uses -c/--continue; an
// opencode agent that never reported a session id starts fresh.
func TestOpenCodeReviveResumesCapturedIDOrStartsFresh(t *testing.T) {
	s := unitEnv(t, team(5))
	c, calls := recordingTmux(t)
	s.Tmux = c
	s.Team.Agents[0].Harness, s.Team.Agents[0].Model = "opencode", "zhipuai-coding-plan/glm-5.3" // orch: no worktree
	cfg := s.Team.Agents[0]
	ctx := context.Background()
	newSession := func() string {
		for _, l := range calls() {
			if strings.Contains(l, "new-session") {
				return l
			}
		}
		return ""
	}
	// first start: fresh, and no session id yet
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	first := newSession()
	cmdline := first[strings.Index(first, "SLAI_AGENT_ID='"):] // after tmux's own -c <cwd>
	if first == "" || strings.Contains(cmdline, " -s ") || strings.Contains(cmdline, "--continue") || strings.Contains(cmdline, " -c ") {
		t.Fatalf("first launch:\n%s", first)
	}
	if !strings.Contains(first, "opencode -m 'zhipuai-coding-plan/glm-5.3' --agent 'slai-orch'") {
		t.Fatalf("not an opencode launch line:\n%s", first)
	}
	// revive with no captured id: fresh again
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for _, l := range calls() {
		if i := strings.Index(l, "SLAI_AGENT_ID='"); i >= 0 && strings.Contains(l[i:], " -s ") {
			t.Fatal("resumed without a captured session id")
		}
	}
	// the plugin reported its id (slai hook session_start): the next revive resumes it, falling back to fresh
	if err := hooks.SaveSessionID(s.Dir, "orch", "ses_captured1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	all := calls()
	last := all[len(all)-1]
	for _, l := range all {
		if strings.Contains(l, "new-session") {
			last = l
		}
	}
	if !strings.Contains(last, "-s 'ses_captured1' ||") || !regexp.MustCompile(`\|\| .*opencode -m`).MatchString(last) {
		t.Fatalf("revive with a captured id:\n%s", last)
	}
	for _, l := range all {
		if i := strings.Index(l, "SLAI_AGENT_ID='"); i >= 0 {
			l = l[i:]
		}
		if strings.Contains(l, "--continue") || strings.Contains(l, " -c ") || strings.Contains(l, "--pure") {
			t.Fatalf("forbidden flag in: %s", l)
		}
	}
}

// AC 15 (backend half): usage pushed by a plugin counts against the spend caps.
func TestPluginUsageCountsAgainstSpendCaps(t *testing.T) {
	s := unitEnv(t, team(5))
	s.Team.Defaults.Caps = config.SpendCaps{PerAgentUSD: 0.05}
	asHarness(s, "eng-1", "opencode")
	_ = s.Store.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer"})
	if g := s.checkSpend(); g.agents["eng-1"] {
		t.Fatal("capped before any spend")
	}
	neutralEvent(t, s, "eng-1", "stop", hooks.Payload{Usage: &hooks.NeutralUsage{Prompt: 4000, Output: 300, Cost: 0.06}})
	if g := s.checkSpend(); !g.agents["eng-1"] {
		t.Fatal("the plugin's cost did not reach the per-agent cap")
	}
	if !hasEvent(s, "spend_cap") {
		t.Fatal("no spend_cap event")
	}
}

// Nothing outside the harness packages builds a harness command line or parses a
// pane: the supervisor, the CLI and the services reach them only through
// harness.For / the agent's Harness (spec harnesses.md decision 8b).
func TestNothingOutsideTheHarnessPackagesBuildsCommandsOrParsesPanes(t *testing.T) {
	forbidden := []struct{ what, re string }{
		{"claude's launch spec", `claude\.Spec\b`},
		{"claude's pane detector", `hooks\.DetectFromPane\b`},
		{"claude's modal detector", `hooks\.ModalExcerpt\(`},
		{"a hard-coded claude key sequence", `Named\("Escape"\)`},
		{"claude CLI flags", `--append-system-prompt|--mcp-config|--session-id`},
		{"claude's trust check", `claude\.IsTrusted`},
	}
	roots := []string{".", filepath.Join("..", "..", "cmd"), filepath.Join("..", "service"), filepath.Join("..", "mcpserver"), filepath.Join("..", "tui")}
	for _, root := range roots {
		files, _ := filepath.Glob(filepath.Join(root, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, fb := range forbidden {
				if m := regexp.MustCompile(fb.re).FindString(string(b)); m != "" {
					// cmd/tui.go answers claude's own trust dialog for the TUI banner: allowed until the TUI gets Trusted()
					if fb.what == "claude's trust check" && strings.HasSuffix(f, "tui.go") {
						continue
					}
					t.Errorf("%s uses %s (%s); go through harness.For", f, fb.what, m)
				}
			}
		}
	}
}

// AC 14 (pi): a revive relaunches with the stored --session-id, which pi resumes or
// creates; never --continue.
func TestPiReviveReusesItsStoredSessionID(t *testing.T) {
	s := unitEnv(t, team(5))
	c, calls := recordingTmux(t)
	s.Tmux = c
	s.Team.Agents[0].Harness, s.Team.Agents[0].Model = "pi", "zai/glm-5.3" // orch: no worktree
	cfg := s.Team.Agents[0]
	ctx := context.Background()
	launch := func() string {
		var last string
		for _, l := range calls() {
			if i := strings.Index(l, "SLAI_AGENT_ID='"); i >= 0 && strings.Contains(l, "new-session") {
				last = l[i:]
			}
		}
		return last
	}
	sid := func(l string) string {
		m := regexp.MustCompile(`--session-id '([^']+)'`).FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("no --session-id in: %s", l)
		}
		return m[1]
	}
	if _, err := s.StartAgent(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	first := launch()
	if !strings.Contains(first, " pi --model 'zai/glm-5.3'") || strings.Contains(first, "--continue") || strings.Contains(first, "--approve") {
		t.Fatalf("first pi launch:\n%s", first)
	}
	if _, err := s.StartAgent(ctx, cfg); err != nil { // the revive
		t.Fatal(err)
	}
	if again := launch(); sid(again) != sid(first) || strings.Contains(again, "--continue") {
		t.Fatalf("revive did not reuse the stored session id:\n first %s\n again %s", first, again)
	}
	if b, _ := os.ReadFile(harness.SessionIDPath(s.Dir, "orch")); strings.TrimSpace(string(b)) != sid(first) {
		t.Fatalf("stored id %q != launch id %q", b, sid(first))
	}
}

// pi's extension and the supervisor must agree on where the guard_ready marker is.
func TestPiAndSupervisorAgreeOnTheGuardMarker(t *testing.T) {
	if harness.PiGuardReadyPath("/p", "eng-1") != harness.GuardReadyPath("/p", "eng-1") {
		t.Fatalf("pi writes %s but the supervisor looks at %s", harness.PiGuardReadyPath("/p", "eng-1"), harness.GuardReadyPath("/p", "eng-1"))
	}
}
