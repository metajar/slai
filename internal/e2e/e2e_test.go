package e2e

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

const loopTimeout = 90 * time.Second

func TestHappyPath(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(loopTimeout, "orch-summary.txt", func() bool { return p.fileExists(p.summaryPath()) })

	if !p.allMerged(2) {
		t.Fatalf("tasks not all merged: %+v", p.tasks())
	}
	sum, _ := os.ReadFile(p.summaryPath())
	if !strings.Contains(string(sum), "T-0001") || !strings.Contains(string(sum), "T-0002") {
		t.Errorf("summary = %q", sum)
	}
	for i, tk := range p.tasks() {
		if tk.ReviewRounds != 0 {
			t.Errorf("task %s review_rounds = %d, want 0", tk.ID, tk.ReviewRounds)
		}
		if want := []string{"eng-1", "eng-2"}[i]; tk.Assignee != want {
			t.Errorf("task %s assignee %s, want %s", tk.ID, tk.Assignee, want)
		}
	}
	// git: both task branches merged with --no-ff into the integration branch.
	log := p.mustGit("log", "slai/integration", "--merges", "--format=%s")
	for _, id := range []string{"[T-0001]", "[T-0002]"} {
		if !strings.Contains(log, id) {
			t.Errorf("integration merge log lacks %s:\n%s", id, log)
		}
	}
	for _, f := range []string{"feature-1.txt", "feature-2.txt"} {
		if _, ok := p.integrationFile(f); !ok {
			t.Errorf("%s missing on slai/integration", f)
		}
		if _, err := p.git("cat-file", "-e", "main:"+f); err == nil {
			t.Errorf("%s present on main", f)
		}
	}
	if !strings.Contains(p.mustGit("log", "slai/integration", "--format=%s", "--no-merges"), "[T-0001] Greeting part 1") {
		t.Errorf("engineer commit message convention not found")
	}
	p.mainUntouched()

	// status --json: live sessions, sane statuses, merged tasks hidden.
	s := p.status()
	if len(s.Agents) != 4 {
		t.Fatalf("agents = %+v", s.Agents)
	}
	p.waitFor(20*time.Second, "all agents idle/finished", func() bool {
		for _, a := range p.status().Agents {
			if a.Status != "idle" && a.Status != "finished" {
				return false
			}
		}
		return true
	})
	if len(p.status().Tasks) != 0 {
		t.Errorf("open tasks should be empty: %+v", p.status().Tasks)
	}

	// events and usage rows
	for _, kind := range []string{"create_task", "request_review", "review_diff", "approve", "merged", "wake", "hook:userpromptsubmit", "hook:stop", "hook:sessionstart"} {
		if len(p.events(kind, "")) == 0 {
			t.Errorf("no %q events", kind)
		}
	}
	st := p.db()
	defer st.Close()
	spend, err := st.TodaySpend()
	if err != nil || spend <= 0 {
		t.Errorf("TodaySpend = %v, %v; want > 0", spend, err)
	}
	for _, id := range []string{"orch", "principal", "eng-1", "eng-2"} {
		if v, _ := st.AgentSpendToday(id); v <= 0 {
			t.Errorf("no usage recorded for %s", id)
		}
		a, _ := st.AgentByID(id)
		if a.Turns == 0 {
			t.Errorf("%s has 0 turns", id)
		}
	}
	// repeated Stop hooks must not double count: usage rows == mark-deltas, so cost per
	// agent is bounded by turns * (cost of 2 usage lines per turn).
	if a, _ := st.AgentByID("orch"); a.Turns > 0 {
		perTurn := 2 * (100*3.0 + 50*15.0 + 1000*0.30 + 200*3.75) / 1e6
		if v, _ := st.AgentSpendToday("orch"); v > float64(a.Turns)*perTurn*1.01 {
			t.Errorf("orch spend %.6f exceeds %d turns * %.6f: double counting?", v, a.Turns, perTurn)
		}
	}
}

func TestReworkRoundTrip(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	p.slai("send", "orch", "build the REWORK feature")
	p.waitFor(loopTimeout, "orch-summary.txt", func() bool { return p.fileExists(p.summaryPath()) })
	if !p.allMerged(2) {
		t.Fatalf("not merged: %+v", p.tasks())
	}
	t1, t2 := p.task("T-0001"), p.task("T-0002")
	if !strings.Contains(t1.Title, "REWORK") || t1.ReviewRounds != 1 {
		t.Errorf("T-0001 %q rounds=%d; want REWORK title and 1 round", t1.Title, t1.ReviewRounds)
	}
	if t2.ReviewRounds != 0 {
		t.Errorf("T-0002 rounds = %d, want 0", t2.ReviewRounds)
	}
	if len(p.events("request_changes", "principal")) != 1 {
		t.Errorf("want exactly one request_changes event")
	}
	if n := len(p.events("request_review", "eng-1")); n != 2 {
		t.Errorf("eng-1 request_review count = %d, want 2", n)
	}
	body, ok := p.integrationFile("feature-1.txt")
	if !ok || !strings.Contains(body, "reworked after review") {
		t.Errorf("feature-1.txt on integration = %q, %v; want reworked content", body, ok)
	}
	p.mainUntouched()
}

