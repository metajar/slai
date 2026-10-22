package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// Self-heal scenarios: the real supervisor, tmux, hooks and git against the
// scripted fake agents (see testdata/fakeclaude/autopilot.go for the knobs).
// SLAI_TEST_TIMESCALE shrinks the supervisor's health thresholds (minutes) to
// seconds; SLAI_NO_DRIVER is cleared so the driver (revive, stuck, escalate) runs.

const healTimeout = 120 * time.Second

func healOpts(extra map[string]string) opts {
	env := map[string]string{"SLAI_NO_DRIVER": "", "SLAI_TEST_TIMESCALE": "0.02", "SLAI_E2E": "1"}
	for k, v := range extra {
		env[k] = v
	}
	return opts{env: env}
}

// healKinds are the harnesses the parity scenarios run on (AC 21): the same
// scenario, with the agent under test on a fake pi / fake opencode.
var healKinds = []string{"claude", "pi", "opencode"}

// healOptsOn is healOpts with agent running on the given harness. Each knob is set
// under both fakes' names, so a scenario is written once.
func healOptsOn(kind, agent string, knobs map[string]string) opts {
	extra := map[string]string{}
	for k, v := range knobs { // knob names are given without the fake's prefix: SLOW, HANG...
		extra["FAKECLAUDE_"+k] = v
		extra["FAKEHARNESS_"+k] = v
	}
	o := healOpts(extra)
	if kind != "claude" {
		o.harnesses = map[string]string{agent: kind}
	}
	return o
}

// eachKind runs a scenario once per harness.
func eachKind(t *testing.T, scenario func(t *testing.T, kind string)) {
	for _, k := range healKinds {
		t.Run(k, func(t *testing.T) { scenario(t, k) })
	}
}

// run runs slai (as the user, or as agent when non-empty) and returns output and exit code.
func (p *project) run(agent string, args ...string) (string, int) {
	p.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "slai"), args...)
	cmd.Dir = p.dir
	cmd.Env = append(append([]string{}, p.env...), "SLAI_AGENT_ID="+agent, "SLAI_PROJECT="+map[bool]string{true: p.dir}[agent != ""])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // the test-mode notice goes here; keep stdout parseable
	b, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		p.t.Fatalf("slai %v: %v", args, err)
	}
	return string(b), code
}

func (p *project) session(agent string) string {
	p.t.Helper()
	st := p.db()
	defer st.Close()
	a, err := st.AgentByID(agent)
	if err != nil || a.TmuxSession == "" {
		p.t.Fatalf("no tmux session for %s: %v", agent, err)
	}
	return a.TmuxSession
}

func (p *project) killSession(agent string) {
	p.t.Helper()
	if out, err := exec.Command("tmux", "-L", p.sock, "kill-session", "-t", "="+p.session(agent)).CombinedOutput(); err != nil {
		p.t.Fatalf("kill-session %s: %v %s", agent, err, out)
	}
}

// fakeRecords returns the fake log records with the given event (and agent when non-empty).
func (p *project) fakeRecords(event, agent string) []map[string]any {
	b, _ := os.ReadFile(p.fakeLg)
	var out []map[string]any
	for _, l := range strings.Split(string(b), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(l), &r) == nil && r["event"] == event && (agent == "" || r["slai_agent_id"] == agent) {
			out = append(out, r)
		}
	}
	return out
}

func (p *project) messagesTo(to, substr string) []store.Message {
	st := p.db()
	defer st.Close()
	ms, _ := st.RecentMessages(500)
	var out []store.Message
	for _, m := range ms {
		if m.To == to && m.From == "slai" && strings.Contains(m.Body, substr) {
			out = append(out, m)
		}
	}
	return out
}

func (p *project) openEscalations() []store.Escalation {
	st := p.db()
	defer st.Close()
	es, _ := st.OpenEscalations()
	return es
}

