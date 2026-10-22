package supervisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

// showLines types each line and presses Enter, so every line is a row of the pane.
func showLines(t *testing.T, s *Supervisor, id string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if err := s.Tmux.SendKeys(context.Background(), s.Session(id), l, true); err != nil {
			t.Fatal(err)
		}
		waitPane(t, s, id, l)
	}
}

func lastEventData(t *testing.T, s *Supervisor, agent, kind string) map[string]any {
	t.Helper()
	es, err := s.Store.EventsByKind(agent, kind, 5)
	if err != nil || len(es) == 0 {
		t.Fatalf("no %s event for %s: %v", kind, agent, err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(es[0].Data), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// AC 2: a refusal to type over a prompt is logged with the rule and a bounded excerpt.
func TestSafeToTypeRefusalEmitsPaneDecision(t *testing.T) {
	s, _ := healthEnv(t)
	ctx := context.Background()
	show(t, s, "eng-1", "Do you want to proceed?", "❯ 1. Yes")
	if s.safeToType(ctx, mustAgent(t, s, "eng-1")) {
		t.Fatal("typed over a live prompt")
	}
	d := lastEventData(t, s, "eng-1", "pane_decision")
	if d["rule"] != "claude.modal" || d["action"] != "type_blocked" {
		t.Errorf("pane_decision = %v", d)
	}
	ex, _ := d["excerpt"].(string)
	if !strings.Contains(ex, "Do you want to proceed?") {
		t.Errorf("excerpt does not show the prompt: %q", ex)
	}
	if n := len([]rune(ex)); n == 0 || n > hooks.ExcerptMax {
		t.Errorf("excerpt is %d chars, want 1..%d", n, hooks.ExcerptMax)
	}
	// once per episode, like type_skipped_modal
	s.safeToType(ctx, mustAgent(t, s, "eng-1"))
	if n := countEvents(s, "pane_decision"); n != 1 {
		t.Errorf("pane_decision events = %d, want 1 for one episode", n)
	}
}

// A pane that is merely quoting a prompt is not a refusal and logs nothing.
func TestNoPaneDecisionWhenNothingIsBlocked(t *testing.T) {
	s, _ := healthEnv(t)
	show(t, s, "eng-1", "he said: Do you want to proceed?")
	showLines(t, s, "eng-1", "⏸ manual mode on") // the input box / footer is drawn below: history
	if !s.safeToType(context.Background(), mustAgent(t, s, "eng-1")) {
		t.Fatal("refused to type under an input box")
	}
	if hasEvent(s, "pane_decision") {
		t.Error("pane_decision logged without a refusal")
	}
}

// AC 4: hooks are the primary signal. While a hook event younger than FallbackStale
// exists the pane is not classified, even when the agent's status is old; once no
// hook event is fresh the pane fallback runs.
func TestPaneDetectionIsNotConsultedWhileAHookEventIsFresh(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	calls := 0
	show(t, s, "eng-1", "ENG1MARK") // the other agents' panes are classified too; count only this one
	s.Detect = func(pane string) (string, bool) {
		if strings.Contains(pane, "ENG1MARK") {
			calls++
		}
		return "working", true
	}
	if err := s.Store.SetAgentStatus("eng-1", "idle"); err != nil {
		t.Fatal(err)
	}
	statusAt := time.Now()
	time.Sleep(3300 * time.Millisecond) // the store stamps whole seconds with the real clock
	_ = s.Store.AddEvent("eng-1", "hook:pretooluse", "{}")
	hookAt, _ := s.Store.LastHookEventAt("eng-1")
	if time.Unix(hookAt, 0).Sub(time.Unix(statusAt.Unix(), 0)) < 3*time.Second {
		t.Fatal("the hook event must be newer than the status for this test")
	}
	// status is stale by the supervisor clock, the hook event is not
	*now = time.Unix(hookAt, 0).Add(FallbackStale - 2*time.Second)
	if a, _ := s.Store.AgentByID("eng-1"); now.Sub(time.Unix(a.StatusAt, 0)) <= FallbackStale {
		t.Fatalf("setup: status must be older than FallbackStale (%v)", now.Sub(time.Unix(a.StatusAt, 0)))
	}
	s.statusTick(ctx)
	if calls != 0 {
		t.Fatalf("the pane was classified %d time(s) although a hook event was fresh", calls)
	}
	*now = now.Add(2 * FallbackStale) // now every hook event is stale too
	s.statusTick(ctx)
	if calls == 0 {
		t.Fatal("with no fresh hook event the pane fallback must run")
	}
}

// Claude Code 2.1.307 prints no "esc to interrupt": when hooks go quiet the pane
// fallback must still see a live spinner, keep the agent working, and so never wake
// (type into) it. A finished "Cogitated for … done" line must not look busy.
func TestPaneFallbackKeepsA2_1_307SpinnerWorkingAndWakesNothing(t *testing.T) {
	s, now := healthEnv(t)
	ctx := context.Background()
	showLines(t, s, "eng-1", "✽ Moonwalking… (31s · ↓ 1.7k tokens)")
	if err := s.Store.SetAgentStatus("eng-1", "idle"); err != nil { // a stale hook-derived status
		t.Fatal(err)
	}
	_, _ = s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "hi"})
	before := pane(t, s, "eng-1")
	*now = now.Add(2 * FallbackStale)
	s.statusTick(ctx)
	if got := status(t, s, "eng-1"); got != "working" {
		t.Fatalf("status = %s, want working from the spinner line", got)
	}
	s.wakeTick(ctx)
	time.Sleep(300 * time.Millisecond)
	if after := pane(t, s, "eng-1"); strings.Contains(after, "You have") || after != before {
		t.Fatalf("typed into a working pane:\n%s", after)
	}

	// after the turn the past-tense line is not a live spinner
	s2, _ := healthEnv(t)
	showLines(t, s2, "eng-1", "✻ Cogitated for 4m 40s · done 1:29 AM")
	if d := hooks.ExplainWith(hooks.ClaudeRules(), pane(t, s2, "eng-1")); d.Status == hooks.StatusWorking {
		t.Fatalf("a finished spinner line looks busy: %+v", d)
	}
}
