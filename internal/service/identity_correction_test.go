package service

import (
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/store"
)

func corrections(t *testing.T, s *Service, agent string) []store.Event {
	t.Helper()
	evs, err := s.Store.EventsByKind(agent, "identity_correction", 20)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// C6 signal (i): calling a tool the role may not use flags the caller and queues its identity
// line, with a whoami instruction, ahead of other inbox items.
func TestRoleDenialQueuesIdentityCorrection(t *testing.T) {
	s := newSvc(t, false)
	if _, err := s.SendMessage(ctx, "orch", SendMessageArgs{To: "eng-1", Body: "ordinary note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, "eng-1", CreateTaskArgs{Title: "x", Description: "y", Assignee: "eng-1"}); err == nil {
		t.Fatal("an engineer created a task")
	}
	evs := corrections(t, s, "eng-1")
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"signal":"role_denial"`) || !strings.Contains(evs[0].Data, "create_task") {
		t.Fatalf("events = %+v", evs)
	}
	inbox := j(must(s.GetInbox(ctx, "eng-1")))["items"].([]any)
	if len(inbox) != 2 {
		t.Fatalf("inbox = %v", inbox)
	}
	first := inbox[0].(map[string]any)
	if first["kind"] != store.IdentityMessageKind || first["from"] != "slai" ||
		!strings.HasPrefix(first["body"].(string), "You are eng-1, the engineer of this team") || !strings.Contains(first["body"].(string), "whoami") {
		t.Fatalf("first inbox item = %v", first)
	}
}

// C6 signal (ii): acting on a task assigned to someone else.
func TestForeignTaskActionQueuesIdentityCorrection(t *testing.T) {
	s := newSvc(t, false)
	s.Team.Agents = append(s.Team.Agents, config.Agent{ID: "eng-2", Role: config.RoleEngineer})
	_ = s.Store.UpsertAgent(store.Agent{ID: "eng-2", Role: config.RoleEngineer})
	id := createTask(t, s) // assigned to eng-1
	if _, err := s.RequestReview(ctx, "eng-2", RequestReviewArgs{TaskID: id, Summary: "mine"}); err == nil {
		t.Fatal("eng-2 requested review of eng-1's task")
	}
	evs := corrections(t, s, "eng-2")
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"signal":"foreign_task"`) || !strings.Contains(evs[0].Data, id) {
		t.Fatalf("events = %+v", evs)
	}
	if ms, _ := s.Store.UndeliveredFor("eng-2"); len(ms) != 1 || !strings.HasPrefix(ms[0].Body, "You are eng-2") {
		t.Fatalf("queued = %+v", ms)
	}
	if got := corrections(t, s, "eng-1"); len(got) != 0 {
		t.Fatalf("the real assignee was flagged: %+v", got)
	}
}

// C6 signal (iii): a message that opens with another agent's id as a signature. Addressing the
// recipient by name or mentioning a teammate mid-sentence is not flagged.
func TestMessageSignedAsAnotherAgentIsFlagged(t *testing.T) {
	s := newSvc(t, false)
	for _, body := range []string{"prin, please look at T-1", "prin: you asked about T-1", "Reviewed by prin yesterday", "orch: this is from me"} {
		if _, err := s.SendMessage(ctx, "eng-1", SendMessageArgs{To: "prin", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	// "orch: ..." sent to prin opens with a teammate (not the recipient): flagged once
	evs := corrections(t, s, "eng-1")
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"signal":"speaks_as_other"`) || !strings.Contains(evs[0].Data, "opens with orch") {
		t.Fatalf("events = %+v", evs)
	}
}
