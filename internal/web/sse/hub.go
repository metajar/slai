package sse

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// Defaults from the principal's addendum.
const (
	DefaultBackfill      = 500                    // events replayed after Last-Event-ID, else resync
	DefaultQueue         = 256                    // frames buffered per stream; full means resync
	DefaultCoalesce      = 100 * time.Millisecond // at most one notice per topic per window
	DefaultHeartbeat     = 15 * time.Second
	DefaultPollEvery     = 250 * time.Millisecond // how often the events table is read while streams are open
	DefaultMaxPerSession = 8
)

// ErrTooManyStreams is returned for a session's ninth stream.
var ErrTooManyStreams = errors.New("too many open streams for this session")

// Source is the part of the store the hub reads.
type Source interface {
	EventsAfter(afterID int64, kind string, limit int) ([]store.Event, error)
	LastEventID() int64
}

// Config tunes a Hub; zero values pick the defaults.
type Config struct {
	Source              Source
	Now                 func() time.Time
	Backfill, Queue     int
	Coalesce, Heartbeat time.Duration
	PollEvery           time.Duration
}

// Frame is one SSE message ready to be written: ID (0: none), Event name and JSON Data; a
// comment frame (heartbeat) has Comment set instead.
type Frame struct {
	ID      int64
	Event   string
	Data    string
	Comment string
}

// Bytes renders the frame in the text/event-stream format.
func (f Frame) Bytes() []byte {
	if f.Comment != "" {
		return []byte(": " + f.Comment + "\n\n")
	}
	s := ""
	if f.ID != 0 {
		s += fmt.Sprintf("id: %d\n", f.ID)
	}
	return []byte(s + "event: " + f.Event + "\ndata: " + f.Data + "\n\n")
}

// Hub fans events out to streams. Create it with New; streams are opened with Subscribe.
// The hub reads the events table only while at least one stream is open.
type Hub struct {
	cfg Config

	mu      sync.Mutex
	subs    map[*Sub]struct{}
	cursor  int64
	pending map[string]*pend // coalescing window contents, by topic
	winAt   time.Time        // when the open window started (zero: none)
	lastHB  time.Time
	stop    chan struct{}
	onWatch func() // called (outside h.mu) when a stream starts watching topics
}

type pend struct {
	ids   map[string]bool
	maxID int64
}

// Sub is one open stream. Read frames from C until it is closed; call Close when done.
type Sub struct {
	hub *Hub
	// C delivers frames in order. It is closed by Close.
	C chan Frame

	mu      sync.Mutex
	closed  bool
	dropped bool     // the queue overflowed and a resync was queued
	watch   []string // topics this stream's page is looking at (e.g. "pane:eng-1")
}

// New returns a hub.
func New(cfg Config) *Hub {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Backfill <= 0 {
		cfg.Backfill = DefaultBackfill
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.Coalesce <= 0 {
		cfg.Coalesce = DefaultCoalesce
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = DefaultPollEvery
	}
	return &Hub{cfg: cfg, subs: map[*Sub]struct{}{}, pending: map[string]*pend{}}
}

// Subscribe opens a stream. lastEventID is the browser's Last-Event-ID (0: a fresh
// connection, which gets no backlog). More than Backfill events behind yields one resync
// frame instead of a replay.
func (h *Hub) Subscribe(lastEventID int64) *Sub {
	s := &Sub{hub: h, C: make(chan Frame, h.cfg.Queue)}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.Now()
	if len(h.subs) == 0 { // nobody was watching: start from the present
		h.cursor = h.cfg.Source.LastEventID()
		h.lastHB = now
		h.pending = map[string]*pend{}
		h.winAt = time.Time{}
	}
	if lastEventID > 0 && lastEventID < h.cursor {
		h.backfill(s, lastEventID)
	}
	h.subs[s] = struct{}{}
	if len(h.subs) == 1 {
		h.stop = make(chan struct{})
		go h.run(h.stop)
	}
	return s
}

// backfill replays what the browser missed, as one notice per topic, or a resync.
// h.mu is held.
func (h *Hub) backfill(s *Sub, after int64) {
	evs, err := h.cfg.Source.EventsAfter(after, "", h.cfg.Backfill+1)
	if err != nil || len(evs) > h.cfg.Backfill {
		s.send(Frame{ID: h.cursor, Event: "resync", Data: Notice{Kind: "resync"}.json()})
		return
	}
	merged := map[string]*pend{}
	for _, e := range evs {
		if e.ID > h.cursor {
			break // the poll loop will deliver it live
		}
		accumulate(merged, e)
	}
	for _, f := range framesOf(merged) {
		s.send(f)
	}
}

// Close ends the stream.
func (s *Sub) Close() {
	h := s.hub
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		if len(h.subs) == 0 && h.stop != nil {
			close(h.stop)
			h.stop = nil
		}
	}
	h.mu.Unlock()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.C)
	}
	s.mu.Unlock()
}

