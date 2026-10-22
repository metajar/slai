package sse

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

type fakeSource struct {
	mu     sync.Mutex
	evs    []store.Event
	lastID int64
	calls  int
}

func (f *fakeSource) add(kind, agent, data string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID++
	f.evs = append(f.evs, store.Event{ID: f.lastID, Agent: agent, Kind: kind, Data: data})
	return f.lastID
}

func (f *fakeSource) EventsAfter(after int64, kind string, limit int) ([]store.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var out []store.Event
	for _, e := range f.evs {
		if e.ID > after && (kind == "" || e.Kind == kind) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeSource) LastEventID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastID
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newHub(t *testing.T) (*Hub, *fakeSource, *clock) {
	t.Helper()
	src, clk := &fakeSource{}, &clock{time.Unix(1_800_000_000, 0)}
	h := New(Config{Source: src, Now: clk.now, PollEvery: time.Hour}) // the test drives Poll
	return h, src, clk
}

// drain returns the frames queued right now.
func drain(s *Sub) []Frame {
	var out []Frame
	for {
		select {
		case f := <-s.C:
			out = append(out, f)
		default:
			return out
		}
	}
}

func notice(t *testing.T, f Frame) Notice {
	t.Helper()
	var n Notice
	if err := json.Unmarshal([]byte(f.Data), &n); err != nil {
		t.Fatalf("frame data %q: %v", f.Data, err)
	}
	return n
}

// P12 AC2: a store change produces a notice, after the coalescing window.
func TestEventBecomesTopicNotice(t *testing.T) {
	h, src, clk := newHub(t)
	s := h.Subscribe(0)
	defer s.Close()
	src.add("request_review", "eng-1", `{"task_id":"T-0012"}`)
	h.Poll()
	if got := drain(s); len(got) != 0 {
		t.Fatalf("notice before the window ended: %v", got)
	}
	clk.t = clk.t.Add(DefaultCoalesce)
	h.Poll()
	topics := map[string]Notice{}
	for _, f := range drain(s) {
		n := notice(t, f)
		topics[n.Topic] = n
	}
	for _, want := range []string{"tasks", "task:T-0012", "agents", "events"} {
		if _, ok := topics[want]; !ok {
			t.Errorf("no %q notice in %v", want, topics)
		}
	}
	if n := topics["task:T-0012"]; len(n.IDs) != 1 || n.IDs[0] != "T-0012" || n.Kind != "topic" {
		t.Errorf("task notice = %+v", n)
	}
}

// Addendum AC6: a burst of 50 events inside 100 ms yields one notice per topic.
func TestBurstIsCoalescedPerTopic(t *testing.T) {
	h, src, clk := newHub(t)
	s := h.Subscribe(0)
	defer s.Close()
	for i := 0; i < 50; i++ {
		src.add("update_task", "orch", `{"task_id":"T-`+strconv.Itoa(100+i)+`"}`)
		clk.t = clk.t.Add(time.Millisecond) // 50 ms in all
		h.Poll()
	}
	if got := drain(s); len(got) != 0 {
		t.Fatalf("%d frames inside the window", len(got))
	}
	clk.t = clk.t.Add(DefaultCoalesce)
	h.Poll()
	count := map[string]int{}
	var tasks Notice
	for _, f := range drain(s) {
		n := notice(t, f)
		count[n.Topic]++
		if n.Topic == "tasks" {
			tasks = n
		}
	}
	if count["tasks"] != 1 || count["events"] != 1 || count["agents"] != 1 {
		t.Fatalf("notices per topic = %v", count)
	}
	if len(tasks.IDs) != 50 {
		t.Fatalf("the single tasks notice carries %d ids, want 50", len(tasks.IDs))
	}
}

// Addendum AC6: Last-Event-ID resume replays at most 500 events (as notices), else resync.
func TestBackfillThenResync(t *testing.T) {
	h, src, _ := newHub(t)
	for i := 0; i < 700; i++ {
		src.add("update_task", "orch", `{"task_id":"T-1"}`)
	}
	first := h.Subscribe(0) // opens the hub at event 700
	defer first.Close()
	src.lastID = 700

	near := h.Subscribe(300) // 400 behind: replayed
	defer near.Close()
	got := drain(near)
	if len(got) == 0 || notice(t, got[0]).Kind != "topic" {
		t.Fatalf("backfill frames = %v", got)
	}
	for _, f := range got {
		if f.Event == "resync" {
			t.Fatal("resync for a 400-event gap")
		}
	}

	far := h.Subscribe(100) // 600 behind: too many
	defer far.Close()
	got = drain(far)
	if len(got) != 1 || got[0].Event != "resync" || notice(t, got[0]).Kind != "resync" {
		t.Fatalf("far-behind frames = %v", got)
	}

	exact := h.Subscribe(200) // exactly 500 behind: still replayed
	defer exact.Close()
	for _, f := range drain(exact) {
		if f.Event == "resync" {
			t.Fatal("resync at exactly the backfill cap")
		}
	}
	if fresh := h.Subscribe(0); len(drain(fresh)) != 0 {
		t.Fatal("a fresh connection got a backlog")
	} else {
		fresh.Close()
	}
}

// Addendum AC6: a full 256-slot queue is dropped and replaced by one resync (slow client).
func TestSlowClientGetsResyncNotBlocked(t *testing.T) {
	h, src, clk := newHub(t)
	slow := h.Subscribe(0)
	defer slow.Close()
	fast := h.Subscribe(0)
	defer fast.Close()
	var fastGot []Frame
	for i := 0; i < DefaultQueue+50; i++ {
		src.add("hook:pretooluse", "eng-"+strconv.Itoa(i), `{}`)
		clk.t = clk.t.Add(DefaultCoalesce)
		h.Poll() // never blocks on the slow reader
		fastGot = append(fastGot, drain(fast)...)
	}
	if len(slow.C) > cap(slow.C) {
		t.Fatal("queue grew past its slots")
	}
	got := drain(slow)
	if len(got) == 0 || got[0].Event != "resync" {
		t.Fatalf("slow client: first frame %v, want resync", got)
	}
	if len(got) > 120 {
		t.Fatalf("the dropped queue was not emptied: %d frames", len(got))
	}
	for _, f := range fastGot {
		if f.Event == "resync" {
			t.Fatal("a reader that keeps up got a resync")
		}
	}
	if len(fastGot) < DefaultQueue {
		t.Fatalf("fast client saw %d frames", len(fastGot))
	}
}

func TestHeartbeatEvery15s(t *testing.T) {
	h, _, clk := newHub(t)
	s := h.Subscribe(0)
	defer s.Close()
	clk.t = clk.t.Add(14 * time.Second)
	h.Poll()
	if got := drain(s); len(got) != 0 {
		t.Fatalf("heartbeat at 14 s: %v", got)
	}
	clk.t = clk.t.Add(time.Second)
	h.Poll()
	got := drain(s)
	if len(got) != 1 || !strings.HasPrefix(string(got[0].Bytes()), ": heartbeat") {
		t.Fatalf("heartbeat frame: %v", got)
	}
	if strings.Contains(string(got[0].Bytes()), "event:") {
		t.Fatal("heartbeat must be a comment, not an event")
	}
}

// The hub reads the table only while someone is watching.
func TestNoPollingWithoutStreams(t *testing.T) {
	h, src, _ := newHub(t)
	h.Poll()
	if src.calls != 0 {
		t.Fatalf("%d reads with no stream open", src.calls)
	}
	s := h.Subscribe(0)
	s.Close()
	if h.Streams() != 0 {
		t.Fatal("stream not removed")
	}
	h.Poll()
	if src.calls != 0 {
		t.Fatal("polled after the last stream closed")
	}
}

// The hub can carry topics the events table does not have, such as pane:<agent>.
func TestPublishCarriesPaneTopic(t *testing.T) {
	h, _, clk := newHub(t)
	s := h.Subscribe(0)
	defer s.Close()
	h.Publish("pane:eng-1", "eng-1")
	h.Publish("pane:eng-1", "eng-1")
	clk.t = clk.t.Add(DefaultCoalesce)
	h.Poll()
	got := drain(s)
	if len(got) != 1 {
		t.Fatalf("frames: %v", got)
	}
	if n := notice(t, got[0]); n.Topic != "pane:eng-1" || len(n.IDs) != 1 {
		t.Fatalf("notice %+v", n)
	}
}

func TestFrameFormat(t *testing.T) {
	f := Frame{ID: 7, Event: "topic", Data: `{"kind":"topic"}`}
	if got, want := string(f.Bytes()), "id: 7\nevent: topic\ndata: {\"kind\":\"topic\"}\n\n"; got != want {
		t.Fatalf("%q != %q", got, want)
	}
}

func TestTopicsForKinds(t *testing.T) {
	topics := func(kind, agent, data string) map[string]bool {
		out := map[string]bool{}
		for _, n := range TopicsFor(store.Event{Kind: kind, Agent: agent, Data: data}) {
			out[n.Topic] = true
		}
		return out
	}
	cases := []struct {
		kind, agent, data string
		want              []string
	}{
		{"team_hold", "", `{"name":"x"}`, []string{"events", "alerts"}},
		{"spend_cap", "eng-1", `{}`, []string{"events", "alerts", "spend", "agents"}},
		{"approve", "prin", `{"task_id":"T-7"}`, []string{"events", "tasks", "task:T-7", "agents"}},
		{"ask_user", "eng-1", `{"question":"?"}`, []string{"events", "inbox"}},
		{"hook:stop", "eng-2", `{}`, []string{"events", "agents"}},
		{"memory_stale", "", `{}`, []string{"events", "memory"}},
	}
	for _, c := range cases {
		got := topics(c.kind, c.agent, c.data)
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: no %q notice (got %v)", c.kind, w, got)
			}
		}
	}
}
