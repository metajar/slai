package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/roles"
)

// Identity (spec context-reset-and-liveness, C9): after a compaction, a /clear, a restart
// that resumes the session and a restart that starts a fresh one, the orchestrator, the
// principal and an engineer still know their id and role. Scripted agents (testdata/fakeclaude)
// stand in for the model, but everything slai says to them is real: the system prompt on every
// launch, the first line typed after the gap, and the whoami answer.

// typedLines returns every line slai typed into the agent's pane so far, oldest first.
func (p *project) typedLines(agent string) []string {
	var out []string
	for _, r := range p.fakeRecords("input", agent) {
		if l, _ := r["line"].(string); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// typeIntoPane types a line the way a user would (tmux send-keys into the agent's pane).
func (p *project) typeIntoPane(agent, line string) {
	p.t.Helper()
	sess := p.session(agent)
	if out, err := exec.Command("tmux", "-L", p.sock, "send-keys", "-t", "="+sess+":", "-l", line).CombinedOutput(); err != nil {
		p.t.Fatalf("send-keys %s: %v %s", agent, err, out)
	}
	if out, err := exec.Command("tmux", "-L", p.sock, "send-keys", "-t", "="+sess+":", "Enter").CombinedOutput(); err != nil {
		p.t.Fatalf("send-keys Enter %s: %v %s", agent, err, out)
	}
}

// waitIdentityLine waits for a line typed to agent after the first `skip` lines that opens with
// want (the identity sentence) and returns it.
func (p *project) waitIdentityLine(agent, want string, skip int, why string) string {
	p.t.Helper()
	var got string
	p.waitFor(healTimeout, why, func() bool {
		ls := p.typedLines(agent)
		for i := skip; i < len(ls); i++ {
			if strings.HasPrefix(ls[i], "[slai]") || strings.HasPrefix(ls[i], "You are ") {
				got = ls[i]
				return true
			}
		}
		return false
	})
	if !strings.HasPrefix(got, want) {
		p.t.Fatalf("%s: first line typed after %s is\n  %q\nwant it to open with the identity sentence\n  %q", agent, why, got, want)
	}
	return got
}

// assertWhoami checks the whoami tool answers with the agent's true id and role, from slai's
// own record, whatever the caller thinks.
func (p *project) assertWhoami(agent, role string) {
	p.t.Helper()
	var w struct {
		ID, Role, Identity string
	}
	out := p.tool(agent, "whoami", "{}")
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &w); err != nil {
		p.t.Fatalf("whoami as %s: %v\n%s", agent, err, out)
	}
	if !strings.EqualFold(w.ID, agent) || w.Role != role || !strings.Contains(w.Identity, "You are "+agent+", the "+role) {
		p.t.Fatalf("whoami as %s = %+v, want id %s role %s", agent, w, agent, role)
	}
}

// assertSystemPrompt checks the launch carried the role prompt as a system prompt naming the
// agent: it is the channel /clear and compaction cannot remove.
func (p *project) assertSystemPrompt(start map[string]any, agent string) {
	p.t.Helper()
	flags, _ := start["flags"].(map[string]any)
	prompt, _ := flags["--append-system-prompt"].(string)
	if !strings.HasPrefix(prompt, "You are **"+agent+"**, role: ") {
		p.t.Fatalf("%s launch --append-system-prompt = %.120q, want it to open with \"You are **%s**, role: \"", agent, prompt, agent)
	}
}

func (p *project) identityLine(agent string) string {
	p.t.Helper()
	team, err := config.Load(filepath.Join(p.dir, ".slai", "team.yaml"))
	if err != nil {
		p.t.Fatal(err)
	}
	l := roles.IdentityLineFor(team, agent)
	if !strings.HasPrefix(l, "You are "+agent+", the ") {
		p.t.Fatalf("identity line for %s = %q", agent, l)
	}
	return l
}

func TestIdentityAfterGaps(t *testing.T) {
	p := newProject(t, healOpts(nil))
	p.up()
	for _, c := range []struct{ agent, role string }{
		{"orch", config.RoleOrchestrator},
		{"principal", config.RolePrincipal},
		{"eng-1", config.RoleEngineer},
	} {
		identityScenario(t, p, c.agent, c.role) // one project, in turn: it keeps the machine-wide slot count down
	}
}

func identityScenario(t *testing.T, p *project, agent, role string) {
	p.waitFor(30*time.Second, agent+" started", func() bool { return len(p.fakeStarts(agent)) >= 1 })
	ident := p.identityLine(agent)
	if role == config.RoleOrchestrator && !strings.Contains(ident, "never write code") {
		t.Errorf("orchestrator identity line lacks the role guard: %q", ident)
	}
	first := p.fakeStarts(agent)[0]
	p.assertSystemPrompt(first, agent)
	p.assertWhoami(agent, role)
	flags, _ := first["flags"].(map[string]any)
	sid, _ := flags["--session-id"].(string)
	if sid == "" {
		t.Fatalf("%s first launch has no --session-id: %v", agent, flags)
	}
	p.waitFor(30*time.Second, agent+" idle", func() bool { st := p.agentStatus(agent); return st == "idle" || st == "finished" })

	// 1. compaction: same session; the next line typed opens with who the agent is.
	n := len(p.typedLines(agent))
	p.typeIntoPane(agent, "/compact")
	p.waitIdentityLine(agent, ident, n+1, "compaction") // +1: the "/compact" line itself
	p.assertWhoami(agent, role)

	// 2. /clear: a new conversation in the same process.
	p.waitFor(30*time.Second, agent+" idle after compact", func() bool { st := p.agentStatus(agent); return st == "idle" || st == "finished" })
	n = len(p.typedLines(agent))
	p.typeIntoPane(agent, "/clear")
	p.waitIdentityLine(agent, ident, n+1, "/clear")
	p.assertWhoami(agent, role)
	if len(p.events("identity_reground", agent)) < 2 {
		t.Errorf("%s: want identity_reground events for compact and clear, got %d", agent, len(p.events("identity_reground", agent)))
	}

	// 3. user restart that resumes the same session (--resume <id>): the launch carries the system
	// prompt again and the first wake line opens with the identity sentence.
	p.waitFor(30*time.Second, agent+" idle after clear", func() bool { st := p.agentStatus(agent); return st == "idle" || st == "finished" })
	starts, n := len(p.fakeStarts(agent)), len(p.typedLines(agent))
	p.killSession(agent)
	p.slai("up") // the user's restart: starts every agent that is not running, resuming its session
	p.waitFor(healTimeout, agent+" restarted", func() bool { return len(p.fakeStarts(agent)) > starts })
	resumed := p.fakeStarts(agent)[starts]
	p.assertSystemPrompt(resumed, agent)
	if rf, _ := resumed["flags"].(map[string]any); rf["--resume"] != sid {
		t.Errorf("%s revive flags = %v, want --resume %s", agent, rf, sid)
	}
	p.waitIdentityLine(agent, ident, n, "restart/resume")
	p.assertWhoami(agent, role)

	// 4. restart with a fresh session id (the reset path): the session-id file is gone, so
	// the launch uses a new --session-id, and the agent is re-grounded the same way.
	p.waitFor(30*time.Second, agent+" idle after resume", func() bool { st := p.agentStatus(agent); return st == "idle" || st == "finished" })
	if err := os.Remove(filepath.Join(p.dir, ".slai", "run", agent, "session-id")); err != nil {
		t.Fatal(err)
	}
	starts, n = len(p.fakeStarts(agent)), len(p.typedLines(agent))
	p.killSession(agent)
	p.slai("up")
	p.waitFor(healTimeout, agent+" restarted fresh", func() bool { return len(p.fakeStarts(agent)) > starts })
	fresh := p.fakeStarts(agent)[starts]
	p.assertSystemPrompt(fresh, agent)
	ff, _ := fresh["flags"].(map[string]any)
	if nsid, _ := ff["--session-id"].(string); nsid == "" || nsid == sid || ff["--resume"] != nil {
		t.Errorf("%s fresh launch flags = %v, want a new --session-id and no --resume", agent, ff)
	}
	// A fresh conversation starts from the system prompt, which names the agent (asserted
	// above); no wake line is owed until mail arrives, so none is asserted here. Mail sent now
	// reaches an agent that still answers whoami correctly.
	p.assertWhoami(agent, role)
	_ = n
}
