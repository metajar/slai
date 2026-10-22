package store

import (
	"testing"
	"time"
)

func TestKVAndPaused(t *testing.T) {
	s, _ := open(t)
	if _, ok, _ := s.GetKV("k"); ok {
		t.Fatal("unset")
	}
	s.SetKV("k", "1")
	s.SetKV("k", "2")
	if v, ok, _ := s.GetKV("k"); !ok || v != "2" {
		t.Fatal(v)
	}
	s.UpsertAgent(Agent{ID: "a"})
	s.SetAgentPaused("a", true)
	if p, _ := s.PausedAgents(); !p["a"] {
		t.Fatal("paused")
	}
	if ok, _ := s.CompareAndSetStatus("a", "working", "idle"); !ok {
		t.Fatal("cas")
	}
	if ok, _ := s.CompareAndSetStatus("a", "idle", "dead"); ok {
		t.Fatal("cas should fail")
	}
}

func TestMessagePairCountsIgnoresBroadcastFanOut(t *testing.T) {
	s, _ := open(t)
	// one broadcast from orch to 4 agents in the same second, plus 2 real DMs
	for _, to := range []string{"pm", "principal", "eng-1", "eng-2"} {
		s.AddMessage(Message{From: "orch", To: to, Kind: "note", Body: "everyone: hello"})
	}
	s.AddMessage(Message{From: "orch", To: "pm", Kind: "note", Body: "dm one"})
	s.AddMessage(Message{From: "pm", To: "orch", Kind: "note", Body: "dm two"})
	pc, err := s.MessagePairCounts(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pc {
		if p.A == "orch" && p.B == "pm" && p.Count != 2 {
			t.Errorf("orch/pm count = %d, want 2 (broadcast copy must not count)", p.Count)
		}
	}
}

func TestWorkingTaskPicksWhatTheAgentIsOn(t *testing.T) {
	type tk struct {
		status string
		age    int64 // seconds since last update (bigger = older)
	}
	cases := []struct {
		name  string
		tasks []tk // created in order T-0001.., all assigned to eng-2
		want  string
	}{
		{"nothing", nil, ""},
		{"only terminal and blocked", []tk{{StatusMerged, 5}, {StatusBlocked, 4}, {StatusDone, 3}, {StatusCancelled, 2}}, ""},
		{"in_progress beats a newer assigned", []tk{{StatusInProgress, 100}, {StatusAssigned, 1}}, "T-0001"},
		{"the live bug: a newer blocked and a newer merged task are ignored", []tk{{StatusInProgress, 500}, {StatusBlocked, 20}, {StatusMerged, 10}}, "T-0001"},
		{"in_progress > changes_requested > in_review > assigned", []tk{{StatusAssigned, 1}, {StatusInReview, 2}, {StatusChangesRequested, 3}}, "T-0003"},
		{"in_review > assigned", []tk{{StatusAssigned, 1}, {StatusInReview, 9}}, "T-0002"},
		{"ties go to the most recently updated", []tk{{StatusInProgress, 50}, {StatusInProgress, 10}, {StatusInProgress, 90}}, "T-0002"},
		{"equal timestamps: the later task", []tk{{StatusAssigned, 5}, {StatusAssigned, 5}}, "T-0002"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := open(t)
			defer s.Close()
			base := time.Now().Unix() - 10000
			for _, x := range c.tasks {
				created, err := s.CreateTask(Task{Title: "t", Assignee: "eng-2", CreatedBy: "orch", Status: x.status})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`UPDATE tasks SET updated_at=? WHERE id=?`, base+10000-x.age, created.ID); err != nil {
					t.Fatal(err)
				}
			}
			// someone else's task never counts
			_, _ = s.CreateTask(Task{Title: "other", Assignee: "eng-1", CreatedBy: "orch", Status: StatusInProgress})
			if got, err := s.WorkingTask("eng-2"); err != nil || got != c.want {
				t.Fatalf("WorkingTask = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestWorkingTaskPrefersTheTaskJustOpenedWithGetTask(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	a, _ := s.CreateTask(Task{Title: "a", Assignee: "eng-2", CreatedBy: "orch", Status: StatusInProgress})
	b, _ := s.CreateTask(Task{Title: "b", Assignee: "eng-2", CreatedBy: "orch", Status: StatusInReview})
	old := time.Now().Unix() - 1000
	_, _ = s.db.Exec(`UPDATE tasks SET updated_at=?`, old)
	if got, _ := s.WorkingTask("eng-2"); got != a.ID {
		t.Fatalf("no get_task yet: %q, want %s", got, a.ID)
	}
	_ = s.AddEvent("eng-2", "get_task", `{"task_id":"`+b.ID+`"}`) // looked at the review task after it was last updated
	if got, _ := s.WorkingTask("eng-2"); got != b.ID {
		t.Fatalf("after get_task %s: %q", b.ID, got)
	}
	// a get_task for a task that is not theirs, merged, or older than the update is ignored
	_ = s.AddEvent("eng-2", "get_task", `{"task_id":"T-9999"}`)
	_ = s.AddEvent("eng-2", "get_task", `{}`)
	if got, _ := s.WorkingTask("eng-2"); got != a.ID && got != b.ID {
		t.Fatalf("junk get_task: %q", got)
	}
	_, _ = s.db.Exec(`UPDATE tasks SET updated_at=? WHERE id=?`, time.Now().Unix()+100, a.ID)
	if got, _ := s.WorkingTask("eng-2"); got != a.ID {
		t.Fatalf("a task updated after the get_task should win again: %q", got)
	}
	_ = s.UpdateTaskStatus(a.ID, StatusInReview)
	_ = s.UpdateTaskStatus(a.ID, StatusMerged)
	_ = s.AddEvent("eng-2", "get_task", `{"task_id":"`+a.ID+`"}`)
	if got, _ := s.WorkingTask("eng-2"); got != b.ID {
		t.Fatalf("get_task on a merged task must not count: %q", got)
	}
}

func TestGetAgentsReportsTheWorkingTask(t *testing.T) {
	s, _ := open(t)
	defer s.Close()
	_ = s.UpsertAgent(Agent{ID: "eng-2", Role: "engineer", CurrentTask: "T-0032"}) // stale: the latest assignment
	blocked, _ := s.CreateTask(Task{Title: "x", Assignee: "eng-2", CreatedBy: "orch", Status: StatusBlocked})
	cur, _ := s.CreateTask(Task{Title: "y", Assignee: "eng-2", CreatedBy: "orch", Status: StatusInProgress})
	_ = blocked
	as, err := s.GetAgents()
	if err != nil || len(as) != 1 || as[0].CurrentTask != cur.ID {
		t.Fatalf("GetAgents = %+v %v, want current_task %s", as, err, cur.ID)
	}
	_ = s.UpdateTaskStatus(cur.ID, StatusInReview)
	_ = s.UpdateTaskStatus(cur.ID, StatusMerged)
	if as, _ := s.GetAgents(); as[0].CurrentTask != "" {
		t.Fatalf("nothing qualifies: %q", as[0].CurrentTask)
	}
}
