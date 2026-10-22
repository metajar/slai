package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func TestRealEventFormats(t *testing.T) {
	team := config.Preset("squad", "p", "", "main")
	c := logCtx{team: team, tasks: []store.Task{{ID: "T-0004", Title: "Add Stripe webhook", Assignee: "eng-1"}}}
	cases := []struct {
		e    store.Event
		want string
	}{
		{store.Event{Agent: "eng-1", Kind: "request_review", Data: `{"task_id":"T-0004","summary":"done"}`}, "eng-1 → principal: request_review T-0004 done"},
		{store.Event{Agent: "orch", Kind: "create_task", Data: `{"assignee":"eng-1","title":"Add Stripe webhook"}`}, "orch → eng-1: create_task T-0004 Add Stripe webhook"},
		{store.Event{Agent: "orch", Kind: "send_message", Data: `{"to":"eng-1","body":"hi"}`}, "orch → eng-1: send_message hi"},
		{store.Event{Agent: "eng-1", Kind: "merged", Data: `{"task_id":"T-0004","branch":"b"}`}, "supervisor: merged T-0004 into slai/integration"},
		{store.Event{Agent: "eng-1", Kind: "wake", Data: `{"count":2,"message_ids":[1,2]}`}, "supervisor → eng-1: wake (2 message(s))"},
		{store.Event{Kind: "spend_warn", Data: `{"scope":"daily","spent_usd":8,"cap_usd":10}`}, "supervisor: spend_warn daily $8.00 of $10.00"},
		{store.Event{Agent: "eng-1", Kind: "ask_user", Data: `{"question":"which db?"}`}, "eng-1 → user: ask_user which db?"},
	}
	for _, tc := range cases {
		got := c.format(tc.e)
		if got != tc.want && !(tc.e.Kind == "merged" && strings.HasPrefix(got, "supervisor: merged T-0004 into ")) {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestLogHidesHooks(t *testing.T) {
	ev := []store.Event{{Agent: "a", Kind: "hook:stop", Data: "{}"}, {Agent: "a", Kind: "report", Data: `{"task_id":"T-1","status":"done"}`}}
	if out := strings.Join(renderLog(ev, logCtx{}, false, 80, 10), "\n"); strings.Contains(out, "hook:") || !strings.Contains(out, "report") {
		t.Fatal(out)
	}
	if out := strings.Join(renderLog(ev, logCtx{}, true, 80, 10), "\n"); !strings.Contains(out, "hook:stop") {
		t.Fatal(out)
	}
}

func TestHistoryAndHeader(t *testing.T) {
	s := openStore(t)
	tk, _ := s.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch"})
	id, _ := s.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "assignment", TaskID: tk.ID, Body: "do it"})
	_, _ = s.AddMessage(store.Message{From: "eng-1", To: "orch", Kind: "note", ReplyTo: id, Body: "ok replying"})
	_, _ = s.AddMessage(store.Message{From: "a", To: "b", Kind: "note", Body: "unrelated"})
	msgs, err := s.MessagesForTask(tk.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("%v %+v", err, msgs)
	}
	h := strings.Join(buildHistory(nil, msgs, logCtx{}, tk.ID), "\n")
	if !strings.Contains(h, "do it") || !strings.Contains(h, "ok replying") || !strings.Contains(h, "reply to") {
		t.Fatal(h)
	}
	if r, err := s.RecentMessages(10); err != nil || len(r) != 3 {
		t.Fatal(err, r)
	}
	ag := []store.Agent{{ID: "eng-2", Status: "waiting_on_user"}}
	b := attentionBanner(ag, []store.Event{{Agent: "eng-2", Kind: "ask_user", Data: `{"question":"which db?"}`}})
	if !strings.Contains(b, "eng-2") || !strings.Contains(b, "which db?") {
		t.Fatal(b)
	}
	if got := headerStyled(ag, 9, 10, true); !strings.Contains(got, "$9.00/$10.00") || !strings.Contains(got, "PAUSED") {
		t.Fatal(got)
	}
}

func TestSystemBanner(t *testing.T) {
	now := time.Unix(10_000, 0)
	ev := func(at int64, agent string) store.Event {
		return store.Event{At: at, Agent: agent, Kind: "ask_user", Data: `{"reason":"spend_cap","question":"daily cap hit"}`}
	}
	if got := systemBanner([]store.Event{ev(9_900, "")}, now); !strings.Contains(got, "daily cap hit") {
		t.Errorf("fresh system ask missing: %q", got)
	}
	if got := systemBanner([]store.Event{ev(9_900, "eng-1")}, now); got != "" {
		t.Errorf("agent ask must not show here: %q", got)
	}
	if got := systemBanner([]store.Event{ev(10_000-3600, "")}, now); got != "" {
		t.Errorf("stale ask should expire: %q", got)
	}
}

func TestFinishedRunShowsInBanner(t *testing.T) {
	m, s := newModel(t)
	_ = s.SetKV("run.done", "Built revlines on slai/integration.\nRun: go run .")
	m.Update(m.refreshCmd()())
	got := ""
	for _, l := range m.bannerLines() {
		got += ansi.Strip(l)
	}
	if !strings.Contains(got, "run finished: Built revlines") {
		t.Fatalf("finished run not shown: %q", got)
	}
}