func TestMergeConflictResolved(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	p.slai("send", "orch", "build the CONFLICT feature")
	p.waitFor(loopTimeout, "orch-summary.txt", func() bool { return p.fileExists(p.summaryPath()) })
	if !p.allMerged(2) {
		t.Fatalf("not merged: %+v", p.tasks())
	}
	cf := p.events("merge_conflict", "")
	if len(cf) != 1 || !strings.Contains(cf[0].Data, "shared.txt") {
		t.Fatalf("merge_conflict events = %+v", cf)
	}
	loser := cf[0].Agent
	st := p.db()
	defer st.Close()
	msgs, _ := st.RecentMessages(200)
	found := false
	for _, m := range msgs {
		if m.To == loser && m.From == "slai" && strings.Contains(m.Body, "conflicted") {
			found = true
		}
	}
	if !found {
		t.Errorf("conflict note to %s not found", loser)
	}
	body, ok := p.integrationFile("shared.txt")
	if !ok || !strings.Contains(body, "line from eng-1") || !strings.Contains(body, "line from eng-2") || strings.Contains(body, "<<<<") {
		t.Errorf("shared.txt on integration = %q (%v); want both sides, no markers", body, ok)
	}
	p.mainUntouched()
}

func TestMergeConflictUnresolved(t *testing.T) {
	p := newProject(t, opts{env: map[string]string{"FAKECLAUDE_NO_RESOLVE": "1"}})
	p.up()
	p.slai("send", "orch", "build the CONFLICT feature")
	p.waitFor(loopTimeout, "merge_conflict event", func() bool { return len(p.events("merge_conflict", "")) > 0 })
	loserEv := p.events("merge_conflict", "")[0]
	var winner string
	p.waitFor(loopTimeout, "one task merged", func() bool {
		for _, tk := range p.tasks() {
			if tk.Status == store.StatusMerged {
				winner = tk.ID
				return true
			}
		}
		return false
	})
	// give the system time to (wrongly) do anything else
	time.Sleep(4 * time.Second)
	var stuck []store.Task
	for _, tk := range p.tasks() {
		if tk.ID != winner {
			stuck = append(stuck, tk)
		}
	}
	if len(stuck) != 1 || stuck[0].Status != store.StatusInProgress || stuck[0].Assignee != loserEv.Agent {
		t.Errorf("conflicting task = %+v; want in_progress for %s", stuck, loserEv.Agent)
	}
	if p.fileExists(p.summaryPath()) {
		t.Errorf("orchestrator must not summarize while a task is unmerged")
	}
	// the merge was aborted: integration worktree is clean and has no markers
	wt := filepath.Join(p.dir, ".slai", "worktrees", "_integration")
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir, cmd.Env = wt, p.env
	out, _ := cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("integration worktree dirty after aborted merge:\n%s", out)
	}
	if body, _ := p.integrationFile("shared.txt"); strings.Contains(body, "<<<<") {
		t.Errorf("conflict markers on integration: %q", body)
	}
	// engineer received the conflict note (wake delivered, inbox claimed)
	st := p.db()
	defer st.Close()
	msgs, _ := st.RecentMessages(200)
	found := false
	for _, m := range msgs {
		if m.To == loserEv.Agent && strings.Contains(m.Body, "conflicted") && m.DeliveredAt > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("delivered conflict note to %s not found", loserEv.Agent)
	}
	p.mainUntouched()
}

// wakes returns wake events for an agent.
func wakeCount(p *project, agent string) int { return len(p.events("wake", agent)) }

