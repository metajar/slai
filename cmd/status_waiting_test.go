package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

// waitMail leaves n unread messages for agent and records the wake-skip reason the supervisor
// would have written (kv wake.skip.<agent> = reason|since).
func waitMail(t *testing.T, st *store.Store, agent, reason string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := st.AddMessage(store.Message{From: "orch", To: agent, Kind: "note", Body: "look"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetKV("wake.skip."+agent, fmt.Sprintf("%s|%d", reason, time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
}

// A golden sentence per reason mail can be waiting (H-4).
func TestWaitingReasonWording(t *testing.T) {
	_, st := groundProject(t)
	for reason, want := range map[string]string{
		supervisor.SkipAttached:   "eng-1: 2 messages waiting, a tmux client is attached; detach or press enter in its pane",
		supervisor.SkipFocus:      "eng-1: 2 messages waiting, you are typing into it in the dashboard; it gets them when you leave its pane",
		supervisor.SkipModal:      "eng-1: 2 messages waiting, it is at a permission or trust prompt only you can answer: `slai attach eng-1`",
		supervisor.SkipHeld:       "eng-1: 2 messages waiting, slai is holding its wake-ups (paused, team hold, spend cap, loop guard or too many unanswered wakes): `slai resume eng-1` lifts a pause, `slai doctor` says which",
		supervisor.SkipSlot:       "eng-1: 2 messages waiting, it waits for a free slot (max_concurrent_agents is full)",
		supervisor.SkipPrepFailed: "eng-1: 2 messages waiting, its worktree could not switch to the assignment's branch; slai retries every tick (`slai doctor` shows the error)",
		supervisor.SkipBusy:       "eng-1: 2 messages waiting, it is mid-turn; they are delivered when the turn ends",
	} {
		_ = st.DeleteKV("wake.skip.eng-1")
		msgs, _ := st.UndeliveredFor("eng-1")
		waitMail(t, st, "eng-1", reason, 2-len(msgs))
		w := waitingFor(st, "eng-1", "orch", true)
		if w == nil || w.Reason != reason || w.Messages != 2 || w.Text != want || w.Since == 0 {
			t.Errorf("%s: got %+v\nwant text %q", reason, w, want)
		}
	}
	if w := waitingFor(st, "orch", "orch", true); w != nil {
		t.Fatalf("an agent with nothing waiting: %+v", w)
	}
	// mail with no recorded reason (it is about to be woken) says nothing
	_ = st.DeleteKV("wake.skip.eng-1")
	if w := waitingFor(st, "eng-1", "orch", true); w != nil {
		t.Fatalf("no reason recorded: %+v", w)
	}
	// one message reads "1 message"
	_, st2 := groundProject(t)
	waitMail(t, st2, "eng-1", supervisor.SkipAttached, 1)
	if w := waitingFor(st2, "eng-1", "orch", true); w == nil || !strings.HasPrefix(w.Text, "eng-1: 1 message waiting,") {
		t.Fatalf("%+v", w)
	}
}

// The first-run trust prompt: the likeliest reason a fresh project "just hangs".
func TestWaitingForTrustPrompt(t *testing.T) {
	_, st := groundProject(t)
	if err := st.SetKV(supervisor.KVPendingStart, "eng-1"); err != nil {
		t.Fatal(err)
	}
	w := waitingFor(st, "orch", "orch", true)
	if w == nil || w.Reason != "trust" || w.Text != "orch is waiting for you to trust this folder: `slai attach orch`, choose \"Yes, I trust this folder\" (the other agents start once you do)" {
		t.Fatalf("orch: %+v", w)
	}
	w = waitingFor(st, "eng-1", "orch", false)
	if w == nil || w.Reason != "trust" || w.Text != "eng-1 has not started: it waits for you to trust this folder in orch's pane (`slai attach orch`)" {
		t.Fatalf("eng-1: %+v", w)
	}
	text := runStatus(t)
	if !strings.Contains(text, "  eng-1 has not started: it waits for you to trust this folder in orch's pane (`slai attach orch`)\n") {
		t.Fatalf("status text:\n%s", text)
	}
	_ = st.SetKV(supervisor.KVPendingStart, "") // trust recorded: the rest started
	if w := waitingFor(st, "eng-1", "orch", true); w != nil {
		t.Fatalf("still waiting for trust: %+v", w)
	}
}

func TestStatusShowsWaitingAndWhoIsWorking(t *testing.T) {
	_, st := groundProject(t)
	// nothing registered yet: no "Who is working" block, plain lines (the never-run goldens)
	if got := runStatus(t); strings.Contains(got, "Who is working") || strings.Contains(got, "waiting") {
		t.Fatalf("a team that never ran:\n%s", got)
	}
	for _, a := range []struct{ id, role string }{{"orch", "orchestrator"}, {"eng-1", "engineer"}} {
		if err := st.UpsertAgent(store.Agent{ID: a.id, Role: a.role, Model: "opus", TmuxSession: "slai_gone_" + a.id}); err != nil {
			t.Fatal(err)
		}
	}
	waitMail(t, st, "eng-1", supervisor.SkipAttached, 2)
	text := runStatus(t)
	if !strings.HasPrefix(text, "Who is working:\n") {
		t.Fatalf("the block comes first:\n%s", text)
	}
	for _, want := range []string{"  orch         dead", "  eng-1        dead", "eng-1: 2 messages waiting, a tmux client is attached; detach or press enter in its pane"} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	c := statusCmd()
	c.SetArgs([]string{"--json"})
	out := captureStdout(t, func() {
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	var s struct {
		Who    []statusWho
		Agents []statusAgent
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(s.Who) != 2 || s.Who[0].ID != "orch" || s.Who[0].State == "" {
		t.Fatalf("who = %+v", s.Who)
	}
	for _, a := range s.Agents {
		if a.ID == "eng-1" && (a.Waiting == nil || a.Waiting.Reason != "attached" || a.Waiting.Messages != 2) {
			t.Fatalf("eng-1 waiting = %+v", a.Waiting)
		}
		if a.ID == "orch" && a.Waiting != nil {
			t.Fatalf("orch waiting = %+v", a.Waiting)
		}
	}
}
