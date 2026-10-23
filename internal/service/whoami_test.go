package service

import (
	"strings"
	"testing"
	"time"
)

// whoami answers from the connection's caller: a conversation that claims another id, or
// an argument naming one, changes nothing.
func TestWhoamiReturnsTheTrueID(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	for _, who := range []struct{ id, role string }{{"orch", "orchestrator"}, {"prin", "principal_engineer"}, {"eng-1", "engineer"}} {
		for _, args := range []string{``, `{}`, `{"agent_id":"orch"}`, `{"id":"fg","role":"focus_group"}`} {
			out := j(must(s.Call(ctx, who.id, "whoami", []byte(args))))
			if out["id"] != who.id || out["role"] != who.role {
				t.Fatalf("%s with args %q: %v", who.id, args, out)
			}
			if !strings.HasPrefix(out["identity"].(string), "You are "+who.id+", the "+who.role+" of this team") {
				t.Fatalf("identity = %v", out["identity"])
			}
			if out["responsibilities"] == "" || out["reports_to"] == "" || len(out["teammates"].([]any)) != 4 {
				t.Fatalf("incomplete: %v", out)
			}
		}
	}
}

func TestWhoamiShowsTheCurrentTask(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := createTask(t, s)
	must(s.GetTask(ctx, "eng-1", GetTaskArgs{TaskID: id}))
	out := j(must(s.Whoami(ctx, "eng-1")))
	task, ok := out["current_task"].(map[string]any)
	if !ok || task["id"] != id || task["title"] == "" {
		t.Fatalf("current_task = %v", out["current_task"])
	}
	if idle := j(must(s.Whoami(ctx, "fg"))); idle["current_task"] != nil {
		t.Fatalf("fg has no task: %v", idle["current_task"])
	}
}

// A pong needs nothing from the agent's shell: no agent_id, still alive, and the ping body
// no longer asks for one.
func TestPongWithoutShellEnv(t *testing.T) {
	curT = t
	old := pingPoll
	pingPoll = 10 * time.Millisecond
	t.Cleanup(func() { pingPoll = old })
	s := newSvc(t, true)
	answerPing(t, s, "eng-1", "eng-1", "") // the agent's shell has no SLAI_AGENT_ID to report
	r := j(must(s.Ping(ctx, "orch", PingArgs{Agent: "eng-1", TimeoutSeconds: 5})))
	if r["result"] != "alive" {
		t.Fatalf("ping answered without agent_id: %v", r)
	}
	for _, x := range must(s.Store.UndeliveredFor("eng-1")) {
		if x.Kind == "ping" && strings.Contains(x.Body, "SLAI_AGENT_ID") {
			t.Fatalf("ping body still asks for the shell env: %q", x.Body)
		}
	}
}