// send queues a frame without ever blocking. A full queue is dropped and replaced by one
// resync, so the browser refetches everything instead of missing a change.
func (s *Sub) send(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.dropped { // a resync is already waiting for this reader
		select {
		case s.C <- f:
		default:
		}
		if len(s.C) < cap(s.C)/2 {
			s.dropped = false
		}
		return
	}
	select {
	case s.C <- f:
	default:
		for len(s.C) > 0 {
			<-s.C
		}
		s.C <- Frame{Event: "resync", Data: Notice{Kind: "resync"}.json()}
		s.dropped = true
	}
}

// Publish notifies a topic that the events table does not carry (for example
// "pane:eng-1" when a pane's content changed). It joins the current coalescing window.
func (h *Hub) Publish(topic string, ids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	p := h.pending[topic]
	if p == nil {
		p = &pend{ids: map[string]bool{}}
		h.pending[topic] = p
	}
	for _, id := range ids {
		p.ids[id] = true
	}
	if h.winAt.IsZero() {
		h.winAt = h.cfg.Now()
	}
}

// MaxWatch is the most topics one stream may name.
const MaxWatch = 8

// SetWatch records the topics the stream's page is looking at: the ones a producer should
// keep fresh only while somebody looks (the pane push). They live and die with the stream.
func (s *Sub) SetWatch(topics []string) {
	if len(topics) > MaxWatch {
		topics = topics[:MaxWatch]
	}
	s.mu.Lock()
	s.watch = append([]string(nil), topics...)
	s.mu.Unlock()
	s.hub.mu.Lock()
	f := s.hub.onWatch
	s.hub.mu.Unlock()
	if f != nil && len(topics) > 0 {
		f()
	}
}

// OnWatch sets the function called when a stream starts watching topics, so a producer can
// start work only then.
func (h *Hub) OnWatch(f func()) {
	h.mu.Lock()
	h.onWatch = f
	h.mu.Unlock()
}

// Watched lists the distinct watched topics that start with prefix, over the open streams.
func (h *Hub) Watched(prefix string) []string {
	h.mu.Lock()
	subs := make([]*Sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	seen := map[string]bool{}
	for _, s := range subs {
		s.mu.Lock()
		for _, t := range s.watch {
			if strings.HasPrefix(t, prefix) {
				seen[t] = true
			}
		}
		s.mu.Unlock()
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Streams reports how many streams are open.
func (h *Hub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Poll reads new events once, flushes the coalescing window when it is due, and sends the
// heartbeat when it is due. The hub's goroutine calls it every PollEvery; tests call it
// with a fake clock.
func (h *Hub) Poll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	now := h.cfg.Now()
	evs, err := h.cfg.Source.EventsAfter(h.cursor, "", 1000)
	if err == nil {
		for _, e := range evs {
			h.cursor = e.ID
			accumulate(h.pending, e)
			if h.winAt.IsZero() {
				h.winAt = now
			}
		}
	}
	if !h.winAt.IsZero() && now.Sub(h.winAt) >= h.cfg.Coalesce {
		frames := framesOf(h.pending)
		h.pending, h.winAt = map[string]*pend{}, time.Time{}
		for _, f := range frames {
			h.broadcast(f)
		}
		h.lastHB = now
	}
	if now.Sub(h.lastHB) >= h.cfg.Heartbeat {
		h.lastHB = now
		h.broadcast(Frame{Comment: "heartbeat"})
	}
}

func (h *Hub) broadcast(f Frame) {
	for s := range h.subs {
		s.send(f)
	}
}

func (h *Hub) run(stop chan struct{}) {
	t := time.NewTicker(h.cfg.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.Poll()
		}
	}
}

func accumulate(into map[string]*pend, e store.Event) {
	for _, n := range TopicsFor(e) {
		p := into[n.Topic]
		if p == nil {
			p = &pend{ids: map[string]bool{}}
			into[n.Topic] = p
		}
		for _, id := range n.IDs {
			p.ids[id] = true
		}
		if e.ID > p.maxID {
			p.maxID = e.ID
		}
	}
}

// framesOf turns a window into one frame per topic, in topic order. The SSE id of a
// frames of a window all carry the newest event id in it, so the browser's Last-Event-ID
// is the newest event it has been told about.
func framesOf(m map[string]*pend) []Frame {
	topics := make([]string, 0, len(m))
	for t := range m {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	var top int64
	for _, p := range m {
		top = max(top, p.maxID)
	}
	var out []Frame
	for _, t := range topics {
		p := m[t]
		ids := make([]string, 0, len(p.ids))
		for id := range p.ids {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out = append(out, Frame{ID: top, Event: "topic", Data: Notice{Kind: "topic", Topic: t, IDs: ids}.json()})
	}
	return out
}