func TestNoWakeWhileWorking(t *testing.T) {
	p := newProject(t, opts{env: map[string]string{"FAKECLAUDE_DELAY_MS_ORCH": "4000"}})
	p.up()
	p.slai("send", "orch", "hello one")
	// The wake flips the status to "working" optimistically, before the agent has
	// run get_inbox. Wait until "hello one" was actually read, or "hello two" could
	// be drained by the same get_inbox and no second wake would ever be needed.
	p.waitFor(20*time.Second, "orch read first message", func() bool {
		st := p.db()
		defer st.Close()
		ms, _ := st.UndeliveredFor("orch")
		return len(ms) == 0 && p.agentStatus("orch") == "working"
	})
	p.slai("send", "orch", "hello two")
	// While the first turn is still running, no second wake may be delivered.
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if p.agentStatus("orch") == "working" && wakeCount(p, "orch") > 1 {
			t.Fatalf("second wake-up delivered while orch was working")
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.waitFor(30*time.Second, "second wake after Stop", func() bool { return wakeCount(p, "orch") >= 2 })
	stops := p.events("hook:stop", "orch")
	wakes := p.events("wake", "orch")
	if len(stops) == 0 || wakes[1].ID < stops[0].ID {
		t.Errorf("second wake (event %d) preceded the first Stop (event %v)", wakes[1].ID, stops)
	}
	// both messages end up delivered
	p.waitFor(30*time.Second, "both messages delivered", func() bool {
		st := p.db()
		defer st.Close()
		ms, _ := st.UndeliveredFor("orch")
		return len(ms) == 0
	})
}

func TestNoWakeWhileAttached(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	session := ""
	st := p.db()
	if a, err := st.AgentByID("orch"); err == nil {
		session = a.TmuxSession
	}
	st.Close()
	if session == "" {
		t.Fatal("no tmux session recorded for orch")
	}
	// A tmux control-mode client counts as an attached client and needs no pty.
	cmd := exec.Command("tmux", "-L", p.sock, "-C", "attach-session", "-t", "="+session)
	cmd.Env = p.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, _ := cmd.StdoutPipe()
	go io.Copy(io.Discard, stdout)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start tmux control client: %v", err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	attached := func() bool {
		out, err := exec.Command("tmux", "-L", p.sock, "list-clients", "-t", session).CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) != ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for !attached() {
		if time.Now().After(deadline) {
			t.Skip("tmux control-mode client did not register as attached on this system")
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.slai("send", "orch", "hello while attached")
	time.Sleep(4 * time.Second)
	if n := wakeCount(p, "orch"); n != 0 {
		t.Fatalf("woke orch %d times while a client was attached", n)
	}
	if p.agentStatus("orch") == "working" {
		t.Fatalf("orch is working although it was never woken")
	}
	// detach: the pending wake must now go through
	stdin.Close()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	p.waitFor(20*time.Second, "wake after detach", func() bool { return wakeCount(p, "orch") >= 1 })
}

func TestRestartSafety(t *testing.T) {
	p := newProject(t, opts{})
	p.up()
	p.slai("send", "orch", "build the greeting feature")
	p.waitFor(loopTimeout, "orch-summary.txt", func() bool { return p.fileExists(p.summaryPath()) })
	before := p.tasks()
	for _, id := range []string{"orch", "principal", "eng-1", "eng-2"} {
		if s := p.fakeStarts(id); len(s) != 1 {
			t.Fatalf("%s starts before restart = %d", id, len(s))
		}
	}

	p.slai("down")
	for _, a := range p.status().Agents {
		if a.Status != "dead" {
			t.Errorf("after down %s is %s, want dead", a.ID, a.Status)
		}
	}
	p.up()
	p.waitFor(20*time.Second, "agents revived", func() bool {
		for _, a := range p.status().Agents {
			if a.Status == "dead" { // pending notes may legitimately wake agents right away
				return false
			}
		}
		return true
	})
	for _, id := range []string{"orch", "principal", "eng-1", "eng-2"} {
		s := p.fakeStarts(id)
		if len(s) != 2 {
			t.Fatalf("%s starts = %d, want 2", id, len(s))
		}
		fl, _ := s[1]["flags"].(map[string]any)
		first, _ := s[0]["flags"].(map[string]any)
		if fl["--continue"] != nil || first["--continue"] != nil {
			t.Errorf("%s used --continue, which resumes whichever conversation in the directory is newest", id)
		}
		sid, _ := first["--session-id"].(string)
		if sid == "" || fl["--resume"] != sid {
			t.Errorf("%s: first launch --session-id=%q, revive --resume=%v (must resume its own session)", id, sid, fl["--resume"])
		}
	}
	// SQLite state persisted across the restart.
	after := p.tasks()
	if len(after) != len(before) {
		t.Fatalf("tasks %d -> %d", len(before), len(after))
	}
	for i := range after {
		if after[i].Status != store.StatusMerged || after[i].ID != before[i].ID {
			t.Errorf("task after restart: %+v", after[i])
		}
	}
	// And the revived team still works: a second request is planned and merged.
	_ = os.Remove(p.summaryPath())
	p.slai("send", "orch", "build another greeting feature")
	p.waitFor(loopTimeout, "second round tasks merged", func() bool { return p.allMerged(4) })
	p.waitFor(20*time.Second, "second summary", func() bool { return p.fileExists(p.summaryPath()) })
	p.mainUntouched()
}

// Reviving after the claude conversation is gone (stale .started marker) must
// still end with live sessions: the launch command falls back to a fresh session.
func TestReviveStaleMarker(t *testing.T) {
	state := t.TempDir()
	p := newProject(t, opts{env: map[string]string{
		"FAKECLAUDE_NO_CONVERSATION_ON_CONTINUE": "1", "FAKECLAUDE_STATE": state}})
	p.up()
	p.slai("down")
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	p.up()
	p.waitFor(20*time.Second, "agents alive after stale-marker revive", func() bool {
		as := p.status().Agents
		for _, a := range as {
			if a.Status == "dead" {
				return false
			}
		}
		return len(as) > 0
	})
	for _, id := range []string{"orch", "principal", "eng-1", "eng-2"} {
		s := p.fakeStarts(id)
		if len(s) < 2 {
			t.Fatalf("%s starts = %d, want continue attempt + fresh fallback", id, len(s))
		}
	}
}
