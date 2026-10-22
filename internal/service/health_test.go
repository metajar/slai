package service

import (
	"strconv"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func TestAgentHealth(t *testing.T) {
	// "now" is well past every timestamp the store stamps in real time, so
	// status changes made by the test count as old activity.
	now := time.Now().Add(3 * StaleAfter)
	old := time.Now().Add(-StaleAfter).Unix()
	cases := []struct {
		name    string
		agent   string // default eng-1
		taskSt  string // status of the task created when task is true (default in_progress)
		alive   bool
		status  string
		lastSee int64
		unread  bool
		task    bool
		want    string
	}{
		{"never seen, session up, nothing owed => idle", "", "", true, "", 0, false, false, "idle"},
		{"idle, ancient last_seen, nothing owed => idle", "", "", true, "idle", old, false, false, "idle"},
		{"finished, nothing owed => idle", "", "", true, "finished", old, false, false, "idle"},
		{"unread and silent => stale", "", "", true, "idle", old, true, false, "stale"},
		{"unread, never seen => stale", "", "", true, "", 0, true, false, "stale"},
		{"open task and silent => stale", "", "", true, "idle", old, false, true, "stale"},
		{"open task, recent call => alive", "", "", true, "idle", now.Unix(), false, true, "alive"},
		{"unread, recent call => alive", "", "", true, "idle", now.Unix(), true, false, "alive"},
		{"working, recent => busy", "", "", true, "working", now.Unix(), false, false, "busy"},
		{"working, silent => stale", "", "", true, "working", old, false, false, "stale"},
		{"session gone, recent call => dead", "", "", false, "idle", now.Unix(), false, false, "dead"},
		{"session gone, nothing owed => dead", "", "", false, "", 0, false, false, "dead"},
		{"rate limited", "", "", true, "rate_limited", old, true, true, "rate_limited"},
		{"waiting on permission prompt", "", "", true, "waiting", old, false, true, "blocked"},
		{"waiting on user", "", "", true, "waiting_on_user", old, true, false, "blocked"},
		{"principal, task in_review, silent => stale", "prin", store.StatusInReview, true, "idle", old, false, true, "stale"},
		{"engineer, task in_review is not theirs => idle", "", store.StatusInReview, true, "idle", old, false, true, "idle"},
		{"engineer, task done => idle", "", store.StatusDone, true, "idle", old, false, true, "idle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSvc(t, true)
			st := s.Store
			id := c.agent
			if id == "" {
				id = "eng-1"
			}
			if c.status != "" {
				_ = st.SetAgentStatus(id, c.status)
			}
			if c.lastSee != 0 {
				_ = st.SetKV(lastSeenKey(id), strconv.FormatInt(c.lastSee, 10))
			}
			if c.unread {
				if _, err := st.AddMessage(store.Message{From: "orch", To: id, Kind: "note", Body: "x"}); err != nil {
					t.Fatal(err)
				}
			}
			if c.task {
				ts := c.taskSt
				if ts == "" {
					ts = store.StatusInProgress
				}
				if _, err := st.CreateTask(store.Task{Title: "t", Assignee: "eng-1", Status: ts}); err != nil {
					t.Fatal(err)
				}
			}
			h := AgentHealth(st, id, c.alive, now)
			if h.Liveness != c.want {
				t.Fatalf("got %s (%s), want %s", h.Liveness, h.Reason, c.want)
			}
			if c.want != "idle" && c.want != "alive" && c.want != "busy" && h.Reason == "" {
				t.Fatalf("%s needs a reason", c.want)
			}
		})
	}
}
