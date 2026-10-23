package agentrows

import (
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func TestWhoIsWorkingStates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ev := func(agent, kind, data string, ago time.Duration) store.Event {
		return store.Event{Agent: agent, Kind: kind, Data: data, At: now.Add(-ago).Unix()}
	}
	base := func(id, status, liveness string) WhoInput {
		return WhoInput{Agent: store.Agent{ID: id, Role: "engineer", Status: status}, Role: "engineer",
			Health: service.Health{Liveness: liveness, LastActivity: now.Add(-12 * time.Minute).Unix()}, Orch: "orch", Now: now}
	}
	cases := []struct {
		name string
		in   WhoInput
		want string
	}{
		{"working", base("eng-1", "working", "busy"), "working"},
		{"idle nothing owed", base("eng-1", "idle", "idle"), "idle, nothing owed"},
		{"needs you", base("eng-1", "waiting_on_user", "blocked"), "needs you"},
		{"stale", base("eng-1", "idle", "stale"), "stale (silent 12m)"},
		{"gated", func() WhoInput { i := base("eng-1", "idle", "gated"); i.Health.Reason = "team hold"; return i }(), "gated: team hold"},
		{"dead", base("eng-1", "dead", "dead"), "dead"},
		{"rate limited", base("eng-1", "idle", "rate_limited"), "rate-limited"},
		{"idle owing work", func() WhoInput {
			i := base("eng-1", "idle", "idle")
			i.Tasks = []store.Task{{ID: "T-1", Assignee: "eng-1", Status: store.StatusInProgress}, {ID: "T-2", Assignee: "eng-2", Status: store.StatusInProgress}}
			i.Unread = 2
			return i
		}(), "idle, owes 1 task(s), 2 unread"},
	}
	for _, c := range cases {
		if got := BuildWho(c.in).State; got != c.want {
			t.Errorf("%s: state %q, want %q", c.name, got, c.want)
		}
	}
	in := base("eng-1", "working", "busy")
	in.Events = []store.Event{ // newest first
		ev("eng-1", "hook:pretooluse", `{"tool":"Bash"}`, 2*time.Minute),
		ev("eng-1", "nudge", `{}`, 3*time.Minute),
		ev("eng-1", "stuck_restart", `{"cause":"x"}`, 20*time.Minute),
		ev("eng-1", "auto_revive", `{"cause":"x"}`, 40*time.Minute),
		ev("eng-1", "agent_stopped", `{"cause":"x"}`, 41*time.Minute), // a deliberate stop is not a restart
		ev("eng-1", "auto_revive", `{"cause":"x"}`, 3*time.Hour),      // outside the hour
		ev("eng-2", "hook:pretooluse", `{"tool":"Edit"}`, time.Minute),
	}
	in.Messages = []store.Message{{To: "eng-1", From: "orch", Kind: "note", CreatedAt: now.Add(-5 * time.Minute).Unix()}}
	w := BuildWho(in)
	if w.State != "working · restarted 2× in the last hour" || w.Last != "Bash 2m ago" || w.Slai != "slai reminded it 3m ago" {
		t.Fatalf("%+v", w)
	}
	if line := w.Line(); line != "working · restarted 2× in the last hour · last: Bash 2m ago · slai reminded it 3m ago" {
		t.Fatalf("line = %q", line)
	}
	// a newer note from orch beats the older slai action
	in.Messages[0].CreatedAt = now.Add(-time.Minute).Unix()
	if w := BuildWho(in); !strings.HasPrefix(w.Slai, "orch sent a note 1m ago") {
		t.Fatalf("slai = %q", w.Slai)
	}
}