func (p *project) supervisorPid() int {
	b, err := os.ReadFile(filepath.Join(p.dir, ".slai", "run", "supervisor.pid"))
	if err != nil {
		p.t.Fatalf("no supervisor pidfile: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// 1. A crashed engineer session is revived, told it was restarted, and its task still merges.
func TestSelfHealCrashRevive(t *testing.T) { eachKind(t, selfHealCrashRevive) }

func selfHealCrashRevive(t *testing.T, kind string) {
	p := newProject(t, healOptsOn(kind, "eng-1", map[string]string{"SLOW_ENG_1": "60000"}))
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(loopTimeout, "eng-1 mid-task", func() bool { return len(p.fakeRecords("slow", "eng-1")) > 0 || len(p.fakeRecords("slow", "")) > 0 })
	p.killSession("eng-1")
	p.waitFor(healTimeout, "eng-1 revived", func() bool { return len(p.events("auto_revive", "eng-1")) > 0 && len(p.fakeStarts("eng-1")) >= 2 })
	p.waitFor(healTimeout, "restart wake line", func() bool {
		for _, r := range p.fakeRecords("input", "eng-1") {
			if l, _ := r["line"].(string); strings.Contains(l, "restarted") {
				return true
			}
		}
		return false
	})
	p.waitFor(healTimeout, "tasks merged after the crash", func() bool { return p.allMerged(2) })
	p.waitFor(30*time.Second, "summary", func() bool { return p.fileExists(p.summaryPath()) })
}

// 2. A hung turn is nudged, then restarted, and the task finishes.
func TestSelfHealHungTurn(t *testing.T) { eachKind(t, selfHealHungTurn) }

func selfHealHungTurn(t *testing.T, kind string) {
	p := newProject(t, healOptsOn(kind, "eng-1", map[string]string{"HANG_ENG_1": "1"}))
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(healTimeout, "stuck_nudge", func() bool { return len(p.events("stuck_nudge", "eng-1")) > 0 })
	p.waitFor(healTimeout, "stuck_restart", func() bool { return len(p.events("stuck_restart", "eng-1")) > 0 })
	nudge, restart := p.events("stuck_nudge", "eng-1")[0], p.events("stuck_restart", "eng-1")[0]
	if nudge.ID > restart.ID {
		t.Errorf("restart (event %d) came before the nudge (event %d)", restart.ID, nudge.ID)
	}
	p.waitFor(healTimeout, "tasks merged", func() bool { return p.allMerged(2) })
	if n := len(p.fakeStarts("eng-1")); n < 2 {
		t.Errorf("eng-1 starts = %d, want a restart", n)
	}
}

// 3. A permission prompt nobody answers: one user escalation, one orchestrator
// note, and slai never types into that pane.
func TestSelfHealPermissionPrompt(t *testing.T) {
	p := newProject(t, healOpts(map[string]string{"FAKECLAUDE_MODAL_ENG_1": "1"}))
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(healTimeout, "modal_stuck escalation", func() bool { return len(p.events("modal_stuck", "eng-1")) > 0 })
	modal := p.fakeRecords("modal", "")
	if len(modal) == 0 {
		t.Fatal("fake never showed the prompt")
	}
	// Stay well inside ModalRepeat (10min * timescale) and check nothing fires twice.
	time.Sleep(4 * time.Second)
	if n := len(p.events("modal_stuck", "eng-1")); n != 1 {
		t.Errorf("modal_stuck events = %d, want exactly 1", n)
	}
	var user bool
	for _, e := range p.openEscalations() {
		if e.Agent == "eng-1" && e.Kind == "modal_stuck" {
			user = e.User
		}
	}
	if !user {
		t.Errorf("modal_stuck must be a user-level escalation: %+v", p.openEscalations())
	}
	if n := len(p.messagesTo("orch", "stuck on a prompt")); n != 1 {
		t.Errorf("orchestrator notes about the prompt = %d, want 1", n)
	}
	// Known issue (reported separately): the orchestrator's pane echoes slai's note,
	// which quotes the prompt text, so the modal detector sees "a prompt" there too.
	if n := len(p.events("type_skipped_modal", "orch")); n > 0 {
		t.Logf("orch pane matched the modal detector %d time(s) because the note quotes the prompt", n)
	}
	// Nothing was typed into eng-1's pane after the prompt appeared (the assignment
	// wake line came before it).
	inputs := p.fakeRecords("input", "eng-1")
	var firstModal string
	if len(modal) > 0 {
		firstModal, _ = modal[0]["time"].(string)
	}
	for _, r := range inputs {
		if at, _ := r["time"].(string); at > firstModal {
			t.Errorf("slai typed into the prompt pane after it appeared: %v", r)
		}
	}
}

// 4. An orchestrator that died while work is open and owes nothing (so autoRevive
// leaves it alone) is declared down once after OrchSilentAfter: the user is told
// directly and it is restarted with a resume brief.
func TestSelfHealOrchDown(t *testing.T) { eachKind(t, selfHealOrchDown) }

func selfHealOrchDown(t *testing.T, kind string) {
	p := newProject(t, healOptsOn(kind, "orch", nil))
	p.up()
	// An open task nobody is working on: eng-1 is held, so nothing wakes or nudges it.
	st := p.db()
	if err := st.SetAgentPaused("eng-1", true); err != nil {
		t.Fatal(err)
	}
	st.Close()
	out, code := p.run("orch", "tool", "create_task", `{"title":"open work","description":"d","assignee":"eng-1","files":["x.txt"]}`)
	if code != 0 {
		t.Fatalf("create_task: %s", out)
	}
	p.killSession("orch")
	p.waitFor(healTimeout, "orch_down", func() bool { return len(p.events("orch_down", "orch")) > 0 })
	p.waitFor(healTimeout, "orch restarted", func() bool { return len(p.fakeStarts("orch")) >= 2 })
	time.Sleep(3 * time.Second)
	if n := len(p.events("orch_down", "orch")); n != 1 {
		t.Errorf("orch_down events = %d, want 1", n)
	}
	// The escalation clears itself once the restarted orchestrator makes progress, so
	// check the user flag on the event.
	if d := p.events("orch_down", "orch")[0].Data; !strings.Contains(d, `"user":true`) {
		t.Errorf("orch_down must reach the user: %s", d)
	}
	if n := len(p.messagesTo("orch", "Resume brief")); n != 1 {
		t.Errorf("resume briefs = %d, want 1", n)
	}
	if n := len(p.messagesTo("eng-1", "orch")); n != 0 {
		t.Errorf("the user-level escalation must not be routed through other agents: %d", n)
	}
}

// 5. A killed supervisor is reported by `slai doctor`, then respawned by the next Stop hook.
func TestSelfHealSupervisorRespawn(t *testing.T) {
	p := newProject(t, healOpts(nil))
	p.up()
	if out, code := p.run("", "doctor"); code != 0 {
		t.Fatalf("doctor should be green on a healthy team (exit %d):\n%s", code, out)
	}
	old := p.supervisorPid()
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	p.waitFor(20*time.Second, "doctor to notice the dead supervisor", func() bool {
		out, code := p.run("", "doctor")
		return code != 0 && strings.Contains(out, "supervisor not running")
	})
	// Type into an agent so its fake runs a turn and fires the Stop hook.
	if out, err := exec.Command("tmux", "-L", p.sock, "send-keys", "-t", "="+p.session("orch")+":", "hello", "Enter").CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v %s", err, out)
	}
	p.waitFor(healTimeout, "supervisor respawned", func() bool { return len(p.events("supervisor_respawn", "")) > 0 })
	p.waitFor(healTimeout, "doctor green again", func() bool {
		_, code := p.run("", "doctor")
		return code == 0
	})
	if np := p.supervisorPid(); np == old {
		t.Errorf("pidfile still names the killed supervisor %d", old)
	}
}

// 6. The original bug: a fresh team with no tasks is idle, never stale.
func TestSelfHealBoardTruthFreshTeam(t *testing.T) {
	p := newProject(t, healOpts(nil))
	p.up()
	p.waitFor(30*time.Second, "all agents up", func() bool {
		for _, a := range p.status().Agents {
			if a.Status == "dead" {
				return false
			}
		}
		return true
	})
	out, code := p.run("orch", "tool", "get_board", "{}")
	if code != 0 {
		t.Fatalf("get_board: %s", out)
	}
	var board struct {
		Agents []struct{ ID, Liveness, Reason string }
		Tasks  []any
	}
	if err := json.Unmarshal([]byte(out), &board); err != nil {
		t.Fatalf("board json: %v\n%s", err, out)
	}
	if len(board.Agents) != 4 || len(board.Tasks) != 0 {
		t.Fatalf("board = %+v", board)
	}
	for _, a := range board.Agents {
		if a.Liveness == "stale" || a.Liveness == "dead" {
			t.Errorf("%s is %s (%s) on a fresh team", a.ID, a.Liveness, a.Reason)
		}
		if a.Liveness != "idle" && a.Liveness != "alive" {
			t.Errorf("%s liveness = %q, want idle", a.ID, a.Liveness)
		}
	}
}

// 7. A wake line the pane swallowed (no UserPromptSubmit hook follows) is retyped once and the work gets done.
func TestSelfHealWakeLanded(t *testing.T) {
	p := newProject(t, healOpts(map[string]string{"FAKECLAUDE_SWALLOW_ENG_1": "1"}))
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(healTimeout, "eng-1 swallowed a wake", func() bool { return len(p.fakeRecords("swallowed", "eng-1")) > 0 })
	p.waitFor(healTimeout, "wake_retyped", func() bool { return len(p.events("wake_retyped", "eng-1")) > 0 })
	p.waitFor(healTimeout, "tasks merged", func() bool { return p.allMerged(2) })
	if n := len(p.events("wake_retyped", "eng-1")); n != 1 {
		t.Errorf("wake_retyped events = %d, want exactly 1", n)
	}
	if n := len(p.events("wake_not_landed", "eng-1")); n != 0 {
		t.Errorf("wake_not_landed = %d: the retype should have landed", n)
	}
}
