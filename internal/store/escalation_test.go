package store

import "testing"

func TestEscalationOnce(t *testing.T) {
	s, _ := open(t)
	e := Escalation{Agent: "eng-1", Kind: "rate_limited", Text: "x", Since: 20}
	if r, err := s.RaiseEscalation(e); err != nil || !r {
		t.Fatalf("first raise: %v %v", r, err)
	}
	e2 := e
	e2.Text = "changed"
	if r, _ := s.RaiseEscalation(e2); r {
		t.Fatal("second raise must report false")
	}
	if r, _ := s.RaiseEscalation(Escalation{Agent: "orch", Kind: "orch_down", Since: 10, User: true}); !r {
		t.Fatal("different agent/kind is independent")
	}
	got, _ := s.OpenEscalations()
	if len(got) != 2 || got[0].Agent != "orch" || got[1].Text != "x" || !got[0].User {
		t.Fatalf("open: %+v", got)
	}
	if err := s.ClearEscalation("eng-1", "rate_limited"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.RaiseEscalation(e); !r {
		t.Fatal("clear must re-arm")
	}
}

func TestEscalationAndHeartbeatPersist(t *testing.T) {
	s, p := open(t)
	s.RaiseEscalation(Escalation{Agent: "a", Kind: "k", Since: 5})
	if _, ok, _ := s.SupervisorHeartbeat(); ok {
		t.Fatal("no heartbeat yet")
	}
	s.SetSupervisorHeartbeat(1234)
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if r, _ := s2.RaiseEscalation(Escalation{Agent: "a", Kind: "k"}); r {
		t.Fatal("escalation lost across reopen")
	}
	if n, ok, _ := s2.SupervisorHeartbeat(); !ok || n != 1234 {
		t.Fatalf("heartbeat %d %v", n, ok)
	}
	if es, _ := s2.OpenEscalations(); len(es) != 1 || es[0].Since != 5 {
		t.Fatalf("%+v", es)
	}
}
