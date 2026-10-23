// Package e2e drives the whole slai loop (supervisor, tmux, hooks, tools, git)
// with scripted fake Claude agents; no tokens are spent.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/tmux"
)

var (
	binDir   string // holds slai and claude (= fakeclaude)
	repoRoot string // slai source tree
	skipWhy  string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	for _, b := range []string{"tmux", "git", "go"} {
		if _, err := exec.LookPath(b); err != nil {
			skipWhy = b + " not found on PATH"
			return m.Run()
		}
	}
	_, file, _, _ := runtime.Caller(0)
	repoRoot = filepath.Join(filepath.Dir(file), "..", "..")
	var err error
	if binDir, err = os.MkdirTemp("", "slai-e2e-bin-"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(binDir)
	for out, pkg := range map[string]string{"slai": ".", "claude": "./testdata/fakeclaude", "pi": "./testdata/fakeharness", "opencode": "./testdata/fakeharness"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, out), pkg)
		cmd.Dir = repoRoot
		if b, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, b)
			return 1
		}
	}
	code := m.Run()
	// Anything still running from this run's binaries outlived its test.
	if desc := reapLeaks(2*time.Second, binDir); len(desc) > 0 {
		fmt.Fprintf(os.Stderr, "e2e: %d process(es) outlived the run (killed now):\n  %s\n", len(desc), strings.Join(desc, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	return code
}

// project is one isolated slai project plus its private tmux server.
type project struct {
	t       *testing.T
	dir     string // user's repo checkout
	sock    string
	env     []string
	fakeLg  string // FAKECLAUDE_LOG
	mainSHA string
}

type opts struct {
	env     map[string]string // extra env for slai and its agents
	engines int               // engineers (default 2)
	// harnesses maps agent id -> harness kind ("pi", "opencode"); those agents run the
	// fake of that harness (testdata/fakeharness) instead of fakeclaude.
	harnesses map[string]string
}

func newProject(t *testing.T, o opts) *project {
	t.Helper()
	if skipWhy != "" {
		t.Skip(skipWhy)
	}
	t.Cleanup(acquireSlot(t)) // registered first, so it runs last: the slot is held through teardown
	tmp := t.TempDir()
	p := &project{t: t, dir: filepath.Join(tmp, "repo"), sock: fmt.Sprintf("slai-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)}
	p.fakeLg = filepath.Join(tmp, "fakeclaude.log")
	p.env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLAI_TMUX_SOCKET="+p.sock,
		"SLAI_NO_TRUST_GATE=1",
		"SLAI_NO_DRIVER=1",
		"SLAI_NO_ORPHAN_SCAN=1", // doctor must not judge the host's real teams
		"FAKECLAUDE_AUTOPILOT=1",
		"FAKECLAUDE_LOG="+p.fakeLg,
		"FAKEHARNESS_LOG="+p.fakeLg, // same file and record shape: the helpers read both
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"SLAI_AGENT_ID=", "SLAI_PROJECT=", // never inherit a real agent identity
	)
	for k, v := range o.env {
		p.env = append(p.env, k+"="+v)
	}
	// copy sample repo and make it a git repo on main
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", filepath.Join(repoRoot, "testdata", "sample-repo")+"/.", p.dir).CombinedOutput(); err != nil {
		t.Fatalf("copy sample: %v %s", err, out)
	}
	p.sh("git", "init", "-q", "-b", "main")
	p.sh("git", "add", "-A")
	p.sh("git", "commit", "-q", "-m", "initial")
	p.mainSHA = strings.TrimSpace(p.sh("git", "rev-parse", "main"))

	t.Cleanup(p.cleanup)
	p.slai("init", "--yes", "--preset", "squad", "--name", "sample")
	// trim to orch + principal + N engineers
	teamPath := filepath.Join(p.dir, ".slai", "team.yaml")
	team, err := config.Load(teamPath)
	if err != nil {
		t.Fatal(err)
	}
	n := o.engines
	if n == 0 {
		n = 2
	}
	var keep []config.Agent
	engs := 0
	for _, a := range team.Agents {
		switch a.Role {
		case config.RoleOrchestrator, config.RolePrincipal:
			keep = append(keep, a)
		case config.RoleEngineer:
			if engs < n {
				keep = append(keep, a)
				engs++
			}
		}
	}
	for i := range keep {
		if k, ok := o.harnesses[keep[i].ID]; ok && k != "" && k != config.HarnessClaude {
			keep[i].Harness, keep[i].Model = k, "fake/model"
		}
	}
	team.Agents = keep
	team.Defaults.MaxConcurrentAgents = 8
	if err := config.Write(teamPath, team); err != nil {
		t.Fatal(err)
	}
	return p
}

// cleanup stops the daemon, kills the private tmux server and runs `slai down`.
func (p *project) cleanup() {
	slai := filepath.Join(binDir, "slai")
	cmd := exec.Command(slai, "down")
	cmd.Dir, cmd.Env = p.dir, p.env
	_ = cmd.Run()
	if b, err := os.ReadFile(filepath.Join(p.dir, ".slai", "run", "supervisor.pid")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	k := exec.Command("tmux", "-L", p.sock, "kill-server")
	_ = k.Run()
	tmux.RemoveSocket(p.sock)
	p.auditLeaks()
}

func (p *project) sh(name string, args ...string) string {
	p.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = p.dir, p.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		p.t.Fatalf("%s %v: %v\n%s%s", name, args, err, out.String(), errb.String())
	}
	return out.String()
}

// git runs git in dir (the user's repo when dir is empty), returning output and error.
func (p *project) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = p.dir, p.env
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (p *project) mustGit(args ...string) string {
	p.t.Helper()
	out, err := p.git(args...)
	if err != nil {
		p.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (p *project) slai(args ...string) string {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "slai"), args...)
	cmd.Dir, cmd.Env = p.dir, p.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		p.t.Fatalf("slai %v: %v\n%s%s", args, err, out.String(), errb.String())
	}
	return out.String()
}

func (p *project) up() { p.t.Helper(); p.slai("up") }

func (p *project) db() *store.Store {
	p.t.Helper()
	st, err := store.Open(filepath.Join(p.dir, ".slai", "state.db"))
	if err != nil {
		p.t.Fatal(err)
	}
	return st
}

// events returns all events of a kind (oldest first) optionally for one agent.
func (p *project) events(kind, agent string) []store.Event {
	p.t.Helper()
	st := p.db()
	defer st.Close()
	evs, err := st.EventsAfter(0, kind, 10000)
	if err != nil {
		p.t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if agent == "" || e.Agent == agent {
			out = append(out, e)
		}
	}
	return out
}

func (p *project) task(id string) store.Task {
	p.t.Helper()
	st := p.db()
	defer st.Close()
	tk, err := st.GetTask(id)
	if err != nil {
		p.t.Fatalf("task %s: %v", id, err)
	}
	return tk
}

func (p *project) tasks() []store.Task {
	p.t.Helper()
	st := p.db()
	defer st.Close()
	ts, err := st.ListTasks("")
	if err != nil {
		p.t.Fatal(err)
	}
	return ts
}

func (p *project) agentStatus(id string) string {
	st := p.db()
	defer st.Close()
	a, err := st.AgentByID(id)
	if err != nil {
		return ""
	}
	return a.Status
}

// waitFor polls cond until it returns true or the timeout passes.
func (p *project) waitFor(timeout time.Duration, what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.dump()
	p.t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// dump prints diagnostic state on failure.
func (p *project) dump() {
	p.t.Helper()
	for _, t := range p.tasks() {
		p.t.Logf("task %s %q assignee=%s status=%s rounds=%d", t.ID, t.Title, t.Assignee, t.Status, t.ReviewRounds)
	}
	st := p.db()
	defer st.Close()
	if evs, err := st.RecentEvents(60); err == nil {
		for i := len(evs) - 1; i >= 0; i-- {
			if !strings.HasPrefix(evs[i].Kind, "hook:") {
				p.t.Logf("event %d %s %s %s", evs[i].ID, evs[i].Agent, evs[i].Kind, evs[i].Data)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(p.dir, ".slai", "run", "supervisor.log")); err == nil {
		p.t.Logf("supervisor.log:\n%s", b)
	}
	if b, err := os.ReadFile(p.fakeLg); err == nil {
		lines := strings.Split(string(b), "\n")
		if len(lines) > 80 {
			lines = lines[len(lines)-80:]
		}
		p.t.Logf("fakeclaude.log (tail):\n%s", strings.Join(lines, "\n"))
	}
}

func (p *project) summaryPath() string {
	return filepath.Join(p.dir, ".slai", "run", "orch-summary.txt")
}

func (p *project) fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

type statusJSON struct {
	Agents []struct{ ID, Role, Status string }
	Tasks  []struct{ ID, Status, Assignee string }
}

func (p *project) status() statusJSON {
	p.t.Helper()
	var s statusJSON
	if err := json.Unmarshal([]byte(p.slai("status", "--json")), &s); err != nil {
		p.t.Fatal(err)
	}
	return s
}

// allMerged reports whether tasks exist and every one is merged.
func (p *project) allMerged(n int) bool {
	ts := p.tasks()
	if len(ts) != n {
		return false
	}
	for _, t := range ts {
		if t.Status != store.StatusMerged {
			return false
		}
	}
	return true
}

// integrationFile reads path on the integration branch ("" , false if absent).
func (p *project) integrationFile(path string) (string, bool) {
	out, err := p.git("show", "slai/integration:"+path)
	return out, err == nil
}

// mainUntouched asserts the user's main branch and checkout are unchanged by the run.
func (p *project) mainUntouched() {
	p.t.Helper()
	if got := strings.TrimSpace(p.mustGit("rev-parse", "main")); got != p.mainSHA {
		p.t.Errorf("main moved: %s -> %s", p.mainSHA, got)
	}
	if got := strings.TrimSpace(p.mustGit("rev-parse", "--abbrev-ref", "HEAD")); got != "main" {
		p.t.Errorf("user checkout switched to %s", got)
	}
	for _, f := range []string{"feature-1.txt", "feature-2.txt", "shared.txt"} {
		if p.fileExists(filepath.Join(p.dir, f)) {
			p.t.Errorf("%s leaked into the user's checkout", f)
		}
	}
	for _, l := range strings.Split(p.mustGit("status", "--porcelain"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasSuffix(l, ".gitignore") || strings.Contains(l, ".slai") {
			continue
		}
		p.t.Errorf("unexpected change in user checkout: %q", l)
	}
}

// fakeStarts returns the logged "start" records for an agent.
func (p *project) fakeStarts(agent string) []map[string]any {
	b, _ := os.ReadFile(p.fakeLg)
	var out []map[string]any
	for _, l := range strings.Split(string(b), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(l), &r) == nil && r["event"] == "start" && r["slai_agent_id"] == agent {
			out = append(out, r)
		}
	}
	return out
}
